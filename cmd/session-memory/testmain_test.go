package main

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"session-memory/internal/vault"
)

// Tests must never start the Codex that happens to be installed on the machine.
func TestMain(m *testing.M) {
	os.Setenv("SM_CODEX_BIN", "/nonexistent/codex")
	if os.Getenv("SM_LIVE_WEIYUN") != "1" {
		// the developer's own Weiyun login must neither change what a test means
		// nor reach the real service; a test that wants a token sets its own
		os.Unsetenv(envWeiyunToken)
		os.Unsetenv(envWeiyunToken2)
		os.Unsetenv("WEIYUN_MCP_URL")
	}
	folderSettle = 20 * time.Millisecond // files in these tests are never half-written
	if os.Getenv("SM_TEST_AS_CLI") == "1" {
		// a process the hook started: this test binary standing in for the program,
		// with its own empty credential store instead of the real one
		newKeyStore = func() vault.KeyStore { return vault.MemStore{} }
		hookQuiet, hookPoll = 0, 5*time.Millisecond
		os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
	}
	// nothing starts a process of the test binary unless a test asks for it
	hookSpawn = func(*App) error { return nil }
	selfCommand = func(context.Context, ...string) (string, error) {
		return "", errors.New("a test ran one of the program's own commands without replacing selfCommand")
	}
	os.Exit(m.Run())
}
