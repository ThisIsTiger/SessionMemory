package engine

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"session-memory/internal/agent"
	"session-memory/internal/session"
)

func TestUniqueLocalDirFindsTheFolderWithTheSameName(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	mk := func(p ...string) string {
		d := filepath.Join(append([]string{home}, p...)...)
		os.MkdirAll(d, 0755)
		return d
	}
	want := mk("Documents", "tiger", "SessionMemory")
	mk("Documents", "other", "Notes")
	mk("Library", "SessionMemory") // never searched
	mk("Documents", "node_modules", "SessionMemory")
	if got, ok := UniqueLocalDir("/Users/yuhuihu/Documents/SessionMemory", nil); !ok || got != want {
		t.Fatalf("got %q %v want %q", got, ok, want)
	}
	// a second folder with that name makes the choice ambiguous
	mk("Projects", "SessionMemory")
	if got, ok := UniqueLocalDir("/x/SessionMemory", nil); ok {
		t.Fatalf("ambiguous match was guessed: %q", got)
	}
	// the agent's own project list decides, even over the scan
	if got, ok := UniqueLocalDir("/x/SessionMemory", []string{want}); !ok || got != want {
		t.Fatalf("hint ignored: %q %v", got, ok)
	}
	// too deep is not looked at
	deep := mk("Desktop", "a", "b", "c", "d", "Deep")
	if _, ok := UniqueLocalDir("/x/Deep", nil); ok {
		t.Fatalf("found %s beyond the search depth", deep)
	}
	if _, ok := UniqueLocalDir("/", nil); ok {
		t.Fatal("root has no name to match")
	}
}

type taskAgent struct {
	agent.Adapter
	origin string
}

func (t taskAgent) Workspace(context.Context, agent.SnapshotReader) (string, error) {
	return t.origin, nil
}
func (t taskAgent) TaskWorkspace(o string) (string, bool) {
	if filepath.Base(o) == "2026-10-09-09-25-53" {
		return filepath.Join(os.Getenv("HOME"), "WorkBuddy", "2026-10-09-09-25-53"), true
	}
	return "", false
}

// A task folder the agent makes itself needs no --cwd: it is made here when the
// session is installed, not when the plan is only previewed.
func TestRestoreWorkspaceUsesTheAgentsTaskFolder(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	want := filepath.Join(home, "WorkBuddy", "2026-10-09-09-25-53")
	ad := taskAgent{origin: "/Users/someone/WorkBuddy/2026-10-09-09-25-53"}
	cwd, pm, gaps, check, notes, err := (&Engine{}).restoreWorkspaceNotes(context.Background(), ad, nil, session.Revision{}, nil, RestoreOptions{AllowCodeMismatch: true})
	if err != nil || cwd != want {
		t.Fatalf("cwd=%q err=%v", cwd, err)
	}
	for _, g := range gaps {
		if g == "project_directory_missing" {
			t.Fatalf("a folder that will be made is not missing: %v", gaps)
		}
	}
	if len(notes) != 1 || len(pm.Rules) != 1 || pm.Rules[0].To != want {
		t.Fatalf("notes=%v rules=%v", notes, pm.Rules)
	}
	if _, err := os.Stat(want); err == nil {
		t.Fatal("the plan must not create the folder")
	}
	if err := check(context.Background()); err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(want); err != nil || !st.IsDir() {
		t.Fatalf("not made on install: %v", err)
	}
	// anything else is still asked for
	ad.origin = "/Users/someone/Documents/other"
	if cwd, _, _, _, _, _ := (&Engine{}).restoreWorkspaceNotes(context.Background(), ad, nil, session.Revision{}, nil, RestoreOptions{}); cwd != "/Users/someone/Documents/other" {
		t.Fatalf("cwd=%q", cwd)
	}
}
