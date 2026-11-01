package platform

import (
	"bufio"
	"bytes"
	"strconv"
)

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
	// RSSBytes is the current resident set (macOS proc_pid_rusage, Linux
	// /proc/self/smaps_rollup); 0 elsewhere.
	RSSBytes uint64
	// PSSBytes is the Linux proportional set: resident pages, each page shared
	// with other processes divided among them. PSSFileBytes is its file-backed
	// part — memory-mapped store files and the executable — and is 0 on
	// kernels that predate the split. Both are 0 off Linux.
	PSSBytes     uint64
	PSSFileBytes uint64
}

// ReadProcessMemory samples the process's memory as the operating system
// accounts it. It never fails: a figure the platform cannot provide is 0. It
// is a few system calls, but on Linux the kernel walks the page tables to sum
// the rollup — milliseconds for a process of several GiB — so read it on
// demand, not on a hot path.
func ReadProcessMemory() ProcessMemory {
	out := ProcessMemory{PeakRSSBytes: peakRSSBytes()}
	readCurrentMemory(&out)
	return out
}

// parseSmapsRollup fills the resident and proportional set sizes from the
// contents of /proc/<pid>/smaps_rollup, whose counters are in kB.
func parseSmapsRollup(data []byte, out *ProcessMemory) {
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		key, rest, ok := bytes.Cut(scanner.Bytes(), []byte(":"))
		fields := bytes.Fields(rest)
		if !ok || len(fields) == 0 {
			continue
		}
		kb, err := strconv.ParseUint(string(fields[0]), 10, 64)
		if err != nil {
			continue
		}
		switch string(key) {
		case "Rss":
			out.RSSBytes = kb << 10
		case "Pss":
			out.PSSBytes = kb << 10
		case "Pss_File":
			out.PSSFileBytes = kb << 10
		}
	}
}
