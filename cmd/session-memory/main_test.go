package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"session-memory/internal/testutil/codexgen"
	"session-memory/internal/vault"
)

func exec(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := run(args, nil, &out, &errb)
	return code, out.String(), errb.String()
}

func isolate(t *testing.T) string {
	// Host credentials and agent homes must not change the meaning of a CLI
	// test or let a test contact the user's cloud account.
	oldKeys := newKeyStore
	keys := vault.MemStore{}
	newKeyStore = func() vault.KeyStore { return keys }
	t.Cleanup(func() { newKeyStore = oldKeys })
	h := t.TempDir()
	t.Setenv("HOME", h)
	t.Setenv("USERPROFILE", h)
	t.Setenv("SM_HOME", "")
	t.Setenv("CODEX_HOME", "")
	t.Setenv("WORKBUDDY_CONFIG_DIR", "")
	t.Setenv("CODEBUDDY_CONFIG_DIR", "")
	t.Setenv(envWeiyunToken, "")
	t.Setenv(envWeiyunToken2, "")
	t.Setenv("WEIYUN_MCP_URL", "")
	return h
}

func TestVersion(t *testing.T) {
	isolate(t)
	code, out, _ := exec(t, "version")
	if code != 0 || !strings.HasPrefix(out, "session-memory dev (") {
		t.Fatalf("%d %q", code, out)
	}
	code, out, _ = exec(t, "version", "--json")
	var v versionInfo
	if code != 0 || json.Unmarshal([]byte(out), &v) != nil || v.Version != "dev" || v.OS == "" {
		t.Fatalf("%d %q", code, out)
	}
}

func TestUsageErrorsAndJSONErrorEnvelope(t *testing.T) {
	isolate(t)
	if code, _, e := exec(t, "nosuch"); code != 2 || !strings.Contains(e, "error: usage") {
		t.Errorf("unknown command: %d %q", code, e)
	}
	code, out, _ := exec(t, "--json", "config", "get", "no.such")
	var env struct{ Error struct{ Code string } }
	if code != 2 || json.Unmarshal([]byte(out), &env) != nil || env.Error.Code != "usage" {
		t.Errorf("json error: %d %q", code, out)
	}
	if code, _, _ := exec(t, "agent", "probe", "nope"); code != 2 {
		t.Errorf("unknown agent exit = %d", code)
	}
	if code, _, _ := exec(t, "agent", "probe", "claude"); code != 5 {
		t.Errorf("unsupported agent exit = %d", code)
	}
}

func TestConfigCommands(t *testing.T) {
	isolate(t)
	if code, _, _ := exec(t, "config", "set", "log.retention_days", "7"); code != 0 {
		t.Fatal("set failed")
	}
	code, out, _ := exec(t, "config", "get", "log.retention_days")
	if code != 0 || strings.TrimSpace(out) != "7" {
		t.Errorf("get: %d %q", code, out)
	}
	if code, _, _ := exec(t, "config", "set", "log.retention_days", "-1"); code != 2 {
		t.Errorf("bad value exit = %d", code)
	}
	code, out, _ = exec(t, "config", "show", "--json")
	var rows []struct{ Key, Value, Source string }
	if code != 0 || json.Unmarshal([]byte(out), &rows) != nil || len(rows) == 0 {
		t.Fatalf("show: %d", code)
	}
	if code, out, _ := exec(t, "config", "path"); code != 0 || !strings.HasSuffix(strings.TrimSpace(out), "config.json") {
		t.Errorf("path: %q", out)
	}
}

func TestAgentProbeCodexIsSafe(t *testing.T) {
	h := isolate(t)
	home := filepath.Join(h, "cx")
	secret := "SECRET-SENTINEL-123"
	tid := codexgen.ThreadIDFor(1)
	p := filepath.Join(home, "sessions", "2026", "09", "01", "rollout-2026-09-01T01-46-53-"+tid+".jsonl")
	os.MkdirAll(filepath.Dir(p), 0700)
	os.WriteFile(p, codexgen.Rollout(codexgen.Options{Seed: 1, Cwd: "/w/" + secret}), 0600)
	if code, _, _ := exec(t, "config", "set", "agents.codex.home", home); code != 0 {
		t.Fatal("config set")
	}
	code, out, _ := exec(t, "agent", "probe", "codex", "--json")
	if code != 0 || strings.Contains(out, secret) || strings.Contains(out, tid) {
		t.Fatalf("probe: %d leaked=%v", code, strings.Contains(out, secret))
	}
	var res struct{ Compatibility string }
	json.Unmarshal([]byte(out), &res)
	if res.Compatibility != "ok" {
		t.Errorf("compat = %s", res.Compatibility)
	}
	if code, out, _ := exec(t, "agent", "probe", "codex"); code != 0 || !strings.Contains(out, "compatibility: ok") {
		t.Errorf("text: %d %q", code, out)
	}
}
