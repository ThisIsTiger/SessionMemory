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
	SchemaVersion int    `json:"schema_version"`
	DeviceID      string `json:"device_id"`
	VaultID       string `json:"vault_id"`
	CreatedAt     string `json:"created_at"`
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
	i.DeviceID = session.NewDeviceID()
	if err := d.write("device.json", i); err != nil {
		return i, err
	}
	return i, d.write("sequence.json", seqFile{SchemaVersion: 1})
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
