package device

import (
	"encoding/json"
	"os"
	"path/filepath"
	"session-memory/internal/session"
	"testing"
)

func writeFixture(t *testing.T, path string, v any) {
	t.Helper()
	os.MkdirAll(filepath.Dir(path), 0700)
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestRegenerateMigratesLocalStateWithoutRewritingHistory(t *testing.T) {
	root := t.TempDir()
	d := At(root)
	old, err := d.Init(session.NewVaultID(), "2026-10-26T00:00:00.000Z")
	if err != nil {
		t.Fatal(err)
	}
	b := session.Binding{BindingID: session.NewBindingID(), SessionID: session.NewSessionID(), DeviceID: old.DeviceID, State: session.StateActive}
	bp := filepath.Join(root, "bindings", b.BindingID+".json")
	writeFixture(t, bp, b)
	history := filepath.Join(root, "store", "revisions", "original.json")
	writeFixture(t, history, map[string]string{"producer_device_id": old.DeviceID})
	before, _ := os.ReadFile(history)
	if err := d.AppendOutbox(session.Entry{Type: session.EntryReceipt, Receipt: &session.ReceiptEntry{DeviceID: old.DeviceID, SessionID: b.SessionID}}); err != nil {
		t.Fatal(err)
	}
	journal := filepath.Join(root, "journal", session.NewTxID()+".json")
	writeFixture(t, journal, map[string]string{"kind": "publish", "state": "uploaded"})
	info, err := d.Regenerate()
	if err != nil {
		t.Fatal(err)
	}
	if info.DeviceID == old.DeviceID || len(info.PreviousDeviceIDs) != 1 || info.PreviousDeviceIDs[0] != old.DeviceID {
		t.Fatalf("info=%+v", info)
	}
	raw, _ := os.ReadFile(bp)
	json.Unmarshal(raw, &b)
	if b.DeviceID != info.DeviceID {
		t.Fatal("binding not migrated")
	}
	entries, err := d.Outbox()
	if err != nil || entries[0].Receipt.DeviceID != info.DeviceID {
		t.Fatalf("outbox=%+v %v", entries, err)
	}
	after, _ := os.ReadFile(history)
	if string(before) != string(after) {
		t.Fatal("immutable history was rewritten")
	}
	if _, err := os.Stat(journal); !os.IsNotExist(err) {
		t.Fatal("old publish journal not retired")
	}
	if err := d.SetLastPublished(7); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Load(); err != nil {
		t.Fatal(err)
	}
	if seq, _ := d.LastPublished(); seq != 7 {
		t.Fatal("loading completed migration reset sequence")
	}
}

func TestRegenerationResumesSameIdentity(t *testing.T) {
	root := t.TempDir()
	d := At(root)
	old, err := d.Init(session.NewVaultID(), "2026-10-26T00:00:00.000Z")
	if err != nil {
		t.Fatal(err)
	}
	next := old
	next.DeviceID = session.NewDeviceID()
	next.PreviousDeviceIDs = []string{old.DeviceID}
	if err := d.write("regenerate.json", regeneration{OldID: old.DeviceID, Info: next}); err != nil {
		t.Fatal(err)
	}
	got, err := d.Load()
	if err != nil || got.DeviceID != next.DeviceID {
		t.Fatalf("resumed identity=%+v %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(root, "device", "regenerate.json")); !os.IsNotExist(err) {
		t.Fatal("migration journal remains")
	}
}
