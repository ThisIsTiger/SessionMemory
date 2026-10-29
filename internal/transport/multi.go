package transport

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
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
	os.MkdirAll(filepath.Dir(m.StateFile), 0700)
	if b, err := json.Marshal(cur); err == nil {
		os.WriteFile(m.StateFile, b, 0600)
	}
}

// CatchUp uploads to every account that missed a write what it lacks, for the
// accounts that can be asked now.
func (m *MultiMirror) CatchUp(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.StateFile == "" {
		return nil
	}
	behind := m.behind()
	// An account never seen before gets the whole local copy, so the copy must
	// first hold everything the other accounts have.
	complete := true
	for _, a := range m.Accounts {
		if _, known := behind[a.Label]; !known {
			_, failed, _ := m.each(func(a Account) error { return a.M.Pull(ctx) })
			complete = len(failed) == 0
			break
		}
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
		a.M.mu.Lock()
		defer a.M.mu.Unlock()
		return a.M.pullLocked(ctx, false, firstSegment(prefix))
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
		a.M.mu.Lock()
		err := a.M.pullLocked(ctx, true, firstSegment(path))
		a.M.mu.Unlock()
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
	ok, _, first := m.each(func(a Account) error { return a.M.Pull(ctx) })
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
