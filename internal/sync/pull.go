package sync

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"session-memory/internal/errcode"
	"session-memory/internal/platform"
	"session-memory/internal/session"
	"session-memory/internal/store"
	"session-memory/internal/transport"
	"session-memory/internal/vault"
)

var manifestName = regexp.MustCompile(`^(\d{12})-([0-9a-f]{12})\.manifest$`)

// PullResult summarises one reconcile.
type PullResult struct {
	Applied     int
	Revisions   int
	PendingPrev int
	PendingObjs int
	Quarantined int
}

// Reconcile imports every new manifest of other devices (spec §7.4).
func (s *Sync) Reconcile(ctx context.Context) (PullResult, error) {
	var res PullResult
	ents, err := s.T.List(ctx, "manifests/")
	if err != nil {
		return res, err
	}
	byDev := map[string][]transport.Entry{}
	for _, e := range ents {
		parts := strings.Split(e.Path, "/")
		if len(parts) != 3 || !session.ValidDeviceID(parts[1]) || !manifestName.MatchString(parts[2]) {
			continue
		}
		byDev[parts[1]] = append(byDev[parts[1]], e)
	}
	devs := make([]string, 0, len(byDev))
	for d := range byDev {
		if d != s.Info.DeviceID {
			devs = append(devs, d)
		}
	}
	sort.Strings(devs)
	for _, dev := range devs {
		list := byDev[dev]
		sort.Slice(list, func(i, j int) bool { return list[i].Path < list[j].Path })
		applied, prevSha := s.appliedState(dev)
		for _, e := range list {
			m := manifestName.FindStringSubmatch(filepath.Base(e.Path))
			var seq int64
			fmt.Sscan(strings.TrimLeft(m[1], "0"), &seq)
			if seq <= applied {
				continue
			}
			if seq != applied+1 {
				res.PendingPrev++
				break
			}
			ok, stop, err := s.importOne(ctx, dev, seq, m[2], e, prevSha, &res)
			if err != nil {
				return res, err
			}
			if !ok {
				if stop {
					break
				}
				continue
			}
			applied++
			prevSha = s.lastSha(dev, seq)
		}
	}
	return res, nil
}

func (s *Sync) appliedState(dev string) (int64, string) {
	var seq int64
	var shaHex string
	row := s.Store.DB().QueryRow(`SELECT sequence, file_sha256 FROM manifests WHERE device_id=? AND status='applied' ORDER BY sequence DESC LIMIT 1`, dev)
	if err := row.Scan(&seq, &shaHex); err != nil {
		return 0, ""
	}
	return seq, shaHex
}

func (s *Sync) lastSha(dev string, seq int64) string {
	_, h := s.appliedState(dev)
	return h
}

func (s *Sync) quarantine(path string, data []byte, reason errcode.Code, res *PullResult) {
	res.Quarantined++
	day := session.FormatTime(s.Store.Now())[:10]
	dst := filepath.Join(s.Store.Root, "quarantine", strings.ReplaceAll(day, "-", ""), strings.ReplaceAll(path, "/", "_"))
	platform.AtomicWrite(dst, data, 0600)
	s.Store.DB().Exec(`INSERT INTO quarantine(path, reason, at) VALUES(?,?,?)`, path, string(reason), session.FormatTime(s.Store.Now()))
}

// importOne validates and applies one manifest. ok=false,stop=true halts the
// device's remaining manifests (pending or quarantined); ok=false,stop=false
// skips only this one.
func (s *Sync) importOne(ctx context.Context, dev string, seq int64, hash12 string, e transport.Entry, prevSha string, res *PullResult) (ok, stop bool, err error) {
	stable, err := s.T.Stable(ctx, e)
	if err != nil || !stable {
		return false, true, nil
	}
	rc, err := s.T.Get(ctx, e.Path)
	if err != nil {
		if errors.Is(err, transport.ErrNotStable) || errors.Is(err, fs.ErrNotExist) {
			return false, true, nil
		}
		return false, true, err
	}
	data, rerr := io.ReadAll(rc)
	rc.Close()
	if rerr != nil {
		return false, true, rerr
	}
	h := sha(data)
	if h[:12] != hash12 {
		s.quarantine(e.Path, data, errcode.AuthFailed, res)
		return false, true, nil
	}
	plain, oerr := s.V.Open(vault.TypeManifest, vault.ManifestAAD(dev, seq, prevSha), data)
	if oerr != nil {
		s.quarantine(e.Path, data, errcode.AuthFailed, res)
		return false, true, nil
	}
	var m session.Manifest
	if err := json.Unmarshal(plain, &m); err != nil || !manifestValid(m, dev, seq, prevSha) {
		s.quarantine(e.Path, data, errcode.UnsafeRecord, res)
		return false, true, nil
	}
	// objects
	var revs []session.Revision
	for _, en := range m.Entries {
		if en.Revision != nil {
			revs = append(revs, en.Revision.Revision)
			need := append([]string{en.Revision.Revision.NativeSnapshotRef}, en.Revision.Objects...)
			if en.Revision.Revision.RepoStateRef != "" {
				need = append(need, en.Revision.Revision.RepoStateRef)
			}
			for _, o := range need {
				if err := s.fetchObject(ctx, o); err != nil {
					res.PendingObjs++
					return false, true, nil
				}
			}
			snap, err := s.Store.LoadSnapshot(ctx, en.Revision.Revision.NativeSnapshotRef)
			if err != nil || snap.Validate() != nil {
				s.quarantine(e.Path, data, errcode.UnsafeRecord, res)
				return false, true, nil
			}
			for _, o := range snap.ObjectRefs() {
				if err := s.fetchObject(ctx, o); err != nil {
					res.PendingObjs++
					return false, true, nil
				}
			}
		}
	}
	tx, err := s.Store.Begin(ctx, store.KindPull, "", "")
	if err != nil {
		return false, true, err
	}
	var ids []string
	for _, r := range revs {
		ids = append(ids, r.RevisionID)
	}
	if err := tx.SetState(store.StVerified, store.PullData{DeviceID: dev, Manifests: []string{filepath.Base(e.Path)}, Revisions: ids}); err != nil {
		return false, true, err
	}
	if err := platform.AtomicWrite(filepath.Join(s.Store.Root, "store", "manifests", dev, filepath.Base(e.Path)), data, 0600); err != nil {
		return false, true, err
	}
	if err := s.Store.ApplyManifest(m, filepath.Base(e.Path), h); err != nil {
		return false, true, err
	}
	if err := tx.SetState(store.StCommitted, nil); err != nil {
		return false, true, err
	}
	var receipts []session.Entry
	for _, r := range revs {
		receipts = append(receipts, session.Entry{Type: session.EntryReceipt, Receipt: &session.ReceiptEntry{
			SessionID: r.SessionID, RevisionID: r.RevisionID, DeviceID: s.Info.DeviceID, Status: session.ReceiptReceived, At: session.FormatTime(s.Store.Now())}})
	}
	if len(receipts) > 0 {
		if err := s.Dev.AppendOutbox(receipts...); err != nil {
			return false, true, err
		}
	}
	res.Applied++
	res.Revisions += len(revs)
	return true, false, tx.Finish()
}

func manifestValid(m session.Manifest, dev string, seq int64, prev string) bool {
	if m.SchemaVersion != session.SchemaVersion || m.DeviceID != dev || m.Sequence != seq || m.PrevManifestHash != prev ||
		len(m.Entries) == 0 || len(m.Entries) > session.MaxManifestEntries {
		return false
	}
	for _, e := range m.Entries {
		if e.Validate() != nil {
			return false
		}
	}
	return true
}

// fetchObject makes sure ref is present locally, downloading it if needed.
func (s *Sync) fetchObject(ctx context.Context, ref string) error {
	if s.Store.HasObject(ref) {
		return nil
	}
	rc, err := s.T.Get(ctx, objectPath(ref))
	if err != nil {
		return err
	}
	defer rc.Close()
	return s.Store.PutSealed(ctx, ref, rc)
}

var _ = os.ErrNotExist
