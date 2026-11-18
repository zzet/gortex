//go:build unix

package platform

import (
	"runtime"

	"golang.org/x/sys/unix"
)

// peakRSSBytes is getrusage's ru_maxrss in bytes: macOS reports bytes, the
// other Unix systems kilobytes.
func peakRSSBytes() uint64 {
	var usage unix.Rusage
	if err := unix.Getrusage(unix.RUSAGE_SELF, &usage); err != nil || usage.Maxrss <= 0 {
		return 0
	}
	if runtime.GOOS == "darwin" || runtime.GOOS == "ios" {
		return uint64(usage.Maxrss)
	}
	return uint64(usage.Maxrss) * 1024
}
