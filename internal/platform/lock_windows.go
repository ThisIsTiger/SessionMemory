//go:build windows

package platform

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"syscall"
	"unsafe"
)

var (
	modkernel32      = syscall.NewLazyDLL("kernel32.dll")
	procLockFileEx   = modkernel32.NewProc("LockFileEx")
	procUnlockFileEx = modkernel32.NewProc("UnlockFileEx")
)

const (
	lockfileFailImmediately = 0x00000001
	lockfileExclusiveLock   = 0x00000002
	processQueryInfo        = 0x0400
	stillActive             = 259

	// errorLockViolation is what LockFileEx reports when
	// LOCKFILE_FAIL_IMMEDIATELY finds the range already locked.
	errorLockViolation syscall.Errno = 33
)

type overlapped struct {
	Internal     uintptr
	InternalHigh uintptr
	Offset       uint32
	OffsetHigh   uint32
	HEvent       syscall.Handle
}

// LockExclusive acquires a blocking exclusive lock via LockFileEx.
func LockExclusive(f *os.File) error {
	ol := new(overlapped)
	r1, _, err := procLockFileEx.Call(f.Fd(), lockfileExclusiveLock, 0,
		0xFFFFFFFF, 0xFFFFFFFF, uintptr(unsafe.Pointer(ol)))
	runtime.KeepAlive(f)
	if r1 == 0 {
		return fmt.Errorf("LockFileEx: %w", err)
	}
	return nil
}

// LockExclusiveNonBlocking returns ErrLockHeld only when another holder owns
// the lock; any other failure (invalid handle, I/O error) is returned as is so
// callers do not mistake it for contention.
func LockExclusiveNonBlocking(f *os.File) error {
	ol := new(overlapped)
	r1, _, callErr := procLockFileEx.Call(f.Fd(), lockfileExclusiveLock|lockfileFailImmediately, 0,
		0xFFFFFFFF, 0xFFFFFFFF, uintptr(unsafe.Pointer(ol)))
	runtime.KeepAlive(f)
	if r1 == 0 {
		if errors.Is(callErr, errorLockViolation) {
			return ErrLockHeld
		}
		return fmt.Errorf("LockFileEx: %w", callErr)
	}
	return nil
}

// Unlock releases the lock via UnlockFileEx.
func Unlock(f *os.File) error {
	ol := new(overlapped)
	r1, _, err := procUnlockFileEx.Call(f.Fd(), 0, 0xFFFFFFFF, 0xFFFFFFFF,
		uintptr(unsafe.Pointer(ol)))
	runtime.KeepAlive(f)
	if r1 == 0 {
		return fmt.Errorf("UnlockFileEx: %w", err)
	}
	return nil
}

// DetachProcess hides the child console window.
func DetachProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x00000010}
}

// IsProcessAlive reports whether pid is still running on Windows.
func IsProcessAlive(pid int) bool {
	handle, err := syscall.OpenProcess(processQueryInfo, false, uint32(pid))
	if err != nil {
		return false
	}
	defer syscall.CloseHandle(handle)
	var exitCode uint32
	if err := syscall.GetExitCodeProcess(handle, &exitCode); err != nil {
		return false
	}
	return exitCode == stillActive
}
