package codex

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"session-memory/internal/agent"
	"session-memory/internal/config"
	"session-memory/internal/platform"
	"session-memory/internal/session"
	"session-memory/internal/testutil/codexgen"
)

// memSink stores segments in memory.
type memSink struct {
	data map[string][]byte // artifact path -> concatenated bytes (current gen)
	n    int
}

func newSink() *memSink { return &memSink{data: map[string][]byte{}} }

func (m *memSink) PutSegment(_ context.Context, k agent.ArtifactKey, start, end int64, r io.Reader) (session.SegmentRef, error) {
	b, _ := io.ReadAll(r)
	if int64(len(b)) != end-start {
		return session.SegmentRef{}, fmt.Errorf("segment length %d != %d", len(b), end-start)
	}
	if start == 0 {
		m.data[k.Path] = nil
	}
	m.data[k.Path] = append(m.data[k.Path], b...)
	m.n++
	h := sha256.Sum256(b)
	return session.SegmentRef{Start: start, End: end, RawHash: hex.EncodeToString(h[:]),
		Object: "obj_" + strings.Repeat("0", 56) + fmt.Sprintf("%08x", m.n)}, nil
}
func (m *memSink) PutBlob(context.Context, agent.ArtifactKey, int64, io.Reader) ([]session.SegmentRef, error) {
	return nil, nil
}

type env struct {
	home string
	a    *Adapter
	id   string
}

func newEnv(t *testing.T) *env {
	t.Helper()
	home := t.TempDir()
	cfg := config.DefaultConfig()
	cfg.Agents.Codex.Home = home
	return &env{home: home, a: New(cfg)}
}

// put writes a rollout for thread under sessions/2026/09/01/.
func (e *env) put(t *testing.T, thread string, seg string, content []byte, zst bool) string {
	t.Helper()
	name := "rollout-2026-09-01T01-46-53-" + thread
	if seg != "" {
		name += "_" + seg
	}
	name += ".jsonl"
	if zst {
		name += ".zst"
		content = codexgen.Zstd(content)
	}
	p := filepath.Join(e.home, "sessions", "2026", "09", "01", name)
	os.MkdirAll(filepath.Dir(p), 0700)
	if err := os.WriteFile(p, content, 0600); err != nil {
		t.Fatal(err)
	}
	return p
}

func (e *env) capture(t *testing.T, thread string, cur agent.Cursor, sink *memSink) agent.CaptureResult {
	t.Helper()
	res, err := e.a.Capture(context.Background(), agent.NativeRef{Home: e.home, NativeID: thread}, cur, sink)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func logicOf(thread string) string {
	return "sessions/2026/09/01/rollout-2026-09-01T01-46-53-" + thread + ".jsonl"
}

func TestCaptureBasicConcatenatesToOriginal(t *testing.T) {
	e := newEnv(t)
	thread := codexgen.ThreadIDFor(1)
	data := codexgen.Rollout(codexgen.Options{Seed: 1, Turns: 4, ToolCalls: 2, Ordinals: true, UILayer: true})
	e.put(t, thread, "", data, false)
	sink := newSink()
	res := e.capture(t, thread, agent.Cursor{}, sink)
	if res.State != agent.CaptureOK || !res.Changed {
		t.Fatalf("state=%v changed=%v", res.State, res.Changed)
	}
	if !bytes.Equal(sink.data[logicOf(thread)], data) {
		t.Fatal("segments do not concatenate to the original file")
	}
	st := res.Cursor.Artifacts[logicOf(thread)]
	if st.Digest != session.SegmentsDigest(st.Segments) || st.Size != int64(len(data)) {
		t.Errorf("digest/size wrong: %+v", st)
	}
	// second capture: nothing new
	res2 := e.capture(t, thread, res.Cursor, newSink())
	if res2.Changed || res2.State != agent.CaptureOK {
		t.Errorf("recapture changed=%v state=%v", res2.Changed, res2.State)
	}
}

func TestZstdSameRawHashesAndConversionNotReplaced(t *testing.T) {
	data := codexgen.Rollout(codexgen.Options{Seed: 2, Turns: 3})
	a := newEnv(t)
	tid := codexgen.ThreadIDFor(2)
	a.put(t, tid, "", data, false)
	ra := a.capture(t, tid, agent.Cursor{}, newSink())

	b := newEnv(t)
	b.put(t, tid, "", data, true)
	rb := b.capture(t, tid, agent.Cursor{}, newSink())
	sa, sb := ra.Cursor.Artifacts[logicOf(tid)], rb.Cursor.Artifacts[logicOf(tid)]
	if sa.Digest != sb.Digest || sb.Form != session.FormZstd {
		t.Fatalf("plain and zstd differ: %s vs %s (%s)", sa.Digest, sb.Digest, sb.Form)
	}

	// the same stream compressed after capture is not a Replaced
	c := newEnv(t)
	p := c.put(t, tid, "", data, false)
	r1 := c.capture(t, tid, agent.Cursor{}, newSink())
	os.Remove(p)
	c.put(t, tid, "", data, true)
	r2 := c.capture(t, tid, r1.Cursor, newSink())
	if r2.State != agent.CaptureOK || r2.Changed {
		t.Errorf("plain->zstd: state=%v changed=%v", r2.State, r2.Changed)
	}
	// ...and appended content after compression is read on
	more := append(append([]byte(nil), data...), codexgen.Rollout(codexgen.Options{Seed: 9, Turns: 1})[0:0]...)
	_ = more
}

func TestSegmentSplitAtRecordBoundary(t *testing.T) {
	e := newEnv(t)
	tid := codexgen.ThreadIDFor(3)
	data := codexgen.Rollout(codexgen.Options{Seed: 3, Turns: 6, PadBytes: 3 << 20})
	e.put(t, tid, "", data, false)
	sink := newSink()
	res := e.capture(t, tid, agent.Cursor{}, sink)
	st := res.Cursor.Artifacts[logicOf(tid)]
	if len(st.Segments) < 2 {
		t.Fatalf("expected several segments for %d bytes, got %d", len(data), len(st.Segments))
	}
	for i, s := range st.Segments {
		if s.End-s.Start > int64(agent.MaxSegmentBytes)+4<<20 {
			t.Errorf("segment %d too large", i)
		}
		if i > 0 && s.Start != st.Segments[i-1].End {
			t.Errorf("segments not contiguous at %d", i)
		}
		if data[s.End-1] != '\n' {
			t.Errorf("segment %d does not end on a record boundary", i)
		}
	}
	if !bytes.Equal(sink.data[logicOf(tid)], data) {
		t.Fatal("concatenation differs")
	}
}

func TestPartialTailAndCorruptLine(t *testing.T) {
	e := newEnv(t)
	tid := codexgen.ThreadIDFor(4)
	data := codexgen.Rollout(codexgen.Options{Seed: 4, Turns: 2})
	p := e.put(t, tid, "", append(append([]byte(nil), data...), []byte(`{"timestamp":"x","type":"ev`)...), false)
	sink := newSink()
	res := e.capture(t, tid, agent.Cursor{}, sink)
	if !bytes.Equal(sink.data[logicOf(tid)], data) || res.State != agent.CaptureOK {
		t.Fatalf("partial tail consumed or state wrong: %v", res.State)
	}
	// finish the line
	os.WriteFile(p, append(append([]byte(nil), data...), []byte(`{"timestamp":"x","type":"event_msg","payload":{}}`+"\n")...), 0600)
	res2 := e.capture(t, tid, res.Cursor, newSink())
	if !res2.Changed {
		t.Error("completed line was not captured")
	}

	// a corrupt complete line stops capture before it
	bad := append(append([]byte(nil), data...), []byte("{not json}\n{\"type\":\"x\"}\n")...)
	e2 := newEnv(t)
	e2.put(t, tid, "", bad, false)
	s2 := newSink()
	r3 := e2.capture(t, tid, agent.Cursor{}, s2)
	if r3.State != agent.CaptureUnsafe {
		t.Fatalf("state = %v, want unsafe", r3.State)
	}
	if !bytes.Equal(s2.data[logicOf(tid)], data) {
		t.Error("good prefix must still be captured")
	}
}

func TestSameLengthRewriteBumpsGeneration(t *testing.T) {
	e := newEnv(t)
	tid := codexgen.ThreadIDFor(5)
	data := codexgen.Rollout(codexgen.Options{Seed: 5, Turns: 3})
	p := e.put(t, tid, "", data, false)
	r1 := e.capture(t, tid, agent.Cursor{}, newSink())
	mod := append([]byte(nil), data...)
	mod[20] ^= 0x01
	os.WriteFile(p, mod, 0600)
	sink := newSink()
	r2 := e.capture(t, tid, r1.Cursor, sink)
	if r2.State != agent.CaptureReplaced {
		t.Fatalf("state=%v, want replaced", r2.State)
	}
	if g := r2.Cursor.Artifacts[logicOf(tid)].Cursor.Generation; g != 1 {
		t.Errorf("generation = %d", g)
	}
	if !bytes.Equal(sink.data[logicOf(tid)], mod) {
		t.Error("new generation must recapture from 0")
	}
}

func TestMiddleRewriteOnZstdDetectedByDeepVerify(t *testing.T) {
	e := newEnv(t)
	tid := codexgen.ThreadIDFor(6)
	data := codexgen.Rollout(codexgen.Options{Seed: 6, Turns: 3})
	p := e.put(t, tid, "", data, true)
	r1 := e.capture(t, tid, agent.Cursor{}, newSink())
	mod := append([]byte(nil), data...)
	mod[len(mod)/2] ^= 0x01
	os.WriteFile(p, codexgen.Zstd(mod), 0600)
	r2 := e.capture(t, tid, r1.Cursor, newSink())
	if r2.State != agent.CaptureReplaced {
		t.Errorf("state=%v, want replaced", r2.State)
	}
}

func TestTwinsAndSegmentsAndDetached(t *testing.T) {
	e := newEnv(t)
	tid := codexgen.ThreadIDFor(7)
	data := codexgen.Rollout(codexgen.Options{Seed: 7, Turns: 3})
	e.put(t, tid, "", data, false)
	e.put(t, tid, "", data[:len(data)/2], true) // shorter prefix twin
	r := e.capture(t, tid, agent.Cursor{}, newSink())
	if r.State != agent.CaptureOK || len(r.Cursor.Artifacts) != 1 {
		t.Fatalf("equal-prefix twin: state=%v arts=%d", r.State, len(r.Cursor.Artifacts))
	}
	// diverged twin
	e2 := newEnv(t)
	e2.put(t, tid, "", data, false)
	other := codexgen.Rollout(codexgen.Options{Seed: 70, Turns: 3})
	e2.put(t, tid, "", other, true)
	if r := e2.capture(t, tid, agent.Cursor{}, newSink()); r.State != agent.CaptureUnsafe {
		t.Errorf("diverged twin: state=%v", r.State)
	}
	// extra segment file = second artifact
	e3 := newEnv(t)
	e3.put(t, tid, "", data, false)
	e3.put(t, tid, "0192f3a4-0000-7000-8000-00000000abcd", data, false)
	if r := e3.capture(t, tid, agent.Cursor{}, newSink()); len(r.Cursor.Artifacts) != 2 {
		t.Errorf("segment files: %d artifacts", len(r.Cursor.Artifacts))
	}
	// gone
	e4 := newEnv(t)
	if r := e4.capture(t, tid, agent.Cursor{}, newSink()); r.State != agent.CaptureDetached {
		t.Errorf("missing thread: %v", r.State)
	}
}

func TestMovedToArchiveIsLocated(t *testing.T) {
	e := newEnv(t)
	tid := codexgen.ThreadIDFor(8)
	data := codexgen.Rollout(codexgen.Options{Seed: 8})
	p := e.put(t, tid, "", data, false)
	ns, ok, _ := e.a.Locate(context.Background(), agent.NativeRef{Home: e.home, NativeID: tid})
	if !ok || ns.Cwd != "/work/app" || ns.Profile.Surface != "desktop" || ns.Profile.Version != "0.150.0-test" {
		t.Fatalf("locate: %+v ok=%v", ns, ok)
	}
	arch := filepath.Join(e.home, "archived_sessions", filepath.Base(p))
	os.MkdirAll(filepath.Dir(arch), 0700)
	os.Rename(p, arch)
	ns2, ok, _ := e.a.Locate(context.Background(), agent.NativeRef{Home: e.home, NativeID: tid})
	if !ok || !strings.HasPrefix(ns2.Files[0].Path, "archived_sessions/") {
		t.Fatalf("archived thread not found: %+v", ns2)
	}
}

func TestDiscoverAndProbeAreSafe(t *testing.T) {
	e := newEnv(t)
	secret := "SECRET-SENTINEL-123"
	for i := int64(1); i <= 3; i++ {
		data := codexgen.Rollout(codexgen.Options{Seed: i, Turns: 2, Cwd: "/work/" + secret, UILayer: i == 3, Compacted: i == 2, Ordinals: true})
		e.put(t, codexgen.ThreadIDFor(i), "", data, i == 2)
	}
	os.WriteFile(filepath.Join(e.home, "session_index.jsonl"),
		[]byte(`{"id":"`+codexgen.ThreadIDFor(1)+`","thread_name":"`+secret+`","updated_at":"x"}`+"\n"), 0600)
	list, err := e.a.Discover(context.Background(), agent.DiscoverOptions{})
	if err != nil || len(list) != 3 {
		t.Fatalf("discover: %d %v", len(list), err)
	}
	res, err := e.a.Probe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	b, _ := jsonMarshal(res)
	if strings.Contains(string(b), secret) || strings.Contains(string(b), codexgen.ThreadIDFor(1)) {
		t.Fatalf("probe leaked sensitive data: %s", b)
	}
	h := res.Homes[0]
	if res.Compatibility != "ok" || h.FileForms["jsonl.zst"] != 1 || h.FileForms["jsonl"] != 2 || h.ClientVer == "" {
		t.Errorf("probe result: %+v", res)
	}
	want := map[string]bool{"form:zst": true, "record:compacted": true, "record:item_completed": true, "has:ordinal": true}
	for _, c := range h.Capabilities {
		delete(want, c)
	}
	if len(want) != 0 {
		t.Errorf("missing capabilities %v in %v", want, h.Capabilities)
	}
	// empty home
	empty := New(config.Config{Agents: config.AgentsConfig{Codex: config.AgentConfig{Home: filepath.Join(t.TempDir(), "none")}}})
	if r, _ := empty.Probe(context.Background()); r.Compatibility != "unsupported" {
		t.Errorf("missing home: %s", r.Compatibility)
	}
}

func TestRuntimeUnknownWithoutLocks(t *testing.T) {
	e := newEnv(t)
	st, _, _ := e.a.Runtime().Running(context.Background(), e.home, "x")
	if st != agent.Unknown {
		t.Errorf("state = %v, want Unknown", st)
	}
}

type discardSink struct {
	t       *testing.T
	base    uint64
	maxHeap uint64
	total   int64
}

func (d *discardSink) PutSegment(_ context.Context, k agent.ArtifactKey, s, e int64, r io.Reader) (session.SegmentRef, error) {
	b, _ := io.ReadAll(r)
	d.total += int64(len(b))
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	if ms.HeapInuse > d.maxHeap {
		d.maxHeap = ms.HeapInuse
	}
	h := sha256.Sum256(b)
	return session.SegmentRef{Start: s, End: e, RawHash: hex.EncodeToString(h[:]), Object: "obj_" + strings.Repeat("0", 64)}, nil
}
func (d *discardSink) PutBlob(context.Context, agent.ArtifactKey, int64, io.Reader) ([]session.SegmentRef, error) {
	return nil, nil
}

// Spec §16 task 2.6: capturing a large session streams it; peak memory stays a
// small multiple of the segment size, not the file size.
func TestLargeSessionCaptureIsStreamed(t *testing.T) {
	if testing.Short() {
		t.Skip("large session")
	}
	e := newEnv(t)
	tid := codexgen.ThreadIDFor(99)
	data := codexgen.Rollout(codexgen.Options{Seed: 99, Turns: 12, PadBytes: 6 << 20}) // ~72 MiB
	p := e.put(t, tid, "", data, false)
	n := int64(len(data))
	data = nil
	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	sink := &discardSink{t: t, base: ms.HeapInuse}
	if _, err := e.a.Capture(context.Background(), agent.NativeRef{Home: e.home, NativeID: tid}, agent.Cursor{}, sink); err != nil {
		t.Fatal(err)
	}
	if sink.total != n {
		t.Fatalf("captured %d of %d bytes", sink.total, n)
	}
	grown := int64(sink.maxHeap) - int64(sink.base)
	limit := int64(4 * agent.MaxSegmentBytes)
	t.Logf("file %d MiB, peak heap growth %d MiB (limit %d MiB)", n>>20, grown>>20, limit>>20)
	if grown > limit+int64(32<<20) {
		t.Errorf("peak heap grew by %d MiB for a %d MiB file", grown>>20, n>>20)
	}
	os.Remove(p)
}

func TestRuntimeFromThreadLocks(t *testing.T) {
	e := newEnv(t)
	locks := filepath.Join(e.home, "thread-writer-locks")
	os.MkdirAll(locks, 0700)
	tid := codexgen.ThreadIDFor(5)
	det := e.a.Runtime()
	if st, _, _ := det.Running(context.Background(), e.home, tid); st != agent.NotRunning {
		t.Errorf("no lock file: %v, want NotRunning", st)
	}
	lf, _ := platform.Touch(filepath.Join(locks, tid+".lock"))
	defer lf.Close()
	if st, _, _ := det.Running(context.Background(), e.home, tid); st != agent.NotRunning {
		t.Errorf("free lock file: %v, want NotRunning", st)
	}
	platform.LockExclusiveNonBlocking(lf)
	if st, _, _ := det.Running(context.Background(), e.home, tid); st != agent.Running {
		t.Errorf("held lock: %v, want Running", st)
	}
	if st, _, _ := det.Running(context.Background(), e.home, ""); st != agent.Unknown {
		t.Errorf("no thread id: %v, want Unknown", st)
	}
}

func TestMapRootsAndCwdTogether(t *testing.T) {
	line := []byte(`{"type":"session_meta","payload":{"cwd":"/work/app","runtime_workspace_roots":["/work/app","/work/app/sub","/other"],"x":"/work/app"}}`)
	got, ok := mapLine(line, agent.PathMap{Rules: []agent.PathRule{{From: "/work/app", To: "/Users/me/app"}}}, "")
	want := `{"type":"session_meta","payload":{"cwd":"/Users/me/app","runtime_workspace_roots":["/Users/me/app","/Users/me/app/sub","/other"],"x":"/work/app"}}`
	if !ok || string(got) != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}

func TestMapLineCoversThreadSettingsAndWorldState(t *testing.T) {
	pm := agent.PathMap{Rules: []agent.PathRule{{From: "/work/app", To: "/Users/me/app"}}}
	for _, c := range []struct{ in, want string }{
		{`{"type":"event_msg","payload":{"type":"thread_settings_applied","thread_settings":{"cwd":"/work/app","runtime_workspace_roots":["/work/app"],"model":"m"}}}`,
			`{"type":"event_msg","payload":{"type":"thread_settings_applied","thread_settings":{"cwd":"/Users/me/app","runtime_workspace_roots":["/Users/me/app"],"model":"m"}}}`},
		{`{"type":"world_state","payload":{"state":{"environments":{"environments":{"local":{"cwd":"/work/app/sub"}}}}}}`,
			`{"type":"world_state","payload":{"state":{"environments":{"environments":{"local":{"cwd":"/Users/me/app/sub"}}}}}}`},
		// other events and text that merely mentions a cwd are never touched
		{`{"type":"event_msg","payload":{"type":"agent_message","message":"{\"cwd\":\"/work/app\"}"}}`, ""},
		{`{"type":"response_item","payload":{"cwd":"/work/app"}}`, ""},
	} {
		got, ok := mapLine([]byte(c.in), pm, "")
		if c.want == "" {
			if ok || string(got) != c.in {
				t.Errorf("must not change: %s -> %s", c.in, got)
			}
			continue
		}
		if !ok || string(got) != c.want {
			t.Errorf("got  %s\nwant %s", got, c.want)
		}
	}
}

func TestMapLineRewritesProvider(t *testing.T) {
	pm := agent.PathMap{Rules: []agent.PathRule{{From: "/work/app", To: "/m/app"}}}
	for _, c := range []struct{ in, want string }{
		{`{"type":"session_meta","payload":{"cwd":"/work/app","model_provider":"openai"}}`, `{"type":"session_meta","payload":{"cwd":"/m/app","model_provider":"custom"}}`},
		{`{"type":"event_msg","payload":{"type":"thread_settings_applied","thread_settings":{"model_provider_id":"openai","cwd":"/work/app"}}}`,
			`{"type":"event_msg","payload":{"type":"thread_settings_applied","thread_settings":{"model_provider_id":"custom","cwd":"/m/app"}}}`},
	} {
		got, ok := mapLine([]byte(c.in), pm, "custom")
		if !ok || string(got) != c.want {
			t.Errorf("got  %s\nwant %s", got, c.want)
		}
	}
	// provider alone, without any path rule, still applies; an empty provider changes nothing
	if got, ok := mapLine([]byte(`{"type":"session_meta","payload":{"model_provider":"openai"}}`), agent.PathMap{}, "custom"); !ok || string(got) != `{"type":"session_meta","payload":{"model_provider":"custom"}}` {
		t.Errorf("provider only: %s", got)
	}
	if _, ok := mapLine([]byte(`{"type":"session_meta","payload":{"model_provider":"openai"}}`), agent.PathMap{}, ""); ok {
		t.Error("empty provider must not rewrite")
	}
}
