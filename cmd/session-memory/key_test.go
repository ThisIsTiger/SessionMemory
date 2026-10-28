package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"session-memory/internal/testutil/codexgen"
	"session-memory/internal/vault"
)

// A new computer joins with nothing but access to the cloud folder; afterwards
// the key can be taken out again without breaking the computers that joined.
func TestJoinFromCloudKeyThenRemoveIt(t *testing.T) {
	root := t.TempDir()
	shared := filepath.Join(root, "cloud")
	A := &machine{t: t, home: filepath.Join(root, "a"), keys: vault.MemStore{}}
	B := &machine{t: t, home: filepath.Join(root, "b"), keys: vault.MemStore{}}
	os.MkdirAll(A.home, 0700)
	os.MkdirAll(B.home, 0700)
	A.must("init")
	A.must("transport", "attach-folder", shared)
	keyPath := filepath.Join(shared, "public", "vault-key.json")
	if _, err := os.Stat(keyPath); err != nil {
		t.Fatalf("attach did not put the key in the folder: %v", err)
	}
	acodex := filepath.Join(A.home, "codex")
	A.must("config", "set", "agents.codex.home", acodex)
	tid := codexgen.ThreadIDFor(41)
	p := filepath.Join(acodex, "sessions", "2026", "09", "01", "rollout-2026-09-01T01-46-53-"+tid+".jsonl")
	os.MkdirAll(filepath.Dir(p), 0700)
	os.WriteFile(p, codexgen.Rollout(codexgen.Options{Seed: 41, Turns: 2}), 0600)
	A.must("track", "--native-session", tid)
	A.must("sync")

	// B has neither a bundle nor a passphrase
	if code, _, errs := B.run("device", "join", "--from-cloud"); code == 0 || !strings.Contains(errs, "transport") {
		t.Fatalf("join before a transport is configured: %d %s", code, errs)
	}
	B.must("config", "set", "transport.folder", shared)
	B.must("device", "join", "--from-cloud")
	// the first sync on B reads A's sessions with the key it just got, then says the key is still in the cloud
	code, _, errs := B.run("sync")
	if code != 0 || !strings.Contains(errs, "vault key is still stored") || !strings.Contains(errs, "key remove --yes") {
		t.Fatalf("first sync on B: exit %d, stderr %q", code, errs)
	}
	if out := B.must("list"); !strings.Contains(out, "received") {
		t.Fatalf("B cannot read A's session: %s", out)
	}
	// said once only
	if _, _, errs := B.run("sync"); strings.Contains(errs, "vault key is still stored") {
		t.Fatal("the removal offer repeated")
	}
	// nobody to ask: an agent or script must confirm explicitly
	if code, _, _ := B.run("key", "remove"); code == 0 {
		t.Fatal("key remove without --yes and without a person must refuse")
	}
	if _, err := os.Stat(keyPath); err != nil {
		t.Fatal("the key was removed without confirmation")
	}
	B.must("key", "remove", "--yes")
	if _, err := os.Stat(keyPath); err == nil {
		t.Fatal("key remove left the file")
	}
	if out := B.must("key", "status"); !strings.Contains(out, "not in the cloud") {
		t.Fatalf("status: %s", out)
	}
	// computers that already joined keep working; a new one cannot join until the key is back
	A.must("sync")
	B.must("sync")
	C := &machine{t: t, home: filepath.Join(root, "c"), keys: vault.MemStore{}}
	os.MkdirAll(C.home, 0700)
	C.must("config", "set", "transport.folder", shared)
	if code, _, errs := C.run("device", "join", "--from-cloud"); code == 0 || !strings.Contains(errs, "key publish") {
		t.Fatalf("join after removal: %d %s", code, errs)
	}
	A.must("key", "publish")
	C.must("device", "join", "--from-cloud")
}
