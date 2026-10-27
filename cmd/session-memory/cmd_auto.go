package main

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"session-memory/internal/agent"
	"session-memory/internal/config"
	"session-memory/internal/engine"
	"session-memory/internal/errcode"
	"session-memory/internal/session"
	"session-memory/internal/source/codex"
)

func newAutoInstallCmd(a *App) *cobra.Command {
	c := &cobra.Command{Use: "auto-install", Short: "Per-agent background install while the client is not running"}
	for _, v := range []struct{ use, val string }{{"enable", "true"}, {"disable", "false"}} {
		val := v.val
		c.AddCommand(&cobra.Command{Use: v.use, Short: v.use + " auto-install for an agent", Args: cobra.NoArgs,
			RunE: func(cmd *cobra.Command, _ []string) error {
				name, _ := cmd.Flags().GetString("agent")
				if name == "" {
					return errcode.New(errcode.Usage, "--agent is required")
				}
				if err := config.Set("agents."+name+".auto_install", val); err != nil {
					return errcode.Wrap(errcode.Usage, err.Error(), err)
				}
				return nil
			}})
	}
	c.PersistentFlags().String("agent", "", "agent name")
	return c
}

// autoInstall tries to bring every installed binding of an auto-install agent
// up to the effective head. It only acts on fast-forwards and only when the
// client is positively known not to be running (RunState NotRunning); anything
// else is counted as waiting (spec §6.4).
func autoInstall(ctx context.Context, a *App, r *rt) (installed, waiting int) {
	bs, _ := r.Store.Bindings("")
	for _, b := range bs {
		if b.State != session.StateActive || b.DeviceID != r.Info.DeviceID || b.InstallMode != session.InstallNative {
			continue
		}
		var ac config.AgentConfig
		switch b.AgentProfile.Agent {
		case "codex":
			ac = a.Cfg.Agents.Codex
		default:
			continue
		}
		if !ac.IsAutoInstall() {
			continue
		}
		g, err := r.Store.Graph(b.SessionID)
		if err != nil {
			continue
		}
		ch, _ := r.Store.Choices(b.SessionID)
		res := session.Resolve(g, ch)
		if res.Conflict || res.Effective == "" || session.LandingFor(g, b.InstalledRevisionID, res.Effective) != session.LandFastForward || res.Effective == b.InstalledRevisionID {
			continue
		}
		cx := codex.New(a.Cfg)
		st, _, _ := cx.Runtime().Running(ctx, b.NativeHome, b.NativeSessionID)
		if st != agent.NotRunning {
			waiting++
			continue
		}
		if _, _, err := r.Eng.Restore(ctx, cx, b.SessionID, engine.RestoreOptions{Home: b.NativeHome, ClientClosed: true}); err == nil {
			installed++
			registerWithCodex(ctx, a, r, b.NativeHome, []string{b.SessionID})
		} else if errcode.CodeOf(err) == errcode.ClientRunning {
			waiting++
		}
	}
	return
}

func newOpenCmd(a *App) *cobra.Command {
	c := &cobra.Command{Use: "open <session_id>", Short: "Pull, then show how to open the session in this device's agent", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			r, err := a.openRT(cmd.Context(), true)
			if err != nil {
				return err
			}
			defer r.Close()
			if _, err := r.Sync.Reconcile(cmd.Context()); err != nil {
				return err
			}
			rows, err := buildRows(r, args[0])
			if err != nil || len(rows) == 0 {
				return errcode.New(errcode.NotTracked, "session is not tracked on this device; run restore")
			}
			fmt.Fprintf(a.Out, "session %s is up to date (heads=%d conflict=%v)\nclose the client, then run: session-memory restore %s --client-closed\n",
				args[0], rows[0].HeadCount, rows[0].Conflict, args[0])
			return nil
		}}
	return c
}
