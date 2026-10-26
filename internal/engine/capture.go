// Package engine orchestrates adapters and the store into the user-level
// operations: track, checkpoint (spec §4.3 capture).
package engine

import (
	"bytes"
	"context"
	"io"
	"sort"
	"time"

	"session-memory/internal/agent"
	"session-memory/internal/canonjson"
	"session-memory/internal/errcode"
	"session-memory/internal/project"
	"session-memory/internal/session"
	"session-memory/internal/store"
	"session-memory/internal/vault"
)

// Engine binds a store, this device's id and a clock.
type Engine struct {
	Store    *store.FS
	DeviceID string
	Now      func() time.Time
}

func (e *Engine) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

// Track puts a native session under SessionMemory: creates its session id and
// binding, then captures the first revision.
func (e *Engine) Track(ctx context.Context, ad agent.Adapter, ref agent.NativeRef, projectRef string) (session.Binding, session.Revision, error) {
	ns, ok, err := ad.Locate(ctx, ref)
	if err != nil {
		return session.Binding{}, session.Revision{}, err
	}
	if !ok {
		return session.Binding{}, session.Revision{}, errcode.New(errcode.NotFound, "native session not found")
	}
	all, err := e.Store.Bindings("")
	if err != nil {
		return session.Binding{}, session.Revision{}, err
	}
	for _, b := range all {
		if b.AgentProfile.Agent == ns.Profile.Agent && b.NativeSessionID == ns.Ref.NativeID && b.NativeHome == ref.Home {
			return session.Binding{}, session.Revision{}, errcode.New(errcode.Conflict, "native session is already tracked")
		}
	}
	b := session.Binding{
		BindingID: session.NewBindingID(), SessionID: session.NewSessionID(), DeviceID: e.DeviceID,
		AgentProfile: ns.Profile, NativeSessionID: ns.Ref.NativeID, NativeHome: ref.Home,
		InstallMode: session.InstallNative, State: session.StateActive, Artifacts: map[string]session.ArtifactState{},
		LocalCwd: ns.Cwd,
	}
	if err := e.Store.SaveBinding(nil, b); err != nil {
		return b, session.Revision{}, err
	}
	rev, _, err := e.Checkpoint(ctx, ad, b.BindingID, projectRef, session.CaptureCheckpoint)
	if err != nil {
		return b, rev, err
	}
	b, _ = e.Store.LoadBinding(b.BindingID)
	return b, rev, nil
}

// Checkpoint captures new bytes of the bound native session and, when
// something changed, commits a new revision. changed=false means the native
// session is unchanged since the last revision.
func (e *Engine) Checkpoint(ctx context.Context, ad agent.Adapter, bindingID, projectRef, captureState string) (session.Revision, bool, error) {
	b, err := e.Store.LoadBinding(bindingID)
	if err != nil {
		return session.Revision{}, false, err
	}
	if b.State != session.StateActive {
		return session.Revision{}, false, errcode.New(errcode.NotFound, "binding is not active")
	}
	tx, err := e.Store.Begin(ctx, store.KindCapture, b.SessionID, b.BindingID)
	if err != nil {
		return session.Revision{}, false, err
	}
	ref := agent.NativeRef{Home: b.NativeHome, NativeID: b.NativeSessionID}
	res, err := ad.Capture(ctx, ref, agent.Cursor{Artifacts: b.Artifacts}, e.Store.Sink())
	if err != nil {
		return session.Revision{}, false, err
	}
	if res.State == agent.CaptureDetached {
		b.State = session.StateDetached
		if err := e.Store.SaveBinding(tx, b); err != nil {
			return session.Revision{}, false, err
		}
		return session.Revision{}, false, tx.Finish()
	}
	ns, found, err := ad.Locate(ctx, ref)
	if err != nil {
		return session.Revision{}, false, err
	}
	if found && ns.Cwd != "" {
		b.LocalCwd = ns.Cwd
	}
	repo, err := project.Inspect(ctx, b.LocalCwd)
	if err != nil {
		return session.Revision{}, false, err
	}
	repoJSON, err := canonjson.Marshal(repo)
	if err != nil {
		return session.Revision{}, false, err
	}
	repoRef := ""
	repoChanged := true
	if b.InstalledRevisionID != "" {
		prev, err := e.Store.LoadRevision(b.InstalledRevisionID)
		if err != nil {
			return session.Revision{}, false, err
		}
		if projectRef == "" {
			projectRef = prev.ProjectRef
		}
		if prev.RepoStateRef != "" {
			rc, err := e.Store.OpenObject(ctx, prev.RepoStateRef, vault.TypeRepoState, nil)
			if err != nil {
				return session.Revision{}, false, err
			}
			old, err := io.ReadAll(rc)
			rc.Close()
			if err != nil {
				return session.Revision{}, false, err
			}
			if bytes.Equal(repoJSON, old) {
				repoRef = prev.RepoStateRef
				repoChanged = false
			}
		}
	}
	if !res.Changed && !repoChanged && b.InstalledRevisionID != "" {
		return session.Revision{}, false, tx.Finish()
	}
	if repoRef == "" {
		obj, err := e.Store.PutObject(ctx, vault.TypeRepoState, nil, bytesNewReader(repoJSON))
		if err != nil {
			return session.Revision{}, false, err
		}
		repoRef = obj.Ref
	}
	if err := tx.SetState(store.StStaged, nil); err != nil {
		return session.Revision{}, false, err
	}

	snap := session.Snapshot{SchemaVersion: session.SchemaVersion}
	paths := make([]string, 0, len(res.Cursor.Artifacts))
	for p := range res.Cursor.Artifacts {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		st := res.Cursor.Artifacts[p]
		gen := 0
		if st.Cursor != nil {
			gen = st.Cursor.Generation
		}
		snap.Artifacts = append(snap.Artifacts, session.Artifact{Path: p, Kind: st.Kind, Form: st.Form, Generation: gen, Mode: st.Mode,
			Size: st.Size, Digest: st.Digest, Segments: st.Segments})
	}
	sj, err := canonjson.Marshal(snap)
	if err != nil {
		return session.Revision{}, false, err
	}
	info, err := e.Store.PutObject(ctx, vault.TypeSnapshot, nil, bytesNewReader(sj))
	if err != nil {
		return session.Revision{}, false, err
	}

	rev := session.Revision{
		SchemaVersion: session.SchemaVersion, SessionID: b.SessionID, ProjectRef: projectRef, ParentRevisionID: b.InstalledRevisionID,
		ProducerDeviceID: e.DeviceID, ProducerBindingID: b.BindingID, AgentProfile: b.AgentProfile,
		NativeSnapshotRef: info.Ref, RepoStateRef: repoRef, CaptureState: captureState, CreatedAt: session.FormatTime(e.now()),
	}
	if rev.ParentRevisionID == "" {
		rev.OriginNativeID = b.NativeSessionID
	}
	if res.State == agent.CaptureReplaced {
		rev.ReplacesRevisionID = b.InstalledRevisionID
	}
	if err := rev.Seal(); err != nil {
		return rev, false, err
	}
	var unsafe *session.UnsafeMark
	if res.State == agent.CaptureUnsafe {
		for _, d := range res.Diags {
			if d.Code == "unsafe_record" {
				unsafe = &session.UnsafeMark{Path: d.Detail, Offset: d.Offset, At: session.FormatTime(e.now())}
			}
		}
	}
	b.Artifacts = res.Cursor.Artifacts
	b.InstalledRevisionID = rev.RevisionID
	b.BaseRevisionID = firstNonEmpty(b.BaseRevisionID, rev.RevisionID)
	b.Unsafe = unsafe
	data := store.CaptureData{NewBinding: &b, RepoState: repoRef, Unsafe: unsafe, Artifacts: res.Cursor.Artifacts, Snapshot: info.Ref, Revision: &rev, Generation: res.State == agent.CaptureReplaced}
	if err := tx.SetState(store.StStored, data); err != nil {
		return rev, false, err
	}
	if err := e.Store.CommitRevision(tx, rev); err != nil {
		return rev, false, err
	}
	if err := tx.SetState(store.StRevisionCommitted, nil); err != nil {
		return rev, false, err
	}
	b.Artifacts = res.Cursor.Artifacts
	b.InstalledRevisionID = rev.RevisionID
	b.BaseRevisionID = firstNonEmpty(b.BaseRevisionID, rev.RevisionID)
	b.Unsafe = unsafe
	if err := e.Store.SaveBinding(tx, b); err != nil {
		return rev, false, err
	}
	if err := tx.SetState(store.StCursorAdvanced, nil); err != nil {
		return rev, false, err
	}
	return rev, true, tx.Finish()
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
