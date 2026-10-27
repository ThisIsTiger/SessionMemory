package main

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"session-memory/internal/agent"
	"session-memory/internal/errcode"
	"session-memory/internal/session"
)

// newAdoptCmd lets the computer that lost a conflict take the chosen version:
// its own copy of the thread is moved to a backup folder and the chosen
// revision is installed in its place.
func newAdoptCmd(a *App) *cobra.Command {
	c := &cobra.Command{Use: "adopt <session_id>", Short: "Replace this computer's copy of a session with the chosen version (yours is backed up)", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name, _ := cmd.Flags().GetString("agent")
			sid := args[0]
			ad, err := a.adapter(name)
			if err != nil {
				return err
			}
			r, err := a.openRT(cmd.Context(), true)
			if err != nil {
				return err
			}
			defer r.Close()
			if _, err := r.Sync.Reconcile(cmd.Context()); err != nil {
				return err
			}
			var bnd *session.Binding
			bs, _ := r.Store.Bindings(sid)
			for i := range bs {
				if bs[i].State == session.StateActive && bs[i].DeviceID == r.Info.DeviceID && bs[i].AgentProfile.Agent == name {
					bnd = &bs[i]
				}
			}
			if bnd == nil {
				return errcode.New(errcode.NotTracked, "this computer has no installed copy of the session; use restore or relay")
			}
			resolve := func() (session.Resolution, *session.Graph, error) {
				g, err := r.Store.Graph(sid)
				if err != nil {
					return session.Resolution{}, nil, err
				}
				ch, _ := r.Store.Choices(sid)
				return session.Resolve(g, ch), g, nil
			}
			res, g, err := resolve()
			if err != nil {
				return err
			}
			if res.Conflict || res.Effective == "" {
				return errcode.New(errcode.NeedsAction, "the session is still in conflict; run choose on one computer first")
			}
			switch session.LandingFor(g, bnd.InstalledRevisionID, res.Effective) {
			case session.LandUpToDate:
				fmt.Fprintln(a.Out, "already on the chosen version")
				return nil
			case session.LandFastForward:
				return errcode.New(errcode.Usage, "this copy can simply be updated; run relay instead")
			}
			closed, _ := cmd.Flags().GetBool("client-closed")
			rs, _, err := ad.Runtime().Running(cmd.Context(), bnd.NativeHome, bnd.NativeSessionID)
			if err != nil {
				return err
			}
			if rs == agent.Running || (rs == agent.Unknown && !closed) {
				return errcode.New(errcode.ClientRunning, "close the agent (or confirm with --client-closed) before replacing the session")
			}
			// keep any work done since the last sync as its own version, then re-check
			if _, changed, err := r.Eng.Checkpoint(cmd.Context(), ad, bnd.BindingID, "", session.CaptureAuto); err != nil {
				return err
			} else if changed {
				if _, err := r.Sync.Publish(cmd.Context()); err != nil {
					return err
				}
				if res, _, err = resolve(); err != nil {
					return err
				}
				if res.Conflict {
					return errcode.New(errcode.NeedsAction, "work done here since the last sync was saved as a new version; run choose again, then adopt")
				}
			}
			ns, found, err := ad.Locate(cmd.Context(), agent.NativeRef{Home: bnd.NativeHome, NativeID: bnd.NativeSessionID})
			if err != nil {
				return err
			}
			backup := filepath.Join(a.Cfg.StateDir(), "abandoned", sid, time.Now().UTC().Format("20060102T150405Z"))
			type moved struct{ from, to string }
			var done []moved
			undo := func() {
				for i := len(done) - 1; i >= 0; i-- {
					os.Rename(done[i].to, done[i].from)
				}
			}
			if found {
				for _, f := range ns.Files {
					from := filepath.Join(bnd.NativeHome, filepath.FromSlash(f.Path))
					to := filepath.Join(backup, filepath.FromSlash(f.Path))
					if err := os.MkdirAll(filepath.Dir(to), 0700); err != nil {
						undo()
						return err
					}
					if err := os.Rename(from, to); err != nil {
						undo()
						return err
					}
					done = append(done, moved{from, to})
				}
			}
			if proj := projectOfSession(r, sid); proj != "" {
				if _, err := loadProject(a.Cfg.StateDir(), proj); err == nil {
					cmd.Flags().Set("project", proj)
				}
			}
			if strict, _ := cmd.Flags().GetBool("strict"); !strict {
				cmd.Flags().Set("allow-code-mismatch", "true")
			}
			opt, err := installOptions(a, cmd, name)
			if err != nil {
				undo()
				return err
			}
			if _, _, err := r.Eng.Restore(cmd.Context(), ad, sid, opt); err != nil {
				undo()
				return err
			}
			if name == "codex" {
				registerWithCodex(cmd.Context(), a, r, opt.Home, []string{sid})
			}
			fmt.Fprintf(a.Out, "now on the chosen version; your previous copy is in %s\n", backup)
			return nil
		}}
	c.Flags().String("agent", "codex", "agent name")
	c.Flags().Bool("strict", false, "refuse when the recorded code state differs from this computer's")
	addInstallFlags(c)
	return c
}

func projectOfSession(r *rt, sid string) string {
	var ref string
	r.Store.DB().QueryRow(`SELECT COALESCE(project_ref,'') FROM sessions WHERE session_id = ?`, sid).Scan(&ref)
	return ref
}
