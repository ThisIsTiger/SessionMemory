package sync

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"path/filepath"
	"testing"
	"time"

	"session-memory/internal/agent"
	"session-memory/internal/engine"
	"session-memory/internal/session"
	"session-memory/internal/testutil/codexgen"
	"session-memory/internal/vault"
)

// Each handoff uses an independently initialized device and store. The model
// is the exact bytes appended by the current owner, not the engine's snapshot.
func TestRandomizedThreeDeviceHandoffs(t *testing.T) {
	for seed := uint64(1); seed <= 3; seed++ {
		t.Run(fmt.Sprint(seed), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
			defer cancel()
			key, err := vault.GenerateKey()
			if err != nil {
				t.Fatal(err)
			}
			v, err := vault.New(session.NewVaultID(), key)
			if err != nil {
				t.Fatal(err)
			}
			shared, project := t.TempDir(), t.TempDir()
			nodes := []*node{newNode(t, v, shared), newNode(t, v, shared), newNode(t, v, shared)}
			tid, path := nodes[0].put(int64(900+seed), 1)
			want := codexgen.Rollout(codexgen.Options{Seed: int64(900 + seed), Turns: 1, Cwd: project})
			if err := os.WriteFile(path, want, 0600); err != nil {
				t.Fatal(err)
			}
			binding, current, err := nodes[0].eng.Track(ctx, nodes[0].ad, agent.NativeRef{Home: nodes[0].home, NativeID: tid}, "")
			if err != nil {
				t.Fatal(err)
			}
			sid, owner := binding.SessionID, 0
			rng := rand.New(rand.NewPCG(seed, 29))
			for step := 0; step < 12; step++ {
				sender := nodes[owner]
				if n, err := sender.sync.Publish(ctx); err != nil || n == 0 {
					t.Fatalf("step %d publish: %d %v", step, n, err)
				}
				if n, err := sender.sync.Publish(ctx); err != nil || n != 0 {
					t.Fatalf("step %d publish not idempotent: %d %v", step, n, err)
				}
				next := (owner + 1 + rng.IntN(2)) % len(nodes)
				receiver := nodes[next]
				if _, err := receiver.sync.Reconcile(ctx); err != nil {
					t.Fatal(err)
				}
				if again, err := receiver.sync.Reconcile(ctx); err != nil || again.Applied != 0 {
					t.Fatalf("step %d reconcile not idempotent: %+v %v", step, again, err)
				}
				if _, _, err := receiver.restore(t, sid, engine.RestoreOptions{Cwd: project, ClientClosed: true}); err != nil {
					t.Fatalf("step %d restore: %v", step, err)
				}
				bs, err := receiver.st.Bindings(sid)
				if err != nil || len(bs) != 1 || bs[0].InstalledRevisionID != current.RevisionID {
					t.Fatalf("step %d bindings: %+v %v", step, bs, err)
				}
				located, found, err := receiver.ad.Locate(ctx, agent.NativeRef{Home: receiver.home, NativeID: tid})
				if err != nil || !found || len(located.Files) != 1 {
					t.Fatalf("step %d locate: %+v %v", step, located, err)
				}
				path = filepath.Join(receiver.home, filepath.FromSlash(located.Files[0].Path))
				got, err := os.ReadFile(path)
				if err != nil || !bytes.Equal(got, want) {
					t.Fatalf("step %d native bytes differ from the model: %v", step, err)
				}
				if _, changed, err := receiver.eng.Checkpoint(ctx, receiver.ad, bs[0].BindingID, "", session.CaptureAuto); err != nil || changed {
					t.Fatalf("step %d post-install checkpoint: changed=%v %v", step, changed, err)
				}
				line := []byte(fmt.Sprintf("{\"type\":\"event_msg\",\"payload\":{\"type\":\"agent_message\",\"message\":\"seed-%d-step-%d-device-%d\"}}\n", seed, step, next))
				want = append(want, line...)
				if err := os.WriteFile(path, want, 0600); err != nil {
					t.Fatal(err)
				}
				previous := current.RevisionID
				var changed bool
				current, changed, err = receiver.eng.Checkpoint(ctx, receiver.ad, bs[0].BindingID, "", session.CaptureAuto)
				if err != nil || !changed || current.ParentRevisionID != previous {
					t.Fatalf("step %d continuation: changed=%v parent=%s want=%s %v", step, changed, current.ParentRevisionID, previous, err)
				}
				if step%4 == 3 {
					if err := receiver.st.Rebuild(ctx); err != nil {
						t.Fatal(err)
					}
					graph, err := receiver.st.Graph(sid)
					if err != nil || len(graph.Heads()) != 1 || graph.Heads()[0] != current.RevisionID {
						t.Fatalf("step %d rebuild lost the effective head: %v", step, err)
					}
				}
				reader, err := receiver.st.SnapshotReader(ctx, current.NativeSnapshotRef)
				if err != nil {
					t.Fatal(err)
				}
				rc, err := reader.Open(ctx, located.Files[0].Path)
				if err != nil {
					t.Fatal(err)
				}
				actual, readErr := io.ReadAll(rc)
				rc.Close()
				if readErr != nil || !bytes.Equal(actual, want) {
					t.Fatalf("step %d snapshot differs from native bytes: %v", step, readErr)
				}
				owner = next
			}
		})
	}
}
