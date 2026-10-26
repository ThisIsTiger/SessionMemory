//go:build windows

package jsonl

import (
	"fmt"
	"os"
	"syscall"
)

// fileID identifies the file by volume serial number and file index; ""
// when unavailable.
func fileID(f *os.File) string {
	var info syscall.ByHandleFileInformation
	if err := syscall.GetFileInformationByHandle(syscall.Handle(f.Fd()), &info); err != nil {
		return ""
	}
	return fmt.Sprintf("%d:%d:%d", info.VolumeSerialNumber, info.FileIndexHigh, info.FileIndexLow)
}
