// Package gitrepo builds isolated local repositories for code-state tests.
package gitrepo

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func Run(t *testing.T, dir string, args ...string) string {
	t.Helper()
	base := []string{"-c", "user.name=SessionMemory Test", "-c", "user.email=fixture@example.invalid", "-c", "commit.gpgsign=false", "-c", "core.hooksPath=" + filepath.Join(dir, "no-hooks"), "-C", dir}
	cmd := exec.Command("git", append(base, args...)...)
	for _, env := range os.Environ() {
		key, _, _ := strings.Cut(env, "=")
		if strings.HasPrefix(key, "GIT_") {
			continue
		}
		cmd.Env = append(cmd.Env, env)
	}
	cmd.Env = append(cmd.Env, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git fixture: %v: %s", err, out)
	}
	return strings.TrimSpace(string(out))
}

func New(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	Run(t, dir, "init", "-q")
	if err := os.WriteFile(filepath.Join(dir, "tracked.txt"), []byte("fixture\n"), 0600); err != nil {
		t.Fatal(err)
	}
	Run(t, dir, "add", "tracked.txt")
	Run(t, dir, "commit", "-q", "-m", "initial")
	Run(t, dir, "remote", "add", "origin", "https://example.invalid/team/project.git")
	return dir
}
