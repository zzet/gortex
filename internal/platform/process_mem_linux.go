//go:build linux

package platform

import "os"

// readCurrentMemory reads the resident and proportional set sizes from the
// kernel's per-process rollup of /proc/self/smaps.
func readCurrentMemory(out *ProcessMemory) {
	if data, err := os.ReadFile("/proc/self/smaps_rollup"); err == nil {
		parseSmapsRollup(data, out)
	}
}
