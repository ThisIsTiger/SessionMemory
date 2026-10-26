// Package device owns the per-device identity and counters under
// $SM_HOME/device (spec §3).
package device

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"

	"session-memory/internal/errcode"
	"session-memory/internal/platform"
	"session-memory/internal/session"
)

// Info is device.json. VaultID lets the device find its key in the credential
// store.
type Info struct {
	SchemaVersion     int      `json:"schema_version"`
	DeviceID          string   `json:"device_id"`
	VaultID           string   `json:"vault_id"`
	CreatedAt         string   `json:"created_at"`
	PreviousDeviceIDs []string `json:"previous_device_ids,omitempty"` // local unpublished history after clone recovery
}

type seqFile struct {
	SchemaVersion int   `json:"schema_version"`
	LastPublished int64 `json:"last_published"`
}

// Device accesses the files in a state directory.
type Device struct{ dir string }

// At returns the Device for a state root.
func At(root string) *Device { return &Device{dir: filepath.Join(root, "device")} }

func (d *Device) read(name string, v any) error {
	b, err := os.ReadFile(filepath.Join(d.dir, name))
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

func (d *Device) write(name string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return platform.AtomicWrite(filepath.Join(d.dir, name), append(b, '\n'), 0600)
}

// Init creates device.json and sequence.json. It refuses to overwrite.
func (d *Device) Init(vaultID, now string) (Info, error) {
	if _, err := d.Load(); err == nil {
		return Info{}, errcode.New(errcode.Conflict, "this device is already initialised")
	}
	info := Info{SchemaVersion: 1, DeviceID: session.NewDeviceID(), VaultID: vaultID, CreatedAt: now}
	if err := d.write("device.json", info); err != nil {
		return info, err
	}
	return info, d.write("sequence.json", seqFile{SchemaVersion: 1})
}

// Load reads device.json.
func (d *Device) Load() (Info, error) {
	var i Info
	if err := d.finishRegeneration(); err != nil {
		return i, err
	}
	if err := d.read("device.json", &i); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return i, errcode.New(errcode.NoVault, "this device is not initialised; run init or device join")
		}
		return i, err
	}
	if i.SchemaVersion > 1 {
		return i, errcode.New(errcode.UnknownFormat, "device.json written by a newer version")
	}
	return i, nil
}

// Regenerate replaces the device id (clone recovery); the sequence restarts at
// zero because a new id has no history.
func (d *Device) Regenerate() (Info, error) {
	i, err := d.Load()
	if err != nil {
		return i, err
	}
	// Publish journals contain ciphertext bound to the old identity. They can
	// be retired because revisions and outbox entries remain durable locally;
	// other unfinished operations must be recovered before changing identity.
	files, err := filepath.Glob(filepath.Join(d.dir, "..", "journal", "*.json"))
	if err != nil {
		return i, err
	}
	for _, p := range files {
		var j struct {
			Kind string `json:"kind"`
		}
		raw, err := os.ReadFile(p)
		if err != nil {
			return i, err
		}
		if json.Unmarshal(raw, &j) != nil || j.Kind != "publish" {
			return i, errcode.New(errcode.RecoveryNeeded, "recover unfinished transactions before regenerating the device id")
		}
	}
	old := i.DeviceID
	i.PreviousDeviceIDs = append(i.PreviousDeviceIDs, old)
	i.DeviceID = session.NewDeviceID()
	if err := d.write("regenerate.json", regeneration{OldID: old, Info: i, PublishJournals: files}); err != nil {
		return i, err
	}
	return i, d.finishRegeneration()
}

type regeneration struct {
	OldID           string   `json:"old_id"`
	Info            Info     `json:"info"`
	PublishJournals []string `json:"publish_journals"`
}

// finishRegeneration is idempotent: a restart completes the same migration,
// never creates another identity or resets an already active identity twice.
func (d *Device) finishRegeneration() error {
	var j regeneration
	if err := d.read("regenerate.json", &j); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if !session.ValidDeviceID(j.OldID) || !session.ValidDeviceID(j.Info.DeviceID) {
		return errcode.New(errcode.RecoveryNeeded, "invalid device migration")
	}
	files, err := filepath.Glob(filepath.Join(d.dir, "..", "bindings", "*.json"))
	if err != nil {
		return err
	}
	for _, p := range files {
		raw, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		var b session.Binding
		if err := json.Unmarshal(raw, &b); err != nil {
			return err
		}
		if b.DeviceID != j.OldID {
			continue
		}
		b.DeviceID = j.Info.DeviceID
		data, err := json.MarshalIndent(b, "", "  ")
		if err != nil {
			return err
		}
		if err := platform.AtomicWrite(p, append(data, '\n'), 0600); err != nil {
			return err
		}
	}
	entries, err := d.Outbox()
	if err != nil {
		return err
	}
	for idx := range entries {
		e := &entries[idx]
		if e.Receipt != nil && e.Receipt.DeviceID == j.OldID {
			e.Receipt.DeviceID = j.Info.DeviceID
		}
		if e.Choice != nil && e.Choice.DeviceID == j.OldID {
			e.Choice.DeviceID = j.Info.DeviceID
		}
		if e.ConvertRequest != nil && e.ConvertRequest.SourceDeviceID == j.OldID {
			e.ConvertRequest.SourceDeviceID = j.Info.DeviceID
		}
		if e.Binding != nil && e.Binding.DeviceID == j.OldID {
			e.Binding.DeviceID = j.Info.DeviceID
		}
	}
	if err := d.write("outbox.json", struct {
		Entries []session.Entry `json:"entries"`
	}{entries}); err != nil {
		return err
	}
	for _, p := range j.PublishJournals {
		dst := filepath.Join(d.dir, "..", "journal", "done", filepath.Base(p))
		if err := os.MkdirAll(filepath.Dir(dst), 0700); err != nil {
			return err
		}
		if err := os.Rename(p, dst); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if err := d.write("sequence.json", seqFile{SchemaVersion: 1}); err != nil {
		return err
	}
	if err := d.write("device.json", j.Info); err != nil {
		return err
	}
	return os.Remove(filepath.Join(d.dir, "regenerate.json"))
}

// LastPublished returns the highest manifest sequence this device published.
func (d *Device) LastPublished() (int64, error) {
	var s seqFile
	if err := d.read("sequence.json", &s); err != nil {
		return 0, err
	}
	return s.LastPublished, nil
}

// SetLastPublished records a published sequence.
func (d *Device) SetLastPublished(n int64) error {
	return d.write("sequence.json", seqFile{SchemaVersion: 1, LastPublished: n})
}

// Outbox holds entries (receipts, choices, requests) waiting for the next
// manifest.
func (d *Device) Outbox() ([]session.Entry, error) {
	var out struct {
		Entries []session.Entry `json:"entries"`
	}
	if err := d.read("outbox.json", &out); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	return out.Entries, nil
}

// AppendOutbox adds entries, skipping ones whose Key is already queued.
func (d *Device) AppendOutbox(entries ...session.Entry) error {
	cur, err := d.Outbox()
	if err != nil {
		return err
	}
	have := map[string]bool{}
	for _, e := range cur {
		have[e.Key()] = true
	}
	for _, e := range entries {
		if !have[e.Key()] {
			cur = append(cur, e)
			have[e.Key()] = true
		}
	}
	return d.write("outbox.json", struct {
		Entries []session.Entry `json:"entries"`
	}{cur})
}

// ClearOutbox removes the first n queued entries (those just published).
func (d *Device) ClearOutbox(n int) error {
	cur, err := d.Outbox()
	if err != nil {
		return err
	}
	if n > len(cur) {
		n = len(cur)
	}
	return d.write("outbox.json", struct {
		Entries []session.Entry `json:"entries"`
	}{cur[n:]})
}
