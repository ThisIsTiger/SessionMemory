package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"session-memory/internal/agent"
	"session-memory/internal/testutil/codexgen"
)

func TestMirrorOfOrigin(t *testing.T) {
	home := isolate(t)
	for _, c := range []struct {
		origin string
		want   string // relative to this computer's home; "" = no mirror
	}{
		{"/Users/ann/Documents/work/app", "Documents/work/app"},
		{"/Users/ann/Documents/work/app/", "Documents/work/app"},
		{"/home/bob/code/app", "code/app"},
		{"/Users/ann", ""},                               // the home folder itself
		{"/Users/ann/Library/Application Support/x", ""}, // an app's own place
		{"/Users/ann/.config/app", ""},                   // hidden
		{"/Users/ann/work/../../etc", ""},                // does not stay under the home
		{"/opt/work/app", ""},                            // not under a home folder
		{"C:\\Users\\ann\\app", ""},
		{"", ""},
	} {
		got, ok := mirrorOfOrigin(c.origin)
		if c.want == "" {
			if ok {
				t.Errorf("%q: got %q, want none", c.origin, got)
			}
			continue
		}
		if !ok || got != filepath.Join(home, c.want) {
			t.Errorf("%q: got %q (%v), want %q", c.origin, got, ok, filepath.Join(home, c.want))
		}
	}
}

// two computers: A's session was recorded in origin; B has no such folder
func twoComputersWithSessionIn(t *testing.T, origin string) (A, B *machine, ref string) {
	t.Helper()
	root := t.TempDir()
	shared := filepath.Join(root, "cloud")
	A = newComputer(t, root, "a", shared)
	A.must("init")
	addCodexSession(A, 51, origin)
	tid := codexgen.ThreadIDFor(51)
	ref = "prj_abcdefghijklmnop"
	A.must("track", "--agent", "codex", "--native-session", tid, "--project", ref)
	A.must("relay", "--client-closed")

	pf := filepath.Join(root, "pass")
	os.WriteFile(pf, []byte("correct horse battery\n"), 0600)
	bundle := filepath.Join(root, "j.bundle")
	A.must("device", "invite", "--out", bundle, "--passphrase-file", pf)
	B = newComputer(t, root, "b", shared)
	B.must("device", "join", bundle, "--passphrase-file", pf)
	return A, B, ref
}

func codexFiles(m *machine) []string {
	files, _ := filepath.Glob(filepath.Join(m.home, "codex", "sessions", "*", "*", "*", "*.jsonl"))
	return files
}

// A conversation of a project that has no folder on this computer is not left
// waiting for somebody to notice: the folder is made at the same place under
// the home folder as on the other computer, and the conversation goes in.
func TestReceivedProjectGetsTheSameFolderUnderHome(t *testing.T) {
	_, B, ref := twoComputersWithSessionIn(t, "/Users/ann/Documents/work/app")
	// B's home is its own temporary folder; the mirror must be inside it
	want := filepath.Join(B.home, "Documents", "work", "app")
	if _, err := os.Stat(want); err == nil {
		t.Fatal("the folder is there before B ever synced")
	}
	out := B.must("relay", "--client-closed")
	if st, err := os.Stat(want); err != nil || !st.IsDir() {
		t.Fatalf("the project folder was not made at %s:\n%s", want, out)
	}
	if len(codexFiles(B)) != 1 {
		t.Fatalf("the conversation was not installed: %v\n%s", codexFiles(B), out)
	}
	if list := B.must("project", "list"); !strings.Contains(list, ref) || !strings.Contains(list, want) {
		t.Fatalf("project not mapped to the new folder:\n%s", list)
	}
	rep := runCheck(t, B)
	if rep.Level != "ok" || rep.mentions("项目 app") {
		t.Fatalf("nothing should be waiting now: %+v", rep)
	}
}

// What cannot be decided is named, with the reason and what to do.
func TestCheckNamesAProjectItCannotPlace(t *testing.T) {
	_, B, ref := twoComputersWithSessionIn(t, "/opt/work/app") // outside any home folder
	B.must("relay", "--client-closed")
	if len(codexFiles(B)) != 0 {
		t.Fatalf("installed without a folder: %v", codexFiles(B))
	}
	rep := runCheck(t, B)
	if rep.Level != "attention" || !rep.mentions("项目 app（1 个 Codex 对话）") || !rep.mentions("没法自动决定放哪里") || !rep.mentions("project map "+ref) {
		t.Fatalf("%+v", rep)
	}

	// the person (or the assistant) puts a folder in place and says so
	dir := filepath.Join(B.home, "wherever", "app")
	os.MkdirAll(dir, 0700)
	B.must("project", "map", ref, "--path", dir)
	B.must("relay", "--client-closed")
	if len(codexFiles(B)) != 1 {
		t.Fatalf("not installed after mapping: %v", codexFiles(B))
	}
	if rep := runCheck(t, B); rep.Level != "ok" {
		t.Fatalf("%+v", rep)
	}
}

type fixedRuntime struct{ st agent.RunState }

func (f fixedRuntime) Running(context.Context, string, string) (agent.RunState, agent.RunDetail, error) {
	return f.st, "", nil
}

type openApp struct {
	agent.Adapter
	st agent.RunState
}

func (o openApp) Runtime() agent.RuntimeDetector { return fixedRuntime{o.st} }

// An app that is open cannot be written into: check says which app, and that
// quitting it is all that is needed.
func TestCheckSaysWhichAppToQuit(t *testing.T) {
	_, B, ref := twoComputersWithSessionIn(t, "/opt/work/app")
	B.must("relay", "--client-closed") // receives it; no folder to put it in yet
	dir := filepath.Join(B.home, "wherever", "app")
	os.MkdirAll(dir, 0700)
	B.must("project", "map", ref, "--path", dir)

	old := wrapAdapter
	wrapAdapter = func(ad agent.Adapter) agent.Adapter { return openApp{Adapter: ad, st: agent.Running} }
	defer func() { wrapAdapter = old }()
	rep := runCheck(t, B)
	if rep.Level != "attention" || !rep.mentions("Codex 正在运行") || !rep.mentions("完全退出 Codex（Cmd+Q）") || len(rep.Next) != 1 {
		t.Fatalf("%+v", rep)
	}
}

// A folder that was removed after the mapping is said so, not left unexplained.
func TestCheckNamesAProjectWhoseFolderIsGone(t *testing.T) {
	_, B, ref := twoComputersWithSessionIn(t, "/opt/work/app")
	dir := filepath.Join(B.home, "wherever", "app")
	os.MkdirAll(dir, 0700)
	B.must("project", "map", ref, "--path", dir)
	os.RemoveAll(dir)
	B.must("relay", "--client-closed")
	rep := runCheck(t, B)
	if rep.Level != "attention" || !rep.mentions("项目文件夹 "+dir+" 不存在了") || !rep.mentions("project map "+ref) {
		t.Fatalf("%+v", rep)
	}
}
