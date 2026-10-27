package engine

import (
	"os"
	"path/filepath"
	"testing"
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
