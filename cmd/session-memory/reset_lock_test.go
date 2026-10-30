package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"session-memory/internal/platform"
)

func TestResetHonorsLegacyLocksWithoutChangingFiles(t *testing.T) {
	for _, name := range []string{"store.lock", "hook.lock"} {
		t.Run(name, func(t *testing.T) {
			m := readyComputer(t)
			state := filepath.Join(m.home, "sm")
			m.must("hooks", "install")
			hooksPath := filepath.Join(m.home, "codex", "hooks.json")
			before := mustRead(t, hooksPath)
			f, err := platform.Touch(filepath.Join(state, "locks", name))
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			if err := platform.LockExclusiveNonBlocking(f); err != nil {
				t.Fatal(err)
			}
			defer platform.Unlock(f)
			if code, _, _ := m.run("reset", "--yes"); code == 0 {
				t.Fatal("reset accepted a legacy process's active lock")
			}
			if _, err := os.Stat(filepath.Join(state, "device", "device.json")); err != nil {
				t.Fatal(err)
			}
			if string(mustRead(t, hooksPath)) != string(before) {
				t.Fatal("busy reset changed hooks")
			}
		})
	}
}

func TestStateGuardSurvivesDirectoryRename(t *testing.T) {
	m := readyComputer(t)
	state := filepath.Join(m.home, "sm")
	oldWait := lockWait
	lockWait = 0
	defer func() { lockWait = oldWait }()
	release, err := lockStateLifetime(state, true, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if err := os.Rename(state, state+".bak-test"); err != nil {
		t.Fatal(err)
	}
	if unlock, err := lockStore(state); err == nil {
		unlock()
		t.Fatal("a new Store bypassed reset's guard after rename")
	}
	if code, _, _ := m.run("init"); code == 0 {
		t.Fatal("init bypassed reset's guard")
	}
	a := &App{Now: time.Now}
	a.Cfg.Home = state
	a.hookEvent("Stop", "codex")
	a.hookWorker(context.Background())
	if _, err := os.Stat(state); !os.IsNotExist(err) {
		t.Fatalf("a blocked command recreated state after rename: %v", err)
	}
}

func TestHookCanRecordWhileStoreIsBusy(t *testing.T) {
	m := readyComputer(t)
	state := filepath.Join(m.home, "sm")
	unlock, err := lockStore(state)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	a := &App{Now: time.Now}
	a.Cfg.Home = state
	a.hookEvent("Stop", "codex")
	if _, dirty := a.hookDirtySince(); !dirty {
		t.Fatal("shared reset protection dropped a hook during normal sync")
	}
	if release, err := lockStateLifetime(state, true, 0); err == nil {
		release()
		t.Fatal("reset guard did not exclude the active Store")
	}
}

func TestResetRefusesWaitingHookWorker(t *testing.T) {
	m := readyComputer(t)
	a := &App{Now: time.Now}
	a.Cfg.Home = filepath.Join(m.home, "sm")
	a.hookEvent("Stop", "codex")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); a.hookWorker(ctx) }()
	defer func() { cancel(); <-done }()
	eventually(t, "waiting hook worker lock", a.hookWorkerRunning)
	if code, _, _ := m.run("reset", "--yes"); code == 0 {
		t.Fatal("reset moved the state of a waiting hook worker")
	}
}
