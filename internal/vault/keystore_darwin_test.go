//go:build darwin

package vault

import (
	"testing"
	"time"
)

func TestKeyringMissingDefaultFailsWithoutWriting(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if keyringAvailable() == nil {
		t.Skip("this macOS does not isolate the default keychain by HOME; do not write to the real keychain")
	}
	start := time.Now()
	err := NewKeyring().Store("sessionmemory-unavailable-keychain-test", make([]byte, 32))
	if err == nil {
		t.Fatal("store accepted an unavailable default keychain")
	}
	if time.Since(start) > 6*time.Second {
		t.Fatal("credential store did not fail promptly")
	}
}
