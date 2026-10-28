package engine

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"

	"session-memory/internal/agent"
	"session-memory/internal/canonjson"
	"session-memory/internal/errcode"
	"session-memory/internal/project"
	"session-memory/internal/session"
	"session-memory/internal/vault"
)

type workspaceReader interface {
	Workspace(context.Context, agent.SnapshotReader) (string, error)
}

// taskWorkspace is implemented by agents whose project-less sessions live in a
// folder the agent makes itself (WorkBuddy tasks): the same folder is made
// here, under this user's home, instead of asking for one.
type taskWorkspace interface {
	TaskWorkspace(origin string) (string, bool)
}

func (e *Engine) restoreWorkspace(ctx context.Context, ad agent.Adapter, src agent.SnapshotReader, rev session.Revision, bnd *session.Binding, opt RestoreOptions) (string, agent.PathMap, []string, func(context.Context) error, error) {
	cwd, pm, gaps, check, _, err := e.restoreWorkspaceNotes(ctx, ad, src, rev, bnd, opt)
	return cwd, pm, gaps, check, err
}

// restoreWorkspaceNotes also reports how the directory was chosen when that
// was not stated by the user (notes are warnings for the plan, not gaps).
func (e *Engine) restoreWorkspaceNotes(ctx context.Context, ad agent.Adapter, src agent.SnapshotReader, rev session.Revision, bnd *session.Binding, opt RestoreOptions) (string, agent.PathMap, []string, func(context.Context) error, []string, error) {
	pm := opt.PathMap
	var notes []string
	origin := ""
	if reader, ok := ad.(workspaceReader); ok {
		var err error
		origin, err = reader.Workspace(ctx, src)
		if err != nil && opt.Cwd == "" {
			return "", pm, nil, nil, notes, err
		}
	}
	cwd := opt.Cwd
	if cwd == "" && bnd != nil {
		cwd = bnd.LocalCwd
	}
	if cwd == "" {
		if mapped, ok := pm.Apply(origin); ok {
			cwd = mapped
		}
	}
	autoCreate := false
	if cwd == "" && origin != "" && !dirExists(origin) {
		if tw, ok := ad.(taskWorkspace); ok {
			if dir, ok := tw.TaskWorkspace(origin); ok {
				cwd, autoCreate = dir, true
				notes = append(notes, "cwd_task_folder:"+dir)
			}
		}
	}
	if cwd == "" && origin != "" && !dirExists(origin) {
		if found, ok := UniqueLocalDir(origin, opt.DirHints); ok {
			cwd = found
			notes = append(notes, "cwd_auto_matched:"+found)
		}
	}
	if cwd == "" {
		cwd = origin
	}
	if origin != "" && cwd != "" && cwd != origin {
		pm.Rules = append([]agent.PathRule{{From: origin, To: cwd}}, pm.Rules...)
	}
	var source *project.State
	if rev.RepoStateRef != "" {
		rc, err := e.Store.OpenObject(ctx, rev.RepoStateRef, vault.TypeRepoState, nil)
		if err != nil {
			return cwd, pm, nil, nil, notes, err
		}
		var state project.State
		err = json.NewDecoder(rc).Decode(&state)
		rc.Close()
		if err != nil {
			return cwd, pm, nil, nil, notes, err
		}
		if state.SchemaVersion != 1 {
			return cwd, pm, nil, nil, notes, errcode.New(errcode.UnknownFormat, "unknown repo state schema")
		}
		if state.Status == "" {
			if state.Head != "" {
				state.Status = "git"
			} else {
				state.Status = "unavailable"
			}
		}
		source = &state
	}
	target, err := project.Inspect(ctx, cwd)
	if err != nil {
		return cwd, pm, nil, nil, notes, err
	}
	gaps := project.Gaps(ctx, source, target, cwd)
	st, statErr := os.Stat(cwd)
	dirOK := (statErr == nil && st.IsDir() && filepath.IsAbs(cwd)) || (autoCreate && os.IsNotExist(statErr))
	if !dirOK {
		gaps = append(gaps, "project_directory_missing")
	}
	baseline, err := canonjson.Hash(target)
	if err != nil {
		return cwd, pm, nil, nil, notes, err
	}
	check := func(ctx context.Context) error {
		if autoCreate {
			if _, err := os.Stat(cwd); os.IsNotExist(err) {
				// made now, empty: the state to compare against is that of the new folder
				if err := os.MkdirAll(cwd, 0755); err != nil {
					return err
				}
				fresh, err := project.Inspect(ctx, cwd)
				if err != nil {
					return err
				}
				if baseline, err = canonjson.Hash(fresh); err != nil {
					return err
				}
			}
		}
		st, err := os.Stat(cwd)
		if err != nil || !st.IsDir() || !filepath.IsAbs(cwd) {
			return errcode.New(errcode.NeedsAction, "select an existing local project directory with --cwd or project map")
		}
		current, err := project.Inspect(ctx, cwd)
		if err != nil {
			return err
		}
		hash, err := canonjson.Hash(current)
		if err != nil {
			return err
		}
		if hash != baseline {
			return errcode.New(errcode.BaselineChanged, "project code state changed after the restore plan")
		}
		if len(gaps) > 0 && !opt.AllowCodeMismatch {
			return errcode.New(errcode.NeedsAction, "code state is incomplete or different; fetch/checkout the recorded commit, or explicitly use --allow-code-mismatch").With("gaps", gaps)
		}
		return nil
	}
	return cwd, pm, gaps, check, notes, nil
}
