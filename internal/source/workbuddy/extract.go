package workbuddy

import (
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
	"session-memory/internal/session"
)

type wbLine struct {
	Type      string          `json:"type"`
	ID        string          `json:"id"`
	ParentID  string          `json:"parentId"`
	Role      string          `json:"role"`
	Content   json.RawMessage `json:"content"`
	Timestamp json.RawMessage `json:"timestamp"`
	CallID    string          `json:"callId"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
	Output    json.RawMessage `json:"output"`
	Status    string          `json:"status"`
}

func wbText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var bs []struct{ Type, Text string }
	if json.Unmarshal(raw, &bs) != nil {
		return ""
	}
	var parts []string
	for _, b := range bs {
		if (b.Type == "input_text" || b.Type == "output_text" || b.Type == "text") && b.Text != "" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n")
}

func rawStr(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return string(raw)
}

func tsOf(raw json.RawMessage) time.Time {
	var n int64
	if json.Unmarshal(raw, &n) == nil && n > 0 {
		return time.UnixMilli(n).UTC()
	}
	return time.Time{}
}

// isHuman: a user message that is not injected context (starts with "<") or a
// scene tag.
func isHuman(text string) bool {
	t := strings.TrimSpace(text)
	return t != "" && !strings.HasPrefix(t, "<") && !IsScene(t)
}

// Current WorkBuddy wraps a human query and injected context in the same
// input_text block. Strip only its leading context envelopes, then unwrap the
// query. RawRef still points to the complete original record.
func wbUserText(text string) (string, bool) {
	text = strings.TrimSpace(text)
	for strings.HasPrefix(text, "<system-reminder>") || strings.HasPrefix(text, "<system-reminder ") {
		end := strings.Index(text, "</system-reminder>")
		if end < 0 {
			return text, false // malformed context stays non-human
		}
		text = strings.TrimSpace(text[end+len("</system-reminder>"):])
	}
	if strings.HasPrefix(text, "<user_query>") && strings.HasSuffix(text, "</user_query>") {
		text = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(text, "<user_query>"), "</user_query>"))
		// A query can itself contain HTML or another leading '<'. The explicit
		// envelope identifies it more reliably than the legacy text heuristic.
		return text, text != "" && !IsScene(text)
	}
	return text, isHuman(text)
}

// Extract reads the main session stream and subagent streams. The history is a
// message tree: the active path runs from the last item up the parentId chain;
// everything else is a branch.
func (a *Adapter) Extract(ctx context.Context, snap agent.SnapshotReader, emit func(canonical.Item) error) error {
	for _, art := range snap.Artifacts() {
		if art.Kind != session.KindStream || !strings.HasSuffix(art.Path, ".jsonl") {
			continue
		}
		rc, err := snap.Open(ctx, art.Path)
		if err != nil {
			return err
		}
		items, err := extractStream(rc, art.Path, art.Generation)
		rc.Close()
		if err != nil {
			return err
		}
		sub := strings.Contains(art.Path, "/subagents/")
		for _, it := range items {
			if sub {
				it.Subagent = art.Path
			}
			if err := emit(it); err != nil {
				return err
			}
		}
	}
	return nil
}

func extractStream(rc io.ReadCloser, path string, gen int) ([]canonical.Item, error) {
	r := jsonl.NewReader(rc, 0, jsonl.Options{MaxRecordBytes: jsonl.DefaultMaxRecordBytes})
	defer r.Close()
	type node struct {
		item   canonical.Item
		parent string
		wbID   string
	}
	var nodes []node
	for {
		rec, err := r.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		if rec.Blank() {
			continue
		}
		var l wbLine
		if json.Unmarshal(rec.Line, &l) != nil {
			continue
		}
		sum := sha256.Sum256(rec.Line)
		h := hex.EncodeToString(sum[:])
		it := canonical.Item{ID: l.ID, Time: tsOf(l.Timestamp), Raw: canonical.RawRef{Artifact: path, Offset: rec.Start, Length: rec.End - rec.Start, Hash: h}}
		if it.ID == "" {
			it.ID = fmt.Sprintf("g%d:%d:%s", gen, rec.Start, h[:12])
		}
		switch l.Type {
		case "message":
			text := wbText(l.Content)
			human := false
			if l.Role == "user" {
				text, human = wbUserText(text)
			}
			it.Kind, it.Role = canonical.KindMessage, canonical.Role(l.Role)
			it.Norm = map[string]any{"text": text}
			it.Human = human
		case "reasoning":
			it.Kind = canonical.KindReasoning
		case "function_call":
			it.Kind, it.CallID = canonical.KindToolCall, l.CallID
			it.Norm = map[string]any{"name": l.Name, "arguments": rawStr(l.Arguments)}
		case "function_call_result":
			it.Kind, it.CallID = canonical.KindToolResult, l.CallID
			it.Norm = map[string]any{"output": rawStr(l.Output), "is_error": l.Status == "failed"}
		case "summary":
			it.Kind = canonical.KindCompaction
			it.Norm = map[string]any{"readable": false} // semantics unconfirmed (E6)
		default:
			it.Kind = canonical.KindUnknown
			it.Norm = map[string]any{"record_type": l.Type}
		}
		nodes = append(nodes, node{it, l.ParentID, l.ID})
	}
	// active path: from the last conversational node up the parent chain
	byID := map[string]int{}
	for i, n := range nodes {
		if n.wbID != "" {
			byID[n.wbID] = i
		}
	}
	active := map[int]bool{}
	leaf := -1
	for i := len(nodes) - 1; i >= 0; i-- {
		if k := nodes[i].item.Kind; k == canonical.KindMessage || k == canonical.KindToolCall || k == canonical.KindToolResult {
			leaf = i
			break
		}
	}
	for cur, steps := leaf, 0; cur >= 0 && steps < len(nodes)+1; steps++ {
		active[cur] = true
		p, ok := byID[nodes[cur].parent]
		if nodes[cur].parent == "" || !ok {
			break
		}
		cur = p
	}
	out := make([]canonical.Item, 0, len(nodes))
	branch := 0
	var branchTag string
	for i, n := range nodes {
		it := n.item
		switch it.Kind {
		case canonical.KindMessage, canonical.KindToolCall, canonical.KindToolResult, canonical.KindReasoning:
			if !active[i] && leaf >= 0 {
				if branchTag == "" || nodes[i].parent == "" {
					branch++
					branchTag = fmt.Sprintf("b%d", branch)
				}
				it.Branch = branchTag
			} else {
				branchTag = ""
			}
		}
		it.ParentID = n.parent
		out = append(out, it)
	}
	return out, nil
}
