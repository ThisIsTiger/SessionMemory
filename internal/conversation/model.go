// Package conversation defines the normalized message model shared by all
// source adapters and the summary service.
package conversation

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
)

// Message is one normalized conversational text block from any source.
type Message struct {
	Role      Role
	Text      string
	Timestamp time.Time
	TurnID    string
}

// Batch is a date-bounded slice of messages from one session.
type Batch struct {
	Source          string
	SessionID       string
	ParentSessionID string // empty if main session
	Cwd             string
	Date            string // YYYYMMDD in configured timezone
	StartOffset     int64
	EndOffset       int64
	Messages        []Message
}

// CleanText normalizes CRLF to LF, strips NUL, replaces invalid UTF-8,
// and trims surrounding whitespace while preserving interior newlines.
func CleanText(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	s = strings.ReplaceAll(s, "\x00", "")
	if !utf8.ValidString(s) {
		var b strings.Builder
		for _, r := range s {
			if r == utf8.RuneError {
				b.WriteRune(0xFFFD)
			} else {
				b.WriteRune(r)
			}
		}
		s = b.String()
	}
	return strings.TrimSpace(s)
}

// ConversationText renders the batch in the canonical prompt format:
//
//	[User]
//	text
//
//	[Assistant]
//	text
//
// Consecutive same-role messages within the same turn are merged.
func (b Batch) ConversationText() string {
	var sb strings.Builder
	var curRole Role
	var curTurn string
	var curText string
	hasCur := false

	flush := func() {
		if hasCur {
			text := CleanText(curText)
			if text != "" {
				roleLabel := "User"
				if curRole == RoleAssistant {
					roleLabel = "Assistant"
				}
				fmt.Fprintf(&sb, "[%s]\n%s\n\n", roleLabel, text)
			}
			hasCur = false
			curRole = ""
			curTurn = ""
			curText = ""
		}
	}
	for i := range b.Messages {
		m := &b.Messages[i]
		if m.Role != RoleUser && m.Role != RoleAssistant {
			continue
		}
		if hasCur && curRole == m.Role && curTurn == m.TurnID {
			curText = CleanText(curText) + "\n\n" + CleanText(m.Text)
			continue
		}
		flush()
		hasCur = true
		curRole = m.Role
		curTurn = m.TurnID
		curText = m.Text
	}
	flush()
	return strings.TrimRight(sb.String(), "\n")
}

// OperationID returns the stable identity of this submit unit:
// sha256(source \0 sessionID \0 start \0 end \0 sha256(conversation)).
// This format is a persistent protocol; do not change without migration.
func (b Batch) OperationID() string {
	convHash := sha256.Sum256([]byte(b.ConversationText()))
	preimage := strings.Join([]string{
		b.Source,
		b.SessionID,
		strconv.FormatInt(b.StartOffset, 10),
		strconv.FormatInt(b.EndOffset, 10),
		hex.EncodeToString(convHash[:]),
	}, "\x00")
	sum := sha256.Sum256([]byte(preimage))
	return hex.EncodeToString(sum[:])
}

// IsEmpty reports whether the batch has no visible conversation text.
func (b Batch) IsEmpty() bool {
	return strings.TrimSpace(b.ConversationText()) == ""
}
