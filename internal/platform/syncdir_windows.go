//go:build windows

package platform

// syncDir is a no-op: Windows cannot fsync a directory handle, and NTFS
// journals the rename metadata itself.
func syncDir(string) error { return nil }
