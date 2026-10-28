package project

import (
	"context"
	"os"
	"path/filepath"
	"session-memory/internal/testutil/gitrepo"
	"testing"
	"time"
)

func TestInspectAndNormalizeRemote(t *testing.T) {
	for _, s := range []string{"ssh://git@EXAMPLE.invalid/team/project.git/", "https://user:synthetic@EXAMPLE.invalid/team/project.git", "git@EXAMPLE.invalid:team/project.git"} {
		if got := NormalizeRemote(s); got != "example.invalid/team/project" {
			t.Fatalf("normalized remote=%q", got)
		}
	}
	dir := gitrepo.New(t)
	os.WriteFile(filepath.Join(dir, "tracked.txt"), []byte("modified\n"), 0600)
	os.WriteFile(filepath.Join(dir, "untracked.txt"), []byte("untracked\n"), 0600)
	s, err := Inspect(context.Background(), dir)
	if err != nil || s.Status != "git" || s.Head == "" || s.RemoteHash == "" || s.Dirty.Modified != 1 || s.Dirty.Untracked != 1 {
		t.Fatalf("state=%+v err=%v", s, err)
	}
	missing, err := Inspect(context.Background(), filepath.Join(t.TempDir(), "absent"))
	if err != nil || missing.Status != "unavailable" {
		t.Fatalf("missing=%+v err=%v", missing, err)
	}
}

// A git that never returns must not hang the caller (it would hold the store lock).
func TestInspectGivesUpOnAGitThatHangs(t *testing.T) {
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte("#!/bin/sh\nsleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	old := gitTimeout
	gitTimeout = 300 * time.Millisecond
	defer func() { gitTimeout = old }()
	start := time.Now()
	st, err := Inspect(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took > 10*time.Second {
		t.Fatalf("Inspect waited %v for a hung git", took)
	}
	if st.Status != "unavailable" {
		t.Fatalf("status %q, want unavailable", st.Status)
	}
}
