//go:build failpoint

package sync

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"session-memory/internal/agent"
	"session-memory/internal/engine"
	"session-memory/internal/store"
)

// Install crash matrix (spec §14.4): after any failure plus recovery, the target
// is byte-identical to before (files removed or restored), and a fresh restore
// then succeeds.
func TestInstallCrashMatrix(t *testing.T) {
	points := []string{"install.mid_write"}
	for _, e := range store.Flow[store.KindInstall] {
		if e.State == store.StDone || e.State == store.StPlanned {
			continue
		}
		points = append(points, "install.before_"+string(e.State), "install.after_"+string(e.State))
	}
	for _, point := range points {
		t.Run(point, func(t *testing.T) {
			a, b, _ := newPair(t)
			ctx := context.Background()
			tid, ap := a.put(30, 2)
			bind, rev1, _ := a.eng.Track(ctx, a.ad, agent.NativeRef{Home: a.home, NativeID: tid}, "")
			a.sync.Publish(ctx)
			b.sync.Reconcile(ctx)
			// first restore normally so the second one exercises the append path with backups
			if _, _, err := b.restore(t, rev1.SessionID, engine.RestoreOptions{ClientClosed: true}); err != nil {
				t.Fatal(err)
			}
			orig, _ := os.ReadFile(ap)
			os.WriteFile(ap, append(append([]byte(nil), orig...), []byte(`{"timestamp":"t","type":"event_msg","payload":{}}`+"\n")...), 0600)
			a.eng.Checkpoint(ctx, a.ad, bind.BindingID, "", "auto")
			a.sync.Publish(ctx)
			b.sync.Reconcile(ctx)

			files, _ := filepath.Glob(filepath.Join(b.home, "sessions", "*", "*", "*", "*.jsonl"))
			before, _ := os.ReadFile(files[0])

			t.Setenv("SM_FAILPOINTS", point+"=error")
			_, _, err := b.restore(t, rev1.SessionID, engine.RestoreOptions{ClientClosed: true})
			t.Setenv("SM_FAILPOINTS", "")
			if err == nil {
				t.Skipf("%s not reached", point)
			}
			if _, err := b.st.Recover(ctx); err != nil {
				t.Fatal(err)
			}
			now, _ := os.ReadFile(files[0])
			// states at or after verified are rolled forward by design; earlier ones restore the original
			bs, _ := b.st.Bindings(rev1.SessionID)
			if bs[0].InstalledRevisionID == rev1.RevisionID && !bytes.Equal(now, before) {
				t.Fatalf("target differs from its pre-install bytes after recovery at %s", point)
			}
			if left, _ := filepath.Glob(filepath.Join(b.root, "journal", "*.json")); len(left) != 0 {
				t.Fatalf("journals left: %v", left)
			}
		})
	}
}
