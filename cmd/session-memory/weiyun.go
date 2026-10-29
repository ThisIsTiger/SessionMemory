package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"session-memory/internal/device"
	"session-memory/internal/errcode"
	"session-memory/internal/transport"
	"session-memory/internal/vault"
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
type wyAccount struct {
	label, token, envName, storeKey, id string
	saved                               bool // the token is the one kept in the credential store
}

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
				slot.saved = t != ""
			}
		}
		if t == "" && slot.label == "primary" {
			t = mcporterWeiyunToken() // Weiyun is already connected for the agent's own skill
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

// mcporterWeiyunToken is the token the Weiyun skill keeps in its mcporter
// configuration once the user has authorised it, so a computer where Weiyun
// already works needs no token to be handed over. It is only the last resort:
// an environment or saved token wins.
func mcporterWeiyunToken() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	b, err := os.ReadFile(filepath.Join(home, ".mcporter", "mcporter.json"))
	if err != nil {
		return ""
	}
	var cfg struct {
		Servers map[string]struct {
			Headers map[string]string `json:"headers"`
		} `json:"mcpServers"`
	}
	if json.Unmarshal(b, &cfg) != nil {
		return ""
	}
	h := cfg.Servers["weiyun"].Headers["WyHeader"]
	const key = "mcp_token="
	if i := strings.Index(h, key); i >= 0 {
		return strings.TrimSpace(h[i+len(key):])
	}
	return ""
}

// saveWeiyunToken keeps the environment's tokens in the credential store so
// that background runs, which do not see the shell environment, can use them.
func (a *App) saveWeiyunToken() error {
	saved := 0
	for _, slot := range wyAccountSlots {
		t := os.Getenv(slot.envName)
		if t == "" && slot.label == "primary" {
			if _, err := a.Keys.Load(slot.storeKey); err != nil {
				t = mcporterWeiyunToken() // nothing saved yet: keep the one the Weiyun skill holds
			}
		}
		if t != "" {
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

// reauthorizedToken is called when the saved token is rejected. After the
// user authorises again in the Weiyun skill, the skill holds a new token; it
// is used only when it opens this computer's own vault (never a different
// account's), and is then saved in place of the rejected one.
func (a *App) reauthorizedToken(ctx context.Context, ac wyAccount, dir string) string {
	fresh := mcporterWeiyunToken()
	if fresh == "" || fresh == ac.token {
		return ""
	}
	info, err := device.At(a.Cfg.StateDir()).Load()
	if err != nil {
		return ""
	}
	c := transport.NewWeiyunClient(fresh)
	if u := os.Getenv("WEIYUN_MCP_URL"); u != "" {
		c.URL = u
	}
	data, ok, err := c.ReadFile(ctx, dir, "public", "vault.json")
	var v vault.Info
	if err != nil || !ok || json.Unmarshal(data, &v) != nil || v.VaultID != info.VaultID {
		return ""
	}
	if a.Keys.Store(ac.storeKey, []byte(fresh)) != nil {
		return ""
	}
	return fresh
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
	local.Settle = folderSettle
	var mirrors []transport.Account
	for _, ac := range accounts {
		ac := ac
		c := transport.NewWeiyunClient(ac.token)
		if u := os.Getenv("WEIYUN_MCP_URL"); u != "" {
			c.URL = u
		}
		if ac.saved && ac.label == "primary" {
			c.Reauth = func(ctx context.Context) string { return a.reauthorizedToken(ctx, ac, name) }
		}
		recorded := false
		c.OnQuota = func(error) {
			if !recorded {
				recorded = true
				a.recordQuota(ac.id)
			}
		}
		ratedOnce := false
		c.OnRate = func(error) {
			if !ratedOnce {
				ratedOnce = true
				a.recordRate(ac.id)
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
		if !errors.Is(err, transport.ErrQuota) && !errors.Is(err, transport.ErrRate) {
			fmt.Fprintf(a.Err, "warning: Weiyun account %s: %v\n", label, err)
		}
	}
	return mm, nil
}
