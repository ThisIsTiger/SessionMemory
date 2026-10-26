package workbuddy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"session-memory/internal/agent"
	"session-memory/internal/canonical"
	"session-memory/internal/config"
	"session-memory/internal/convert"
	"session-memory/internal/session"
)

const sentinel = "SECRET-SENTINEL-123"

type memSink struct{ n int }

func (m *memSink) PutSegment(_ context.Context, k agent.ArtifactKey, s, e int64, r io.Reader) (session.SegmentRef, error) {
	b, _ := io.ReadAll(r)
	h := sha256.Sum256(b)
	m.n++
	return session.SegmentRef{Start: s, End: e, RawHash: hex.EncodeToString(h[:]), Object: "obj_" + strings.Repeat("0", 56) + hex8(m.n)}, nil
}
func (m *memSink) PutBlob(_ context.Context, k agent.ArtifactKey, size int64, r io.Reader) ([]session.SegmentRef, error) {
	b, _ := io.ReadAll(r)
	if len(b) == 0 {
		return nil, nil
	}
	ref, _ := m.PutSegment(nil, k, 0, int64(len(b)), bytes.NewReader(b))
	return []session.SegmentRef{ref}, nil
}
func hex8(n int) string { return strings.Repeat("0", 7) + string("0123456789abcdef"[n%16]) }

func line(m map[string]any) string { b, _ := json.Marshal(m); return string(b) + "\n" }

func writeSession(t *testing.T, home, dir, id string, extra bool) {
	root := filepath.Join(home, "projects", dir)
	os.MkdirAll(root, 0700)
	var b strings.Builder
	b.WriteString(line(map[string]any{"type": "message", "id": "m1", "role": "user", "sessionId": id, "cwd": "/w/app", "timestamp": 1760000000000,
		"content": []any{map[string]any{"type": "input_text", "text": "hello " + sentinel}}}))
	b.WriteString(line(map[string]any{"type": "reasoning", "id": "r1", "parentId": "m1", "content": []any{}}))
	b.WriteString(line(map[string]any{"type": "function_call", "id": "f1", "parentId": "r1", "callId": "c1", "name": "shell", "arguments": `{"cmd":"ls"}`}))
	b.WriteString(line(map[string]any{"type": "function_call_result", "id": "f1r", "parentId": "f1", "callId": "c1", "name": "shell", "output": "a\nb", "status": "completed"}))
	b.WriteString(line(map[string]any{"type": "message", "id": "m2", "parentId": "f1r", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": "done"}}}))
	b.WriteString(line(map[string]any{"type": "ai-title", "aiTitle": "AI " + sentinel}))
	b.WriteString(line(map[string]any{"type": "custom-title", "customTitle": "Mine"}))
	os.WriteFile(filepath.Join(root, id+".jsonl"), []byte(b.String()), 0600)
	if extra {
		os.WriteFile(filepath.Join(root, id+".meta.json"), []byte(`{"cwd":"/from/meta"}`), 0600)
		os.MkdirAll(filepath.Join(root, id, "subagents"), 0700)
		os.WriteFile(filepath.Join(root, id, "subagents", "agent-1.jsonl"), []byte(line(map[string]any{"type": "message", "role": "user"})), 0600)
		os.MkdirAll(filepath.Join(root, id, "tool-results"), 0700)
		os.WriteFile(filepath.Join(root, id, "tool-results", "big.txt"), []byte("tool output"), 0600)
	}
}

func newAd(t *testing.T) (*Adapter, string) {
	home := t.TempDir()
	cfg := config.DefaultConfig()
	cfg.Agents.WorkBuddy.Home = home
	return New(cfg), home
}

func TestDiscoverCaptureAndSoftDelete(t *testing.T) {
	ad, home := newAd(t)
	writeSession(t, home, "Users-me-app", "sess-1", true)
	writeSession(t, home, "Users-me-app", "sess-2", false)
	// sess-2 is soft-deleted in the registry
	db, _ := sql.Open("sqlite", filepath.Join(home, "workbuddy.db"))
	db.Exec(`CREATE TABLE sessions(id TEXT PRIMARY KEY, deleted_at TEXT)`)
	db.Exec(`INSERT INTO sessions VALUES('sess-1', NULL), ('sess-2', '2026-10-01')`)
	db.Close()

	list, err := ad.Discover(context.Background(), agent.DiscoverOptions{})
	if err != nil || len(list) != 1 || list[0].Ref.NativeID != "sess-1" {
		t.Fatalf("discover: %+v %v", list, err)
	}
	if list[0].Cwd != "/w/app" || list[0].Title != "Mine" {
		t.Errorf("cwd/title: %q %q", list[0].Cwd, list[0].Title)
	}
	sink := &memSink{}
	res, err := ad.Capture(context.Background(), agent.NativeRef{Home: home, NativeID: "sess-1"}, agent.Cursor{}, sink)
	if err != nil || res.State != agent.CaptureOK || !res.Changed {
		t.Fatalf("capture: %+v %v", res, err)
	}
	var paths []string
	for p := range res.Cursor.Artifacts {
		paths = append(paths, p)
	}
	if len(paths) != 4 {
		t.Fatalf("artifacts: %v", paths)
	}
	// unchanged -> nothing new, including the blobs
	res2, _ := ad.Capture(context.Background(), agent.NativeRef{Home: home, NativeID: "sess-1"}, res.Cursor, &memSink{})
	if res2.Changed {
		t.Error("recapture reported changes")
	}
	// a changed blob is re-captured
	os.WriteFile(filepath.Join(home, "projects", "Users-me-app", "sess-1", "tool-results", "big.txt"), []byte("different"), 0600)
	res3, _ := ad.Capture(context.Background(), agent.NativeRef{Home: home, NativeID: "sess-1"}, res.Cursor, &memSink{})
	if !res3.Changed {
		t.Error("changed blob not detected")
	}
	bad, _ := ad.VerifyBinding(context.Background(), agent.NativeRef{Home: home, NativeID: "sess-1"}, res.Cursor.Artifacts, false)
	if len(bad) != 1 || !strings.Contains(bad[0], "tool-results") {
		t.Errorf("verify: %v", bad)
	}
	if _, ok, _ := ad.Locate(context.Background(), agent.NativeRef{Home: home, NativeID: "nope"}); ok {
		t.Error("located a missing session")
	}
}

func TestProbeIsSafeAndMetaCwdFallback(t *testing.T) {
	ad, home := newAd(t)
	writeSession(t, home, "d", "s1", true)
	// no cwd in records: falls back to meta.json
	p := filepath.Join(home, "projects", "d", "s2.jsonl")
	os.WriteFile(p, []byte(line(map[string]any{"type": "message", "role": "user"})), 0600)
	os.WriteFile(filepath.Join(home, "projects", "d", "s2.meta.json"), []byte(`{"cwd":"/from/meta"}`), 0600)
	list, _ := ad.Discover(context.Background(), agent.DiscoverOptions{NativeIDs: []string{"s2"}})
	if len(list) != 1 || list[0].Cwd != "/from/meta" {
		t.Fatalf("meta fallback: %+v", list)
	}
	res, err := ad.Probe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(res)
	if strings.Contains(string(b), sentinel) || strings.Contains(string(b), "s1") {
		t.Fatalf("probe leaked: %s", b)
	}
	if res.Compatibility != "ok" || res.Homes[0].RecordTypes["function_call"] == 0 {
		t.Errorf("probe: %+v", res)
	}
	if !IsScene("@agent:run") || IsScene("hello") {
		t.Error("scene predicate wrong")
	}
	if _, err := ad.Writer().Plan(context.Background(), agent.WriteInput{}); err == nil {
		t.Error("the writer must stay unsupported")
	}
}

func TestExtractActivePathAndBranches(t *testing.T) {
	var b strings.Builder
	b.WriteString(line(map[string]any{"type": "message", "id": "m1", "role": "user", "content": "first"}))
	b.WriteString(line(map[string]any{"type": "message", "id": "a1", "parentId": "m1", "role": "assistant", "content": "old answer"}))
	b.WriteString(line(map[string]any{"type": "message", "id": "a2", "parentId": "m1", "role": "assistant", "content": "new answer"}))
	b.WriteString(line(map[string]any{"type": "message", "id": "u2", "parentId": "a2", "role": "user", "content": "<system-reminder>x"}))
	b.WriteString(line(map[string]any{"type": "message", "id": "u3", "parentId": "u2", "role": "user", "content": "follow up"}))
	items, err := extractStream(io.NopCloser(strings.NewReader(b.String())), "p.jsonl", 0)
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]canonical.Item{}
	for _, it := range items {
		byID[it.ID] = it
	}
	if byID["a1"].Branch == "" || byID["a2"].Branch != "" || byID["m1"].Branch != "" || byID["u3"].Branch != "" {
		t.Errorf("branches: a1=%q a2=%q", byID["a1"].Branch, byID["a2"].Branch)
	}
	if !byID["m1"].Human || byID["u2"].Human || !byID["u3"].Human {
		t.Error("human predicate wrong")
	}
	out, _ := convert.Convert(items, convert.Options{})
	if c := out.Report.Counts["message"]; c.Kept != 3 || c.Dropped != 2 || out.Report.Dropped["branch"] != 1 {
		t.Errorf("conversion of the tree: %+v %v", c, out.Report.Dropped)
	}
}
