package platform

// ProcessMemory is the operating system's view of this process's memory,
// beyond what the Go runtime can see: resident pages, and on macOS the
// physical footprint (resident plus compressed and swapped anonymous pages,
// the figure Activity Monitor and memory pressure act on — a Go heap that
// released its pages still counts there until they are touched again).
type ProcessMemory struct {
	// PeakRSSBytes is the process's lifetime maximum resident set size
	// (getrusage ru_maxrss); 0 where unavailable.
	PeakRSSBytes uint64
	// FootprintBytes and PeakFootprintBytes are the current and lifetime
	// maximum physical footprint (macOS proc_pid_rusage); 0 elsewhere.
	FootprintBytes     uint64
	PeakFootprintBytes uint64
}

// ReadProcessMemory samples the process's memory as the operating system
// accounts it. It is cheap (two system calls) and never fails: a figure the
// platform cannot provide is 0.
func ReadProcessMemory() ProcessMemory {
	out := ProcessMemory{PeakRSSBytes: peakRSSBytes()}
	out.FootprintBytes, out.PeakFootprintBytes = physFootprintBytes()
	return out
}
