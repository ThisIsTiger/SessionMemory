package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"session-memory/internal/errcode"
)

// newRelayCmd is the one command a person needs day to day: run it when
// leaving a computer and again when arriving at another.
func newRelayCmd(a *App) *cobra.Command {
	c := &cobra.Command{Use: "relay", Short: "One step to hand work over: upload local progress, download the other computer's, update Codex", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			name, _ := cmd.Flags().GetString("agent")
			strict, _ := cmd.Flags().GetBool("strict")
			ad, err := a.adapter(name)
			if err != nil {
				return err
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
				opt, err := installOptions(a, cmd, name)
				if err != nil {
					return err
				}
				res, err := installProject(cmd.Context(), a, r, ad, name, pm.ProjectRef, opt)
				if err != nil {
					return err
				}
				label := pm.Label
				if label == "" {
					label = filepath.Base(pm.LocalPath)
				}
				projects = append(projects, projOut{Ref: pm.ProjectRef, Label: label, Path: pm.LocalPath, Updated: res.Installed, UpToDate: res.UpToDate,
					Waiting: res.Waiting, Failed: res.Failed, Problems: res.Problems})
				failed += res.Failed
				waiting += res.Waiting
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
			quiet, _ := cmd.Flags().GetBool("quiet")
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
						fmt.Fprintf(a.Out, ", %d waiting for Codex to close", p.Waiting)
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
					fmt.Fprintln(a.Out, "close Codex (or the open session) and run relay again to finish updating")
				}
			}
			if failed > 0 {
				return errcode.New(errcode.RecoveryNeeded, fmt.Sprintf("%d session(s) need attention (conflict or divergence); see the messages above", failed))
			}
			return nil
		}}
	c.Flags().String("agent", "codex", "agent name")
	c.Flags().Bool("quiet", false, "print only when something happened (for background runs)")
	c.Flags().Bool("strict", false, "refuse sessions whose recorded code state differs from this computer's")
	addInstallFlags(c)
	return c
}
