package engine

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"session-memory/internal/agent"
	"session-memory/internal/config"
	"session-memory/internal/session"
	"session-memory/internal/source/codex"
	"session-memory/internal/store"
	"session-memory/internal/testutil/codexgen"
	"session-memory/internal/vault"
)

type rig struct {
	t     *testing.T
	home  string
	root  string
	st    *store.FS
	eng   *Engine
	ad    agent.Adapter
	vault *vault.Vault
}

func newRig(t *testing.T) *rig {
	t.Helper()
	k, _ := vault.GenerateKey()
	v, _ := vault.New(session.NewVaultID(), k)
	root := t.TempDir()
	st, err := store.Open(root, v)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	home := t.TempDir()
	cfg := config.DefaultConfig()
	cfg.Agents.Codex.Home = home
	return &rig{t: t, home: home, root: root, st: st, vault: v, ad: codex.New(cfg),
		eng: &Engine{Store: st, DeviceID: session.NewDeviceID()}}
}

func (r *rig) put(seed int64, turns int) (string, string) {
	tid := codexgen.ThreadIDFor(seed)
	p := filepath.Join(r.home, "sessions", "2026", "09", "01", "rollout-2026-09-01T01-46-53-"+tid+".jsonl")
	os.MkdirAll(filepath.Dir(p), 0700)
	os.WriteFile(p, codexgen.Rollout(codexgen.Options{Seed: seed, Turns: turns, ToolCalls: 1}), 0600)
	return tid, p
}

func (r *rig) readSnapshotFile(rev session.Revision, path string) []byte {
	snap, err := r.st.LoadSnapshot(context.Background(), rev.NativeSnapshotRef)
	if err != nil {
		r.t.Fatal(err)
	}
	var out bytes.Buffer
	for _, a := range snap.Artifacts {
		if a.Path != path {
			continue
		}
		for _, sg := range a.Segments {
			rc, err := r.st.OpenObject(context.Background(), sg.Object, vault.TypeSegment, nil)
			if err != nil {
				r.t.Fatal(err)
			}
			b, _ := io.ReadAll(rc)
			out.Write(b)
		}
	}
	return out.Bytes()
}

func TestStoreImplementsInterface(t *testing.T) {
	var _ store.Store = (*store.FS)(nil)
}

func TestTrackCheckpointAndAppend(t *testing.T) {
	r := newRig(t)
	tid, p := r.put(1, 3)
	ref := agent.NativeRef{Home: r.home, NativeID: tid}
	b, rev1, err := r.eng.Track(context.Background(), r.ad, ref, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := rev1.Validate(); err != nil || rev1.ParentRevisionID != "" || rev1.OriginNativeID != tid {
		t.Fatalf("root revision wrong: %+v %v", rev1, err)
	}
	orig, _ := os.ReadFile(p)
	logic := "sessions/2026/09/01/" + filepath.Base(p)
	if !bytes.Equal(r.readSnapshotFile(rev1, logic), orig) {
		t.Fatal("snapshot content differs from the native file")
	}
	if _, _, err := r.eng.Track(context.Background(), r.ad, ref, ""); err == nil {
		t.Error("tracked the same native session twice")
	}
	if _, changed, err := r.eng.Checkpoint(context.Background(), r.ad, b.BindingID, "", session.CaptureAuto); err != nil || changed {
		t.Fatalf("unchanged checkpoint: changed=%v err=%v", changed, err)
	}
	more := append(append([]byte(nil), orig...), []byte(`{"timestamp":"2026-09-01T02:00:00.000Z","type":"event_msg","payload":{"type":"agent_message","message":"x"}}`+"\n")...)
	os.WriteFile(p, more, 0600)
	rev2, changed, err := r.eng.Checkpoint(context.Background(), r.ad, b.BindingID, "", session.CaptureAuto)
	if err != nil || !changed || rev2.ParentRevisionID != rev1.RevisionID {
		t.Fatalf("append: changed=%v err=%v parent=%s", changed, err, rev2.ParentRevisionID)
	}
	if !bytes.Equal(r.readSnapshotFile(rev2, logic), more) {
		t.Fatal("second snapshot content wrong")
	}
	g, _ := r.st.Graph(rev1.SessionID)
	if h := g.Heads(); len(h) != 1 || h[0] != rev2.RevisionID {
		t.Errorf("heads = %v", h)
	}
	un, _ := r.st.UnpublishedRevisions(r.eng.DeviceID)
	if len(un) != 2 {
		t.Errorf("unpublished = %d", len(un))
	}
	s1, _ := r.st.LoadSnapshot(context.Background(), rev1.NativeSnapshotRef)
	s2, _ := r.st.LoadSnapshot(context.Background(), rev2.NativeSnapshotRef)
	if s1.Artifacts[0].Segments[0].Object != s2.Artifacts[0].Segments[0].Object {
		t.Error("existing segment was re-encrypted instead of reused")
	}
}

func TestRebuildRestoresViews(t *testing.T) {
	r := newRig(t)
	tid, _ := r.put(2, 2)
	b, rev, err := r.eng.Track(context.Background(), r.ad, agent.NativeRef{Home: r.home, NativeID: tid}, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := r.st.Rebuild(context.Background()); err != nil {
		t.Fatal(err)
	}
	g, err := r.st.Graph(rev.SessionID)
	if err != nil || len(g.Heads()) != 1 {
		t.Fatalf("graph after rebuild: %v %v", err, g)
	}
	bs, _ := r.st.Bindings(b.SessionID)
	if len(bs) != 1 {
		t.Errorf("bindings after rebuild = %d", len(bs))
	}
}

func TestUniqueActiveBinding(t *testing.T) {
	r := newRig(t)
	tid, _ := r.put(3, 1)
	b, _, err := r.eng.Track(context.Background(), r.ad, agent.NativeRef{Home: r.home, NativeID: tid}, "")
	if err != nil {
		t.Fatal(err)
	}
	dup := b
	dup.BindingID = session.NewBindingID()
	dup.NativeSessionID = "other"
	if err := r.st.SaveBinding(nil, dup); err == nil {
		t.Error("a second active binding for the same session and agent was accepted")
	}
}

func TestTamperedObjectIsRejected(t *testing.T) {
	r := newRig(t)
	tid, _ := r.put(4, 1)
	_, rev, _ := r.eng.Track(context.Background(), r.ad, agent.NativeRef{Home: r.home, NativeID: tid}, "")
	snap, _ := r.st.LoadSnapshot(context.Background(), rev.NativeSnapshotRef)
	ref := snap.Artifacts[0].Segments[0].Object
	p := filepath.Join(r.root, "store", "objects", ref[4:6], ref[4:]+".obj")
	b, _ := os.ReadFile(p)
	b[len(b)-1] ^= 1
	os.WriteFile(p, b, 0600)
	if _, err := r.st.OpenObject(context.Background(), ref, vault.TypeSegment, nil); err == nil {
		t.Error("tampered object opened")
	}
	if err := r.st.PutSealed(context.Background(), ref, bytes.NewReader([]byte("junk"))); err == nil {
		t.Error("PutSealed accepted bytes whose hash differs from the reference")
	}
}

// A binding the store refuses (a second active one for the same session and
// agent) leaves no file behind: the files are the truth, not the view.
func TestRejectedBindingLeavesNoFile(t *testing.T) {
	r := newRig(t)
	tid, _ := r.put(990, 1)
	b, _, err := r.eng.Track(context.Background(), r.ad, agent.NativeRef{Home: r.home, NativeID: tid}, "")
	if err != nil {
		t.Fatal(err)
	}
	dup := b
	dup.BindingID, dup.NativeSessionID = session.NewBindingID(), "duplicate"
	if err := r.st.SaveBinding(nil, dup); err == nil {
		t.Fatal("setup: duplicate must be rejected")
	}
	bs, err := r.st.Bindings(b.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(bs) != 1 {
		t.Fatalf("rejected SaveBinding persisted a second active binding: %d", len(bs))
	}
	// saving the existing binding again, or a retired one, still works
	if err := r.st.SaveBinding(nil, b); err != nil {
		t.Fatal(err)
	}
	dup.State = session.StateSuperseded
	if err := r.st.SaveBinding(nil, dup); err != nil {
		t.Fatalf("a non-active binding is not limited: %v", err)
	}
}
