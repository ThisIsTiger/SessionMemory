package sync

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"session-memory/internal/agent"
	"session-memory/internal/engine"
	"session-memory/internal/errcode"
	"session-memory/internal/session"
)

// updatedPair publishes a thread from a, lets b receive it (and, when
// initiallyInstalled, install it), then publishes one more turn from a, so b
// holds a newer revision than what its Codex home has. It returns the thread's
// session ID and the path the next install writes.
func updatedPair(t *testing.T, initiallyInstalled bool) (a, b *node, sid, target string) {
	t.Helper()
	a, b, _ = newPair(t)
	ctx := context.Background()
	tid, ap := a.put(992, 1)
	bind, rev, err := a.eng.Track(ctx, a.ad, agent.NativeRef{Home: a.home, NativeID: tid}, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = a.sync.Publish(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err = b.sync.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if initiallyInstalled {
		p, _, err := b.restore(t, rev.SessionID, engine.RestoreOptions{ClientClosed: true})
		if err != nil {
			t.Fatal(err)
		}
		target = filepath.Join(b.home, p.Writes[0].Path)
	}
	f, err := os.OpenFile(ap, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.WriteString("{\"type\":\"event_msg\",\"payload\":{\"message\":\"REMOTE_NEW\"}}\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if _, changed, err := a.eng.Checkpoint(ctx, a.ad, bind.BindingID, "", session.CaptureAuto); err != nil || !changed {
		t.Fatalf("checkpoint %v %v", changed, err)
	}
	if _, err = a.sync.Publish(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err = b.sync.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if target == "" {
		p, _, err := b.restore(t, rev.SessionID, engine.RestoreOptions{ClientClosed: true, PlanOnly: true})
		if err != nil {
			t.Fatal(err)
		}
		target = filepath.Join(b.home, p.Writes[0].Path)
	}
	return a, b, rev.SessionID, target
}

// Local rollout files the revision does not hold are unpublished progress; an
// install must refuse instead of folding them into the binding cursors, where
// no revision would ever contain them.
func TestRestoreRefusesUnpublishedExtraRollout(t *testing.T) {
	_, b, sid, target := updatedPair(t, true)
	before, err := b.st.Bindings(sid)
	if err != nil || len(before) != 1 {
		t.Fatalf("setup: bindings %v %v", before, err)
	}
	main0, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	name := filepath.Base(target)
	id := name[len("rollout-2026-09-01T01-46-53-") : len(name)-len(".jsonl")]
	extra := filepath.Join(filepath.Dir(target), "rollout-2026-09-01T01-46-53-"+id+"_11111111-1111-4111-8111-111111111111.jsonl")
	const localOnly = "{\"type\":\"event_msg\",\"payload\":{\"message\":\"LOCAL_ONLY\"}}\n"
	if err := os.WriteFile(extra, []byte(localOnly), 0600); err != nil {
		t.Fatal(err)
	}
	// the preview already says so
	if _, _, err := b.restore(t, sid, engine.RestoreOptions{ClientClosed: true, PlanOnly: true}); errcode.CodeOf(err) != errcode.NeedsAction {
		t.Fatalf("plan with an unpublished rollout: %v", err)
	}
	if _, rec, err := b.restore(t, sid, engine.RestoreOptions{ClientClosed: true}); err == nil || rec != nil {
		t.Fatalf("install over an unpublished rollout must be refused: %v", err)
	} else if errcode.CodeOf(err) != errcode.NeedsAction {
		t.Fatalf("refusal code: %v", err)
	}
	if got, err := os.ReadFile(extra); err != nil || string(got) != localOnly {
		t.Fatalf("the extra rollout was touched: %q %v", got, err)
	}
	if got, err := os.ReadFile(target); err != nil || string(got) != string(main0) {
		t.Fatalf("the installed rollout was touched: %v", err)
	}
	after, err := b.st.Bindings(sid)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("a binding or its cursors changed: %v %v", after, err)
	}
	// once the local progress is checkpointed (here: removed) the install goes through
	if err := os.Remove(extra); err != nil {
		t.Fatal(err)
	}
	if _, _, err := b.restore(t, sid, engine.RestoreOptions{ClientClosed: true}); err != nil {
		t.Fatalf("install after the extra rollout is gone: %v", err)
	}
}
