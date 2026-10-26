package claude

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"time"

	"session-memory/internal/conversation"
)

// transcriptEnvelope is the Claude Code JSONL line shape we care about.
type transcriptEnvelope struct {
	Type    string          `json:"type"`
	Message json.RawMessage `json:"message"`
}

type innerMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type contentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// ParseOptions configures transcript parsing.
type ParseOptions struct {
	StartOffset    int64
	MaxRecordBytes int64
	Location       *time.Location
	// Now substitutes the clock in tests.
	Now func() time.Time

	// Subagent options
	IsSubagent      bool
	SubagentDesc    string
	ParentSessionID string
}

// ParseResult mirrors the codex parser contract for shared use.
type ParseResult struct {
	Batches        []conversation.Batch
	SafeEndOffset  int64
	LastEventAt    time.Time
	UnknownRecords map[string]int
	NoText         bool
}

// Parse reads the transcript from StartOffset and produces date-batched
// normalized messages. Only user/assistant text blocks are extracted.
func Parse(ctx context.Context, path, sessionID, cwd string, opts ParseOptions) (ParseResult, error) {
	res := ParseResult{UnknownRecords: map[string]int{}}
	if opts.Location == nil {
		opts.Location = time.Local
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}

	st, err := os.Stat(path)
	if err != nil {
		return res, err
	}
	if st.Size() < opts.StartOffset {
		return res, ErrFileReplaced
	}

	f, err := os.Open(path)
	if err != nil {
		return res, err
	}
	defer f.Close()
	if _, err := f.Seek(opts.StartOffset, 0); err != nil {
		return res, err
	}

	offset := opts.StartOffset
	reader := bufio.NewReaderSize(f, 1<<16)

	type dated struct {
		msg  conversation.Message
		off0 int64
		off1 int64
	}
	var datedMsgs []dated

	for {
		select {
		case <-ctx.Done():
			return res, ctx.Err()
		default:
		}
		line, err := readFullLine(reader, opts.MaxRecordBytes)
		if len(line) > 0 {
			start := offset
			end := offset + int64(len(line))

			// blank lines (just "\n") are structurally valid JSONL separators:
			// skip them instead of treating them as corrupt records
			if len(bytes.TrimSpace(line)) == 0 {
				offset = end
				res.SafeEndOffset = end
				if err == io.EOF {
					break
				}
				continue
			}

			var env transcriptEnvelope
			if jsonErr := json.Unmarshal(line, &env); jsonErr != nil {
				// a complete-but-corrupt line stops the parse WITHOUT
				// advancing SafeEndOffset (spec 8.2): the checkpoint must
				// stay before the failure so the next run retries the
				// record instead of silently skipping it
				return res, fmt.Errorf("invalid_json at byte offset %d", start)
			}
			offset = end
			res.SafeEndOffset = end
			role, texts, ok := extractMessage(env)
			if !ok {
				// known-ignorable metadata record types (attachment blobs,
				// request blobs, UI state, ...) are not "unknown schema":
				// counting them turned unknown_records into noise (~70k per
				// sync) and hid genuinely new unparsed types
				if !knownIgnorableType(env.Type) {
					res.UnknownRecords[recordKey(env.Type)]++
				}
			} else {
				ts := timestampOf(line, now())
				for _, text := range texts {
					clean := conversation.CleanText(text)
					if clean == "" {
						continue
					}
					if opts.IsSubagent {
						if role == "user" {
							if opts.SubagentDesc != "" {
								clean = fmt.Sprintf("[Subagent Task: %s]\n%s", opts.SubagentDesc, clean)
							} else {
								clean = fmt.Sprintf("[Subagent Task]\n%s", clean)
							}
						} else if role == "assistant" {
							clean = fmt.Sprintf("[Subagent Output]\n%s", clean)
						}
					}
					datedMsgs = append(datedMsgs, dated{
						msg:  conversation.Message{Role: conversation.Role(role), Text: clean, Timestamp: ts, TurnID: ""},
						off0: start,
						off1: end,
					})
				}
			}
		}
		if err != nil {
			if err == io.EOF {
				break // clean end or trailing partial line left for next run
			}
			return res, err
		}
	}

	// group by date
	loc := opts.Location
	type dayGroup struct {
		date  string
		first int64
		last  int64
		msgs  []conversation.Message
	}
	var order []string
	groups := map[string]*dayGroup{}
	for _, dm := range datedMsgs {
		date := dm.msg.Timestamp.In(loc).Format("20060102")
		g, ok := groups[date]
		if !ok {
			g = &dayGroup{date: date, first: dm.off0}
			groups[date] = g
			order = append(order, date)
		}
		g.msgs = append(g.msgs, dm.msg)
		g.last = dm.off1
	}
	sort.Strings(order)
	for _, date := range order {
		g := groups[date]
		res.Batches = append(res.Batches, conversation.Batch{
			Source:          SourceName,
			SessionID:       sessionID,
			ParentSessionID: opts.ParentSessionID,
			Cwd:             cwd,
			Date:            date,
			StartOffset:     g.first,
			EndOffset:       g.last,
			Messages:        g.msgs,
		})
	}
	res.NoText = len(res.Batches) == 0
	if len(datedMsgs) > 0 {
		res.LastEventAt = datedMsgs[len(datedMsgs)-1].msg.Timestamp
	}
	return res, nil
}

// SourceName identifies this adapter in batches and checkpoints.
const SourceName = "claude-code"

var ErrFileReplaced = fmt.Errorf("file_replaced: transcript shrank below checkpoint offset")

func readFullLine(r *bufio.Reader, max int64) ([]byte, error) {
	if max <= 0 {
		max = 16 << 20
	}
	var buf []byte
	for {
		chunk, err := r.ReadSlice('\n')
		buf = append(buf, chunk...)
		switch {
		case err == bufio.ErrBufferFull:
			if int64(len(buf)) > max {
				return nil, fmt.Errorf("record_too_large")
			}
			continue
		case err == io.EOF:
			// trailing bytes without a final newline are NOT consumed
			return nil, io.EOF
		case err != nil:
			return nil, err
		default:
			return buf, nil // complete record ending in '\n'
		}
	}
}

// extractMessage returns role and text blocks for user/assistant records.
func extractMessage(env transcriptEnvelope) (string, []string, bool) {
	if env.Type != "user" && env.Type != "assistant" {
		return "", nil, false
	}
	if len(env.Message) == 0 {
		return "", nil, false
	}
	var inner innerMessage
	if err := json.Unmarshal(env.Message, &inner); err != nil {
		return "", nil, false
	}
	role := inner.Role
	if role == "" {
		role = env.Type
	}
	if role != "user" && role != "assistant" {
		return "", nil, false
	}
	var texts []string
	// content: string or array of blocks
	var asString string
	if err := json.Unmarshal(inner.Content, &asString); err == nil {
		if asString != "" {
			texts = append(texts, asString)
		}
		return role, texts, true
	}
	var blocks []contentBlock
	if err := json.Unmarshal(inner.Content, &blocks); err == nil {
		for _, b := range blocks {
			if b.Type == "text" && b.Text != "" {
				texts = append(texts, b.Text)
			}
		}
	}
	return role, texts, true
}

func recordKey(t string) string {
	if t == "" {
		return "<empty>"
	}
	return t
}

type tsEnvelope struct {
	Timestamp string `json:"timestamp"`
}

// timestampOf extracts the record timestamp; falls back to now with the
// caller responsible for warnings.
func timestampOf(line []byte, fallback time.Time) time.Time {
	var env tsEnvelope
	if err := json.Unmarshal(line, &env); err == nil && env.Timestamp != "" {
		if ts, err := time.Parse(time.RFC3339, env.Timestamp); err == nil {
			return ts
		}
		if ts, err := time.Parse(time.RFC3339Nano, env.Timestamp); err == nil {
			return ts
		}
	}
	return fallback
}

// knownIgnorableType reports whether a transcript record type is metadata
// that never carries conversation text. These are silently consumed; the
// unknown_records counter is reserved for types the parser genuinely does
// not recognize yet (spec 16 error-code semantics).
func knownIgnorableType(t string) bool {
	switch t {
	case "attachment", "api-request", "api-request-blob", "api-request-shape",
		"custom-title", "last-prompt", "agent-name", "atis-latch", "mode",
		"queue-operation", "file-history-delta", "file-history-snapshot",
		"summary", "system":
		return true
	}
	return false
}
