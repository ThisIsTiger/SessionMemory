package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"session-memory/internal/testutil/codexgen"
	"session-memory/internal/vault"
)

// two simulated computers: separate HOME, SM_HOME and key stores
type machine struct {
	t    *testing.T
	home string
	keys vault.MemStore
}

func (m *machine) run(args ...string) (int, string, string) {
	m.t.Helper()
	m.t.Setenv("HOME", m.home)
	m.t.Setenv("USERPROFILE", m.home)
	m.t.Setenv("SM_HOME", filepath.Join(m.home, "sm"))
	m.t.Setenv("CODEX_HOME", "")
	old := newKeyStore
	newKeyStore = func() vault.KeyStore { return m.keys }
	defer func() { newKeyStore = old }()
	return exec(m.t, args...)
}

func (m *machine) must(args ...string) string {
	m.t.Helper()
	code, out, errs := m.run(args...)
	if code != 0 {
		m.t.Fatalf("%v -> exit %d\nout=%s\nerr=%s", args, code, out, errs)
	}
	return out
}

func TestTwoMachinesEndToEnd(t *testing.T) {
	root := t.TempDir()
	shared := filepath.Join(root, "cloud")
	A := &machine{t: t, home: filepath.Join(root, "a"), keys: vault.MemStore{}}
	B := &machine{t: t, home: filepath.Join(root, "b"), keys: vault.MemStore{}}
	os.MkdirAll(A.home, 0700)
	os.MkdirAll(B.home, 0700)

	// machine A
	A.must("init")
	A.must("transport", "attach-folder", shared)
	acodex := filepath.Join(A.home, "codex")
	A.must("config", "set", "agents.codex.home", acodex)
	tid := codexgen.ThreadIDFor(1)
	p := filepath.Join(acodex, "sessions", "2026", "09", "01", "rollout-2026-09-01T01-46-53-"+tid+".jsonl")
	os.MkdirAll(filepath.Dir(p), 0700)
	os.WriteFile(p, codexgen.Rollout(codexgen.Options{Seed: 1, Turns: 3}), 0600)

	out := A.must("discover", "--json")
	if !strings.Contains(out, tid) {
		t.Fatalf("discover missed the session: %s", out)
	}
	var tr struct {
		SessionID  string `json:"session_id"`
		RevisionID string `json:"revision_id"`
	}
	if err := json.Unmarshal([]byte(A.must("track", "--native-session", tid, "--json")), &tr); err != nil || tr.SessionID == "" {
		t.Fatalf("track: %v", err)
	}
	if code, _, _ := A.run("track", "--native-session", tid); code != 3 {
		t.Errorf("tracking twice should be a conflict (exit 3), got %d", code)
	}
	A.must("sync")

	// invite machine B
	pf := filepath.Join(root, "pass")
	os.WriteFile(pf, []byte("correct horse battery\n"), 0600)
	bundle := filepath.Join(root, "join.bundle")
	A.must("device", "invite", "--out", bundle, "--passphrase-file", pf)
	if code, _, _ := A.run("device", "invite", "--out", bundle); code != 2 {
		t.Errorf("invite without a passphrase file should be a usage error, got %d", code)
	}

	// machine B joins and syncs
	B.must("config", "set", "transport.folder", shared)
	badpf := filepath.Join(root, "badpass")
	os.WriteFile(badpf, []byte("wrong passphrase!!\n"), 0600)
	if code, _, _ := B.run("device", "join", bundle, "--passphrase-file", badpf); code != 9 {
		t.Errorf("wrong passphrase should exit 9, got %d", code)
	}
	B.must("device", "join", bundle, "--passphrase-file", pf)
	B.must("sync")
	out = B.must("revisions", tr.SessionID, "--json")
	if !strings.Contains(out, tr.RevisionID) {
		t.Fatalf("B did not receive the revision: %s", out)
	}

	// A appends; B receives the child revision
	orig, _ := os.ReadFile(p)
	os.WriteFile(p, append(orig, []byte(`{"timestamp":"2026-09-01T02:00:00.000Z","type":"event_msg","payload":{"type":"agent_message","message":"z"}}`+"\n")...), 0600)
	A.must("checkpoint", tr.SessionID)
	B.must("sync")
	out = B.must("revisions", tr.SessionID, "--json")
	var rv struct {
		Revisions []struct {
			RevisionID string `json:"revision_id"`
		}
		Heads []string
	}
	json.Unmarshal([]byte(out), &rv)
	if len(rv.Revisions) != 2 || len(rv.Heads) != 1 {
		t.Fatalf("B revisions=%d heads=%d", len(rv.Revisions), len(rv.Heads))
	}
	// A's status shows the receipt flow did not break anything
	if out := A.must("status", tr.SessionID); !strings.Contains(out, "conflict=false") {
		t.Errorf("status: %s", out)
	}
	// a command on an uninitialised machine says so
	C := &machine{t: t, home: filepath.Join(root, "c"), keys: vault.MemStore{}}
	os.MkdirAll(C.home, 0700)
	if code, _, _ := C.run("list"); code != 9 {
		t.Errorf("uninitialised list exit = %d, want 9", code)
	}
}

func TestRestoreVerifyAndProjectMapViaCLI(t *testing.T) {
	root := t.TempDir()
	shared := filepath.Join(root, "cloud")
	A := &machine{t: t, home: filepath.Join(root, "a"), keys: vault.MemStore{}}
	B := &machine{t: t, home: filepath.Join(root, "b"), keys: vault.MemStore{}}
	os.MkdirAll(A.home, 0700)
	os.MkdirAll(B.home, 0700)
	A.must("init")
	A.must("transport", "attach-folder", shared)
	acodex := filepath.Join(A.home, "codex")
	A.must("config", "set", "agents.codex.home", acodex)
	tid := codexgen.ThreadIDFor(3)
	p := filepath.Join(acodex, "sessions", "2026", "09", "01", "rollout-2026-09-01T01-46-53-"+tid+".jsonl")
	os.MkdirAll(filepath.Dir(p), 0700)
	os.WriteFile(p, codexgen.Rollout(codexgen.Options{Seed: 3, Turns: 2}), 0600)
	var tr struct {
		SessionID string `json:"session_id"`
	}
	json.Unmarshal([]byte(A.must("track", "--native-session", tid, "--json")), &tr)
	A.must("sync")
	if out := A.must("verify", "--deep"); !strings.Contains(out, "ok") {
		t.Errorf("verify: %q", out)
	}
	// corrupt A's file in the middle: deep verify must report it
	raw, _ := os.ReadFile(p)
	bad := append([]byte(nil), raw...)
	bad[len(bad)/2] ^= 1
	os.WriteFile(p, bad, 0600)
	if code, _, errs := A.run("verify", "--deep"); code != 6 || !strings.Contains(errs, "mismatch") {
		t.Errorf("deep verify of a rewritten file: exit %d %q", code, errs)
	}
	os.WriteFile(p, raw, 0600)

	pf := filepath.Join(root, "pass")
	os.WriteFile(pf, []byte("correct horse battery\n"), 0600)
	bundle := filepath.Join(root, "j.bundle")
	A.must("device", "invite", "--out", bundle, "--passphrase-file", pf)
	B.must("config", "set", "transport.folder", shared)
	B.must("device", "join", bundle, "--passphrase-file", pf)
	bcodex := filepath.Join(B.home, "codex")
	B.must("config", "set", "agents.codex.home", bcodex)
	B.must("sync")

	projDir := filepath.Join(B.home, "code", "app")
	os.MkdirAll(projDir, 0700)
	ref := strings.TrimSpace(B.must("project", "map", "--path", projDir, "--rule", "/work/app="+projDir))
	if code, _, _ := B.run("restore", tr.SessionID, "--project", ref, "--allow-code-mismatch"); code != 4 {
		t.Errorf("restore without --client-closed should exit 4, got %d", code)
	}
	plan := B.must("restore", tr.SessionID, "--plan", "--project", ref)
	if !strings.Contains(plan, "create") || !strings.Contains(plan, "nothing written") {
		t.Fatalf("plan output: %q", plan)
	}
	B.must("restore", tr.SessionID, "--client-closed", "--project", ref, "--allow-code-mismatch")
	files, _ := filepath.Glob(filepath.Join(bcodex, "sessions", "*", "*", "*", "*.jsonl"))
	if len(files) != 1 {
		t.Fatalf("restored files: %v", files)
	}
	got, _ := os.ReadFile(files[0])
	if !strings.Contains(string(got), `"cwd":"`+projDir+`"`) {
		t.Error("project rule was not applied")
	}
	if out := B.must("discover"); !strings.Contains(out, tid) {
		t.Errorf("restored thread not discoverable: %s", out)
	}
	if code, _, _ := B.run("restore", tr.SessionID, "--client-closed"); code == 0 {
		t.Error("restoring the same revision twice must not succeed silently")
	}
}

func TestCrossAgentPlanAndResumePack(t *testing.T) {
	root := t.TempDir()
	A := &machine{t: t, home: filepath.Join(root, "a"), keys: vault.MemStore{}}
	os.MkdirAll(A.home, 0700)
	A.must("init")
	wb := filepath.Join(A.home, "wb")
	A.must("config", "set", "agents.workbuddy.home", wb)
	dir := filepath.Join(wb, "projects", "Users-me-app")
	os.MkdirAll(dir, 0700)
	var b strings.Builder
	add := func(m map[string]any) { x, _ := json.Marshal(m); b.Write(x); b.WriteByte('\n') }
	add(map[string]any{"type": "message", "id": "m1", "role": "user", "cwd": "/w/app", "content": []any{map[string]any{"type": "input_text", "text": "list files"}}})
	add(map[string]any{"type": "reasoning", "id": "r1", "parentId": "m1"})
	add(map[string]any{"type": "function_call", "id": "f1", "parentId": "r1", "callId": "c1", "name": "shell", "arguments": `{"cmd":"ls"}`})
	add(map[string]any{"type": "function_call_result", "id": "f1r", "parentId": "f1", "callId": "c1", "output": "a b"})
	add(map[string]any{"type": "message", "id": "m2", "parentId": "f1r", "role": "assistant", "content": "here they are"})
	os.WriteFile(filepath.Join(dir, "wb-1.jsonl"), []byte(b.String()), 0600)

	var tr struct {
		SessionID string `json:"session_id"`
	}
	if err := json.Unmarshal([]byte(A.must("track", "--agent", "workbuddy", "--native-session", "wb-1", "--json")), &tr); err != nil || tr.SessionID == "" {
		t.Fatalf("track workbuddy: %v", err)
	}
	var rep struct {
		Counts map[string]struct{ Kept, Rendered, Dropped int }
	}
	out := A.must("convert", tr.SessionID, "--agent", "codex", "--plan", "--json")
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatal(err)
	}
	if rep.Counts["message"].Kept != 2 || rep.Counts["reasoning"].Dropped != 1 || rep.Counts["tool_call"].Rendered != 1 {
		t.Errorf("report: %s", out)
	}
	// writing the conversion: a missing local project directory refuses, a plan
	// writes nothing, the real run creates a new native Codex thread
	cx := filepath.Join(A.home, "codex")
	os.MkdirAll(cx, 0700)
	A.must("config", "set", "agents.codex.home", cx)
	proj := filepath.Join(root, "proj")
	os.MkdirAll(proj, 0700)
	if code, _, _ := A.run("convert", tr.SessionID, "--agent", "codex", "--client-closed"); code == 0 {
		t.Error("a conversion without an existing local project directory must be refused")
	}
	planOut := A.must("convert", tr.SessionID, "--agent", "codex", "--plan", "--cwd", proj, "--client-closed")
	if !strings.Contains(planOut, "would create sessions/") {
		t.Errorf("plan does not show the install: %s", planOut)
	}
	if m, _ := filepath.Glob(filepath.Join(cx, "sessions", "*", "*", "*", "*.jsonl")); len(m) != 0 {
		t.Fatalf("--plan wrote %v", m)
	}
	A.must("convert", tr.SessionID, "--agent", "codex", "--cwd", proj, "--client-closed", "--allow-code-mismatch")
	m, _ := filepath.Glob(filepath.Join(cx, "sessions", "*", "*", "*", "rollout-*.jsonl"))
	if len(m) != 1 {
		t.Fatalf("converted thread files: %v", m)
	}
	conv, _ := os.ReadFile(m[0])
	if !strings.Contains(string(conv), "list files") || !strings.Contains(string(conv), "here they are") || !strings.Contains(string(conv), proj) {
		t.Errorf("converted thread lacks history or project: %s", conv)
	}
	if code, _, _ := A.run("convert", tr.SessionID, "--agent", "codex", "--cwd", proj, "--client-closed", "--allow-code-mismatch"); code == 0 {
		t.Error("the same revision must not be converted twice")
	}
	pack := filepath.Join(root, "pack.md")
	A.must("resume-pack", tr.SessionID, "--out", pack)
	b2, _ := os.ReadFile(pack)
	if !strings.Contains(string(b2), "list files") || !strings.Contains(string(b2), "here they are") {
		t.Errorf("pack: %s", b2)
	}
	if code, _, _ := A.run("resume-pack", tr.SessionID, "--out", pack); code == 0 {
		t.Error("the pack must not overwrite an existing file")
	}
	if code, _, _ := A.run("restore", tr.SessionID, "--agent", "workbuddy"); code != 5 {
		t.Errorf("restoring into WorkBuddy must be unsupported, got %d", code)
	}
	if out := A.must("agent", "probe", "workbuddy", "--json"); !strings.Contains(out, `"compatibility": "ok"`) {
		t.Errorf("probe: %s", out)
	}
}

func TestAutoInstallWaitsWhenClientStateIsUnknown(t *testing.T) {
	root := t.TempDir()
	shared := filepath.Join(root, "cloud")
	A := &machine{t: t, home: filepath.Join(root, "a"), keys: vault.MemStore{}}
	B := &machine{t: t, home: filepath.Join(root, "b"), keys: vault.MemStore{}}
	os.MkdirAll(A.home, 0700)
	os.MkdirAll(B.home, 0700)
	A.must("init")
	A.must("transport", "attach-folder", shared)
	acodex, bcodex := filepath.Join(A.home, "codex"), filepath.Join(B.home, "codex")
	A.must("config", "set", "agents.codex.home", acodex)
	tid := codexgen.ThreadIDFor(7)
	p := filepath.Join(acodex, "sessions", "2026", "09", "01", "rollout-2026-09-01T01-46-53-"+tid+".jsonl")
	os.MkdirAll(filepath.Dir(p), 0700)
	os.WriteFile(p, codexgen.Rollout(codexgen.Options{Seed: 7, Turns: 2, Cwd: A.home}), 0600)
	var tr struct {
		SessionID string `json:"session_id"`
	}
	json.Unmarshal([]byte(A.must("track", "--native-session", tid, "--json")), &tr)
	A.must("sync")
	pf := filepath.Join(root, "pass")
	os.WriteFile(pf, []byte("correct horse battery\n"), 0600)
	bundle := filepath.Join(root, "j.bundle")
	A.must("device", "invite", "--out", bundle, "--passphrase-file", pf)
	B.must("config", "set", "transport.folder", shared)
	B.must("device", "join", bundle, "--passphrase-file", pf)
	B.must("config", "set", "agents.codex.home", bcodex)
	B.must("sync")
	B.must("restore", tr.SessionID, "--client-closed")
	B.must("auto-install", "enable", "--agent", "codex")

	orig, _ := os.ReadFile(p)
	os.WriteFile(p, append(orig, []byte(`{"timestamp":"2026-09-01T02:00:00.000Z","type":"event_msg","payload":{"type":"agent_message","message":"later"}}`+"\n")...), 0600)
	A.must("checkpoint", tr.SessionID)
	out := B.must("sync", "--auto-install", "--json")
	var res map[string]int
	json.Unmarshal([]byte(out), &res)
	if res["installed"] != 0 || res["waiting_client_close"] != 1 {
		t.Fatalf("auto-install must wait while run state is unknown: %s", out)
	}
	files, _ := filepath.Glob(filepath.Join(bcodex, "sessions", "*", "*", "*", "*.jsonl"))
	got, _ := os.ReadFile(files[0])
	if bytes.Contains(got, []byte("later")) {
		t.Error("the update was installed although the client state was unknown")
	}
	// the user confirms the client is closed: the manual path installs it
	B.must("restore", tr.SessionID, "--client-closed")
	got, _ = os.ReadFile(files[0])
	if !bytes.Contains(got, []byte("later")) {
		t.Error("manual restore did not bring B up to date")
	}
}
