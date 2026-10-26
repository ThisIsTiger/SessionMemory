// Package platform centralizes cross-platform primitives:
// advisory file locks, atomic file replacement, process detachment and liveness.
package platform

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
)

// AtomicWrite writes data to path atomically: temp file in the same directory,
// fsync, rename over the target, then fsync the parent directory so the rename
// itself survives a power loss (a no-op on Windows). Perm applies to the final
// file. Missing parent directories are created 0700; callers that need a
// different mode create the directory first (see EnsurePrivateDir).
func AtomicWrite(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("creating directory for %s: %w", filepath.Base(path), err)
	}
	tmp, err := os.CreateTemp(dir, ".session-memory-tmp-*")
	if err != nil {
		return fmt.Errorf("creating temp file: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath) // no-op after successful rename

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("writing temp file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("syncing temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing temp file: %w", err)
	}
	if err := os.Chmod(tmpPath, perm); err != nil {
		return fmt.Errorf("chmod temp file: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("renaming into place: %w", err)
	}
	if err := syncDir(dir); err != nil {
		return fmt.Errorf("syncing directory: %w", err)
	}
	return nil
}

// EnsurePrivateDir creates dir (and parents) with 0700 permissions.
func EnsurePrivateDir(dir string) error {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("creating directory: %w", err)
	}
	// MkdirAll does not tighten existing dirs; try to enforce 0700 best-effort.
	os.Chmod(dir, 0700)
	return nil
}

// Touch creates or opens the lock file for flock usage.
func Touch(path string) (*os.File, error) {
	if err := EnsurePrivateDir(filepath.Dir(path)); err != nil {
		return nil, fmt.Errorf("creating lock directory: %w", err)
	}
	return os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
}

// SafeName maps an arbitrary identifier to a filesystem-safe file name:
// ids matching [A-Za-z0-9._-]+ are used verbatim, anything else is replaced
// by its SHA-256 hex digest. Shared by checkpoint session ids and project
// profile file names.
func SafeName(id string) string {
	for _, r := range id {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '_' || r == '-') {
			sum := sha256.Sum256([]byte(id))
			return hex.EncodeToString(sum[:])
		}
	}
	return id
}
