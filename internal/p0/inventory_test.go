package p0

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"session-memory/internal/testutil/codexgen"
)

const sentinel = "SECRET-SENTINEL-123"

func TestScrubKeepsStructureAndRemovesSentinel(t *testing.T) {
	in := `{"timestamp":"2026-09-01T01:46:53.000Z","type":"response_item","payload":{"type":"message","role":"user","id":"0192f3a4-0000-7000-8000-000000000001","content":[{"type":"input_text","text":"` + sentinel + ` hello"}],"encrypted_content":"c2VjcmV0","n":42,"ok":true,"none":null}}` + "\n" +
		`{"type":"x","payload":{"nested":{"deep":["` + sentinel + `",{"k":"` + sentinel + `"}]}}}` + "\n"
	var out bytes.Buffer
	if err := Scrub(strings.NewReader(in), &out, ScrubOptions{Seed: "s", Shift: 48 * time.Hour}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), sentinel) || strings.Contains(out.String(), "hello") || strings.Contains(out.String(), "c2VjcmV0") {
		t.Fatalf("content leaked: %s", out.String())
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("line count %d", len(lines))
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &m); err != nil {
		t.Fatalf("output is not JSON: %v", err)
	}
	p := m["payload"].(map[string]any)
	if p["type"] != "message" || p["role"] != "user" || p["n"] != float64(42) || p["ok"] != true || p["none"] != nil {
		t.Errorf("structure/allowlist not preserved: %v", p)
	}
	if m["timestamp"] != "2026-09-03T01:46:53.000Z" {
		t.Errorf("timestamp shift: %v", m["timestamp"])
	}
	if p["id"] == "0192f3a4-0000-7000-8000-000000000001" || len(p["id"].(string)) != 36 {
		t.Errorf("uuid not mapped: %v", p["id"])
	}
	// reproducible
	var out2 bytes.Buffer
	Scrub(strings.NewReader(in), &out2, ScrubOptions{Seed: "s", Shift: 48 * time.Hour})
	if out.String() != out2.String() {
		t.Error("scrub is not deterministic")
	}
}

func TestCollectInventoryIsStructureOnly(t *testing.T) {
	home := t.TempDir()
	tid := codexgen.ThreadIDFor(1)
	p := filepath.Join(home, "sessions", "2026", "09", "01", "rollout-2026-09-01T01-46-53-"+tid+".jsonl")
	os.MkdirAll(filepath.Dir(p), 0700)
	os.WriteFile(p, codexgen.Rollout(codexgen.Options{Seed: 1, Cwd: "/w/" + sentinel, Ordinals: true, UILayer: true}), 0600)
	z := filepath.Join(home, "archived_sessions", "rollout-2026-08-01T01-46-53-"+codexgen.ThreadIDFor(2)+".jsonl.zst")
	os.MkdirAll(filepath.Dir(z), 0700)
	os.WriteFile(z, codexgen.Zstd(codexgen.Rollout(codexgen.Options{Seed: 2})), 0600)
	os.WriteFile(filepath.Join(home, "auth.json"), []byte(sentinel), 0600)
	os.MkdirAll(filepath.Join(home, "credentials"), 0700)
	os.WriteFile(filepath.Join(home, "credentials", "k"), []byte(sentinel), 0600)

	dbp := filepath.Join(home, "state_5.sqlite")
	db, _ := sql.Open("sqlite", dbp)
	db.Exec(`CREATE TABLE threads(id TEXT PRIMARY KEY, title TEXT NOT NULL)`)
	db.Exec(`INSERT INTO threads VALUES('a','` + sentinel + `')`)
	db.Close()

	inv, err := Collect(context.Background(), "codex", home)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(inv)
	if strings.Contains(string(b), sentinel) || strings.Contains(string(b), tid) {
		t.Fatalf("inventory leaked content: %s", b)
	}
	if len(inv.SQLite) != 1 || inv.SQLite[0].Tables[0].Name != "threads" || inv.SQLite[0].Tables[0].Rows != 1 || len(inv.SQLite[0].Tables[0].Columns) != 2 {
		t.Errorf("sqlite: %+v", inv.SQLite)
	}
	var sessions, archived *JSONLInfo
	for i := range inv.JSONL {
		switch inv.JSONL[i].Glob {
		case "sessions/**":
			sessions = &inv.JSONL[i]
		case "archived_sessions/**":
			archived = &inv.JSONL[i]
		}
	}
	if sessions == nil || archived == nil || archived.Forms["zstd"] != 1 || sessions.ByType["event_msg/item_completed"] == 0 {
		t.Fatalf("jsonl groups: %+v", inv.JSONL)
	}
	foundSkip := false
	for _, d := range inv.Dirs {
		if d.Ext["(skipped)"] == 1 {
			foundSkip = true
		}
	}
	if !foundSkip {
		t.Error("credential entries must be listed as skipped, never read")
	}
}
