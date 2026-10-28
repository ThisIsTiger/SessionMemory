package main

import (
	"context"
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
	"session-memory/internal/session"
	"session-memory/internal/vault"
)

func readPassphrase(cmd *cobra.Command) (string, error) {
	f, _ := cmd.Flags().GetString("passphrase-file")
	if f == "" {
		return "", errcode.New(errcode.Usage, "--passphrase-file is required (the passphrase is never taken from the command line)")
	}
	b, err := os.ReadFile(f)
	if err != nil {
		return "", errcode.Wrap(errcode.Usage, "cannot read the passphrase file", err)
	}
	return strings.TrimRight(string(b), "\r\n"), nil
}

func newInitCmd(a *App) *cobra.Command {
	return &cobra.Command{Use: "init", Short: "Create a vault and this device's identity", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			root := a.Cfg.StateDir()
			if err := platform.EnsurePrivateDir(filepath.Join(root, "device")); err != nil {
				return err
			}
			if _, err := device.At(root).Load(); err == nil {
				return errcode.New(errcode.Conflict, "this device is already initialised")
			}
			key, err := vault.GenerateKey()
			if err != nil {
				return err
			}
			id := session.NewVaultID()
			if err := a.Keys.Store(id, key); err != nil {
				return errcode.Wrap(errcode.NoVault, "cannot store the vault key in the credential store", err)
			}
			info, err := device.At(root).Init(id, session.FormatTime(a.Now()))
			if err != nil {
				return err
			}
			if a.JSON {
				return a.printJSON(map[string]string{"vault_id": id, "device_id": info.DeviceID})
			}
			fmt.Fprintf(a.Out, "vault %s\ndevice %s\n", id, info.DeviceID)
			return nil
		}}
}

// ensureVaultJSON writes public/vault.json into the folder, or verifies the
// existing one against the local key.
func ensureVaultJSON(v *vault.Vault, folder string, now string) error {
	p := filepath.Join(folder, "public", "vault.json")
	if b, err := os.ReadFile(p); err == nil {
		var info vault.Info
		if json.Unmarshal(b, &info) != nil || !v.CheckKey(info) {
			return errcode.New(errcode.AuthFailed, "the folder belongs to a different vault")
		}
		return nil
	}
	info, err := v.NewInfo(now)
	if err != nil {
		return err
	}
	return writeJSONFile(p, info)
}

func newTransportCmd(a *App) *cobra.Command {
	c := &cobra.Command{Use: "transport", Short: "Sync folder"}
	c.AddCommand(&cobra.Command{Use: "attach-weiyun [dir-name]", Short: "Use a Weiyun directory as the shared vault (token from WEIYUN_MCP_TOKEN)", Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := "SessionMemory"
			if len(args) == 1 {
				name = args[0]
			}
			m, err := a.weiyunMirror(name)
			if err != nil {
				return err
			}
			info, err := device.At(a.Cfg.StateDir()).Load()
			if err != nil {
				return err
			}
			key, err := a.Keys.Load(info.VaultID)
			if err != nil {
				return errcode.Wrap(errcode.NoVault, "the vault key is not available", err)
			}
			v, _ := vault.New(info.VaultID, key)
			if err := m.Pull(cmd.Context()); err != nil {
				return errcode.Wrap(errcode.Usage, "cannot reach Weiyun: "+err.Error(), err)
			}
			if err := ensureVaultJSON(v, a.weiyunCache(), session.FormatTime(a.Now())); err != nil {
				return err
			}
			if err := m.Push(cmd.Context()); err != nil {
				return errcode.Wrap(errcode.Usage, "cannot publish to Weiyun: "+err.Error(), err)
			}
			if err := config.Set("transport.weiyun", name); err != nil {
				return errcode.Wrap(errcode.Usage, err.Error(), err)
			}
			a.Cfg.Transport.Weiyun = name
			if err := a.saveWeiyunToken(); err != nil { // background runs read the saved tokens
				fmt.Fprintf(a.Err, "warning: could not save the Weiyun token(s) for background runs: %v\n", err)
			}
			if put, err := a.publishCloudKey(cmd.Context(), false); err != nil {
				touch(a.keyMarker(markKeyPending)) // setup and relay try again
				fmt.Fprintf(a.Err, "warning: could not put the vault key in the cloud folder (%v); a new computer will need an invite bundle\n", err)
			} else if put {
				fmt.Fprintln(a.Err, "note: the vault key is stored in the cloud folder so new computers can join without a passphrase")
			}
			if a.JSON {
				return a.printJSON(map[string]string{"weiyun": name})
			}
			fmt.Fprintln(a.Out, "weiyun:"+name)
			return nil
		}})
	c.AddCommand(&cobra.Command{Use: "attach-folder <dir>", Short: "Use a directory as the shared vault folder", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			abs, err := filepath.Abs(args[0])
			if err != nil {
				return err
			}
			if err := os.MkdirAll(abs, 0700); err != nil {
				return errcode.Wrap(errcode.Usage, "cannot create the folder", err)
			}
			probe := filepath.Join(abs, ".sm-write-test")
			if err := os.WriteFile(probe, []byte("x"), 0600); err != nil {
				return errcode.Wrap(errcode.Usage, "the folder is not writable", err)
			}
			os.Remove(probe)
			info, err := device.At(a.Cfg.StateDir()).Load()
			if err != nil {
				return err
			}
			key, err := a.Keys.Load(info.VaultID)
			if err != nil {
				return errcode.Wrap(errcode.NoVault, "the vault key is not available", err)
			}
			v, _ := vault.New(info.VaultID, key)
			if err := ensureVaultJSON(v, abs, session.FormatTime(a.Now())); err != nil {
				return err
			}
			if err := config.Set("transport.folder", abs); err != nil {
				return errcode.Wrap(errcode.Usage, err.Error(), err)
			}
			a.Cfg.Transport.Folder = abs
			if put, err := a.publishCloudKey(cmd.Context(), false); err != nil {
				touch(a.keyMarker(markKeyPending)) // setup and relay try again
				fmt.Fprintf(a.Err, "warning: could not put the vault key in the folder (%v); a new computer will need an invite bundle\n", err)
			} else if put {
				fmt.Fprintln(a.Err, "note: the vault key is stored in the folder so new computers can join without a passphrase")
			}
			if a.JSON {
				return a.printJSON(map[string]string{"folder": abs})
			}
			fmt.Fprintln(a.Out, abs)
			return nil
		}})
	return c
}

func newDeviceCmd(a *App) *cobra.Command {
	c := &cobra.Command{Use: "device", Short: "Device membership"}
	inv := &cobra.Command{Use: "invite", Short: "Write a join bundle for another device", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			out, _ := cmd.Flags().GetString("out")
			if out == "" {
				return errcode.New(errcode.Usage, "--out is required")
			}
			pass, err := readPassphrase(cmd)
			if err != nil {
				return err
			}
			info, err := device.At(a.Cfg.StateDir()).Load()
			if err != nil {
				return err
			}
			key, err := a.Keys.Load(info.VaultID)
			if err != nil {
				return errcode.Wrap(errcode.NoVault, "the vault key is not available", err)
			}
			b, err := vault.CreateBundle(info.VaultID, key, pass)
			if err != nil {
				return errcode.Wrap(errcode.Usage, err.Error(), err)
			}
			return os.WriteFile(out, b, 0600)
		}}
	inv.Flags().String("out", "", "bundle file to write")
	inv.Flags().String("passphrase-file", "", "file containing the bundle passphrase")
	join := &cobra.Command{Use: "join [bundle]", Short: "Join an existing vault (with a bundle and passphrase, or --from-cloud)", Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var id string
			var key []byte
			fromCloud, _ := cmd.Flags().GetBool("from-cloud")
			if fromCloud {
				if wy, _ := cmd.Flags().GetString("weiyun"); wy != "" {
					if err := config.Set("transport.weiyun", wy); err != nil {
						return errcode.Wrap(errcode.Usage, err.Error(), err)
					}
					a.Cfg.Transport.Weiyun = wy
				}
				var err error
				if id, key, err = a.fetchCloudKey(cmd.Context()); err != nil {
					return err
				}
			} else {
				if len(args) != 1 {
					return errcode.New(errcode.Usage, "give the invite bundle (with --passphrase-file), or use --from-cloud")
				}
				pass, err := readPassphrase(cmd)
				if err != nil {
					return err
				}
				b, err := os.ReadFile(args[0])
				if err != nil {
					return errcode.Wrap(errcode.Usage, "cannot read the bundle", err)
				}
				if id, key, err = vault.OpenBundle(b, pass); err != nil {
					return errcode.Wrap(errcode.AuthFailed, "wrong passphrase or damaged bundle", err)
				}
			}
			v, err := vault.New(id, key)
			if err != nil {
				return err
			}
			if name := a.Cfg.Transport.Weiyun; name != "" {
				m, err := a.weiyunMirror(name)
				if err != nil {
					return err
				}
				if err := m.Pull(cmd.Context()); err != nil {
					return errcode.Wrap(errcode.Usage, "cannot reach Weiyun: "+err.Error(), err)
				}
				if err := ensureVaultJSON(v, a.weiyunCache(), session.FormatTime(a.Now())); err != nil {
					return err
				}
			} else if folder := a.Cfg.Transport.Folder; folder != "" {
				if err := ensureVaultJSON(v, config.ExpandPath(folder), session.FormatTime(a.Now())); err != nil {
					return err
				}
			}
			root := a.Cfg.StateDir()
			if err := platform.EnsurePrivateDir(filepath.Join(root, "device")); err != nil {
				return err
			}
			if _, err := device.At(root).Load(); err == nil {
				return errcode.New(errcode.Conflict, "this device is already initialised")
			}
			if err := a.Keys.Store(id, key); err != nil {
				return errcode.Wrap(errcode.NoVault, "cannot store the vault key", err)
			}
			info, err := device.At(root).Init(id, session.FormatTime(a.Now()))
			if err != nil {
				return err
			}
			if a.JSON {
				return a.printJSON(map[string]string{"vault_id": id, "device_id": info.DeviceID})
			}
			if fromCloud {
				touch(a.keyMarker(markKeyHint)) // offer to remove the cloud key after the first sync
			}
			fmt.Fprintf(a.Out, "joined vault %s as %s\n", id, info.DeviceID)
			return nil
		}}
	join.Flags().String("passphrase-file", "", "file containing the bundle passphrase")
	join.Flags().Bool("from-cloud", false, "take the vault key from the cloud folder instead of an invite bundle")
	join.Flags().String("weiyun", "", "Weiyun directory to use with --from-cloud (token from WEIYUN_MCP_TOKEN)")
	regen := &cobra.Command{Use: "regenerate-id", Short: "Give this device a new id (after a restore or clone)", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			unlock, err := lockStore(a.Cfg.StateDir())
			if err != nil {
				return err
			}
			defer unlock()
			info, err := device.At(a.Cfg.StateDir()).Regenerate()
			if err != nil {
				return err
			}
			if a.JSON {
				return a.printJSON(map[string]string{"device_id": info.DeviceID})
			}
			fmt.Fprintln(a.Out, info.DeviceID)
			return nil
		}}
	c.AddCommand(inv, join, regen)
	return c
}

var _ = context.Background
