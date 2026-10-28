package workbuddy

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"session-memory/internal/agent"
	"session-memory/internal/errcode"
	"session-memory/internal/platform"
	"session-memory/internal/store"
)

// The registry writer is tied to the 5.x line, not to one release: what it
// really depends on (the sessions table layout, no triggers, the file format)
// is checked on every write by the schema fingerprint and the record checks.
const supportedMajor = "5"

func supportedVersion(v string) bool {
	major, _, _ := strings.Cut(strings.TrimSpace(v), ".")
	return major == supportedMajor
}

type runtimeDetector struct{}

func (a *Adapter) Runtime() agent.RuntimeDetector { return runtimeDetector{} }

// Any WorkBuddy instance blocks registry writes, including other profiles.
func (runtimeDetector) Running(ctx context.Context, home, _ string) (agent.RunState, agent.RunDetail, error) {
	if runtime.GOOS != "darwin" {
		return agent.Unknown, agent.DetailUnknown, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "/bin/ps", "-axo", "comm=").Output()
	if err != nil {
		return agent.Unknown, agent.DetailUnknown, err
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.Contains(line, "/WorkBuddy.app/Contents/") {
			return agent.Running, agent.DetailProcessAlive, nil
		}
	}
	if link, err := os.Readlink(filepath.Join(home, "app", "SingletonLock")); err == nil {
		i := strings.LastIndexByte(link, '-')
		if i < 0 {
			return agent.Unknown, agent.DetailUnknown, nil
		}
		pid, err := strconv.Atoi(link[i+1:])
		if err != nil || pid < 1 {
			return agent.Unknown, agent.DetailUnknown, nil
		}
		if platform.IsProcessAlive(pid) {
			return agent.Running, agent.DetailProcessAlive, nil
		}
	} else if !os.IsNotExist(err) {
		return agent.Unknown, agent.DetailUnknown, err
	}
	return agent.NotRunning, agent.DetailNotRunning, nil
}

func checkVersion(ctx context.Context) error {
	if runtime.GOOS != "darwin" {
		return errcode.New(errcode.Unsupported, "WorkBuddy registry writes currently support macOS 5.8.3 only")
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "/usr/bin/plutil", "-extract", "CFBundleShortVersionString", "raw", "-o", "-", "/Applications/WorkBuddy.app/Contents/Info.plist").Output()
	if err != nil {
		return errcode.New(errcode.UnknownFormat, "cannot read the installed WorkBuddy version")
	}
	if v := strings.TrimSpace(string(out)); !supportedVersion(v) {
		return errcode.New(errcode.UnknownFormat, "installed WorkBuddy "+v+" is outside the 5.x line the registry writer supports")
	}
	return nil
}

func checkClosed(ctx context.Context, home string) error {
	state, _, err := (runtimeDetector{}).Running(ctx, home, "")
	if err != nil {
		return errcode.New(errcode.ClientRunning, "cannot tell whether WorkBuddy is running ("+err.Error()+"); nothing was written")
	}
	switch state {
	case agent.NotRunning:
		return nil
	case agent.Unknown:
		return errcode.New(errcode.ClientRunning, "cannot tell whether WorkBuddy is running; nothing was written")
	}
	return errcode.New(errcode.ClientRunning, "quit WorkBuddy completely before restoring its sessions")
}

// This lock serializes SessionMemory writers, not WorkBuddy itself. Explicit
// client-closed, repeated process checks and CAS remain mandatory.
func (w *Writer) lockHome(ctx context.Context, home string) (func(), error) {
	w.defaults()
	if !w.ClientClosed {
		return nil, errcode.New(errcode.ClientRunning, "WorkBuddy restore requires --client-closed after quitting the application")
	}
	if err := w.version(ctx); err != nil {
		return nil, err
	}
	if err := w.closed(ctx, home); err != nil {
		return nil, err
	}
	path, err := safeTarget(home, ".session-memory-install.lock")
	if err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := platform.LockExclusiveNonBlocking(f); err != nil {
		f.Close()
		if errors.Is(err, platform.ErrLockHeld) {
			return nil, errcode.New(errcode.ClientRunning, "another WorkBuddy install is running")
		}
		return nil, err
	}
	release := func() { platform.Unlock(f); f.Close() }
	if err := w.closed(ctx, home); err != nil {
		release()
		return nil, err
	}
	return release, nil
}

func (w *Writer) LockInstall(ctx context.Context, in agent.WriteInput) (func(), error) {
	return w.lockHome(ctx, in.TargetState.Home)
}

func GuardRecovery(ctx context.Context, d store.InstallData) (func(), error) {
	return (&Writer{ClientClosed: d.ClientClosed, version: checkVersion, closed: checkClosed}).lockHome(ctx, d.Home)
}
