package claude

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"session-memory/internal/config"
)

func testCfg(t *testing.T) config.Config {
	t.Helper()
	cfg := config.DefaultConfig()
	cfg.Home = t.TempDir()
	return cfg
}

// withFakeHome redirects the Claude settings path into a temp HOME so tests
// never touch the developer machine's real settings.json.
func withFakeHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // Windows os.UserHomeDir
	return home
}

func fixturePath(name string) string {
	return filepath.Join("testdata", name)
}

// ---- hook input ----

func TestParseHookInputValidation(t *testing.T) {
	// native absolute paths: "/abs/x" is not filepath.IsAbs on Windows
	// (no drive letter), so build them from TempDir
	dir := t.TempDir()
	tp := filepath.Join(dir, "x.jsonl")
	cwd := filepath.Join(dir, "proj")
	valid := fmt.Sprintf(`{"session_id":"s1","transcript_path":%q,"cwd":%q}`, tp, cwd)
	in, err := ParseHookInput([]byte(valid))
	if err != nil {
		t.Fatalf("valid input rejected: %v", err)
	}
	if in.SessionID != "s1" || in.Cwd != cwd {
		t.Errorf("parsed wrong: %+v", in)
	}
	for _, bad := range []string{
		fmt.Sprintf(`{"transcript_path":%q,"cwd":%q}`, tp, cwd),
		`{"session_id":"s1","transcript_path":"rel/x.jsonl","cwd":"` + cwd + `"}`,
		`{"session_id":"s1","transcript_path":"` + tp + `","cwd":"rel/proj"}`,
		`{not json`,
	} {
		if _, err := ParseHookInput([]byte(bad)); err == nil {
			t.Errorf("invalid payload accepted: %s", bad)
		}
	}
}

func TestEnqueuePrivatePerms(t *testing.T) {
	cfg := testCfg(t)
	path, err := Enqueue(cfg, HookInput{SessionID: "s-1", TranscriptPath: "/a/b.jsonl", Cwd: "/a"})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if runtime.GOOS != "windows" {
		info, _ := os.Stat(path)
		if info.Mode().Perm() != 0600 {
			t.Errorf("queue file perm = %o", info.Mode().Perm())
		}
	}
	// session id with hostile characters is sanitized
	path2, _ := Enqueue(cfg, HookInput{SessionID: "../evil/id", TranscriptPath: "/a/b.jsonl", Cwd: "/a"})
	if strings.Contains(filepath.Base(path2), "..") {
		t.Errorf("unsafe queue name: %s", path2)
	}
}

// ---- parser ----

func TestParseBasicSession(t *testing.T) {
	res, err := Parse(context.Background(), fixturePath("basic_session.jsonl"), "s1", "/proj",
		ParseOptions{Location: time.UTC})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(res.Batches) != 1 {
		t.Fatalf("batches = %d", len(res.Batches))
	}
	conv := res.Batches[0].ConversationText()
	if !strings.Contains(conv, "[User]\n阅读这个项目的 README 并总结") {
		t.Errorf("user text missing: %q", conv)
	}
	if !strings.Contains(conv, "[Assistant]\n这是一个 Go 项目") || !strings.Contains(conv, "我建议先跑测试。") {
		t.Errorf("assistant text missing: %q", conv)
	}
	for _, forbidden := range []string{"internal reasoning", "tool output", "session init", "ignored type"} {
		if strings.Contains(conv, forbidden) {
			t.Errorf("forbidden content leaked: %q in %q", forbidden, conv)
		}
	}
	if res.Batches[0].Date == "" {
		t.Error("batch date empty")
	}
}

func TestParseCrossDayBatches(t *testing.T) {
	res, err := Parse(context.Background(), fixturePath("cross_day.jsonl"), "s1", "/proj",
		ParseOptions{Location: time.UTC})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(res.Batches) != 2 {
		t.Fatalf("expected 2 date batches, got %d", len(res.Batches))
	}
	if res.Batches[0].Date != "20260831" || res.Batches[1].Date != "20260901" {
		t.Errorf("dates = %s, %s", res.Batches[0].Date, res.Batches[1].Date)
	}
	// batches must cover disjoint offsets and be ordered
	if res.Batches[0].EndOffset > res.Batches[1].StartOffset {
		t.Errorf("offsets overlap: %d > %d", res.Batches[0].EndOffset, res.Batches[1].StartOffset)
	}
}

func TestParseHalfLineNotConsumed(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "t.jsonl")
	full := `{"type":"user","message":{"role":"user","content":"complete line"},"timestamp":"2026-09-01T10:00:00Z"}` + "\n"
	half := `{"type":"user","message":{"role":"user","content":"partial wit`
	os.WriteFile(path, []byte(full+half), 0600)

	res, err := Parse(context.Background(), path, "s", "/p", ParseOptions{Location: time.UTC})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(res.Batches) != 1 {
		t.Fatalf("batches = %d", len(res.Batches))
	}
	if !strings.Contains(res.Batches[0].ConversationText(), "complete line") {
		t.Error("complete line missing")
	}
	if strings.Contains(res.Batches[0].ConversationText(), "partial") {
		t.Error("half line was consumed!")
	}
	if res.SafeEndOffset != int64(len(full)) {
		t.Errorf("SafeEndOffset = %d, want %d", res.SafeEndOffset, len(full))
	}
}

func TestParseOffsetResume(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "t.jsonl")
	line1 := `{"type":"user","message":{"role":"user","content":"one"},"timestamp":"2026-09-01T10:00:00Z"}` + "\n"
	line2 := `{"type":"user","message":{"role":"user","content":"two"},"timestamp":"2026-09-01T10:01:00Z"}` + "\n"
	// first run sees only the first complete line (session still growing)
	os.WriteFile(path, []byte(line1), 0600)

	first, err := Parse(context.Background(), path, "s", "/p", ParseOptions{Location: time.UTC})
	if err != nil {
		t.Fatalf("first Parse: %v", err)
	}
	if len(first.Batches) != 1 || !strings.Contains(first.Batches[0].ConversationText(), "one") {
		t.Fatalf("first parse wrong: %+v", first.Batches)
	}
	if first.SafeEndOffset != int64(len(line1)) {
		t.Fatalf("SafeEndOffset = %d, want %d", first.SafeEndOffset, len(line1))
	}

	// session grows: second run resumes from the committed offset
	os.WriteFile(path, []byte(line1+line2), 0600)
	second, err := Parse(context.Background(), path, "s", "/p",
		ParseOptions{StartOffset: first.SafeEndOffset, Location: time.UTC})
	if err != nil {
		t.Fatalf("second Parse: %v", err)
	}
	if len(second.Batches) != 1 || !strings.Contains(second.Batches[0].ConversationText(), "two") {
		t.Fatalf("resume from offset lost data: %+v", second.Batches)
	}
	if strings.Contains(second.Batches[0].ConversationText(), "one") {
		t.Error("already-consumed content re-parsed")
	}
}

func TestParseBlankLinesSkipped(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "t.jsonl")
	// double newlines create blank separator lines between records
	base := `{"type":"user","message":{"role":"user","content":"one"},"timestamp":"2026-09-01T10:00:00Z"}` + "\n"
	blank := "\n"
	line3 := `{"type":"user","message":{"role":"user","content":"two"},"timestamp":"2026-09-01T10:01:00Z"}` + "\n"
	os.WriteFile(path, []byte(base+blank+line3), 0600)

	res, err := Parse(context.Background(), path, "s", "/p", ParseOptions{Location: time.UTC})
	if err != nil {
		t.Fatalf("blank line must not fail: %v", err)
	}
	conv := ""
	for _, b := range res.Batches {
		conv += b.ConversationText()
	}
	if !strings.Contains(conv, "one") || !strings.Contains(conv, "two") {
		t.Errorf("content lost: %q", conv)
	}
	st, _ := os.Stat(path)
	if res.SafeEndOffset != st.Size() {
		t.Errorf("SafeEndOffset = %d, want %d", res.SafeEndOffset, st.Size())
	}
}

func TestParseFileReplaced(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "t.jsonl")
	os.WriteFile(path, []byte(`{"type":"user","message":{"role":"user","content":"x"}}`+"\n"), 0600)
	_, err := Parse(context.Background(), path, "s", "/p", ParseOptions{StartOffset: 1000, Location: time.UTC})
	if err == nil {
		t.Error("shrunken file must be ErrFileReplaced")
	}
}

func TestParseCorruptLineStopsWithoutAdvancingOffset(t *testing.T) {
	// a complete-but-corrupt line must stop the parse WITHOUT advancing
	// SafeEndOffset past it (spec 8.2): the saved checkpoint stays before
	// the failure so the record is retried once healed, never skipped (F14)
	dir := t.TempDir()
	path := filepath.Join(dir, "t.jsonl")
	line1 := `{"type":"user","message":{"role":"user","content":"good"},"timestamp":"2026-09-01T10:00:00Z"}` + "\n"
	corrupt := `{"type":"user","message":{"role":"user","content":` + "\n" // no closing braces
	line3 := `{"type":"user","message":{"role":"user","content":"after"},"timestamp":"2026-09-01T10:02:00Z"}` + "\n"
	os.WriteFile(path, []byte(line1+corrupt+line3), 0600)

	res, err := Parse(context.Background(), path, "s", "/p", ParseOptions{Location: time.UTC})
	if err == nil || !strings.Contains(err.Error(), "invalid_json") {
		t.Fatalf("corrupt line must error, got %v", err)
	}
	if res.SafeEndOffset != int64(len(line1)) {
		t.Errorf("SafeEndOffset = %d, want %d (must NOT pass the corrupt line)", res.SafeEndOffset, len(line1))
	}

	// healing the corrupt line and resuming from the checkpoint must
	// recover the record: nothing was silently swallowed
	healed := `{"type":"user","message":{"role":"user","content":"healed"},"timestamp":"2026-09-01T10:01:00Z"}` + "\n"
	os.WriteFile(path, []byte(line1+healed+line3), 0600)
	resume, err := Parse(context.Background(), path, "s", "/p",
		ParseOptions{StartOffset: res.SafeEndOffset, Location: time.UTC})
	if err != nil {
		t.Fatalf("resume after heal: %v", err)
	}
	conv := ""
	for _, b := range resume.Batches {
		conv += b.ConversationText()
	}
	if !strings.Contains(conv, "healed") || !strings.Contains(conv, "after") {
		t.Errorf("healed run lost records: %q", conv)
	}
}

// ---- install/uninstall non-destructive ----

func TestInstallUninstallPreservesOthers(t *testing.T) {
	withFakeHome(t)

	// pre-existing user hooks and settings that must survive untouched
	settings := map[string]interface{}{
		"model": "custom-model",
		"hooks": map[string]interface{}{
			"SessionEnd": []interface{}{
				map[string]interface{}{
					"hooks": []interface{}{
						map[string]interface{}{"type": "command", "command": "/usr/bin/other-tool on session end"},
					},
				},
			},
			"PreToolUse": []interface{}{
				map[string]interface{}{
					"hooks": []interface{}{
						map[string]interface{}{"type": "command", "command": "clerk punch"},
					},
				},
			},
		},
	}
	os.MkdirAll(filepath.Dir(SettingsPath()), 0755)
	data, _ := json.MarshalIndent(settings, "", "  ")
	os.WriteFile(SettingsPath(), data, 0600)

	installed, err := Install()
	if err != nil || !installed {
		t.Fatalf("Install: %v installed=%v", err, installed)
	}
	// idempotent
	installed2, err := Install()
	if err != nil || installed2 {
		t.Errorf("second Install should be no-op: %v %v", err, installed2)
	}

	raw, _ := os.ReadFile(SettingsPath())
	var after map[string]interface{}
	json.Unmarshal(raw, &after)

	// user key preserved
	if after["model"] != "custom-model" {
		t.Error("user setting lost")
	}
	hooks := after["hooks"].(map[string]interface{})
	// other SessionEnd hook preserved
	se := hooks["SessionEnd"].([]interface{})
	foundOther, foundOurs := false, false
	for _, e := range se {
		em := e.(map[string]interface{})
		for _, it := range em["hooks"].([]interface{}) {
			cmd := it.(map[string]interface{})["command"].(string)
			if strings.Contains(cmd, "other-tool") {
				foundOther = true
			}
			if IsManaged(cmd) {
				foundOurs = true
			}
		}
	}
	if !foundOther {
		t.Error("pre-existing hook removed — MUST be preserved")
	}
	if !foundOurs {
		t.Error("session-memory hook not appended")
	}
	// PreToolUse untouched
	if _, ok := hooks["PreToolUse"]; !ok {
		t.Error("PreToolUse hooks removed")
	}

	// uninstall removes only ours
	removed, err := Uninstall()
	if err != nil || !removed {
		t.Fatalf("Uninstall: %v %v", err, removed)
	}
	raw, _ = os.ReadFile(SettingsPath())
	var final map[string]interface{}
	json.Unmarshal(raw, &final)
	hooks = final["hooks"].(map[string]interface{})
	se = hooks["SessionEnd"].([]interface{})
	for _, e := range se {
		em := e.(map[string]interface{})
		for _, it := range em["hooks"].([]interface{}) {
			cmd := it.(map[string]interface{})["command"].(string)
			if IsManaged(cmd) {
				t.Errorf("session-memory hook survived uninstall: %s", cmd)
			}
		}
	}
	if final["model"] != "custom-model" {
		t.Error("user setting lost after uninstall")
	}

	// uninstall with nothing installed: no-op, no error
	removed2, err := Uninstall()
	if err != nil || removed2 {
		t.Errorf("empty uninstall should be no-op: %v %v", err, removed2)
	}
}

func TestUninstallRefusesAmbiguousOwnership(t *testing.T) {
	withFakeHome(t)
	// a hook that mentions session-memory but is NOT ours (no claude-session-end subcommand)
	settings := map[string]interface{}{
		"hooks": map[string]interface{}{
			"SessionEnd": []interface{}{
				map[string]interface{}{
					"hooks": []interface{}{
						map[string]interface{}{"type": "command", "command": "echo session-memory-ish"},
					},
				},
			},
		},
	}
	os.MkdirAll(filepath.Dir(SettingsPath()), 0755)
	data, _ := json.MarshalIndent(settings, "", "  ")
	os.WriteFile(SettingsPath(), data, 0600)

	removed, err := Uninstall()
	if err != nil || removed {
		t.Errorf("foreign hook must not be removed: %v %v", err, removed)
	}
	raw, _ := os.ReadFile(SettingsPath())
	if !strings.Contains(string(raw), "session-memory-ish") {
		t.Error("foreign hook was deleted")
	}
}

func TestStatus(t *testing.T) {
	withFakeHome(t)
	if ok, err := Status(); err != nil || ok {
		t.Errorf("fresh install Status = %v %v", ok, err)
	}
	if _, err := Install(); err != nil {
		t.Fatal(err)
	}
	if ok, err := Status(); err != nil || !ok {
		t.Errorf("installed Status = %v %v", ok, err)
	}
}
