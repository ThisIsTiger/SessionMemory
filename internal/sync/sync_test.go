package sync

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"session-memory/internal/agent"
	"session-memory/internal/config"
	"session-memory/internal/device"
	"session-memory/internal/engine"
	"session-memory/internal/errcode"
	"session-memory/internal/session"
	"session-memory/internal/source/codex"
	"session-memory/internal/store"
	"session-memory/internal/testutil/codexgen"
	"session-memory/internal/transport"
	"session-memory/internal/vault"
)

type node struct {
	t    *testing.T
	root string
	home string
	st   *store.FS
	eng  *engine.Engine
	ad   agent.Adapter
	sync *Sync
}

func newNode(t *testing.T, v *vault.Vault, shared string) *node {
	t.Helper()
	root := t.TempDir()
	st, err := store.Open(root, v)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	info, err := device.At(root).Init(v.ID(), session.FormatTime(time.Now()))
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	cfg := config.DefaultConfig()
	cfg.Agents.Codex.Home = home
	tr := transport.NewFolder(shared)
	tr.Settle = 5 * time.Millisecond
	return &node{t: t, root: root, home: home, st: st, ad: codex.New(cfg),
		eng:  &engine.Engine{Store: st, DeviceID: info.DeviceID},
		sync: New(st, tr, v, root, info)}
}

func newPair(t *testing.T) (*node, *node, string) {
	k, _ := vault.GenerateKey()
	v, _ := vault.New(session.NewVaultID(), k)
	shared := t.TempDir()
	return newNode(t, v, shared), newNode(t, v, shared), shared
}

func (n *node) put(seed int64, turns int) (string, string) {
	tid := codexgen.ThreadIDFor(seed)
	p := filepath.Join(n.home, "sessions", "2026", "09", "01", "rollout-2026-09-01T01-46-53-"+tid+".jsonl")
	os.MkdirAll(filepath.Dir(p), 0700)
	os.WriteFile(p, codexgen.Rollout(codexgen.Options{Seed: seed, Turns: turns, ToolCalls: 1, Cwd: n.home}), 0600)
	return tid, p
}

func TestAtoBRoundTripAndReceipts(t *testing.T) {
	a, b, _ := newPair(t)
	ctx := context.Background()
	tid, p := a.put(1, 3)
	bind, rev1, err := a.eng.Track(ctx, a.ad, agent.NativeRef{Home: a.home, NativeID: tid}, "")
	if err != nil {
		t.Fatal(err)
	}
	n, err := a.sync.Publish(ctx)
	if err != nil || n != 1 {
		t.Fatalf("publish n=%d err=%v", n, err)
	}
	if un, _ := a.st.UnpublishedRevisions(a.eng.DeviceID); len(un) != 0 {
		t.Fatal("revision still unpublished after publish")
	}

	res, err := b.sync.Reconcile(ctx)
	if err != nil || res.Applied != 1 || res.Revisions != 1 {
		t.Fatalf("reconcile: %+v %v", res, err)
	}
	g, _ := b.st.Graph(rev1.SessionID)
	if h := g.Heads(); len(h) != 1 || h[0] != rev1.RevisionID {
		t.Fatalf("B heads = %v", h)
	}
	// B can read A's content byte for byte
	snap, err := b.st.LoadSnapshot(ctx, rev1.NativeSnapshotRef)
	if err != nil {
		t.Fatal(err)
	}
	var got bytes.Buffer
	for _, sg := range snap.Artifacts[0].Segments {
		rc, err := b.st.OpenObject(ctx, sg.Object, vault.TypeSegment, nil)
		if err != nil {
			t.Fatal(err)
		}
		buf := new(bytes.Buffer)
		buf.ReadFrom(rc)
		got.Write(buf.Bytes())
	}
	orig, _ := os.ReadFile(p)
	if !bytes.Equal(got.Bytes(), orig) {
		t.Fatal("B's copy differs from A's native file")
	}
	// reconcile again: idempotent
	if r2, _ := b.sync.Reconcile(ctx); r2.Applied != 0 {
		t.Errorf("second reconcile applied %d", r2.Applied)
	}
	// B publishes its received receipt; A sees it
	if _, err := b.sync.Publish(ctx); err != nil {
		t.Fatal(err)
	}
	if r3, err := a.sync.Reconcile(ctx); err != nil || r3.Applied != 1 {
		t.Fatalf("A reconcile of receipt: %+v %v", r3, err)
	}
	var cnt int
	a.st.DB().QueryRow(`SELECT count(*) FROM receipts WHERE revision_id=? AND status='received'`, rev1.RevisionID).Scan(&cnt)
	if cnt != 1 {
		t.Errorf("receipts = %d", cnt)
	}

	// incremental: A appends, B receives only the new segment
	more := append(append([]byte(nil), orig...), []byte(`{"timestamp":"2026-09-01T02:00:00.000Z","type":"event_msg","payload":{"type":"agent_message","message":"y"}}`+"\n")...)
	os.WriteFile(p, more, 0600)
	rev2, changed, err := a.eng.Checkpoint(ctx, a.ad, bind.BindingID, "", session.CaptureAuto)
	if err != nil || !changed {
		t.Fatal(err)
	}
	a.sync.Publish(ctx)
	if r, err := b.sync.Reconcile(ctx); err != nil || r.Applied != 1 {
		t.Fatalf("B incremental: %+v %v", r, err)
	}
	g, _ = b.st.Graph(rev1.SessionID)
	if h := g.Heads(); len(h) != 1 || h[0] != rev2.RevisionID {
		t.Fatalf("heads after incremental = %v", h)
	}
}

func TestTamperedManifestIsQuarantined(t *testing.T) {
	a, b, shared := newPair(t)
	ctx := context.Background()
	tid, _ := a.put(2, 2)
	a.eng.Track(ctx, a.ad, agent.NativeRef{Home: a.home, NativeID: tid}, "")
	a.sync.Publish(ctx)
	files, _ := filepath.Glob(filepath.Join(shared, "manifests", "*", "*.manifest"))
	if len(files) != 1 {
		t.Fatalf("manifests = %v", files)
	}
	raw, _ := os.ReadFile(files[0])
	raw[len(raw)-1] ^= 1
	// keep the name consistent with the (tampered) content so only AEAD can catch it
	newName := filepath.Join(filepath.Dir(files[0]), session.ManifestFileName(1, sha(raw)))
	os.Remove(files[0])
	os.WriteFile(newName, raw, 0600)
	res, err := b.sync.Reconcile(ctx)
	if err != nil || res.Applied != 0 || res.Quarantined != 1 {
		t.Fatalf("tampered manifest: %+v %v", res, err)
	}
}

func TestWrongVaultCannotRead(t *testing.T) {
	a, _, shared := newPair(t)
	ctx := context.Background()
	tid, _ := a.put(3, 1)
	a.eng.Track(ctx, a.ad, agent.NativeRef{Home: a.home, NativeID: tid}, "")
	a.sync.Publish(ctx)
	k2, _ := vault.GenerateKey()
	v2, _ := vault.New(session.NewVaultID(), k2)
	c := newNode(t, v2, shared)
	res, _ := c.sync.Reconcile(ctx)
	if res.Applied != 0 || res.Quarantined != 1 {
		t.Errorf("a device with another vault key imported data: %+v", res)
	}
}

func TestMissingObjectsStayPending(t *testing.T) {
	a, b, shared := newPair(t)
	ctx := context.Background()
	tid, _ := a.put(4, 2)
	a.eng.Track(ctx, a.ad, agent.NativeRef{Home: a.home, NativeID: tid}, "")
	a.sync.Publish(ctx)
	objs, _ := filepath.Glob(filepath.Join(shared, "objects", "*", "*.obj"))
	hidden := objs[0] + ".hidden"
	os.Rename(objs[0], hidden)
	res, _ := b.sync.Reconcile(ctx)
	if res.Applied != 0 || res.PendingObjs != 1 {
		t.Fatalf("with a missing object: %+v", res)
	}
	if g, _ := b.st.Graph("any"); len(g.Heads()) != 0 {
		t.Error("a half-imported revision became visible")
	}
	os.Rename(hidden, objs[0])
	if res, err := b.sync.Reconcile(ctx); err != nil || res.Applied != 1 {
		t.Fatalf("after the object arrived: %+v %v", res, err)
	}
}

func TestCloneDetection(t *testing.T) {
	a, _, _ := newPair(t)
	ctx := context.Background()
	tid, p := a.put(5, 1)
	bind, _, _ := a.eng.Track(ctx, a.ad, agent.NativeRef{Home: a.home, NativeID: tid}, "")
	a.sync.Publish(ctx)
	orig, _ := os.ReadFile(p)
	os.WriteFile(p, append(orig, []byte(`{"timestamp":"t","type":"event_msg","payload":{}}`+"\n")...), 0600)
	a.eng.Checkpoint(ctx, a.ad, bind.BindingID, "", session.CaptureAuto)
	// simulate restoring an old backup: local sequence goes back to 0
	a.sync.Dev.SetLastPublished(0)
	_, err := a.sync.Publish(ctx)
	if errcode.CodeOf(err) != errcode.DeviceIDConflict {
		t.Fatalf("clone not detected: %v", err)
	}
	info, err := a.sync.Dev.Regenerate()
	if err != nil {
		t.Fatal(err)
	}
	a.sync.Info = info
	a.eng.DeviceID = info.DeviceID
	if err := a.sync.CheckDeviceID(ctx); err != nil {
		t.Errorf("a regenerated id must be accepted: %v", err)
	}
}

func TestManifestChainGapIsPending(t *testing.T) {
	a, b, shared := newPair(t)
	ctx := context.Background()
	tid, p := a.put(6, 1)
	bind, _, _ := a.eng.Track(ctx, a.ad, agent.NativeRef{Home: a.home, NativeID: tid}, "")
	a.sync.Publish(ctx)
	orig, _ := os.ReadFile(p)
	os.WriteFile(p, append(orig, []byte(`{"timestamp":"t","type":"event_msg","payload":{}}`+"\n")...), 0600)
	a.eng.Checkpoint(ctx, a.ad, bind.BindingID, "", session.CaptureAuto)
	a.sync.Publish(ctx)
	first, _ := filepath.Glob(filepath.Join(shared, "manifests", "*", "000000000001-*"))
	hidden := first[0] + ".hold"
	os.Rename(first[0], hidden)
	res, _ := b.sync.Reconcile(ctx)
	if res.Applied != 0 || res.PendingPrev == 0 {
		t.Fatalf("gap: %+v", res)
	}
	os.Rename(hidden, first[0])
	if res, _ := b.sync.Reconcile(ctx); res.Applied != 2 {
		t.Fatalf("after the gap closed: %+v", res)
	}
	_ = strings.TrimSpace
}
