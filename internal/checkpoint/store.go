// Package checkpoint persists per-session incremental ingestion state keyed
// by source + session_id. Offsets are byte offsets into the rollout/transcript.
package checkpoint

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"session-memory/internal/platform"
)

// Status enumerations (spec 6.1).
const (
	StatusNew           = "new"
	StatusPending       = "pending"
	StatusReady         = "ready"
	StatusFailed        = "failed"
	StatusIgnoredSource = "ignored_source"
	StatusUnknownSource = "unknown_source"
	StatusFileReplaced  = "file_replaced"
)

// SchemaVersion is the persisted state format version.
const SchemaVersion = 1

// State is one session's checkpoint.
type State struct {
	SchemaVersion     int       `json:"schema_version"`
	SessionID         string    `json:"session_id"`
	ParentSession     string    `json:"parent_session,omitempty"`
	RolloutPath       string    `json:"rollout_path"`
	FileSize          int64     `json:"file_size"`
	FileModTime       time.Time `json:"file_mod_time"`
	Offset            int64     `json:"offset"`
	Status            string    `json:"status"`
	Classification    string    `json:"classification"`
	Source            string    `json:"source"`
	Adapter           string    `json:"adapter,omitempty"`
	Originator        string    `json:"originator"`
	ConfigFingerprint string    `json:"config_fingerprint"`
	LastEventAt       time.Time `json:"last_event_at"`
	LastSuccessAt     time.Time `json:"last_success_at"`
	LastErrorCode     string    `json:"last_error_code"`
}

// Store loads and saves session states under <root>/<source>/sessions/.
type Store struct {
	root string
}

// NewStore creates a checkpoint store rooted at the state directory.
func NewStore(root string) *Store { return &Store{root: root} }

func (s *Store) dirFor(source string) string {
	return filepath.Join(s.root, strings.ToLower(source), "sessions")
}

// SafeSessionID maps a session id to a filesystem-safe file name.
func SafeSessionID(id string) string { return platform.SafeName(id) }

func (s *Store) pathFor(source, sessionID string) string {
	return filepath.Join(s.dirFor(source), SafeSessionID(sessionID)+".json")
}

// CorruptError marks an unparseable state file; the file is renamed
// to .corrupt-<timestamp> before this is returned.
type CorruptError struct {
	Path string
	Err  error
}

func (e CorruptError) Error() string { return fmt.Sprintf("corrupt checkpoint state: %v", e.Err) }

// Load reads the state for one session; os.ErrNotExist when absent.
//
// A redundant mirror (<path>.mirror) is kept alongside every state file so a
// corrupt primary can be recovered without rebuilding from source: rebuilding
// re-derives batch boundaries from offset 0, which changes OperationIDs and
// defeats receipt dedup (double provider billing). Only when BOTH copies are
// unreadable does Load return CorruptError and the caller rebuild.
func (s *Store) Load(source, sessionID string) (State, error) {
	path := s.pathFor(source, sessionID)
	st, err := readStateFile(path)
	if err == nil {
		return st, nil
	}
	if os.IsNotExist(err) {
		return State{}, err
	}
	if _, ok := err.(schemaError); ok {
		return State{}, err
	}
	// primary unreadable: quarantine it, then try the mirror
	_ = os.Rename(path, path+".corrupt-"+time.Now().Format("20060102T150405"))
	if st, merr := readStateFile(path + mirrorSuffix); merr == nil {
		return st, nil
	}
	return State{}, CorruptError{Path: path, Err: err}
}

type schemaError struct{ msg string }

func (e schemaError) Error() string { return e.msg }

// mirrorSuffix marks the redundant copy of a state file.
const mirrorSuffix = ".mirror"

func readStateFile(path string) (State, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return State{}, err
	}
	var st State
	if err := json.Unmarshal(data, &st); err != nil {
		return State{}, fmt.Errorf("unmarshal %s: %w", filepath.Base(path), err)
	}
	if st.SchemaVersion > SchemaVersion {
		return State{}, schemaError{msg: fmt.Sprintf("checkpoint schema %d newer than supported %d", st.SchemaVersion, SchemaVersion)}
	}
	return st, nil
}

// Save atomically persists the state with 0600 perms. The directory is
// routed by Adapter (the agent identity, e.g. "codex"); Source keeps the
// session's meta source for diagnostics per the state schema.
//
// After the primary write succeeds, a mirror copy is written best-effort:
// losing the mirror is harmless (corrupt-primary recovery falls back to
// rebuild), so a mirror failure never fails the commit that just landed.
func (s *Store) Save(st State) error {
	if st.SchemaVersion == 0 {
		st.SchemaVersion = SchemaVersion
	}
	if st.SessionID == "" {
		return fmt.Errorf("state requires session_id")
	}
	dir := st.Adapter
	if dir == "" {
		dir = st.Source
	}
	st.Adapter = dir
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling state: %w", err)
	}
	if err := platform.EnsurePrivateDir(s.dirFor(dir)); err != nil {
		return err
	}
	if err := platform.AtomicWrite(s.pathFor(dir, st.SessionID), append(data, '\n'), 0600); err != nil {
		return err
	}
	_ = platform.AtomicWrite(s.pathFor(dir, st.SessionID)+mirrorSuffix, append(data, '\n'), 0600)
	return nil
}

// ListRetryable returns pending and failed states for a source.
func (s *Store) ListRetryable(source string) ([]State, error) {
	return s.listByStatus(source, StatusPending, StatusFailed)
}

// ListAll returns every state for a source.
func (s *Store) ListAll(source string) ([]State, error) {
	return s.listByStatus(source)
}

func (s *Store) listByStatus(source string, statuses ...string) ([]State, error) {
	dir := s.dirFor(source)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var states []State
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") || strings.Contains(e.Name(), ".corrupt-") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		var st State
		if err := json.Unmarshal(data, &st); err != nil {
			continue // corrupt files are quarantined on Load; skip here
		}
		if len(statuses) == 0 {
			states = append(states, st)
			continue
		}
		for _, want := range statuses {
			if st.Status == want {
				states = append(states, st)
				break
			}
		}
	}
	sort.Slice(states, func(i, j int) bool { return states[i].SessionID < states[j].SessionID })
	return states, nil
}

// Remove deletes one session's state (used in tests and cleanup).
func (s *Store) Remove(source, sessionID string) error {
	return os.Remove(s.pathFor(source, sessionID))
}
