package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"session-memory/internal/vault"
)

func tokens(a *App) map[string]string {
	out := map[string]string{}
	for _, ac := range a.weiyunAccounts() {
		out[ac.label] = ac.token
	}
	return out
}

func TestWeiyunTokensSurviveWithoutEnvironment(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // not the real Weiyun skill config
	a := &App{Keys: vault.MemStore{}}
	t.Setenv(envWeiyunToken, "")
	t.Setenv(envWeiyunToken2, "")
	if len(tokens(a)) != 0 {
		t.Fatal("no token expected yet")
	}
	if err := a.saveWeiyunToken(); err == nil {
		t.Fatal("saving without any token must fail")
	}
	t.Setenv(envWeiyunToken, "tok-123")
	t.Setenv(envWeiyunToken2, "tok-456")
	if err := a.saveWeiyunToken(); err != nil {
		t.Fatal(err)
	}
	t.Setenv(envWeiyunToken, "")
	t.Setenv(envWeiyunToken2, "")
	if got := tokens(a); got["primary"] != "tok-123" || got["backup"] != "tok-456" || len(got) != 2 {
		t.Fatalf("a background run must find both saved tokens, got %v", got)
	}
	t.Setenv(envWeiyunToken, "fresh")
	if got := tokens(a); got["primary"] != "fresh" || got["backup"] != "tok-456" {
		t.Fatalf("the environment wins over the saved token: %v", got)
	}
	// one account is enough
	// a pause belongs to the account, not to the slot: swapping the tokens makes it not apply
	c := &App{Keys: vault.MemStore{}}
	t.Setenv(envWeiyunToken, "acct-one")
	t.Setenv(envWeiyunToken2, "")
	idOne := c.weiyunAccounts()[0].id
	t.Setenv(envWeiyunToken, "acct-two")
	if idTwo := c.weiyunAccounts()[0].id; idTwo == idOne || !strings.HasPrefix(idTwo, "primary#") {
		t.Fatalf("ids %q %q", idOne, idTwo)
	}
	b := &App{Keys: vault.MemStore{}}
	t.Setenv(envWeiyunToken, "only")
	if got := tokens(b); len(got) != 1 || got["primary"] != "only" {
		t.Fatalf("single account: %v", got)
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
