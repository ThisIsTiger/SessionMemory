package codex

import (
	"strings"
	"testing"

	"session-memory/internal/agent"
)

// A thread that went to another computer and came back carries that computer's
// folder in the turn records and in the environment text; all of it must read
// as this computer's folder again.
func TestMapLineRewritesWorkspaceRootsAndEnvironmentText(t *testing.T) {
	pm := agent.PathMap{Rules: []agent.PathRule{{From: "/Users/b/Documents/App", To: "/Users/a/Documents/tiger/App"}}}
	cases := map[string]string{
		`{"type":"turn_context","payload":{"cwd":"/Users/b/Documents/App","workspace_roots":["/Users/b/Documents/App","/Users/b/.codex/viz/1"]}}`:                                                                                    `{"type":"turn_context","payload":{"cwd":"/Users/a/Documents/tiger/App","workspace_roots":["/Users/a/Documents/tiger/App","/Users/b/.codex/viz/1"]}}`,
		`{"type":"world_state","payload":{"environments":{"filesystem":"<filesystem><workspace_roots><root>/Users/b/Documents/App</root></workspace_roots></filesystem>"}}}`:                                                         `{"type":"world_state","payload":{"environments":{"filesystem":"<filesystem><workspace_roots><root>/Users/a/Documents/tiger/App</root></workspace_roots></filesystem>"}}}`,
		`{"type":"response_item","payload":{"type":"message","content":[{"type":"input_text","text":"<environment_context>\n  <cwd>/Users/b/Documents/App/sub</cwd>\n<root>/Users/b/Documents/App</root></environment_context>"}]}}`: `{"type":"response_item","payload":{"type":"message","content":[{"type":"input_text","text":"<environment_context>\n  <cwd>/Users/a/Documents/tiger/App/sub</cwd>\n<root>/Users/a/Documents/tiger/App</root></environment_context>"}]}}`,
	}
	for in, want := range cases {
		got, ok := mapLine([]byte(in), pm, "")
		if !ok || string(got) != want {
			t.Errorf("in:   %s\ngot:  %s\nwant: %s", in, got, want)
		}
	}
	// Prose that merely mentions the folder is conversation content and stays.
	prose := `{"type":"response_item","payload":{"type":"message","content":[{"type":"output_text","text":"see /Users/b/Documents/App and <root>/Users/b/Documents/App</root>"}]}}`
	if got, ok := mapLine([]byte(prose), pm, ""); ok || !strings.Contains(string(got), "/Users/b/Documents/App") {
		t.Errorf("conversation text was rewritten: %s", got)
	}
}
