package conversation

import (
	"encoding/hex"
	"regexp"
	"strings"
	"testing"
	"time"
)

func mk(role Role, text, turn string) Message {
	return Message{Role: role, Text: text, TurnID: turn, Timestamp: time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)}
}

func TestConversationTextFormat(t *testing.T) {
	b := Batch{
		Source: "claude-code", SessionID: "s1", Date: "20260901",
		Messages: []Message{
			mk(RoleUser, "hello\r\nworld", "t1"),
			mk(RoleUser, "second line", "t1"),
			mk(RoleAssistant, "hi there", "t1"),
			mk(RoleUser, "new turn", "t2"),
		},
	}
	got := b.ConversationText()
	want := "[User]\nhello\nworld\n\nsecond line\n\n[Assistant]\nhi there\n\n[User]\nnew turn"
	if got != want {
		t.Errorf("ConversationText mismatch:\ngot:  %q\nwant: %q", got, want)
	}
}

func TestConversationTextIdempotent(t *testing.T) {
	b := Batch{
		Source: "claude-code", SessionID: "s1", Date: "20260901",
		Messages: []Message{
			mk(RoleUser, "first", "t1"),
			mk(RoleUser, "second", "t1"),
		},
	}
	s1 := b.ConversationText()
	s2 := b.ConversationText()
	if s1 != s2 {
		t.Fatalf("ConversationText is not idempotent:\n1st: %q\n2nd: %q", s1, s2)
	}
	want := "[User]\nfirst\n\nsecond"
	if s1 != want {
		t.Errorf("got %q, want %q", s1, want)
	}
	// verify underlying slice was not mutated
	if b.Messages[0].Text != "first" || b.Messages[1].Text != "second" {
		t.Errorf("underlying messages were mutated: %+v", b.Messages)
	}
}

func TestCleanTextStripsInvalid(t *testing.T) {
	in := "abc\x00def\r\nghi"
	got := CleanText(in)
	if got != "abcdef\nghi" {
		t.Errorf("CleanText = %q", got)
	}
	if !strings.HasPrefix(CleanText("  padded  "), "padded") {
		t.Error("trim failed")
	}
}

func TestOperationIDGolden(t *testing.T) {
	b := Batch{
		Source: "codex", SessionID: "abc", Cwd: "/w",
		Date: "20260901", StartOffset: 10, EndOffset: 200,
		Messages: []Message{mk(RoleUser, "hello", "t1")},
	}
	// computed by re-deriving manually to lock the protocol
	conv := b.ConversationText()
	ch := sha256Hex(conv)
	pre := strings.Join([]string{"codex", "abc", "10", "200", ch}, "\x00")
	want := sha256Hex(pre)
	if got := b.OperationID(); got != want {
		t.Errorf("OperationID changed:\ngot  %s\nwant %s", got, want)
	}
	// determinism
	if b.OperationID() != b.OperationID() {
		t.Error("OperationID not deterministic")
	}
	// offset change alters id
	b.EndOffset = 201
	if b.OperationID() == want {
		t.Error("OperationID insensitive to offset")
	}
}

func TestOperationIDHex64(t *testing.T) {
	b := Batch{Source: "x", SessionID: "y", Messages: []Message{mk(RoleUser, "z", "")}}
	id := b.OperationID()
	if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(id) {
		t.Errorf("OperationID not 64 lowercase hex: %s", id)
	}
	_ = hex.EncodeToString
}

func sha256Hex(s string) string {
	sum := sha256Sum(s)
	return sum
}
