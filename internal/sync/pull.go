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
	"strconv"
	"strings"

	"session-memory/internal/errcode"
	"session-memory/internal/platform"
	"session-memory/internal/session"
	"session-memory/internal/store"
	"session-memory/internal/transport"
	"session-memory/internal/vault"
)

// parseManifestSeq reads the zero-padded decimal sequence of a manifest name.
// (fmt.Sscan would take a leading zero as octal: "000000000010" is 8.)
func parseManifestSeq(s string) (int64, error) { return strconv.ParseInt(s, 10, 64) }

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
	if _, err := s.Store.Recover(ctx); err != nil {
		return res, err
	}
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
	// Recovery must have the exact authenticated bytes before verified is
	// durable, including a crash before the local manifest mirror is written.
	if err := platform.AtomicWrite(filepath.Join(tx.StagingDir(), "manifest.sealed"), data, 0600); err != nil {
		return false, true, err
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

func (s *Sync) resumePull(ctx context.Context, rec store.JournalRecord) error {
	tx := s.Store.AttachTx(rec)
	var d store.PullData
	if err := tx.Data(&d); err != nil {
		return err
	}
	for _, name := range d.Manifests {
		match := manifestName.FindStringSubmatch(name)
		if match == nil || !session.ValidDeviceID(d.DeviceID) {
			return errcode.New(errcode.RecoveryNeeded, "invalid pull journal")
		}
		seq, err := parseManifestSeq(match[1])
		if err != nil {
			return errcode.Wrap(errcode.RecoveryNeeded, "invalid pull journal", err)
		}
		prev := ""
		if seq > 1 {
			if err := s.Store.DB().QueryRow(`SELECT file_sha256 FROM manifests WHERE device_id=? AND sequence=? AND status='applied'`, d.DeviceID, seq-1).Scan(&prev); err != nil {
				return err
			}
		}
		data, err := os.ReadFile(filepath.Join(tx.StagingDir(), "manifest.sealed"))
		if err != nil {
			// Older journals may have already persisted the local mirror.
			data, err = os.ReadFile(filepath.Join(s.Store.Root, "store", "manifests", d.DeviceID, name))
		}
		if err != nil {
			rc, getErr := s.T.Get(ctx, "manifests/"+d.DeviceID+"/"+name)
			if getErr != nil {
				return getErr
			}
			data, err = io.ReadAll(rc)
			rc.Close()
		}
		if err != nil {
			return err
		}
		h := sha(data)
		if h[:12] != match[2] {
			return errcode.New(errcode.AuthFailed, "pull journal manifest hash mismatch")
		}
		plain, err := s.V.Open(vault.TypeManifest, vault.ManifestAAD(d.DeviceID, seq, prev), data)
		if err != nil {
			return err
		}
		var m session.Manifest
		if err := json.Unmarshal(plain, &m); err != nil {
			return err
		}
		if !manifestValid(m, d.DeviceID, seq, prev) {
			return errcode.New(errcode.UnsafeRecord, "invalid recovered manifest")
		}
		if err := platform.AtomicWrite(filepath.Join(s.Store.Root, "store", "manifests", d.DeviceID, name), data, 0600); err != nil {
			return err
		}
		if err := s.Store.ApplyManifest(m, name, h); err != nil {
			return err
		}
		var receipts []session.Entry
		for _, e := range m.Entries {
			if e.Revision != nil {
				r := e.Revision.Revision
				receipts = append(receipts, session.Entry{Type: session.EntryReceipt, Receipt: &session.ReceiptEntry{SessionID: r.SessionID, RevisionID: r.RevisionID, DeviceID: s.Info.DeviceID, Status: session.ReceiptReceived, At: session.FormatTime(s.Store.Now())}})
			}
		}
		if len(receipts) > 0 {
			if err := s.Dev.AppendOutbox(receipts...); err != nil {
				return err
			}
		}
	}
	return tx.Finish()
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
