package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"time"

	"session-memory/internal/conversation"
)

// ErrFileReplaced signals the file shrank below the checkpoint offset.
var ErrFileReplaced = fmt.Errorf("file_replaced: rollout shrank below checkpoint offset")

// ParseOptions configures a codex parse run (spec 8.1).
type ParseOptions struct {
	StartOffset    int64
	MaxRecordBytes int64
	MaxDeltaBytes  int64
	Location       *time.Location
}

// ParseResult carries the batched output plus stability metadata.
type ParseResult struct {
	Batches          []conversation.Batch
	SafeEndOffset    int64
	LastEventAt      time.Time
	LastTurnTerminal bool
	UnknownRecords   map[string]int
	NoText           bool
}

type envelope struct {
	Timestamp string          `json:"timestamp"`
	Type      string          `json:"type"`
	Payload   json.RawMessage `json:"payload"`
}

type responseItem struct {
	Type string `json:"type"`
	Role string `json:"role"`
	ID   string `json:"id"`
	// message content blocks
	Content json.RawMessage `json:"content"`
}

type contentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type turnContext struct {
	TurnID    string `json:"turn_id"`
	Timestamp string `json:"timestamp"`
}

type eventPayload struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

// turnAccum collects everything within one turn before date-batching.
type turnAccum struct {
	id       string
	startOff int64
	endOff   int64
	firstAt  time.Time
	// response_item texts keyed by role take precedence over event_msg
	respText map[string][]string
	// event fallback texts keyed by role
	eventText map[string][]string
	terminal  bool
	hasAny    bool
}

// Parse reads complete JSONL records from StartOffset and produces
// date-batched normalized conversation (spec 8.2–8.4).
func Parse(ctx context.Context, path string, meta SessionMeta, opts ParseOptions) (ParseResult, error) {
	res := ParseResult{UnknownRecords: map[string]int{}}
	if opts.Location == nil {
		opts.Location = time.Local
	}
	if opts.MaxRecordBytes <= 0 {
		opts.MaxRecordBytes = 16 << 20
	}
	if opts.MaxDeltaBytes <= 0 {
		opts.MaxDeltaBytes = 2 << 20
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

	reader := bufio.NewReaderSize(f, 1<<16)
	offset := opts.StartOffset

	turn := newTurn("t0", offset)
	var turns []*turnAccum

	for {
		select {
		case <-ctx.Done():
			return res, ctx.Err()
		default:
		}
		if reachedDeltaBudget(turns, opts.MaxDeltaBytes) {
			break
		}
		line, rerr := readFullLine(reader, opts.MaxRecordBytes)
		if len(line) > 0 {
			recStart := offset
			recEnd := offset + int64(len(line))

			// blank separator lines are valid JSONL; skip, do not fail
			if isBlankJSONL(line) {
				offset = recEnd
				res.SafeEndOffset = recEnd
				if rerr == io.EOF {
					break
				}
				continue
			}

			var env envelope
			if uerr := json.Unmarshal(line, &env); uerr != nil {
				// corrupt complete line: stop WITHOUT advancing SafeEndOffset
				return res, fmt.Errorf("invalid_json at byte offset %d", recStart)
			}
			offset = recEnd
			res.SafeEndOffset = recEnd
			ts := parseTimestamp(env.Timestamp, meta.Timestamp)

			switch env.Type {
			case "turn_context":
				if turn.hasAny {
					turns = append(turns, turn)
				}
				var tc turnContext
				json.Unmarshal(env.Payload, &tc)
				id := tc.TurnID
				if id == "" {
					id = fmt.Sprintf("turn@%d", recStart)
				}
				turn = newTurn(id, recStart)
				turn.firstAt = ts
				// never split mid-turn on delta budget
			case "response_item":
				var ri responseItem
				if uerr := json.Unmarshal(env.Payload, &ri); uerr != nil {
					res.UnknownRecords["response_item:<unparsable>"]++
					break
				}
				if ri.Type == "message" && (ri.Role == "user" || ri.Role == "assistant") {
					texts, imgCount := extractTexts(ri.Content)
					if len(texts) > 0 {
						turn.respText[ri.Role] = append(turn.respText[ri.Role], texts...)
						turn.endOff = recEnd
						turn.hasAny = true
						if turn.firstAt.IsZero() {
							turn.firstAt = ts
						}
					}
					// one placeholder per message regardless of image count
					if imgCount > 0 {
						turn.respText[ri.Role] = append(turn.respText[ri.Role], "[Image omitted]")
						turn.endOff = recEnd
						turn.hasAny = true
						if turn.firstAt.IsZero() {
							turn.firstAt = ts
						}
					}
				} else if ri.Type == "reasoning" || ri.Type == "function_call" ||
					ri.Type == "custom_tool_call" || ri.Type == "custom_tool_call_output" ||
					ri.Type == "function_call_output" || ri.Type == "local_shell_call" ||
					ri.Type == "web_search_call" {
					// explicitly never summarized
					turn.endOff = recEnd
				} else if ri.Type == "message" {
					// developer/system role message: skipped silently
					turn.endOff = recEnd
				} else {
					res.UnknownRecords["response_item:"+ri.Type]++
					turn.endOff = recEnd
				}
				if !res.LastEventAt.After(ts) {
					res.LastEventAt = ts
				}
			case "event_msg":
				var ev eventPayload
				if uerr := json.Unmarshal(env.Payload, &ev); uerr != nil {
					res.UnknownRecords["event_msg:<unparsable>"]++
					break
				}
				switch ev.Type {
				case "user_message":
					if ev.Message != "" {
						turn.eventText["user"] = append(turn.eventText["user"], ev.Message)
						turn.endOff = recEnd
						turn.hasAny = true
						if turn.firstAt.IsZero() {
							turn.firstAt = ts
						}
					}
				case "agent_message":
					if ev.Message != "" {
						turn.eventText["assistant"] = append(turn.eventText["assistant"], ev.Message)
						turn.endOff = recEnd
						turn.hasAny = true
						if turn.firstAt.IsZero() {
							turn.firstAt = ts
						}
					}
				case "task_complete", "turn_aborted":
					turn.terminal = true
					turn.endOff = recEnd
				case "token_count", "exec_begin", "exec_end", "mcp_tool_call_begin",
					"mcp_tool_call_end", "mcp_tool_call_output", "patch_apply_begin",
					"patch_apply_end", "web_search_begin", "web_search_end", "turn_diff",
					"agent_reasoning", "agent_reasoning_delta", "agent_reasoning_section_break",
					"plan_update", "agent_roots", "error", "warning":
					turn.endOff = recEnd
				default:
					res.UnknownRecords["event_msg:"+ev.Type]++
					turn.endOff = recEnd
				}
				if !res.LastEventAt.After(ts) {
					res.LastEventAt = ts
				}
			case "compacted", "context_compacted", "world_state":
				// skipped: compaction history would duplicate content
				turn.endOff = recEnd
			case "session_meta":
				// only meaningful at offset 0; mid-file copies ignored
				turn.endOff = recEnd
			default:
				res.UnknownRecords[env.Type]++
				turn.endOff = recEnd
			}
		}
		if rerr != nil {
			if rerr == io.EOF {
				break // clean end or trailing partial line
			}
			return res, rerr
		}
	}
	if turn.hasAny {
		turns = append(turns, turn)
	}
	res.LastTurnTerminal = turn.terminal

	// group consecutive same-date turns into batches
	loc := opts.Location
	type dayBatch struct {
		date  string
		first int64
		last  int64
		msgs  []conversation.Message
	}
	var order []string
	batches := map[string]*dayBatch{}

	for _, t := range turns {
		msgs := turnMessages(t)
		if len(msgs) == 0 {
			continue
		}
		at := t.firstAt
		if at.IsZero() {
			at = meta.Timestamp
		}
		if at.IsZero() {
			at = st.ModTime()
		}
		date := at.In(loc).Format("20060102")
		b, ok := batches[date]
		if !ok {
			b = &dayBatch{date: date}
			batches[date] = b
			order = append(order, date)
		}
		if len(b.msgs) == 0 {
			b.first = t.startOff
		}
		b.msgs = append(b.msgs, msgs...)
		b.last = t.endOff
	}
	sort.Strings(order)
	for _, date := range order {
		b := batches[date]
		res.Batches = append(res.Batches, conversation.Batch{
			Source:      SourceName,
			SessionID:   meta.SessionID,
			Cwd:         meta.Cwd,
			Date:        date,
			StartOffset: b.first,
			EndOffset:   b.last,
			Messages:    b.msgs,
		})
	}
	res.NoText = len(res.Batches) == 0
	return res, nil
}

// turnMessages converts one accumulated turn into ordered messages using
// response_item texts with event_msg fallback per role (spec 8.3 rule 5).
func turnMessages(t *turnAccum) []conversation.Message {
	var msgs []conversation.Message
	at := t.firstAt
	push := func(role string, texts []string) {
		if len(texts) == 0 {
			return
		}
		joined := ""
		for _, s := range texts {
			c := conversation.CleanText(s)
			if c == "" {
				continue
			}
			if joined == "" {
				joined = c
			} else {
				joined += "\n\n" + c
			}
		}
		if joined == "" {
			return
		}
		msgs = append(msgs, conversation.Message{
			Role: conversation.Role(role), Text: joined, Timestamp: at, TurnID: t.id,
		})
	}
	// order: user then assistant within a turn (conversation flow)
	if len(t.respText["user"]) > 0 {
		push("user", t.respText["user"])
	} else {
		push("user", t.eventText["user"])
	}
	if len(t.respText["assistant"]) > 0 {
		push("assistant", t.respText["assistant"])
	} else {
		push("assistant", t.eventText["assistant"])
	}
	return msgs
}

func newTurn(id string, start int64) *turnAccum {
	return &turnAccum{
		id: id, startOff: start, endOff: start,
		respText:  map[string][]string{},
		eventText: map[string][]string{},
	}
}

func reachedDeltaBudget(turns []*turnAccum, max int64) bool {
	if max <= 0 {
		return false
	}
	var total int64
	for _, t := range turns {
		for _, texts := range t.respText {
			for _, s := range texts {
				total += int64(len(s))
			}
		}
	}
	return total >= max
}

// extractTexts pulls input_text/output_text blocks and counts images.
func extractTexts(raw json.RawMessage) (texts []string, imgCount int) {
	if len(raw) == 0 {
		return nil, 0
	}
	var asString string
	if err := json.Unmarshal(raw, &asString); err == nil {
		if asString != "" {
			return []string{asString}, 0
		}
		return nil, 0
	}
	var blocks []contentBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return nil, 0
	}
	var out []string
	for _, b := range blocks {
		switch b.Type {
		case "input_text", "output_text", "text":
			if b.Text != "" {
				out = append(out, b.Text)
			}
		case "input_image", "image":
			imgCount++
		}
	}
	return out, imgCount
}

func parseTimestamp(s string, fallback time.Time) time.Time {
	if s == "" {
		return fallback
	}
	if ts, err := time.Parse(time.RFC3339, s); err == nil {
		return ts
	}
	if ts, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return ts
	}
	return fallback
}

func isBlankJSONL(line []byte) bool {
	for _, b := range line {
		if b != '\n' && b != '\r' && b != ' ' && b != '\t' {
			return false
		}
	}
	return true
}

func readFullLine(r *bufio.Reader, max int64) ([]byte, error) {
	var buf []byte
	for {
		chunk, err := r.ReadSlice('\n')
		buf = append(buf, chunk...)
		if int64(len(buf)) > max {
			return nil, fmt.Errorf("record_too_large")
		}
		switch {
		case err == bufio.ErrBufferFull:
			continue
		case err == io.EOF:
			return nil, io.EOF // trailing partial line not consumed
		case err != nil:
			return nil, err
		default:
			return buf, nil // complete record ending in '\n'
		}
	}
}
