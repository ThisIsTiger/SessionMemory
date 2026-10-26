package p0

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"session-memory/internal/errcode"
	"session-memory/internal/platform"
)

// SandboxRoot is $SM_HOME/dev/sandboxes.
func SandboxRoot(smHome string) string { return filepath.Join(smHome, "dev", "sandboxes") }

func writeMarker(dir, purpose string) error {
	b, _ := json.MarshalIndent(Marker{SchemaVersion: 1, CreatedAt: time.Now().UTC().Format("2006-01-02T15:04:05.000Z"), Purpose: purpose, Owner: "session-memory-dev"}, "", "  ")
	return platform.AtomicWrite(filepath.Join(dir, MarkerName), append(b, '\n'), 0600)
}

// NewSandbox creates an isolated agent home plus env scripts that point the
// clients at it. agentName is codex or workbuddy.
func NewSandbox(smHome, name, agentName string) (string, error) {
	if name == "" || filepath.Base(name) != name || name == "." || name == ".." {
		return "", errcode.New(errcode.Usage, "bad sandbox name")
	}
	dir := filepath.Join(SandboxRoot(smHome), name)
	if _, err := os.Stat(dir); err == nil {
		return "", errcode.New(errcode.Conflict, "sandbox already exists")
	}
	if err := platform.EnsurePrivateDir(dir); err != nil {
		return "", err
	}
	if err := writeMarker(dir, agentName); err != nil {
		return "", err
	}
	var vars [][2]string
	switch agentName {
	case "codex":
		vars = [][2]string{{"CODEX_HOME", filepath.Join(dir, "codex")}}
		if err := platform.EnsurePrivateDir(filepath.Join(dir, "codex", "sessions")); err != nil {
			return "", err
		}
	case "workbuddy":
		vars = [][2]string{{"WORKBUDDY_CONFIG_DIR", filepath.Join(dir, "workbuddy")}, {"CODEBUDDY_CONFIG_DIR", filepath.Join(dir, "codebuddy")}}
		for _, v := range vars {
			if err := platform.EnsurePrivateDir(v[1]); err != nil {
				return "", err
			}
		}
	default:
		return "", errcode.New(errcode.Usage, "agent must be codex or workbuddy")
	}
	var sh, ps string
	for _, v := range vars {
		sh += fmt.Sprintf("export %s=%q\n", v[0], v[1])
		ps += fmt.Sprintf("$env:%s = '%s'\n", v[0], v[1])
	}
	if err := platform.AtomicWrite(filepath.Join(dir, "env.sh"), []byte(sh), 0600); err != nil {
		return "", err
	}
	return dir, platform.AtomicWrite(filepath.Join(dir, "env.ps1"), []byte(ps), 0600)
}

// MarkExternal marks an existing, externally isolated directory as a sandbox.
func MarkExternal(dir string) error {
	st, err := os.Stat(dir)
	if err != nil || !st.IsDir() {
		return errcode.New(errcode.Usage, "directory does not exist")
	}
	if home, err := os.UserHomeDir(); err == nil && samePath(dir, home) {
		return errcode.New(errcode.NotIsolated, "the user's home directory cannot be a sandbox")
	}
	return writeMarker(dir, "external")
}

// RemoveSandbox deletes a sandbox created under SandboxRoot.
func RemoveSandbox(smHome, name string) error {
	dir := filepath.Join(SandboxRoot(smHome), name)
	if filepath.Base(name) != name {
		return errcode.New(errcode.Usage, "bad sandbox name")
	}
	if err := RequireSandbox(dir, nil); err != nil {
		return err
	}
	return os.RemoveAll(dir)
}
