//go:build darwin && cgo

package platform

/*
#include <libproc.h>
#include <unistd.h>

static int gortex_process_memory(unsigned long long *footprint, unsigned long long *peak, unsigned long long *resident) {
	struct rusage_info_v4 info;
	if (proc_pid_rusage(getpid(), RUSAGE_INFO_V4, (rusage_info_t *)&info) != 0) {
		return -1;
	}
	*footprint = info.ri_phys_footprint;
	*peak = info.ri_lifetime_max_phys_footprint;
	*resident = info.ri_resident_size;
	return 0;
}
*/
import "C"

// readCurrentMemory reads the process's current and lifetime maximum physical
// footprint and its current resident size (proc_pid_rusage RUSAGE_INFO_V4).
func readCurrentMemory(out *ProcessMemory) {
	var footprint, peak, resident C.ulonglong
	if C.gortex_process_memory(&footprint, &peak, &resident) != 0 {
		return
	}
	out.FootprintBytes, out.PeakFootprintBytes, out.RSSBytes = uint64(footprint), uint64(peak), uint64(resident)
}
