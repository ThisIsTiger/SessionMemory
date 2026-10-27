package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"session-memory/internal/config"
	"session-memory/internal/source/codex"
)

func TestCodexAssignWaitsForAppToCloseThenFilesThread(t *testing.T) {
	state, home := t.TempDir(), t.TempDir()
	cfg := config.DefaultConfig()
	cfg.Home = state
	a := &App{Cfg: cfg, Out: &bytes.Buffer{}, Err: &bytes.Buffer{}}

	os.WriteFile(filepath.Join(home, ".codex-global-state.json"),
		[]byte(`{"local-projects":{"p":{"id":"p","name":"P","rootPaths":["/w"]}},"projectless-thread-ids":["t1"]}`), 0600)
	dir := filepath.Join(home, "sessions", "2026", "07", "01")
	os.MkdirAll(dir, 0755)
	os.WriteFile(filepath.Join(dir, "rollout-2026-07-01T00-00-00-t1.jsonl"), []byte(`{"payload":{"cwd":"/w/sub"}}`+"\n"), 0644)

	running := true
	old := codex.AppRunning
	codex.AppRunning = func() bool { return running }
	defer func() { codex.AppRunning = old }()

	queueCodexAssign(a, home, []string{"t1"})
	if !strings.Contains(a.Err.(*bytes.Buffer).String(), "next time sync runs while Codex is closed") {
		t.Errorf("no waiting note: %q", a.Err.(*bytes.Buffer).String())
	}
	if len(loadPendingAssign(a)) != 1 {
		t.Fatal("thread must stay queued while the app runs")
	}

	running = false
	flushCodexAssign(a, false) // what a background sync does
	if len(loadPendingAssign(a)) != 0 {
		t.Fatal("queue should be empty after filing")
	}
	b, _ := os.ReadFile(filepath.Join(home, ".codex-global-state.json"))
	var d struct {
		Assign map[string]map[string]string `json:"thread-project-assignments"`
	}
	json.Unmarshal(b, &d)
	if d.Assign["t1"]["projectId"] != "p" {
		t.Fatalf("not filed: %s", b)
	}
}

func TestCodexAssignOffDoesNothing(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Home = t.TempDir()
	off := false
	cfg.Agents.Codex.AssignProjects = &off
	a := &App{Cfg: cfg, Out: &bytes.Buffer{}, Err: &bytes.Buffer{}}
	queueCodexAssign(a, t.TempDir(), []string{"t1"})
	if len(loadPendingAssign(a)) != 0 {
		t.Fatal("disabled option must not queue anything")
	}
}
