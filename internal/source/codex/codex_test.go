package codex

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"session-memory/internal/checkpoint"
	"session-memory/internal/config"
)

func readFixtureBytes(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return data
}

func loadFixture(t *testing.T, name string) (SessionMeta, []byte) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	firstLine := data
	if i := strings.IndexByte(string(data), '\n'); i >= 0 {
		firstLine = data[:i+1]
	}
	meta, err := ParseSessionMeta(firstLine)
	if err != nil {
		t.Fatalf("ParseSessionMeta: %v", err)
	}
	return meta, data
}

func parseFixture(t *testing.T, name string, opts ParseOptions) ParseResult {
	t.Helper()
	meta, _ := loadFixture(t, name)
	res, err := Parse(context.Background(), filepath.Join("testdata", name), meta, opts)
	if err != nil {
		t.Fatalf("Parse(%s): %v", name, err)
	}
	return res
}

func utcOpts() ParseOptions { return ParseOptions{Location: time.UTC} }

// ---- classify ----

func TestClassifyOrder(t *testing.T) {
	inc, exc, orig := []string{"vscode"}, []string{"cli", "exec"}, []string{"Codex Desktop", "codex_work_desktop"}

	vscode := SessionMeta{Source: "vscode", Originator: "whatever"}
	cli := SessionMeta{Source: "cli", Originator: "Codex Desktop"}
	exec := SessionMeta{Source: "exec"}
	desktopNoSource := SessionMeta{Source: "", Originator: "Codex Desktop"}
	unknown := SessionMeta{Source: "newthing", Originator: "SomeOther"}

	if got := Classify(vscode, inc, exc, orig); got != ClassIncluded {
		t.Errorf("vscode = %s", got)
	}
	// exclude wins even when originator would include
	if got := Classify(cli, inc, exc, orig); got != ClassExcluded {
		t.Errorf("cli = %s", got)
	}
	if got := Classify(exec, inc, exc, orig); got != ClassExcluded {
		t.Errorf("exec = %s", got)
	}
	if got := Classify(desktopNoSource, inc, exc, orig); got != ClassIncluded {
		t.Errorf("desktop without source = %s", got)
	}
	if got := Classify(unknown, inc, exc, orig); got != ClassUnknown {
		t.Errorf("unknown = %s", got)
	}
}

func TestClassifySourceCaseInsensitive(t *testing.T) {
	meta := SessionMeta{Source: "VSCode"}
	if got := Classify(meta, []string{"vscode"}, nil, nil); got != ClassIncluded {
		t.Errorf("case mismatch: %s", got)
	}
}

func TestParseSessionMetaObjectSource(t *testing.T) {
	// source may be an object (subagent threads); must not crash
	line := `{"type":"session_meta","payload":{"session_id":"s1","cwd":"/w","source":{"subagent":{"parent_thread_id":"p"}},"originator":"Codex Desktop"}}`
	meta, err := ParseSessionMeta([]byte(line))
	if err != nil {
		t.Fatalf("object source crashed: %v", err)
	}
	if meta.Source != "<object>" {
		t.Errorf("object source = %q", meta.Source)
	}
	// classification: structured (subagent) source is hard-excluded,
	// never rescued by the originator fallback (design 5.2)
	if got := Classify(meta, []string{"vscode"}, []string{"cli"}, []string{"Codex Desktop"}); got != ClassExcluded {
		t.Errorf("subagent session must be excluded, got %s", got)
	}
}

func TestParseSessionMetaValidation(t *testing.T) {
	for _, bad := range []string{
		`{"type":"turn_context"}`,                                       // not session_meta
		`{"type":"session_meta","payload":{}}`,                          // missing id + cwd
		`{"type":"session_meta","payload":{"session_id":"s","cwd":""}}`, // missing cwd
		`not json`,
	} {
		if _, err := ParseSessionMeta([]byte(bad)); err == nil {
			t.Errorf("accepted bad meta: %s", bad)
		}
	}
}

// ---- parser behavior ----

func TestParserBasicDesktop(t *testing.T) {
	res := parseFixture(t, "desktop_vscode_basic.jsonl", utcOpts())
	if len(res.Batches) != 1 {
		t.Fatalf("batches = %d", len(res.Batches))
	}
	conv := res.Batches[0].ConversationText()
	for _, want := range []string{"帮我看一下为什么解析器在边界条件下崩溃", "问题出在 offset 计算没有考虑半行"} {
		if !strings.Contains(conv, want) {
			t.Errorf("missing %q in conversation", want)
		}
	}
	// response_item takes precedence: event agent_message duplicate dropped
	if strings.Contains(conv, "重复的 assistant 内容") {
		t.Error("duplicate event agent_message was kept")
	}
	// forbidden content classes
	for _, forbidden := range []string{
		"内部推理", "developer 指令", "cat /etc/passwd", "工具输出", "AKIAIOSFODNN7EXAMPLE",
	} {
		if strings.Contains(conv, forbidden) {
			t.Errorf("forbidden content leaked: %q", forbidden)
		}
	}
	if !res.LastTurnTerminal {
		t.Error("task_complete should mark turn terminal")
	}
	if len(res.UnknownRecords) != 0 {
		t.Errorf("unexpected unknown records: %v", res.UnknownRecords)
	}
}

func TestParserEventOnlyFallback(t *testing.T) {
	res := parseFixture(t, "event_only_fallback.jsonl", utcOpts())
	if len(res.Batches) != 1 {
		t.Fatalf("batches = %d", len(res.Batches))
	}
	conv := res.Batches[0].ConversationText()
	if !strings.Contains(conv, "只有 event_msg 表示的用户输入") {
		t.Error("event user_message fallback missing")
	}
	if !strings.Contains(conv, "兼容回退必须保留") {
		t.Error("event agent_message fallback missing")
	}
	if !res.LastTurnTerminal {
		t.Error("turn_aborted should mark terminal")
	}
}

func TestParserCrossDay(t *testing.T) {
	res := parseFixture(t, "cross_day_turns.jsonl", utcOpts())
	if len(res.Batches) != 2 {
		t.Fatalf("expected 2 date batches, got %d", len(res.Batches))
	}
	if res.Batches[0].Date != "20260831" || res.Batches[1].Date != "20260901" {
		t.Errorf("dates = %s, %s", res.Batches[0].Date, res.Batches[1].Date)
	}
	if !strings.Contains(res.Batches[0].ConversationText(), "八月三十一日") {
		t.Error("day 1 content in wrong batch")
	}
	if !strings.Contains(res.Batches[1].ConversationText(), "九月一日") {
		t.Error("day 2 content in wrong batch")
	}
}

func TestParserEdgeCases(t *testing.T) {
	res := parseFixture(t, "edge_cases.jsonl", utcOpts())
	if len(res.Batches) != 1 {
		t.Fatalf("batches = %d", len(res.Batches))
	}
	conv := res.Batches[0].ConversationText()
	if !strings.Contains(conv, "[Image omitted]") {
		t.Error("image placeholder missing")
	}
	if strings.Contains(conv, "iVBOR") {
		t.Error("image data leaked")
	}
	if strings.Contains(conv, "压缩后的历史摘要") {
		t.Error("compacted content leaked into batch")
	}
	if res.UnknownRecords["some_future_record_type"] != 1 {
		t.Errorf("unknown type not counted: %v", res.UnknownRecords)
	}
}

func TestParserHalfLineTail(t *testing.T) {
	dir := t.TempDir()
	meta, data := loadFixture(t, "half_line_tail.jsonl")
	full := len(data)
	half := `{"type":"response_item","payload":{"type":"message","id":"m3","role":"user","content":[{"type":"input_text","text":"尾部半`
	path := filepath.Join(dir, "rollout.jsonl")
	os.WriteFile(path, append(data, []byte(half)...), 0600)

	res, err := Parse(context.Background(), path, meta, utcOpts())
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	conv := ""
	for _, b := range res.Batches {
		conv += b.ConversationText()
	}
	if strings.Contains(conv, "尾部半") {
		t.Error("half line consumed")
	}
	if res.SafeEndOffset != int64(full) {
		t.Errorf("SafeEndOffset = %d, want %d (complete lines only)", res.SafeEndOffset, full)
	}
	_ = half
}

func TestParserRecordTooLarge(t *testing.T) {
	dir := t.TempDir()
	meta, _ := loadFixture(t, "half_line_tail.jsonl")
	big := `{"type":"response_item","payload":{"type":"message","id":"m","role":"user","content":[{"type":"input_text","text":"` +
		strings.Repeat("x", 200) + `"}]}}` + "\n"
	path := filepath.Join(dir, "r.jsonl")
	os.WriteFile(path, []byte(big), 0600)

	_, err := Parse(context.Background(), path, meta, ParseOptions{Location: time.UTC, MaxRecordBytes: 100})
	if err == nil || !strings.Contains(err.Error(), "record_too_large") {
		t.Errorf("expected record_too_large, got %v", err)
	}
}

func TestParserInvalidJSONStops(t *testing.T) {
	dir := t.TempDir()
	meta, _ := loadFixture(t, "half_line_tail.jsonl")
	ok := `{"type":"turn_context","payload":{"turn_id":"t1"}}` + "\n"
	bad := `{broken json` + "\n"
	after := `{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"after"}]}}` + "\n"
	path := filepath.Join(dir, "r.jsonl")
	os.WriteFile(path, []byte(ok+bad+after), 0600)

	res, err := Parse(context.Background(), path, meta, utcOpts())
	if err == nil || !strings.Contains(err.Error(), "invalid_json") {
		t.Fatalf("expected invalid_json error, got %v", err)
	}
	// offset stopped at the corrupt line, not past it
	if res.SafeEndOffset != int64(len(ok)) {
		t.Errorf("SafeEndOffset advanced past corrupt line: %d", res.SafeEndOffset)
	}
}

func TestParserBlankLinesSkipped(t *testing.T) {
	dir := t.TempDir()
	meta, _ := loadFixture(t, "desktop_vscode_basic.jsonl")
	// fixture with blank separator lines between records
	content := string(readFixtureBytes(t, "desktop_vscode_basic.jsonl"))
	withBlanks := strings.ReplaceAll(content, "\n{", "\n\n{")
	path := filepath.Join(dir, "blank.jsonl")
	os.WriteFile(path, []byte(withBlanks), 0600)

	res, err := Parse(context.Background(), path, meta, ParseOptions{Location: time.UTC})
	if err != nil {
		t.Fatalf("blank lines must not fail the parse: %v", err)
	}
	conv := ""
	for _, b := range res.Batches {
		conv += b.ConversationText()
	}
	if !strings.Contains(conv, "解析器在边界条件下崩溃") {
		t.Error("content lost after blank-line handling")
	}
	// full file consumed (all lines were newline-terminated)
	st, _ := os.Stat(path)
	if res.SafeEndOffset != st.Size() {
		t.Errorf("SafeEndOffset = %d, want %d", res.SafeEndOffset, st.Size())
	}
}

func TestParserFileReplaced(t *testing.T) {
	dir := t.TempDir()
	meta, _ := loadFixture(t, "half_line_tail.jsonl")
	path := filepath.Join(dir, "r.jsonl")
	os.WriteFile(path, []byte("{}\n"), 0600)
	_, err := Parse(context.Background(), path, meta, ParseOptions{StartOffset: 999, Location: time.UTC})
	if err != ErrFileReplaced {
		t.Errorf("expected ErrFileReplaced, got %v", err)
	}
}

// ---- discovery ----

func buildCodexTree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	files := map[string]string{
		"sessions/2026/08/30/rollout-old.jsonl":          "old",
		"sessions/2026/09/01/rollout-a.jsonl":            "a",
		"sessions/2026/09/01/rollout-b.jsonl":            "b",
		"sessions/2026/09/02/rollout-c.jsonl":            "c",
		"sessions/2026/09/02/notes.txt":                  "not jsonl",
		"archived_sessions/2026/09/01/rollout-arc.jsonl": "arc",
	}
	for rel, content := range files {
		p := filepath.Join(root, rel)
		os.MkdirAll(filepath.Dir(p), 0755)
		os.WriteFile(p, []byte(content), 0644)
	}
	return root
}

func TestDiscoverLookbackWindow(t *testing.T) {
	root := buildCodexTree(t)
	cfg := config.DefaultConfig()
	cfg.Agents.Codex.Home = root
	store := checkpoint.NewStore(filepath.Join(t.TempDir(), "state"))

	now := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	opts := DiscoverOptions{Now: now, IncludeArchived: true, Root: root}
	cands, err := Discover(cfg, store, opts)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	var names []string
	for _, c := range cands {
		names = append(names, filepath.Base(c.Path))
	}
	// lookback 48h from 09-02 noon covers 08-31..09-02 plus archived
	joined := strings.Join(names, ",")
	for _, want := range []string{"rollout-a.jsonl", "rollout-b.jsonl", "rollout-c.jsonl", "rollout-arc.jsonl"} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %s in %v", want, names)
		}
	}
	if strings.Contains(joined, "rollout-old") {
		t.Errorf("out-of-window file included: %v", names)
	}
	if strings.Contains(joined, "notes.txt") {
		t.Errorf("non-jsonl file included: %v", names)
	}
}

func TestDiscoverAllAndSince(t *testing.T) {
	root := buildCodexTree(t)
	cfg := config.DefaultConfig()
	cfg.Agents.Codex.Home = root
	store := checkpoint.NewStore(filepath.Join(t.TempDir(), "state"))

	all, err := Discover(cfg, store, DiscoverOptions{Now: time.Now(), All: true, Root: root})
	if err != nil {
		t.Fatal(err)
	}
	found := 0
	for _, c := range all {
		if strings.Contains(c.Path, "rollout-old") {
			found++
		}
	}
	if found != 1 {
		t.Errorf("--all must include historical files: %d", found)
	}

	since := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	bySince, _ := Discover(cfg, store, DiscoverOptions{Now: time.Now(), Since: &since, Root: root})
	for _, c := range bySince {
		if strings.Contains(c.Path, "2026/08/30") {
			t.Errorf("--since included pre-since file: %s", c.Path)
		}
	}
}

func TestDiscoverMissingHome(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Agents.Codex.Home = filepath.Join(t.TempDir(), "nonexistent")
	store := checkpoint.NewStore(filepath.Join(t.TempDir(), "state"))
	_, err := Discover(cfg, store, DiscoverOptions{Now: time.Now()})
	if err == nil || !strings.Contains(err.Error(), "codex_home_missing") {
		t.Errorf("expected codex_home_missing, got %v", err)
	}
}

func TestDiscoverMergesRetryable(t *testing.T) {
	root := buildCodexTree(t)
	cfg := config.DefaultConfig()
	cfg.Agents.Codex.Home = root
	stateDir := t.TempDir()
	store := checkpoint.NewStore(filepath.Join(stateDir, "state"))
	// a failed session pointing at the old, out-of-window file
	store.Save(checkpoint.State{
		SessionID: "s-old", Source: SourceName, Status: checkpoint.StatusFailed,
		RolloutPath: filepath.Join(root, "sessions/2026/08/30/rollout-old.jsonl"),
	})

	cands, err := Discover(cfg, store, DiscoverOptions{
		Now: time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC), Root: root,
	})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, c := range cands {
		if strings.Contains(c.Path, "rollout-old") {
			found = true
		}
	}
	if !found {
		t.Error("retryable pending/failed session not merged into candidates")
	}
}

func TestConfigFingerprintStability(t *testing.T) {
	a := ConfigFingerprint([]string{"vscode"}, []string{"cli", "exec"}, []string{"Codex Desktop"})
	b := ConfigFingerprint([]string{"vscode"}, []string{"cli", "exec"}, []string{"Codex Desktop"})
	c := ConfigFingerprint([]string{"vscode"}, []string{"cli"}, []string{"Codex Desktop"})
	if a != b {
		t.Error("fingerprint not deterministic")
	}
	if a == c {
		t.Error("fingerprint insensitive to config change")
	}
}

func TestDateInRangeWindowsSeparators(t *testing.T) {
	start, _ := time.Parse("2006/01/02", "2026/09/01")
	end, _ := time.Parse("2006/01/02", "2026/09/03")
	// Windows-style backslashes
	if !dateInRange("2026\\09\\02", &start, &end) {
		t.Errorf("expected 2026\\09\\02 to be in range")
	}
	if dateInRange("2026\\08\\30", &start, &end) {
		t.Errorf("expected 2026\\08\\30 to be out of range")
	}
}

// Spec 18 hard gate: canaries planted in tool outputs and developer/system
// messages must never reach the provider prompt — only user/assistant text
// is summarized.
func TestParserCanaryInExcludedRecordsNeverLeaks(t *testing.T) {
	dir := t.TempDir()
	toolCanary := "AKIAIOSFODNN7EXAMPLE"
	devCanary := "sk-ant-api03-AAAABBBBCCCCDDDDEEEEFFFFGGGGHHHHIIIIJJJJ"
	reasoningCanary := "ghp_16C7e42F292c6912E7710c838347Ae178B4a"
	content := `{"type":"session_meta","payload":{"session_id":"canary-1","cwd":"/w","originator":"Codex Desktop","source":"vscode","timestamp":"2026-09-01T01:00:00Z"}}` + "\n" +
		`{"type":"turn_context","payload":{"turn_id":"t1"}}` + "\n" +
		`{"type":"response_item","payload":{"type":"message","role":"developer","content":[{"type":"input_text","text":"` + devCanary + `"}]}}` + "\n" +
		`{"type":"response_item","payload":{"type":"reasoning","content":[{"type":"reasoning_text","text":"` + reasoningCanary + `"}]}}` + "\n" +
		`{"type":"response_item","payload":{"type":"custom_tool_call_output","output":"tool output ` + toolCanary + `"}}` + "\n" +
		`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"real user text"}]}}` + "\n"
	path := filepath.Join(dir, "canary.jsonl")
	os.WriteFile(path, []byte(content), 0644)

	meta, err := ParseSessionMeta([]byte(content[:strings.IndexByte(content, '\n')+1]))
	if err != nil {
		t.Fatal(err)
	}
	res, err := Parse(context.Background(), path, meta, ParseOptions{Location: time.UTC})
	if err != nil {
		t.Fatal(err)
	}
	conv := ""
	for _, b := range res.Batches {
		conv += b.ConversationText()
	}
	for _, c := range []string{toolCanary, devCanary, reasoningCanary} {
		if strings.Contains(conv, c) {
			t.Errorf("canary from excluded record type leaked into conversation: %s", c[:12])
		}
	}
	if !strings.Contains(conv, "real user text") {
		t.Error("legitimate user text lost")
	}
}
