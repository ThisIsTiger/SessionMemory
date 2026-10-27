package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"session-memory/internal/platform"
	"session-memory/internal/session"
	"session-memory/internal/source/codex"
)

// Threads installed into Codex wait here until the desktop app is closed and
// one of its projects covers their directory; then they are filed under it
// (agents.codex.assign_projects). A thread that stays unplaced is dropped
// after assignGiveUp so a folder the user never adds does not wait forever.
const assignGiveUp = 14 * 24 * time.Hour

type pendingAssign struct {
	Home  string `json:"home"`
	ID    string `json:"id"`
	Since string `json:"since"`
}

func pendingAssignPath(a *App) string {
	return filepath.Join(a.Cfg.StateDir(), "codex-assign-pending.json")
}

func loadPendingAssign(a *App) []pendingAssign {
	var ps []pendingAssign
	if b, err := os.ReadFile(pendingAssignPath(a)); err == nil {
		_ = json.Unmarshal(b, &ps)
	}
	return ps
}

func savePendingAssign(a *App, ps []pendingAssign) {
	if len(ps) == 0 {
		os.Remove(pendingAssignPath(a))
		return
	}
	b, _ := json.MarshalIndent(ps, "", "  ")
	_ = platform.AtomicWrite(pendingAssignPath(a), append(b, '\n'), 0600)
}

// queueCodexAssign records freshly installed threads and tries to place them.
func queueCodexAssign(a *App, home string, ids []string) {
	if !a.Cfg.Agents.Codex.IsAssignProjects() || len(ids) == 0 {
		return
	}
	ps := loadPendingAssign(a)
	have := map[string]bool{}
	for _, p := range ps {
		have[p.Home+"|"+p.ID] = true
	}
	now := time.Now().UTC().Format(time.RFC3339)
	for _, id := range ids {
		if !have[home+"|"+id] {
			ps = append(ps, pendingAssign{Home: home, ID: id, Since: now})
		}
	}
	savePendingAssign(a, ps)
	flushCodexAssign(a, true)
}

// flushCodexAssign places every waiting thread it can. verbose prints why the
// rest still wait; the background sync passes false so the log stays quiet.
func flushCodexAssign(a *App, verbose bool) {
	if !a.Cfg.Agents.Codex.IsAssignProjects() {
		return
	}
	ps := loadPendingAssign(a)
	if len(ps) == 0 {
		return
	}
	byHome := map[string][]string{}
	var homes []string
	for _, p := range ps {
		if _, ok := byHome[p.Home]; !ok {
			homes = append(homes, p.Home)
		}
		byHome[p.Home] = append(byHome[p.Home], p.ID)
	}
	outcome := map[string]codex.AssignResult{}
	for _, h := range homes {
		res, err := codex.AssignProjects(h, filepath.Join(a.Cfg.StateDir(), "backups"), byHome[h])
		if err != nil {
			if verbose {
				fmt.Fprintf(a.Err, "warning: could not file sessions under their Codex project (%v)\n", err)
			}
			continue
		}
		for _, r := range res {
			outcome[h+"|"+r.ID] = r
		}
	}
	var keep []pendingAssign
	placed, running, noProject, seen := 0, 0, 0, ""
	for _, p := range ps {
		r, ok := outcome[p.Home+"|"+p.ID]
		if !ok { // the home failed this round
			keep = append(keep, p)
			continue
		}
		switch r.Outcome {
		case codex.Assigned:
			placed++
		case codex.AppIsRunning:
			running++
			if running == 1 {
				seen = codex.AppSeen()
			}
			keep = append(keep, p)
		case codex.NoProject:
			noProject++
			if t, err := time.Parse(time.RFC3339, p.Since); err != nil || time.Since(t) < assignGiveUp {
				keep = append(keep, p)
			}
		}
	}
	savePendingAssign(a, keep)
	if placed > 0 { // rare and worth a log line even in the background
		fmt.Fprintf(a.Out, "filed %d session(s) under their Codex project\n", placed)
	}
	if !verbose {
		return
	}
	if running > 0 {
		fmt.Fprintf(a.Err, "note: Codex app is running (%s); %d session(s) will be filed under their Codex project the next time sync runs while it is closed\n", seen, running)
	}
	if noProject > 0 {
		fmt.Fprintf(a.Err, "note: %d session(s) stay under Recent: add their folder as a project in Codex once and they are filed automatically\n", noProject)
	}
}

// newFileProjectsCmd files the Codex sessions installed on this computer under
// their desktop-app project right away, including ones installed before the
// automatic step existed.
func newFileProjectsCmd(a *App) *cobra.Command {
	return &cobra.Command{Use: "file-projects", Short: "File sessions installed into Codex under their Codex project (Codex must be closed)", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !a.Cfg.Agents.Codex.IsAssignProjects() {
				fmt.Fprintln(a.Out, "agents.codex.assign_projects is off")
				return nil
			}
			r, err := a.openRT(cmd.Context(), false)
			if err != nil {
				return err
			}
			defer r.Close()
			bs, _ := r.Store.Bindings("")
			byHome := map[string][]string{}
			for _, b := range bs {
				if b.State == session.StateActive && b.DeviceID == r.Info.DeviceID && b.AgentProfile.Agent == "codex" && b.ImportReceipt != nil {
					byHome[b.NativeHome] = append(byHome[b.NativeHome], b.NativeSessionID)
				}
			}
			if len(byHome) == 0 {
				fmt.Fprintln(a.Out, "no installed Codex sessions on this computer")
				return nil
			}
			for h, ids := range byHome {
				queueCodexAssign(a, h, ids)
			}
			if n := len(loadPendingAssign(a)); n == 0 {
				fmt.Fprintln(a.Out, "nothing left to file")
			}
			return nil
		}}
}
