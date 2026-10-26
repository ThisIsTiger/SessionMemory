package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// withTempHome redirects home-dependent paths to a temp dir and clears the
// environment overrides so the developer's own settings never leak into tests.
func withTempHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // os.UserHomeDir reads USERPROFILE on Windows
	t.Setenv(EnvHome, "")
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	return home
}

func writeGlobal(t *testing.T, home, content string) string {
	t.Helper()
	dir := filepath.Join(home, ".config", "session-memory")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "config.json")
	if err := os.WriteFile(p, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestDefaultConfigValues(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.Home != "~/.session-memory" {
		t.Errorf("home default = %q", cfg.Home)
	}
	for name, a := range map[string]AgentConfig{"codex": cfg.Agents.Codex, "claude": cfg.Agents.Claude, "workbuddy": cfg.Agents.WorkBuddy} {
		if !a.IsEnabled() {
			t.Errorf("%s should default to enabled", name)
		}
		if a.IsAutoInstall() {
			t.Errorf("%s auto_install must default to off", name)
		}
		if a.MaxRecordBytes != 16<<20 {
			t.Errorf("%s max_record_bytes = %d", name, a.MaxRecordBytes)
		}
	}
	if cfg.Agents.Codex.Home != "~/.codex" || cfg.Agents.Claude.Home != "~/.claude" || cfg.Agents.WorkBuddy.Home != "~/.workbuddy" {
		t.Errorf("agent homes = %q %q %q", cfg.Agents.Codex.Home, cfg.Agents.Claude.Home, cfg.Agents.WorkBuddy.Home)
	}
	if cfg.Transport.Folder != "" {
		t.Errorf("transport.folder must default to unset, got %q", cfg.Transport.Folder)
	}
	if cfg.Log.RetentionDays != 30 {
		t.Errorf("log.retention_days default = %d", cfg.Log.RetentionDays)
	}
	if err := Validate(cfg); err != nil {
		t.Errorf("default config failed validation: %v", err)
	}
}

func TestLoadOverlaysGlobalFile(t *testing.T) {
	home := withTempHome(t)
	writeGlobal(t, home, `{"agents":{"codex":{"home":"/data/codex","auto_install":true}},"transport":{"folder":"/sync/vault"}}`)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Agents.Codex.Home != "/data/codex" || !cfg.Agents.Codex.IsAutoInstall() {
		t.Errorf("codex block = %+v", cfg.Agents.Codex)
	}
	if cfg.Transport.Folder != "/sync/vault" {
		t.Errorf("transport.folder = %q", cfg.Transport.Folder)
	}
	// unspecified fields keep their defaults, including the rest of the codex block
	if cfg.Agents.Codex.MaxRecordBytes != 16<<20 || !cfg.Agents.Codex.IsEnabled() {
		t.Errorf("codex defaults lost: %+v", cfg.Agents.Codex)
	}
	if cfg.Agents.Claude.Home != "~/.claude" {
		t.Errorf("claude block leaked changes: %+v", cfg.Agents.Claude)
	}
}

func TestSMHomeEnvOverridesConfiguredHome(t *testing.T) {
	home := withTempHome(t)
	writeGlobal(t, home, `{"home":"/from/file"}`)
	override := filepath.Join(t.TempDir(), "device-b")
	t.Setenv(EnvHome, override)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.StateDir() != override {
		t.Errorf("StateDir = %q, want %q", cfg.StateDir(), override)
	}
}

func TestEmptySMHomeIsIgnored(t *testing.T) {
	withTempHome(t)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(homeOf(t), ".session-memory"); cfg.StateDir() != want {
		t.Errorf("StateDir = %q, want %q", cfg.StateDir(), want)
	}
}

func TestProjectConfigIsNotLoaded(t *testing.T) {
	withTempHome(t)
	proj := t.TempDir()
	if err := os.WriteFile(filepath.Join(proj, ".session-memory.json"),
		[]byte(`{"agents":{"codex":{"home":"/attacker/controlled"}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	wd, _ := os.Getwd()
	defer os.Chdir(wd)
	if err := os.Chdir(proj); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Agents.Codex.Home != "~/.codex" {
		t.Errorf("a repository file changed agents.codex.home to %q", cfg.Agents.Codex.Home)
	}
}

func TestLoadRejectsInvalid(t *testing.T) {
	home := withTempHome(t)
	cases := []struct {
		name string
		json string
	}{
		{"record bytes too small", `{"agents":{"codex":{"max_record_bytes":10}}}`},
		{"record bytes too large", `{"agents":{"claude":{"max_record_bytes":999999999999}}}`},
		{"relative state dir", `{"home":"relative/path"}`},
		{"relative agent home", `{"agents":{"workbuddy":{"home":"rel"}}}`},
		{"relative transport folder", `{"transport":{"folder":"vault"}}`},
		{"negative retention", `{"log":{"retention_days":-1}}`},
		{"wrong type", `{"log":{"retention_days":"thirty"}}`},
		{"malformed json", `{"home":`},
	}
	for _, c := range cases {
		writeGlobal(t, home, c.json)
		if _, err := Load(); err == nil {
			t.Errorf("%s: expected an error, got nil", c.name)
		}
	}
}

func TestLoadRejectsLegacyAndUnknownKeys(t *testing.T) {
	home := withTempHome(t)
	for _, c := range []struct{ name, json string }{
		{"daily-report summary block", `{"summary":{"timeout":"5m"}}`},
		{"blog block", `{"blog":{"enabled":true}}`},
		{"schedule block", `{"schedule":{"blog_time":"00:20"}}`},
		{"old sources block", `{"sources":{"codex":{"lookback":"48h"}}}`},
		{"old output block", `{"output":{"dir":"/x"}}`},
		{"provider api key", `{"summary":{"provider":{"api_key":"sk-test"}}}`},
		{"typo in known block", `{"agents":{"codex":{"hom":"/x"}}}`},
	} {
		writeGlobal(t, home, c.json)
		_, err := Load()
		if err == nil {
			t.Errorf("%s: expected unknown-key rejection, got nil", c.name)
			continue
		}
		if !strings.Contains(err.Error(), "unknown field") {
			t.Errorf("%s: error should name the unknown field, got %v", c.name, err)
		}
	}
}

func homeOf(t *testing.T) string {
	t.Helper()
	h, _ := os.UserHomeDir()
	return h
}

func TestDerivedPaths(t *testing.T) {
	withTempHome(t)
	h := homeOf(t)
	cfg := DefaultConfig()
	if cfg.CodexHome() != filepath.Join(h, ".codex") {
		t.Errorf("CodexHome = %q", cfg.CodexHome())
	}
	if cfg.WorkBuddyHome() != filepath.Join(h, ".workbuddy") {
		t.Errorf("WorkBuddyHome = %q", cfg.WorkBuddyHome())
	}
	if cfg.ClaudeHome() != filepath.Join(h, ".claude") {
		t.Errorf("ClaudeHome = %q", cfg.ClaudeHome())
	}

	t.Setenv("CLAUDE_CONFIG_DIR", "/env/claude")
	if cfg.ClaudeHome() != "/env/claude" {
		t.Errorf("CLAUDE_CONFIG_DIR ignored: %q", cfg.ClaudeHome())
	}
	cfg.Agents.Claude.Home = "~/custom-claude"
	if cfg.ClaudeHome() != filepath.Join(h, "custom-claude") {
		t.Errorf("explicit setting must win over CLAUDE_CONFIG_DIR: %q", cfg.ClaudeHome())
	}

	empty := Config{}
	if empty.StateDir() != filepath.Join(h, ".session-memory") || empty.CodexHome() != filepath.Join(h, ".codex") {
		t.Errorf("zero Config must fall back to defaults: %q %q", empty.StateDir(), empty.CodexHome())
	}
}

func readGlobalMap(t *testing.T, home string) map[string]interface{} {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(home, ".config", "session-memory", "config.json"))
	if err != nil {
		t.Fatalf("config file unreadable: %v", err)
	}
	var m map[string]interface{}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("config file is not JSON: %v", err)
	}
	return m
}

func TestValidKeysAndSet(t *testing.T) {
	home := withTempHome(t)
	for _, k := range ValidKeys() {
		if !IsValidKey(k) {
			t.Errorf("ValidKeys contains unmatchable key %q", k)
		}
	}
	if IsValidKey("no.such.key") || IsValidKey("summary.timeout") {
		t.Error("IsValidKey accepted an unknown or legacy key")
	}
	if err := Set("no.such.key", "x"); err == nil {
		t.Error("Set accepted unknown key")
	}

	if err := Set("agents.codex.home", "/data/codex"); err != nil {
		t.Fatalf("Set agents.codex.home: %v", err)
	}
	if err := Set("agents.codex.auto_install", "true"); err != nil {
		t.Fatalf("Set auto_install: %v", err)
	}
	if err := Set("log.retention_days", "7"); err != nil {
		t.Fatalf("Set retention_days: %v", err)
	}
	m := readGlobalMap(t, home)
	codex := m["agents"].(map[string]interface{})["codex"].(map[string]interface{})
	if codex["home"] != "/data/codex" || codex["auto_install"] != true {
		t.Errorf("codex block = %v", codex)
	}
	if m["log"].(map[string]interface{})["retention_days"] != float64(7) {
		t.Errorf("retention_days = %v", m["log"])
	}

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load after Set: %v", err)
	}
	if cfg.Agents.Codex.Home != "/data/codex" || !cfg.Agents.Codex.IsAutoInstall() || cfg.Log.RetentionDays != 7 {
		t.Errorf("Set values not visible through Load: %+v", cfg)
	}
}

func TestSetRejectsBadValuesWithoutWriting(t *testing.T) {
	home := withTempHome(t)
	path := writeGlobal(t, home, `{"log":{"retention_days":5}}`)
	before, _ := os.ReadFile(path)

	for _, c := range []struct{ key, val string }{
		{"log.retention_days", "zero"},
		{"log.retention_days", "-3"},
		{"agents.codex.max_record_bytes", "10"},
		{"agents.codex.auto_install", "maybe"},
	} {
		if err := Set(c.key, c.val); err == nil {
			t.Errorf("Set(%s, %s) accepted", c.key, c.val)
		}
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Errorf("a rejected Set modified the file:\n%s", after)
	}
}

func TestSetMakesRelativePathsAbsolute(t *testing.T) {
	home := withTempHome(t)
	if err := Set("transport.folder", "vault-dir"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got := readGlobalMap(t, home)["transport"].(map[string]interface{})["folder"].(string)
	if !filepath.IsAbs(got) || filepath.Base(got) != "vault-dir" {
		t.Errorf("folder = %q, want an absolute path ending in vault-dir", got)
	}
	// "~" paths are stored unexpanded so the file stays portable between machines
	if err := Set("home", "~/state"); err != nil {
		t.Fatal(err)
	}
	if got := readGlobalMap(t, home)["home"]; got != "~/state" {
		t.Errorf("home = %v, want ~/state", got)
	}
}

func TestSetPreservesUnrelatedFields(t *testing.T) {
	home := withTempHome(t)
	writeGlobal(t, home, `{"transport":{"folder":"/sync/vault"},"agents":{"claude":{"enabled":false}}}`)
	if err := Set("log.retention_days", "9"); err != nil {
		t.Fatal(err)
	}
	m := readGlobalMap(t, home)
	if m["transport"].(map[string]interface{})["folder"] != "/sync/vault" {
		t.Errorf("transport lost: %v", m)
	}
	if m["agents"].(map[string]interface{})["claude"].(map[string]interface{})["enabled"] != false {
		t.Errorf("claude.enabled lost: %v", m)
	}
}

func TestLoadSourcesTracksOrigin(t *testing.T) {
	home := withTempHome(t)
	writeGlobal(t, home, `{"agents":{"codex":{"home":"/data/codex"}}}`)

	find := func(srcs []ConfigSource, key string) ConfigSource {
		for _, s := range srcs {
			if s.Key == key {
				return s
			}
		}
		t.Fatalf("key %s missing from sources", key)
		return ConfigSource{}
	}

	srcs, err := LoadSources()
	if err != nil {
		t.Fatalf("LoadSources: %v", err)
	}
	if len(srcs) != len(ValidKeys()) {
		t.Errorf("got %d sources for %d keys", len(srcs), len(ValidKeys()))
	}
	if s := find(srcs, "agents.codex.home"); s.Source != "global" || s.Value != "/data/codex" {
		t.Errorf("agents.codex.home = %+v", s)
	}
	if s := find(srcs, "agents.claude.home"); s.Source != "default" {
		t.Errorf("agents.claude.home = %+v", s)
	}

	t.Setenv(EnvHome, "/dev/device-a")
	srcs, _ = LoadSources()
	if s := find(srcs, "home"); s.Source != "env" || s.Value != "/dev/device-a" {
		t.Errorf("home = %+v", s)
	}
}
