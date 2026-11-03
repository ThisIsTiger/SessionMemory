package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"session-memory/internal/session"
	syncpkg "session-memory/internal/sync"
)

// settings.json as WorkBuddy keeps it: the application's own, with a value it
// encrypts, and not a word of it ours.
const workbuddySettings = `{
  "enabledPlugins": {
    "zeta@market": true
  },
  "claw": {
    "users": {
      "u1": {
        "webhookUrl": {
          "$wbEncrypted": 1,
          "envelope": "eyJzdWl0ZSI6MSwia2V5SWQiOiI5MTI3ZGVh=="
        }
      }
    }
  },
  "sandbox": {
    "enabled": true
  }
}
`

func workbuddyHome(t *testing.T, m *machine, settings string) (home, file string) {
	t.Helper()
	home = filepath.Join(m.home, "workbuddy")
	os.MkdirAll(home, 0700)
	file = filepath.Join(home, "settings.json")
	if settings != "" {
		if err := os.WriteFile(file, []byte(settings), 0600); err != nil {
			t.Fatal(err)
		}
	}
	m.must("config", "set", "agents.workbuddy.home", home)
	return home, file
}

func TestWorkBuddyHooksInstallStatusUninstall(t *testing.T) {
	m := readyComputer(t)
	_, file := workbuddyHome(t, m, workbuddySettings)

	out := m.must("hooks", "install", "--host", "workbuddy")
	if !strings.Contains(out, "WorkBuddy hooks added to "+file) || strings.Contains(out, "/hooks") {
		t.Fatalf("install output:\n%s", out)
	}
	raw := string(mustRead(t, file))
	if !strings.Contains(raw, "hook --event Stop --agent workbuddy") || strings.Contains(raw, "SessionStart") {
		t.Fatalf("settings after install:\n%s", raw)
	}
	for _, kept := range []string{`"zeta@market": true`, `"$wbEncrypted": 1`, `"envelope": "eyJzdWl0ZSI6MSwia2V5SWQiOiI5MTI3ZGVh=="`, `"sandbox"`} {
		if !strings.Contains(raw, kept) {
			t.Errorf("%s is gone:\n%s", kept, raw)
		}
	}
	if bak := mustRead(t, file+".sm-bak"); string(bak) != workbuddySettings {
		t.Fatalf("backup = %q", bak)
	}
	// nothing was put in Codex by asking for WorkBuddy only
	if _, err := os.Stat(filepath.Join(m.home, "codex", "hooks.json")); err == nil {
		t.Fatal("Codex got hooks too")
	}

	out = m.must("hooks", "install", "--host", "workbuddy")
	if !strings.Contains(out, "already in place") {
		t.Fatalf("second install:\n%s", out)
	}

	out = m.must("hooks", "status", "--host", "workbuddy")
	if !strings.Contains(out, "installed for Stop") || strings.Contains(out, "trust") || strings.Contains(out, "NOT installed") {
		t.Fatalf("status:\n%s", out)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal([]byte(m.must("hooks", "status", "--host", "workbuddy", "--json")), &doc); err != nil || len(doc["workbuddy"]) == 0 {
		t.Fatalf("json status: %v %s", err, doc)
	}

	out = m.must("hooks", "uninstall", "--host", "workbuddy")
	if !strings.Contains(out, "removed 1 session-memory hook(s) from WorkBuddy") {
		t.Fatalf("uninstall:\n%s", out)
	}
	if back := mustRead(t, file); string(back) != workbuddySettings {
		t.Fatalf("settings after uninstall are not what they were:\n%s", back)
	}
}

// With no --host both are handled, and only the ones that are on the computer.
func TestHooksWithoutAHostFollowWhatIsInstalled(t *testing.T) {
	m := readyComputer(t)
	cxFile := filepath.Join(m.home, "codex", "hooks.json")

	m.must("hooks", "install") // Codex only: there is no WorkBuddy here
	if _, err := os.Stat(cxFile); err != nil {
		t.Fatalf("Codex hooks missing: %v", err)
	}
	_, wbFile := workbuddyHome(t, m, workbuddySettings)
	out := m.must("hooks", "install")
	if !strings.Contains(out, "WorkBuddy hooks added to "+wbFile) {
		t.Fatalf("install with both:\n%s", out)
	}
	out = m.must("hooks", "uninstall")
	if !strings.Contains(out, "from Codex") || !strings.Contains(out, "from WorkBuddy") {
		t.Fatalf("uninstall:\n%s", out)
	}
	if _, err := os.Stat(cxFile); err == nil {
		t.Fatal("the Codex hooks file was left behind")
	}
	if back := mustRead(t, wbFile); string(back) != workbuddySettings {
		t.Fatalf("WorkBuddy settings after uninstall:\n%s", back)
	}
}

func TestWorkBuddyHooksStatusProblems(t *testing.T) {
	m := readyComputer(t)
	_, file := workbuddyHome(t, m, `{"disableAllHooks": true}`)
	m.must("hooks", "install", "--host", "workbuddy")
	if out := m.must("hooks", "status", "--host", "workbuddy"); !strings.Contains(out, "disableAllHooks") {
		t.Fatalf("hooks switched off were not reported:\n%s", out)
	}
	// a settings file that is not valid is not overwritten, and is said so
	os.WriteFile(file, []byte("{ // not json\n}"), 0600)
	if code, _, errs := m.run("hooks", "install", "--host", "workbuddy"); code == 0 || !strings.Contains(errs, "not touched") {
		t.Fatalf("invalid settings: exit %d %s", code, errs)
	}
	if b := mustRead(t, file); string(b) != "{ // not json\n}" {
		t.Fatalf("the file was changed: %q", b)
	}
	if out := m.must("hooks", "status", "--host", "workbuddy"); !strings.Contains(out, "problem:") {
		t.Fatalf("status of invalid settings:\n%s", out)
	}
}

// A WorkBuddy hook is told apart from a Codex one, and reset takes it out.
func TestWorkBuddyHookEventIsRememberedPerAgentAndResetRemovesIt(t *testing.T) {
	m := readyComputer(t)
	_, file := workbuddyHome(t, m, workbuddySettings)
	m.must("hooks", "install", "--host", "workbuddy")

	events, wait := hookWorkers(t, 1)
	if code, out, errs := m.run("hook", "--event", "Stop", "--agent", "workbuddy"); code != 0 || out != "" || errs != "" {
		t.Fatalf("hook: exit %d out=%q err=%q", code, out, errs)
	}
	if events.Load() != 1 {
		t.Fatalf("workers started = %d", events.Load())
	}
	// reset refuses while a worker is alive, so let the one just started finish
	wait()
	var st hookState
	if err := json.Unmarshal(mustRead(t, filepath.Join(m.home, "sm", "hook-state.json")), &st); err != nil {
		t.Fatal(err)
	}
	if st.Agents["workbuddy"].Count != 1 || st.Agents["codex"].Count != 0 || st.Count != 1 {
		t.Fatalf("state = %+v", st)
	}
	if out := m.must("hooks", "status", "--host", "workbuddy"); !strings.Contains(out, "workbuddy: Stop at ") {
		t.Fatalf("status:\n%s", out)
	}

	out := m.must("reset")
	if !strings.Contains(out, file) {
		t.Fatalf("the reset preview does not mention the WorkBuddy settings:\n%s", out)
	}
	m.must("reset", "--yes")
	if back := mustRead(t, file); string(back) != workbuddySettings {
		t.Fatalf("WorkBuddy settings after reset:\n%s", back)
	}
}

// A hook without --agent, as Codex hooks installed before agents were told
// apart run, counts for Codex, and so does a state file from then.
func TestHookWithoutAnAgentIsCodexs(t *testing.T) {
	m := readyComputer(t)
	events, _ := hookWorkers(t, 1)
	if code, _, _ := m.run("hook", "--event", "Stop"); code != 0 {
		t.Fatal("hook failed")
	}
	if events.Load() != 1 {
		t.Fatal("no worker")
	}
	var st hookState
	json.Unmarshal(mustRead(t, filepath.Join(m.home, "sm", "hook-state.json")), &st)
	if st.Agents["codex"].Count != 1 {
		t.Fatalf("state = %+v", st)
	}
	old := hookState{Event: "Stop", At: time.Now(), Count: 3}
	if got := old.lastByAgent(); got["codex"].IsZero() || len(got) != 1 {
		t.Fatalf("legacy state = %v", got)
	}
}

func TestHooksCoverIsPerAgent(t *testing.T) {
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	min := func(n int) time.Duration { return time.Duration(n) * time.Minute }
	at := func(ago int) time.Time { return now.Add(-min(ago)) }
	cx, wb := rev("codex", at(1)), rev("workbuddy", at(2))
	for _, tc := range []struct {
		name string
		revs []session.Revision
		last map[string]time.Time
		want bool
	}{
		{"WorkBuddy progress and a WorkBuddy hook", []session.Revision{wb}, map[string]time.Time{"workbuddy": at(1)}, true},
		{"both, each with its own hook", []session.Revision{cx, wb}, map[string]time.Time{"codex": at(1), "workbuddy": at(1)}, true},
		{"WorkBuddy's hook cannot vouch for Codex", []session.Revision{cx, wb}, map[string]time.Time{"workbuddy": at(1)}, false},
		{"Codex's hook cannot vouch for WorkBuddy", []session.Revision{wb}, map[string]time.Time{"codex": at(1)}, false},
		{"one agent's hook is old", []session.Revision{cx, wb}, map[string]time.Time{"codex": at(1), "workbuddy": at(60)}, false},
		{"one agent's progress has waited too long", []session.Revision{rev("codex", at(1)), rev("workbuddy", at(11))}, map[string]time.Time{"codex": at(1), "workbuddy": at(1)}, false},
		{"an agent nobody hooks", []session.Revision{rev("claude", at(1))}, map[string]time.Time{"codex": at(1), "workbuddy": at(1)}, false},
	} {
		if got := hooksCover(syncpkg.Pending{Revisions: tc.revs}, tc.last, now); got != tc.want {
			t.Errorf("%s: hooksCover = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// WorkBuddy's settings file is never deleted, not even when ours were all it held.
func TestWorkBuddySettingsSurviveUninstallWhenOnlyOurHookWasInThem(t *testing.T) {
	m := readyComputer(t)
	_, file := workbuddyHome(t, m, "{}\n")
	m.must("hooks", "install", "--host", "workbuddy")
	m.must("hooks", "uninstall", "--host", "workbuddy")
	if b, err := os.ReadFile(file); err != nil || strings.TrimSpace(string(b)) != "{}" {
		t.Fatalf("settings after uninstall: %q, %v", b, err)
	}
}
