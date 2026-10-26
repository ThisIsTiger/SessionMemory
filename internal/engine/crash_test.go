//go:build failpoint

package engine

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"session-memory/internal/agent"
	"session-memory/internal/session"
	"session-memory/internal/store"
)

// Every capture failpoint: after the injected failure, Recover must leave a
// state from which a fresh Checkpoint yields exactly the revision an
// uninterrupted run would (spec §14.4, capture row).
func TestCaptureCrashMatrix(t *testing.T) {
	var points []string
	for _, e := range store.Flow[store.KindCapture] {
		if e.State == store.StDone {
			continue
		}
		points = append(points, "capture.before_"+string(e.State), "capture.after_"+string(e.State))
	}
	for _, point := range points {
		t.Run(point, func(t *testing.T) {
			r := newRig(t)
			tid, p := r.put(10, 2)
			ref := agent.NativeRef{Home: r.home, NativeID: tid}
			t.Setenv("SM_FAILPOINTS", point+"=error")
			b, _, err := r.eng.Track(context.Background(), r.ad, ref, "")
			t.Setenv("SM_FAILPOINTS", "")
			if err == nil {
				t.Skipf("failpoint %s not reached on this path", point)
			}
			if _, err := r.st.Recover(context.Background()); err != nil {
				t.Fatal(err)
			}
			if left, _ := filepath.Glob(filepath.Join(r.root, "journal", "*.json")); len(left) != 0 {
				t.Fatalf("journals left after recovery: %v", left)
			}
			// the binding file exists from Track's first step in every case
			bs, _ := r.st.Bindings("")
			if len(bs) != 1 {
				t.Fatalf("bindings = %d", len(bs))
			}
			rev, _, err := r.eng.Checkpoint(context.Background(), r.ad, bs[0].BindingID, "", session.CaptureCheckpoint)
			if err != nil {
				t.Fatal(err)
			}
			final, _ := r.st.Bindings("")
			if final[0].InstalledRevisionID == "" {
				t.Fatal("no revision after recovery + checkpoint")
			}
			cur, _ := r.st.Revisions(final[0].SessionID)
			if len(cur) < 1 {
				t.Fatal("no committed revision")
			}
			// the revision's content is the native file, byte for byte
			orig, _ := os.ReadFile(p)
			head := cur[len(cur)-1]
			logic := "sessions/2026/09/01/" + filepath.Base(p)
			if !bytes.Equal(r.readSnapshotFile(head, logic), orig) {
				t.Fatal("recovered snapshot differs from the native file")
			}
			_ = rev
			_ = b
		})
	}
}
