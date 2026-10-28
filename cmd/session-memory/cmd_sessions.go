package main

import (
	"context"
	"fmt"
	"sort"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"session-memory/internal/agent"
	"session-memory/internal/errcode"
	"session-memory/internal/session"
)

func newDiscoverCmd(a *App) *cobra.Command {
	c := &cobra.Command{Use: "discover", Short: "List native sessions that can be tracked", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			name, _ := cmd.Flags().GetString("agent")
			ad, err := a.adapter(name)
			if err != nil {
				return err
			}
			list, err := ad.Discover(cmd.Context(), agent.DiscoverOptions{})
			if err != nil {
				return err
			}
			if a.JSON {
				return a.printJSON(list)
			}
			w := tabwriter.NewWriter(a.Out, 0, 4, 2, ' ', 0)
			for _, s := range list {
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", s.Ref.NativeID, s.Profile.Surface, s.UpdatedAt, s.Title)
			}
			return w.Flush()
		}}
	c.Flags().String("agent", "codex", "agent name")
	return c
}

func newTrackCmd(a *App) *cobra.Command {
	c := &cobra.Command{Use: "track", Short: "Put a native session under SessionMemory", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			name, _ := cmd.Flags().GetString("agent")
			native, _ := cmd.Flags().GetString("native-session")
			home, _ := cmd.Flags().GetString("home-dir")
			project, _ := cmd.Flags().GetString("project")
			if native == "" {
				return errcode.New(errcode.Usage, "--native-session is required")
			}
			ad, err := a.adapter(name)
			if err != nil {
				return err
			}
			r, err := a.openRT(cmd.Context(), false)
			if err != nil {
				return err
			}
			defer r.Close()
			ref, err := resolveRef(cmd, ad, native, home)
			if err != nil {
				return err
			}
			b, rev, err := r.Eng.Track(cmd.Context(), ad, ref, project)
			if err != nil {
				return err
			}
			if a.JSON {
				return a.printJSON(map[string]string{"session_id": b.SessionID, "binding_id": b.BindingID, "revision_id": rev.RevisionID})
			}
			fmt.Fprintf(a.Out, "session %s\nrevision %s\n", b.SessionID, rev.RevisionID)
			return nil
		}}
	c.Flags().String("agent", "codex", "agent name")
	c.Flags().String("native-session", "", "native session id")
	c.Flags().String("home-dir", "", "agent home containing the session (default: search all homes)")
	c.Flags().String("project", "", "project_ref label")
	return c
}

func resolveRef(cmd *cobra.Command, ad agent.Adapter, native, home string) (agent.NativeRef, error) {
	list, err := ad.Discover(cmd.Context(), agent.DiscoverOptions{NativeIDs: []string{native}})
	if err != nil {
		return agent.NativeRef{}, err
	}
	for _, s := range list {
		if home == "" || s.Ref.Home == home {
			return s.Ref, nil
		}
	}
	return agent.NativeRef{}, errcode.New(errcode.NotFound, "native session not found")
}

type listRow struct {
	SessionID  string `json:"session_id"`
	BindingID  string `json:"binding_id"`
	Agent      string `json:"agent"`
	NativeID   string `json:"native_session_id"`
	State      string `json:"state"`
	Mode       string `json:"install_mode"`
	HeadCount  int    `json:"heads"`
	Conflict   bool   `json:"conflict"`
	Unpub      int    `json:"unpublished"`
	Unsafe     bool   `json:"unsafe"`
	Revisions  int    `json:"revisions"`
	LastUpdate string `json:"updated_at"`
}

func newListCmd(a *App) *cobra.Command {
	return &cobra.Command{Use: "list", Short: "List tracked sessions", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			r, err := a.openRT(cmd.Context(), false)
			if err != nil {
				return err
			}
			defer r.Close()
			rows, err := buildRows(r, "")
			if err != nil {
				return err
			}
			if a.JSON {
				return a.printJSON(rows)
			}
			w := tabwriter.NewWriter(a.Out, 0, 4, 2, ' ', 0)
			for _, x := range rows {
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\theads=%d\tconflict=%v\tunpublished=%d\n", x.SessionID, x.Agent, x.State, x.Mode, x.HeadCount, x.Conflict, x.Unpub)
			}
			return w.Flush()
		}}
}

func buildRows(r *rt, only string) ([]listRow, error) {
	bs, err := r.Store.Bindings(only)
	if err != nil {
		return nil, err
	}
	unpub, _ := r.Store.UnpublishedRevisions(r.Info.DeviceID)
	unpubBy := map[string]int{}
	for _, u := range unpub {
		unpubBy[u.SessionID]++
	}
	rows := []listRow{}
	bound := map[string]bool{}
	for _, b := range bs {
		bound[b.SessionID] = true
		g, err := r.Store.Graph(b.SessionID)
		if err != nil {
			return nil, err
		}
		ch, _ := r.Store.Choices(b.SessionID)
		res := session.Resolve(g, ch)
		revs, _ := r.Store.Revisions(b.SessionID)
		rows = append(rows, listRow{SessionID: b.SessionID, BindingID: b.BindingID, Agent: b.AgentProfile.Agent, NativeID: b.NativeSessionID,
			State: b.State, Mode: b.InstallMode, HeadCount: len(res.Heads), Conflict: res.Conflict, Unpub: unpubBy[b.SessionID],
			Unsafe: b.Unsafe != nil, Revisions: len(revs), LastUpdate: b.UpdatedAt})
	}
	ids, err := r.Store.SessionIDs()
	if err != nil {
		return nil, err
	}
	for _, id := range ids {
		if bound[id] || (only != "" && only != id) {
			continue
		}
		revs, err := r.Store.Revisions(id)
		if err != nil {
			return nil, err
		}
		if len(revs) == 0 {
			continue
		}
		ch, err := r.Store.Choices(id)
		if err != nil {
			return nil, err
		}
		res := session.Resolve(session.NewGraph(revs), ch)
		last := revs[len(revs)-1]
		rows = append(rows, listRow{SessionID: id, Agent: last.AgentProfile.Agent, NativeID: revs[0].OriginNativeID, State: "received", HeadCount: len(res.Heads), Conflict: res.Conflict, Unpub: unpubBy[id], Revisions: len(revs), LastUpdate: last.CreatedAt})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].LastUpdate > rows[j].LastUpdate })
	return rows, nil
}

func newStatusCmd(a *App) *cobra.Command {
	return &cobra.Command{Use: "status [session_id]", Short: "Show session status", Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			r, err := a.openRT(cmd.Context(), false)
			if err != nil {
				return err
			}
			defer r.Close()
			only := ""
			if len(args) == 1 {
				only = args[0]
			}
			rows, err := buildRows(r, only)
			if err != nil {
				return err
			}
			if only != "" && len(rows) == 0 {
				return errcode.New(errcode.NotTracked, "session is not tracked on this device")
			}
			if a.JSON {
				return a.printJSON(rows)
			}
			for _, x := range rows {
				fmt.Fprintf(a.Out, "%s  %s  revisions=%d heads=%d unpublished=%d conflict=%v unsafe=%v\n", x.SessionID, x.State, x.Revisions, x.HeadCount, x.Unpub, x.Conflict, x.Unsafe)
			}
			return nil
		}}
}

func newCheckpointCmd(a *App) *cobra.Command {
	c := &cobra.Command{Use: "checkpoint <session_id>", Short: "Capture now and publish", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			r, err := a.openRT(cmd.Context(), false)
			if err != nil {
				return err
			}
			defer r.Close()
			bs, _ := r.Store.Bindings(args[0])
			name, _ := cmd.Flags().GetString("agent")
			var active *session.Binding
			for i := range bs {
				if bs[i].State == session.StateActive && bs[i].DeviceID == r.Info.DeviceID && (name == "" || bs[i].AgentProfile.Agent == name) {
					if active != nil {
						return errcode.New(errcode.Usage, "multiple active agent bindings; select --agent or use sync to capture all")
					}
					active = &bs[i]
				}
			}
			if active == nil {
				return errcode.New(errcode.NotTracked, "no active binding for this session on this device")
			}
			ad, err := a.adapter(active.AgentProfile.Agent)
			if err != nil {
				return err
			}
			rev, changed, err := r.Eng.Checkpoint(cmd.Context(), ad, active.BindingID, "", session.CaptureCheckpoint)
			if err != nil {
				return err
			}
			published := 0
			if r.Sync != nil {
				published, err = r.Sync.Publish(cmd.Context())
				if err != nil {
					return err
				}
			}
			if a.JSON {
				return a.printJSON(map[string]any{"changed": changed, "revision_id": rev.RevisionID, "published": published})
			}
			fmt.Fprintf(a.Out, "changed=%v revision=%s published=%d\n", changed, rev.RevisionID, published)
			return nil
		}}
	c.Flags().String("agent", "", "capture this agent when the session has multiple active bindings")
	return c
}

func newSyncCmd(a *App) *cobra.Command {
	c := &cobra.Command{Use: "sync", Short: "Capture, publish and receive", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			watch, _ := cmd.Flags().GetBool("watch")
			r, err := a.openRT(cmd.Context(), true)
			if err != nil {
				return err
			}
			defer r.Close()
			once := func() (map[string]int, error) {
				out, err := syncOnce(cmd.Context(), a, r)
				if err != nil {
					return out, err
				}
				if auto, _ := cmd.Flags().GetBool("auto-install"); auto {
					out["installed"], out["waiting_client_close"] = autoInstall(cmd.Context(), a, r)
				}
				return out, nil
			}
			for {
				out, err := once()
				if err != nil {
					return err
				}
				if a.JSON {
					a.printJSON(out)
				} else {
					fmt.Fprintf(a.Out, "auto_tracked=%d captured=%d published=%d received=%d/%d pending_objects=%d quarantined=%d installed=%d waiting_client_close=%d\n", out["auto_tracked"], out["captured"], out["published"],
						out["received_manifests"], out["received_revisions"], out["pending_objects"], out["quarantined"], out["installed"], out["waiting_client_close"])
				}
				if !watch {
					if err := captureFailure(out); err != nil {
						return err
					}
					offerKeyRemoval(cmd.Context(), a)
					return nil
				}
				select {
				case <-cmd.Context().Done():
					return nil
				case <-time.After(60 * time.Second):
				}
			}
		}}
	c.Flags().Bool("watch", false, "keep syncing every 60 seconds")
	c.Flags().Bool("auto-install", false, "install fast-forward updates for agents with auto_install enabled")
	return c
}

// syncOnce is one round of: take on new sessions of watched projects, capture
// local changes, publish them, receive what other devices published.
func syncOnce(ctx context.Context, a *App, r *rt) (map[string]int, error) {
	out := map[string]int{}
	if r.Multi != nil {
		r.Multi.CatchUp(ctx) // an account that missed writes is brought level; failures are reported by the mirror
	}
	out["auto_tracked"] = autoTrackProjects(ctx, a, r)
	bs, _ := r.Store.Bindings("")
	for _, b := range bs {
		if b.State != session.StateActive || b.DeviceID != r.Info.DeviceID || (b.InstallMode != session.InstallNative && b.InstallMode != session.InstallConverted) {
			continue
		}
		ad, err := a.adapter(b.AgentProfile.Agent)
		if err != nil {
			continue
		}
		if _, changed, err := r.Eng.Checkpoint(ctx, ad, b.BindingID, "", session.CaptureAuto); err != nil {
			// keep going with the other sessions, but never report the round as clean
			out["capture_failed"]++
			fmt.Fprintf(a.Err, "warning: could not capture session %s: %v\n", b.SessionID, err)
		} else if changed {
			out["captured"]++
		}
	}
	n, err := r.Sync.Publish(ctx)
	if err != nil {
		return out, err
	}
	out["published"] = n
	res, err := r.Sync.Reconcile(ctx)
	if err != nil {
		return out, err
	}
	out["received_manifests"], out["received_revisions"] = res.Applied, res.Revisions
	out["pending_objects"], out["quarantined"] = res.PendingObjs, res.Quarantined
	flushCodexAssign(a, false)
	return out, nil
}

func newRevisionsCmd(a *App) *cobra.Command {
	return &cobra.Command{Use: "revisions <session_id>", Short: "Show the revision graph", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			r, err := a.openRT(cmd.Context(), false)
			if err != nil {
				return err
			}
			defer r.Close()
			revs, _ := r.Store.Revisions(args[0])
			if len(revs) == 0 {
				return errcode.New(errcode.NotFound, "no revisions for this session")
			}
			g := session.NewGraph(revs)
			ch, _ := r.Store.Choices(args[0])
			res := session.Resolve(g, ch)
			if a.JSON {
				return a.printJSON(map[string]any{"revisions": revs, "heads": res.Heads, "conflict": res.Conflict, "effective": res.Effective})
			}
			for _, rv := range revs {
				fmt.Fprintf(a.Out, "%s  parent=%.16s  %s  %s\n", rv.RevisionID[:16], rv.ParentRevisionID, rv.CreatedAt, rv.AgentProfile.Agent)
			}
			fmt.Fprintf(a.Out, "heads=%d conflict=%v\n", len(res.Heads), res.Conflict)
			return nil
		}}
}

func newChooseCmd(a *App) *cobra.Command {
	c := &cobra.Command{Use: "choose <session_id>", Short: "Choose which head to continue after a conflict", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			rev, _ := cmd.Flags().GetString("revision")
			r, err := a.openRT(cmd.Context(), false)
			if err != nil {
				return err
			}
			defer r.Close()
			g, err := r.Store.Graph(args[0])
			if err != nil {
				return err
			}
			heads := g.Heads()
			found := false
			for _, h := range heads {
				found = found || h == rev
			}
			if !found {
				return errcode.New(errcode.Usage, "--revision must be one of the current heads").With("heads", len(heads))
			}
			c := session.Choice{SessionID: args[0], SeenHeads: heads, Chosen: rev, DeviceID: r.Info.DeviceID, At: session.FormatTime(a.Now())}
			e := session.Entry{Type: session.EntryChoice, Choice: &c}
			if err := e.Validate(); err != nil {
				return errcode.Wrap(errcode.Usage, err.Error(), err)
			}
			if err := r.Sync2().AppendOutbox(e); err != nil {
				return err
			}
			// apply locally right away so the next list reflects it
			seen, _ := jsonMarshal(c.SeenHeads)
			r.Store.DB().Exec(`INSERT OR IGNORE INTO choices(entry_key, session_id, chosen, seen_heads, device_id, at) VALUES(?,?,?,?,?,?)`,
				e.Key(), c.SessionID, c.Chosen, string(seen), c.DeviceID, c.At)
			if r.Sync != nil {
				if _, err := r.Sync.Publish(cmd.Context()); err != nil {
					return err
				}
			}
			return nil
		}}
	c.Flags().String("revision", "", "revision id of the head to keep")
	return c
}

// captureFailure turns sessions that could not be captured in a sync round
// into a non-success result.
func captureFailure(out map[string]int) error {
	if n := out["capture_failed"]; n > 0 {
		return errcode.New(errcode.RecoveryNeeded, fmt.Sprintf("%d session(s) could not be captured; see the warnings above", n))
	}
	return nil
}
