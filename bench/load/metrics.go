package main

import (
	"bufio"
	"fmt"
	"strconv"
	"strings"
)

type processSampler interface {
	sample(int) map[string]Metric
}

func absentProcessMetrics(reason string) map[string]Metric {
	result := make(map[string]Metric)
	for _, name := range []string{"rss_bytes", "phys_footprint_bytes", "pss_bytes", "rss_anon_bytes", "logical_written_bytes", "diskio_written_bytes", "syscall_written_bytes"} {
		result[name] = unavailable("bytes", reason)
	}
	result["cpu_seconds"] = unavailable("s", reason)
	return result
}

func machCPUSeconds(user, system uint64, numer, denom uint32) Metric {
	if numer == 0 || denom == 0 {
		return unavailable("s", "Mach timebase unavailable")
	}
	return measured((float64(user)+float64(system))*float64(numer)/float64(denom)/1e9, "s")
}

func counterDelta(before, after Metric) Metric {
	if before.Value == nil || after.Value == nil {
		return unavailable(after.Unit, "counter endpoint unavailable: "+before.Reason+" "+after.Reason)
	}
	if before.Unit != after.Unit || *after.Value < *before.Value {
		return unavailable(after.Unit, "counter reset or incompatible units")
	}
	return measured(*after.Value-*before.Value, after.Unit)
}

// parseKeyValues handles /proc and cgroup counters. Units are checked by the
// caller; memory.stat's swap counters are page counts, not bytes.
func parseKeyValues(data string) map[string]float64 {
	values := make(map[string]float64)
	scanner := bufio.NewScanner(strings.NewReader(data))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 2 {
			continue
		}
		value, err := strconv.ParseFloat(fields[1], 64)
		if err == nil && value >= 0 {
			values[strings.TrimSuffix(fields[0], ":")] = value
		}
	}
	return values
}

func valueMetric(values map[string]float64, key, unit string, multiplier float64) Metric {
	if value, ok := values[key]; ok {
		return measured(value*multiplier, unit)
	}
	return unavailable(unit, fmt.Sprintf("counter %s unavailable", key))
}
