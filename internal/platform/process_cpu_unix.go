//go:build unix

package platform

import (
	"syscall"
	"time"
)

// ProcessCPUTime returns the current process's cumulative user plus system CPU.
// It samples only the calling process, without reading files or child usage.
func ProcessCPUTime() (time.Duration, error) {
	var usage syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &usage); err != nil {
		return 0, err
	}
	user, system := usage.Utime.Nano(), usage.Stime.Nano()
	if user < 0 || system < 0 || user > (1<<63-1)-system {
		return 0, errInvalidProcessCPUTime
	}
	return time.Duration(user + system), nil
}
