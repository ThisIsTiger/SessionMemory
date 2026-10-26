package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"

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
	unlock func()
}

func (r *rt) Close() {
	r.Store.Close()
	if r.unlock != nil {
		r.unlock()
	}
}

func (a *App) adapter(name string) (agent.Adapter, error) {
	switch name {
	case "codex":
		return codex.New(a.Cfg), nil
	case "workbuddy":
		return workbuddy.New(a.Cfg), nil
	}
	return nil, errcode.New(errcode.Unsupported, name+" adapter is not available yet")
}

func lockStore(root string) (func(), error) {
	f, err := platform.Touch(filepath.Join(root, "locks", "store.lock"))
	if err != nil {
		return nil, err
	}
	if err := platform.LockExclusiveNonBlocking(f); err != nil {
		f.Close()
		if errors.Is(err, platform.ErrLockHeld) {
			return nil, errcode.New(errcode.Internal, "another session-memory is running")
		}
		return nil, err
	}
	return func() { platform.Unlock(f); f.Close() }, nil
}

// openRT opens the runtime. needSync requires a configured sync folder.
func (a *App) openRT(ctx context.Context, needSync bool) (*rt, error) {
	root := a.Cfg.StateDir()
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
	if err := os.MkdirAll(filepath.Join(root, "locks"), 0700); err != nil {
		return nil, err
	}
	unlock, err := lockStore(root)
	if err != nil {
		return nil, err
	}
	st, err := store.Open(root, v)
	if err != nil {
		unlock()
		return nil, err
	}
	r := &rt{Root: root, Store: st, Vault: v, Info: info, unlock: unlock,
		Eng: &engine.Engine{Store: st, DeviceID: info.DeviceID}}
	if _, err := st.Recover(ctx); err != nil {
		r.Close()
		return nil, err
	}
	if folder := a.Cfg.Transport.Folder; folder != "" {
		r.Sync = syncpkg.New(st, transport.NewFolder(config.ExpandPath(folder)), v, root, info)
	} else if needSync {
		r.Close()
		return nil, errcode.New(errcode.Usage, "no sync folder configured; run transport attach-folder")
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
