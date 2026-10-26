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

func (e *Engine) restoreWorkspace(ctx context.Context, ad agent.Adapter, src agent.SnapshotReader, rev session.Revision, bnd *session.Binding, opt RestoreOptions) (string, agent.PathMap, []string, func(context.Context) error, error) {
	pm := opt.PathMap
	origin := ""
	if reader, ok := ad.(workspaceReader); ok {
		var err error
		origin, err = reader.Workspace(ctx, src)
		if err != nil && opt.Cwd == "" {
			return "", pm, nil, nil, err
		}
	}
	cwd := opt.Cwd
	if cwd == "" && bnd != nil {
		cwd = bnd.LocalCwd
	}
	if cwd == "" {
		cwd, _ = pm.Apply(origin)
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
			return cwd, pm, nil, nil, err
		}
		var state project.State
		err = json.NewDecoder(rc).Decode(&state)
		rc.Close()
		if err != nil {
			return cwd, pm, nil, nil, err
		}
		if state.SchemaVersion != 1 {
			return cwd, pm, nil, nil, errcode.New(errcode.UnknownFormat, "unknown repo state schema")
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
		return cwd, pm, nil, nil, err
	}
	gaps := project.Gaps(ctx, source, target, cwd)
	st, statErr := os.Stat(cwd)
	dirOK := statErr == nil && st.IsDir() && filepath.IsAbs(cwd)
	if !dirOK {
		gaps = append(gaps, "project_directory_missing")
	}
	baseline, err := canonjson.Hash(target)
	if err != nil {
		return cwd, pm, nil, nil, err
	}
	check := func(ctx context.Context) error {
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
	return cwd, pm, gaps, check, nil
}
