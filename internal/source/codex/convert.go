package codex

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"

	"session-memory/internal/agent"
	"session-memory/internal/convert"
	"session-memory/internal/errcode"
	"session-memory/internal/session"
)

// ConvertVersion identifies the conversion serialiser in import receipts.
const ConvertVersion = "codex-convert/0.1"

// defaultCLIVersion is written into session_meta when the target's own version
// is unknown. Codex reads it for compatibility decisions only.
const defaultCLIVersion = "0.162.1"

var providerRe = regexp.MustCompile(`(?m)^\s*model_provider\s*=\s*"([^"]+)"`)

// targetProvider reads the default model provider from the target's own
// config.toml, so a converted thread uses the account the target already has.
func targetProvider(home string) string {
	if b, err := os.ReadFile(filepath.Join(home, "config.toml")); err == nil {
		if m := providerRe.FindSubmatch(b); m != nil {
			return string(m[1])
		}
	}
	return "openai"
}

type rolloutWriter struct {
	buf     bytes.Buffer
	n       int
	last    time.Time
	turn    string
	lastMsg string
}

func (w *rolloutWriter) stamp(t time.Time) time.Time {
	if t.IsZero() || t.Before(w.last) {
		t = w.last
	}
	w.last = t
	return t
}

// rec appends one rollout record with an ordinal (matching what Codex writes).
func (w *rolloutWriter) rec(t time.Time, typ string, payload any) error {
	t = w.stamp(t)
	var b bytes.Buffer
	fmt.Fprintf(&b, `{"timestamp":%q,"ordinal":%d,"type":%q,"payload":`, t.UTC().Format("2006-01-02T15:04:05.000Z"), w.n, typ)
	p, err := marshalNoEscape(payload)
	if err != nil {
		return err
	}
	b.Write(p)
	b.WriteString("}\n")
	w.buf.Write(b.Bytes())
	w.n++
	return nil
}

func marshalNoEscape(v any) ([]byte, error) {
	var b bytes.Buffer
	e := json.NewEncoder(&b)
	e.SetEscapeHTML(false)
	if err := e.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(b.Bytes(), "\n"), nil
}

func (w *rolloutWriter) openTurn(t time.Time) error {
	if w.turn != "" {
		return nil
	}
	w.turn = uuid.Must(uuid.NewV7()).String()
	w.lastMsg = ""
	return w.rec(t, "event_msg", map[string]any{"type": "task_started", "turn_id": w.turn, "started_at": w.stamp(t).Unix(),
		"model_context_window": nil, "collaboration_mode_kind": "default"})
}

// itemCompleted writes the UI-layer event Codex 0.16x derives its turn history
// from (a legacy user_message/agent_message event alone leaves the turn empty).
func (w *rolloutWriter) itemCompleted(t time.Time, threadID string, item map[string]any) error {
	ms := w.stamp(t).UnixMilli()
	return w.rec(t, "event_msg", map[string]any{"type": "item_completed", "thread_id": threadID, "turn_id": w.turn, "item": item, "started_at_ms": ms, "completed_at_ms": ms})
}

func (w *rolloutWriter) closeTurn() error {
	if w.turn == "" {
		return nil
	}
	id := w.turn
	w.turn = ""
	var last any
	if w.lastMsg != "" {
		last = w.lastMsg
	}
	return w.rec(w.last, "event_msg", map[string]any{"type": "task_complete", "turn_id": id, "last_agent_message": last,
		"completed_at": w.last.Unix(), "duration_ms": 0})
}

// SerializeConverted builds a new Codex thread from conversion output (spec
// §9.2). Every conversational record is written in both Codex layers (the
// model-visible response_item and the UI item_completed event); turns are delimited by
// task_started/task_complete as Codex itself does.
func (a *Adapter) SerializeConverted(out convert.Output, t convert.Target) (convert.Serialized, error) {
	if !filepath.IsAbs(t.Cwd) {
		return convert.Serialized{}, errcode.New(errcode.NeedsAction, "a converted Codex thread needs an absolute local project directory")
	}
	now := t.Now
	if now.IsZero() {
		now = time.Now()
	}
	id := uuid.Must(uuid.NewV7()).String()
	w := &rolloutWriter{last: now}
	meta := map[string]any{
		"session_id": id, "id": id, "timestamp": now.UTC().Format("2006-01-02T15:04:05.000Z"),
		"cwd": t.Cwd, "runtime_workspace_roots": []string{t.Cwd},
		"originator": "Codex Desktop", "cli_version": defaultCLIVersion, "source": "vscode", "thread_source": "user",
		"model_provider": targetProvider(t.Home), "history_mode": "paginated",
		"context_window": map[string]any{"window_id": uuid.Must(uuid.NewV7()).String()},
	}
	if err := w.rec(now, "session_meta", meta); err != nil {
		return convert.Serialized{}, err
	}
	for _, r := range out.Records {
		switch r.Kind {
		case convert.OutUser:
			if err := w.closeTurn(); err != nil {
				return convert.Serialized{}, err
			}
			if err := w.openTurn(r.Time); err != nil {
				return convert.Serialized{}, err
			}
			content := []any{map[string]any{"type": "input_text", "text": r.Text}}
			for _, img := range r.Images {
				content = append(content, map[string]any{"type": "input_image", "image_url": img})
			}
			itemID := uuid.Must(uuid.NewV7()).String()
			if err := w.rec(r.Time, "response_item", map[string]any{"type": "message", "role": "user", "content": content}); err != nil {
				return convert.Serialized{}, err
			}
			ui := []any{map[string]any{"type": "text", "text": r.Text, "text_elements": []any{}}}
			if err := w.itemCompleted(r.Time, id, map[string]any{"type": "UserMessage", "id": itemID, "content": ui}); err != nil {
				return convert.Serialized{}, err
			}
		case convert.OutAssistant, convert.OutSummary:
			if err := w.openTurn(r.Time); err != nil {
				return convert.Serialized{}, err
			}
			text := r.Text
			if r.Kind == convert.OutSummary {
				text = "[Summary of earlier conversation, carried over from " + t.SourceAgent + "]\n" + text
			}
			w.lastMsg = text
			msgID := "msg_" + strings.ReplaceAll(uuid.NewString(), "-", "")
			if err := w.rec(r.Time, "response_item", map[string]any{"type": "message", "id": msgID, "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": text}}}); err != nil {
				return convert.Serialized{}, err
			}
			if err := w.itemCompleted(r.Time, id, map[string]any{"type": "AgentMessage", "id": msgID, "content": []any{map[string]any{"type": "Text", "text": text}}}); err != nil {
				return convert.Serialized{}, err
			}
		case convert.OutToolCall:
			if err := w.openTurn(r.Time); err != nil {
				return convert.Serialized{}, err
			}
			args := string(r.Tool.Args)
			if err := w.rec(r.Time, "response_item", map[string]any{"type": "function_call", "name": r.Tool.Name, "arguments": args, "call_id": r.Tool.CallID}); err != nil {
				return convert.Serialized{}, err
			}
		case convert.OutToolResult:
			if err := w.openTurn(r.Time); err != nil {
				return convert.Serialized{}, err
			}
			if err := w.rec(r.Time, "response_item", map[string]any{"type": "function_call_output", "call_id": r.Tool.CallID, "output": r.Tool.Output}); err != nil {
				return convert.Serialized{}, err
			}
		default:
			return convert.Serialized{}, errcode.New(errcode.Internal, "unknown conversion record kind")
		}
	}
	if err := w.closeTurn(); err != nil {
		return convert.Serialized{}, err
	}
	local := now.In(time.Local)
	path := fmt.Sprintf("sessions/%04d/%02d/%02d/rollout-%s-%s.jsonl", local.Year(), int(local.Month()), local.Day(), local.Format("2006-01-02T15-04-05"), id)
	return convert.Serialized{
		NativeID:      id,
		Profile:       session.AgentProfile{Agent: AgentName, Surface: "desktop", Version: defaultCLIVersion, Format: "rollout-jsonl", AdapterVersion: AdapterVersion, OS: osName()},
		Snapshot:      &agent.MemSnapshot{Files: []agent.MemFile{{Path: path, Kind: session.KindStream, Mode: 0600, Data: w.buf.Bytes()}}},
		Records:       w.n,
		WriterVersion: ConvertVersion,
		Last:          fmt.Sprintf("o%d", w.n-1),
	}, nil
}
