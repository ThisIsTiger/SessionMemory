//go:build failpoint

package main

import (
	"os"
	"path/filepath"
	"session-memory/internal/testutil/codexgen"
	"session-memory/internal/vault"
	"testing"
)

func TestRegressionStartupResumesSameManifest(t *testing.T) {
	root := t.TempDir()
	shared := filepath.Join(root, "cloud")
	a := &machine{t: t, home: filepath.Join(root, "a"), keys: vault.MemStore{}}
	os.MkdirAll(a.home, 0700)
	a.must("init")
	a.must("transport", "attach-folder", shared)
	home := filepath.Join(a.home, "codex")
	a.must("config", "set", "agents.codex.home", home)
	tid := codexgen.ThreadIDFor(504)
	p := filepath.Join(home, "sessions", "2026", "09", "01", "rollout-2026-09-01T01-46-53-"+tid+".jsonl")
	os.MkdirAll(filepath.Dir(p), 0700)
	os.WriteFile(p, codexgen.Rollout(codexgen.Options{Seed: 504, Turns: 2}), 0600)
	a.must("track", "--native-session", tid)
	t.Setenv("SM_FAILPOINTS", "publish.after_manifest_published=error")
	code, _, _ := a.run("sync")
	if code == 0 {
		t.Fatal("expected failpoint")
	}
	before, _ := filepath.Glob(filepath.Join(shared, "manifests", "*", "*.manifest"))
	if len(before) != 1 {
		t.Fatalf("before=%v", before)
	}
	t.Setenv("SM_FAILPOINTS", "")
	a.must("sync")
	after, _ := filepath.Glob(filepath.Join(shared, "manifests", "*", "000000000001-*.manifest"))
	if len(after) != 1 {
		t.Fatalf("restart resealed sequence 1 instead of resuming: %v", after)
	}
	pending, _ := filepath.Glob(filepath.Join(a.home, "sm", "journal", "*.json"))
	if len(pending) != 0 {
		t.Fatalf("unrecovered journals: %v", pending)
	}
}
