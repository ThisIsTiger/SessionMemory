package store

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"session-memory/internal/agent"
	"session-memory/internal/device"
	"session-memory/internal/errcode"
	"session-memory/internal/failpoint"
	"session-memory/internal/platform"
	"session-memory/internal/session"
)

type tx struct {
	s   *FS
	rec JournalRecord
}

func (s *FS) journalPath(id string) string { return s.path("journal", id+".json") }

// Begin writes the first journal record (State planned) before anything else.
func (s *FS) Begin(ctx context.Context, k Kind, sessionID, bindingID string) (Tx, error) {
	if _, ok := Flow[k]; !ok {
		return nil, fmt.Errorf("store: unknown transaction kind %q", k)
	}
	now := session.FormatTime(s.Now())
	t := &tx{s: s, rec: JournalRecord{SchemaVersion: session.SchemaVersion, TxID: session.NewTxID(), Kind: k, SessionID: sessionID,
		BindingID: bindingID, State: StPlanned, StartedAt: now, UpdatedAt: now, Data: json.RawMessage("{}")}}
	if err := platform.EnsurePrivateDir(t.StagingDir()); err != nil {
		return nil, err
	}
	if err := failpoint.Hit(string(k) + ".before_planned"); err != nil {
		return nil, err
	}
	if err := t.write(); err != nil {
		return nil, err
	}
	return t, failpoint.Hit(string(k) + ".after_planned")
}

func (t *tx) ID() string         { return t.rec.TxID }
func (t *tx) Kind() Kind         { return t.rec.Kind }
func (t *tx) State() State       { return t.rec.State }
func (t *tx) StagingDir() string { return t.s.path("store", "staging", t.rec.TxID) }
func (t *tx) BackupDir() string  { return t.s.path("backups", t.rec.TxID) }

func (t *tx) write() error {
	t.rec.UpdatedAt = session.FormatTime(t.s.Now())
	b, err := json.MarshalIndent(t.rec, "", "  ")
	if err != nil {
		return err
	}
	return platform.AtomicWrite(t.s.journalPath(t.rec.TxID), append(b, '\n'), 0600)
}

// SetState records a completed step. failpoints fire before and after.
func (t *tx) SetState(st State, data any) error {
	name := string(t.rec.Kind) + "."
	if err := failpoint.Hit(name + "before_" + string(st)); err != nil {
		return err
	}
	if data != nil {
		b, err := json.Marshal(data)
		if err != nil {
			return err
		}
		t.rec.Data = b
	}
	t.rec.State = st
	if err := t.write(); err != nil {
		return err
	}
	return failpoint.Hit(name + "after_" + string(st))
}

// Data unmarshals the journal data into v.
func (t *tx) Data(v any) error { return json.Unmarshal(t.rec.Data, v) }

// Backed and Wrote update the install journal data so recovery can undo.
func (t *tx) Backed(rel string) error {
	var d InstallData
	t.Data(&d)
	d.BackupDir = t.BackupDir()
	d.Backed = append(d.Backed, rel)
	return t.SetState(t.rec.State, d)
}

func (t *tx) Wrote(w agent.PlannedWrite) error {
	var d InstallData
	t.Data(&d)
	d.Written = append(d.Written, w)
	return t.SetState(t.rec.State, d)
}

// Finish moves the journal to done/ and removes staging.
func (t *tx) Finish() error {
	t.rec.State = StDone
	if err := t.write(); err != nil {
		return err
	}
	if err := os.Rename(t.s.journalPath(t.rec.TxID), t.s.path("journal", "done", t.rec.TxID+".json")); err != nil {
		return err
	}
	return os.RemoveAll(t.StagingDir())
}

func (s *FS) loadJournal(path string) (JournalRecord, error) {
	var r JournalRecord
	b, err := os.ReadFile(path)
	if err != nil {
		return r, err
	}
	return r, json.Unmarshal(b, &r)
}

// Recover runs startup recovery (spec §4.2). Journals it cannot finish alone
// (publish, pull) go to the registered Resumers; without one they are
// reported and left in place.
func (s *FS) Recover(ctx context.Context) ([]RecoveryReport, error) {
	ents, _ := filepath.Glob(s.path("journal", "*.json"))
	type item struct {
		path string
		rec  JournalRecord
	}
	var items []item
	var reports []RecoveryReport
	for _, p := range ents {
		rec, err := s.loadJournal(p)
		if err != nil {
			os.Rename(p, p+".corrupt")
			reports = append(reports, RecoveryReport{TxID: filepath.Base(p), Err: "recovery_needed"})
			continue
		}
		items = append(items, item{p, rec})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].rec.StartedAt < items[j].rec.StartedAt })
	for _, it := range items {
		rep := RecoveryReport{TxID: it.rec.TxID, Kind: it.rec.Kind, State: it.rec.State}
		act, err := RecoveryFor(it.rec.Kind, it.rec.State)
		if err != nil {
			rep.Err = "recovery_needed"
			reports = append(reports, rep)
			continue
		}
		rep.Action = act
		t := &tx{s: s, rec: it.rec}
		switch act {
		case Discard, Archive:
			err = t.Finish()
		case Undo:
			err = func() error {
				if s.InstallGuard != nil {
					var data InstallData
					if err := t.Data(&data); err != nil {
						return err
					}
					release, err := s.InstallGuard(ctx, data)
					if err != nil {
						return err
					}
					defer release()
				}
				return s.undoInstall(t)
			}()
			if err == nil {
				t.rec.State = StRolledBack
				err = t.Finish()
			}
		case Resume:
			err = s.resume(ctx, t)
		}
		if err != nil {
			rep.Err = "recovery_needed"
		}
		reports = append(reports, rep)
	}
	for _, rep := range reports {
		if rep.Err != "" {
			return reports, errcode.New(errcode.RecoveryNeeded, "unfinished transaction requires recovery").With("tx_id", rep.TxID)
		}
	}
	return reports, nil
}

func (s *FS) resume(ctx context.Context, t *tx) error {
	if r, ok := s.Resumers[t.rec.Kind]; ok {
		return r(ctx, s, t.rec)
	}
	switch t.rec.Kind {
	case KindCapture:
		var d CaptureData
		if err := t.Data(&d); err != nil {
			return err
		}
		if t.rec.State == StStored && d.Revision != nil {
			if err := s.CommitRevision(t, *d.Revision); err != nil {
				return err
			}
			t.rec.State = StRevisionCommitted
		}
		if t.rec.State == StRevisionCommitted && t.rec.BindingID != "" {
			b, err := s.LoadBinding(t.rec.BindingID)
			if err != nil {
				return err
			}
			b.Artifacts = d.Artifacts
			b.Unsafe = d.Unsafe
			if d.Revision != nil {
				b.InstalledRevisionID = d.Revision.RevisionID
				b.BaseRevisionID = firstNonEmpty(b.BaseRevisionID, d.Revision.RevisionID)
			}
			if d.NewBinding != nil {
				b = *d.NewBinding
			}
			if err := s.SaveBinding(t, b); err != nil {
				return err
			}
		}
		return t.Finish()
	case KindInstall:
		return s.resumeInstall(t)
	}
	return fmt.Errorf("store: no resumer for %s", t.rec.Kind)
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// undoInstall restores backups over the target and removes created files.
func (s *FS) undoInstall(t *tx) error {
	var d InstallData
	if err := t.Data(&d); err != nil {
		return err
	}
	if d.RegistryUndo != "" {
		if s.InstallRowUndo == nil {
			return errcode.New(errcode.RecoveryNeeded, "vendor registry recovery handler is not registered")
		}
		if err := s.InstallRowUndo(context.Background(), d); err != nil {
			return err
		}
	}
	for _, rel := range d.Backed {
		src := filepath.Join(d.BackupDir, filepath.FromSlash(rel))
		b, err := os.ReadFile(src)
		if err != nil {
			return err
		}
		st, err := os.Stat(src)
		if err != nil {
			return err
		}
		if err := platform.AtomicWrite(filepath.Join(d.Home, filepath.FromSlash(rel)), b, st.Mode().Perm()); err != nil {
			return err
		}
	}
	for _, rel := range d.Created {
		if err := os.Remove(filepath.Join(d.Home, filepath.FromSlash(rel))); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

// resumeInstall continues an install journal at StVerified or later: the
// binding in Data.NewBinding is committed.
func (s *FS) resumeInstall(t *tx) error {
	var d InstallData
	if err := t.Data(&d); err != nil {
		return err
	}
	if d.NewBinding != nil {
		if err := s.SaveBinding(t, *d.NewBinding); err != nil {
			return err
		}
		if r := d.Receipt; r != nil {
			e := session.Entry{Type: session.EntryReceipt, Receipt: &session.ReceiptEntry{SessionID: r.SessionID, RevisionID: r.RevisionID,
				DeviceID: d.NewBinding.DeviceID, Status: session.ReceiptInstalled, InstallMode: session.InstallNative, BindingID: d.NewBinding.BindingID, At: r.At}}
			if err := device.At(s.Root).AppendOutbox(e); err != nil {
				return err
			}
		}
	}
	return t.Finish()
}

// AttachTx re-opens an unfinished transaction from its journal record (used by
// resumers).
func (s *FS) AttachTx(rec JournalRecord) Tx { return &tx{s: s, rec: rec} }

// RollbackTx undoes an install transaction in place (journal-protected) and
// finishes it.
func (s *FS) RollbackTx(t Tx) error {
	tt, ok := t.(*tx)
	if !ok {
		return fmt.Errorf("store: foreign transaction")
	}
	if err := tt.SetState(StRollingBack, nil); err != nil {
		return err
	}
	if err := s.undoInstall(tt); err != nil {
		return err
	}
	if err := tt.SetState(StRolledBack, nil); err != nil {
		return err
	}
	return tt.Finish()
}

func (t *tx) Rollback() error { return t.s.RollbackTx(t) }
