// Package convert turns canonical items into a target-neutral list of output
// records plus a fidelity report. It knows rules, not file formats: each
// target adapter's Writer serialises OutRecord into its native records.
// convert never imports source/*.
package convert

import (
	"encoding/json"
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
	return Output{}, errNotImplemented
}

type constError string

func (e constError) Error() string { return string(e) }

const errNotImplemented = constError("convert: not implemented")
