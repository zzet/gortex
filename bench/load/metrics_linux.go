package main

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

type linuxProcessSampler struct{ ticksPerSecond float64 }

func newProcessSampler() processSampler {
	data, err := exec.Command("getconf", "CLK_TCK").Output()
	if err != nil {
		return linuxProcessSampler{}
	}
	ticks, _ := strconv.ParseFloat(strings.TrimSpace(string(data)), 64)
	return linuxProcessSampler{ticksPerSecond: ticks}
}

func (s linuxProcessSampler) sample(pid int) map[string]Metric {
	metrics := absentProcessMetrics("not provided by Linux procfs")
	root := fmt.Sprintf("/proc/%d/", pid)
	if data, err := os.ReadFile(root + "stat"); err == nil {
		metrics["cpu_seconds"] = linuxCPU(string(data), s.ticksPerSecond)
	}
	if data, err := os.ReadFile(root + "status"); err == nil {
		values := parseKeyValues(string(data))
		metrics["rss_bytes"] = valueMetric(values, "VmRSS", "bytes", 1024)
		metrics["rss_anon_bytes"] = valueMetric(values, "RssAnon", "bytes", 1024)
	}
	if data, err := os.ReadFile(root + "smaps_rollup"); err == nil {
		metrics["pss_bytes"] = valueMetric(parseKeyValues(string(data)), "Pss", "bytes", 1024)
	}
	if data, err := os.ReadFile(root + "io"); err == nil {
		values := parseKeyValues(string(data))
		metrics["diskio_written_bytes"] = valueMetric(values, "write_bytes", "bytes", 1)
		metrics["syscall_written_bytes"] = valueMetric(values, "wchar", "bytes", 1)
	}
	return metrics
}

func linuxCPU(stat string, ticksPerSecond float64) Metric {
	if ticksPerSecond <= 0 {
		return unavailable("s", "getconf CLK_TCK unavailable")
	}
	// comm may itself contain spaces and parentheses. Fields following the
	// last ')' begin at field 3, so utime/stime are offsets 11 and 12.
	end := strings.LastIndexByte(stat, ')')
	if end < 0 {
		return unavailable("s", "malformed proc stat")
	}
	fields := strings.Fields(stat[end+1:])
	if len(fields) < 13 {
		return unavailable("s", "incomplete proc stat")
	}
	user, uerr := strconv.ParseUint(fields[11], 10, 64)
	system, serr := strconv.ParseUint(fields[12], 10, 64)
	if uerr != nil || serr != nil {
		return unavailable("s", "invalid proc CPU counters")
	}
	return measured((float64(user)+float64(system))/ticksPerSecond, "s")
}
