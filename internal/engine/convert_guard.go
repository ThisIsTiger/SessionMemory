package engine

import (
	"context"
	"errors"
	"io"

	"session-memory/internal/agent"
	"session-memory/internal/errcode"
	"session-memory/internal/session"
)

var errUncaptured = errors.New("uncaptured native progress")

// A read-only capture probe must not create encrypted objects or advance
// cursors. Any attempt to emit fresh bytes is enough to reject retirement.
type rejectNewBytes struct{}

func (rejectNewBytes) PutSegment(context.Context, agent.ArtifactKey, int64, int64, io.Reader) (session.SegmentRef, error) {
	return session.SegmentRef{}, errUncaptured
}
func (rejectNewBytes) PutBlob(context.Context, agent.ArtifactKey, int64, io.Reader) ([]session.SegmentRef, error) {
	return nil, errUncaptured
}

func checkConversionRetirement(ctx context.Context, ad agent.Adapter, w agent.Writer, bindings []session.Binding, closed bool) error {
	for _, b := range bindings {
		if guard, ok := w.(rollbackGuard); ok {
			if err := guard.CanRollback(ctx, agent.WriteInput{TargetState: agent.TargetState{Home: b.NativeHome}}); err != nil {
				return err
			}
		} else {
			state, _, err := ad.Runtime().Running(ctx, b.NativeHome, b.NativeSessionID)
			if err != nil {
				return err
			}
			if state == agent.Running || (state == agent.Unknown && !closed) {
				return errcode.New(errcode.ClientRunning, "close the existing target session before replacing its binding")
			}
		}
		res, err := ad.Capture(ctx, agent.NativeRef{Home: b.NativeHome, NativeID: b.NativeSessionID}, agent.Cursor{Artifacts: b.Artifacts}, rejectNewBytes{})
		if err != nil && !errors.Is(err, errUncaptured) {
			return err
		}
		if err != nil || res.State != agent.CaptureOK || res.Changed || b.Unsafe != nil || len(res.Cursor.Artifacts) != len(b.Artifacts) {
			return errcode.New(errcode.NeedsAction, "the existing target session has uncaptured changes; checkpoint and resolve its progress before converting")
		}
		for _, diag := range res.Diags {
			if diag.Code == "partial_tail" {
				return errcode.New(errcode.NeedsAction, "the existing target session has an incomplete native record")
			}
		}
	}
	return nil
}
