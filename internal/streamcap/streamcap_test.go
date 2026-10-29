package streamcap

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"session-memory/internal/agent"
	"session-memory/internal/session"
)

// memSink keeps what it was asked to store and can run a hook right after the
// first segment is stored, to rewrite the source "while it is being captured".
type memSink struct {
	hook  func()
	bytes []byte
}

func (s *memSink) PutSegment(_ context.Context, _ agent.ArtifactKey, start, end int64, r io.Reader) (session.SegmentRef, error) {
	b, err := io.ReadAll(r)
	if err != nil {
		return session.SegmentRef{}, err
	}
	s.bytes = append(s.bytes, b...)
	h := sha256.Sum256(b)
	if s.hook != nil {
		s.hook()
		s.hook = nil
	}
	return session.SegmentRef{Start: start, End: end, RawHash: hex.EncodeToString(h[:]), Object: "obj_" + strings.Repeat("a", 64)}, nil
}

func (s *memSink) PutBlob(ctx context.Context, key agent.ArtifactKey, size int64, r io.Reader) ([]session.SegmentRef, error) {
	v, err := s.PutSegment(ctx, key, 0, size, r)
	return []session.SegmentRef{v}, err
}

// A source rewritten (same length) between storing a segment and computing the
// cursor must not be vouched for by the cursor: the stored bytes are the old
// ones, so certifying the new file would hide the rewrite for good.
func TestCaptureStreamRefusesRewriteDuringCapture(t *testing.T) {
	home := t.TempDir()
	p := filepath.Join(home, "a.jsonl")
	if err := os.WriteFile(p, []byte("{\"x\":1}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	sink := &memSink{hook: func() {
		if err := os.WriteFile(p, []byte("{\"x\":2}\n"), 0600); err != nil {
			t.Error(err)
		}
	}}
	files := []File{{Rel: "a.jsonl", Mode: 0600}}
	st, _, _, _, err := CaptureStream(context.Background(), home, "a.jsonl", files, session.ArtifactState{}, sink, Options{})
	if !errors.Is(err, ErrChangedDuringCapture) {
		t.Fatalf("want ErrChangedDuringCapture, got %v (state %v)", err, st)
	}
	if st != nil {
		t.Fatal("a refused capture must not return a state to persist")
	}
	// nothing was persisted, so the next capture starts from scratch and sees x=2
	next := &memSink{}
	st, _, changed, _, err := CaptureStream(context.Background(), home, "a.jsonl", files, session.ArtifactState{}, next, Options{})
	if err != nil || !changed || st == nil {
		t.Fatalf("recapture: %v changed=%v", err, changed)
	}
	if string(next.bytes) != "{\"x\":2}\n" {
		t.Fatalf("recapture stored %q", next.bytes)
	}
}

// An append retains the committed segments and accepts a consistent full
// prefix, including the newly stored range.
func TestCaptureStreamAppendUnchangedSource(t *testing.T) {
	home := t.TempDir()
	p := filepath.Join(home, "a.jsonl")
	if err := os.WriteFile(p, []byte("{\"x\":1}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	files := []File{{Rel: "a.jsonl", Mode: 0600}}
	st, _, _, _, err := CaptureStream(context.Background(), home, "a.jsonl", files, session.ArtifactState{}, &memSink{}, Options{})
	if err != nil || st == nil {
		t.Fatalf("first capture: %v", err)
	}
	f, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString("{\"x\":2}\n")
	f.Close()
	st2, _, changed, _, err := CaptureStream(context.Background(), home, "a.jsonl", files, *st, &memSink{}, Options{})
	if err != nil || !changed || st2 == nil || len(st2.Segments) != 2 {
		t.Fatalf("append capture: %v changed=%v state=%v", err, changed, st2)
	}
}

// Deep verification must notice a blob whose bytes changed but whose length
// did not; the cheap check only compares sizes.
func TestVerifyArtifactsDeepDetectsEqualLengthBlobChange(t *testing.T) {
	home := t.TempDir()
	p := filepath.Join(home, "blob")
	if err := os.WriteFile(p, []byte("aaaa"), 0600); err != nil {
		t.Fatal(err)
	}
	st, _, err := CaptureBlob(context.Background(), home, File{Rel: "blob", Mode: 0600}, "blob", session.ArtifactState{}, &memSink{})
	if err != nil {
		t.Fatal(err)
	}
	files := map[string][]File{"blob": {{Rel: "blob", Mode: 0600}}}
	states := map[string]session.ArtifactState{"blob": *st}
	if bad, err := VerifyArtifacts(home, files, states, true); err != nil || len(bad) != 0 {
		t.Fatalf("unchanged blob rejected: %v %v", bad, err)
	}
	if err := os.WriteFile(p, []byte("bbbb"), 0600); err != nil {
		t.Fatal(err)
	}
	if bad, err := VerifyArtifacts(home, files, states, false); err != nil || len(bad) != 0 {
		t.Fatalf("the cheap check compares sizes only: %v %v", bad, err)
	}
	bad, err := VerifyArtifacts(home, files, states, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(bad) != 1 {
		t.Fatalf("deep verification accepted altered blob with unchanged length: %v", bad)
	}
}
