package transport

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// History received from another computer must converge to the backup too,
// including when every computer has two accounts but the publisher's backup
// was temporarily unavailable and that computer is now offline.
func TestMultiMirrorReplicatesReceivedHistory(t *testing.T) {
	for _, mode := range []string{"single_account_publisher", "dual_account_publisher_backup_unavailable"} {
		t.Run(mode, func(t *testing.T) {
			mm, f1, f2, _ := twoAccounts(t)
			ctx := context.Background()
			if err := mm.CatchUp(ctx); err != nil {
				t.Fatal(err)
			}
			for _, label := range []string{"primary", "backup"} {
				behind, known := mm.behind()[label]
				if !known || behind {
					t.Fatalf("setup: account %s not enrolled: %v", label, mm.behind())
				}
			}
			producerLocal := NewFolder(t.TempDir())
			producerLocal.Settle = 0
			producer := NewMirror(producerLocal, f1.client(), "SessionMemory")
			const path = "objects/ab/foreign-history.obj"
			const body = "HISTORY_FROM_ANOTHER_COMPUTER"
			if mode == "single_account_publisher" {
				if err := producer.Put(ctx, path, strings.NewReader(body)); err != nil {
					t.Fatal(err)
				}
			} else {
				dual := NewMultiMirror(producerLocal, Account{"primary", producer}, Account{"backup", NewMirror(producerLocal, f2.client(), "SessionMemory")})
				dual.StateFile = filepath.Join(t.TempDir(), "producer-behind.json")
				if err := dual.CatchUp(ctx); err != nil {
					t.Fatal(err)
				}
				f2.mu.Lock()
				f2.quota = true
				f2.mu.Unlock()
				if err := dual.Put(ctx, path, strings.NewReader(body)); err != nil {
					t.Fatal(err)
				}
				if !dual.behind()["backup"] {
					t.Fatal("setup: publishing device did not record missed backup write")
				}
				f2.mu.Lock()
				f2.quota = false
				f2.mu.Unlock()
				// The publishing computer is now offline. The receiving computer can
				// reach both accounts, so it should repair the missing replica itself.
			}
			if err := mm.Pull(ctx); err != nil {
				t.Fatal(err)
			}
			rc, err := mm.Local.Get(ctx, path)
			if err != nil {
				t.Fatal(err)
			}
			data, readErr := io.ReadAll(rc)
			rc.Close()
			if readErr != nil || string(data) != body {
				t.Fatalf("setup: local history missing %q %v", data, readErr)
			}
			for i := 0; i < 3; i++ {
				if err := mm.CatchUp(ctx); err != nil {
					t.Fatal(err)
				}
				if err := mm.Pull(ctx); err != nil {
					t.Fatal(err)
				}
			}
			if countFiles(f2) != 1 {
				t.Fatalf("known backup remains empty after receiving history and three catch-up rounds; behind=%v", mm.behind())
			}
		})
	}
}

func TestMultiMirrorUpgradeRepairsPreviouslyCachedHistory(t *testing.T) {
	mm, f1, f2, _ := twoAccounts(t)
	ctx := context.Background()
	local := NewFolder(t.TempDir())
	local.Settle = 0
	const path = "objects/ab/old-received.obj"
	if err := NewMirror(local, f1.client(), "SessionMemory").Put(ctx, path, strings.NewReader("cached-before-upgrade")); err != nil {
		t.Fatal(err)
	}
	if err := mm.Local.Put(ctx, path, strings.NewReader("cached-before-upgrade")); err != nil {
		t.Fatal(err)
	}
	// This is the old on-disk state: both accounts allegedly level even though
	// the previously received object never reached the backup. No new download
	// will trigger markReceived because the object is already cached.
	state, err := json.Marshal(map[string]bool{"primary": false, "backup": false})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(mm.StateFile, state, 0600); err != nil {
		t.Fatal(err)
	}
	if err := mm.CatchUp(ctx); err != nil {
		t.Fatal(err)
	}
	if countFiles(f2) != 1 || mm.behind()["backup"] {
		t.Fatal("upgrade trusted stale level flags and failed to repair the cached history")
	}
	if _, err := os.Stat(mm.StateFile + ".received-v1"); err != nil {
		t.Fatal(err)
	}
}

func TestMultiMirrorUpgradeRequiresCompleteUnionAfterFailedWrites(t *testing.T) {
	mm, f1, f2, skipped := twoAccounts(t)
	ctx := context.Background()
	local := NewFolder(t.TempDir())
	local.Settle = 0
	if err := NewMirror(local, f1.client(), "SessionMemory").Put(ctx, "objects/ab/old.obj", strings.NewReader("old")); err != nil {
		t.Fatal(err)
	}
	(*skipped)["primary"] = true
	if err := mm.CatchUp(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(mm.StateFile + ".received-v1"); !os.IsNotExist(err) {
		t.Fatalf("incomplete union was marked upgraded: %v", err)
	}
	if err := mm.Put(ctx, "objects/ab/first.obj", strings.NewReader("first")); err != nil {
		t.Fatal(err)
	}
	delete(*skipped, "primary")
	(*skipped)["backup"] = true
	if err := mm.Put(ctx, "objects/ab/second.obj", strings.NewReader("second")); err != nil {
		t.Fatal(err)
	}
	delete(*skipped, "backup")
	if err := mm.CatchUp(ctx); err != nil {
		t.Fatal(err)
	}
	if countFiles(f2) != 3 {
		t.Fatalf("missed-write flags hid the incomplete baseline: %d files", countFiles(f2))
	}
}

func TestMultiMirrorGetReceiveSurvivesRestart(t *testing.T) {
	mm, f1, f2, _ := twoAccounts(t)
	ctx := context.Background()
	if err := mm.CatchUp(ctx); err != nil {
		t.Fatal(err)
	}
	local := NewFolder(t.TempDir())
	local.Settle = 0
	const path = "objects/ab/received.obj"
	if err := NewMirror(local, f1.client(), "SessionMemory").Put(ctx, path, strings.NewReader("remote")); err != nil {
		t.Fatal(err)
	}
	rc, err := mm.Get(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	rc.Close()
	if state := mm.behind(); !state["primary"] || !state["backup"] {
		t.Fatalf("receive did not persist repair work: %v", state)
	}

	restarted := NewMultiMirror(mm.Local, Account{"primary", NewMirror(mm.Local, f1.client(), "SessionMemory")}, Account{"backup", NewMirror(mm.Local, f2.client(), "SessionMemory")})
	restarted.StateFile = mm.StateFile
	if err := restarted.CatchUp(ctx); err != nil {
		t.Fatal(err)
	}
	if countFiles(f2) != 1 {
		t.Fatal("receive repair was lost across restart")
	}
	f1.mu.Lock()
	before1 := f1.calls
	f1.mu.Unlock()
	f2.mu.Lock()
	before2 := f2.calls
	f2.mu.Unlock()
	if err := restarted.CatchUp(ctx); err != nil {
		t.Fatal(err)
	}
	f1.mu.Lock()
	after1 := f1.calls
	f1.mu.Unlock()
	f2.mu.Lock()
	after2 := f2.calls
	f2.mu.Unlock()
	if before1 != after1 || before2 != after2 {
		t.Fatal("idle catch-up made remote calls")
	}
}

func TestMultiMirrorListReceiveWaitsForSkippedBackup(t *testing.T) {
	mm, f1, f2, skipped := twoAccounts(t)
	ctx := context.Background()
	if err := mm.CatchUp(ctx); err != nil {
		t.Fatal(err)
	}
	local := NewFolder(t.TempDir())
	local.Settle = 0
	if err := NewMirror(local, f1.client(), "SessionMemory").Put(ctx, "manifests/dev_a/received.manifest", strings.NewReader("manifest")); err != nil {
		t.Fatal(err)
	}
	(*skipped)["backup"] = true
	if entries, err := mm.List(ctx, "manifests/"); err != nil || len(entries) != 1 {
		t.Fatalf("list: %v %v", entries, err)
	}
	if err := mm.CatchUp(ctx); err != nil {
		t.Fatal(err)
	}
	if !mm.behind()["backup"] || countFiles(f2) != 0 {
		t.Fatal("skipped backup was incorrectly marked level")
	}
	delete(*skipped, "backup")
	if err := mm.CatchUp(ctx); err != nil {
		t.Fatal(err)
	}
	if mm.behind()["backup"] || countFiles(f2) != 1 {
		t.Fatal("recovered backup was not repaired")
	}
}

func TestMultiMirrorReceiveRequiresDurableRepairMark(t *testing.T) {
	if os.Geteuid() == 0 || runtime.GOOS == "windows" {
		t.Skip("requires POSIX write permissions")
	}
	mm, f1, _, _ := twoAccounts(t)
	ctx := context.Background()
	if err := mm.CatchUp(ctx); err != nil {
		t.Fatal(err)
	}
	local := NewFolder(t.TempDir())
	local.Settle = 0
	const path = "objects/ab/received.obj"
	if err := NewMirror(local, f1.client(), "SessionMemory").Put(ctx, path, strings.NewReader("remote")); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(mm.StateFile)
	if err := os.Chmod(dir, 0500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(dir, 0700)
	if rc, err := mm.Get(ctx, path); err == nil {
		rc.Close()
		t.Fatal("received a file without persisting its repair mark")
	}
	if _, err := os.Stat(filepath.Join(mm.Local.Root, path)); !os.IsNotExist(err) {
		t.Fatalf("failed receive left an unmarked cached file: %v", err)
	}
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if rc, err := mm.Get(ctx, path); err != nil {
		t.Fatal(err)
	} else {
		rc.Close()
	}
	if !mm.behind()["backup"] {
		t.Fatal("retried receive lost its repair mark")
	}
}

// A backup that cannot be reached for good (token expired, quota paused) must
// not make every round invalidate the state again and pull the healthy account
// in full; once it works, it is brought level from the complete local copy.
func TestMultiMirrorUnreachableBackupDoesNotRepeatForcedPulls(t *testing.T) {
	for _, mode := range []string{"skipped", "failing"} {
		t.Run(mode, func(t *testing.T) {
			mm, f1, f2, skipped := twoAccounts(t)
			ctx := context.Background()
			if err := NewMirror(mm.Local, f1.client(), "SessionMemory").Put(ctx, "objects/ab/old.obj", strings.NewReader("old")); err != nil {
				t.Fatal(err)
			}
			// the old on-disk claim: level, although nothing proves it
			if err := os.WriteFile(mm.StateFile, []byte(`{"primary":false,"backup":false}`), 0600); err != nil {
				t.Fatal(err)
			}
			down := func(on bool) {
				if mode == "skipped" {
					(*skipped)["backup"] = on
					return
				}
				f2.mu.Lock()
				f2.quota = on
				f2.mu.Unlock()
			}
			calls := func(f *fakeWeiyun) int { f.mu.Lock(); defer f.mu.Unlock(); return f.calls }
			down(true)
			_ = mm.CatchUp(ctx) // the first round reads the primary in full
			after := calls(f1)
			if after == 0 {
				t.Fatal("the first round never read the primary")
			}
			for i := 0; i < 3; i++ {
				_ = mm.CatchUp(ctx)
			}
			if got := calls(f1); got != after {
				t.Fatalf("the healthy primary was asked %d more times while the backup stayed down", got-after)
			}
			if _, err := os.Stat(mm.StateFile + ".received-v1"); !os.IsNotExist(err) {
				t.Fatalf("the upgrade was marked done without reading the backup: %v", err)
			}
			down(false)
			if err := mm.CatchUp(ctx); err != nil {
				t.Fatal(err)
			}
			if countFiles(f2) != 1 || mm.behind()["backup"] || mm.behind()["primary"] {
				t.Fatalf("backup not brought level once reachable: files=%d state=%v", countFiles(f2), mm.behind())
			}
			if _, err := os.Stat(mm.StateFile + ".received-v1"); err != nil {
				t.Fatal(err)
			}
			settled := calls(f1)
			if err := mm.CatchUp(ctx); err != nil || calls(f1) != settled {
				t.Fatalf("a settled mirror asked the primary again: %v", err)
			}
		})
	}
}
