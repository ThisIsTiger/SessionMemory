package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"session-memory/internal/testutil/codexgen"
)

// With --json, stdout holds exactly one document even when the command fails
// after it has printed its result; the failure goes to stderr and the exit code.
func TestSyncFailureJSONIsOneDocument(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("file permissions do not stop root")
	}
	root := t.TempDir()
	shared := filepath.Join(root, "cloud")
	a := newComputer(t, root, "a", shared)
	a.must("init")
	a.must("transport", "attach-folder", shared)
	project := filepath.Join(a.home, "app")
	os.MkdirAll(project, 0700)
	addCodexSession(a, 995, project)
	tid := codexgen.ThreadIDFor(995)
	a.must("track", "--native-session", tid)
	p := filepath.Join(a.home, "codex/sessions/2026/09/01", "rollout-2026-09-01T01-46-53-"+tid+".jsonl")
	if err := os.Chmod(p, 0); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(p, 0600)
	code, out, errout := a.run("sync", "--json")
	if code == 0 {
		t.Fatalf("setup: unreadable source must fail: %s", errout)
	}
	dec := json.NewDecoder(strings.NewReader(out))
	var doc map[string]any
	if err := dec.Decode(&doc); err != nil || dec.More() {
		t.Fatalf("failed sync must emit one JSON document: %q (%v)", out, err)
	}
	if !strings.Contains(errout, "error:") {
		t.Fatalf("the failure must still be reported on stderr: %q", errout)
	}
	// a failure before any result is still the usual error document
	code, out, _ = a.run("--json", "config", "get", "no.such")
	var env struct{ Error struct{ Code string } }
	if code == 0 || json.Unmarshal([]byte(out), &env) != nil || env.Error.Code == "" {
		t.Fatalf("error envelope: %d %q", code, out)
	}
}
