//go:build !windows

package goanalysis

import (
	"os"
	"syscall"
)

func statStamp(path string) (fileStamp, bool) {
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return fileStamp{}, false
	}
	st := fileStamp{size: info.Size(), mtime: info.ModTime().UnixNano()}
	if sys, ok := info.Sys().(*syscall.Stat_t); ok {
		st.identity[0] = uint64(sys.Ino)
	}
	return st, true
}
