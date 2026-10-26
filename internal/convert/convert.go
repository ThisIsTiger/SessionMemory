// Package convert turns canonical items into a target-neutral list of output
// records plus a fidelity report. It knows rules, not file formats: each
// target adapter's Writer serialises OutRecord into its native records.
// convert never imports source/*.
package convert

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"session-memory/internal/agent"
	"session-memory/internal/canonical"
)

// Default limits (design §7.2; the 64 KiB cap follows magpie's convention).
const (
	DefaultToolOutputCap = 64 << 10
	TruncationMark       = "\n[…]"
)

// OutKind classifies an output record.
type OutKind string

const (
	OutUser       OutKind = "user"
	OutAssistant  OutKind = "assistant"
	OutToolCall   OutKind = "tool_call" // a call the target has a native tool for
	OutToolResult OutKind = "tool_result"
	OutSummary    OutKind = "summary" // a compaction rendered as a message
)

// OutRecord is one record to write, in order.
type OutRecord struct {
	Kind   OutKind
	Time   time.Time
	Text   string           // messages, summaries; rendered tool notes
	Images []string         // data: URLs of user images
	Tool   *OutTool         // tool_call / tool_result
	Source canonical.RawRef // origin record, for the import receipt and traceability
	Note   string           // "" or a rendering tag: "tool_as_text", "truncated", "compaction"
}

// OutTool is a native tool call or its result.
type OutTool struct {
	Name    string          // target's tool name (e.g. "exec_command")
	CallID  string          // new id, unique within the output
	Args    json.RawMessage // call arguments in the target's shape
	Output  string          // result text
	IsError bool
}

// Options tune a conversion.
type Options struct {
	ToolOutputCap int // bytes; <=0 means DefaultToolOutputCap
	// ToolMap maps an origin tool name to the target's native tool name. A call
	// whose name is absent is rendered as text.
	ToolMap map[string]string
	// IncludeSubagents is decided by E1c; false until then.
	IncludeSubagents bool
}

// Output is the result of Convert.
type Output struct {
	Records []OutRecord
	Report  agent.FidelityReport
}

// Convert applies the rules of design §7.2:
//
//   - message user/assistant -> OutUser/OutAssistant (only the active path;
//     other branches are counted as dropped);
//   - system/developer messages and reasoning -> dropped;
//   - a tool_call+tool_result pair with a ToolMap entry -> OutToolCall +
//     OutToolResult (shell commands included); otherwise one OutAssistant text
//     "[<name>]" + fenced input + "[result]"/"[error]" + fenced output;
//   - an orphan call or result is never written as a native record: it is
//     rendered as text;
//   - tool output longer than ToolOutputCap is cut at a UTF-8 boundary and
//     TruncationMark appended;
//   - compaction -> OutSummary (origin summary text only; encrypted
//     compaction items are dropped);
//   - session_meta / unknown -> dropped, counted by record type name;
//   - Report.Counts must equal what was actually emitted (a test asserts it).
//
// Convert is deterministic: same input, same output, same report.
func Convert(items []canonical.Item, opt Options) (Output, error) {
	capBytes := opt.ToolOutputCap
	if capBytes <= 0 {
		capBytes = DefaultToolOutputCap
	}
	out := Output{Report: agent.FidelityReport{SchemaVersion: 1, Counts: map[string]agent.KindCount{}, Dropped: map[string]int{}}}
	count := func(k canonical.Kind, f func(*agent.KindCount)) {
		c := out.Report.Counts[string(k)]
		f(&c)
		out.Report.Counts[string(k)] = c
	}
	kept := func(k canonical.Kind) { count(k, func(c *agent.KindCount) { c.Kept++ }) }
	rendered := func(k canonical.Kind) { count(k, func(c *agent.KindCount) { c.Rendered++ }) }
	dropped := func(k canonical.Kind, name string) {
		count(k, func(c *agent.KindCount) { c.Dropped++ })
		if name != "" {
			out.Report.Dropped[name]++
		}
	}
	results := map[string]int{}
	calls := map[string]int{}
	for i, it := range items {
		switch it.Kind {
		case canonical.KindToolResult:
			if it.CallID != "" {
				if _, dup := results[it.CallID]; !dup {
					results[it.CallID] = i
				}
			}
		case canonical.KindToolCall:
			if it.CallID != "" {
				if _, dup := calls[it.CallID]; !dup {
					calls[it.CallID] = i
				}
			}
		}
	}
	used := map[int]bool{}
	nextCall := 0
	str := func(it canonical.Item, k string) string {
		v, _ := it.Norm[k].(string)
		return v
	}
	for i, it := range items {
		if used[i] {
			continue
		}
		switch it.Kind {
		case canonical.KindMessage:
			text := str(it, "text")
			switch {
			case it.Branch != "":
				dropped(it.Kind, "branch")
			case it.Role == canonical.RoleSystem || it.Role == canonical.RoleDeveloper:
				dropped(it.Kind, "instruction_message")
			case it.Role == canonical.RoleUser && !it.Human:
				dropped(it.Kind, "non_human_user")
			case it.Role == canonical.RoleUser:
				r := OutRecord{Kind: OutUser, Time: it.Time, Text: text, Source: it.Raw}
				if imgs, ok := it.Norm["images"].([]any); ok {
					for _, v := range imgs {
						if s, ok := v.(string); ok {
							r.Images = append(r.Images, s)
						}
					}
				}
				out.Records = append(out.Records, r)
				kept(it.Kind)
			case it.Role == canonical.RoleAssistant:
				if text == "" {
					dropped(it.Kind, "empty_message")
					continue
				}
				out.Records = append(out.Records, OutRecord{Kind: OutAssistant, Time: it.Time, Text: text, Source: it.Raw})
				kept(it.Kind)
			default:
				dropped(it.Kind, "message_role_"+string(it.Role))
			}
		case canonical.KindReasoning:
			dropped(it.Kind, "reasoning")
		case canonical.KindToolCall, canonical.KindToolResult:
			if it.Branch != "" {
				dropped(it.Kind, "branch")
				continue
			}
			var call, res *canonical.Item
			var ri int = -1
			if it.Kind == canonical.KindToolCall {
				call = &items[i]
				if j, ok := results[it.CallID]; ok && it.CallID != "" && !used[j] && j > i && items[j].Branch == "" {
					res, ri = &items[j], j
				}
			} else {
				res = &items[i]
				if j, ok := calls[it.CallID]; ok && it.CallID != "" && j < i && used[j] {
					continue
				}
			}
			if call != nil && res != nil {
				used[ri] = true
				name := str(*call, "name")
				output, trunc := cut(str(*res, "output"), capBytes)
				if native, ok := opt.ToolMap[name]; ok {
					nextCall++
					id := fmt.Sprintf("call_%d", nextCall)
					args := json.RawMessage(str(*call, "arguments"))
					if !json.Valid(args) {
						args, _ = json.Marshal(str(*call, "arguments"))
					}
					isErr, _ := res.Norm["is_error"].(bool)
					note := ""
					if trunc {
						note = "truncated"
					}
					out.Records = append(out.Records,
						OutRecord{Kind: OutToolCall, Time: call.Time, Tool: &OutTool{Name: native, CallID: id, Args: args}, Source: call.Raw},
						OutRecord{Kind: OutToolResult, Time: res.Time, Tool: &OutTool{Name: native, CallID: id, Output: output, IsError: isErr}, Source: res.Raw, Note: note})
					kept(canonical.KindToolCall)
					kept(canonical.KindToolResult)
					continue
				}
				isErr, _ := res.Norm["is_error"].(bool)
				out.Records = append(out.Records, OutRecord{Kind: OutAssistant, Time: call.Time, Source: call.Raw, Note: "tool_as_text",
					Text: textNote(name, str(*call, "arguments"), &output, isErr)})
				rendered(canonical.KindToolCall)
				rendered(canonical.KindToolResult)
				continue
			}
			// orphan: never a native record
			if call != nil {
				out.Records = append(out.Records, OutRecord{Kind: OutAssistant, Time: call.Time, Source: call.Raw, Note: "tool_as_text",
					Text: textNote(str(*call, "name"), str(*call, "arguments"), nil, false)})
				rendered(canonical.KindToolCall)
			} else {
				output, _ := cut(str(*res, "output"), capBytes)
				out.Records = append(out.Records, OutRecord{Kind: OutAssistant, Time: res.Time, Source: res.Raw, Note: "tool_as_text",
					Text: textNote("result", "", &output, false)})
				rendered(canonical.KindToolResult)
			}
		case canonical.KindCompaction:
			if readable, _ := it.Norm["readable"].(bool); readable && str(it, "text") != "" {
				out.Records = append(out.Records, OutRecord{Kind: OutSummary, Time: it.Time, Text: str(it, "text"), Source: it.Raw, Note: "compaction"})
				kept(it.Kind)
			} else {
				dropped(it.Kind, "compaction_unreadable")
			}
		case canonical.KindAttachmentRef:
			dropped(it.Kind, "attachment")
		case canonical.KindSessionMeta:
			dropped(it.Kind, "session_meta")
		default:
			name, _ := it.Norm["record_type"].(string)
			if name == "" {
				name = "unknown"
			}
			dropped(canonical.KindUnknown, name)
		}
	}
	chars := 0
	for _, r := range out.Records {
		chars += len(r.Text)
		if r.Tool != nil {
			chars += len(r.Tool.Args) + len(r.Tool.Output)
		}
		switch r.Kind {
		case OutUser, OutAssistant, OutToolCall, OutSummary:
			out.Report.VisibleItems++
		}
	}
	out.Report.TokenEstimate = chars / 4
	if out.Report.Counts[string(canonical.KindReasoning)].Dropped > 0 {
		out.Report.ContextNotes = append(out.Report.ContextNotes, "reasoning is not carried to the target")
	}
	if n := out.Report.Dropped["branch"]; n > 0 {
		out.Report.ContextNotes = append(out.Report.ContextNotes, fmt.Sprintf("%d items on non-active branches were not written", n))
	}
	return out, nil
}

// cut truncates s to max bytes at a UTF-8 boundary.
func cut(s string, max int) (string, bool) {
	if len(s) <= max {
		return s, false
	}
	n := max
	for n > 0 && n < len(s) && s[n]&0xC0 == 0x80 {
		n--
	}
	return s[:n] + TruncationMark, true
}

// fence wraps text in a code fence no line of it can close.
func fence(s string) string {
	f := "```"
	for strings.Contains(s, f) {
		f += "`"
	}
	return f + "\n" + strings.TrimRight(s, "\n") + "\n" + f + "\n"
}

func textNote(name, input string, output *string, isErr bool) string {
	var b strings.Builder
	b.WriteString("[" + name + "]\n")
	if strings.TrimSpace(input) != "" {
		b.WriteString(fence(input))
	}
	if output != nil {
		if isErr {
			b.WriteString("\n[error]\n")
		} else {
			b.WriteString("\n[result]\n")
		}
		b.WriteString(fence(*output))
	}
	return b.String()
}
