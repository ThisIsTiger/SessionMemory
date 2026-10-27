package workbuddy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path/filepath"
	"runtime"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"session-memory/internal/agent"
	"session-memory/internal/convert"
	"session-memory/internal/errcode"
	"session-memory/internal/session"
)

// ConvertVersion identifies the conversion serialiser in import receipts.
const ConvertVersion = "workbuddy-convert/0.2"

// titleOf derives a short title from the first human message.
func titleOf(out convert.Output) string {
	for _, r := range out.Records {
		if r.Kind != convert.OutUser {
			continue
		}
		s := strings.Join(strings.Fields(r.Text), " ")
		if utf8.RuneCountInString(s) > 40 {
			s = string([]rune(s)[:40])
		}
		if s != "" {
			return s
		}
	}
	return "Converted session"
}

// SerializeConverted builds a new WorkBuddy session from conversion output.
// Records are OpenAI-Agents style items chained by parentId; a user message
// carries the same <user_query> envelope WorkBuddy writes itself, so that the
// client and this adapter both see a human turn. Tool calls are always
// rendered as text (convert's ToolMap for this target is empty), because no
// native tool-record shape has been verified.
func (a *Adapter) SerializeConverted(out convert.Output, t convert.Target) (convert.Serialized, error) {
	if !filepath.IsAbs(t.Cwd) {
		return convert.Serialized{}, errcode.New(errcode.NeedsAction, "a converted WorkBuddy session needs an absolute local project directory")
	}
	now := t.Now
	if now.IsZero() {
		now = time.Now()
	}
	sid := uuid.NewString()
	var buf bytes.Buffer
	n := 0
	last := now.UnixMilli()
	parent := ""
	lastID := ""
	titled := false
	line := func(m map[string]any) error {
		b, err := marshalLine(m)
		if err != nil {
			return err
		}
		buf.Write(b)
		buf.WriteByte('\n')
		n++
		return nil
	}
	ms := func(t time.Time) int64 {
		v := t.UnixMilli()
		if t.IsZero() {
			v = last
		}
		last = v
		return v
	}
	if err := line(map[string]any{"type": "session-meta", "id": uuid.Must(uuid.NewV7()).String(), "sessionId": sid, "timestamp": ms(now),
		"meta": map[string]any{"codebuddy.ai/hostKind": "unopted"}}); err != nil {
		return convert.Serialized{}, err
	}
	title := t.Title
	if title == "" {
		title = titleOf(out)
	}
	for _, r := range out.Records {
		id := uuid.Must(uuid.NewV7()).String()
		rec := map[string]any{"id": id, "timestamp": ms(r.Time), "type": "message", "sessionId": sid, "cwd": t.Cwd}
		if parent != "" {
			rec["parentId"] = parent
		}
		switch r.Kind {
		case convert.OutUser:
			content := []any{map[string]any{"type": "input_text", "text": "<user_query>\n" + r.Text + "\n</user_query>"}}
			for _, img := range r.Images {
				content = append(content, map[string]any{"type": "input_image", "image": img})
			}
			rec["role"], rec["content"] = "user", content
			rec["providerData"] = map[string]any{"startsNewUserRequest": true, "agent": "cli"}
		case convert.OutAssistant, convert.OutSummary:
			text := r.Text
			if r.Kind == convert.OutSummary {
				text = "[Summary of earlier conversation, carried over from " + t.SourceAgent + "]\n" + text
			}
			rec["role"], rec["status"] = "assistant", "completed"
			rec["content"] = []any{map[string]any{"type": "output_text", "text": text}}
			rec["providerData"] = map[string]any{"agent": "cli"}
		default:
			return convert.Serialized{}, errcode.New(errcode.Unsupported, "native tool records are not written to WorkBuddy; render them as text")
		}
		if err := line(rec); err != nil {
			return convert.Serialized{}, err
		}
		parent, lastID = id, id
		if r.Kind == convert.OutUser && !titled {
			titled = true
			// the title record follows the first user message, as WorkBuddy does
			if err := line(map[string]any{"id": uuid.Must(uuid.NewV7()).String(), "timestamp": ms(r.Time), "type": "ai-title", "aiTitle": title, "sessionId": sid, "cwd": t.Cwd}); err != nil {
				return convert.Serialized{}, err
			}
		}
	}
	if lastID == "" {
		return convert.Serialized{}, errcode.New(errcode.NotFound, "nothing to write: the conversion has no visible records")
	}
	dir := strings.Trim(strings.ReplaceAll(t.Cwd, "/", "-"), "-")
	return convert.Serialized{
		NativeID:      sid,
		Profile:       session.AgentProfile{Agent: AgentName, Surface: "desktop", Format: "workbuddy-jsonl", AdapterVersion: AdapterVersion, OS: runtime.GOOS},
		Snapshot:      &agent.MemSnapshot{Files: []agent.MemFile{{Path: "projects/" + dir + "/" + sid + ".jsonl", Kind: session.KindStream, Mode: 0600, Data: buf.Bytes()}}},
		Records:       n,
		WriterVersion: ConvertVersion,
		Last:          lastID,
	}, nil
}

func marshalLine(m map[string]any) ([]byte, error) {
	var b bytes.Buffer
	e := json.NewEncoder(&b)
	e.SetEscapeHTML(false)
	if err := e.Encode(m); err != nil {
		return nil, fmt.Errorf("encode record: %w", err)
	}
	return bytes.TrimRight(b.Bytes(), "\n"), nil
}
