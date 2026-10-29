package transport

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"sync"

	"session-memory/internal/platform"
)

// Account is one Weiyun account that holds a full copy of the vault.
type Account struct {
	Label string
	M     *Mirror
}

// MultiMirror keeps the vault in several Weiyun accounts at once, so that one
// account running out of its daily quota (or failing) does not stop syncing.
// Every file is write-once and named by content, so the accounts simply hold
// the same set of files: writes go to every account that can take them,
// reads gather from every account that answers, and an account that missed a
// write is brought level (CatchUp) as soon as it works again. All accounts
// share one local working copy.
type MultiMirror struct {
	Accounts []Account
	Local    *Folder
	// Skip reports an account that must not be asked now (its quota is known
	// to be used up). It counts as unavailable, not as a failure.
	Skip func(label string) bool
	// Note is told about every account that failed an operation, even when
	// another account made up for it.
	Note func(label string, err error)
	// StateFile remembers, across runs, which accounts missed a write (true) and
	// which were brought level (false). An account it has never heard of, such
	// as a backup added later, is treated as behind until CatchUp has filled it.
	StateFile string

	mu sync.Mutex
}

// NewMultiMirror returns a mirror over the accounts; they must share local.
func NewMultiMirror(local *Folder, accounts ...Account) *MultiMirror {
	return &MultiMirror{Accounts: accounts, Local: local}
}

type unavailable struct{ label string }

func (u unavailable) Error() string {
	return fmt.Sprintf("weiyun account %s is paused: %v", u.label, ErrQuota)
}
func (u unavailable) Unwrap() error { return ErrQuota }

// each runs f on every account that is not skipped and reports how many
// accepted it, plus the first error (a paused account counts as one when
// none could be asked).
func (m *MultiMirror) each(f func(Account) error) (ok int, failed []string, first error) {
	for _, a := range m.Accounts {
		if m.Skip != nil && m.Skip(a.Label) {
			failed = append(failed, a.Label)
			if first == nil {
				first = unavailable{a.Label}
			}
			continue
		}
		if err := f(a); err != nil {
			failed = append(failed, a.Label)
			if first == nil || (errors.Is(first, ErrQuota) && !errors.Is(err, ErrQuota)) {
				first = fmt.Errorf("weiyun account %s: %w", a.Label, err)
			}
			if m.Note != nil {
				m.Note(a.Label, err)
			}
			continue
		}
		ok++
	}
	return
}

// ---- which accounts are behind ----

func (m *MultiMirror) behind() map[string]bool {
	out := map[string]bool{}
	if m.StateFile == "" {
		return out
	}
	if b, err := os.ReadFile(m.StateFile); err == nil {
		json.Unmarshal(b, &out)
	}
	return out
}

func (m *MultiMirror) setBehind(label string, v bool) {
	if m.StateFile == "" {
		return
	}
	cur := m.behind()
	if old, known := cur[label]; known && old == v {
		return
	}
	cur[label] = v
	_ = m.saveBehind(cur)
}

func (m *MultiMirror) saveBehind(state map[string]bool) error {
	b, err := json.Marshal(state)
	if err != nil {
		return err
	}
	return platform.AtomicWrite(m.StateFile, b, 0600)
}

// markReceived runs before downloads enter the shared local cache. Persist the
// repair work first: if this process stops after the download, a future Pull
// will find the cached file and must still know it needs replication. Unknown
// accounts remain unknown until CatchUp has read the complete remote union.
// The caller holds m.mu.
func (m *MultiMirror) markReceived() error {
	if m.StateFile == "" {
		return nil
	}
	state := m.behind()
	changed := false
	for _, a := range m.Accounts {
		if level, known := state[a.Label]; known && !level {
			state[a.Label] = true
			changed = true
		}
	}
	if changed {
		return m.saveBehind(state)
	}
	return nil
}

// pullAccount is called with m.mu held; Mirror holds its own lock while it
// inventories and downloads, including the durable pre-download repair mark.
func (m *MultiMirror) pullAccount(ctx context.Context, a Account, force bool, only string) error {
	a.M.mu.Lock()
	defer a.M.mu.Unlock()
	return a.M.pullWithReceiveLocked(ctx, force, only, m.markReceived)
}

// ---- which accounts have been read in full ----

// An account's content is "read" once it has been pulled into the local copy
// in full. The local copy only grows, so this stays true; later files reach it
// through ordinary pulls, and markReceived dirties the accounts that are level
// when they do. The record keeps an account that cannot be reached from making
// CatchUp pull the others in full again on every round.
func (m *MultiMirror) readFile() string { return m.StateFile + ".read" }

func (m *MultiMirror) read() map[string]bool {
	out := map[string]bool{}
	if b, err := os.ReadFile(m.readFile()); err == nil {
		json.Unmarshal(b, &out)
	}
	return out
}

func (m *MultiMirror) saveRead(read map[string]bool) error {
	b, err := json.Marshal(read)
	if err != nil {
		return err
	}
	return platform.AtomicWrite(m.readFile(), b, 0600)
}

// CatchUp uploads to every account that missed a write what it lacks, for the
// accounts that can be asked now.
//
// An account never seen before gets the whole local copy, so the local copy
// must first hold everything every account has. The same goes once, after an
// upgrade: older versions marked accounts level without accounting for files
// received from elsewhere, so their marks (and any mark made since) say nothing
// until the whole vault has been read; then they are dropped and every account
// is brought level from the complete local copy.
func (m *MultiMirror) CatchUp(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.StateFile == "" {
		return nil
	}
	marker := m.StateFile + ".received-v1"
	settled := true
	if _, err := os.Stat(marker); os.IsNotExist(err) {
		settled = false
	} else if err != nil {
		return err
	}
	behind := m.behind()
	need := !settled
	for _, a := range m.Accounts {
		if _, known := behind[a.Label]; !known {
			need = true
		}
	}
	read := m.read()
	if need {
		var saveErr error
		m.each(func(a Account) error {
			if read[a.Label] {
				return nil
			}
			if err := m.pullAccount(ctx, a, true, ""); err != nil {
				return err
			}
			read[a.Label] = true
			if err := m.saveRead(read); err != nil && saveErr == nil {
				saveErr = err
			}
			return nil
		})
		if saveErr != nil {
			return saveErr
		}
	}
	complete := true
	for _, a := range m.Accounts {
		complete = complete && read[a.Label]
	}
	if !settled {
		if complete {
			state := m.behind()
			for _, a := range m.Accounts {
				delete(state, a.Label)
			}
			if err := m.saveBehind(state); err != nil {
				return err
			}
			if err := platform.AtomicWrite(marker, []byte("1\n"), 0600); err != nil {
				return err
			}
		}
		behind = map[string]bool{} // nothing is known to be level yet
	} else {
		behind = m.behind() // receives may have dirtied previously known accounts
	}
	var first error
	for _, a := range m.Accounts {
		if level, known := behind[a.Label]; (known && !level) || (!known && !complete) || (m.Skip != nil && m.Skip(a.Label)) {
			continue
		}
		if err := a.M.Push(ctx); err != nil {
			if first == nil {
				first = fmt.Errorf("weiyun account %s: %w", a.Label, err)
			}
			if m.Note != nil {
				m.Note(a.Label, err)
			}
			continue
		}
		m.setBehind(a.Label, false)
	}
	return first
}

// ---- Transport ----

// Put writes locally (write-once), then publishes to every account that can
// take it; an account that could not is remembered as behind.
func (m *MultiMirror) Put(ctx context.Context, path string, r io.Reader) error {
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	var conflict bool
	ok, failed, first := m.each(func(a Account) error {
		err := a.M.Put(ctx, path, bytes.NewReader(data))
		if errors.Is(err, ErrExists) {
			conflict = true
		}
		return err
	})
	if conflict {
		return ErrExists
	}
	if ok == 0 {
		return first
	}
	for _, l := range failed {
		m.setBehind(l, true)
	}
	return nil
}

// List pulls what the local copy lacks from every account that answers, then
// lists the local copy. It fails only when no account could be read.
func (m *MultiMirror) List(ctx context.Context, prefix string) ([]Entry, error) {
	m.mu.Lock()
	ok, _, first := m.each(func(a Account) error {
		return m.pullAccount(ctx, a, false, firstSegment(prefix))
	})
	m.mu.Unlock()
	if ok == 0 && len(m.Accounts) > 0 {
		return nil, first
	}
	return m.Local.List(ctx, prefix)
}

// Get opens a file from the local copy, fetching it from the accounts first if
// it is missing.
func (m *MultiMirror) Get(ctx context.Context, path string) (io.ReadCloser, error) {
	if rc, err := m.Local.Get(ctx, path); err == nil {
		return rc, nil
	}
	m.mu.Lock()
	var found io.ReadCloser
	ok, _, first := m.each(func(a Account) error {
		if found != nil {
			return nil
		}
		err := m.pullAccount(ctx, a, true, firstSegment(path))
		if err != nil {
			return err
		}
		if rc, err := m.Local.Get(ctx, path); err == nil {
			found = rc
		}
		return nil
	})
	m.mu.Unlock()
	if found != nil {
		return found, nil
	}
	if ok == 0 && len(m.Accounts) > 0 {
		return nil, first
	}
	return m.Local.Get(ctx, path)
}

// Stable reports on the local copy.
func (m *MultiMirror) Stable(ctx context.Context, e Entry) (bool, error) {
	return m.Local.Stable(ctx, e)
}

// Pull downloads, from every account, the files the local copy lacks.
func (m *MultiMirror) Pull(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	ok, _, first := m.each(func(a Account) error { return m.pullAccount(ctx, a, true, "") })
	if ok == 0 && len(m.Accounts) > 0 {
		return first
	}
	return nil
}

// Push uploads, to every account, the local files it lacks. An account that
// fails stays behind until CatchUp.
func (m *MultiMirror) Push(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	ok, failed, first := m.each(func(a Account) error {
		err := a.M.Push(ctx)
		if _, known := m.behind()[a.Label]; err == nil && known {
			m.setBehind(a.Label, false) // an account CatchUp has not seen yet may be missing history
		}
		return err
	})
	if ok == 0 && len(m.Accounts) > 0 {
		return first
	}
	for _, l := range failed {
		m.setBehind(l, true)
	}
	return nil
}

// PutKey stores the key file in every account that can take it.
func (m *MultiMirror) PutKey(ctx context.Context, data []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	ok, _, first := m.each(func(a Account) error { return a.M.PutKey(ctx, data) })
	if ok == 0 && len(m.Accounts) > 0 {
		return first
	}
	return nil
}

// GetKey returns the key file from the first account that has it. "Not
// there" is only reported when every account answered.
func (m *MultiMirror) GetKey(ctx context.Context) ([]byte, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var data []byte
	var found bool
	_, failed, first := m.each(func(a Account) error {
		if found {
			return nil
		}
		d, ok, err := a.M.GetKey(ctx)
		if err != nil {
			return err
		}
		if ok {
			data, found = d, true
		}
		return nil
	})
	if found {
		return data, true, nil
	}
	if len(failed) > 0 {
		return nil, false, first
	}
	return nil, false, nil
}

// DeleteKey moves the key file to the recycle bin in every account. It
// reports an error unless every account was cleared: a key left behind in an
// account that could not be asked is still readable there.
func (m *MultiMirror) DeleteKey(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, failed, first := m.each(func(a Account) error { return a.M.DeleteKey(ctx) })
	if len(failed) > 0 {
		sort.Strings(failed)
		return fmt.Errorf("the key file could not be removed from account(s) %v: %w", failed, first)
	}
	return nil
}

// RemoveTestVault moves a throwaway remote vault in every account to the recycle bin.
func (m *MultiMirror) RemoveTestVault(ctx context.Context) error {
	for _, a := range m.Accounts {
		if err := a.M.RemoveTestVault(ctx); err != nil {
			return err
		}
	}
	return nil
}
