package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"session-memory/internal/testutil/codexgen"
	"session-memory/internal/vault"
	"strings"
	"testing"
)

func TestRegressionRegenerateIDKeepsTrackedBindings(t *testing.T) {
	root := t.TempDir()
	a := &machine{t: t, home: filepath.Join(root, "a"), keys: vault.MemStore{}}
	os.MkdirAll(a.home, 0700)
	a.must("init")
	home := filepath.Join(a.home, "codex")
	a.must("config", "set", "agents.codex.home", home)
	tid := codexgen.ThreadIDFor(506)
	p := filepath.Join(home, "sessions", "2026", "09", "01", "rollout-2026-09-01T01-46-53-"+tid+".jsonl")
	os.MkdirAll(filepath.Dir(p), 0700)
	os.WriteFile(p, codexgen.Rollout(codexgen.Options{Seed: 506, Turns: 2}), 0600)
	var tr struct {
		SessionID string `json:"session_id"`
	}
	json.Unmarshal([]byte(a.must("track", "--native-session", tid, "--json")), &tr)
	a.must("device", "regenerate-id")
	code, out, errout := a.run("checkpoint", tr.SessionID)
	if code != 0 {
		t.Fatalf("regenerating device id broke existing tracked session: exit=%d out=%s err=%s", code, out, errout)
	}
}

func TestRegressionReceivedSessionAppearsInList(t *testing.T) {
	root := t.TempDir()
	shared := filepath.Join(root, "cloud")
	a := &machine{t: t, home: filepath.Join(root, "a"), keys: vault.MemStore{}}
	b := &machine{t: t, home: filepath.Join(root, "b"), keys: vault.MemStore{}}
	os.MkdirAll(a.home, 0700)
	os.MkdirAll(b.home, 0700)
	a.must("init")
	a.must("transport", "attach-folder", shared)
	home := filepath.Join(a.home, "codex")
	a.must("config", "set", "agents.codex.home", home)
	tid := codexgen.ThreadIDFor(507)
	p := filepath.Join(home, "sessions", "2026", "09", "01", "rollout-2026-09-01T01-46-53-"+tid+".jsonl")
	os.MkdirAll(filepath.Dir(p), 0700)
	os.WriteFile(p, codexgen.Rollout(codexgen.Options{Seed: 507, Turns: 2}), 0600)
	var tr struct {
		SessionID string `json:"session_id"`
	}
	json.Unmarshal([]byte(a.must("track", "--native-session", tid, "--json")), &tr)
	a.must("sync")
	pass := filepath.Join(root, "pass")
	os.WriteFile(pass, []byte("review-passphrase-only"), 0600)
	bundle := filepath.Join(root, "join.bundle")
	a.must("device", "invite", "--out", bundle, "--passphrase-file", pass)
	b.must("device", "join", bundle, "--passphrase-file", pass)
	b.must("transport", "attach-folder", shared)
	b.must("sync")
	out := b.must("list", "--json")
	if !strings.Contains(out, tr.SessionID) {
		t.Fatalf("received session is invisible without prior restore: list=%s", out)
	}
}
