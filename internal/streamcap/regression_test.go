package streamcap

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"session-memory/internal/agent"
	"session-memory/internal/jsonl"
	"session-memory/internal/session"
	"session-memory/internal/store"
	"session-memory/internal/vault"
)

// Revalidating only the newly appended range must not certify a rewrite in
// a segment that the previous checkpoint had already committed.
func TestCaptureStreamRefusesRewriteInCommittedPrefix(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	path := filepath.Join(home, "a.jsonl")
	initial := []byte("{\"x\":1}\n")
	if err := os.WriteFile(path, initial, 0600); err != nil {
		t.Fatal(err)
	}
	files := []File{{Rel: "a.jsonl", Mode: 0600}}
	old, _, _, _, err := CaptureStream(ctx, home, "a.jsonl", files, session.ArtifactState{}, &memSink{}, Options{})
	if err != nil || old == nil {
		t.Fatalf("initial capture: %v", err)
	}
	appended := append(bytes.Clone(initial), []byte("{\"tail\":1}\n")...)
	if err := os.WriteFile(path, appended, 0600); err != nil {
		t.Fatal(err)
	}
	sink := &memSink{hook: func() {
		updated := bytes.Replace(appended, []byte(`"x":1`), []byte(`"x":2`), 1)
		if bytes.Equal(updated, appended) {
			t.Fatal("setup did not alter the committed prefix")
		}
		if err := os.WriteFile(path, updated, 0600); err != nil {
			t.Fatal(err)
		}
	}}
	fresh, state, _, _, err := CaptureStream(ctx, home, "a.jsonl", files, *old, sink, Options{})
	if errors.Is(err, ErrChangedDuringCapture) {
		return
	}
	if err != nil || fresh == nil {
		t.Fatalf("incremental capture: %v", err)
	}
	src, err := jsonl.OpenSource(path)
	if err != nil {
		t.Fatal(err)
	}
	consistent, err := VerifySegments(src, fresh.Segments)
	if err != nil {
		t.Fatal(err)
	}
	_, _, retryChanged, _, err := CaptureStream(ctx, home, "a.jsonl", files, *fresh, &memSink{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if !consistent && !retryChanged {
		t.Fatalf("incremental capture certified rewritten old bytes: state=%v, full verification=%v, next capture Changed=%v", state, consistent, retryChanged)
	}
	if !consistent && state == agent.CaptureOK {
		t.Fatal("an inconsistent snapshot was accepted as CaptureOK")
	}
}

// Positive checks for A14 use the real store sink, including a blob that spans
// two encrypted objects. A single-chunk test sink would hide chunking mistakes.
func TestBlobDeepVerifyAtChunkBoundaries(t *testing.T) {
	for _, test := range []struct {
		name string
		size int
	}{{"empty", 0}, {"small", 1}, {"full_chunk", agent.MaxSegmentBytes}, {"cross_chunk", agent.MaxSegmentBytes + 1}} {
		t.Run(test.name, func(t *testing.T) {
			key, err := vault.GenerateKey()
			if err != nil {
				t.Fatal(err)
			}
			v, err := vault.New(session.NewVaultID(), key)
			if err != nil {
				t.Fatal(err)
			}
			st, err := store.Open(t.TempDir(), v)
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			home := t.TempDir()
			path := filepath.Join(home, "blob")
			data := bytes.Repeat([]byte{19}, test.size)
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			state, _, err := CaptureBlob(context.Background(), home, File{Rel: "blob", Mode: 0600}, "blob", session.ArtifactState{}, st.Sink())
			if err != nil {
				t.Fatal(err)
			}
			files := map[string][]File{"blob": {{Rel: "blob", Mode: 0600}}}
			states := map[string]session.ArtifactState{"blob": *state}
			if bad, err := VerifyArtifacts(home, files, states, true); err != nil || len(bad) != 0 {
				t.Fatalf("unchanged: %v %v", bad, err)
			}
			if len(data) == 0 {
				return
			}
			data[len(data)-1] ^= 1
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			if bad, err := VerifyArtifacts(home, files, states, true); err != nil || len(bad) != 1 {
				t.Fatalf("tampered: %v %v", bad, err)
			}
		})
	}
}
