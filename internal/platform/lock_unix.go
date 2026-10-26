//go:build !windows

package platform

import (
	"os"
	"os/exec"
	"syscall"
)

// LockExclusive acquires a blocking exclusive advisory lock on f. A signal
// interrupting the wait is retried rather than reported as a failure.
func LockExclusive(f *os.File) error {
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
		if err != syscall.EINTR {
			return err
		}
	}
}

// LockExclusiveNonBlocking returns ErrLockHeld when the lock is busy.
func LockExclusiveNonBlocking(f *os.File) error {
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		switch err {
		case syscall.EINTR:
			continue
		case syscall.EWOULDBLOCK: // == EAGAIN on the supported platforms
			return ErrLockHeld
		}
		return err
	}
}

// Unlock releases the advisory lock on f.
func Unlock(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}

// DetachProcess configures cmd to survive its parent (setsid).
func DetachProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}

// IsProcessAlive reports whether pid is currently running.
func IsProcessAlive(pid int) bool {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return proc.Signal(syscall.Signal(0)) == nil
}
