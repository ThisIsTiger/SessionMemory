//go:build dev

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"session-memory/internal/testutil/codexgen"
)

func TestDevSandboxWorkflowAndRefusals(t *testing.T) {
	h := isolate(t)
	sm := filepath.Join(h, "sm")
	t.Setenv("SM_HOME", sm)
	if code, out, errs := exec(t, "dev", "sandbox", "new", "e1", "--agent", "codex"); code != 0 {
		t.Fatalf("sandbox new: %d %s %s", code, out, errs)
	}
	env, _ := os.ReadFile(filepath.Join(sm, "dev", "sandboxes", "e1", "env.sh"))
	if !strings.Contains(string(env), "CODEX_HOME") {
		t.Fatalf("env.sh: %s", env)
	}
	rollout := filepath.Join(h, "r.jsonl")
	tid := codexgen.ThreadIDFor(1)
	os.WriteFile(rollout, codexgen.Rollout(codexgen.Options{Seed: 1}), 0600)
	if code, _, e := exec(t, "dev", "codex", "write", "--sandbox", "e1", "--rollout", rollout); code != 0 {
		t.Fatalf("write: %d %s", code, e)
	}
	files, _ := filepath.Glob(filepath.Join(sm, "dev", "sandboxes", "e1", "codex", "sessions", "*", "*", "*", "*"+tid+".jsonl"))
	if len(files) != 1 {
		t.Fatalf("placed files: %v", files)
	}
	longer := filepath.Join(h, "long.jsonl")
	os.WriteFile(longer, codexgen.Rollout(codexgen.Options{Seed: 1, Turns: 4}), 0600)
	if code, _, e := exec(t, "dev", "codex", "append", "--sandbox", "e1", "--thread-id", tid, "--rollout", longer); code != 0 {
		t.Fatalf("append: %d %s", code, e)
	}
	// an unmarked name is refused and nothing is written
	if code, _, _ := exec(t, "dev", "codex", "write", "--sandbox", "nope", "--rollout", rollout); code == 0 {
		t.Error("a missing sandbox was accepted")
	}
	// the user's real codex home can never be a sandbox, even if someone marks it
	real := filepath.Join(h, ".codex")
	os.MkdirAll(real, 0700)
	if code, _, _ := exec(t, "dev", "sandbox", "mark", real); code != 2 {
		t.Errorf("mark without confirmation: %d", code)
	}
	// inventory is read-only and structure-only
	out := filepath.Join(h, "inv.json")
	if code, _, e := exec(t, "dev", "inventory", "--agent", "codex", "--home-dir", filepath.Join(sm, "dev", "sandboxes", "e1", "codex"), "--out", out); code != 0 {
		t.Fatalf("inventory: %d %s", code, e)
	}
	b, _ := os.ReadFile(out)
	if strings.Contains(string(b), tid) {
		t.Error("inventory leaked the thread id")
	}
	if code, _, _ := exec(t, "dev", "sandbox", "rm", "e1"); code != 0 {
		t.Error("sandbox rm failed")
	}
}
