package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"session-memory/internal/agent"
	"session-memory/internal/engine"
	"session-memory/internal/errcode"
	"session-memory/internal/platform"
	"session-memory/internal/session"
	"session-memory/internal/source/codex"
)

func saveProject(a *App, pm session.ProjectMap) error {
	b, _ := json.MarshalIndent(pm, "", "  ")
	return platform.AtomicWrite(projectPath(a.Cfg.StateDir(), pm.ProjectRef), append(b, '\n'), 0600)
}

// loadProjects returns every project mapping on this device.
func loadProjects(root string) []session.ProjectMap {
	var out []session.ProjectMap
	files, _ := filepath.Glob(filepath.Join(root, "projects", "*.json"))
	for _, f := range files {
		var pm session.ProjectMap
		if b, err := os.ReadFile(f); err == nil && json.Unmarshal(b, &pm) == nil && session.ValidProjectRef(pm.ProjectRef) {
			out = append(out, pm)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ProjectRef < out[j].ProjectRef })
	return out
}

// pathWithin reports whether p is dir or lies beneath it (component boundary).
func pathWithin(p, dir string) bool {
	canon := func(s string) string {
		s = filepath.Clean(s)
		if r, err := filepath.EvalSymlinks(s); err == nil {
			return r
		}
		return s
	}
	p, dir = canon(p), canon(dir)
	return p == dir || strings.HasPrefix(p, strings.TrimSuffix(dir, string(filepath.Separator))+string(filepath.Separator))
}

// trackProject puts every not-yet-tracked native session of agent ad whose cwd
// is inside pm.LocalPath under SessionMemory, labelled with the project.
func trackProject(ctx context.Context, r *rt, ad agent.Adapter, pm session.ProjectMap) (tracked, already int, err error) {
	list, err := ad.Discover(ctx, agent.DiscoverOptions{})
	if err != nil {
		return 0, 0, err
	}
	for _, s := range list {
		if s.Cwd == "" || !pathWithin(s.Cwd, pm.LocalPath) {
			continue
		}
		if _, _, err := r.Eng.Track(ctx, ad, s.Ref, pm.ProjectRef); err != nil {
			if errcode.CodeOf(err) == errcode.Conflict {
				already++
				continue
			}
			return tracked, already, fmt.Errorf("track %s: %w", s.Ref.NativeID, err)
		}
		tracked++
	}
	return tracked, already, nil
}

// autoTrackProjects is the sync-time half of project track: sessions created
// later under a watched project directory join the project.
func autoTrackProjects(ctx context.Context, a *App, r *rt) int {
	n := 0
	for _, pm := range loadProjects(a.Cfg.StateDir()) {
		for _, name := range pm.Watch {
			ad, err := a.adapter(name)
			if err != nil {
				continue
			}
			if t, _, err := trackProject(ctx, r, ad, pm); err == nil {
				n += t
			}
		}
	}
	return n
}

func projectSessions(r *rt, ref string) ([]string, error) {
	rows, err := r.Store.DB().Query(`SELECT session_id FROM sessions WHERE project_ref = ? ORDER BY created_at`, ref)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func newProjectTrackCmd(a *App) *cobra.Command {
	c := &cobra.Command{Use: "track", Short: "Track a whole project directory: all its sessions now, new ones on every sync", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			name, _ := cmd.Flags().GetString("agent")
			cwd, _ := cmd.Flags().GetString("cwd")
			label, _ := cmd.Flags().GetString("label")
			ref, _ := cmd.Flags().GetString("project")
			if cwd == "" {
				return errcode.New(errcode.Usage, "--cwd is required")
			}
			abs, err := filepath.Abs(cwd)
			if err != nil {
				return err
			}
			if st, err := os.Stat(abs); err != nil || !st.IsDir() {
				return errcode.New(errcode.Usage, "--cwd must be an existing directory")
			}
			ad, err := a.adapter(name)
			if err != nil {
				return err
			}
			var pm session.ProjectMap
			for _, p := range loadProjects(a.Cfg.StateDir()) {
				if (ref != "" && p.ProjectRef == ref) || (ref == "" && p.LocalPath == abs) {
					pm = p
				}
			}
			if pm.ProjectRef == "" {
				if ref == "" {
					ref = session.NewProjectRef()
				} else if !session.ValidProjectRef(ref) {
					return errcode.New(errcode.Usage, "bad project ref")
				}
				pm = session.ProjectMap{SchemaVersion: 1, ProjectRef: ref}
			}
			pm.LocalPath = abs
			if label != "" {
				pm.Label = label
			} else if pm.Label == "" {
				pm.Label = filepath.Base(abs)
			}
			watched := false
			for _, w := range pm.Watch {
				watched = watched || w == name
			}
			if !watched {
				pm.Watch = append(pm.Watch, name)
			}
			if err := saveProject(a, pm); err != nil {
				return err
			}
			r, err := a.openRT(cmd.Context(), true)
			if err != nil {
				return err
			}
			defer r.Close()
			tracked, already, err := trackProject(cmd.Context(), r, ad, pm)
			if err != nil {
				return err
			}
			published := 0
			if r.Sync != nil {
				if published, err = r.Sync.Publish(cmd.Context()); err != nil {
					return err
				}
			}
			if a.JSON {
				return a.printJSON(map[string]any{"project_ref": pm.ProjectRef, "tracked": tracked, "already_tracked": already, "published": published})
			}
			fmt.Fprintf(a.Out, "project %s (%s)\ntracked=%d already_tracked=%d published=%d\nnew sessions under this directory are picked up by every sync\n", pm.ProjectRef, pm.Label, tracked, already, published)
			return nil
		}}
	c.Flags().String("agent", "codex", "agent name")
	c.Flags().String("cwd", "", "project directory on this machine")
	c.Flags().String("label", "", "display label (default: directory name)")
	c.Flags().String("project", "", "reuse an existing project_ref")
	return c
}

func newProjectListCmd(a *App) *cobra.Command {
	return &cobra.Command{Use: "list", Short: "List projects known to this device", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			r, err := a.openRT(cmd.Context(), false)
			if err != nil {
				return err
			}
			defer r.Close()
			type row struct {
				Ref      string `json:"project_ref"`
				Label    string `json:"label,omitempty"`
				Path     string `json:"local_path,omitempty"`
				Sessions int    `json:"sessions"`
			}
			byRef := map[string]*row{}
			q, err := r.Store.DB().Query(`SELECT project_ref, COUNT(*) FROM sessions WHERE project_ref IS NOT NULL GROUP BY project_ref`)
			if err != nil {
				return err
			}
			for q.Next() {
				x := &row{}
				if err := q.Scan(&x.Ref, &x.Sessions); err != nil {
					q.Close()
					return err
				}
				byRef[x.Ref] = x
			}
			q.Close()
			for _, pm := range loadProjects(a.Cfg.StateDir()) {
				x := byRef[pm.ProjectRef]
				if x == nil {
					x = &row{Ref: pm.ProjectRef}
					byRef[pm.ProjectRef] = x
				}
				x.Label, x.Path = pm.Label, pm.LocalPath
			}
			rows := []row{}
			for _, x := range byRef {
				rows = append(rows, *x)
			}
			sort.Slice(rows, func(i, j int) bool { return rows[i].Ref < rows[j].Ref })
			if a.JSON {
				return a.printJSON(rows)
			}
			w := tabwriter.NewWriter(a.Out, 0, 4, 2, ' ', 0)
			for _, x := range rows {
				path := x.Path
				if path == "" {
					path = "(not mapped on this device)"
				}
				fmt.Fprintf(w, "%s\t%s\tsessions=%d\t%s\n", x.Ref, x.Label, x.Sessions, path)
			}
			return w.Flush()
		}}
}

func newProjectRestoreCmd(a *App) *cobra.Command {
	c := &cobra.Command{Use: "restore <project_ref>", Short: "Install every session of a project into this device's agent", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name, _ := cmd.Flags().GetString("agent")
			ref := args[0]
			planOnly, _ := cmd.Flags().GetBool("plan")
			if !session.ValidProjectRef(ref) {
				return errcode.New(errcode.Usage, "bad project ref")
			}
			if name == "" { // not said: the agent the project's sessions came from
				var err error
				if name, err = projectAgent(cmd.Context(), a, ref); err != nil {
					return err
				}
			}
			cwd, _ := cmd.Flags().GetString("cwd")
			autoTask := false
			previewOnly := false // --plan on an unmapped project uses the folder and remembers nothing
			if _, err := loadProject(a.Cfg.StateDir(), ref); err != nil {
				if cwd == "" {
					found, note := autoProjectDir(cmd.Context(), a, ref, name)
					autoTask = found != "" && strings.HasPrefix(note, taskFolderNote)
					if found == "" {
						return errcode.New(errcode.NotFound, "project is not mapped on this device and its folder was not found automatically ("+note+"); pass --cwd <local project directory>")
					}
					fmt.Fprintf(a.Err, "note: using %s as this project's folder here (%s)\n", found, note)
					cwd = found
				}
				abs, err := filepath.Abs(cwd)
				if err != nil {
					return err
				}
				if autoTask { // a task folder the agent itself would make
					if err := os.MkdirAll(abs, 0755); err != nil {
						return err
					}
				}
				if st, err := os.Stat(abs); err != nil || !st.IsDir() {
					return errcode.New(errcode.Usage, "--cwd must be an existing directory")
				}
				if planOnly {
					previewOnly = true
					if err := cmd.Flags().Set("cwd", abs); err != nil {
						return err
					}
				} else if err := saveProject(a, session.ProjectMap{SchemaVersion: 1, ProjectRef: ref, LocalPath: abs}); err != nil {
					return err
				}
			}
			if !previewOnly {
				if err := cmd.Flags().Set("project", ref); err != nil {
					return err
				}
			}
			if pm, err := loadProject(a.Cfg.StateDir(), ref); err == nil && !planOnly {
				// sessions created later on this device under the mapped directory flow back
				watched := false
				for _, w := range pm.Watch {
					watched = watched || w == name
				}
				if !watched {
					pm.Watch = append(pm.Watch, name)
					if err := saveProject(a, pm); err != nil {
						return err
					}
				}
			}
			ad, err := a.adapter(name)
			if err != nil {
				return err
			}
			opt, err := installOptions(a, cmd, name)
			if err != nil {
				return err
			}
			r, err := a.openRT(cmd.Context(), true)
			if err != nil {
				return err
			}
			defer r.Close()
			if r.Sync != nil {
				if _, err := r.Sync.Reconcile(cmd.Context()); err != nil {
					return err
				}
			}
			res, err := installProject(cmd.Context(), a, r, ad, name, ref, opt)
			if err != nil {
				return err
			}
			installed, upToDate, skipped, failed := res.Installed, res.UpToDate, res.Skipped, res.Failed+res.Waiting
			if !a.JSON { // JSON mode prints one document and nothing else
				verb := "installed"
				if opt.PlanOnly {
					verb = "would install"
				}
				for _, d := range res.Done {
					fmt.Fprintf(a.Out, "%s %s\n", verb, d)
				}
			}
			for _, pr := range res.Problems {
				fmt.Fprintln(a.Err, pr)
			}
			if a.JSON {
				a.printJSON(map[string]int{"sessions": res.Sessions, "installed": installed, "up_to_date": upToDate, "skipped": skipped, "failed": failed})
			} else {
				verb := "installed"
				if opt.PlanOnly {
					verb = "would install"
				}
				fmt.Fprintf(a.Out, "project %s: sessions=%d %s=%d up_to_date=%d other_agent=%d failed=%d\n", ref, res.Sessions, verb, installed, upToDate, skipped, failed)
			}
			if failed > 0 {
				return errcode.New(errcode.RecoveryNeeded, fmt.Sprintf("%d session(s) failed to install", failed))
			}
			return nil
		}}
	c.Flags().String("agent", "", "agent name (default: the one the project's sessions came from)")
	addInstallFlags(c)
	return c
}

// projectAgent is the agent a project's sessions were recorded by, when that
// is one agent; with several, the caller has to say which.
func projectAgent(ctx context.Context, a *App, ref string) (string, error) {
	r, err := a.openRT(ctx, true)
	if err != nil {
		return "", err
	}
	defer r.Close()
	ids, err := projectSessions(r, ref)
	if err != nil {
		return "", err
	}
	seen := map[string]bool{}
	for _, id := range ids {
		if revs, _ := r.Store.Revisions(id); len(revs) > 0 {
			seen[revs[len(revs)-1].AgentProfile.Agent] = true
		}
	}
	if len(seen) == 0 {
		return "codex", nil // nothing to go by: what it always was
	}
	if len(seen) > 1 {
		return "", errcode.New(errcode.Usage, "cannot tell which agent to install for (the project has sessions of "+strconv.Itoa(len(seen))+" agents); pass --agent codex|workbuddy")
	}
	for n := range seen {
		return n, nil
	}
	return "", nil
}

// registerWithCodex has Codex itself register freshly installed threads, so the
// running desktop client lists them without the user restarting it first. A
// failure only costs that convenience and is reported, never fatal.
func registerWithCodex(ctx context.Context, a *App, r *rt, home string, sessionIDs []string) {
	var ids []string
	for _, sid := range sessionIDs {
		bs, _ := r.Store.Bindings(sid)
		for _, b := range bs {
			if b.State == session.StateActive && b.DeviceID == r.Info.DeviceID && b.AgentProfile.Agent == "codex" && b.NativeHome == home {
				ids = append(ids, b.NativeSessionID)
			}
		}
	}
	if len(ids) == 0 {
		return
	}
	defer queueCodexAssign(a, home, ids)
	found, err := codex.Register(ctx, home, os.Getenv("SM_CODEX_BIN"), ids)
	switch {
	case err != nil:
		fmt.Fprintf(a.Err, "warning: could not register with Codex (%v); the sessions appear after Codex rescans (restart it)\n", err)
	case len(found) < len(ids):
		fmt.Fprintf(a.Err, "warning: Codex listed %d of %d installed sessions; check the model provider in its config\n", len(found), len(ids))
	}
}

type projectResult struct {
	Sessions, Installed, UpToDate, Skipped, Failed, Waiting int
	Done                                                    []string // session ids installed now
	Problems                                                []string // "<session>: <error>" for sessions that failed or wait
}

// installProject brings every session of a project, in the target agent's
// format, up to its effective head on this device. Sessions already current
// count as up to date; a session Codex still has open counts as waiting.
func installProject(ctx context.Context, a *App, r *rt, ad agent.Adapter, name, ref string, opt engine.RestoreOptions) (projectResult, error) {
	var res projectResult
	ids, err := projectSessions(r, ref)
	if err != nil {
		return res, err
	}
	res.Sessions = len(ids)
	for _, id := range ids {
		revs, _ := r.Store.Revisions(id)
		if len(revs) == 0 || revs[len(revs)-1].AgentProfile.Agent != name {
			res.Skipped++
			continue
		}
		_, _, err := r.Eng.Restore(ctx, ad, id, opt)
		switch {
		case err == nil:
			res.Installed++
			res.Done = append(res.Done, id)
		case strings.Contains(err.Error(), "already installed"), strings.Contains(err.Error(), "already matches"):
			res.UpToDate++
		case errcode.CodeOf(err) == errcode.ClientRunning:
			res.Waiting++
			res.Problems = append(res.Problems, fmt.Sprintf("%s: %v", id, err))
		default:
			res.Failed++
			res.Problems = append(res.Problems, fmt.Sprintf("%s: %v", id, err))
		}
	}
	if res.Installed > 0 && !opt.PlanOnly && name == "codex" {
		registerWithCodex(ctx, a, r, opt.Home, res.Done)
	}
	return res, nil
}

// autoProjectDir finds this computer's folder for a project that has no
// mapping here: the folder named like the one its sessions were recorded in.
// The second result says what was matched, or why nothing was.
func autoProjectDir(ctx context.Context, a *App, ref, agentName string) (string, string) {
	r, err := a.openRT(ctx, true)
	if err != nil {
		return "", err.Error()
	}
	defer r.Close()
	if r.Sync != nil {
		if _, err := r.Sync.Reconcile(ctx); err != nil {
			return "", err.Error()
		}
	}
	return autoProjectDirWith(ctx, a, r, ref, agentName)
}

const (
	taskFolderNote = "the agent's own task folder "
	sameNameNote   = "same folder name as "
)

func autoProjectDirWith(ctx context.Context, a *App, r *rt, ref, agentName string) (string, string) {
	ad, err := a.adapter(agentName)
	if err != nil {
		return "", err.Error()
	}
	ids, err := projectSessions(r, ref)
	if err != nil || len(ids) == 0 {
		return "", "no sessions of this project here yet"
	}
	var hints []string
	if agentName == "codex" {
		hints = codex.AppProjectRoots(a.Cfg.CodexHome())
	}
	origin := projectOrigin(ctx, r, ad, ids)
	if origin == "" {
		return "", "the sessions do not record a folder"
	}
	if tw, ok := ad.(interface {
		TaskWorkspace(string) (string, bool)
	}); ok {
		if dir, ok := tw.TaskWorkspace(origin); ok {
			return dir, taskFolderNote + filepath.Base(origin)
		}
	}
	c := engine.LocalDirCandidates(origin, hints)
	switch {
	case len(c) == 1:
		return c[0], sameNameNote + filepath.Base(origin)
	case len(c) > 1:
		return "", fmt.Sprintf("%d folders named %s: %s", len(c), filepath.Base(origin), strings.Join(c, ", "))
	}
	return "", "no folder named " + filepath.Base(origin) + " in the usual places"
}

// projectOrigin is the folder a project's sessions were recorded in: sessions
// may sit in subfolders, so it is what they share ("" when unknown).
func projectOrigin(ctx context.Context, r *rt, ad agent.Adapter, ids []string) string {
	var origins []string
	for _, id := range ids {
		if origin, err := r.Eng.OriginDir(ctx, ad, id); err == nil && origin != "" {
			origins = append(origins, origin)
		}
	}
	return commonDir(origins)
}

// commonDir is the deepest directory containing every given path ("" when
// they share no plausible project folder).
func commonDir(paths []string) string {
	if len(paths) == 0 {
		return ""
	}
	parts := strings.Split(filepath.Clean(paths[0]), string(filepath.Separator))
	for _, p := range paths[1:] {
		q := strings.Split(filepath.Clean(p), string(filepath.Separator))
		n := 0
		for n < len(parts) && n < len(q) && parts[n] == q[n] {
			n++
		}
		parts = parts[:n]
	}
	common := strings.Join(parts, string(filepath.Separator))
	if len(parts) < 2 {
		return ""
	}
	for _, p := range paths {
		if filepath.Clean(p) == common {
			return common // a session's own folder: that is the project
		}
	}
	if len(parts) < 4 { // an ancestor like /Users/me is not a project folder
		return ""
	}
	return common
}
