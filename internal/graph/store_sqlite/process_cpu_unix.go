//go:build unix

package store_sqlite

import (
	"syscall"
	"time"
)

// processCPUTime is the process's user plus system CPU time so far.
func storeProcessCPUTime() time.Duration {
	var usage syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &usage); err != nil {
		return 0
	}
	return time.Duration(usage.Utime.Nano() + usage.Stime.Nano())
}
