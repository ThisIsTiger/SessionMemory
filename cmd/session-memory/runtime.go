package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"

	"session-memory/internal/agent"
	"session-memory/internal/config"
	"session-memory/internal/device"
	"session-memory/internal/engine"
	"session-memory/internal/errcode"
	"session-memory/internal/platform"
	"session-memory/internal/session"
	"session-memory/internal/source/codex"
	"session-memory/internal/source/workbuddy"
	"session-memory/internal/store"
	syncpkg "session-memory/internal/sync"
	"session-memory/internal/transport"
	"session-memory/internal/vault"
)

// newKeyStore is replaced in tests.
var newKeyStore = func() vault.KeyStore { return vault.NewKeyring() }

// rt is an opened runtime: store, vault, device, engine and (when a sync
// folder is attached) the sync engine. Commands that write take the store lock.
type rt struct {
	Root   string
	Store  *store.FS
	Vault  *vault.Vault
	Info   device.Info
	Eng    *engine.Engine
	Sync   *syncpkg.Sync
	Multi  *transport.MultiMirror // the Weiyun accounts, when Weiyun is the transport
	unlock func()

	// claims lets tracking link native sessions to sessions the vault already
	// holds (see trackOne); set by setup, which reads the vault's list for it.
	claims *engine.ClaimIndex
	// claimed counts the sessions linked rather than tracked, claimedRefs the
	// project refs they carry (reset by whoever reads them).
	claimed     int
	claimedRefs map[string]bool
}

func (r *rt) Close() {
	r.Store.Close()
	if r.unlock != nil {
		r.unlock()
	}
}

// wrapAdapter lets a test stand between the commands and a real adapter.
var wrapAdapter = func(ad agent.Adapter) agent.Adapter { return ad }

func (a *App) adapter(name string) (agent.Adapter, error) {
	switch name {
	case "codex":
		return wrapAdapter(codex.New(a.Cfg)), nil
	case "workbuddy":
		return wrapAdapter(workbuddy.New(a.Cfg)), nil
	}
	return nil, errcode.New(errcode.Unsupported, name+" adapter is not available yet")
}

func lockStore(root string) (func(), error) {
	release, err := lockStateLifetime(root, false, lockWait)
	if err != nil {
		return nil, err
	}
	unlock, err := lockFile(filepath.Join(root, "locks", "store.lock"), false, lockWait)
	if err != nil {
		release()
		return nil, err
	}
	return func() { unlock(); release() }, nil
}

// This guard lives beside the state directory, so reset cannot move it away
// and let a new process lock a different inode at the old path. Shared users
// include waiting hooks and commands that write config before opening a Store.
func lockStateLifetime(root string, exclusive bool, wait time.Duration) (func(), error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	// Resolve parent aliases without depending on the state directory existing.
	if err := os.MkdirAll(filepath.Dir(abs), 0700); err != nil {
		return nil, err
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(abs))
	if err != nil {
		return nil, err
	}
	path := filepath.Join(parent, filepath.Base(abs)) + ".state.lock"
	return lockFile(path, !exclusive, wait)
}

func lockFile(path string, shared bool, wait time.Duration) (func(), error) {
	// Do not chmod an existing parent: the lifetime guard may be in HOME.
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	// a background relay may hold the lock for a minute or two: wait for it
	// instead of failing the command the user just typed
	deadline := time.Now().Add(wait)
	for {
		var err error
		if shared {
			err = platform.LockSharedNonBlocking(f)
		} else {
			err = platform.LockExclusiveNonBlocking(f)
		}
		if err == nil {
			return func() { platform.Unlock(f); f.Close() }, nil
		}
		if !errors.Is(err, platform.ErrLockHeld) {
			f.Close()
			return nil, err
		}
		if time.Now().After(deadline) {
			f.Close()
			return nil, errcode.New(errcode.Internal, "another session-memory is running (sync, setup or hook worker); retry after it finishes")
		}
		time.Sleep(min(500*time.Millisecond, time.Until(deadline)))
	}
}

// folderSettle is how long a file in the sync folder must stay unchanged before
// it is read (the transport's default); tests shorten it.
var folderSettle = transport.NewFolder("").Settle

// lockWait is how long a command waits for another session-memory to finish.
var lockWait = 3 * time.Minute

// openRT opens the runtime. needSync requires a configured sync folder.
func (a *App) openRT(ctx context.Context, needSync bool) (*rt, error) {
	root := a.Cfg.StateDir()
	unlock, err := lockStore(root)
	if err != nil {
		return nil, err
	}
	keepLock := false
	defer func() {
		if !keepLock {
			unlock()
		}
	}()
	info, err := device.At(root).Load()
	if err != nil {
		return nil, err
	}
	key, err := a.Keys.Load(info.VaultID)
	if err != nil {
		return nil, errcode.Wrap(errcode.NoVault, "the vault key is not available in the credential store", err)
	}
	v, err := vault.New(info.VaultID, key)
	if err != nil {
		return nil, err
	}
	st, err := store.Open(root, v)
	if err != nil {
		return nil, err
	}
	st.InstallGuard = func(ctx context.Context, d store.InstallData) (func(), error) {
		if d.Agent == workbuddy.AgentName {
			return workbuddy.GuardRecovery(ctx, d)
		}
		return codex.GuardRecovery(ctx, d)
	}
	st.InstallRowUndo = workbuddy.UndoRegistry
	keepLock = true
	r := &rt{Root: root, Store: st, Vault: v, Info: info, unlock: unlock,
		Eng: &engine.Engine{Store: st, DeviceID: info.DeviceID}}
	if name := a.Cfg.Transport.Weiyun; name != "" {
		m, err := a.weiyunMirror(name)
		if err != nil {
			r.Close()
			return nil, err
		}
		r.Sync = syncpkg.New(st, m, v, root, info)
		r.Multi = m
	} else if folder := a.Cfg.Transport.Folder; folder != "" {
		tf := transport.NewFolder(config.ExpandPath(folder))
		tf.Settle = folderSettle
		r.Sync = syncpkg.New(st, tf, v, root, info)
	} else if needSync {
		r.Close()
		return nil, errcode.New(errcode.Usage, "no sync folder configured; run transport attach-folder or attach-weiyun")
	}
	if _, err := st.Recover(ctx); err != nil {
		r.Close()
		return nil, err
	}
	return r, nil
}

func writeJSONFile(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return platform.AtomicWrite(path, append(b, '\n'), 0644)
}

var _ = session.SchemaVersion

// Sync2 returns the device accessor (outbox) even without a sync folder.
func (r *rt) Sync2() *device.Device { return device.At(r.Root) }

func jsonMarshal(v any) ([]byte, error) { return json.Marshal(v) }
