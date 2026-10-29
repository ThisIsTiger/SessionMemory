package main

import (
	"os"
	"testing"
	"time"
)

// Tests must never start the Codex that happens to be installed on the machine.
func TestMain(m *testing.M) {
	os.Setenv("SM_CODEX_BIN", "/nonexistent/codex")
	folderSettle = 20 * time.Millisecond // files in these tests are never half-written
	os.Exit(m.Run())
}
