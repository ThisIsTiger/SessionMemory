package sync

import (
	"bytes"
	"context"
	"io"
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
	"session-memory/internal/source/workbuddy"
	"session-memory/internal/store"
	"session-memory/internal/transport"
	"session-memory/internal/vault"
)

// A computer whose local state is gone but whose agent homes are still there:
// a fresh store and device identity over an existing home and the shared vault.
func nodeAt(t *testing.T, v *vault.Vault, shared, home string) *node {
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
	cfg := config.DefaultConfig()
	cfg.Agents.Codex.Home = home
	cfg.Agents.WorkBuddy.Home = home
	tr := transport.NewFolder(shared)
	tr.Settle = 5 * time.Millisecond
	return &node{t: t, root: root, home: home, st: st, ad: codex.New(cfg),
		eng:  &engine.Engine{Store: st, DeviceID: info.DeviceID},
		sync: New(st, tr, v, root, info)}
}

func claimPair(t *testing.T) (a, b *node, v *vault.Vault, shared string) {
	t.Helper()
	k, _ := vault.GenerateKey()
	v, _ = vault.New(session.NewVaultID(), k)
	shared = t.TempDir()
	return nodeAt(t, v, shared, t.TempDir()), nodeAt(t, v, shared, t.TempDir()), v, shared
}

func (n *node) claim(ix *engine.ClaimIndex, id string) (session.Binding, session.Revision, bool, error) {
	return n.eng.Claim(context.Background(), n.ad, agent.NativeRef{Home: n.home, NativeID: id}, ix)
}

// the index a setup builds: the vault's list read from the manifests, the small
// objects fetched on demand
func (n *node) index() *engine.ClaimIndex {
	return &engine.ClaimIndex{Load: n.sync.PeekRevisions, FetchSnapshot: n.sync.FetchSnapshot, FetchState: n.sync.FetchRepoState}
}

func countFiles(t *testing.T, dir string) int {
	t.Helper()
	n := 0
	filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			n++
		}
		return nil
	})
	return n
}

func sessionSet(t *testing.T, n *node) map[string]bool {
	t.Helper()
	ids, err := n.st.SessionIDs()
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]bool{}
	for _, id := range ids {
		out[id] = true
	}
	return out
}

func appendLine(t *testing.T, p, line string) {
	t.Helper()
	f, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(line + "\n"); err != nil {
		t.Fatal(err)
	}
}

const moreRecord = `{"timestamp":"2026-09-01T02:00:00.000Z","type":"event_msg","payload":{"type":"agent_message","message":"more"}}`

// Both computers lose their local state. Each finds its sessions in the vault:
// the ones it published itself and the ones installed from the other one.
// Nothing new is made or uploaded, and every sync afterwards is clean.
func TestClaimAfterResetLinksInsteadOfDuplicating(t *testing.T) {
	a, b, v, shared := claimPair(t)
	ctx := context.Background()
	t1, _ := a.put(1, 3)
	t2, p2 := a.put(2, 2)
	var bound []session.Binding
	for _, id := range []string{t1, t2} {
		bd, _, err := a.eng.Track(ctx, a.ad, agent.NativeRef{Home: a.home, NativeID: id}, "")
		if err != nil {
			t.Fatal(err)
		}
		bound = append(bound, bd)
	}
	appendLine(t, p2, moreRecord) // the second session has two revisions
	if _, changed, err := a.eng.Checkpoint(ctx, a.ad, bound[1].BindingID, "", session.CaptureAuto); err != nil || !changed {
		t.Fatalf("second revision: %v %v", changed, err)
	}
	if _, err := a.sync.Publish(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := b.sync.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	for _, bd := range bound { // B installs both from A
		if _, _, err := b.restore(t, bd.SessionID, engine.RestoreOptions{ClientClosed: true}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := b.sync.Publish(ctx); err != nil { // B's receipts
		t.Fatal(err)
	}
	want := map[string]bool{bound[0].SessionID: true, bound[1].SessionID: true}
	objects := countFiles(t, filepath.Join(shared, "objects"))

	for name, old := range map[string]*node{"A": a, "B": b} {
		fresh := nodeAt(t, v, shared, old.home)
		ix := fresh.index()
		for i, id := range []string{t1, t2} {
			bd, rev, claimed, err := fresh.claim(ix, id)
			if err != nil || !claimed {
				t.Fatalf("%s: native %s was not linked to its vault session: claimed=%v err=%v", name, id, claimed, err)
			}
			if _, err := fresh.st.LoadRevision(bd.InstalledRevisionID); err != nil {
				t.Fatalf("%s: the revision the binding names is not in the store: %v", name, err)
			}
			if bd.SessionID != bound[i].SessionID || bd.InstalledRevisionID != rev.RevisionID || bd.BaseRevisionID != rev.RevisionID ||
				bd.State != session.StateActive || bd.InstallMode != session.InstallNative || bd.NativeSessionID != id || bd.LocalCwd == "" {
				t.Fatalf("%s: binding %+v", name, bd)
			}
		}
		// nothing was made: no revision of its own, no object uploaded
		if un, _ := fresh.st.UnpublishedRevisions(fresh.eng.DeviceID); len(un) != 0 {
			t.Fatalf("%s: claiming made %d revision(s)", name, len(un))
		}
		if _, err := fresh.sync.Publish(ctx); err != nil {
			t.Fatalf("%s: publish: %v", name, err)
		}
		if n := countFiles(t, filepath.Join(shared, "objects")); n != objects {
			t.Fatalf("%s: objects in the cloud went from %d to %d", name, objects, n)
		}
		// the normal round that follows (capture, publish, pull) is clean
		bs, _ := fresh.st.Bindings("")
		for _, bd := range bs {
			if _, changed, err := fresh.eng.Checkpoint(ctx, fresh.ad, bd.BindingID, "", session.CaptureAuto); err != nil || changed {
				t.Fatalf("%s: checkpoint after claim: changed=%v err=%v", name, changed, err)
			}
		}
		if _, err := fresh.sync.Reconcile(ctx); err != nil {
			t.Fatalf("%s: reconcile: %v", name, err)
		}
		got := sessionSet(t, fresh)
		if len(got) != len(want) {
			t.Fatalf("%s: %d sessions, want %d: %v", name, len(got), len(want), got)
		}
		for id := range want {
			if !got[id] {
				t.Fatalf("%s: session %s missing", name, id)
			}
		}
		if un, _ := fresh.st.UnpublishedRevisions(fresh.eng.DeviceID); len(un) != 0 {
			t.Fatalf("%s: unpublished revisions after the round: %d", name, len(un))
		}
		for _, id := range []string{t1, t2} { // and nothing needs attention: the head is installed
			var sid string
			for i, x := range []string{t1, t2} {
				if x == id {
					sid = bound[i].SessionID
				}
			}
			if _, _, err := fresh.restore(t, sid, engine.RestoreOptions{ClientClosed: true}); err == nil || errcode.CodeOf(err) != errcode.NotFound {
				t.Fatalf("%s: restore of an up-to-date linked session: %v", name, err)
			}
		}
	}
}

// The native is older than the vault's head: it is linked at the revision it
// holds and the next install fast-forwards it.
func TestClaimOlderThanTheHeadThenFastForward(t *testing.T) {
	a, b, v, shared := claimPair(t)
	ctx := context.Background()
	tid, ap := a.put(3, 2)
	bd, rev1, err := a.eng.Track(ctx, a.ad, agent.NativeRef{Home: a.home, NativeID: tid}, "")
	if err != nil {
		t.Fatal(err)
	}
	a.sync.Publish(ctx)
	b.sync.Reconcile(ctx)
	if _, _, err := b.restore(t, bd.SessionID, engine.RestoreOptions{ClientClosed: true}); err != nil {
		t.Fatal(err)
	}
	// A moves on; B's native stays at revision 1
	appendLine(t, ap, moreRecord)
	rev2, changed, err := a.eng.Checkpoint(ctx, a.ad, bd.BindingID, "", session.CaptureAuto)
	if err != nil || !changed {
		t.Fatalf("checkpoint: %v %v", changed, err)
	}
	if _, err := a.sync.Publish(ctx); err != nil {
		t.Fatal(err)
	}

	fresh := nodeAt(t, v, shared, b.home)
	got, rev, claimed, err := fresh.claim(fresh.index(), tid)
	if err != nil || !claimed {
		t.Fatalf("claim: %v %v", claimed, err)
	}
	if rev.RevisionID != rev1.RevisionID || got.InstalledRevisionID != rev1.RevisionID {
		t.Fatalf("linked at %s, want the revision the native holds (%s)", got.InstalledRevisionID, rev1.RevisionID)
	}
	if _, err := fresh.sync.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	g, _ := fresh.st.Graph(bd.SessionID)
	if h := g.Heads(); len(h) != 1 || h[0] != rev2.RevisionID || !g.IsAncestorOrEqual(got.InstalledRevisionID, rev2.RevisionID) {
		t.Fatalf("heads %v", h)
	}
	if _, rec, err := fresh.restore(t, bd.SessionID, engine.RestoreOptions{ClientClosed: true}); err != nil || rec == nil {
		t.Fatalf("fast-forward of a linked session: %v", err)
	}
	if now, _ := os.ReadFile(filepath.Join(b.home, "sessions", "2026", "09", "01", "rollout-2026-09-01T01-46-53-"+tid+".jsonl")); !bytes.HasSuffix(now, []byte(moreRecord+"\n")) {
		t.Fatal("the native session was not brought to the head")
	}
	nb, _ := fresh.st.Bindings(bd.SessionID)
	if len(nb) != 1 || nb[0].InstalledRevisionID != rev2.RevisionID {
		t.Fatalf("binding after the fast-forward: %+v", nb)
	}
}

// Everything that is not exactly a revision of the session is tracked as before,
// never linked: nothing is claimed, nothing is written, the files are not touched.
func TestClaimRefusesWhatIsNotExactlyARevision(t *testing.T) {
	a, _, v, shared := claimPair(t)
	ctx := context.Background()
	tid, ap := a.put(4, 3)
	if _, _, err := a.eng.Track(ctx, a.ad, agent.NativeRef{Home: a.home, NativeID: tid}, ""); err != nil {
		t.Fatal(err)
	}
	a.sync.Publish(ctx)
	orig, _ := os.ReadFile(ap)
	cases := map[string]func() []byte{
		"unpublished progress":      func() []byte { return append(append([]byte{}, orig...), []byte(moreRecord+"\n")...) },
		"same size, other content":  func() []byte { return bytes.Replace(orig, []byte("a"), []byte("b"), 1) },
		"partial last record":       func() []byte { return append(append([]byte{}, orig...), []byte(moreRecord[:40])...) },
		"cut at a record boundary":  func() []byte { return orig[:bytes.LastIndexByte(orig[:len(orig)-1], '\n')+1] },
		"cut in the middle of text": func() []byte { return orig[:len(orig)/2] },
		"empty":                     func() []byte { return nil },
		"garbage":                   func() []byte { return []byte("not json at all\n") },
		"tampered, same length":     func() []byte { b := append([]byte{}, orig...); b[len(b)/2] ^= 1; return b },
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			if bytes.Equal(content(), orig) {
				t.Skip("fixture produced the original")
			}
			fresh := nodeAt(t, v, shared, a.home)
			if err := os.WriteFile(ap, content(), 0600); err != nil {
				t.Fatal(err)
			}
			defer os.WriteFile(ap, orig, 0600)
			before := countFiles(t, filepath.Join(fresh.root, "store", "objects"))
			bd, _, claimed, err := fresh.claim(fresh.index(), tid)
			if err != nil || claimed || bd.BindingID != "" {
				t.Fatalf("claimed=%v err=%v binding=%+v", claimed, err, bd)
			}
			if bs, _ := fresh.st.Bindings(""); len(bs) != 0 {
				t.Fatalf("a binding was made: %+v", bs)
			}
			if left, _ := filepath.Glob(filepath.Join(fresh.root, "journal", "*.json")); len(left) != 0 {
				t.Fatalf("journal left behind: %v", left)
			}
			// only the snapshot and the repo state of the revision looked at, never the native's bytes
			if n := countFiles(t, filepath.Join(fresh.root, "store", "objects")); n-before > 2 {
				t.Fatalf("the check stored %d object(s)", n-before)
			}
			if now, _ := os.ReadFile(ap); !bytes.Equal(now, content()) {
				t.Fatal("the native file was modified")
			}
			if _, _, err := fresh.eng.Track(ctx, fresh.ad, agent.NativeRef{Home: a.home, NativeID: tid}, ""); name != "empty" && name != "garbage" && err != nil {
				t.Fatalf("the fallback (Track) failed: %v", err)
			}
		})
	}
	// a different native id never matches, whatever the content
	t.Run("other native id", func(t *testing.T) {
		fresh := nodeAt(t, v, shared, a.home)
		other := "019aaaaa-0000-7000-8000-000000000001"
		if _, _, claimed, err := fresh.claim(fresh.index(), other); claimed || err == nil {
			t.Fatalf("an unknown native session: claimed=%v err=%v", claimed, err)
		}
		p := filepath.Join(a.home, "sessions", "2026", "09", "01", "rollout-2026-09-01T01-46-53-"+other+".jsonl")
		if err := os.WriteFile(p, orig, 0600); err != nil {
			t.Fatal(err)
		}
		if _, _, claimed, err := fresh.claim(fresh.index(), other); claimed || err != nil {
			t.Fatalf("same bytes under another id: claimed=%v err=%v", claimed, err)
		}
	})
}

// A session that is already bound here is left alone: no change, no receipt.
func TestClaimLeavesAnAlreadyBoundSessionAlone(t *testing.T) {
	a, b, _, _ := claimPair(t)
	ctx := context.Background()
	tid, _ := a.put(5, 2)
	bd, _, err := a.eng.Track(ctx, a.ad, agent.NativeRef{Home: a.home, NativeID: tid}, "")
	if err != nil {
		t.Fatal(err)
	}
	a.sync.Publish(ctx)
	b.sync.Reconcile(ctx)
	if _, _, err := b.restore(t, bd.SessionID, engine.RestoreOptions{ClientClosed: true}); err != nil {
		t.Fatal(err)
	}
	for name, n := range map[string]*node{"tracked here": a, "installed here": b} {
		before, _ := n.st.Bindings("")
		outbox, _ := n.sync.Dev.Outbox()
		got, _, claimed, err := n.claim(n.index(), tid)
		if err != nil || claimed || got.BindingID != "" {
			t.Fatalf("%s: claimed=%v err=%v", name, claimed, err)
		}
		after, _ := n.st.Bindings("")
		outboxAfter, _ := n.sync.Dev.Outbox()
		if len(after) != len(before) || len(after) != 1 || after[0].BindingID != before[0].BindingID || len(outboxAfter) != len(outbox) {
			t.Fatalf("%s: bindings %d -> %d, outbox %d -> %d", name, len(before), len(after), len(outbox), len(outboxAfter))
		}
		if _, _, err := n.eng.Track(ctx, n.ad, agent.NativeRef{Home: n.home, NativeID: tid}, ""); errcode.CodeOf(err) != errcode.Conflict {
			t.Fatalf("%s: the fallback must still say already tracked: %v", name, err)
		}
	}
	// a session bound on this device is not taken a second time under another home
	other := t.TempDir()
	p := filepath.Join(a.home, "sessions", "2026", "09", "01", "rollout-2026-09-01T01-46-53-"+tid+".jsonl")
	data, _ := os.ReadFile(p)
	dst := filepath.Join(other, "sessions", "2026", "09", "01", filepath.Base(p))
	os.MkdirAll(filepath.Dir(dst), 0700)
	os.WriteFile(dst, data, 0600)
	if _, _, claimed, err := a.eng.Claim(ctx, a.ad, agent.NativeRef{Home: other, NativeID: tid}, a.index()); claimed || err != nil {
		t.Fatalf("a copy in another home while the session is bound: claimed=%v err=%v", claimed, err)
	}
}

// Reading the vault's list and linking a session downloads the manifests and
// two small objects per revision looked at, never a segment.
func TestClaimDownloadsNoSegments(t *testing.T) {
	a, _, v, shared := claimPair(t)
	ctx := context.Background()
	tid, _ := a.put(6, 6)
	_, rev, err := a.eng.Track(ctx, a.ad, agent.NativeRef{Home: a.home, NativeID: tid}, "")
	if err != nil {
		t.Fatal(err)
	}
	a.sync.Publish(ctx)
	snap, err := a.st.LoadSnapshot(ctx, rev.NativeSnapshotRef)
	if err != nil {
		t.Fatal(err)
	}
	fresh := nodeAt(t, v, shared, a.home)
	rec := &recordingTransport{Transport: fresh.sync.T}
	fresh.sync.T = rec
	if _, _, claimed, err := fresh.claim(fresh.index(), tid); err != nil || !claimed {
		t.Fatalf("claim: %v %v", claimed, err)
	}
	segs := map[string]bool{}
	for _, o := range snap.ObjectRefs() {
		segs[objectPath(o)] = true
	}
	small := 0
	for _, p := range rec.gets {
		if segs[p] {
			t.Fatalf("a segment object was downloaded: %s", p)
		}
		if strings.HasPrefix(p, "objects/") {
			small++
		}
	}
	if small != 2 { // the snapshot and the repo state
		t.Fatalf("%d object downloads: %v", small, rec.gets)
	}
	for _, o := range snap.ObjectRefs() {
		if fresh.st.HasObject(o) {
			// the capture of the native files stores segments locally; those are
			// this computer's own objects, not downloads
			continue
		}
	}
	// the normal pull afterwards still gets everything
	if res, err := fresh.sync.Reconcile(ctx); err != nil || res.Applied == 0 {
		t.Fatalf("reconcile after a claim: %+v %v", res, err)
	}
	// and reading the list changes nothing in the store
	third := nodeAt(t, v, shared, t.TempDir())
	revs, err := third.sync.PeekRevisions(ctx)
	if err != nil || len(revs) != 1 || revs[0].RevisionID != rev.RevisionID {
		t.Fatalf("peek: %v %v", revs, err)
	}
	if ids, _ := third.st.SessionIDs(); len(ids) != 0 {
		t.Fatalf("peek imported %v", ids)
	}
	if n := countFiles(t, filepath.Join(third.root, "store", "objects")); n != 0 {
		t.Fatalf("peek stored %d objects", n)
	}
}

type recordingTransport struct {
	transport.Transport
	gets []string
}

func (r *recordingTransport) Get(ctx context.Context, path string) (io.ReadCloser, error) {
	r.gets = append(r.gets, path)
	return r.Transport.Get(ctx, path)
}

// Two vault sessions record the same native id (a copy an earlier setup made):
// the older one is the original and is the one linked.
func TestClaimPrefersTheOriginalOfTwoCopies(t *testing.T) {
	a, _, v, shared := claimPair(t)
	ctx := context.Background()
	tid, _ := a.put(7, 2)
	orig, _, err := a.eng.Track(ctx, a.ad, agent.NativeRef{Home: a.home, NativeID: tid}, "")
	if err != nil {
		t.Fatal(err)
	}
	a.sync.Publish(ctx)
	time.Sleep(20 * time.Millisecond)
	second := nodeAt(t, v, shared, a.home) // a computer that tracked the same native as new
	dup, _, err := second.eng.Track(ctx, second.ad, agent.NativeRef{Home: a.home, NativeID: tid}, "")
	if err != nil {
		t.Fatal(err)
	}
	second.sync.Publish(ctx)
	if dup.SessionID == orig.SessionID {
		t.Fatal("fixture: no copy")
	}
	fresh := nodeAt(t, v, shared, a.home)
	got, _, claimed, err := fresh.claim(fresh.index(), tid)
	if err != nil || !claimed || got.SessionID != orig.SessionID {
		t.Fatalf("claimed=%v err=%v session=%s, want the original %s", claimed, err, got.SessionID, orig.SessionID)
	}
}

// ---- WorkBuddy: several artifacts, side files, an empty blob ----

const wbID = "0b1c2d3e-4f50-4a61-8b72-93a4b5c6d7e8"

type wbFiles map[string]string

func wbFixture() wbFiles {
	dir := "projects/Users-me-work-app/"
	return wbFiles{
		dir + wbID + ".jsonl": `{"type":"user","sessionId":"` + wbID + `","timestamp":1790000000000,"cwd":"/Users/me/work/app","message":"hello"}` + "\n" +
			`{"type":"assistant","sessionId":"` + wbID + `","timestamp":1790000001000,"message":"hi"}` + "\n",
		dir + wbID + ".meta.json":                     `{"cwd":"/Users/me/work/app"}`,
		dir + wbID + ".file-rollback.ndjson":          `{"file":"a.txt"}` + "\n",
		dir + wbID + "/subagents/agent-1.jsonl":       `{"type":"user","timestamp":1790000000500,"message":"sub"}` + "\n",
		dir + wbID + "/tool-results/out.txt":          "tool output\nline two\n",
		dir + wbID + "/tool-results/empty.txt":        "",
		dir + wbID + "/tool-results/nested/deep.json": `{"k":1}`,
	}
}

func (f wbFiles) write(t *testing.T, home string) {
	t.Helper()
	for rel, content := range f {
		p := filepath.Join(home, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
}

func wbNode(t *testing.T, v *vault.Vault, shared, home string) *node {
	n := nodeAt(t, v, shared, home)
	cfg := config.DefaultConfig()
	cfg.Agents.WorkBuddy.Home = home
	n.ad = workbuddy.New(cfg)
	return n
}

func TestClaimWorkBuddySessionWithSideFilesAndAnEmptyBlob(t *testing.T) {
	k, _ := vault.GenerateKey()
	v, _ := vault.New(session.NewVaultID(), k)
	shared := t.TempDir()
	ctx := context.Background()
	homeA, homeB := t.TempDir(), t.TempDir()
	fix := wbFixture()
	fix.write(t, homeA)
	fix.write(t, homeB) // what an install of the same revision leaves on the other computer
	a := wbNode(t, v, shared, homeA)
	bd, rev, err := a.eng.Track(ctx, a.ad, agent.NativeRef{Home: homeA, NativeID: wbID}, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.sync.Publish(ctx); err != nil {
		t.Fatal(err)
	}
	snap, _ := a.st.LoadSnapshot(ctx, rev.NativeSnapshotRef)
	if len(snap.Artifacts) != len(fix) {
		t.Fatalf("the snapshot holds %d artifacts, the fixture %d", len(snap.Artifacts), len(fix))
	}
	objects := countFiles(t, filepath.Join(shared, "objects"))

	for name, home := range map[string]string{"the computer that published it": homeA, "the one it was installed on": homeB} {
		fresh := wbNode(t, v, shared, home)
		got, r, claimed, err := fresh.claim(fresh.index(), wbID)
		if err != nil || !claimed {
			t.Fatalf("%s: claimed=%v err=%v", name, claimed, err)
		}
		if got.SessionID != bd.SessionID || r.RevisionID != rev.RevisionID || len(got.Artifacts) != len(fix) {
			t.Fatalf("%s: binding %+v", name, got)
		}
		if st := got.Artifacts["projects/Users-me-work-app/"+wbID+"/tool-results/empty.txt"]; st.Kind != session.KindBlob || st.Size != 0 {
			t.Fatalf("%s: the empty blob is not part of the binding: %+v", name, st)
		}
		if _, changed, err := fresh.eng.Checkpoint(ctx, fresh.ad, got.BindingID, "", session.CaptureAuto); err != nil || changed {
			t.Fatalf("%s: checkpoint: changed=%v err=%v", name, changed, err)
		}
		if _, err := fresh.sync.Publish(ctx); err != nil {
			t.Fatal(err)
		}
		if n := countFiles(t, filepath.Join(shared, "objects")); n != objects {
			t.Fatalf("%s: objects %d -> %d", name, objects, n)
		}
	}

	// any difference in any artifact, or an extra or missing one, is not the revision
	mutations := map[string]func(home string){
		"meta.json changed": func(h string) {
			os.WriteFile(filepath.Join(h, "projects/Users-me-work-app/"+wbID+".meta.json"), []byte(`{"cwd":"/Users/me/work/app2"}`), 0600)
		},
		"blob changed": func(h string) {
			os.WriteFile(filepath.Join(h, "projects/Users-me-work-app/"+wbID+"/tool-results/out.txt"), []byte("tool output\nline twX\n"), 0600)
		},
		"empty blob got bytes": func(h string) {
			os.WriteFile(filepath.Join(h, "projects/Users-me-work-app/"+wbID+"/tool-results/empty.txt"), []byte("x"), 0600)
		},
		"empty blob missing": func(h string) {
			os.Remove(filepath.Join(h, "projects/Users-me-work-app/"+wbID+"/tool-results/empty.txt"))
		},
		"extra tool result": func(h string) {
			os.WriteFile(filepath.Join(h, "projects/Users-me-work-app/"+wbID+"/tool-results/new.txt"), []byte("n"), 0600)
		},
		"subagent grew": func(h string) {
			appendLine(t, filepath.Join(h, "projects/Users-me-work-app/"+wbID+"/subagents/agent-1.jsonl"), `{"type":"user","message":"more"}`)
		},
		"subagent missing": func(h string) {
			os.Remove(filepath.Join(h, "projects/Users-me-work-app/"+wbID+"/subagents/agent-1.jsonl"))
		},
		"main stream grew": func(h string) {
			appendLine(t, filepath.Join(h, "projects/Users-me-work-app/"+wbID+".jsonl"), `{"type":"user","timestamp":1790000002000,"message":"more"}`)
		},
		"main stream truncated": func(h string) {
			os.WriteFile(filepath.Join(h, "projects/Users-me-work-app/"+wbID+".jsonl"), []byte(fix["projects/Users-me-work-app/"+wbID+".jsonl"][:60]), 0600)
		},
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			home := t.TempDir()
			fix.write(t, home)
			mutate(home)
			fresh := wbNode(t, v, shared, home)
			got, _, claimed, err := fresh.claim(fresh.index(), wbID)
			if err != nil || claimed || got.BindingID != "" {
				t.Fatalf("claimed=%v err=%v", claimed, err)
			}
			if bs, _ := fresh.st.Bindings(""); len(bs) != 0 {
				t.Fatalf("binding made: %+v", bs)
			}
		})
	}
}

// A native that is ahead of the vault's newest revision cannot be any older
// revision either: one small download settles it, not one per revision.
func TestClaimOfANativeAheadOfTheVaultReadsOneSnapshot(t *testing.T) {
	a, _, v, shared := claimPair(t)
	ctx := context.Background()
	tid, ap := a.put(8, 2)
	bd, _, err := a.eng.Track(ctx, a.ad, agent.NativeRef{Home: a.home, NativeID: tid}, "")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		appendLine(t, ap, moreRecord)
		if _, changed, err := a.eng.Checkpoint(ctx, a.ad, bd.BindingID, "", session.CaptureAuto); err != nil || !changed {
			t.Fatalf("revision %d: %v %v", i, changed, err)
		}
	}
	a.sync.Publish(ctx)
	appendLine(t, ap, moreRecord) // progress the vault does not have

	fresh := nodeAt(t, v, shared, a.home)
	rec := &recordingTransport{Transport: fresh.sync.T}
	fresh.sync.T = rec
	if _, _, claimed, err := fresh.claim(fresh.index(), tid); claimed || err != nil {
		t.Fatalf("claimed=%v err=%v", claimed, err)
	}
	objects := 0
	for _, p := range rec.gets {
		if strings.HasPrefix(p, "objects/") {
			objects++
		}
	}
	if objects != 1 {
		t.Fatalf("%d objects downloaded to see that the native is ahead: %v", objects, rec.gets)
	}
}
