package sync

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"session-memory/internal/agent"
	"session-memory/internal/config"
	"session-memory/internal/engine"
	"session-memory/internal/errcode"
	"session-memory/internal/session"
	"session-memory/internal/source/codex"
	"session-memory/internal/source/workbuddy"
	"session-memory/internal/store"
)

func TestRegressionFreeThreadLockRestore(t *testing.T) {
	a, b, _ := newPair(t)
	ctx := context.Background()
	tid, _ := a.put(501, 2)
	_, rev, err := a.eng.Track(ctx, a.ad, agent.NativeRef{Home: a.home, NativeID: tid}, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = a.sync.Publish(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err = b.sync.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	lockdir := filepath.Join(b.home, "thread-writer-locks")
	os.MkdirAll(lockdir, 0700)
	if err = os.WriteFile(filepath.Join(lockdir, tid+".lock"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err = b.restore(t, rev.SessionID, engine.RestoreOptions{ClientClosed: true}); err != nil {
		t.Fatalf("restore with an existing FREE thread lock must succeed: %v", err)
	}
}

type reviewHookSnapshot struct {
	agent.SnapshotReader
	opens int
	hook  func()
}

func (r *reviewHookSnapshot) Open(ctx context.Context, p string) (io.ReadCloser, error) {
	r.opens++
	if r.opens == 2 {
		r.hook()
	}
	return r.SnapshotReader.Open(ctx, p)
}

func TestRegressionBaselineChangesBeforeLock(t *testing.T) {
	a, b, _ := newPair(t)
	ctx := context.Background()
	tid, p := a.put(505, 2)
	bind, rev1, err := a.eng.Track(ctx, a.ad, agent.NativeRef{Home: a.home, NativeID: tid}, "")
	if err != nil {
		t.Fatal(err)
	}
	a.sync.Publish(ctx)
	b.sync.Reconcile(ctx)
	plan, _, err := b.restore(t, rev1.SessionID, engine.RestoreOptions{ClientClosed: true})
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(b.home, plan.Writes[0].Path)
	original, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(p, append(original, []byte("{\"type\":\"event_msg\",\"payload\":{\"message\":\"remote-new\"}}\n")...), 0600)
	rev2, _, err := a.eng.Checkpoint(ctx, a.ad, bind.BindingID, "", session.CaptureCheckpoint)
	if err != nil {
		t.Fatal(err)
	}
	a.sync.Publish(ctx)
	b.sync.Reconcile(ctx)
	src, err := b.st.SnapshotReader(ctx, rev2.NativeSnapshotRef)
	if err != nil {
		t.Fatal(err)
	}
	local := []byte("{\"type\":\"event_msg\",\"payload\":{\"message\":\"UNSYNCED-LOCAL\"}}\n")
	hook := &reviewHookSnapshot{SnapshotReader: src, hook: func() {
		f, e := os.OpenFile(target, os.O_APPEND|os.O_WRONLY, 0600)
		if e != nil {
			t.Fatal(e)
		}
		f.Write(local)
		f.Close()
	}}
	os.MkdirAll(filepath.Join(b.home, "thread-writer-locks"), 0700)
	in := agent.WriteInput{SessionID: rev2.SessionID, RevisionID: rev2.RevisionID, Mode: agent.ModeNative, Source: hook, SourceProfile: rev2.AgentProfile, Target: rev2.AgentProfile, TargetState: agent.TargetState{Home: b.home}}
	w := &codex.Writer{}
	pl, err := w.Plan(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := b.st.Begin(ctx, store.KindInstall, rev2.SessionID, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = w.Apply(ctx, in, pl, tx); err != nil {
		if errcode.CodeOf(err) != errcode.BaselineChanged {
			t.Fatalf("expected baseline_changed, got %v", err)
		}
		got, readErr := os.ReadFile(target)
		if readErr != nil || !bytes.Contains(got, []byte("UNSYNCED-LOCAL")) {
			t.Fatal("refusal lost the local append")
		}
		return
	}
	got, _ := os.ReadFile(target)
	if !bytes.Contains(got, []byte("UNSYNCED-LOCAL")) {
		t.Fatal("install passed but overwrote local data appended between baseline check and locking")
	}
}

func TestRegressionNativeRestoreRefusesDifferentAgent(t *testing.T) {
	a, b, _ := newPair(t)
	ctx := context.Background()
	p := filepath.Join(a.home, "projects", "sample", "wb-review.jsonl")
	os.MkdirAll(filepath.Dir(p), 0700)
	os.WriteFile(p, []byte("{\"id\":\"m1\",\"type\":\"message\",\"role\":\"user\",\"content\":\"hello\"}\n"), 0600)
	cfg := config.DefaultConfig()
	cfg.Agents.WorkBuddy.Home = a.home
	ad := workbuddy.New(cfg)
	_, rev, err := a.eng.Track(ctx, ad, agent.NativeRef{Home: a.home, NativeID: "wb-review"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = a.sync.Publish(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err = b.sync.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	_, rec, err := b.restore(t, rev.SessionID, engine.RestoreOptions{ClientClosed: true})
	if err == nil {
		t.Fatalf("native Codex restore accepted WorkBuddy data: receipt=%+v", rec)
	}
}

func TestRegressionPullRevisionFailureRetried(t *testing.T) {
	a, b, _ := newPair(t)
	ctx := context.Background()
	tid, _ := a.put(502, 2)
	_, rev, err := a.eng.Track(ctx, a.ad, agent.NativeRef{Home: a.home, NativeID: tid}, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = a.sync.Publish(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err = b.st.DB().Exec(`CREATE TRIGGER review_failure BEFORE INSERT ON revisions BEGIN SELECT RAISE(FAIL, 'simulated revision write failure'); END;`); err != nil {
		t.Fatal(err)
	}
	if _, err = b.sync.Reconcile(ctx); err == nil {
		t.Fatal("expected transient revision error")
	}
	if _, err = b.st.DB().Exec(`DROP TRIGGER review_failure`); err != nil {
		t.Fatal(err)
	}
	if _, err = b.sync.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	revs, err := b.st.Revisions(rev.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(revs) != 1 {
		t.Fatalf("manifest was marked applied but its revision was lost: revisions=%d", len(revs))
	}
}

// Manifest names carry a zero-padded decimal sequence; fmt.Sscan reads a
// leading zero as octal ("000000000010" became 8, "000000000009" became 0).
func TestParseManifestSeqIsDecimal(t *testing.T) {
	for in, want := range map[string]int64{"000000000001": 1, "000000000008": 8, "000000000009": 9, "000000000010": 10, "000000000017": 17, "000000000100": 100} {
		got, err := parseManifestSeq(in)
		if err != nil || got != want {
			t.Errorf("%s -> %d %v, want %d", in, got, err, want)
		}
	}
	if _, err := parseManifestSeq("0000000000x"); err == nil {
		t.Error("garbage was accepted")
	}
}
