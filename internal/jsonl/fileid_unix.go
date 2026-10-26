//go:build !windows

package jsonl

import (
	"fmt"
	"os"
	"syscall"
)

// fileID identifies the file by device and inode; "" when unavailable.
func fileID(f *os.File) string {
	st, err := f.Stat()
	if err != nil {
		return ""
	}
	sys, ok := st.Sys().(*syscall.Stat_t)
	if !ok {
		return ""
	}
	return fmt.Sprintf("%d:%d", uint64(sys.Dev), uint64(sys.Ino))
}
