package hookfile

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A settings file the way an application keeps it: keys in no particular order,
// two-space layout, an encrypted value, characters that a JSON encoder likes to
// escape, non-ASCII text, nested objects and numbers.
const appSettings = `{
  "enabledPlugins": {
    "zeta@market": true,
    "alpha@market": false
  },
  "claw": {
    "token": {
      "$wbEncrypted": 1,
      "envelope": "eyJzdWl0ZSI6MSwia2V5SWQiOiI5MTI3ZGVh=="
    },
    "note": "a < b && c > d — 中文 \u00e9",
    "ratio": 1.50,
    "big": 12345678901234567890
  },
  "sandbox": {
    "enabled": true
  },
  "autoLaunchLegacyCleanerRanAt": 1761700000000
}
`

func settingsFile(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func cmds() map[string]string {
	return map[string]string{"Stop": Command("/opt/session-memory", "", "Stop", "workbuddy")}
}

func TestInstallLeavesTheRestOfTheFileByteForByte(t *testing.T) {
	p := settingsFile(t, appSettings)
	changed, err := Install(p, cmds(), false)
	if err != nil || !changed {
		t.Fatalf("install = %v, %v", changed, err)
	}
	got, _ := os.ReadFile(p)
	// everything that was there is still there, exactly, in the same order
	order, top, err := parseTop(got)
	if err != nil {
		t.Fatalf("result is not valid: %v\n%s", err, got)
	}
	origOrder, origTop, _ := parseTop([]byte(appSettings))
	if strings.Join(order, ",") != strings.Join(append(append([]string{}, origOrder...), "hooks"), ",") {
		t.Fatalf("key order = %v", order)
	}
	for _, k := range origOrder {
		if !bytes.Equal(top[k], origTop[k]) {
			t.Errorf("%s changed:\n%s\nwas\n%s", k, top[k], origTop[k])
		}
	}
	if !strings.Contains(string(got), "a < b && c > d") || strings.Contains(string(got), "\\u003c") {
		t.Errorf("text was re-escaped:\n%s", got)
	}
	if bak, _ := os.ReadFile(p + ".sm-bak"); string(bak) != appSettings {
		t.Errorf("the backup is not the original: %q", bak)
	}
	// and taking ours out gives the original file back
	n, err := Remove(p, false)
	if err != nil || n != 1 {
		t.Fatalf("remove = %d, %v", n, err)
	}
	if back, _ := os.ReadFile(p); string(back) != appSettings {
		t.Fatalf("after remove the file is not the original:\n%s", back)
	}
}

func TestInstallIsIdempotentAndKeepsOneBackup(t *testing.T) {
	p := settingsFile(t, appSettings)
	if _, err := Install(p, cmds(), false); err != nil {
		t.Fatal(err)
	}
	first, _ := os.ReadFile(p)
	changed, err := Install(p, cmds(), false)
	if err != nil || changed {
		t.Fatalf("second install = %v, %v", changed, err)
	}
	if again, _ := os.ReadFile(p); !bytes.Equal(first, again) {
		t.Fatal("a second install changed the file")
	}
	// a later change does not replace the first backup
	if _, err := Install(p, map[string]string{"Stop": Command("/other/session-memory", "", "Stop", "workbuddy")}, false); err != nil {
		t.Fatal(err)
	}
	if bak, _ := os.ReadFile(p + ".sm-bak"); string(bak) != appSettings {
		t.Fatal("the backup was replaced")
	}
}

func TestInstallKeepsTheUsersOwnHooksInPlace(t *testing.T) {
	own := `{
  "a": 1,
  "hooks": {
    "PreToolUse": [
      {
        "matcher": "Bash",
        "hooks": [
          {
            "type": "command",
            "command": "echo mine"
          }
        ]
      }
    ],
    "Stop": [
      {
        "hooks": [
          {
            "type": "command",
            "command": "notify-send done"
          }
        ]
      }
    ]
  },
  "z": [1, 2]
}
`
	p := settingsFile(t, own)
	if _, err := Install(p, cmds(), false); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(p)
	order, top, err := parseTop(got)
	if err != nil || strings.Join(order, ",") != "a,hooks,z" || string(top["z"]) != "[1, 2]" || string(top["a"]) != "1" {
		t.Fatalf("keys moved or changed: %v %v\n%s", order, err, got)
	}
	for _, want := range []string{"echo mine", "notify-send done", "hook --event Stop --agent workbuddy"} {
		if !strings.Contains(string(got), want) {
			t.Errorf("%q is missing:\n%s", want, got)
		}
	}
	n, err := Remove(p, false)
	if err != nil || n != 1 {
		t.Fatalf("remove = %d, %v", n, err)
	}
	after, _ := os.ReadFile(p)
	if !strings.Contains(string(after), "echo mine") || !strings.Contains(string(after), "notify-send done") || strings.Contains(string(after), "session-memory") {
		t.Fatalf("after remove:\n%s", after)
	}
}

func TestFilesThatAreNotSettingsAreNotTouched(t *testing.T) {
	for name, content := range map[string]string{
		"not json":         "{ nope",
		"array":            `[1,2]`,
		"two values":       `{} {}`,
		"hooks a string":   `{"hooks":"x"}`,
		"event not a list": `{"hooks":{"Stop":{}}}`,
		"comment":          "{ // c\n}",
	} {
		p := settingsFile(t, content)
		if _, err := Install(p, cmds(), false); err == nil {
			t.Errorf("%s: install did not refuse", name)
		}
		if _, err := Remove(p, false); err == nil && name != "x" {
			t.Errorf("%s: remove did not refuse", name)
		}
		if b, _ := os.ReadFile(p); string(b) != content {
			t.Errorf("%s: the file was changed: %q", name, b)
		}
		if fileExists(p + ".sm-bak") {
			t.Errorf("%s: a backup was made of a file that was not touched", name)
		}
	}
}

func TestSettingsAreKeptWhenOnlyOurHookWasInThem(t *testing.T) {
	p := settingsFile(t, "{}\n")
	if _, err := Install(p, cmds(), false); err != nil {
		t.Fatal(err)
	}
	if _, err := Remove(p, false); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(p); err != nil || strings.TrimSpace(string(b)) != "{}" {
		t.Fatalf("settings after remove: %q, %v", b, err)
	}
	// a hooks file of our own making goes away with its last hook
	h := filepath.Join(t.TempDir(), "hooks.json")
	if _, err := Install(h, cmds(), true); err != nil {
		t.Fatal(err)
	}
	if _, err := Remove(h, true); err != nil {
		t.Fatal(err)
	}
	if fileExists(h) {
		t.Fatal("an empty hooks file was left behind")
	}
}

func TestNewFileAndPermissions(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "settings.json")
	if changed, err := Install(p, cmds(), false); err != nil || !changed {
		t.Fatalf("install into nothing = %v, %v", changed, err)
	}
	var doc struct {
		Hooks map[string][]struct {
			Hooks []struct {
				Type    string `json:"type"`
				Command string `json:"command"`
				Timeout int    `json:"timeout"`
			} `json:"hooks"`
		} `json:"hooks"`
	}
	b, _ := os.ReadFile(p)
	if err := json.Unmarshal(b, &doc); err != nil || len(doc.Hooks["Stop"]) != 1 || doc.Hooks["Stop"][0].Hooks[0].Type != "command" || doc.Hooks["Stop"][0].Hooks[0].Timeout != Timeout {
		t.Fatalf("new file: %v\n%s", err, b)
	}
	if fileExists(p + ".sm-bak") {
		t.Error("a backup of a file that did not exist")
	}
	// the mode of an existing file is kept
	q := settingsFile(t, appSettings)
	os.Chmod(q, 0o640)
	if _, err := Install(q, cmds(), false); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(q); st.Mode().Perm() != 0o640 {
		t.Errorf("mode = %v", st.Mode().Perm())
	}
}

func TestKeyReadsTheHostsOwnSwitch(t *testing.T) {
	p := settingsFile(t, `{"disableAllHooks": true, "x": 1}`)
	if v, ok := Key(p, "disableAllHooks"); !ok || string(v) != "true" {
		t.Fatalf("Key = %s, %v", v, ok)
	}
	if _, ok := Key(p, "nope"); ok {
		t.Fatal("a key that is not there")
	}
	if _, ok := Key(settingsFile(t, "{ nope"), "x"); ok {
		t.Fatal("a key from a file that is not JSON")
	}
}

func TestCommandIsRecognisedAndCarriesTheAgent(t *testing.T) {
	c := Command("/Users/a b/session-memory", "/tmp/my state", "Stop", "workbuddy")
	if !IsOurs(c) {
		t.Fatalf("not recognised: %s", c)
	}
	f := Fields(c)
	if got := strings.Join(f[len(f)-4:], " "); got != "--event Stop --agent workbuddy" {
		t.Fatalf("tail = %q", got)
	}
	for _, other := range []string{"echo hook", "notify hook --event Stop", ""} {
		if IsOurs(other) {
			t.Errorf("%q counted as ours", other)
		}
	}
}
