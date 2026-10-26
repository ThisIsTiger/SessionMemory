//go:build failpoint

package sync

import (
	"context"
	"path/filepath"
	"testing"

	"session-memory/internal/agent"
	"session-memory/internal/store"
)

func TestPullCrashMatrix(t *testing.T) {
	points := []string{}
	for _, state := range []store.State{store.StVerified, store.StCommitted} {
		points = append(points, "pull.before_"+string(state), "pull.after_"+string(state))
	}
	for _, point := range points {
		t.Run(point, func(t *testing.T) {
			a, b, _ := newPair(t)
			ctx := context.Background()
			tid, _ := a.put(801, 2)
			_, rev, err := a.eng.Track(ctx, a.ad, agent.NativeRef{Home: a.home, NativeID: tid}, "")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := a.sync.Publish(ctx); err != nil {
				t.Fatal(err)
			}
			t.Setenv("SM_FAILPOINTS", point+"=error")
			if _, err := b.sync.Reconcile(ctx); err == nil {
				t.Fatal("failpoint was not reached")
			}
			t.Setenv("SM_FAILPOINTS", "")
			// Simulate a restart, including re-registration of the pull resumer.
			v := b.st.Sealer()
			b.st.Close()
			reopened, err := store.Open(b.root, v)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			b.st = reopened
			b.sync = New(reopened, b.sync.T, v, b.root, b.sync.Info)
			if _, err := reopened.Recover(ctx); err != nil {
				t.Fatal(err)
			}
			if _, err := b.sync.Reconcile(ctx); err != nil {
				t.Fatal(err)
			}
			revs, err := reopened.Revisions(rev.SessionID)
			if err != nil || len(revs) != 1 {
				t.Fatalf("revisions=%d %v", len(revs), err)
			}
			out, err := b.sync.Dev.Outbox()
			if err != nil || len(out) != 1 {
				t.Fatalf("received receipt missing or duplicated: %d %v", len(out), err)
			}
			if files, _ := filepath.Glob(filepath.Join(b.root, "journal", "*.json")); len(files) != 0 {
				t.Fatalf("unrecovered pull journals: %v", files)
			}
		})
	}
}
