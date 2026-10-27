package main

import (
	"os"
	"testing"
)

// Tests must never start the Codex that happens to be installed on the machine.
func TestMain(m *testing.M) {
	os.Setenv("SM_CODEX_BIN", "/nonexistent/codex")
	os.Exit(m.Run())
}
