package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func doneFiles(t *testing.T, root string) []string {
	t.Helper()
	files, _ := filepath.Glob(filepath.Join(root, "journal", "done", "*.json"))
	return files
}

// A transaction that changed nothing leaves no journal and no staging behind,
// where one that did is archived.
func TestDiscardLeavesNothingAndFinishArchives(t *testing.T) {
	root := t.TempDir()
	s, err := Open(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()

	empty, err := s.Begin(ctx, KindCapture, "ss_x", "b_x")
	if err != nil {
		t.Fatal(err)
	}
	staging := empty.StagingDir()
	if err := empty.Discard(); err != nil {
		t.Fatal(err)
	}
	if left, _ := filepath.Glob(filepath.Join(root, "journal", "*.json")); len(left) != 0 {
		t.Fatalf("an open journal is left: %v", left)
	}
	if _, err := os.Stat(staging); err == nil {
		t.Fatal("staging is left")
	}
	if n := len(doneFiles(t, root)); n != 0 {
		t.Fatalf("a discarded transaction was archived: %d", n)
	}

	real, err := s.Begin(ctx, KindCapture, "ss_x", "b_x")
	if err != nil {
		t.Fatal(err)
	}
	if err := real.Finish(); err != nil {
		t.Fatal(err)
	}
	if n := len(doneFiles(t, root)); n != 1 {
		t.Fatalf("a finished transaction must be archived: %d", n)
	}
}

func TestPruneDoneKeepsAWeekAndLooksOnceADay(t *testing.T) {
	root := t.TempDir()
	s, err := Open(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Now()
	s.Now = func() time.Time { return now }
	put := func(name string, age time.Duration) string {
		p := filepath.Join(root, "journal", "done", name)
		if err := os.WriteFile(p, []byte("{}"), 0600); err != nil {
			t.Fatal(err)
		}
		os.Chtimes(p, now.Add(-age), now.Add(-age))
		return p
	}
	old1, old2 := put("tx_old1.json", 8*24*time.Hour), put("tx_old2.json", 30*24*time.Hour)
	recent := put("tx_recent.json", 2*24*time.Hour)

	n, err := s.PruneDone(7 * 24 * time.Hour)
	if err != nil || n != 2 {
		t.Fatalf("pruned %d, %v", n, err)
	}
	for _, p := range []string{old1, old2} {
		if _, err := os.Stat(p); err == nil {
			t.Errorf("%s is still there", p)
		}
	}
	if _, err := os.Stat(recent); err != nil {
		t.Fatal("a recent journal was removed")
	}

	// another old one appears; within the day nothing is looked at, after a day it is
	old3 := put("tx_old3.json", 9*24*time.Hour)
	if n, _ := s.PruneDone(7 * 24 * time.Hour); n != 0 {
		t.Fatalf("looked again within the day: %d", n)
	}
	now = now.Add(25 * time.Hour)
	if n, _ := s.PruneDone(7 * 24 * time.Hour); n != 1 {
		t.Fatalf("the next day's look removed %d", n)
	}
	if _, err := os.Stat(old3); err == nil {
		t.Fatal("old3 is still there")
	}
}
