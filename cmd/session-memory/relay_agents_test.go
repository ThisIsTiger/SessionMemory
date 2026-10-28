package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"session-memory/internal/agent"
	"session-memory/internal/config"
	"session-memory/internal/engine"
	"session-memory/internal/vault"
)

func TestRelayAgentsOnlyIncludesWorkBuddyAfterOptIn(t *testing.T) {
	cfg := config.DefaultConfig()
	a := &App{Cfg: cfg}
	if got := relayAgents(a, ""); !reflect.DeepEqual(got, []string{"codex"}) {
		t.Fatalf("default: %v", got)
	}
	a.Cfg.Agents.WorkBuddy.AllowRegistryWrite = true
	if got := relayAgents(a, ""); !reflect.DeepEqual(got, []string{"codex", "workbuddy"}) {
		t.Fatalf("opted in: %v", got)
	}
	off := false
	a.Cfg.Agents.WorkBuddy.Enabled = &off
	if got := relayAgents(a, ""); !reflect.DeepEqual(got, []string{"codex"}) {
		t.Fatalf("WorkBuddy switched off: %v", got)
	}
	a.Cfg.Agents.WorkBuddy.Enabled = nil
	if got := relayAgents(a, "workbuddy"); !reflect.DeepEqual(got, []string{"workbuddy"}) {
		t.Fatalf("explicit: %v", got)
	}
}

type fakeRuntime struct {
	state agent.RunState
	err   error
}

func (f fakeRuntime) Running(context.Context, string, string) (agent.RunState, agent.RunDetail, error) {
	return f.state, agent.DetailUnknown, f.err
}

type fakeAgent struct {
	agent.Adapter
	state agent.RunState
	err   error
}

func (f fakeAgent) Runtime() agent.RuntimeDetector { return fakeRuntime{f.state, f.err} }

// A failed process check is reported as such, not as a running app.
func TestRelayClientStateReportsFailedCheck(t *testing.T) {
	opt, note := relayClientState(context.Background(), fakeAgent{state: agent.Unknown, err: errors.New("operation not permitted")}, "workbuddy", engine.RestoreOptions{Home: "/h"})
	if opt.ClientClosed || !strings.Contains(note, "cannot tell") || !strings.Contains(note, "operation not permitted") {
		t.Fatalf("ClientClosed=%v note=%q", opt.ClientClosed, note)
	}
}

// A background run has nobody to say "the app is closed": for WorkBuddy that
// statement is made from the process check, never when the app is running.
func TestRelayClientStateForWorkBuddy(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		name  string
		state agent.RunState
		given bool
		want  bool
	}{
		{"workbuddy", agent.NotRunning, false, true},
		{"workbuddy", agent.Running, false, false},
		{"workbuddy", agent.Unknown, false, false},
		{"workbuddy", agent.Running, true, true},  // the user's own statement is kept; the writer still checks the processes
		{"codex", agent.NotRunning, false, false}, // Codex decides by its thread locks
	} {
		got, _ := relayClientState(ctx, fakeAgent{state: c.state}, c.name, engine.RestoreOptions{Home: "/h", ClientClosed: c.given})
		if got.ClientClosed != c.want {
			t.Errorf("%s state=%v given=%v: ClientClosed=%v, want %v", c.name, c.state, c.given, got.ClientClosed, c.want)
		}
	}
}

// A WorkBuddy task folder (~/WorkBuddy/<timestamp>) needs no --cwd on the
// receiving computer: relay maps the project to the same folder under its own
// home and makes it.
func TestRelayMapsTaskProjectsByItself(t *testing.T) {
	root := t.TempDir()
	shared := filepath.Join(root, "cloud")
	A := &machine{t: t, home: filepath.Join(root, "a"), keys: vault.MemStore{}}
	B := &machine{t: t, home: filepath.Join(root, "b"), keys: vault.MemStore{}}
	os.MkdirAll(A.home, 0700)
	os.MkdirAll(B.home, 0700)
	A.must("init")
	A.must("transport", "attach-folder", shared)
	name := "2026-08-04-15-13-47"
	taskA := filepath.Join(A.home, "WorkBuddy", name)
	os.MkdirAll(taskA, 0700)
	wbA := filepath.Join(A.home, "wb")
	A.must("config", "set", "agents.workbuddy.home", wbA)
	id := "0192f3a4-0000-7000-8000-000000000042"
	dir := filepath.Join(wbA, "projects", strings.Trim(strings.ReplaceAll(taskA, "/", "-"), "-"))
	os.MkdirAll(dir, 0700)
	rec := func(m map[string]any) string { x, _ := json.Marshal(m); return string(x) + "\n" }
	os.WriteFile(filepath.Join(dir, id+".jsonl"), []byte(
		rec(map[string]any{"type": "message", "id": "u1", "sessionId": id, "timestamp": int64(1793000000000), "role": "user", "cwd": taskA, "content": "hello"})+
			rec(map[string]any{"type": "message", "id": "a1", "parentId": "u1", "sessionId": id, "timestamp": int64(1793000000001), "role": "assistant", "cwd": taskA, "content": "answer"})), 0600)
	var pt struct {
		ProjectRef string `json:"project_ref"`
	}
	json.Unmarshal([]byte(A.must("project", "track", "--agent", "workbuddy", "--cwd", taskA, "--json")), &pt)
	if pt.ProjectRef == "" {
		t.Fatal("no project")
	}

	pf := filepath.Join(root, "pass")
	os.WriteFile(pf, []byte("correct horse battery\n"), 0600)
	bundle := filepath.Join(root, "j.bundle")
	A.must("device", "invite", "--out", bundle, "--passphrase-file", pf)
	B.must("config", "set", "transport.folder", shared)
	B.must("device", "join", bundle, "--passphrase-file", pf)
	B.must("config", "set", "agents.workbuddy.allow_registry_write", "true")
	B.must("config", "set", "agents.workbuddy.home", filepath.Join(B.home, "wb"))
	B.must("config", "set", "agents.codex.home", filepath.Join(B.home, "codex"))

	code, o, e := B.run("relay", "--client-closed")
	t.Logf("relay %d\n%s\n%s", code, o, e) // installing needs a real WorkBuddy; the folder mapping does not
	want := filepath.Join(B.home, "WorkBuddy", name)
	if out := B.must("project", "list"); !strings.Contains(out, pt.ProjectRef) || !strings.Contains(out, want) {
		t.Fatalf("project not mapped to %s:\n%s", want, out)
	}
	if st, err := os.Stat(want); err != nil || !st.IsDir() {
		t.Fatalf("task folder not made: %v", err)
	}
}
