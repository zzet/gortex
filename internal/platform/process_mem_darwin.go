//go:build darwin && cgo

package platform

/*
#include <libproc.h>
#include <unistd.h>

static int gortex_phys_footprint(unsigned long long *current, unsigned long long *peak) {
	struct rusage_info_v4 info;
	if (proc_pid_rusage(getpid(), RUSAGE_INFO_V4, (rusage_info_t *)&info) != 0) {
		return -1;
	}
	*current = info.ri_phys_footprint;
	*peak = info.ri_lifetime_max_phys_footprint;
	return 0;
}
*/
import "C"

// physFootprintBytes is the process's current and lifetime maximum physical
// footprint (proc_pid_rusage RUSAGE_INFO_V4).
func physFootprintBytes() (current, peak uint64) {
	var cur, max C.ulonglong
	if C.gortex_phys_footprint(&cur, &max) != 0 {
		return 0, 0
	}
	return uint64(cur), uint64(max)
}
