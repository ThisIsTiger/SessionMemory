// Package store owns everything durable on this device: the sealed object
// store, the revision/manifest mirror, bindings, the transaction journal and
// the rebuildable SQLite views.
package store

import (
	"context"
	"encoding/json"
	"fmt"
	"io"

	"session-memory/internal/agent"
	"session-memory/internal/session"
	"session-memory/internal/vault"
)

// ---- journal ----

// Kind is the type of a transaction.
type Kind string

const (
	KindCapture Kind = "capture" // adapter bytes -> objects -> revision -> cursor
	KindPublish Kind = "publish" // objects + manifest -> sync folder
	KindPull    Kind = "pull"    // sync folder -> verified local revisions
	KindInstall Kind = "install" // revision -> vendor store (native, converted, or choice landing)
)

// State names a COMPLETED step: the journal file is rewritten with the new
// State only after the step's effects are durable. Recovery reads the last
// State and performs the next step, which every step makes safe to repeat.
type State string

const (
	StPlanned           State = "planned"
	StStaged            State = "staged"
	StStored            State = "stored"
	StRevisionCommitted State = "revision_committed"
	StCursorAdvanced    State = "cursor_advanced"
	StSealed            State = "sealed"
	StUploaded          State = "uploaded"
	StManifestPublished State = "manifest_published"
	StMarked            State = "marked"
	StVerified          State = "verified"
	StCommitted         State = "committed"
	StBackedUp          State = "backed_up"
	StWritten           State = "written"
	StBindingCommitted  State = "binding_committed"
	StRollingBack       State = "rolling_back"
	StRolledBack        State = "rolled_back"
	StDone              State = "done"
)

// Recovery is what startup recovery does for a journal in a given state.
type Recovery int

const (
	// Discard: nothing durable happened that anything depends on; delete the
	// staging directory and archive the journal.
	Discard Recovery = iota
	// Resume: perform the next step(s) from the journal's Data.
	Resume
	// Undo: roll back using the recorded backups and writes.
	Undo
	// Archive: finished; move the journal to journal/done/.
	Archive
)

// Flow lists, in order, the states of each kind and the recovery action when
// the journal's last State is that state. It is the single source of truth for
// both the state machine and its tests.
var Flow = map[Kind][]struct {
	State    State
	Recovery Recovery
}{
	KindCapture: {
		{StPlanned, Discard}, {StStaged, Discard}, {StStored, Resume},
		{StRevisionCommitted, Resume}, {StCursorAdvanced, Resume}, {StDone, Archive},
	},
	KindPublish: {
		{StPlanned, Resume}, {StSealed, Resume}, {StUploaded, Resume},
		{StManifestPublished, Resume}, {StMarked, Resume}, {StDone, Archive},
	},
	KindPull: {
		{StPlanned, Discard}, {StVerified, Resume}, {StCommitted, Resume}, {StDone, Archive},
	},
	KindInstall: {
		{StPlanned, Discard}, {StStaged, Discard}, {StBackedUp, Undo}, {StWritten, Undo},
		{StVerified, Resume}, {StBindingCommitted, Resume}, {StDone, Archive},
	},
}

// Rollback states shared by KindInstall.
var RollbackFlow = []struct {
	State    State
	Recovery Recovery
}{{StRollingBack, Undo}, {StRolledBack, Archive}}

// RecoveryFor returns the action for a journal in state s.
func RecoveryFor(k Kind, s State) (Recovery, error) {
	for _, e := range Flow[k] {
		if e.State == s {
			return e.Recovery, nil
		}
	}
	if k == KindInstall {
		for _, e := range RollbackFlow {
			if e.State == s {
				return e.Recovery, nil
			}
		}
	}
	return 0, fmt.Errorf("store: kind %q has no state %q", k, s)
}

// JournalRecord is journal/<tx_id>.json (completed ones move to journal/done/).
type JournalRecord struct {
	SchemaVersion int             `json:"schema_version"`
	TxID          string          `json:"tx_id"`
	Kind          Kind            `json:"kind"`
	SessionID     string          `json:"session_id,omitempty"`
	BindingID     string          `json:"binding_id,omitempty"`
	State         State           `json:"state"`
	StartedAt     string          `json:"started_at"`
	UpdatedAt     string          `json:"updated_at"`
	Data          json.RawMessage `json:"data"`
	LastError     string          `json:"last_error,omitempty"` // code only
}

// ---- per-kind journal data ----

// CaptureData is JournalRecord.Data for KindCapture. Everything recovery needs
// to commit the revision is here, so it never re-reads the source.
type CaptureData struct {
	NewBinding *session.Binding                 `json:"new_binding,omitempty"`
	Artifacts  map[string]session.ArtifactState `json:"artifacts"` // the state to commit
	Snapshot   string                           `json:"snapshot"`  // ObjectRef
	RepoState  string                           `json:"repo_state,omitempty"`
	Revision   *session.Revision                `json:"revision,omitempty"` // sealed, set at StStored
	Unsafe     *session.UnsafeMark              `json:"unsafe,omitempty"`
	Generation bool                             `json:"generation_bump,omitempty"`
}

// PublishData is JournalRecord.Data for KindPublish.
type PublishData struct {
	Sequence     int64    `json:"sequence"`
	Entries      int      `json:"entries"`
	Objects      []string `json:"objects"`                 // ObjectRefs to upload
	ManifestFile string   `json:"manifest_file,omitempty"` // file name, set at StSealed
	ManifestHash string   `json:"manifest_hash,omitempty"` // hex SHA-256 of the sealed bytes
	// the sealed manifest bytes are kept in staging/<tx_id>/manifest.sealed so a
	// resumed publish re-sends the SAME bytes (never two files for one sequence)
}

// PullData is JournalRecord.Data for KindPull.
type PullData struct {
	DeviceID  string   `json:"device_id"`
	Manifests []string `json:"manifests"` // file names
	Revisions []string `json:"revisions"`
}

// InstallData is JournalRecord.Data for KindInstall.
type InstallData struct {
	Agent           string               `json:"agent,omitempty"`
	RegistryUndo    string               `json:"registry_undo,omitempty"` // row-level backup file under BackupDir
	NativeSessionID string               `json:"native_session_id,omitempty"`
	ClientClosed    bool                 `json:"client_closed,omitempty"`
	PlanDigest      string               `json:"plan_digest"`
	Mode            agent.Mode           `json:"mode"`
	Route           agent.Route          `json:"route"`
	Home            string               `json:"home"`
	BackupDir       string               `json:"backup_dir"`
	Backed          []string             `json:"backed,omitempty"`  // relative paths copied to BackupDir
	Created         []string             `json:"created,omitempty"` // files that did not exist before
	Written         []agent.PlannedWrite `json:"written,omitempty"`
	Planned         []agent.PlannedWrite `json:"planned,omitempty"` // every file the install will write, with its final hash (recovery refuses files that are neither this nor the backup)
	Landing         string               `json:"landing,omitempty"` // "choice" for a §9.2 landing
	Receipt         *agent.Receipt       `json:"receipt,omitempty"` // set at StVerified
	NewBinding      *session.Binding     `json:"new_binding,omitempty"`
	Superseded      []string             `json:"superseded,omitempty"` // active bindings retired by a cross-agent conversion
}

// Tx is an open transaction. Every method that changes state writes the
// journal atomically first.
type Tx interface {
	ID() string
	Kind() Kind
	// SetState records a completed step and its data (nil keeps Data).
	SetState(s State, data any) error
	// StagingDir is store/staging/<tx_id>/ (0700). Plaintext may exist only here.
	StagingDir() string
	agent.Tx // BackupDir, Backed, Wrote
	// Data unmarshals the journal data into v; State is the last completed state.
	Data(v any) error
	State() State
	// Rollback restores install backups while the caller still holds vendor locks.
	Rollback() error
	// Finish moves the journal to done/ and removes StagingDir.
	Finish() error
}

// ---- store ----

// ObjectInfo describes a stored object.
type ObjectInfo struct {
	Ref         string // session.ObjectRefOf(sha256 of the sealed file)
	PlainSize   int64
	PlainSHA256 string
	SealedSize  int64
}

// Store is the local durable state. Implementations must be safe for use by
// one process at a time per SM_HOME (guarded by locks/store.lock).
type Store interface {
	// Objects
	PutObject(ctx context.Context, t vault.ObjectType, extra []byte, r io.Reader) (ObjectInfo, error)
	PutSealed(ctx context.Context, ref string, sealed io.Reader) error // verifies sha256(sealed) == ref
	OpenObject(ctx context.Context, ref string, t vault.ObjectType, extra []byte) (io.ReadCloser, error)
	SealedBytes(ctx context.Context, ref string) (io.ReadCloser, error) // for upload
	HasObject(ref string) bool

	// Transactions
	Begin(ctx context.Context, k Kind, sessionID, bindingID string) (Tx, error)
	Recover(ctx context.Context) ([]RecoveryReport, error) // run at every start

	// Revisions and views
	CommitRevision(tx Tx, r session.Revision) error // all objects of its snapshot must exist
	Graph(sessionID string) (*session.Graph, error)
	Choices(sessionID string) ([]session.Choice, error)

	// Bindings (JSON files are the source of truth)
	LoadBinding(id string) (session.Binding, error)
	SaveBinding(tx Tx, b session.Binding) error
	Bindings(sessionID string) ([]session.Binding, error)

	// Rebuild drops sessions.db and rebuilds it from store/manifests, bindings and objects.
	Rebuild(ctx context.Context) error
}

// RecoveryReport is one journal's recovery outcome, for logs and `verify`.
type RecoveryReport struct {
	TxID   string
	Kind   Kind
	State  State
	Action Recovery
	Err    string // code only
}
