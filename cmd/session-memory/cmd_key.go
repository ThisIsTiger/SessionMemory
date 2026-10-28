package main

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"session-memory/internal/config"
	"session-memory/internal/device"
	"session-memory/internal/errcode"
	"session-memory/internal/platform"
)

// The vault key can be kept in the cloud folder next to the data, so a new
// computer joins with nothing but the cloud access (no passphrase, no invite
// file). That protects against casual viewing, not against whoever can open the
// cloud folder; once every computer has joined the key can be removed again.
// Normal sync never moves this file (see transport.KeyPath).

const keyFormat = "smkey1"

type keyFile struct {
	Format  string `json:"format"`
	VaultID string `json:"vault_id"`
	Key     string `json:"key"` // base64
}

// cloudKey is the key file in whatever transport this device uses.
type cloudKey interface {
	Put(ctx context.Context, data []byte) error // no-op when a key file already exists
	Get(ctx context.Context) ([]byte, bool, error)
	Delete(ctx context.Context) error
}

type weiyunKey struct {
	m interface {
		PutKey(context.Context, []byte) error
		GetKey(context.Context) ([]byte, bool, error)
		DeleteKey(context.Context) error
	}
}

func (w weiyunKey) Put(ctx context.Context, d []byte) error       { return w.m.PutKey(ctx, d) }
func (w weiyunKey) Get(ctx context.Context) ([]byte, bool, error) { return w.m.GetKey(ctx) }
func (w weiyunKey) Delete(ctx context.Context) error              { return w.m.DeleteKey(ctx) }

type folderKey struct{ path string }

func (f folderKey) Put(ctx context.Context, d []byte) error {
	if _, err := os.Stat(f.path); err == nil {
		return nil
	}
	return platform.AtomicWrite(f.path, d, 0600)
}
func (f folderKey) Get(ctx context.Context) ([]byte, bool, error) {
	b, err := os.ReadFile(f.path)
	if os.IsNotExist(err) {
		return nil, false, nil
	}
	return b, err == nil, err
}
func (f folderKey) Delete(ctx context.Context) error {
	if err := os.Remove(f.path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func (a *App) cloudKey() (cloudKey, error) {
	if name := a.Cfg.Transport.Weiyun; name != "" {
		m, err := a.weiyunMirror(name)
		if err != nil {
			return nil, err
		}
		return weiyunKey{m}, nil
	}
	if f := a.Cfg.Transport.Folder; f != "" {
		return folderKey{filepath.Join(config.ExpandPath(f), "public", "vault-key.json")}, nil
	}
	return nil, errcode.New(errcode.Usage, "no transport is configured; attach a Weiyun directory or a folder first")
}

func (a *App) keyMarker(name string) string { return filepath.Join(a.Cfg.StateDir(), name) }

const (
	markKeyRemoved = "key-removed-from-cloud" // the user took the key out: do not put it back by itself
	markKeyHint    = "key-hint-pending"       // this computer joined from the cloud key: offer to remove it after the next sync
)

func touch(path string) { _ = platform.AtomicWrite(path, []byte("1\n"), 0600) }
func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func (a *App) localKeyFile() ([]byte, error) {
	info, err := device.At(a.Cfg.StateDir()).Load()
	if err != nil {
		return nil, err
	}
	key, err := a.Keys.Load(info.VaultID)
	if err != nil {
		return nil, errcode.Wrap(errcode.NoVault, "the vault key is not available on this computer", err)
	}
	return json.Marshal(keyFile{Format: keyFormat, VaultID: info.VaultID, Key: base64.StdEncoding.EncodeToString(key)})
}

// publishCloudKey puts this vault's key in the cloud folder unless the user
// removed it on purpose (force overrides that) or it is already there.
func (a *App) publishCloudKey(ctx context.Context, force bool) (bool, error) {
	if !force && exists(a.keyMarker(markKeyRemoved)) {
		return false, nil
	}
	ck, err := a.cloudKey()
	if err != nil {
		return false, err
	}
	if _, ok, err := ck.Get(ctx); err != nil {
		return false, err
	} else if ok {
		return false, nil
	}
	data, err := a.localKeyFile()
	if err != nil {
		return false, err
	}
	if err := ck.Put(ctx, data); err != nil {
		return false, err
	}
	os.Remove(a.keyMarker(markKeyRemoved))
	return true, nil
}

// fetchCloudKey reads the vault id and key a computer needs to join.
func (a *App) fetchCloudKey(ctx context.Context) (string, []byte, error) {
	ck, err := a.cloudKey()
	if err != nil {
		return "", nil, err
	}
	data, ok, err := ck.Get(ctx)
	if err != nil {
		return "", nil, errcode.Wrap(errcode.Usage, "cannot read the cloud folder: "+err.Error(), err)
	}
	if !ok {
		return "", nil, errcode.New(errcode.NotFound, "the cloud folder has no key file; on a computer that already has the vault run `session-memory key publish`")
	}
	var kf keyFile
	if json.Unmarshal(data, &kf) != nil || kf.Format != keyFormat || kf.VaultID == "" {
		return "", nil, errcode.New(errcode.UnknownFormat, "the key file in the cloud folder is not readable")
	}
	key, err := base64.StdEncoding.DecodeString(kf.Key)
	if err != nil || len(key) == 0 {
		return "", nil, errcode.New(errcode.UnknownFormat, "the key file in the cloud folder is damaged")
	}
	return kf.VaultID, key, nil
}

// stdinIsTerminal is true when a person can answer: a character device that is
// not /dev/null (which is what agents and test runners hand to a command).
func stdinIsTerminal() bool {
	st, err := os.Stdin.Stat()
	if err != nil || st.Mode()&os.ModeCharDevice == 0 {
		return false
	}
	if null, err := os.Stat(os.DevNull); err == nil && os.SameFile(st, null) {
		return false
	}
	return true
}

func (a *App) removeCloudKey(ctx context.Context) error {
	ck, err := a.cloudKey()
	if err != nil {
		return err
	}
	if err := ck.Delete(ctx); err != nil {
		return err
	}
	touch(a.keyMarker(markKeyRemoved))
	os.Remove(a.keyMarker(markKeyHint))
	return nil
}

// offerKeyRemoval runs after a successful foreground sync on a computer that
// joined with the cloud key: it says the key is still in the cloud and offers
// to remove it. At a terminal it asks; from an agent or script it only prints,
// so the agent can ask the user and then run `key remove --yes`.
func offerKeyRemoval(ctx context.Context, a *App) {
	if !exists(a.keyMarker(markKeyHint)) {
		return
	}
	os.Remove(a.keyMarker(markKeyHint)) // say it once
	fmt.Fprintln(a.Err, "note: the vault key is still stored in your cloud folder next to the data, so anyone who can open that folder can read the sessions.")
	fmt.Fprintln(a.Err, "      Once all your computers have joined you can remove it (it goes to the cloud recycle bin); computers that already joined keep working.")
	if !stdinIsTerminal() {
		fmt.Fprintln(a.Err, "      Remove it with: session-memory key remove --yes")
		return
	}
	fmt.Fprint(a.Err, "Remove the key from the cloud now? [y/N] ")
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(line)), "y") {
		if err := a.removeCloudKey(ctx); err != nil {
			fmt.Fprintf(a.Err, "could not remove it: %v\n", err)
			return
		}
		fmt.Fprintln(a.Err, "removed; `session-memory key publish` puts it back")
	}
}

func newKeyCmd(a *App) *cobra.Command {
	c := &cobra.Command{Use: "key", Short: "The vault key in the cloud folder (lets a new computer join without a passphrase)"}
	c.AddCommand(&cobra.Command{Use: "publish", Short: "Put this computer's vault key in the cloud folder", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			put, err := a.publishCloudKey(cmd.Context(), true)
			if err != nil {
				return err
			}
			if put {
				fmt.Fprintln(a.Out, "the vault key is now in the cloud folder")
			} else {
				fmt.Fprintln(a.Out, "the cloud folder already has a key file")
			}
			return nil
		}})
	rm := &cobra.Command{Use: "remove", Short: "Remove the vault key from the cloud folder (recycle bin); joined computers keep working", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if yes, _ := cmd.Flags().GetBool("yes"); !yes {
				if !stdinIsTerminal() {
					return errcode.New(errcode.Usage, "ask the user first, then run again with --yes")
				}
				fmt.Fprint(a.Err, "Remove the vault key from the cloud folder? New computers can then not join until it is published again. [y/N] ")
				line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
				if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(line)), "y") {
					return nil
				}
			}
			if err := a.removeCloudKey(cmd.Context()); err != nil {
				return err
			}
			fmt.Fprintln(a.Out, "removed from the cloud folder (recycle bin); `session-memory key publish` puts it back")
			return nil
		}}
	rm.Flags().Bool("yes", false, "confirm without asking (use only after the user agreed)")
	c.AddCommand(rm)
	c.AddCommand(&cobra.Command{Use: "status", Short: "Say whether the cloud folder holds the vault key", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ck, err := a.cloudKey()
			if err != nil {
				return err
			}
			_, ok, err := ck.Get(cmd.Context())
			if err != nil {
				return err
			}
			if ok {
				fmt.Fprintln(a.Out, "the vault key is in the cloud folder")
			} else {
				fmt.Fprintln(a.Out, "the vault key is not in the cloud folder")
			}
			return nil
		}})
	return c
}
