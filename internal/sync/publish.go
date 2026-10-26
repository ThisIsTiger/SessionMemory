// Package sync publishes this device's revisions to the shared folder and
// imports other devices' manifests (spec §7).
package sync

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"session-memory/internal/canonjson"
	"session-memory/internal/device"
	"session-memory/internal/errcode"
	"session-memory/internal/failpoint"
	"session-memory/internal/platform"
	"session-memory/internal/session"
	"session-memory/internal/store"
	"session-memory/internal/transport"
	"session-memory/internal/vault"
)

// Sync is the sync engine of one device.
type Sync struct {
	Store *store.FS
	T     transport.Transport
	V     vault.Sealer
	Dev   *device.Device
	Info  device.Info
}

// New wires the engine and registers journal resumers on the store.
func New(st *store.FS, t transport.Transport, v vault.Sealer, root string, info device.Info) *Sync {
	s := &Sync{Store: st, T: t, V: v, Dev: device.At(root), Info: info}
	st.Resumers[store.KindPublish] = func(ctx context.Context, _ *store.FS, rec store.JournalRecord) error {
		return s.resumePublish(ctx, rec)
	}
	st.Resumers[store.KindPull] = func(ctx context.Context, _ *store.FS, rec store.JournalRecord) error {
		return s.resumePull(ctx, rec)
	}
	return s
}

func objectPath(ref string) string {
	h := ref[len("obj_"):]
	return "objects/" + h[:2] + "/" + h + ".obj"
}

func sha(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// CheckDeviceID refuses publishing when the folder holds a newer sequence for
// this device id than the local record (clone or restored backup, §7.4).
func (s *Sync) CheckDeviceID(ctx context.Context) error {
	last, err := s.Dev.LastPublished()
	if err != nil {
		return err
	}
	ents, err := s.T.List(ctx, "manifests/"+s.Info.DeviceID+"/")
	if err != nil {
		return err
	}
	pending, _ := s.pendingSequence()
	for _, e := range ents {
		var seq int64
		if _, err := fmt.Sscanf(filepath.Base(e.Path), "%012d-", &seq); err == nil && seq > last && seq != pending {
			return errcode.New(errcode.DeviceIDConflict, "the sync folder has a newer manifest for this device id").With("sequence", seq)
		}
	}
	return nil
}

// pendingSequence is the sequence of an in-flight publish journal, if any.
func (s *Sync) pendingSequence() (int64, bool) {
	files, _ := filepath.Glob(filepath.Join(s.Store.Root, "journal", "*.json"))
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		var rec store.JournalRecord
		if json.Unmarshal(b, &rec) == nil && rec.Kind == store.KindPublish {
			var d store.PublishData
			if json.Unmarshal(rec.Data, &d) == nil && d.Sequence > 0 {
				return d.Sequence, true
			}
		}
	}
	return 0, false
}

// newObjects returns the objects a revision introduces relative to its parent.
func (s *Sync) newObjects(ctx context.Context, r session.Revision) ([]string, error) {
	snap, err := s.Store.LoadSnapshot(ctx, r.NativeSnapshotRef)
	if err != nil {
		return nil, err
	}
	have := map[string]bool{}
	if r.ParentRevisionID != "" {
		if revs, _ := s.Store.Revisions(r.SessionID); len(revs) > 0 {
			for _, p := range revs {
				if p.RevisionID == r.ParentRevisionID {
					if ps, err := s.Store.LoadSnapshot(ctx, p.NativeSnapshotRef); err == nil {
						for _, o := range ps.ObjectRefs() {
							have[o] = true
						}
					}
				}
			}
		}
	}
	set := map[string]bool{r.NativeSnapshotRef: true}
	if r.RepoStateRef != "" {
		set[r.RepoStateRef] = true
	}
	for _, o := range snap.ObjectRefs() {
		if !have[o] {
			set[o] = true
		}
	}
	var out []string
	for o := range set {
		out = append(out, o)
	}
	sort.Strings(out)
	return out, nil
}

// Publish sends every unpublished revision and queued entry; it returns the
// number of entries published.
func (s *Sync) Publish(ctx context.Context) (int, error) {
	if _, err := s.Store.Recover(ctx); err != nil {
		return 0, err
	}
	if err := s.CheckDeviceID(ctx); err != nil {
		return 0, err
	}
	var revs []session.Revision
	for _, id := range append([]string{s.Info.DeviceID}, s.Info.PreviousDeviceIDs...) {
		rs, err := s.Store.UnpublishedRevisions(id)
		if err != nil {
			return 0, err
		}
		revs = append(revs, rs...)
	}
	sort.Slice(revs, func(i, j int) bool {
		if revs[i].CreatedAt == revs[j].CreatedAt {
			return revs[i].RevisionID < revs[j].RevisionID
		}
		return revs[i].CreatedAt < revs[j].CreatedAt
	})
	var entries []session.Entry
	for _, r := range revs {
		objs, err := s.newObjects(ctx, r)
		if err != nil {
			return 0, err
		}
		entries = append(entries, session.Entry{Type: session.EntryRevision, Revision: &session.RevisionEntry{Revision: r, Objects: objs}})
	}
	out, err := s.Dev.Outbox()
	if err != nil {
		return 0, err
	}
	entries = append(entries, out...)
	total := 0
	for len(entries) > 0 {
		n := len(entries)
		if n > session.MaxManifestEntries {
			n = session.MaxManifestEntries
		}
		if err := s.publishBatch(ctx, entries[:n]); err != nil {
			return total, err
		}
		total += n
		entries = entries[n:]
	}
	if len(out) > 0 {
		if err := s.Dev.ClearOutbox(len(out)); err != nil {
			return total, err
		}
	}
	return total, nil
}

func (s *Sync) publishBatch(ctx context.Context, entries []session.Entry) error {
	last, err := s.Dev.LastPublished()
	if err != nil {
		return err
	}
	seq := last + 1
	prev := s.prevHash(seq)
	tx, err := s.Store.Begin(ctx, store.KindPublish, "", "")
	if err != nil {
		return err
	}
	var objs []string
	for _, e := range entries {
		if e.Revision != nil {
			objs = append(objs, e.Revision.Objects...)
		}
	}
	sort.Strings(objs)
	if err := tx.SetState(store.StPlanned, store.PublishData{Sequence: seq, Entries: len(entries), Objects: objs}); err != nil {
		return err
	}
	m := session.Manifest{SchemaVersion: session.SchemaVersion, DeviceID: s.Info.DeviceID, Sequence: seq, PrevManifestHash: prev,
		CreatedAt: session.FormatTime(s.Store.Now()), Entries: entries}
	plain, err := canonjson.Marshal(m)
	if err != nil {
		return err
	}
	sealed, err := s.V.Seal(vault.TypeManifest, vault.ManifestAAD(m.DeviceID, seq, prev), plain)
	if err != nil {
		return err
	}
	if err := platform.AtomicWrite(filepath.Join(tx.StagingDir(), "manifest.sealed"), sealed, 0600); err != nil {
		return err
	}
	d := store.PublishData{Sequence: seq, Entries: len(entries), Objects: objs, ManifestFile: session.ManifestFileName(seq, sha(sealed)), ManifestHash: sha(sealed)}
	if err := tx.SetState(store.StSealed, d); err != nil {
		return err
	}
	return s.continuePublish(ctx, tx, d, sealed, m)
}

// prevHash is the file hash of this device's manifest seq-1 ("" for the first).
func (s *Sync) prevHash(seq int64) string {
	if seq <= 1 {
		return ""
	}
	p := filepath.Join(s.Store.Root, "store", "manifests", s.Info.DeviceID, fmt.Sprintf("%012d-", seq-1))
	files, _ := filepath.Glob(p + "*.manifest")
	if len(files) == 1 {
		if b, err := os.ReadFile(files[0]); err == nil {
			return sha(b)
		}
	}
	return ""
}

// continuePublish performs the steps after StSealed; it is also the resume path.
func (s *Sync) continuePublish(ctx context.Context, tx store.Tx, d store.PublishData, sealed []byte, m session.Manifest) error {
	// A restored/cloned identity can have a pending journal at a sequence
	// already published by another copy. Never publish a second hash there.
	ents, err := s.T.List(ctx, "manifests/"+s.Info.DeviceID+"/")
	if err != nil {
		return err
	}
	for _, entry := range ents {
		var sequence int64
		if _, err := fmt.Sscanf(filepath.Base(entry.Path), "%012d-", &sequence); err == nil && sequence == d.Sequence && filepath.Base(entry.Path) != d.ManifestFile {
			return errcode.New(errcode.DeviceIDConflict, "another manifest already occupies this device sequence")
		}
	}
	if tx.State() == store.StSealed {
		for i, ref := range d.Objects {
			if i == len(d.Objects)/2 {
				if err := failpoint.Hit("publish.mid_upload"); err != nil {
					return err
				}
			}
			rc, err := s.Store.SealedBytes(ctx, ref)
			if err != nil {
				return err
			}
			err = s.T.Put(ctx, objectPath(ref), rc)
			rc.Close()
			if err != nil {
				return err
			}
		}
		if err := tx.SetState(store.StUploaded, nil); err != nil {
			return err
		}
	}
	if tx.State() == store.StUploaded {
		path := "manifests/" + s.Info.DeviceID + "/" + d.ManifestFile
		if err := s.T.Put(ctx, path, bytes.NewReader(sealed)); err != nil {
			if errors.Is(err, transport.ErrExists) {
				return errcode.New(errcode.DeviceIDConflict, "a different manifest already exists at this sequence")
			}
			return err
		}
		if err := tx.SetState(store.StManifestPublished, nil); err != nil {
			return err
		}
	}
	if tx.State() == store.StManifestPublished {
		dst := filepath.Join(s.Store.Root, "store", "manifests", s.Info.DeviceID, d.ManifestFile)
		if err := platform.AtomicWrite(dst, sealed, 0600); err != nil {
			return err
		}
		if err := s.Store.ApplyManifest(m, d.ManifestFile, d.ManifestHash); err != nil {
			return err
		}
		if err := s.Dev.SetLastPublished(d.Sequence); err != nil {
			return err
		}
		if err := tx.SetState(store.StMarked, nil); err != nil {
			return err
		}
	}
	return tx.Finish()
}

// resumePublish re-sends the SAME sealed bytes recorded in staging (never
// re-seals).
func (s *Sync) resumePublish(ctx context.Context, rec store.JournalRecord) error {
	tx := s.Store.AttachTx(rec)
	var d store.PublishData
	if err := tx.Data(&d); err != nil {
		return err
	}
	if tx.State() == store.StPlanned {
		// nothing sealed yet: abandon; the next Publish allocates the sequence again
		return tx.Finish()
	}
	sealed, err := os.ReadFile(filepath.Join(tx.StagingDir(), "manifest.sealed"))
	if err != nil {
		return err
	}
	plain, err := s.V.Open(vault.TypeManifest, vault.ManifestAAD(s.Info.DeviceID, d.Sequence, s.prevHash(d.Sequence)), sealed)
	if err != nil {
		return err
	}
	var m session.Manifest
	if err := json.Unmarshal(plain, &m); err != nil {
		return err
	}
	return s.continuePublish(ctx, tx, d, sealed, m)
}
