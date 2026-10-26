//go:build !windows

package platform

import (
	"errors"
	"os"
	"syscall"
)

// syncDir flushes directory metadata so a preceding rename is durable.
// Filesystems that cannot fsync a directory report EINVAL/ENOTSUP; that is not
// a failure of the write, so it is ignored.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	if err := d.Sync(); err != nil && !syncUnsupported(err) {
		return err
	}
	return nil
}

func syncUnsupported(err error) bool {
	return errors.Is(err, syscall.EINVAL) || errors.Is(err, syscall.ENOTSUP)
}
