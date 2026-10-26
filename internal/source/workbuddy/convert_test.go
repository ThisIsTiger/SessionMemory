package workbuddy

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"session-memory/internal/agent"
	"session-memory/internal/canonical"
	"session-memory/internal/config"
	"session-memory/internal/convert"
	"session-memory/internal/engine"
	"session-memory/internal/errcode"
	"session-memory/internal/session"
	"session-memory/internal/source/codex"
	"session-memory/internal/testutil/codexgen"
)

func sampleOutput() convert.Output {
	t0 := time.Date(2026, 9, 1, 1, 0, 0, 0, time.UTC)
	return convert.Output{Records: []convert.OutRecord{
		{Kind: convert.OutUser, Time: t0, Text: "<b>question</b> one"},
		{Kind: convert.OutAssistant, Time: t0.Add(time.Second), Text: "answer one"},
		{Kind: convert.OutAssistant, Time: t0.Add(2 * time.Second), Text: "[exec_command]\n```\nls\n```\n", Note: "tool_as_text"},
		{Kind: convert.OutUser, Time: t0.Add(3 * time.Second), Text: "question two"},
		{Kind: convert.OutSummary, Time: t0.Add(4 * time.Second), Text: "earlier summary"},
	}}
}

// What the WorkBuddy serialiser writes must be read back by its own adapter as
// the same human conversation (spec §14.3).
func TestSerializeConvertedReadsBack(t *testing.T) {
	a := New(config.DefaultConfig())
	out := sampleOutput()
	s, err := a.SerializeConverted(out, convert.Target{Cwd: "/work/app", Now: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC), SourceAgent: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	if !nativeIDPattern.MatchString(s.NativeID) || s.Profile.Format != "workbuddy-jsonl" || s.WriterVersion == "" {
		t.Fatalf("bad identity: %+v", s)
	}
	var got []canonical.Item
	if err := a.Extract(context.Background(), s.Snapshot, func(it canonical.Item) error { got = append(got, it); return nil }); err != nil {
		t.Fatal(err)
	}
	var msgs []string
	for _, it := range got {
		if it.Kind != canonical.KindMessage {
			continue
		}
		if it.Branch != "" {
			t.Fatalf("a converted record landed on a branch: %+v", it)
		}
		if it.Role == canonical.RoleUser && !it.Human {
			t.Fatalf("converted user message is not human: %+v", it)
		}
		msgs = append(msgs, string(it.Role)+"|"+it.Norm["text"].(string))
	}
	want := []string{"user|<b>question</b> one", "assistant|answer one", "assistant|[exec_command]\n```\nls\n```\n", "user|question two",
		"assistant|[Summary of earlier conversation, carried over from codex]\nearlier summary"}
	if strings.Join(msgs, "\n--\n") != strings.Join(want, "\n--\n") {
		t.Fatalf("history changed:\n%q\nwant\n%q", msgs, want)
	}
	// records chain by parentId and every conversation record carries the session id and cwd
	var prev string
	for _, l := range bytes.Split(bytes.TrimSpace(s.Snapshot.Files[0].Data), []byte("\n")) {
		var r struct{ ID, ParentID, SessionID, Cwd, Type string }
		if err := json.Unmarshal(l, &r); err != nil {
			t.Fatal(err)
		}
		if r.Type != "message" {
			continue
		}
		if r.ParentID != prev || r.SessionID != s.NativeID || r.Cwd != "/work/app" {
			t.Fatalf("broken chain at %s: %+v prev=%s", r.ID, r, prev)
		}
		prev = r.ID
	}
}

func TestSerializeConvertedRefusesNativeToolRecords(t *testing.T) {
	a := New(config.DefaultConfig())
	_, err := a.SerializeConverted(convert.Output{Records: []convert.OutRecord{{Kind: convert.OutToolCall, Tool: &convert.OutTool{Name: "x", CallID: "c"}}}}, convert.Target{Cwd: "/w"})
	if errcode.CodeOf(err) != errcode.Unsupported {
		t.Fatalf("native tool records accepted: %v", err)
	}
	if _, err := a.SerializeConverted(sampleOutput(), convert.Target{Cwd: "relative"}); errcode.CodeOf(err) != errcode.NeedsAction {
		t.Fatalf("relative cwd accepted: %v", err)
	}
}

// Codex -> WorkBuddy -> (continue) -> Codex through the engine: the full
// history appears natively, the import boundary hides it from capture, the
// user's own records become the next revision, and converting back yields a new
// Codex thread with everything.
func TestCrossAgentRoundTrip(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("native WorkBuddy writer path encoding currently supports macOS only")
	}
	ctx := context.Background()
	codexHome, wbHome := t.TempDir(), t.TempDir()
	cwdA, cwdB := t.TempDir(), t.TempDir()
	db := testRegistry(t, wbHome, "account-B")

	tid := codexgen.ThreadIDFor(41)
	p := filepath.Join(codexHome, "sessions", "2026", "09", "01", "rollout-2026-09-01T01-46-53-"+tid+".jsonl")
	os.MkdirAll(filepath.Dir(p), 0700)
	os.WriteFile(p, codexgen.Rollout(codexgen.Options{Seed: 41, Turns: 2, ToolCalls: 1, Cwd: cwdA}), 0600)

	cfg := config.DefaultConfig()
	cfg.Agents.Codex.Home = codexHome
	cfg.Agents.WorkBuddy.Home = wbHome
	cx := codex.New(cfg)
	wb := writerTestAdapter{New(cfg), testWriter()}
	st := writerStore(t)
	e := &engine.Engine{Store: st, DeviceID: session.NewDeviceID()}

	cb, rev1, err := e.Track(ctx, cx, agent.NativeRef{Home: codexHome, NativeID: tid}, "")
	if err != nil {
		t.Fatal(err)
	}

	// --- Codex -> WorkBuddy
	plan, rec, err := e.ConvertRestore(ctx, cx, wb, cb.SessionID, engine.RestoreOptions{Home: wbHome, Cwd: cwdB, ClientClosed: true})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Mode != agent.ModeConvert || plan.Report == nil || rec.InstallMode != session.InstallConverted || rec.NativeSessionID == tid {
		t.Fatalf("plan/receipt wrong: mode=%v report=%v receipt=%+v", plan.Mode, plan.Report, rec)
	}
	dir := strings.Trim(strings.ReplaceAll(cwdB, "/", "-"), "-")
	wbFile := filepath.Join(wbHome, "projects", dir, rec.NativeSessionID+".jsonl")
	imported, err := os.ReadFile(wbFile)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"user message 0", "assistant reply 1", "[exec_command]", "user message 1"} {
		if !bytes.Contains(imported, []byte(want)) {
			t.Fatalf("converted history misses %q", want)
		}
	}
	if r, _ := readRow(ctx, db, rec.NativeSessionID); r == nil || r["user_id"] != "account-B" || r["cwd"] != cwdB {
		t.Fatalf("registry row wrong: %+v", r)
	}
	var wbBinding session.Binding
	for _, b := range mustBindings(t, e, cb.SessionID) {
		if b.AgentProfile.Agent == AgentName {
			wbBinding = b
		}
	}
	if wbBinding.InstallMode != session.InstallConverted || wbBinding.BaseRevisionID != rev1.RevisionID || wbBinding.InstalledRevisionID != rev1.RevisionID {
		t.Fatalf("binding wrong: %+v", wbBinding)
	}
	ir := wbBinding.ImportReceipt
	if ir == nil || ir.SourceRevision != rev1.RevisionID || ir.BoundaryOffset != int64(len(imported)) || ir.WrittenCount != int64(bytes.Count(imported, []byte("\n"))) || ir.WriterVersion != ConvertVersion {
		t.Fatalf("import receipt wrong: %+v (file %d bytes)", ir, len(imported))
	}
	// the same revision is converted only once
	if _, _, err := e.ConvertRestore(ctx, cx, wb, cb.SessionID, engine.RestoreOptions{Home: wbHome, Cwd: cwdB, ClientClosed: true}); errcode.CodeOf(err) != errcode.NotFound {
		t.Fatalf("converted twice: %v", err)
	}
	// the imported history is not new content
	if _, changed, err := e.Checkpoint(ctx, wb, wbBinding.BindingID, "", session.CaptureCheckpoint); err != nil || changed {
		t.Fatalf("imported history was captured as new: changed=%v err=%v", changed, err)
	}

	// --- the user continues in WorkBuddy
	lines := bytes.Split(bytes.TrimSpace(imported), []byte("\n"))
	var lastMsg struct{ ID string }
	for i := len(lines) - 1; i >= 0; i-- {
		var h struct{ ID, Type string }
		json.Unmarshal(lines[i], &h)
		if h.Type == "message" {
			lastMsg.ID = h.ID
			break
		}
	}
	f, err := os.OpenFile(wbFile, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(line(map[string]any{"type": "message", "id": "wb-u3", "parentId": lastMsg.ID, "role": "user", "sessionId": rec.NativeSessionID, "cwd": cwdB, "timestamp": int64(1893000000000),
		"content": []any{map[string]any{"type": "input_text", "text": "<user_query>\ncontinued on WB\n</user_query>"}}}))
	f.WriteString(line(map[string]any{"type": "message", "id": "wb-a3", "parentId": "wb-u3", "role": "assistant", "sessionId": rec.NativeSessionID, "cwd": cwdB, "timestamp": int64(1893000000001),
		"content": []any{map[string]any{"type": "output_text", "text": "WB answer"}}}))
	f.Close()
	rev2, changed, err := e.Checkpoint(ctx, wb, wbBinding.BindingID, "", session.CaptureCheckpoint)
	if err != nil || !changed {
		t.Fatalf("continuation not captured: changed=%v err=%v", changed, err)
	}
	if rev2.ParentRevisionID != rev1.RevisionID || rev2.AgentProfile.Agent != AgentName || rev2.SessionID != cb.SessionID {
		t.Fatalf("continuation revision wrong: %+v", rev2)
	}

	// --- WorkBuddy -> Codex: a new thread with everything; the old Codex binding is superseded
	_, rec2, err := e.ConvertRestore(ctx, wb, cx, cb.SessionID, engine.RestoreOptions{Home: codexHome, Cwd: cwdA, ClientClosed: true})
	if err != nil {
		t.Fatal(err)
	}
	if rec2.NativeSessionID == tid || rec2.NativeSessionID == rec.NativeSessionID {
		t.Fatal("conversion back reused a native id")
	}
	matches, _ := filepath.Glob(filepath.Join(codexHome, "sessions", "*", "*", "*", "rollout-*-"+rec2.NativeSessionID+".jsonl"))
	if len(matches) != 1 {
		t.Fatalf("new Codex thread file: %v", matches)
	}
	back, _ := os.ReadFile(matches[0])
	for _, want := range []string{"user message 0", "assistant reply 1", "continued on WB", "WB answer"} {
		if !bytes.Contains(back, []byte(want)) {
			t.Fatalf("round trip lost %q", want)
		}
	}
	active := 0
	for _, b := range mustBindings(t, e, cb.SessionID) {
		if b.AgentProfile.Agent == "codex" {
			if b.State == session.StateActive {
				active++
				if b.NativeSessionID != rec2.NativeSessionID || b.InstallMode != session.InstallConverted {
					t.Fatalf("wrong active Codex binding: %+v", b)
				}
			} else if b.NativeSessionID != tid || b.State != session.StateSuperseded {
				t.Fatalf("old Codex binding not superseded: %+v", b)
			}
		}
	}
	if active != 1 {
		t.Fatalf("active Codex bindings = %d", active)
	}
	// the original Codex thread is untouched
	orig, _ := os.ReadFile(p)
	if !bytes.Equal(orig, codexgen.Rollout(codexgen.Options{Seed: 41, Turns: 2, ToolCalls: 1, Cwd: cwdA})) {
		t.Fatal("the original Codex thread was modified")
	}
}

func mustBindings(t *testing.T, e *engine.Engine, sid string) []session.Binding {
	t.Helper()
	bs, err := e.Store.Bindings(sid)
	if err != nil {
		t.Fatal(err)
	}
	return bs
}
