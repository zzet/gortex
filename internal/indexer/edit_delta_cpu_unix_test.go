//go:build unix

package indexer

import (
	"syscall"
	"time"
)

// editDeltaProcessCPU is the process's user plus system CPU time so far. On a
// loaded host a build's wall time includes waiting for a core; this does not.
func editDeltaProcessCPU() time.Duration {
	var usage syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &usage); err != nil {
		return 0
	}
	return time.Duration(usage.Utime.Nano() + usage.Stime.Nano())
}
