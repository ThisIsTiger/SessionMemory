package platform

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestAtomicWrite(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "sub", "testfile.txt")

	data1 := []byte("hello world")
	if err := AtomicWrite(target, data1, 0644); err != nil {
		t.Fatalf("AtomicWrite: %v", err)
	}

	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != string(data1) {
		t.Errorf("got %q, want %q", got, data1)
	}

	// overwrite atomically
	data2 := []byte("replaced content")
	if err := AtomicWrite(target, data2, 0600); err != nil {
		t.Fatalf("AtomicWrite overwrite: %v", err)
	}
	got2, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("ReadFile after overwrite: %v", err)
	}
	if string(got2) != string(data2) {
		t.Errorf("got %q, want %q", got2, data2)
	}
}

func TestEnsurePrivateDirAndTouch(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "private-dir")
	if err := EnsurePrivateDir(dir); err != nil {
		t.Fatalf("EnsurePrivateDir: %v", err)
	}

	st, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if !st.IsDir() {
		t.Fatalf("expected directory")
	}
	if runtime.GOOS != "windows" {
		if mode := st.Mode().Perm(); mode != 0700 {
			t.Errorf("expected 0700 dir permissions, got %04o", mode)
		}
	}

	lockPath := filepath.Join(dir, "sub", "test.lock")
	f, err := Touch(lockPath)
	if err != nil {
		t.Fatalf("Touch: %v", err)
	}
	f.Close()

	stFile, err := os.Stat(lockPath)
	if err != nil {
		t.Fatalf("Stat lock file: %v", err)
	}
	if runtime.GOOS != "windows" {
		if mode := stFile.Mode().Perm(); mode != 0600 {
			t.Errorf("expected 0600 file permissions, got %04o", mode)
		}
	}
}

func TestLockExclusiveAndUnlock(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, "test.lock")
	f, err := Touch(lockPath)
	if err != nil {
		t.Fatalf("Touch: %v", err)
	}
	defer f.Close()

	if err := LockExclusive(f); err != nil {
		t.Fatalf("LockExclusive: %v", err)
	}
	if err := Unlock(f); err != nil {
		t.Fatalf("Unlock: %v", err)
	}
}

func TestLockExclusiveNonBlocking(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, "test.lock")

	f1, err := Touch(lockPath)
	if err != nil {
		t.Fatalf("Touch f1: %v", err)
	}
	defer f1.Close()

	f2, err := os.OpenFile(lockPath, os.O_RDWR, 0600)
	if err != nil {
		t.Fatalf("Open f2: %v", err)
	}
	defer f2.Close()

	// f1 acquires non-blocking
	if err := LockExclusiveNonBlocking(f1); err != nil {
		t.Fatalf("LockExclusiveNonBlocking f1: %v", err)
	}

	// f2 should fail with ErrLockHeld
	err = LockExclusiveNonBlocking(f2)
	if err != ErrLockHeld {
		t.Fatalf("expected ErrLockHeld for f2, got: %v", err)
	}

	// unlock f1
	if err := Unlock(f1); err != nil {
		t.Fatalf("Unlock f1: %v", err)
	}

	// now f2 should succeed
	if err := LockExclusiveNonBlocking(f2); err != nil {
		t.Fatalf("LockExclusiveNonBlocking f2 after release: %v", err)
	}
	_ = Unlock(f2)
}

func TestIsProcessAlive(t *testing.T) {
	// Current process must be alive
	if !IsProcessAlive(os.Getpid()) {
		t.Errorf("expected current process %d to be alive", os.Getpid())
	}
	// Non-existent PID should be false
	if IsProcessAlive(99999999) {
		t.Errorf("expected non-existent pid 99999999 to not be alive")
	}
}

func TestAtomicWriteCreatesPrivateParents(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "a", "b", "file.json")
	if err := AtomicWrite(target, []byte("{}"), 0600); err != nil {
		t.Fatalf("AtomicWrite: %v", err)
	}
	if runtime.GOOS == "windows" {
		return
	}
	for _, d := range []string{filepath.Join(root, "a"), filepath.Join(root, "a", "b")} {
		st, err := os.Stat(d)
		if err != nil {
			t.Fatal(err)
		}
		if mode := st.Mode().Perm(); mode != 0700 {
			t.Errorf("%s: parent dir mode %04o, want 0700", d, mode)
		}
	}
	st, _ := os.Stat(target)
	if mode := st.Mode().Perm(); mode != 0600 {
		t.Errorf("file mode %04o, want 0600", mode)
	}
}

func TestAtomicWriteFailureLeavesNoTempFile(t *testing.T) {
	dir := t.TempDir()
	// renaming a file over an existing directory fails on every platform
	target := filepath.Join(dir, "occupied")
	if err := os.Mkdir(target, 0700); err != nil {
		t.Fatal(err)
	}
	if err := AtomicWrite(target, []byte("x"), 0600); err == nil {
		t.Fatal("expected AtomicWrite over a directory to fail")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != "occupied" {
			t.Errorf("leftover temp file after failed write: %s", e.Name())
		}
	}
}

func TestSyncDir(t *testing.T) {
	if err := syncDir(t.TempDir()); err != nil {
		t.Errorf("syncDir on a fresh directory: %v", err)
	}
}

func TestLockNonBlockingReportsOtherErrors(t *testing.T) {
	f, err := Touch(filepath.Join(t.TempDir(), "closed.lock"))
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	err = LockExclusiveNonBlocking(f)
	if err == nil {
		t.Fatal("locking a closed file must fail")
	}
	if err == ErrLockHeld {
		t.Errorf("a closed handle is not lock contention, got ErrLockHeld")
	}
}
