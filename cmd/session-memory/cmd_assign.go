package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"session-memory/internal/platform"
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
	placed, running, noProject := 0, 0, 0
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
			keep = append(keep, p)
		case codex.NoProject:
			noProject++
			if t, err := time.Parse(time.RFC3339, p.Since); err != nil || time.Since(t) < assignGiveUp {
				keep = append(keep, p)
			}
		}
	}
	savePendingAssign(a, keep)
	if !verbose {
		return
	}
	if placed > 0 {
		fmt.Fprintf(a.Out, "filed %d session(s) under their Codex project\n", placed)
	}
	if running > 0 {
		fmt.Fprintf(a.Err, "note: %d session(s) will be filed under their Codex project the next time sync runs while Codex is closed\n", running)
	}
	if noProject > 0 {
		fmt.Fprintf(a.Err, "note: %d session(s) stay under Recent: add their folder as a project in Codex once and they are filed automatically\n", noProject)
	}
}
