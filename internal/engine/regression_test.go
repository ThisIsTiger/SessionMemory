//go:build failpoint

package engine

import (
	"context"
	"os"
	"session-memory/internal/agent"
	"session-memory/internal/session"
	"testing"
)

func TestRegressionRecoveryAdvancesInstalledRevision(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	tid, p := r.put(503, 2)
	b, _, err := r.eng.Track(ctx, r.ad, agent.NativeRef{Home: r.home, NativeID: tid}, "")
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.WriteString("{\"type\":\"event_msg\",\"payload\":{\"type\":\"agent_message\",\"message\":\"new\"}}\n")
	f.Close()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("SM_FAILPOINTS", "capture.after_revision_committed=error")
	rev, _, err := r.eng.Checkpoint(ctx, r.ad, b.BindingID, "", session.CaptureCheckpoint)
	if err == nil {
		t.Fatal("expected failpoint")
	}
	t.Setenv("SM_FAILPOINTS", "")
	reports, err := r.st.Recover(ctx)
	if err != nil {
		t.Fatal(err)
	}
	recovered, err := r.st.LoadBinding(b.BindingID)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.InstalledRevisionID != rev.RevisionID {
		t.Fatalf("cursor recovered but installed revision stale: got=%s want=%s reports=%+v", recovered.InstalledRevisionID, rev.RevisionID, reports)
	}
}
