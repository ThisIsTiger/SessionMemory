package sync

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"session-memory/internal/agent"
	"session-memory/internal/engine"
	"session-memory/internal/errcode"
	"session-memory/internal/platform"
	"session-memory/internal/session"
	"session-memory/internal/source/codex"
)

func (n *node) restore(t *testing.T, sid string, opt engine.RestoreOptions) (agent.Plan, *agent.Receipt, error) {
	t.Helper()
	opt.Home = n.home
	return n.eng.Restore(context.Background(), n.ad.(*codex.Adapter), sid, opt)
}

func TestRestorePingPong(t *testing.T) {
	a, b, _ := newPair(t)
	ctx := context.Background()
	tid, ap := a.put(1, 3)
	bind, rev1, _ := a.eng.Track(ctx, a.ad, agent.NativeRef{Home: a.home, NativeID: tid}, "")
	a.sync.Publish(ctx)
	b.sync.Reconcile(ctx)

	// refuse without the explicit "client closed" statement
	if _, _, err := b.restore(t, rev1.SessionID, engine.RestoreOptions{}); errcode.CodeOf(err) != errcode.ClientRunning {
		t.Fatalf("restore without confirmation: %v", err)
	}
	plan, _, err := b.restore(t, rev1.SessionID, engine.RestoreOptions{PlanOnly: true})
	if err != nil || len(plan.Writes) != 1 || plan.Writes[0].Op != "create" || plan.Digest == "" {
		t.Fatalf("plan: %+v %v", plan, err)
	}
	if _, err := os.Stat(filepath.Join(b.home, plan.Writes[0].Path)); err == nil {
		t.Fatal("--plan wrote a file")
	}
	_, rec, err := b.restore(t, rev1.SessionID, engine.RestoreOptions{ClientClosed: true})
	if err != nil || rec == nil || rec.Import == nil {
		t.Fatalf("restore: %v", err)
	}
	orig, _ := os.ReadFile(ap)
	bp := filepath.Join(b.home, plan.Writes[0].Path)
	got, _ := os.ReadFile(bp)
	if !bytes.Equal(got, orig) {
		t.Fatal("restored file differs from the origin")
	}
	// the thread is discoverable and already tracked: no spurious revision
	bs, _ := b.st.Bindings(rev1.SessionID)
	if len(bs) != 1 || bs[0].InstalledRevisionID != rev1.RevisionID || bs[0].ImportReceipt == nil {
		t.Fatalf("binding after restore: %+v", bs)
	}
	if _, changed, err := b.eng.Checkpoint(ctx, b.ad, bs[0].BindingID, "", session.CaptureAuto); err != nil || changed {
		t.Fatalf("checkpoint right after restore: changed=%v err=%v", changed, err)
	}
	// restoring again is a no-op
	if _, _, err := b.restore(t, rev1.SessionID, engine.RestoreOptions{ClientClosed: true}); err == nil {
		t.Error("a second restore of the same revision must say so")
	}

	// B continues the conversation; A picks it up (the A2 scenario)
	more := append(append([]byte(nil), got...), []byte(`{"timestamp":"2026-09-01T03:00:00.000Z","type":"event_msg","payload":{"type":"agent_message","message":"from home"}}`+"\n")...)
	os.WriteFile(bp, more, 0600)
	rev2, changed, err := b.eng.Checkpoint(ctx, b.ad, bs[0].BindingID, "", session.CaptureAuto)
	if err != nil || !changed || rev2.ParentRevisionID != rev1.RevisionID {
		t.Fatalf("B checkpoint: changed=%v err=%v parent=%s", changed, err, rev2.ParentRevisionID)
	}
	b.sync.Publish(ctx)
	a.sync.Reconcile(ctx)
	if _, _, err := a.restore(t, rev1.SessionID, engine.RestoreOptions{ClientClosed: true}); err != nil {
		t.Fatalf("A fast-forward restore: %v", err)
	}
	final, _ := os.ReadFile(ap)
	if !bytes.Equal(final, more) {
		t.Fatal("A's file was not brought up to B's version")
	}
	ab, _ := a.st.Bindings(rev1.SessionID)
	if ab[0].InstalledRevisionID != rev2.RevisionID || ab[0].BindingID != bind.BindingID {
		t.Errorf("A binding: %+v", ab[0])
	}
	// no spurious revision on A either
	if _, changed, _ := a.eng.Checkpoint(ctx, a.ad, bind.BindingID, "", session.CaptureAuto); changed {
		t.Error("A produced a revision right after fast-forward")
	}
}

func TestRestoreMapsPathsAndLeavesOriginAlone(t *testing.T) {
	a, b, _ := newPair(t)
	ctx := context.Background()
	tid, _ := a.put(2, 2)
	_, rev1, _ := a.eng.Track(ctx, a.ad, agent.NativeRef{Home: a.home, NativeID: tid}, "")
	a.sync.Publish(ctx)
	b.sync.Reconcile(ctx)
	pm := agent.PathMap{Rules: []agent.PathRule{{From: "/work/app", To: "/Users/me/code/application"}}}
	plan, _, err := b.restore(t, rev1.SessionID, engine.RestoreOptions{ClientClosed: true, PathMap: pm})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(filepath.Join(b.home, plan.Writes[0].Path))
	if !strings.Contains(string(got), `"cwd":"/Users/me/code/application"`) || strings.Contains(string(got), `"cwd":"/work/app"`) {
		t.Fatal("cwd was not mapped in session_meta/turn_context")
	}
	// tool-call text that merely mentions the old path is untouched
	if !strings.Contains(string(got), `\"workdir\":\"/work/app\"`) {
		t.Error("historical text was rewritten")
	}
	// the mapped thread keeps working: checkpoint is clean, appends become children
	bs, _ := b.st.Bindings(rev1.SessionID)
	if _, changed, err := b.eng.Checkpoint(ctx, b.ad, bs[0].BindingID, "", session.CaptureAuto); err != nil || changed {
		t.Fatalf("checkpoint after mapped restore: changed=%v err=%v", changed, err)
	}
}

func TestRestoreRefusesRunningClientAndConflict(t *testing.T) {
	a, b, _ := newPair(t)
	ctx := context.Background()
	tid, _ := a.put(3, 1)
	_, rev1, _ := a.eng.Track(ctx, a.ad, agent.NativeRef{Home: a.home, NativeID: tid}, "")
	a.sync.Publish(ctx)
	b.sync.Reconcile(ctx)

	// a Codex holding the thread's writer lock blocks the install even when confirmed
	locks := filepath.Join(b.home, "thread-writer-locks")
	os.MkdirAll(locks, 0700)
	lf, _ := platform.Touch(filepath.Join(locks, tid+".lock"))
	platform.LockExclusiveNonBlocking(lf)
	defer lf.Close()
	if _, _, err := b.restore(t, rev1.SessionID, engine.RestoreOptions{ClientClosed: true}); errcode.CodeOf(err) != errcode.ClientRunning {
		t.Fatalf("held thread lock: %v", err)
	}
	if _, err := os.Stat(filepath.Join(b.home, "sessions")); err == nil {
		entries, _ := filepath.Glob(filepath.Join(b.home, "sessions", "*", "*", "*", "*.jsonl"))
		if len(entries) != 0 {
			t.Fatalf("files were written despite the refusal: %v", entries)
		}
	}
	platform.Unlock(lf)
}
