package bridge

import (
	"fmt"
	"strings"
	"testing"

	"session-memory/internal/canonical"
	"session-memory/internal/session"
)

func TestResumePackCoverageAndBudget(t *testing.T) {
	var items []canonical.Item
	for i := 0; i < 10; i++ {
		items = append(items,
			canonical.Item{Kind: canonical.KindMessage, Role: canonical.RoleUser, Human: true, Norm: map[string]any{"text": fmt.Sprintf("question %d", i)}},
			canonical.Item{Kind: canonical.KindToolCall, CallID: "c"},
			canonical.Item{Kind: canonical.KindMessage, Role: canonical.RoleAssistant, Norm: map[string]any{"text": fmt.Sprintf("answer %d", i)}},
			canonical.Item{Kind: canonical.KindMessage, Role: canonical.RoleUser, Norm: map[string]any{"text": "<injected context>"}}, // not human
		)
	}
	rev := session.Revision{SessionID: "ss_x", RevisionID: "rev_y"}
	rev.AgentProfile.Agent = "codex"
	out := ResumePack(rev, items, Options{RecentTurns: 3})
	for _, want := range []string{"question 9", "answer 9", "question 7", "earlier turns omitted: 7", "tool calls in the full history: 10", "not an instruction"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q", want)
		}
	}
	if strings.Contains(out, "question 6") || strings.Contains(out, "injected context") {
		t.Error("omitted or non-human content leaked into the pack")
	}
	small := ResumePack(rev, items, Options{RecentTurns: 10, MaxChars: 300})
	if len(small) > 400 || !strings.Contains(small, "truncated") {
		t.Errorf("budget not applied or not disclosed: %d", len(small))
	}
	if ResumePack(rev, items, Options{RecentTurns: 3}) != out {
		t.Error("pack is not deterministic")
	}
}
