package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"session-memory/internal/agent"
	"session-memory/internal/engine"
	"session-memory/internal/errcode"
	"session-memory/internal/session"
)

// relayAgents are the agents relay brings up to date. Named explicitly, only
// that one. Otherwise Codex, plus WorkBuddy once the user has opted in to its
// registry writer (agents.workbuddy.allow_registry_write): writing into
// WorkBuddy stays off until asked for.
func relayAgents(a *App, explicit string) []string {
	if explicit != "" {
		return []string{explicit}
	}
	var out []string
	if a.Cfg.Agents.Codex.IsEnabled() {
		out = append(out, "codex")
	}
	if a.Cfg.Agents.WorkBuddy.IsEnabled() && a.Cfg.Agents.WorkBuddy.AllowRegistryWrite {
		out = append(out, "workbuddy")
	}
	return out
}

// relayClientState lets a run without --client-closed install into WorkBuddy
// when, and only when, the process check says the app is not running. The
// WorkBuddy writer insists on that statement and checks the processes again
// before it writes, so this never overrides a running app.
// The returned note is set when the check itself failed (for example the
// process list cannot be read), which looks like "app running" but is not.
func relayClientState(ctx context.Context, ad agent.Adapter, name string, opt engine.RestoreOptions) (engine.RestoreOptions, string) {
	if name != "workbuddy" || opt.ClientClosed {
		return opt, ""
	}
	st, _, err := ad.Runtime().Running(ctx, opt.Home, "")
	switch {
	case err != nil:
		return opt, "cannot tell whether WorkBuddy is running (" + err.Error() + "); not installing"
	case st == agent.NotRunning:
		opt.ClientClosed = true
	case st == agent.Unknown:
		return opt, "cannot tell whether WorkBuddy is running; not installing"
	}
	return opt, ""
}

// mapTaskProjects gives a project that arrived from another computer its folder
// here when that folder is one the agent makes by itself (WorkBuddy tasks), so
// handing a task over needs no command on the receiving side.
func mapTaskProjects(ctx context.Context, a *App, r *rt, agents []string) error {
	mapped := map[string]bool{}
	for _, pm := range loadProjects(a.Cfg.StateDir()) {
		mapped[pm.ProjectRef] = true
	}
	q, err := r.Store.DB().Query(`SELECT DISTINCT project_ref FROM sessions WHERE project_ref IS NOT NULL ORDER BY project_ref`)
	if err != nil {
		return err
	}
	var refs []string
	for q.Next() {
		var ref string
		if err := q.Scan(&ref); err != nil {
			q.Close()
			return err
		}
		if !mapped[ref] {
			refs = append(refs, ref)
		}
	}
	q.Close()
	for _, ref := range refs {
		for _, name := range agents {
			dir, note := autoProjectDirWith(ctx, a, r, ref, name)
			if dir == "" || !strings.HasPrefix(note, taskFolderNote) {
				continue
			}
			if err := os.MkdirAll(dir, 0755); err != nil {
				return err
			}
			if err := saveProject(a, session.ProjectMap{SchemaVersion: 1, ProjectRef: ref, LocalPath: dir, Watch: []string{name}}); err != nil {
				return err
			}
			break
		}
	}
	return nil
}

// newRelayCmd is the one command a person needs day to day: run it when
// leaving a computer and again when arriving at another.
func newRelayCmd(a *App) *cobra.Command {
	c := &cobra.Command{Use: "relay", Short: "One step to hand work over: upload local progress, download the other computer's, update Codex (and WorkBuddy once enabled)", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			quiet, _ := cmd.Flags().GetBool("quiet")
			if quiet { // background runs hold back; one started by hand never does
				a.quietRun = true
				if a.allAccountsPaused() || a.idleSkip() {
					return nil
				}
			}
			explicit, _ := cmd.Flags().GetString("agent")
			strict, _ := cmd.Flags().GetBool("strict")
			type target struct {
				name string
				ad   agent.Adapter
			}
			var targets []target
			for _, n := range relayAgents(a, explicit) {
				ad, err := a.adapter(n)
				if err != nil {
					if explicit != "" {
						return err
					}
					continue
				}
				targets = append(targets, target{n, ad})
			}
			r, err := a.openRT(cmd.Context(), true)
			if err != nil {
				return err
			}
			defer r.Close()
			sync, err := syncOnce(cmd.Context(), a, r)
			if err != nil {
				return err
			}
			if err := captureFailure(sync); err != nil {
				return err // handing over work that was not saved would lose it
			}
			if !strict {
				// the history is what is being handed over; a differing code state is only noted in the receipt
				cmd.Flags().Set("allow-code-mismatch", "true")
			}
			var names []string
			for _, t := range targets {
				names = append(names, t.name)
			}
			if err := mapTaskProjects(cmd.Context(), a, r, names); err != nil {
				return err
			}
			type projOut struct {
				Ref      string   `json:"project_ref"`
				Label    string   `json:"label,omitempty"`
				Path     string   `json:"local_path"`
				Updated  int      `json:"updated"`
				UpToDate int      `json:"up_to_date"`
				Waiting  int      `json:"waiting_for_codex_to_close"`
				Failed   int      `json:"needs_attention"`
				Problems []string `json:"problems,omitempty"`
			}
			var projects []projOut
			mapped := map[string]bool{}
			failed, waiting := 0, 0
			for _, pm := range loadProjects(a.Cfg.StateDir()) {
				mapped[pm.ProjectRef] = true
				if st, err := os.Stat(pm.LocalPath); err != nil || !st.IsDir() {
					continue
				}
				if err := cmd.Flags().Set("project", pm.ProjectRef); err != nil {
					return err
				}
				label := pm.Label
				if label == "" {
					label = filepath.Base(pm.LocalPath)
				}
				po := projOut{Ref: pm.ProjectRef, Label: label, Path: pm.LocalPath}
				for _, t := range targets {
					opt, err := installOptions(a, cmd, t.name)
					if err != nil {
						if explicit != "" {
							return err
						}
						continue
					}
					opt, note := relayClientState(cmd.Context(), t.ad, t.name, opt)
					if note != "" {
						po.Problems = append(po.Problems, note)
					}
					res, err := installProject(cmd.Context(), a, r, t.ad, t.name, pm.ProjectRef, opt)
					if err != nil {
						return err
					}
					po.Updated += res.Installed
					po.UpToDate += res.UpToDate
					po.Waiting += res.Waiting
					po.Failed += res.Failed
					po.Problems = append(po.Problems, res.Problems...)
				}
				projects = append(projects, po)
				failed += po.Failed
				waiting += po.Waiting
			}
			type unmappedOut struct {
				Ref      string `json:"project_ref"`
				Sessions int    `json:"sessions"`
			}
			var unmapped []unmappedOut
			q, err := r.Store.DB().Query(`SELECT project_ref, COUNT(*) FROM sessions WHERE project_ref IS NOT NULL GROUP BY project_ref ORDER BY project_ref`)
			if err != nil {
				return err
			}
			for q.Next() {
				var u unmappedOut
				if err := q.Scan(&u.Ref, &u.Sessions); err != nil {
					q.Close()
					return err
				}
				if !mapped[u.Ref] {
					unmapped = append(unmapped, u)
				}
			}
			q.Close()
			if quiet {
				active := sync["published"] > 0 || sync["received_revisions"] > 0 || sync["captured"] > 0 || waiting > 0
				for _, p := range projects {
					active = active || p.Updated > 0
				}
				a.noteRound(active)
			}
			if quiet && !a.JSON {
				noteworthy := sync["published"] > 0 || sync["received_revisions"] > 0 || sync["quarantined"] > 0 || len(unmapped) > 0 || failed > 0 || waiting > 0
				for _, p := range projects {
					noteworthy = noteworthy || p.Updated > 0
				}
				if !noteworthy {
					return nil
				}
				fmt.Fprintf(a.Out, "[%s] ", a.Now().Format("2006-01-02 15:04:05"))
			}
			if a.JSON {
				if err := a.printJSON(map[string]any{"sync": sync, "projects": projects, "unmapped": unmapped}); err != nil {
					return err
				}
			} else {
				fmt.Fprintf(a.Out, "uploaded %d item(s), received %d version(s)\n", sync["published"], sync["received_revisions"])
				for _, p := range projects {
					fmt.Fprintf(a.Out, "project %s: updated %d, up to date %d", p.Label, p.Updated, p.UpToDate)
					if p.Waiting > 0 {
						fmt.Fprintf(a.Out, ", %d waiting for the app to close", p.Waiting)
					}
					if p.Failed > 0 {
						fmt.Fprintf(a.Out, ", %d need attention", p.Failed)
					}
					fmt.Fprintln(a.Out)
					for _, pr := range p.Problems {
						fmt.Fprintf(a.Err, "  %s\n", pr)
					}
				}
				for _, u := range unmapped {
					fmt.Fprintf(a.Out, "project %s (%d session(s)) has no folder on this computer; run once: session-memory project restore %s --cwd <folder> --client-closed\n", u.Ref, u.Sessions, u.Ref)
				}
				if sync["quarantined"] > 0 {
					fmt.Fprintf(a.Out, "%d received item(s) were set aside as invalid\n", sync["quarantined"])
				}
				if waiting > 0 {
					fmt.Fprintln(a.Out, "close the app (Codex, or WorkBuddy completely) and run relay again to finish updating")
				}
			}
			if failed > 0 {
				return errcode.New(errcode.RecoveryNeeded, fmt.Sprintf("%d session(s) need attention (conflict or divergence); see the messages above", failed))
			}
			if !quiet { // a background run has nobody to ask
				offerKeyRemoval(cmd.Context(), a)
			}
			return nil
		}}
	c.Flags().String("agent", "", "install into this agent only (default: Codex, and WorkBuddy once agents.workbuddy.allow_registry_write is on)")
	c.Flags().Bool("quiet", false, "print only when something happened (for background runs)")
	c.Flags().Bool("strict", false, "refuse sessions whose recorded code state differs from this computer's")
	addInstallFlags(c)
	return c
}
