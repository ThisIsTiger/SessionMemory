package convert

import (
	"strings"
	"testing"

	"session-memory/internal/canonical"
)

func msg(id string, role canonical.Role, text string, human bool) canonical.Item {
	return canonical.Item{ID: id, Kind: canonical.KindMessage, Role: role, Human: human, Norm: map[string]any{"text": text}}
}
func call(id, callID, name, args string) canonical.Item {
	return canonical.Item{ID: id, Kind: canonical.KindToolCall, CallID: callID, Norm: map[string]any{"name": name, "arguments": args}}
}
func result(id, callID, out string) canonical.Item {
	return canonical.Item{ID: id, Kind: canonical.KindToolResult, CallID: callID, Norm: map[string]any{"output": out}}
}

func TestRulesTable(t *testing.T) {
	items := []canonical.Item{
		{ID: "meta", Kind: canonical.KindSessionMeta},
		msg("sys", canonical.RoleDeveloper, "permissions...", false),
		msg("ctx", canonical.RoleUser, "<environment>", false),
		msg("u1", canonical.RoleUser, "hello", true),
		{ID: "r", Kind: canonical.KindReasoning},
		call("c1", "k1", "exec_command", `{"cmd":"ls"}`),
		result("r1", "k1", "a\nb"),
		call("c2", "k2", "web_search", `{"q":"x"}`),
		result("r2", "k2", "found"),
		call("c3", "k3", "exec_command", `{"cmd":"pwd"}`), // orphan call
		result("r9", "k9", "stray"),                       // orphan result
		msg("a1", canonical.RoleAssistant, "done", false),
		{ID: "cmp", Kind: canonical.KindCompaction, Norm: map[string]any{"readable": true, "text": "earlier summary"}},
		{ID: "enc", Kind: canonical.KindCompaction, Norm: map[string]any{"readable": false}},
		{ID: "unk", Kind: canonical.KindUnknown, Norm: map[string]any{"record_type": "event_msg/token_count"}},
		{ID: "br", Kind: canonical.KindMessage, Role: canonical.RoleAssistant, Branch: "b1", Norm: map[string]any{"text": "abandoned"}},
	}
	out, err := Convert(items, Options{ToolMap: map[string]string{"exec_command": "shell"}})
	if err != nil {
		t.Fatal(err)
	}
	var kinds []OutKind
	for _, r := range out.Records {
		kinds = append(kinds, r.Kind)
	}
	want := []OutKind{OutUser, OutToolCall, OutToolResult, OutAssistant, OutAssistant, OutAssistant, OutAssistant, OutSummary}
	if strings.Join(toS(kinds), ",") != strings.Join(toS(want), ",") {
		t.Fatalf("records = %v\nwant     %v", kinds, want)
	}
	// the mapped pair shares a fresh id and uses the target's tool name
	if out.Records[1].Tool.Name != "shell" || out.Records[1].Tool.CallID != out.Records[2].Tool.CallID || out.Records[1].Tool.CallID == "k1" {
		t.Errorf("tool pair: %+v %+v", out.Records[1].Tool, out.Records[2].Tool)
	}
	// unmapped tool, orphan call and orphan result are text, never native records
	for _, i := range []int{3, 4, 5} {
		r := out.Records[i]
		if r.Tool != nil || r.Note != "tool_as_text" {
			t.Errorf("record %d must be rendered text: %+v", i, r)
		}
	}
	if !strings.Contains(out.Records[3].Text, "[web_search]") || !strings.Contains(out.Records[3].Text, "[result]") {
		t.Errorf("text note: %q", out.Records[3].Text)
	}
	c := out.Report.Counts
	check := func(k canonical.Kind, kept, rend, drop int) {
		got := c[string(k)]
		if got.Kept != kept || got.Rendered != rend || got.Dropped != drop {
			t.Errorf("%s counts = %+v, want kept=%d rendered=%d dropped=%d", k, got, kept, rend, drop)
		}
	}
	check(canonical.KindMessage, 2, 0, 3) // u1,a1 kept; developer, non-human user, branch dropped
	check(canonical.KindReasoning, 0, 0, 1)
	check(canonical.KindToolCall, 1, 2, 0)   // c1 native; c2 text; c3 orphan text
	check(canonical.KindToolResult, 1, 2, 0) // r1 native; r2 text; r9 orphan text
	check(canonical.KindCompaction, 1, 0, 1)
	check(canonical.KindSessionMeta, 0, 0, 1)
	if out.Report.Dropped["event_msg/token_count"] != 1 || out.Report.Dropped["instruction_message"] != 1 || out.Report.Dropped["branch"] != 1 {
		t.Errorf("dropped types: %v", out.Report.Dropped)
	}
	if len(out.Report.ContextNotes) == 0 {
		t.Error("reasoning/branch notes missing")
	}
	// the report's totals equal the emitted records
	native, text := 0, 0
	for _, r := range out.Records {
		if r.Tool != nil {
			native++
		}
		if r.Note == "tool_as_text" {
			text++
		}
	}
	if native != c["tool_call"].Kept+c["tool_result"].Kept || text != 3 {
		t.Errorf("report disagrees with output: native=%d text=%d", native, text)
	}
}

func toS(k []OutKind) []string {
	var s []string
	for _, x := range k {
		s = append(s, string(x))
	}
	return s
}

func TestTruncationFenceAndDeterminism(t *testing.T) {
	big := strings.Repeat("é", 40000) // 80000 bytes
	items := []canonical.Item{call("c", "k", "x", "in"), result("r", "k", big+"\n```\ninner fence\n```")}
	out, _ := Convert(items, Options{ToolOutputCap: 1000})
	txt := out.Records[0].Text
	if !strings.Contains(txt, TruncationMark) || strings.Contains(txt, "inner fence") {
		t.Errorf("output not truncated: %d bytes", len(txt))
	}
	if !strings.Contains(txt, "```\nin\n```") {
		t.Errorf("input fence missing")
	}
	// the fence grows when the content contains one
	items2 := []canonical.Item{call("c", "k", "x", "in"), result("r", "k", "has ``` inside")}
	o2, _ := Convert(items2, Options{})
	if !strings.Contains(o2.Records[0].Text, "````") {
		t.Errorf("fence not widened: %q", o2.Records[0].Text)
	}
	a, _ := Convert(items2, Options{})
	b, _ := Convert(items2, Options{})
	if a.Records[0].Text != b.Records[0].Text || a.Report.TokenEstimate != b.Report.TokenEstimate {
		t.Error("Convert is not deterministic")
	}
	// truncation never splits a UTF-8 character
	for _, r := range out.Records[0].Text {
		if r == 0xFFFD {
			t.Error("invalid UTF-8 produced")
		}
	}
}

func TestSubagentItemsAreDroppedUnlessIncluded(t *testing.T) {
	sub := func(it canonical.Item) canonical.Item { it.Subagent = "projects/d/s/subagents/a.jsonl"; return it }
	items := []canonical.Item{
		msg("u1", canonical.RoleUser, "main question", true),
		call("c1", "k1", "web_search", `{}`),
		sub(msg("su", canonical.RoleUser, "SUBAGENT_TASK", true)),
		sub(call("sc", "k1", "web_search", `{}`)), // same call id, other stream
		sub(result("sr", "k1", "SUBAGENT_RESULT")),
		result("r1", "k1", "main result"),
		sub(msg("sa", canonical.RoleAssistant, "SUBAGENT_ANSWER", false)),
	}
	out, err := Convert(items, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Records) != 2 {
		t.Fatalf("want the main question and its tool note only: %+v", out.Records)
	}
	for _, r := range out.Records {
		if strings.Contains(r.Text, "SUBAGENT") {
			t.Fatalf("sub-agent text leaked into the main conversation: %q", r.Text)
		}
	}
	if !strings.Contains(out.Records[1].Text, "main result") {
		t.Fatalf("main call paired with the wrong result: %q", out.Records[1].Text)
	}
	if n := out.Report.Dropped["subagent"]; n != 4 {
		t.Fatalf("dropped sub-agent items = %d, want 4 (%v)", n, out.Report.Dropped)
	}
	in, err := Convert(items, Options{IncludeSubagents: true})
	if err != nil {
		t.Fatal(err)
	}
	var sawSub, sawMain bool
	for _, r := range in.Records {
		sawSub = sawSub || strings.Contains(r.Text, "SUBAGENT_RESULT")
		sawMain = sawMain || strings.Contains(r.Text, "main result")
	}
	if len(in.Records) != 5 || !sawSub || !sawMain {
		t.Fatalf("included sub-agent calls must pair within their own stream: %+v", in.Records)
	}
}
