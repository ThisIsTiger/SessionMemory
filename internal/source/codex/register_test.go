package codex

import (
	"context"
	"os"
	"path/filepath"
	"session-memory/internal/testutil/codexgen"
	"testing"
)

func TestRegisterTalksToAppServer(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "home.txt")
	bin := filepath.Join(dir, "codex")
	script := "#!/bin/sh\n" +
		"[ \"$1\" = app-server ] || exit 3\n" +
		"echo \"$CODEX_HOME\" > '" + out + "'\n" +
		"read l1; echo '{\"id\":1,\"result\":{}}'\n" +
		"read l2\n" +
		"read l3; echo '{\"method\":\"noise\"}'; echo '{\"id\":2,\"result\":{\"data\":[{\"id\":\"AAA\"},{\"id\":\"other\"}],\"nextCursor\":null}}'\n" +
		"cat >/dev/null\n"
	if err := os.WriteFile(bin, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(dir, "codex-home")
	found, err := Register(context.Background(), home, bin, []string{"aaa", "missing"})
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 1 || found[0] != "AAA" {
		t.Fatalf("found = %v", found)
	}
	if b, _ := os.ReadFile(out); string(b) != home+"\n" {
		t.Fatalf("CODEX_HOME = %q", b)
	}
	if _, err := Register(context.Background(), home, filepath.Join(dir, "absent"), []string{"x"}); err == nil {
		t.Fatal("a missing binary must be an error")
	}
}

// SM_LIVE_CODEX=<codex binary> runs Register against a real Codex app-server
// with a throw-away home holding one generated rollout.
func TestRegisterLive(t *testing.T) {
	bin := os.Getenv("SM_LIVE_CODEX")
	if bin == "" {
		t.Skip("set SM_LIVE_CODEX to a Codex binary")
	}
	home := t.TempDir()
	tid := codexgen.ThreadIDFor(77)
	p := filepath.Join(home, "sessions", "2026", "09", "01", "rollout-2026-09-01T01-46-53-"+tid+".jsonl")
	os.MkdirAll(filepath.Dir(p), 0700)
	os.WriteFile(p, codexgen.Rollout(codexgen.Options{Seed: 77, Turns: 2, Cwd: home}), 0600)
	found, err := Register(context.Background(), home, bin, []string{tid})
	if err != nil || len(found) != 1 {
		t.Fatalf("found=%v err=%v", found, err)
	}
}
