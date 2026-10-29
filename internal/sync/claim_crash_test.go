//go:build failpoint

package sync

import (
	"context"
	"path/filepath"
	"testing"

	"session-memory/internal/agent"
	"session-memory/internal/session"
)

// A claim interrupted at any point leaves no half-made binding: after recovery
// the next claim links the session (or finds it linked), exactly once.
func TestClaimCrashMatrix(t *testing.T) {
	for _, point := range []string{"capture.after_planned", "capture.before_staged", "capture.after_staged", "claim.after_binding"} {
		t.Run(point, func(t *testing.T) {
			a, _, v, shared := claimPair(t)
			ctx := context.Background()
			tid, _ := a.put(9, 2)
			orig, rev, err := a.eng.Track(ctx, a.ad, agent.NativeRef{Home: a.home, NativeID: tid}, "")
			if err != nil {
				t.Fatal(err)
			}
			a.sync.Publish(ctx)

			fresh := nodeAt(t, v, shared, a.home)
			t.Setenv("SM_FAILPOINTS", point+"=error")
			_, _, claimed, err := fresh.claim(fresh.index(), tid)
			t.Setenv("SM_FAILPOINTS", "")
			if err == nil || claimed && point != "claim.after_binding" {
				t.Fatalf("failpoint %s did not interrupt the claim: claimed=%v err=%v", point, claimed, err)
			}
			if _, err := fresh.st.Recover(ctx); err != nil {
				t.Fatal(err)
			}
			if left, _ := filepath.Glob(filepath.Join(fresh.root, "journal", "*.json")); len(left) != 0 {
				t.Fatalf("journals left after recovery: %v", left)
			}
			_, _, claimed, err = fresh.claim(fresh.index(), tid)
			if err != nil {
				t.Fatal(err)
			}
			if wantClaimed := point != "claim.after_binding"; claimed != wantClaimed {
				t.Fatalf("second claim: claimed=%v, want %v", claimed, wantClaimed)
			}
			bs, _ := fresh.st.Bindings("")
			if len(bs) != 1 || bs[0].SessionID != orig.SessionID || bs[0].InstalledRevisionID != rev.RevisionID || bs[0].State != session.StateActive {
				t.Fatalf("bindings: %+v", bs)
			}
			if _, changed, err := fresh.eng.Checkpoint(ctx, fresh.ad, bs[0].BindingID, "", session.CaptureAuto); err != nil || changed {
				t.Fatalf("checkpoint: changed=%v err=%v", changed, err)
			}
			if un, _ := fresh.st.UnpublishedRevisions(fresh.eng.DeviceID); len(un) != 0 {
				t.Fatalf("a revision was made: %d", len(un))
			}
		})
	}
}
