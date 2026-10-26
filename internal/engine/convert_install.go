package engine

import (
	"context"
	"time"

	"session-memory/internal/agent"
	"session-memory/internal/canonjson"
	"session-memory/internal/convert"
	"session-memory/internal/errcode"
	"session-memory/internal/session"
)

// ConvertRestore installs the full history of a revision produced by another
// agent as a NEW native session of dst (design §7.2). The history is converted
// by convert.Convert, serialised in memory into dst's native files, and then
// written by dst's own native Writer, so planning, locking, backup, journal,
// verification and crash recovery are exactly those of a native restore.
//
// The new binding is bound to the same session id with install mode
// "converted"; its import receipt (count, boundary offset and hash) fixes the
// part of the native file that came from the conversion, and the adoption
// cursor starts after it, so only the user's later records form new revisions.
// An older active binding of dst for this session is superseded.
func (e *Engine) ConvertRestore(ctx context.Context, from, dst agent.Adapter, sessionID string, opt RestoreOptions) (agent.Plan, *agent.Receipt, error) {
	ser, ok := dst.(convert.Serializer)
	ex, isEx := from.(agent.Extractor)
	if !ok || !isEx || from.Name() == dst.Name() {
		return agent.Plan{}, nil, errcode.New(errcode.Unsupported, "conversion needs two different agents with a conversion source and target")
	}
	if _, ok := ToolMaps[dst.Name()]; !ok {
		return agent.Plan{}, nil, errcode.New(errcode.Unsupported, "no conversion rules for target "+dst.Name())
	}
	g, rev, err := e.resolveRevision(sessionID, opt.RevisionID)
	if err != nil {
		return agent.Plan{}, nil, err
	}
	if rev.AgentProfile.Agent != from.Name() {
		return agent.Plan{}, nil, errcode.New(errcode.Unsupported, "the revision was produced by "+rev.AgentProfile.Agent+", not "+from.Name())
	}
	bs, _ := e.Store.Bindings(sessionID)
	var supersede []string
	for _, b := range bs {
		if b.DeviceID != e.DeviceID || b.AgentProfile.Agent != dst.Name() {
			continue
		}
		if b.ImportReceipt != nil && b.ImportReceipt.SourceRevision == rev.RevisionID && b.InstallMode == session.InstallConverted {
			return agent.Plan{}, nil, errcode.New(errcode.NotFound, "this revision was already converted into "+dst.Name()).With("native_session_id", b.NativeSessionID)
		}
		if b.State != session.StateActive {
			continue
		}
		switch session.LandingFor(g, b.InstalledRevisionID, rev.RevisionID) {
		case session.LandUpToDate:
			return agent.Plan{}, nil, errcode.New(errcode.NotFound, "the existing "+dst.Name()+" session is already at this revision").With("native_session_id", b.NativeSessionID)
		case session.LandNeedsAction:
			return agent.Plan{}, nil, errcode.New(errcode.NeedsAction, "the existing "+dst.Name()+" session has progress this revision does not include; choose or merge first")
		}
		supersede = append(supersede, b.BindingID)
	}
	out, err := e.ConversionReport(ctx, ex, rev, dst.Name())
	if err != nil {
		return agent.Plan{}, nil, err
	}
	if len(out.Records) == 0 {
		return agent.Plan{}, nil, errcode.New(errcode.NotFound, "the revision has no visible history to convert")
	}
	srcSnap, err := e.Store.SnapshotReader(ctx, rev.NativeSnapshotRef)
	if err != nil {
		return agent.Plan{}, nil, err
	}
	cwd, pmap, gaps, check, err := e.restoreWorkspace(ctx, from, srcSnap, rev, nil, opt)
	if err != nil {
		return agent.Plan{}, nil, err
	}
	built, err := ser.SerializeConverted(out, convert.Target{Home: opt.Home, Cwd: cwd, Now: e.now(), SourceAgent: from.Name(), SourceRevision: rev.RevisionID})
	if err != nil {
		return agent.Plan{}, nil, err
	}
	var w agent.Writer
	if cw, ok := dst.(closedWriter); ok {
		w = cw.WriterWith(opt.ClientClosed)
	} else {
		w = dst.Writer()
	}
	in := agent.WriteInput{SessionID: sessionID, RevisionID: rev.RevisionID, Mode: agent.ModeNative, Source: built.Snapshot, SourceProfile: built.Profile,
		Target: built.Profile, TargetState: agent.TargetState{Home: opt.Home}, Cwd: cwd, ValidateEnvironment: check}
	plan, err := w.Plan(ctx, in)
	if err != nil {
		return plan, nil, err
	}
	_ = pmap
	plan.Mode = agent.ModeConvert
	report := out.Report
	report.Route = plan.Route
	plan.Report = &report
	plan.Warnings = append(plan.Warnings, gaps...)
	if len(gaps) > 0 && opt.AllowCodeMismatch {
		plan.Warnings = append(plan.Warnings, "code_state_override")
	}
	plan.Digest = ""
	if plan.Digest, err = canonjson.Hash(plan); err != nil || opt.PlanOnly {
		return plan, nil, err
	}
	if len(plan.Writes) == 0 {
		return plan, nil, errcode.New(errcode.NotFound, "nothing to write")
	}
	writerVersion, last, count := built.WriterVersion, built.Last, int64(built.Records)
	src := rev.RevisionID
	return e.runInstall(ctx, installSpec{sessionID: sessionID, revisionID: rev.RevisionID, ad: dst, w: w, in: in, plan: plan, opt: opt, cwd: cwd, gaps: gaps,
		mode: session.InstallConverted, profile: built.Profile, supersede: supersede,
		boundary: func(r *session.ImportReceipt) {
			r.WriterVersion, r.SourceRevision, r.LastMarker, r.WrittenCount = writerVersion, src, last, count
			r.At = session.FormatTime(time.Now())
		}})
}
