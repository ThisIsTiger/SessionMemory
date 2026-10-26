//go:build failpoint

package sync

import (
	"context"
	"path/filepath"
	"testing"

	"session-memory/internal/agent"
	"session-memory/internal/store"
)

// Spec §14.4 publish row: whatever point fails, the folder never holds two
// different files for one sequence, and after recovery the other device
// imports the revision exactly once.
func TestPublishCrashMatrix(t *testing.T) {
	points := []string{"publish.mid_upload"}
	for _, e := range store.Flow[store.KindPublish] {
		if e.State == store.StDone || e.State == store.StPlanned {
			continue
		}
		points = append(points, "publish.before_"+string(e.State), "publish.after_"+string(e.State))
	}
	for _, point := range points {
		t.Run(point, func(t *testing.T) {
			a, b, shared := newPair(t)
			ctx := context.Background()
			tid, _ := a.put(20, 2)
			_, rev, err := a.eng.Track(ctx, a.ad, agent.NativeRef{Home: a.home, NativeID: tid}, "")
			if err != nil {
				t.Fatal(err)
			}
			t.Setenv("SM_FAILPOINTS", point+"=error")
			_, perr := a.sync.Publish(ctx)
			t.Setenv("SM_FAILPOINTS", "")
			if perr == nil {
				t.Skipf("%s not reached", point)
			}
			if _, err := a.st.Recover(ctx); err != nil {
				t.Fatal(err)
			}
			if _, err := a.sync.Publish(ctx); err != nil {
				t.Fatal(err)
			}
			mf, _ := filepath.Glob(filepath.Join(shared, "manifests", "*", "0000000000*"))
			seqs := map[string]int{}
			for _, f := range mf {
				seqs[filepath.Base(f)[:12]]++
			}
			for s, n := range seqs {
				if n != 1 {
					t.Fatalf("sequence %s has %d files", s, n)
				}
			}
			if res, err := b.sync.Reconcile(ctx); err != nil || res.Revisions != 1 || res.Quarantined != 0 {
				t.Fatalf("B reconcile: %+v %v", res, err)
			}
			g, _ := b.st.Graph(rev.SessionID)
			if h := g.Heads(); len(h) != 1 || h[0] != rev.RevisionID {
				t.Fatalf("B heads = %v", h)
			}
			if un, _ := a.st.UnpublishedRevisions(a.eng.DeviceID); len(un) != 0 {
				t.Fatal("revision still unpublished on A")
			}
		})
	}
}
