package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"session-memory/internal/agent"
	"session-memory/internal/canonical"
	"session-memory/internal/config"
	"session-memory/internal/convert"
	"session-memory/internal/errcode"
	"session-memory/internal/session"
)

func convSample() convert.Output {
	t0 := time.Date(2026, 9, 1, 1, 0, 0, 0, time.UTC)
	return convert.Output{Records: []convert.OutRecord{
		{Kind: convert.OutUser, Time: t0, Text: "first question 你好 <tag>"},
		{Kind: convert.OutAssistant, Time: t0.Add(time.Second), Text: "first answer"},
		{Kind: convert.OutAssistant, Time: t0.Add(2 * time.Second), Text: "[Bash]\n```\nls\n```\n", Note: "tool_as_text"},
		{Kind: convert.OutUser, Time: t0.Add(3 * time.Second), Text: "second question", Images: []string{"data:image/png;base64,AAAA"}},
		{Kind: convert.OutSummary, Time: t0.Add(4 * time.Second), Text: "older context"},
		{Kind: convert.OutToolCall, Time: t0.Add(5 * time.Second), Tool: &convert.OutTool{Name: "exec_command", CallID: "call_1", Args: json.RawMessage(`{"cmd":"ls"}`)}},
		{Kind: convert.OutToolResult, Time: t0.Add(6 * time.Second), Tool: &convert.OutTool{Name: "exec_command", CallID: "call_1", Output: "a\nb"}},
	}}
}

func TestSerializeConvertedShape(t *testing.T) {
	a := New(config.DefaultConfig())
	cwd := t.TempDir()
	s, err := a.SerializeConverted(convSample(), convert.Target{Home: t.TempDir(), Cwd: cwd, Now: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC), SourceAgent: "workbuddy"})
	if err != nil {
		t.Fatal(err)
	}
	f := s.Snapshot.Files[0]
	if id, _, ok := parseName(filepath.Base(f.Path)); !ok || id != s.NativeID {
		t.Fatalf("file name does not carry the thread id: %s", f.Path)
	}
	if !strings.HasPrefix(f.Path, "sessions/2026/") && !strings.HasPrefix(f.Path, "sessions/2025/") {
		t.Fatalf("unexpected location %s", f.Path)
	}
	var starts, completes, userItems, agentItems, userResp, ordinals int
	for i, l := range bytes.Split(bytes.TrimSpace(f.Data), []byte("\n")) {
		var r struct {
			Ordinal int
			Type    string
			Payload struct {
				Type, Role, ID, Cwd string
				HistoryMode         string   `json:"history_mode"`
				Roots               []string `json:"runtime_workspace_roots"`
				Item                struct{ Type string }
			}
		}
		if err := json.Unmarshal(l, &r); err != nil {
			t.Fatalf("line %d invalid: %v", i, err)
		}
		if r.Ordinal != i {
			t.Fatalf("ordinal %d at line %d", r.Ordinal, i)
		}
		ordinals++
		switch {
		case r.Type == "session_meta":
			if i != 0 || r.Payload.ID != s.NativeID || r.Payload.Cwd != cwd || len(r.Payload.Roots) != 1 || r.Payload.Roots[0] != cwd || r.Payload.HistoryMode != "paginated" {
				t.Fatalf("bad session_meta: %s", l)
			}
		case r.Type == "event_msg" && r.Payload.Type == "task_started":
			starts++
		case r.Type == "event_msg" && r.Payload.Type == "task_complete":
			completes++
		case r.Type == "event_msg" && r.Payload.Type == "item_completed" && r.Payload.Item.Type == "UserMessage":
			userItems++
		case r.Type == "event_msg" && r.Payload.Type == "item_completed" && r.Payload.Item.Type == "AgentMessage":
			agentItems++
		case r.Type == "response_item" && r.Payload.Type == "message" && r.Payload.Role == "user":
			userResp++
		case r.Type == "event_msg" && (r.Payload.Type == "user_message" || r.Payload.Type == "agent_message"):
			t.Fatalf("legacy event written; Codex 0.16x derives turns from item_completed: %s", l)
		}
	}
	// two user questions -> two turns, each opened and closed once; three assistant messages
	if starts != 2 || completes != 2 || userItems != 2 || userResp != 2 || agentItems != 3 || ordinals != s.Records {
		t.Fatalf("starts=%d completes=%d userItems=%d userResp=%d agentItems=%d lines=%d records=%d", starts, completes, userItems, userResp, agentItems, ordinals, s.Records)
	}
	// the native Writer must accept it as a new thread, and our own extractor reads the history back
	var got []string
	if err := a.Extract(context.Background(), s.Snapshot, func(it canonical.Item) error {
		if it.Kind == canonical.KindMessage {
			if it.Role == canonical.RoleUser && !it.Human {
				t.Fatalf("converted user message is not human")
			}
			got = append(got, string(it.Role)+"|"+it.Norm["text"].(string))
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	want := []string{"user|first question 你好 <tag>", "assistant|first answer", "assistant|[Bash]\n```\nls\n```\n", "user|second question",
		"assistant|[Summary of earlier conversation, carried over from workbuddy]\nolder context"}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("history changed:\n%q\nwant\n%q", got, want)
	}
	home := t.TempDir()
	plan, err := a.WriterWith(true).Plan(context.Background(), agent.WriteInput{SessionID: session.NewSessionID(), RevisionID: "rev", Mode: agent.ModeNative,
		Source: s.Snapshot, SourceProfile: s.Profile, Target: s.Profile, TargetState: agent.TargetState{Home: home}})
	if err != nil || len(plan.Writes) != 1 || plan.Writes[0].Op != "create" {
		t.Fatalf("Codex writer rejects its own converted thread: %+v %v", plan, err)
	}
}

func TestSerializeConvertedRelativeCwdRefused(t *testing.T) {
	if _, err := New(config.DefaultConfig()).SerializeConverted(convSample(), convert.Target{Cwd: "rel/dir"}); errcode.CodeOf(err) != errcode.NeedsAction {
		t.Fatalf("relative cwd accepted: %v", err)
	}
}

func TestTargetProviderFromConfig(t *testing.T) {
	home := t.TempDir()
	if got := targetProvider(home); got != "openai" {
		t.Fatalf("default provider = %q", got)
	}
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte("model = \"x\"\nmodel_provider = \"corp\"\n[model_providers.corp]\nname=\"c\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if got := targetProvider(home); got != "corp" {
		t.Fatalf("provider = %q", got)
	}
}
