//go:build unix

package goanalysis

import (
	"syscall"
	"time"
)

// processUsage is a snapshot of the process's fault and CPU counters, taken
// around the load (type checking) and the apply (the graph's reads and
// writes) so each stage's log line carries its own faults and CPU.
type processUsage struct {
	majorFaults, minorFaults int64
	cpu                      time.Duration
}

func readProcessUsage() processUsage {
	var usage syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &usage); err != nil {
		return processUsage{}
	}
	return processUsage{
		majorFaults: int64(usage.Majflt),
		minorFaults: int64(usage.Minflt),
		cpu:         time.Duration(usage.Utime.Nano() + usage.Stime.Nano()),
	}
}

func (u processUsage) since(start processUsage) processUsage {
	return processUsage{
		majorFaults: u.majorFaults - start.majorFaults,
		minorFaults: u.minorFaults - start.minorFaults,
		cpu:         u.cpu - start.cpu,
	}
}
