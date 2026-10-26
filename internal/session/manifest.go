package session

import (
	"fmt"
	"sort"
)

// Entry types of a manifest (design §11.4).
const (
	EntryRevision       = "revision"
	EntryReceipt        = "receipt"
	EntryChoice         = "choice"
	EntryConvertRequest = "convert-request"
	EntryBinding        = "binding"
)

// Receipt statuses.
const (
	ReceiptReceived  = "received"
	ReceiptInstalled = "installed"
)

// Convert-request modes.
const (
	ModeConvert = "convert"
	ModeContext = "context"
)

// Manifest is the plaintext payload of one published manifest object. Its
// sealed form is the file manifests/<device_id>/<seq>-<hash12>.manifest.
type Manifest struct {
	SchemaVersion int    `json:"schema_version"`
	DeviceID      string `json:"device_id"`
	// Sequence starts at 1 and grows by exactly 1 per published manifest of
	// this device.
	Sequence int64 `json:"sequence"`
	// PrevManifestHash is the hex SHA-256 of the previous manifest FILE of this
	// device (sealed bytes); empty for sequence 1.
	PrevManifestHash string  `json:"prev_manifest_hash,omitempty"`
	CreatedAt        string  `json:"created_at"`
	Entries          []Entry `json:"entries"`
}

// MaxManifestEntries bounds one manifest; larger batches are split.
const MaxManifestEntries = 200

// Entry is a tagged union: Type selects which pointer is set.
type Entry struct {
	Type           string               `json:"type"`
	Revision       *RevisionEntry       `json:"revision,omitempty"`
	Receipt        *ReceiptEntry        `json:"receipt,omitempty"`
	Choice         *Choice              `json:"choice,omitempty"`
	ConvertRequest *ConvertRequestEntry `json:"convert_request,omitempty"`
	Binding        *BindingEntry        `json:"binding,omitempty"`
}

// RevisionEntry publishes a revision and the objects it introduced (those not
// already referenced by its parent's snapshot). A receiver must hold every
// object of the snapshot before importing the revision (design §11.2).
type RevisionEntry struct {
	Revision Revision `json:"revision"`
	Objects  []string `json:"objects"` // ObjectRefs, sorted
}

// ReceiptEntry is published by the TARGET device only.
type ReceiptEntry struct {
	SessionID   string `json:"session_id"`
	RevisionID  string `json:"revision_id"`
	DeviceID    string `json:"device_id"`
	Status      string `json:"status"`
	InstallMode string `json:"install_mode,omitempty"` // for installed
	BindingID   string `json:"binding_id,omitempty"`
	At          string `json:"at"`
}

// ConvertRequestEntry is what `continue --device` publishes.
type ConvertRequestEntry struct {
	RequestID      string `json:"request_id"`
	SessionID      string `json:"session_id"`
	RevisionID     string `json:"revision_id"`
	SourceDeviceID string `json:"source_device_id"`
	TargetAgent    string `json:"target_agent"`
	TargetDeviceID string `json:"target_device_id"`
	Mode           string `json:"mode"`
	At             string `json:"at"`
}

// BindingEntry is the synced summary of a binding: enough to rebuild a
// device's bindings after a reinstall (E8). It carries no local paths or
// cursors.
type BindingEntry struct {
	Version             int            `json:"version"` // grows with every change of this binding
	BindingID           string         `json:"binding_id"`
	SessionID           string         `json:"session_id"`
	DeviceID            string         `json:"device_id"`
	AgentProfile        AgentProfile   `json:"agent_profile"`
	NativeSessionID     string         `json:"native_session_id"`
	InstallMode         string         `json:"install_mode"`
	State               string         `json:"state"`
	BaseRevisionID      string         `json:"base_revision_id,omitempty"`
	InstalledRevisionID string         `json:"installed_revision_id,omitempty"`
	ImportReceipt       *ImportReceipt `json:"import_receipt,omitempty"`
}

// Validate checks the union and the fields every consumer relies on.
func (e Entry) Validate() error {
	set := 0
	for _, p := range []bool{e.Revision != nil, e.Receipt != nil, e.Choice != nil, e.ConvertRequest != nil, e.Binding != nil} {
		if p {
			set++
		}
	}
	if set != 1 {
		return fmt.Errorf("entry: exactly one payload must be set, got %d", set)
	}
	switch e.Type {
	case EntryRevision:
		if e.Revision == nil {
			break
		}
		if err := e.Revision.Revision.Validate(); err != nil {
			return err
		}
		if !sort.StringsAreSorted(e.Revision.Objects) {
			return fmt.Errorf("entry: objects must be sorted")
		}
		for _, o := range e.Revision.Objects {
			if !ValidObjectRef(o) {
				return fmt.Errorf("entry: bad object ref %q", o)
			}
		}
		return nil
	case EntryReceipt:
		if r := e.Receipt; r != nil {
			if !ValidSessionID(r.SessionID) || !ValidRevisionID(r.RevisionID) || !ValidDeviceID(r.DeviceID) {
				return fmt.Errorf("entry: bad receipt ids")
			}
			if r.Status != ReceiptReceived && r.Status != ReceiptInstalled {
				return fmt.Errorf("entry: bad receipt status %q", r.Status)
			}
			return nil
		}
	case EntryChoice:
		if c := e.Choice; c != nil {
			if !ValidSessionID(c.SessionID) || !ValidRevisionID(c.Chosen) || !ValidDeviceID(c.DeviceID) {
				return fmt.Errorf("entry: bad choice ids")
			}
			if !sort.StringsAreSorted(c.SeenHeads) {
				return fmt.Errorf("entry: seen_heads must be sorted")
			}
			found := false
			for _, h := range c.SeenHeads {
				found = found || h == c.Chosen
			}
			if !found {
				return fmt.Errorf("entry: chosen must be among seen_heads")
			}
			return nil
		}
	case EntryConvertRequest:
		if c := e.ConvertRequest; c != nil {
			if !ValidRequestID(c.RequestID) || !ValidSessionID(c.SessionID) || !ValidRevisionID(c.RevisionID) ||
				!ValidDeviceID(c.SourceDeviceID) || !ValidDeviceID(c.TargetDeviceID) {
				return fmt.Errorf("entry: bad convert-request ids")
			}
			if c.Mode != ModeConvert && c.Mode != ModeContext {
				return fmt.Errorf("entry: bad convert-request mode %q", c.Mode)
			}
			return nil
		}
	case EntryBinding:
		if b := e.Binding; b != nil {
			if !ValidBindingID(b.BindingID) || !ValidSessionID(b.SessionID) || !ValidDeviceID(b.DeviceID) || b.Version < 1 {
				return fmt.Errorf("entry: bad binding summary")
			}
			return nil
		}
	default:
		return fmt.Errorf("entry: unknown type %q", e.Type)
	}
	return fmt.Errorf("entry: type %q does not match its payload", e.Type)
}

// Key is the idempotency key: entries with the same Key are the same fact and
// are stored once (design §11.4).
func (e Entry) Key() string {
	switch {
	case e.Revision != nil:
		return "revision:" + e.Revision.Revision.RevisionID
	case e.Receipt != nil:
		r := e.Receipt
		return "receipt:" + r.DeviceID + ":" + r.RevisionID + ":" + r.Status
	case e.Choice != nil:
		c := e.Choice
		return "choice:" + c.SessionID + ":" + c.Chosen + ":" + fmt.Sprint(c.SeenHeads) + ":" + c.DeviceID + ":" + c.At
	case e.ConvertRequest != nil:
		return "convert-request:" + e.ConvertRequest.RequestID
	case e.Binding != nil:
		return fmt.Sprintf("binding:%s:%d", e.Binding.BindingID, e.Binding.Version)
	}
	return ""
}

// ManifestFileName is the file name under manifests/<device_id>/: a 12-digit
// zero-padded sequence, then the first 12 hex characters of the SHA-256 of the
// sealed file.
func ManifestFileName(sequence int64, fileSHA256Hex string) string {
	return fmt.Sprintf("%012d-%s.manifest", sequence, fileSHA256Hex[:12])
}
