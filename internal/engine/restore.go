package engine

import (
	"context"

	"session-memory/internal/agent"
	"session-memory/internal/canonjson"
	"session-memory/internal/errcode"
	"session-memory/internal/session"
	"session-memory/internal/store"
)

// RestoreOptions configures Restore.
type RestoreOptions struct {
	Home              string // target agent home
	PathMap           agent.PathMap
	ClientClosed      bool // the user states the client is not running
	PlanOnly          bool
	RevisionID        string // default: the effective head
	Cwd               string
	AllowCodeMismatch bool // explicitly accept recorded code-state gaps
}

type closedWriter interface{ WriterWith(bool) agent.Writer }
type rollbackGuard interface {
	CanRollback(context.Context, agent.WriteInput) error
}

// Restore installs a synced revision into this device's agent home (spec §4.4,
// design §6.2). It refuses conflicts and non-fast-forward landings.
func (e *Engine) Restore(ctx context.Context, ad agent.Adapter, sessionID string, opt RestoreOptions) (agent.Plan, *agent.Receipt, error) {
	g, rev, err := e.resolveRevision(sessionID, opt.RevisionID)
	if err != nil {
		return agent.Plan{}, nil, err
	}
	target := rev.RevisionID
	if rev.AgentProfile.Agent != ad.Name() {
		return agent.Plan{}, nil, errcode.New(errcode.Unsupported, "native restore requires the same source and target agent; use conversion for a different agent")
	}
	bs, _ := e.Store.Bindings(sessionID)
	var bnd *session.Binding
	for i := range bs {
		if bs[i].State == session.StateActive && bs[i].DeviceID == e.DeviceID && bs[i].AgentProfile.Agent == ad.Name() {
			bnd = &bs[i]
		}
	}
	if bnd != nil {
		switch session.LandingFor(g, bnd.InstalledRevisionID, target) {
		case session.LandUpToDate:
			return agent.Plan{}, nil, errcode.New(errcode.NotFound, "already installed at this revision")
		case session.LandNeedsAction:
			return agent.Plan{}, nil, errcode.New(errcode.NeedsAction, "the installed version is not an ancestor of the chosen revision")
		}
	}
	src, err := e.Store.SnapshotReader(ctx, rev.NativeSnapshotRef)
	if err != nil {
		return agent.Plan{}, nil, err
	}
	cwd, pmap, gaps, checkEnvironment, err := e.restoreWorkspace(ctx, ad, src, rev, bnd, opt)
	if err != nil {
		return agent.Plan{}, nil, err
	}
	var w agent.Writer
	if cw, ok := ad.(closedWriter); ok {
		w = cw.WriterWith(opt.ClientClosed)
	} else {
		w = ad.Writer()
	}
	in := agent.WriteInput{SessionID: sessionID, RevisionID: target, Mode: agent.ModeNative, Source: src, SourceProfile: rev.AgentProfile,
		PathMap: pmap, Target: session.AgentProfile{Agent: ad.Name(), Format: rev.AgentProfile.Format}, TargetState: agent.TargetState{Home: opt.Home}, Cwd: cwd, ValidateEnvironment: checkEnvironment}
	if bnd != nil {
		in.BindingID = bnd.BindingID
	}
	plan, err := w.Plan(ctx, in)
	if err != nil {
		return plan, nil, err
	}
	plan.Warnings = append(plan.Warnings, gaps...)
	if len(gaps) > 0 && opt.AllowCodeMismatch {
		plan.Warnings = append(plan.Warnings, "code_state_override")
	}
	plan.Digest = ""
	plan.Digest, err = canonjson.Hash(plan)
	if err != nil || opt.PlanOnly {
		return plan, nil, err
	}
	if len(plan.Writes) == 0 {
		return plan, nil, errcode.New(errcode.NotFound, "the target already matches this revision")
	}
	return e.runInstall(ctx, installSpec{sessionID: sessionID, revisionID: target, ad: ad, w: w, in: in, plan: plan, opt: opt, cwd: cwd, gaps: gaps, bnd: bnd,
		mode: session.InstallNative, profile: rev.AgentProfile})
}

// installSpec is everything the shared install tail needs.
type installSpec struct {
	sessionID  string
	revisionID string // the revision this install represents (source revision for a conversion)
	ad         agent.Adapter
	w          agent.Writer
	in         agent.WriteInput
	plan       agent.Plan
	opt        RestoreOptions
	cwd        string
	gaps       []string
	bnd        *session.Binding // existing binding being advanced (native only)
	mode       string           // session.InstallNative | session.InstallConverted
	profile    session.AgentProfile
	supersede  []string // active bindings retired when the new binding commits
	boundary   func(*session.ImportReceipt)
}

// runInstall executes a planned install under the journal: locks, apply,
// adoption, verification, then the binding commit (spec §4.3 install row).
func (e *Engine) runInstall(ctx context.Context, sp installSpec) (agent.Plan, *agent.Receipt, error) {
	sessionID, target, ad, w, in, plan, opt, cwd, gaps, bnd := sp.sessionID, sp.revisionID, sp.ad, sp.w, sp.in, sp.plan, sp.opt, sp.cwd, sp.gaps, sp.bnd
	tx, err := e.Store.Begin(ctx, store.KindInstall, sessionID, in.BindingID)
	if err != nil {
		return plan, nil, err
	}
	if err := tx.SetState(store.StPlanned, store.InstallData{PlanDigest: plan.Digest, Mode: plan.Mode, Route: plan.Route, Home: opt.Home}); err != nil {
		return plan, nil, err
	}
	if locker, ok := w.(agent.InstallLocker); ok {
		release, err := locker.LockInstall(ctx, in)
		if err != nil {
			tx.Finish()
			return plan, nil, err
		}
		defer release()
		in.VendorLocked = true
	}
	rollback := func() error {
		if tx.State() == store.StBackedUp || tx.State() == store.StWritten || tx.State() == store.StRollingBack {
			if guard, ok := w.(rollbackGuard); ok {
				if err := guard.CanRollback(context.Background(), in); err != nil {
					return err
				}
			}
		}
		return tx.Rollback()
	}
	rec, err := w.Apply(ctx, in, plan, tx)
	if err == nil {
		rec.InstallMode = sp.mode
	}
	if err != nil {
		if tx.State() == store.StRollingBack || errcode.CodeOf(err) == errcode.RecoveryNeeded {
			return plan, nil, err // preserve the rollback journal for startup recovery
		}
		if tx.State() == store.StBackedUp || tx.State() == store.StWritten || tx.State() == store.StStaged {
			if rerr := rollback(); rerr != nil {
				return plan, nil, errcode.Wrap(errcode.RecoveryNeeded, "install failed and rollback did not finish", rerr)
			}
		} else if tx.State() != store.StDone {
			tx.Finish()
		}
		return plan, nil, err
	}
	// adopt the installed files as the binding's capture state (no revision)
	ref := agent.NativeRef{Home: opt.Home, NativeID: rec.NativeSessionID}
	adopt, err := ad.Capture(ctx, ref, agent.Cursor{}, e.Store.Sink())
	if err != nil {
		if rollbackErr := rollback(); rollbackErr != nil {
			return plan, &rec, errcode.Wrap(errcode.RecoveryNeeded, "adoption rollback failed", rollbackErr)
		}
		return plan, &rec, err
	}
	if rec.NativeSessionID == "" || adopt.State != agent.CaptureOK || len(adopt.Cursor.Artifacts) == 0 {
		if rollbackErr := rollback(); rollbackErr != nil {
			return plan, &rec, errcode.Wrap(errcode.RecoveryNeeded, "adoption rollback failed", rollbackErr)
		}
		return plan, &rec, errcode.New(errcode.TargetRejected, "installed native session could not be safely adopted")
	}
	if err := w.Verify(ctx, in, rec); err != nil {
		if rollbackErr := rollback(); rollbackErr != nil {
			return plan, &rec, errcode.Wrap(errcode.RecoveryNeeded, "post-adoption rollback failed", rollbackErr)
		}
		return plan, &rec, err
	}
	if opt.AllowCodeMismatch && rec.Import != nil {
		rec.Import.CodeStateOverride = gaps
	}
	if sp.boundary != nil && rec.Import != nil {
		sp.boundary(rec.Import)
	}
	nb := session.Binding{BindingID: session.NewBindingID(), SessionID: sessionID, DeviceID: e.DeviceID, AgentProfile: sp.profile,
		NativeSessionID: rec.NativeSessionID, NativeHome: opt.Home, InstallMode: sp.mode, State: session.StateActive,
		BaseRevisionID: target, Artifacts: adopt.Cursor.Artifacts}
	if bnd != nil {
		nb = *bnd
		nb.Artifacts = adopt.Cursor.Artifacts
	}
	nb.LocalCwd = cwd
	nb.InstalledRevisionID = target
	nb.ImportReceipt = rec.Import
	var d store.InstallData
	tx.Data(&d)
	d.NewBinding = &nb
	d.Superseded = sp.supersede
	d.Receipt = &rec
	// verified = files verified AND the binding to commit is fully prepared, so
	// recovery can roll forward from here (spec §4.3 install row)
	if err := tx.SetState(store.StVerified, d); err != nil {
		return plan, &rec, err
	}
	if err := e.Store.Supersede(tx, sp.supersede); err != nil {
		return plan, &rec, err
	}
	if err := e.Store.SaveBinding(tx, nb); err != nil {
		return plan, &rec, err
	}
	if err := tx.SetState(store.StBindingCommitted, nil); err != nil {
		return plan, &rec, err
	}
	now := session.FormatTime(e.now())
	if err := (&deviceOutbox{root: e.Store.Root}).add(
		session.Entry{Type: session.EntryReceipt, Receipt: &session.ReceiptEntry{SessionID: sessionID, RevisionID: target, DeviceID: e.DeviceID,
			Status: session.ReceiptInstalled, InstallMode: sp.mode, BindingID: nb.BindingID, At: now}},
	); err != nil {
		return plan, &rec, err
	}
	return plan, &rec, tx.Finish()
}

// resolveRevision picks the revision to install: the requested one, else the
// effective head (refusing a conflict).
func (e *Engine) resolveRevision(sessionID, want string) (*session.Graph, session.Revision, error) {
	g, err := e.Store.Graph(sessionID)
	if err != nil {
		return nil, session.Revision{}, err
	}
	ch, _ := e.Store.Choices(sessionID)
	res := session.Resolve(g, ch)
	if len(res.Heads) == 0 {
		return nil, session.Revision{}, errcode.New(errcode.NotFound, "no revisions for this session")
	}
	if want == "" {
		if res.Conflict {
			return nil, session.Revision{}, errcode.New(errcode.Conflict, "the session has more than one live head; run choose first").With("heads", len(res.Heads))
		}
		want = res.Effective
	}
	revs, _ := e.Store.Revisions(sessionID)
	for _, r := range revs {
		if r.RevisionID == want {
			return g, r, nil
		}
	}
	return nil, session.Revision{}, errcode.New(errcode.NotFound, "revision not found")
}
