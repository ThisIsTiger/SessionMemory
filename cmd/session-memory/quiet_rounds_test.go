package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"session-memory/internal/testutil/codexgen"
)

// divergedPair is two computers that continued the same conversation
// separately: A's next relay cannot take B's branch.
func divergedPair(t *testing.T) (A, B *machine) {
	t.Helper()
	root := t.TempDir()
	shared := filepath.Join(root, "cloud")
	A = newComputer(t, root, "a", shared)
	A.must("init")
	proj := filepath.Join(A.home, "code", "app")
	os.MkdirAll(proj, 0700)
	tid := codexgen.ThreadIDFor(71)
	ap := filepath.Join(A.home, "codex", "sessions", "2026", "09", "01", "rollout-2026-09-01T01-46-53-"+tid+".jsonl")
	os.MkdirAll(filepath.Dir(ap), 0700)
	os.WriteFile(ap, codexgen.Rollout(codexgen.Options{Seed: 71, Turns: 2, Cwd: proj}), 0600)
	A.must("project", "track", "--cwd", proj)
	A.must("relay", "--client-closed")

	pf := filepath.Join(root, "pass")
	os.WriteFile(pf, []byte("correct horse battery\n"), 0600)
	bundle := filepath.Join(root, "j.bundle")
	A.must("device", "invite", "--out", bundle, "--passphrase-file", pf)
	B = newComputer(t, root, "b", shared)
	B.must("device", "join", bundle, "--passphrase-file", pf)
	projB := filepath.Join(B.home, "work", "app")
	os.MkdirAll(projB, 0700)
	B.must("relay", "--client-closed")
	files := codexFiles(B)
	if len(files) != 1 {
		t.Fatalf("B has %v", files)
	}
	appendLine := func(p, msg string) {
		f, _ := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0600)
		f.WriteString(`{"timestamp":"2026-09-01T02:00:00.000Z","type":"event_msg","payload":{"type":"agent_message","message":"` + msg + `"}}` + "\n")
		f.Close()
	}
	appendLine(ap, "FROM_A")
	appendLine(files[0], "FROM_B")
	B.must("relay", "--client-closed")
	return A, B
}

func journalDone(m *machine) int {
	files, _ := filepath.Glob(filepath.Join(m.home, "sm", "journal", "done", "*.json"))
	return len(files)
}

// Rounds that find nothing to do leave nothing behind: no archived
// transaction per session per round.
func TestRoundsWithNothingToDoLeaveNoJournal(t *testing.T) {
	root := t.TempDir()
	A := newComputer(t, root, "a", filepath.Join(root, "cloud"))
	A.must("init")
	proj := filepath.Join(A.home, "code", "app")
	os.MkdirAll(proj, 0700)
	addCodexSession(A, 61, proj)
	addCodexSession(A, 62, proj)
	A.must("project", "track", "--cwd", proj)
	A.must("relay", "--client-closed")
	A.must("relay", "--client-closed") // publishes the install receipts
	before := journalDone(A)
	if before == 0 {
		t.Fatal("the real work must be archived")
	}
	for i := 0; i < 3; i++ {
		A.must("relay", "--client-closed")
		A.must("sync")
	}
	if after := journalDone(A); after != before {
		t.Fatalf("idle rounds archived %d transactions", after-before)
	}
	if left, _ := filepath.Glob(filepath.Join(A.home, "sm", "journal", "*.json")); len(left) != 0 {
		t.Fatalf("open journals are left: %v", left)
	}
}

// The same refusal is spoken of when it is new, not every round: a background
// run says it once, then stays quiet and does not end in an error.
func TestBackgroundRoundDoesNotRepeatAKnownRefusal(t *testing.T) {
	A, _ := divergedPair(t)
	code, out, errs := A.run("relay", "--quiet", "--client-closed")
	if code != 6 || !strings.Contains(out+errs, "need attention") {
		t.Fatalf("the first background round must say it: exit %d\n%s%s", code, out, errs)
	}
	A.run("relay", "--quiet", "--client-closed") // may still upload its own receipt, and say so
	code, out, errs = A.run("relay", "--quiet", "--client-closed")
	if code != 0 || strings.TrimSpace(out+errs) != "" {
		t.Fatalf("the same refusal again must be silent: exit %d\n%s%s", code, out, errs)
	}
	// run by hand it is still told, with its exit code
	if code, _, _ := A.run("relay", "--client-closed"); code != 6 {
		t.Fatalf("a round run by hand must still report it: exit %d", code)
	}
	if rep := runCheck(t, A); !rep.mentions("装不进去") && !rep.mentions("没能装进本机") {
		t.Fatalf("check must still know: %+v", rep)
	}
}

func TestLogIsKeptSmall(t *testing.T) {
	a := newNotifyApp(t)
	dir := a.Cfg.StateDir()
	os.MkdirAll(dir, 0700)
	p := filepath.Join(dir, "autosync.log")
	os.WriteFile(p, []byte("small"), 0600)
	a.rotateLog()
	if _, err := os.Stat(p + ".1"); err == nil {
		t.Fatal("a small log was rotated")
	}
	os.WriteFile(p, make([]byte, logLimit+1), 0600)
	a.rotateLog()
	if st, err := os.Stat(p + ".1"); err != nil || st.Size() != logLimit+1 {
		t.Fatalf("the large log was not moved aside: %v", err)
	}
	if _, err := os.Stat(p); err == nil {
		t.Fatal("the old log is still the live one")
	}
}
