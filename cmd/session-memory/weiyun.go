package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"session-memory/internal/errcode"
	"session-memory/internal/transport"
)

// envWeiyunToken names the environment variable holding the user's Weiyun
// access token. It is deliberately never stored in the config file; for
// background runs (autosync) it is kept in the OS credential store.
const envWeiyunToken = "WEIYUN_MCP_TOKEN"

// A second Weiyun account can hold a full copy of the vault: when one account
// runs out of its daily quota or fails, the other carries on.
const envWeiyunToken2 = "WEIYUN_MCP_TOKEN_2"

// weiyunTokenKey is the credential-store id of the saved token.
const (
	weiyunTokenKey  = "weiyun-mcp-token"
	weiyunTokenKey2 = "weiyun-mcp-token-2"
)

// id names the account itself, not the slot: the slot's token can be swapped,
// and pauses, remembered keys and "behind" marks belong to the account.
type wyAccount struct{ label, token, envName, storeKey, id string }

var wyAccountSlots = []wyAccount{
	{label: "primary", envName: envWeiyunToken, storeKey: weiyunTokenKey},
	{label: "backup", envName: envWeiyunToken2, storeKey: weiyunTokenKey2},
}

// weiyunAccounts are the configured accounts: each token from the
// environment, else the saved one.
func (a *App) weiyunAccounts() []wyAccount {
	var out []wyAccount
	for _, slot := range wyAccountSlots {
		t := os.Getenv(slot.envName)
		if t == "" {
			if b, err := a.Keys.Load(slot.storeKey); err == nil {
				t = string(b)
			}
		}
		if t != "" {
			slot.token = t
			sum := sha256.Sum256([]byte(t))
			slot.id = slot.label + "#" + hex.EncodeToString(sum[:])[:6]
			out = append(out, slot)
		}
	}
	return out
}

// saveWeiyunToken keeps the environment's tokens in the credential store so
// that background runs, which do not see the shell environment, can use them.
func (a *App) saveWeiyunToken() error {
	saved := 0
	for _, slot := range wyAccountSlots {
		if t := os.Getenv(slot.envName); t != "" {
			if err := a.Keys.Store(slot.storeKey, []byte(t)); err != nil {
				return err
			}
			saved++
		}
	}
	if saved == 0 && len(a.weiyunAccounts()) == 0 {
		return errcode.New(errcode.Usage, "set "+envWeiyunToken+" first")
	}
	return nil
}

// weiyunCache is the local working copy of the Weiyun vault folder.
func (a *App) weiyunCache() string { return filepath.Join(a.Cfg.StateDir(), "weiyun-cache") }

// weiyunMirror returns the mirror over every configured account for the
// configured Weiyun directory, or nil when Weiyun is not the transport.
func (a *App) weiyunMirror(name string) (*transport.MultiMirror, error) {
	if name == "" {
		return nil, nil
	}
	accounts := a.weiyunAccounts()
	if len(accounts) == 0 {
		return nil, errcode.New(errcode.Usage, "set "+envWeiyunToken+" to use the Weiyun transport")
	}
	if err := os.MkdirAll(a.weiyunCache(), 0700); err != nil {
		return nil, err
	}
	a.migrateAccountKeys(accounts)
	local := transport.NewFolder(a.weiyunCache())
	var mirrors []transport.Account
	for _, ac := range accounts {
		ac := ac
		c := transport.NewWeiyunClient(ac.token)
		if u := os.Getenv("WEIYUN_MCP_URL"); u != "" {
			c.URL = u
		}
		recorded := false
		c.OnQuota = func(error) {
			if !recorded {
				recorded = true
				a.recordQuota(ac.id)
			}
		}
		c.OnOK = func() { a.clearQuota(ac.id) }
		mir := transport.NewMirror(local, c, name)
		mir.KeyCache = filepath.Join(a.Cfg.StateDir(), "weiyun-keys-"+strings.ReplaceAll(ac.id, "#", "-")+".json")
		mirrors = append(mirrors, transport.Account{Label: ac.id, M: mir})
	}
	mm := transport.NewMultiMirror(local, mirrors...)
	mm.StateFile = filepath.Join(a.Cfg.StateDir(), "weiyun-behind.json")
	// a background run leaves an account alone while its quota is known to be used up
	mm.Skip = func(label string) bool {
		_, paused := a.quotaPausedUntil(label)
		return a.quietRun && paused
	}
	mm.Note = func(label string, err error) {
		if !errors.Is(err, transport.ErrQuota) {
			fmt.Fprintf(a.Err, "warning: Weiyun account %s: %v\n", label, err)
		}
	}
	return mm, nil
}
