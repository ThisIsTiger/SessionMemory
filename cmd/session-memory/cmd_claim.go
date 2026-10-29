package main

import (
	"context"

	"session-memory/internal/agent"
	"session-memory/internal/engine"
	"session-memory/internal/errcode"
	"session-memory/internal/session"
)

// trackOne puts one native session under SessionMemory. When the vault already
// holds the session (published by this computer before its state was lost, or
// installed on it from another computer) and the native files are exactly one
// of its revisions, it is linked to that vault session: no second copy, nothing
// uploaded. Otherwise it is tracked as a new session, as before; that still
// makes a duplicate when the native has progress the vault lacks.
func (r *rt) trackOne(ctx context.Context, ad agent.Adapter, ref agent.NativeRef, projectRef string) error {
	_, rev, claimed, err := r.Eng.Claim(ctx, ad, ref, r.claims)
	if err != nil {
		return err
	}
	if claimed {
		r.claimed++
		if r.claimedRefs == nil {
			r.claimedRefs = map[string]bool{}
		}
		if rev.ProjectRef != "" {
			r.claimedRefs[rev.ProjectRef] = true
		}
		return nil
	}
	_, _, err = r.Eng.Track(ctx, ad, ref, projectRef)
	return err
}

// vaultClaims is the claim index of a runtime with a sync folder: the vault's
// sessions are read from the other devices' manifests (no segment is
// downloaded), and only when the first native session needs them. A vault that
// cannot be read must not end in duplicates, so that stops the tracking.
func (r *rt) vaultClaims() *engine.ClaimIndex {
	if r.Sync == nil {
		return nil
	}
	return &engine.ClaimIndex{
		Load: func(ctx context.Context) ([]session.Revision, error) {
			revs, err := r.Sync.PeekRevisions(ctx)
			if err != nil {
				return nil, errcode.Wrap(errcode.Internal, "could not read the list of sessions in your cloud folder, so nothing was tracked (it would risk copies of sessions that are already there); run setup again later", err)
			}
			return revs, nil
		},
		FetchSnapshot: r.Sync.FetchSnapshot,
		FetchState:    r.Sync.FetchRepoState,
	}
}

// mapClaimedProjects gives the projects of the sessions just linked a folder
// here: the folder the sessions are in. Their project refs come from the
// computer that made them, so they differ from the ref a fresh folder mapping
// gets, and without a mapping a later relay would not update them.
func (a *App) mapClaimedProjects(r *rt, pm session.ProjectMap, existing []session.ProjectMap) ([]session.ProjectMap, error) {
	refs := r.claimedRefs
	r.claimedRefs = nil
	for ref := range refs {
		if ref == pm.ProjectRef {
			continue
		}
		mapped := false
		for _, p := range existing {
			mapped = mapped || p.ProjectRef == ref
		}
		if mapped {
			continue
		}
		extra := session.ProjectMap{SchemaVersion: 1, ProjectRef: ref, Label: pm.Label, LocalPath: pm.LocalPath, Since: pm.Since}
		if err := saveProject(a, extra); err != nil {
			return existing, err
		}
		existing = append(existing, extra)
	}
	return existing, nil
}
