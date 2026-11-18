//go:build darwin && cgo

package main

/*
#include <libproc.h>
#include <sys/resource.h>
#include <mach/mach_time.h>
static int load_process_usage(int pid, struct rusage_info_v4 *info) {
    return proc_pid_rusage(pid, RUSAGE_INFO_V4, (rusage_info_t *)info);
}
*/
import "C"

import "fmt"

type darwinProcessSampler struct{ numer, denom uint32 }

func newProcessSampler() processSampler {
	var timebase C.mach_timebase_info_data_t
	if C.mach_timebase_info(&timebase) != 0 {
		return darwinProcessSampler{}
	}
	return darwinProcessSampler{numer: uint32(timebase.numer), denom: uint32(timebase.denom)}
}

func (s darwinProcessSampler) sample(pid int) map[string]Metric {
	var info C.struct_rusage_info_v4
	if status := C.load_process_usage(C.int(pid), &info); status != 0 {
		return absentProcessMetrics(fmt.Sprintf("proc_pid_rusage failed (%d)", status))
	}
	metrics := absentProcessMetrics("not provided by macOS proc_pid_rusage")
	// XNU fill_task_rusage copies task_power_info totals; those originate in
	// recount's rm_time_mach, not nanoseconds. Apple silicon's non-1:1
	// timebase otherwise silently undercounts CPU by roughly 41.7 times.
	// https://github.com/apple-oss-distributions/xnu/blob/main/osfmk/kern/bsd_kern.c
	// https://github.com/apple-oss-distributions/xnu/blob/main/osfmk/kern/task.c
	metrics["cpu_seconds"] = machCPUSeconds(uint64(info.ri_user_time), uint64(info.ri_system_time), s.numer, s.denom)
	metrics["rss_bytes"] = measured(float64(info.ri_resident_size), "bytes")
	metrics["phys_footprint_bytes"] = measured(float64(info.ri_phys_footprint), "bytes")
	// XNU gather_rusage_info uses get_task_logical_writes(task, false): the
	// internal-volume ledger. External-volume writes have a separate ledger
	// and are not included here; disk I/O is a distinct physical meter.
	// https://github.com/apple-oss-distributions/xnu/blob/main/bsd/kern/kern_resource.c
	// https://github.com/apple-oss-distributions/xnu/blob/main/osfmk/kern/bsd_kern.c
	metrics["logical_written_bytes"] = measured(float64(info.ri_logical_writes), "bytes")
	metrics["diskio_written_bytes"] = measured(float64(info.ri_diskio_byteswritten), "bytes")
	return metrics
}
