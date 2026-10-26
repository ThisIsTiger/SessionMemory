// Package canonical defines the neutral item model used to convert a session
// between agents (design §5.4). It is derived on the consuming device by an
// adapter's Extract and never synced.
package canonical

import "time"

// Kind classifies an item.
type Kind string

const (
	KindMessage       Kind = "message"
	KindReasoning     Kind = "reasoning"
	KindToolCall      Kind = "tool_call"
	KindToolResult    Kind = "tool_result"
	KindCompaction    Kind = "compaction"
	KindAttachmentRef Kind = "attachment_ref"
	KindSessionMeta   Kind = "session_meta"
	KindUnknown       Kind = "unknown"
)

// Role of a message.
type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleSystem    Role = "system"
	RoleDeveloper Role = "developer"
)

// RawRef points back at the origin record inside the native snapshot.
type RawRef struct {
	Artifact string `json:"artifact"` // session.Artifact.Path
	Offset   int64  `json:"offset"`   // byte offset of the record in the decompressed stream
	Length   int64  `json:"length"`
	Hash     string `json:"hash"` // hex SHA-256 of the record bytes
}

// Item is one conversation element.
type Item struct {
	// ID is the stable record key: the origin record's own id or ordinal
	// (Codex: "o<ordinal>"), identical across adapter versions. Only when the
	// origin has neither is it "g<generation>:<offset>:<hash12>".
	ID       string         `json:"id"`
	Kind     Kind           `json:"kind"`
	Role     Role           `json:"role,omitempty"` // messages
	ParentID string         `json:"parent_id,omitempty"`
	CallID   string         `json:"call_id,omitempty"` // pairs tool_call with tool_result
	Time     time.Time      `json:"time"`
	Human    bool           `json:"human,omitempty"` // user message typed by a person (adapter predicate)
	Norm     map[string]any `json:"norm,omitempty"`  // text, tool name, arguments, status, ...
	Raw      RawRef         `json:"raw"`
	Links    []RawRef       `json:"links,omitempty"`  // the other layer's records for the same event
	Branch   string         `json:"branch,omitempty"` // "" = active path; else the branch tag
}

// Issue is a validation finding; Validate never drops items.
type Issue struct {
	ItemID string
	Code   string // "orphan_call", "orphan_result", "out_of_order", "missing_field"
}

// Validate checks ordering and call/result pairing.
func Validate(items []Item) []Issue {
	var out []Issue
	calls := map[string]bool{}
	results := map[string]bool{}
	var last int64 = -1
	for _, it := range items {
		if it.Raw.Offset < last && it.Raw.Artifact != "" {
			out = append(out, Issue{it.ID, "out_of_order"})
		}
		if it.Raw.Offset > last {
			last = it.Raw.Offset
		}
		if it.ID == "" || it.Kind == "" {
			out = append(out, Issue{it.ID, "missing_field"})
		}
		switch it.Kind {
		case KindToolCall:
			calls[it.CallID] = true
		case KindToolResult:
			results[it.CallID] = true
		}
	}
	for _, it := range items {
		switch it.Kind {
		case KindToolCall:
			if it.CallID == "" || !results[it.CallID] {
				out = append(out, Issue{it.ID, "orphan_call"})
			}
		case KindToolResult:
			if it.CallID == "" || !calls[it.CallID] {
				out = append(out, Issue{it.ID, "orphan_result"})
			}
		}
	}
	return out
}
