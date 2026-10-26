// Package agent defines the contracts between the core (store, sync, CLI) and
// each agent adapter (Codex, WorkBuddy, ...): discovery, capture, snapshot
// reading, installation and run-state detection.
//
// Items marked "P0" are fixed in shape here but their behaviour is decided by
// the P0 experiments (E1a-E6); the interfaces do not change when they do.
package agent

import (
	"context"
	"io"

	"session-memory/internal/session"
)

// ---- discovery ----

// NativeRef locates one native session on this device. Nothing in it is ever
// synced.
type NativeRef struct {
	Home     string `json:"home"`      // agent home (one CODEX_HOME, ~/.workbuddy, ...)
	NativeID string `json:"native_id"` // thread id / session id
}

// NativeSession is one discovered session for `discover` / `list`.
type NativeSession struct {
	Ref       NativeRef            `json:"ref"`
	Profile   session.AgentProfile `json:"profile"`
	Cwd       string               `json:"cwd"`
	Title     string               `json:"title,omitempty"` // device-local display text; printed by the CLI to the user's own terminal, never logged, never synced
	UpdatedAt string               `json:"updated_at"`      // session.TimeLayout
	// Parent is the parent native session id for sub-agent threads, else "".
	Parent string `json:"parent,omitempty"`
	// Files are the files on disk, for display.
	Files []FileStat `json:"files"`
}

// FileStat is a file's identity on disk.
type FileStat struct {
	Path    string `json:"path"` // relative to Home, slash-separated, as stored on disk (may end in .zst)
	Size    int64  `json:"size"`
	ModTime string `json:"mod_time"`
}

// DiscoverOptions narrows discovery. The zero value lists everything.
type DiscoverOptions struct {
	NativeIDs []string // only these sessions
	Homes     []string // override the configured homes (tests)
}

// ProbeResult is the safe output of `agent probe`: no titles, no cwd, no text.
type ProbeResult struct {
	SchemaVersion int            `json:"schema_version"`
	Agent         string         `json:"agent"`
	Homes         []HomeProbe    `json:"homes"`
	Compatibility string         `json:"compatibility"` // ok | partial | unsupported
	Notes         []string       `json:"notes,omitempty"`
	Counts        map[string]int `json:"counts"`
}

// HomeProbe describes one agent home.
type HomeProbe struct {
	Exists       bool            `json:"exists"`
	ClientVer    string          `json:"client_version,omitempty"`
	Stores       map[string]bool `json:"stores"`       // named store -> present
	FileForms    map[string]int  `json:"file_forms"`   // "jsonl", "jsonl.zst", ...
	RecordTypes  map[string]int  `json:"record_types"` // histogram of record type names
	Capabilities []string        `json:"capabilities"` // the capability fingerprint (design §6.3)
}

// ---- capture ----

// Cursor is the adapter's resume state for one binding: one entry per stream
// artifact. It is persisted inside session.Binding.Artifacts.
type Cursor struct {
	Artifacts map[string]session.ArtifactState
}

// CaptureState is the outcome of one Capture call.
type CaptureState int

const (
	CaptureOK        CaptureState = iota // cursor may be committed
	CaptureUnsafe                        // corrupt record: cursor stays before it
	CaptureReplaced                      // prefix mismatch: a new generation is needed
	CaptureTruncated                     // file shorter than the cursor
	CaptureDetached                      // native files gone or moved and not found again
)

// ArtifactKey names the artifact a segment belongs to.
type ArtifactKey struct {
	Path       string // logical path (session.Artifact.Path)
	Kind       string // session.KindStream | session.KindBlob
	Form       string // session.FormPlain | session.FormZstd (streams)
	Generation int
	Mode       uint32
}

// SegmentSink receives captured bytes. The store implements it: it hashes the
// plaintext, seals it and returns the reference. Segments of one artifact are
// delivered in order and are contiguous.
type SegmentSink interface {
	// PutSegment stores bytes [start, end) of a stream artifact. The reader
	// yields exactly end-start bytes.
	PutSegment(ctx context.Context, k ArtifactKey, start, end int64, r io.Reader) (session.SegmentRef, error)
	// PutBlob stores an immutable file, chunked by the sink at MaxSegmentBytes.
	PutBlob(ctx context.Context, k ArtifactKey, size int64, r io.Reader) ([]session.SegmentRef, error)
}

// MaxSegmentBytes is the default segment size cap (design §8.2). A single
// record larger than this occupies a segment of its own.
const MaxSegmentBytes = 8 << 20

// CaptureResult is what Capture returns after streaming segments to the sink.
type CaptureResult struct {
	State  CaptureState
	Cursor Cursor // meaningful for CaptureOK only
	// Changed reports whether any new bytes were captured.
	Changed bool
	// Diags are counts and offsets only, never record text.
	Diags []Diag
}

// Diag is one non-sensitive diagnostic.
type Diag struct {
	Code   string // e.g. "unknown_record_type"
	Detail string // type name, artifact path, never content
	Count  int
	Offset int64
}

// SnapshotReader gives an adapter read access to one revision's files.
type SnapshotReader interface {
	Artifacts() []session.Artifact
	// Open returns the artifact's plaintext, segments concatenated in order.
	Open(ctx context.Context, path string) (io.ReadCloser, error)
}

// ---- installation (Writer) ----

// Mode selects what a Writer does with a revision.
type Mode int

const (
	ModeNative  Mode = iota + 1 // install the origin agent's own snapshot
	ModeConvert                 // convert a foreign snapshot into this agent's form
)

// Route is the write route of design §6.2.
type Route int

const (
	RouteOfficial Route = 1 // vendor import/resume interface
	RouteFiles    Route = 2 // session files only; the client registers them itself
	RouteRegistry Route = 3 // minimal row-level registry write (off by default)
)

// RunState is the client run-state; Unknown is treated as running.
type RunState int

const (
	NotRunning RunState = iota
	Running
	Unknown
)

// RunDetail refines RunState for display and for the support matrix.
type RunDetail string

const (
	DetailWindowOpen     RunDetail = "window_open"
	DetailProcessAlive   RunDetail = "process_alive"
	DetailBackgroundOnly RunDetail = "background_only"
	DetailNotRunning     RunDetail = "not_running"
	DetailUnknown        RunDetail = "unknown"
)

// RuntimeDetector reports whether the client could be writing the target.
// When thread-level information exists (Codex thread-writer-locks) the
// adapter passes the native id; otherwise it is "".
type RuntimeDetector interface {
	Running(ctx context.Context, home, nativeID string) (RunState, RunDetail, error)
}

// PathRule rewrites a path prefix (company /work/app -> home D:\code\app).
type PathRule struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// PathMap holds the rules of one project mapping. Apply uses the longest
// matching prefix on path-component boundaries; a path matching no rule is
// returned unchanged with ok=false.
type PathMap struct{ Rules []PathRule }

// TargetState is a read-only snapshot of the target taken by the caller.
type TargetState struct {
	Home          string
	ClientVersion string
	Run           RunState
	RunDetail     RunDetail
	// Existing lists the target files the plan may touch.
	Existing map[string]FileStat
	// BaselineHash is the hex SHA-256 over the canonical JSON of Existing plus
	// the registry rows the plan may touch; Apply re-computes it.
	BaselineHash string
}

// WriteInput is everything a Writer needs; it never reads the sync store.
type WriteInput struct {
	SessionID  string
	RevisionID string
	Mode       Mode
	Source     SnapshotReader
	// SourceProfile is the producer's profile (differs from Target for conversion).
	SourceProfile session.AgentProfile
	PathMap       PathMap
	Target        session.AgentProfile
	TargetState   TargetState
	// BindingID is the active binding being updated, "" for a first install.
	BindingID string
	// Cwd is the local project directory the session is bound to.
	Cwd string
}

// PlannedWrite is one file operation of a plan.
type PlannedWrite struct {
	Op     string `json:"op"`   // create | replace | append | row_insert | row_update
	Path   string `json:"path"` // relative to Home, or "<table>#<key>" for rows
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// Plan is the Writer's pure description of what Apply will do.
type Plan struct {
	SchemaVersion int             `json:"schema_version"`
	SessionID     string          `json:"session_id"`
	RevisionID    string          `json:"revision_id"`
	Mode          Mode            `json:"mode"`
	Route         Route           `json:"route"`
	Writes        []PlannedWrite  `json:"writes"`
	BaselineHash  string          `json:"baseline_hash"`
	Warnings      []string        `json:"warnings,omitempty"` // codes only
	Report        *FidelityReport `json:"report,omitempty"`   // conversion only
	// Digest is the hex SHA-256 of the canonical JSON of this Plan with Digest
	// empty; Apply refuses a plan whose digest does not match.
	Digest string `json:"digest"`
}

// KindCount is one row of the fidelity report.
type KindCount struct {
	Kept     int `json:"kept"`
	Rendered int `json:"rendered"` // converted to text
	Dropped  int `json:"dropped"`
}

// FidelityReport is the structured conversion report (design §7.2). It holds
// counts and type names only, never text.
type FidelityReport struct {
	SchemaVersion int                  `json:"schema_version"`
	Counts        map[string]KindCount `json:"counts"` // canonical kind -> counts
	VisibleItems  int                  `json:"visible_items"`
	TokenEstimate int                  `json:"token_estimate"`
	ContextNotes  []string             `json:"context_notes,omitempty"`
	EnvGaps       []string             `json:"env_gaps,omitempty"`
	DataEgress    bool                 `json:"data_egress"`
	Route         Route                `json:"route"`
	Dropped       map[string]int       `json:"dropped_types,omitempty"` // record type name -> count
}

// Receipt is returned by Apply and verified by Verify.
type Receipt struct {
	SessionID       string                 `json:"session_id"`
	RevisionID      string                 `json:"revision_id"`
	NativeSessionID string                 `json:"native_session_id"`
	InstallMode     string                 `json:"install_mode"`
	Written         []PlannedWrite         `json:"written"`
	Import          *session.ImportReceipt `json:"import,omitempty"`
	At              string                 `json:"at"`
}

// Tx is the slice of the store's transaction a Writer may use.
type Tx interface {
	// BackupDir returns the directory for this transaction's backups of vendor
	// files; the Writer copies every file it will replace or append to into it
	// BEFORE writing and records it with Backed.
	BackupDir() string
	Backed(relPath string) error
	// Wrote records a completed write so recovery can undo it.
	Wrote(w PlannedWrite) error
}

// Writer installs a revision into the agent's native store.
type Writer interface {
	// Plan has no side effects and does not take locks.
	Plan(ctx context.Context, in WriteInput) (Plan, error)
	// Apply executes the plan: backup, write, using tx for the journal. It
	// re-checks the run state after writing and returns errcode.ClientRunning
	// (after rolling back) when the client started meanwhile.
	Apply(ctx context.Context, in WriteInput, p Plan, tx Tx) (Receipt, error)
	// Verify re-reads the written files (and rows) and checks them against r.
	Verify(ctx context.Context, in WriteInput, r Receipt) error
	// Rollback restores everything recorded by tx.Backed / tx.Wrote.
	Rollback(ctx context.Context, home string, tx Tx) error
}

// ---- the adapter ----

// Adapter is one supported agent.
type Adapter interface {
	Name() string // "codex", "workbuddy"
	Probe(ctx context.Context) (ProbeResult, error)
	Discover(ctx context.Context, o DiscoverOptions) ([]NativeSession, error)
	// Capture streams new bytes of the session's artifacts to sink, starting
	// from cur, and returns the new cursor. It never writes to the native store.
	Capture(ctx context.Context, ref NativeRef, cur Cursor, sink SegmentSink) (CaptureResult, error)
	// Locate finds a session's files again by native id (after a move,
	// archive or compression). found=false means the files are gone.
	Locate(ctx context.Context, ref NativeRef) (NativeSession, bool, error)
	Runtime() RuntimeDetector
	Writer() Writer
}
