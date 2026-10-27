package codex

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"session-memory/internal/agent"
	"session-memory/internal/canonical"
	"session-memory/internal/jsonl"
)

type cxBlock struct {
	Type     string `json:"type"`
	Text     string `json:"text"`
	ImageURL string `json:"image_url"`
}

type cxPayload struct {
	Type      string          `json:"type"`
	Role      string          `json:"role"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Arguments string          `json:"arguments"`
	Input     string          `json:"input"`
	CallID    string          `json:"call_id"`
	Output    json.RawMessage `json:"output"`
	Content   json.RawMessage `json:"content"`
	Message   string          `json:"message"`
	Action    struct {
		Command []string `json:"command"`
	} `json:"action"`
	Replacement []json.RawMessage `json:"replacement_history"`
}

// isOwnWords reports a user message Codex wrote itself (context, not a person).
func isOwnWords(text string) bool {
	t := strings.TrimSpace(text)
	return t == "" || strings.HasPrefix(t, "<") || strings.HasPrefix(t, "# AGENTS.md")
}

func blocks(raw json.RawMessage) (text string, images []string) {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s, nil
	}
	var bs []cxBlock
	if json.Unmarshal(raw, &bs) != nil {
		return "", nil
	}
	var parts []string
	for _, b := range bs {
		switch b.Type {
		case "input_text", "output_text", "text", "Text":
			parts = append(parts, b.Text)
		case "input_image":
			if strings.HasPrefix(b.ImageURL, "data:") {
				images = append(images, b.ImageURL)
			}
		}
	}
	return strings.Join(parts, "\n"), images
}

func outputText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var o struct {
		Content string `json:"content"`
		Text    string `json:"text"`
	}
	if json.Unmarshal(raw, &o) == nil {
		if o.Content != "" {
			return o.Content
		}
		return o.Text
	}
	return string(raw)
}

// Extract reads every rollout artifact of the snapshot. The model layer
// (response_item) is the main stream; event_msg user/agent messages with the
// same text are linked to the item instead of being emitted again.
func (a *Adapter) Extract(ctx context.Context, snap agent.SnapshotReader, emit func(canonical.Item) error) error {
	for _, art := range snap.Artifacts() {
		rc, err := snap.Open(ctx, art.Path)
		if err != nil {
			return err
		}
		items, err := extractStream(rc, art.Path, art.Generation)
		rc.Close()
		if err != nil {
			return err
		}
		for _, it := range items {
			if err := emit(it); err != nil {
				return err
			}
		}
	}
	return nil
}

func extractStream(rc io.ReadCloser, path string, gen int) ([]canonical.Item, error) {
	r := jsonl.NewReader(rc, 0, jsonl.Options{MaxRecordBytes: jsonl.DefaultMaxRecordBytes})
	var items []canonical.Item
	lastMsg := map[string]int{} // "role|text" -> index
	for {
		rec, err := r.Next()
		if errors.Is(err, io.EOF) {
			return items, nil
		}
		if err != nil {
			return nil, err
		}
		if rec.Blank() {
			continue
		}
		var env struct {
			Timestamp string          `json:"timestamp"`
			Ordinal   *int            `json:"ordinal"`
			Type      string          `json:"type"`
			Payload   json.RawMessage `json:"payload"`
		}
		if json.Unmarshal(rec.Line, &env) != nil {
			continue
		}
		sum := sha256.Sum256(rec.Line)
		h := hex.EncodeToString(sum[:])
		raw := canonical.RawRef{Artifact: path, Offset: rec.Start, Length: rec.End - rec.Start, Hash: h}
		id := fmt.Sprintf("g%d:%d:%s", gen, rec.Start, h[:12])
		if env.Ordinal != nil {
			id = fmt.Sprintf("o%d", *env.Ordinal)
		}
		ts, _ := time.Parse(time.RFC3339Nano, env.Timestamp)
		base := canonical.Item{ID: id, Time: ts, Raw: raw}
		var p cxPayload
		json.Unmarshal(env.Payload, &p)
		switch env.Type {
		case "session_meta":
			base.Kind = canonical.KindSessionMeta
			items = append(items, base)
		case "response_item":
			switch p.Type {
			case "message":
				text, imgs := blocks(p.Content)
				base.Kind, base.Role = canonical.KindMessage, canonical.Role(p.Role)
				base.Norm = map[string]any{"text": text}
				if len(imgs) > 0 {
					anys := make([]any, len(imgs))
					for i, v := range imgs {
						anys[i] = v
					}
					base.Norm["images"] = anys
				}
				if p.Role == "user" {
					base.Human = !isOwnWords(text) || len(imgs) > 0
				}
				lastMsg[p.Role+"|"+text] = len(items)
				items = append(items, base)
			case "reasoning":
				base.Kind = canonical.KindReasoning
				items = append(items, base)
			case "function_call":
				base.Kind, base.CallID = canonical.KindToolCall, p.CallID
				base.Norm = map[string]any{"name": p.Name, "arguments": p.Arguments}
				items = append(items, base)
			case "custom_tool_call":
				base.Kind, base.CallID = canonical.KindToolCall, p.CallID
				base.Norm = map[string]any{"name": p.Name, "arguments": p.Input}
				items = append(items, base)
			case "local_shell_call":
				base.Kind, base.CallID = canonical.KindToolCall, p.CallID
				base.Norm = map[string]any{"name": "shell", "arguments": strings.Join(p.Action.Command, " ")}
				items = append(items, base)
			case "function_call_output", "custom_tool_call_output":
				base.Kind, base.CallID = canonical.KindToolResult, p.CallID
				base.Norm = map[string]any{"output": outputText(p.Output)}
				items = append(items, base)
			case "compaction", "compaction_summary":
				base.Kind = canonical.KindCompaction
				base.Norm = map[string]any{"readable": false}
				items = append(items, base)
			default:
				base.Kind = canonical.KindUnknown
				base.Norm = map[string]any{"record_type": "response_item/" + p.Type}
				items = append(items, base)
			}
		case "compacted":
			base.Kind = canonical.KindCompaction
			base.Norm = map[string]any{"text": p.Message, "readable": p.Message != ""}
			items = append(items, base)
		case "event_msg":
			var ev struct {
				Type    string `json:"type"`
				Message string `json:"message"`
				Item    struct {
					Type    string
					Content json.RawMessage
				}
			}
			json.Unmarshal(env.Payload, &ev)
			role := map[string]string{"user_message": "user", "agent_message": "assistant"}[ev.Type]
			if ev.Type == "item_completed" {
				role = map[string]string{"UserMessage": "user", "AgentMessage": "assistant"}[ev.Item.Type]
				ev.Message, _ = blocks(ev.Item.Content)
			}
			if role != "" {
				if i, ok := lastMsg[role+"|"+ev.Message]; ok {
					items[i].Links = append(items[i].Links, raw)
					if role == "user" && ev.Type == "item_completed" && ev.Message != "" {
						items[i].Human = true
					}
					continue
				}
			}
			base.Kind = canonical.KindUnknown
			base.Norm = map[string]any{"record_type": "event_msg/" + ev.Type}
			items = append(items, base)
		default:
			base.Kind = canonical.KindUnknown
			base.Norm = map[string]any{"record_type": env.Type}
			items = append(items, base)
		}
	}
}

var _ = bytes.NewReader
