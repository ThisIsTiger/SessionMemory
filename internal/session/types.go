// Package session holds the protocol's data types and its pure logic
// (revision identity, head/conflict resolution, install landing). It does no
// I/O and imports no other internal package except canonjson.
package session

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"
	"time"

	"session-memory/internal/canonjson"
)

// SchemaVersion is the version of every persisted or synced JSON document in
// this package.
const SchemaVersion = 1

// ---- identifiers ----

var (
	reSession  = regexp.MustCompile(`^ss_[a-z2-7]{26}$`)
	reDevice   = regexp.MustCompile(`^dev_[a-z2-7]{16}$`)
	reBinding  = regexp.MustCompile(`^bind_[a-z2-7]{16}$`)
	reTx       = regexp.MustCompile(`^tx_[a-z2-7]{16}$`)
	reRevision = regexp.MustCompile(`^rev_[0-9a-f]{64}$`)
	reObject   = regexp.MustCompile(`^obj_[0-9a-f]{64}$`)
	reRequest  = regexp.MustCompile(`^req_[a-z2-7]{16}$`)
	reVault    = regexp.MustCompile(`^vlt_[a-z2-7]{26}$`)
	reProject  = regexp.MustCompile(`^prj_[a-z2-7]{16}$`)
)

var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

func randomID(prefix string, nbytes int) string {
	b := make([]byte, nbytes)
	if _, err := rand.Read(b); err != nil {
		panic(err) // the system RNG failing is not recoverable
	}
	return prefix + strings.ToLower(b32.EncodeToString(b))
}

func NewSessionID() string  { return randomID("ss_", 16) }
func NewDeviceID() string   { return randomID("dev_", 10) }
func NewBindingID() string  { return randomID("bind_", 10) }
func NewTxID() string       { return randomID("tx_", 10) }
func NewRequestID() string  { return randomID("req_", 10) }
func NewVaultID() string    { return randomID("vlt_", 16) }
func NewProjectRef() string { return randomID("prj_", 10) }

func ValidSessionID(s string) bool  { return reSession.MatchString(s) }
func ValidDeviceID(s string) bool   { return reDevice.MatchString(s) }
func ValidBindingID(s string) bool  { return reBinding.MatchString(s) }
func ValidTxID(s string) bool       { return reTx.MatchString(s) }
func ValidRequestID(s string) bool  { return reRequest.MatchString(s) }
func ValidVaultID(s string) bool    { return reVault.MatchString(s) }
func ValidProjectRef(s string) bool { return reProject.MatchString(s) }
func ValidRevisionID(s string) bool { return reRevision.MatchString(s) }
func ValidObjectRef(s string) bool  { return reObject.MatchString(s) }

// ObjectRefOf builds an object reference from the hex SHA-256 of the sealed
// (ciphertext) object file.
func ObjectRefOf(sealedSHA256Hex string) string { return "obj_" + sealedSHA256Hex }

// ---- time ----

// TimeLayout is the only timestamp format in synced documents: UTC with
// millisecond precision.
const TimeLayout = "2006-01-02T15:04:05.000Z"

func FormatTime(t time.Time) string { return t.UTC().Format(TimeLayout) }

func ParseTime(s string) (time.Time, error) { return time.Parse(TimeLayout, s) }

// ---- agent profile ----

// AgentProfile identifies the producer or target client precisely enough to
// choose a compatibility-matrix entry.
type AgentProfile struct {
	Agent          string `json:"agent"`   // codex | workbuddy | claude | ...
	Surface        string `json:"surface"` // desktop | cli | vscode | ...
	Version        string `json:"version,omitempty"`
	Format         string `json:"format,omitempty"` // adapter-defined storage format tag
	AdapterVersion string `json:"adapter_version"`
	OS             string `json:"os"` // darwin | linux | windows
}

// ---- snapshot ----

// Artifact kinds.
const (
	KindStream = "stream" // append-only JSONL, addressed by decompressed byte offsets
	KindBlob   = "blob"   // immutable file
)

// Source forms of a stream artifact (informational).
const (
	FormPlain = "plain"
	FormZstd  = "zstd"
)

// SegmentRef is one immutable chunk of an artifact: bytes [Start, End) of its
// (decompressed) content, stored as one sealed object.
type SegmentRef struct {
	Start   int64  `json:"start"`
	End     int64  `json:"end"`
	RawHash string `json:"raw_hash"` // hex SHA-256 of the plaintext bytes
	Object  string `json:"object"`   // ObjectRef of the sealed segment
}

// Artifact is one file of a native snapshot.
type Artifact struct {
	// Path is relative to the agent home, slash-separated, never starts with
	// '/' or contains ".." or a drive letter. For a Codex rollout it is the
	// logical .jsonl name even when the file on disk is .jsonl.zst.
	Path       string       `json:"path"`
	Kind       string       `json:"kind"`
	Form       string       `json:"form,omitempty"` // streams only
	Generation int          `json:"generation"`
	Mode       uint32       `json:"mode"` // permission bits to recreate, informational
	Size       int64        `json:"size"` // total plaintext bytes
	Digest     string       `json:"segments_digest"`
	Segments   []SegmentRef `json:"segments"` // contiguous from 0, ordered
}

// SegmentsDigest is the hex SHA-256 over the concatenated raw_hash hex strings
// of segs, in order. It identifies the whole plaintext without a running
// hash state: extending an artifact only appends segments. Deep verification
// re-hashes each segment's byte range of the source and compares raw_hash.
func SegmentsDigest(segs []SegmentRef) string {
	h := sha256.New()
	for _, s := range segs {
		h.Write([]byte(s.RawHash))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// Snapshot is the sealed object a revision points to: the complete file set
// of the native session at capture time.
type Snapshot struct {
	SchemaVersion int        `json:"schema_version"`
	Artifacts     []Artifact `json:"artifacts"` // sorted by Path
}

// ---- revision ----

// Capture states of a revision.
const (
	CaptureCheckpoint = "checkpoint" // user ran checkpoint
	CaptureAuto       = "auto"       // background capture at a stable boundary
)

// Revision is an immutable version of one synced session. Its ID is the
// SHA-256 of its canonical JSON without the revision_id field.
type Revision struct {
	SchemaVersion      int          `json:"schema_version"`
	RevisionID         string       `json:"revision_id,omitempty"`
	SessionID          string       `json:"session_id"`
	ProjectRef         string       `json:"project_ref,omitempty"`      // label only (design §10)
	OriginNativeID     string       `json:"origin_native_id,omitempty"` // root revision only
	ParentRevisionID   string       `json:"parent_revision_id,omitempty"`
	ReplacesRevisionID string       `json:"replaces_revision_id,omitempty"` // set when a generation bump replaced an earlier snapshot
	ProducerDeviceID   string       `json:"producer_device_id"`
	ProducerBindingID  string       `json:"producer_binding_id"`
	AgentProfile       AgentProfile `json:"agent_profile"`
	NativeSnapshotRef  string       `json:"native_snapshot_ref"`
	RepoStateRef       string       `json:"repo_state_ref,omitempty"`
	CaptureState       string       `json:"capture_state"`
	CreatedAt          string       `json:"created_at"`
}

// ComputeID returns the revision ID for r, ignoring r.RevisionID.
func (r Revision) ComputeID() (string, error) {
	r.RevisionID = ""
	h, err := canonjson.Hash(r)
	if err != nil {
		return "", err
	}
	return "rev_" + h, nil
}

// Seal fills in RevisionID.
func (r *Revision) Seal() error {
	id, err := r.ComputeID()
	if err != nil {
		return err
	}
	r.RevisionID = id
	return nil
}

// Validate checks structure and that RevisionID matches the content.
func (r Revision) Validate() error {
	switch {
	case r.SchemaVersion != SchemaVersion:
		return fmt.Errorf("revision: unsupported schema_version %d", r.SchemaVersion)
	case !ValidSessionID(r.SessionID):
		return fmt.Errorf("revision: bad session_id %q", r.SessionID)
	case r.ProjectRef != "" && !ValidProjectRef(r.ProjectRef):
		return fmt.Errorf("revision: bad project_ref")
	case r.ParentRevisionID != "" && !ValidRevisionID(r.ParentRevisionID):
		return fmt.Errorf("revision: bad parent_revision_id")
	case r.ReplacesRevisionID != "" && !ValidRevisionID(r.ReplacesRevisionID):
		return fmt.Errorf("revision: bad replaces_revision_id")
	case !ValidDeviceID(r.ProducerDeviceID) || !ValidBindingID(r.ProducerBindingID):
		return fmt.Errorf("revision: bad producer ids")
	case !ValidObjectRef(r.NativeSnapshotRef):
		return fmt.Errorf("revision: bad native_snapshot_ref")
	case r.RepoStateRef != "" && !ValidObjectRef(r.RepoStateRef):
		return fmt.Errorf("revision: bad repo_state_ref")
	case r.CaptureState != CaptureCheckpoint && r.CaptureState != CaptureAuto:
		return fmt.Errorf("revision: bad capture_state %q", r.CaptureState)
	}
	if _, err := ParseTime(r.CreatedAt); err != nil {
		return fmt.Errorf("revision: bad created_at: %w", err)
	}
	want, err := r.ComputeID()
	if err != nil {
		return err
	}
	if r.RevisionID != want {
		return fmt.Errorf("revision: revision_id does not match content")
	}
	return nil
}

// ---- device-local binding ----

// Install modes of a binding.
const (
	InstallNative    = "native"
	InstallConverted = "converted"
	InstallContext   = "context"
)

// Binding states.
const (
	StateActive     = "active"
	StateSuperseded = "superseded"
	StateDetached   = "detached"
)

// StreamCursor mirrors jsonl.Cursor field for field (same JSON), so this
// package stays free of other internal imports.
type StreamCursor struct {
	Generation int    `json:"generation"`
	Offset     int64  `json:"offset"`
	FileID     string `json:"file_id,omitempty"`
	HeadHash   string `json:"head_hash,omitempty"`
	TailHash   string `json:"tail_hash,omitempty"`
}

// ArtifactState is what a binding remembers about one captured artifact.
type ArtifactState struct {
	Kind     string        `json:"kind"`
	Form     string        `json:"form,omitempty"`
	Mode     uint32        `json:"mode"`
	Cursor   *StreamCursor `json:"cursor,omitempty"` // streams only
	Segments []SegmentRef  `json:"segments"`         // current generation only
	Size     int64         `json:"size"`
	Digest   string        `json:"segments_digest"` // SegmentsDigest(Segments)
}

// ImportReceipt records what a Writer put into a target so the capture side
// can tell the imported prefix from the user's later records (design §8.3).
type ImportReceipt struct {
	WriterVersion     string   `json:"writer_version"`
	SourceRevision    string   `json:"source_revision"`
	WrittenCount      int64    `json:"written_count"`   // records written
	LastMarker        string   `json:"last_marker"`     // id or ordinal of the last written record
	BoundaryOffset    int64    `json:"boundary_offset"` // byte offset (decompressed) just after the last written record
	BoundaryHash      string   `json:"boundary_hash"`   // hex SHA-256 of bytes [0, BoundaryOffset)
	At                string   `json:"at"`
	CodeStateOverride []string `json:"code_state_override,omitempty"`
}

// UnsafeMark records where capture stopped at a corrupt record. Capture of the
// binding is paused until the user runs `verify`; revisions before the mark
// stay valid.
type UnsafeMark struct {
	Path   string `json:"path"`
	Offset int64  `json:"offset"`
	At     string `json:"at"`
}

// ProjectMap is projects/<project_ref>.json: device-local, never synced.
type ProjectMap struct {
	SchemaVersion int    `json:"schema_version"`
	ProjectRef    string `json:"project_ref"` // prj_<base32>
	Label         string `json:"label,omitempty"`
	LocalPath     string `json:"local_path"`
	// Rules rewrite recorded cwd prefixes to this device's paths (origin
	// /work/app -> local D:\code\app).
	Rules         []PathRuleJSON `json:"rules"`
	GitRemoteHash string         `json:"git_remote_hash,omitempty"` // sha256 of the normalised remote URL
	// Watch lists the agents whose new sessions under LocalPath are tracked
	// automatically by sync (set by project track; device-local).
	Watch []string `json:"watch,omitempty"`
}

// PathRuleJSON is the persisted form of agent.PathRule.
type PathRuleJSON struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// Binding maps a synced session to one native session on this device. It is
// the device's source of truth for cursors and receipts.
type Binding struct {
	SchemaVersion       int                      `json:"schema_version"`
	BindingID           string                   `json:"binding_id"`
	SessionID           string                   `json:"session_id"`
	DeviceID            string                   `json:"device_id"`
	AgentProfile        AgentProfile             `json:"agent_profile"`
	NativeSessionID     string                   `json:"native_session_id"`
	NativeHome          string                   `json:"native_home"` // device-local, never synced
	LocalCwd            string                   `json:"local_cwd,omitempty"`
	InstallMode         string                   `json:"install_mode"`
	State               string                   `json:"state"`
	BaseRevisionID      string                   `json:"base_revision_id,omitempty"`
	InstalledRevisionID string                   `json:"installed_revision_id,omitempty"`
	Artifacts           map[string]ArtifactState `json:"artifacts"` // key = Artifact.Path
	ImportReceipt       *ImportReceipt           `json:"import_receipt,omitempty"`
	FidelityReportRef   string                   `json:"fidelity_report_ref,omitempty"`
	Unsafe              *UnsafeMark              `json:"unsafe,omitempty"`
	CreatedAt           string                   `json:"created_at"`
	UpdatedAt           string                   `json:"updated_at"`
}
