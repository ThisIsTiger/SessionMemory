package p0

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"session-memory/internal/errcode"
)

func mark(t *testing.T, dir string) {
	t.Helper()
	b, _ := json.Marshal(Marker{SchemaVersion: 1, CreatedAt: "2026-10-26T10:00:00.000Z", Purpose: "test", Owner: "session-memory-dev"})
	if err := os.WriteFile(filepath.Join(dir, MarkerName), b, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestRequireSandbox(t *testing.T) {
	base := t.TempDir()
	box := filepath.Join(base, "box")
	real := filepath.Join(base, "real-codex")
	for _, d := range []string{filepath.Join(box, "sessions"), filepath.Join(real, "sessions")} {
		if err := os.MkdirAll(d, 0700); err != nil {
			t.Fatal(err)
		}
	}
	mark(t, box)

	if err := RequireSandbox(filepath.Join(box, "sessions"), []string{real}); err != nil {
		t.Errorf("a marked sandbox was refused: %v", err)
	}
	if err := RequireSandbox(filepath.Join(box, "not", "yet", "created"), []string{real}); err != nil {
		t.Errorf("a not-yet-created path inside the sandbox was refused: %v", err)
	}
	for name, target := range map[string]string{
		"unmarked directory": real,
		"outside":            base,
	} {
		err := RequireSandbox(target, []string{real})
		if errcode.CodeOf(err) != errcode.NotIsolated {
			t.Errorf("%s: want not_isolated, got %v", name, err)
		}
	}

	// a symlink inside the sandbox that points at a real home must be refused
	link := filepath.Join(box, "escape")
	if err := os.Symlink(real, link); err != nil {
		t.Skip("symlinks unavailable: ", err)
	}
	if err := RequireSandbox(filepath.Join(link, "sessions"), []string{real}); errcode.CodeOf(err) != errcode.NotIsolated {
		t.Errorf("symlink escape accepted: %v", err)
	}

	// a marker whose owner is not ours does not count
	other := filepath.Join(base, "other")
	os.MkdirAll(other, 0700)
	os.WriteFile(filepath.Join(other, MarkerName), []byte(`{"owner":"someone-else"}`), 0600)
	if err := RequireSandbox(other, nil); errcode.CodeOf(err) != errcode.NotIsolated {
		t.Errorf("foreign marker accepted: %v", err)
	}

	// the user's home can never be a sandbox root, even when marked
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	mark(t, home)
	if err := RequireSandbox(filepath.Join(home, "x"), nil); errcode.CodeOf(err) != errcode.NotIsolated {
		t.Errorf("home as sandbox accepted: %v", err)
	}
}
