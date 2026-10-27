package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"session-memory/internal/vault"
)

func TestWeiyunTokenSurvivesWithoutEnvironment(t *testing.T) {
	a := &App{Keys: vault.MemStore{}}
	t.Setenv(envWeiyunToken, "")
	if a.weiyunToken() != "" {
		t.Fatal("no token expected yet")
	}
	if err := a.saveWeiyunToken(); err == nil {
		t.Fatal("saving without any token must fail")
	}
	t.Setenv(envWeiyunToken, "tok-123")
	if err := a.saveWeiyunToken(); err != nil {
		t.Fatal(err)
	}
	t.Setenv(envWeiyunToken, "")
	if got := a.weiyunToken(); got != "tok-123" {
		t.Fatalf("a background run must find the saved token, got %q", got)
	}
	t.Setenv(envWeiyunToken, "fresh")
	if a.weiyunToken() != "fresh" {
		t.Fatal("the environment wins over the saved token")
	}
}

func TestLockStoreWaitsForTheHolder(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "locks"), 0700)
	first, err := lockStore(root)
	if err != nil {
		t.Fatal(err)
	}
	old := lockWait
	lockWait = 5 * time.Second
	defer func() { lockWait = old }()
	go func() { time.Sleep(700 * time.Millisecond); first() }()
	start := time.Now()
	second, err := lockStore(root)
	if err != nil {
		t.Fatalf("second command should have waited for the first: %v", err)
	}
	second()
	if time.Since(start) < 500*time.Millisecond {
		t.Fatal("did not wait")
	}
	held, _ := lockStore(root)
	lockWait = 300 * time.Millisecond
	if _, err := lockStore(root); err == nil || !strings.Contains(err.Error(), "another session-memory is running") {
		t.Fatalf("a lock that is never released must still fail: %v", err)
	}
	held()
}
