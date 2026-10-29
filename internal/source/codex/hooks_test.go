package codex

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func cmds(exe string) map[string]string {
	out := map[string]string{}
	for _, ev := range HookEvents {
		out[ev] = HookCommand(exe, "", ev)
	}
	return out
}

func readJSON(t *testing.T, path string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("%s is not JSON: %v\n%s", path, err, b)
	}
	return m
}

func TestHookCommandAndRecognition(t *testing.T) {
	for _, tc := range []struct {
		cmd  string
		ours bool
	}{
		{HookCommand("/Users/a/.local/bin/session-memory", "", "Stop"), true},
		{HookCommand("/Users/a b/it's/session-memory", "", "SessionStart"), true}, // spaces and a quote in the path
		{HookCommand("/x/session-memory", "/tmp/my state", "Stop"), true},         // SM_HOME carried along
		{"/usr/local/bin/session-memory hook --event Stop", true},
		{"session-memory hook", true},
		{`"C:\\tools\\session-memory.exe" hook --event Stop`, true},
		{"SM_HOOK=1 '/x/renamed-binary' hook --event Stop", true}, // whatever the program is called
		{"/x/renamed-binary hook --event Stop", false},            // without the mark, only the name counts
		{"echo hook", false},
		{"/usr/bin/python3 /x/hook.py", false},
		{"/x/session-memory relay --quiet", false},
		{"/x/session-memory-other hook --event Stop", false},
		{"/x/my-session-memory hook", false},
		{"", false},
		{"FOO=bar", false},
	} {
		if got := IsOurHook(tc.cmd); got != tc.ours {
			t.Errorf("IsOurHook(%q) = %v, want %v", tc.cmd, got, tc.ours)
		}
	}
	// the quoted path survives a round trip through the shell-like splitter
	f := shellFields(HookCommand("/Users/a b/it's/session-memory", "/tmp/my state", "Stop"))
	want := []string{"SM_HOOK=1", "SM_HOME=/tmp/my state", "/Users/a b/it's/session-memory", "hook", "--event", "Stop"}
	if !reflect.DeepEqual(f, want) {
		t.Errorf("fields = %q, want %q", f, want)
	}
}

func TestInstallCreatesTheFileAndIsIdempotent(t *testing.T) {
	home := t.TempDir()
	c := cmds("/x/session-memory")
	changed, err := InstallHooks(home, c)
	if err != nil || !changed {
		t.Fatalf("first install: changed=%v err=%v", changed, err)
	}
	doc := readJSON(t, HooksPath(home))
	hooks := doc["hooks"].(map[string]any)
	if len(hooks) != len(HookEvents) {
		t.Fatalf("events = %v", hooks)
	}
	for _, ev := range HookEvents {
		groups := hooks[ev].([]any)
		h := groups[0].(map[string]any)["hooks"].([]any)[0].(map[string]any)
		if h["type"] != "command" || h["command"] != c[ev] || h["timeout"].(float64) != hookTimeout {
			t.Errorf("%s handler = %v", ev, h)
		}
	}
	before, _ := os.ReadFile(HooksPath(home))
	changed, err = InstallHooks(home, c)
	if err != nil || changed {
		t.Fatalf("second install: changed=%v err=%v", changed, err)
	}
	after, _ := os.ReadFile(HooksPath(home))
	if string(before) != string(after) {
		t.Fatal("an install that changed nothing rewrote the file")
	}
	if fileExists(HooksPath(home) + ".sm-bak") {
		t.Fatal("a file that did not exist before has no backup to keep")
	}
	st, err := ReadHooks(home, c)
	if err != nil || !st.Exists {
		t.Fatalf("state %+v %v", st, err)
	}
	for _, ev := range HookEvents {
		if !st.Installed[ev] || !st.Current[ev] {
			t.Errorf("%s: installed=%v current=%v", ev, st.Installed[ev], st.Current[ev])
		}
	}
}

const usersHooks = `{
  "description": "my hooks",
  "hooks": {
    "PreToolUse": [
      {"matcher": "Bash", "hooks": [{"type": "command", "command": "/usr/bin/python3 /x/policy.py", "timeout": 30, "statusMessage": "Checking"}]}
    ],
    "Stop": [
      {"hooks": [{"type": "command", "command": "/x/notify.sh done", "async": true}]},
      "not-an-object-group"
    ],
    "SessionEnd": [
      {"hooks": [{"type": "command", "command": "/x/bye.sh"}]}
    ]
  },
  "extra": {"keep": [1, 2, 3]}
}`

// Everything the user has stays exactly as it was, through an install and an
// uninstall; the file as it was is kept once beside it.
func TestInstallAndRemoveLeaveTheUsersHooksAlone(t *testing.T) {
	home := t.TempDir()
	path := HooksPath(home)
	if err := os.WriteFile(path, []byte(usersHooks), 0600); err != nil {
		t.Fatal(err)
	}
	var original map[string]any
	json.Unmarshal([]byte(usersHooks), &original)

	c := cmds("/x/session-memory")
	if changed, err := InstallHooks(home, c); err != nil || !changed {
		t.Fatalf("install: %v %v", changed, err)
	}
	doc := readJSON(t, path)
	if !reflect.DeepEqual(doc["extra"], original["extra"]) || doc["description"] != "my hooks" {
		t.Fatalf("top-level keys changed: %v", doc)
	}
	hooks := doc["hooks"].(map[string]any)
	if !reflect.DeepEqual(hooks["PreToolUse"], original["hooks"].(map[string]any)["PreToolUse"]) ||
		!reflect.DeepEqual(hooks["SessionEnd"], original["hooks"].(map[string]any)["SessionEnd"]) {
		t.Fatalf("the user's other events changed: %v", hooks)
	}
	stop := hooks["Stop"].([]any)
	if len(stop) != 3 || stop[2].(map[string]any)["hooks"].([]any)[0].(map[string]any)["command"] != c["Stop"] {
		t.Fatalf("Stop = %v", stop)
	}
	if !reflect.DeepEqual(stop[:2], original["hooks"].(map[string]any)["Stop"]) {
		t.Fatalf("the user's Stop hooks changed: %v", stop[:2])
	}
	if st, _ := os.Stat(path); st.Mode().Perm() != 0600 {
		t.Fatalf("mode = %v", st.Mode())
	}
	bak, err := os.ReadFile(path + ".sm-bak")
	if err != nil || string(bak) != usersHooks {
		t.Fatalf("backup = %q, %v", bak, err)
	}

	n, err := RemoveHooks(home)
	if err != nil || n != len(HookEvents) {
		t.Fatalf("remove: %d %v", n, err)
	}
	got := readJSON(t, path)
	if !reflect.DeepEqual(got, original) {
		t.Fatalf("after remove:\n%v\nwant:\n%v", got, original)
	}
	if n, err := RemoveHooks(home); err != nil || n != 0 {
		t.Fatalf("second remove: %d %v", n, err)
	}
	// a later install keeps the first backup (the file as the user had it)
	InstallHooks(home, c)
	if b, _ := os.ReadFile(path + ".sm-bak"); string(b) != usersHooks {
		t.Fatal("the backup was overwritten")
	}
}

func TestRemoveTakesOnlyOurHandlerOutOfASharedGroup(t *testing.T) {
	home := t.TempDir()
	c := cmds("/x/session-memory")
	shared := map[string]any{"hooks": map[string]any{"Stop": []any{
		map[string]any{"matcher": "x", "hooks": []any{
			map[string]any{"type": "command", "command": "/x/mine.sh"},
			map[string]any{"type": "command", "command": c["Stop"]},
		}},
	}}}
	b, _ := json.Marshal(shared)
	os.WriteFile(HooksPath(home), b, 0644)
	if n, err := RemoveHooks(home); err != nil || n != 1 {
		t.Fatalf("remove: %d %v", n, err)
	}
	doc := readJSON(t, HooksPath(home))
	g := doc["hooks"].(map[string]any)["Stop"].([]any)[0].(map[string]any)
	hs := g["hooks"].([]any)
	if g["matcher"] != "x" || len(hs) != 1 || hs[0].(map[string]any)["command"] != "/x/mine.sh" {
		t.Fatalf("group = %v", g)
	}
}

func TestInstallReplacesAnOldCommandAndCollapsesDuplicates(t *testing.T) {
	home := t.TempDir()
	old := cmds("/old/place/session-memory")
	InstallHooks(home, old)
	now := cmds("/new/place/session-memory")
	st, _ := ReadHooks(home, now)
	for _, ev := range HookEvents {
		if !st.Installed[ev] || st.Current[ev] {
			t.Fatalf("%s: installed=%v current=%v (an old copy is installed, not the current one)", ev, st.Installed[ev], st.Current[ev])
		}
	}
	if changed, err := InstallHooks(home, now); err != nil || !changed {
		t.Fatalf("update: %v %v", changed, err)
	}
	b, _ := os.ReadFile(HooksPath(home))
	if strings.Contains(string(b), "/old/place") || strings.Count(string(b), "/new/place") != len(HookEvents) {
		t.Fatalf("not replaced:\n%s", b)
	}
	// two of ours under one event (a hand edit) become one
	dup := map[string]any{"hooks": map[string]any{"Stop": []any{
		map[string]any{"hooks": []any{map[string]any{"type": "command", "command": now["Stop"]}}},
		map[string]any{"hooks": []any{map[string]any{"type": "command", "command": now["Stop"]}}},
	}}}
	raw, _ := json.Marshal(dup)
	os.WriteFile(HooksPath(home), raw, 0644)
	if _, err := InstallHooks(home, map[string]string{"Stop": now["Stop"]}); err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(readAll(t, HooksPath(home)), now["Stop"]); n != 1 {
		t.Fatalf("%d copies left", n)
	}
}

func readAll(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// A file that is not what hooks.json should be is never overwritten.
func TestUnreadableHooksFilesAreLeftAlone(t *testing.T) {
	for name, content := range map[string]string{
		"not json":          "{ this is not json",
		"array on top":      `[1,2]`,
		"hooks not object":  `{"hooks": [1]}`,
		"event not a list":  `{"hooks": {"Stop": {"hooks": []}}}`,
		"json with comment": "// mine\n{\"hooks\": {}}",
	} {
		home := t.TempDir()
		path := HooksPath(home)
		os.WriteFile(path, []byte(content), 0644)
		if _, err := InstallHooks(home, cmds("/x/session-memory")); err == nil {
			t.Errorf("%s: install did not refuse", name)
		}
		if _, err := RemoveHooks(home); err == nil {
			t.Errorf("%s: remove did not refuse", name)
		}
		if got := readAll(t, path); got != content {
			t.Errorf("%s: file changed to %q", name, got)
		}
		if fileExists(path + ".sm-bak") {
			t.Errorf("%s: a backup was made of a file that was not touched", name)
		}
	}
}

func TestEmptyFilesAndRemovalOfTheLastHook(t *testing.T) {
	for _, content := range []string{"", "  \n", "{}", `{"hooks":{}}`} {
		home := t.TempDir()
		os.WriteFile(HooksPath(home), []byte(content), 0644)
		if changed, err := InstallHooks(home, cmds("/x/session-memory")); err != nil || !changed {
			t.Fatalf("%q: %v %v", content, changed, err)
		}
		if _, err := RemoveHooks(home); err != nil {
			t.Fatal(err)
		}
		if content == "" || strings.TrimSpace(content) == "" {
			continue
		}
		// a file that held nothing but our hooks goes away with them
		if fileExists(HooksPath(home)) {
			t.Errorf("%q: an empty hooks file was left behind: %q", content, readAll(t, HooksPath(home)))
		}
	}
	home := t.TempDir()
	if n, err := RemoveHooks(home); err != nil || n != 0 {
		t.Fatalf("no file: %d %v", n, err)
	}
	st, err := ReadHooks(home, cmds("/x/session-memory"))
	if err != nil || st.Exists || len(st.Installed) != 0 {
		t.Fatalf("no file: %+v %v", st, err)
	}
}

// fakeCodex stands in for the Codex program: it answers the app-server
// requests the way the real one does (checked against codex 0.162).
func fakeCodex(t *testing.T, hooksReply string) string {
	t.Helper()
	dir := t.TempDir()
	script := `#!/bin/sh
while IFS= read -r line; do
  case "$line" in
    *'"method":"initialize"'*) echo '{"id":1,"result":{"userAgent":"x","codexHome":"/h"}}' ;;
    *'"method":"hooks/list"'*) echo '` + hooksReply + `' ;;
  esac
done
`
	p := filepath.Join(dir, "codex")
	if err := os.WriteFile(p, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestHookTrustReadsWhatCodexSays(t *testing.T) {
	// one line, as the app-server sends it
	reply := `{"id":2,"result":{"data":[{"cwd":"/h","hooks":[` +
		`{"eventName":"sessionStart","command":"'/x/session-memory' hook --event SessionStart","trustStatus":"untrusted","enabled":true},` +
		`{"eventName":"stop","command":"'/x/session-memory' hook --event Stop","trustStatus":"trusted","enabled":true},` +
		`{"eventName":"stop","command":"/x/notify.sh done","trustStatus":"untrusted","enabled":true}` +
		`],"warnings":[],"errors":[]}]}}`
	got, err := SessionMemoryHookTrust(context.Background(), t.TempDir(), fakeCodex(t, reply))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 { // the user's own hook is not ours to report
		t.Fatalf("got %+v", got)
	}
	by := map[string]string{}
	for _, h := range got {
		by[strings.ToLower(h.Event)] = h.Trust
	}
	if by["sessionstart"] != "untrusted" || by["stop"] != "trusted" {
		t.Fatalf("trust = %v", by)
	}
}

func TestHookTrustReportsFailuresInsteadOfGuessing(t *testing.T) {
	if _, err := SessionMemoryHookTrust(context.Background(), t.TempDir(), "/nonexistent/codex"); err == nil {
		t.Error("a missing Codex was not reported")
	}
	if _, err := SessionMemoryHookTrust(context.Background(), t.TempDir(), fakeCodex(t, `{"id":2,"error":{"code":-32601,"message":"method not found"}}`)); err == nil || !strings.Contains(err.Error(), "method not found") {
		t.Errorf("an error reply: %v", err)
	}
	if _, err := SessionMemoryHookTrust(context.Background(), t.TempDir(), fakeCodex(t, `{"id":2,"result":"nonsense"}`)); err == nil {
		t.Error("an unreadable reply was accepted")
	}
}

// With a real Codex on the machine: the file written here is accepted as
// hooks, listed per event, and not run before the user has trusted it.
func TestRealCodexListsTheInstalledHooksAsUntrusted(t *testing.T) {
	if testing.Short() {
		t.Skip("starts the installed Codex")
	}
	bin, err := findCodexBinary("")
	if err != nil {
		t.Skip("no Codex on this machine")
	}
	home := t.TempDir()
	c := cmds("/x/session-memory")
	if _, err := InstallHooks(home, c); err != nil {
		t.Fatal(err)
	}
	got, err := SessionMemoryHookTrust(context.Background(), home, bin)
	if err != nil {
		t.Fatal(err)
	}
	by := map[string]string{}
	for _, h := range got {
		by[strings.ToLower(h.Event)] = h.Trust
		if !h.Enabled {
			t.Errorf("%s is listed as disabled", h.Event)
		}
	}
	for _, ev := range HookEvents {
		if by[strings.ToLower(ev)] != "untrusted" {
			t.Errorf("%s: Codex says %q, want untrusted (all: %v)", ev, by[strings.ToLower(ev)], by)
		}
	}
}

// An event an earlier version listened to is taken out on the next install,
// together with nothing of the user's.
func TestInstallDropsOurHandlerFromEventsNoLongerUsed(t *testing.T) {
	home := t.TempDir()
	old := HookCommand("/x/session-memory", "", "SessionStart")
	content := map[string]any{"hooks": map[string]any{
		"SessionStart": []any{
			map[string]any{"hooks": []any{map[string]any{"type": "command", "command": old}}},
			map[string]any{"hooks": []any{map[string]any{"type": "command", "command": "/x/mine.sh"}}},
		},
	}}
	b, _ := json.Marshal(content)
	os.WriteFile(HooksPath(home), b, 0644)
	if changed, err := InstallHooks(home, cmds("/x/session-memory")); err != nil || !changed {
		t.Fatalf("install: %v %v", changed, err)
	}
	doc := readJSON(t, HooksPath(home))["hooks"].(map[string]any)
	ss := doc["SessionStart"].([]any)
	if len(ss) != 1 || ss[0].(map[string]any)["hooks"].([]any)[0].(map[string]any)["command"] != "/x/mine.sh" {
		t.Fatalf("SessionStart = %v", ss)
	}
	if _, ok := doc["Stop"]; !ok {
		t.Fatal("Stop was not installed")
	}
	if len(HookEvents) != 1 || HookEvents[0] != "Stop" {
		t.Fatalf("events = %v", HookEvents)
	}
}
