//go:build unix

package resolver

import "syscall"

// processMajorFaults is the process's major page-fault count so far: the
// store pages an incremental phase had to bring in from disk (the whole
// process, so concurrent work is included).
func processMajorFaults() int64 {
	var usage syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &usage); err != nil {
		return 0
	}
	return int64(usage.Majflt)
}
