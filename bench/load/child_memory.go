package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// descendantPSS sums the proportional set of every process below pid (the
// language servers and other helpers the daemon starts, and theirs) from the
// procfs mounted at proc. Each thread lists the children it forked in
// task/<tid>/children. A descendant that exits during the walk counts as
// nothing; a kernel that keeps no children lists leaves the figure
// unavailable.
func descendantPSS(proc string, pid int) Metric {
	total := 0.0
	seen := map[int]bool{pid: true}
	queue := []int{pid}
	for len(queue) > 0 {
		parent := queue[0]
		queue = queue[1:]
		lists, _ := filepath.Glob(filepath.Join(proc, strconv.Itoa(parent), "task", "*", "children"))
		if parent == pid && len(lists) == 0 {
			return unavailable("bytes", "procfs lists no task children")
		}
		for _, list := range lists {
			data, _ := os.ReadFile(list)
			for _, field := range strings.Fields(string(data)) {
				child, err := strconv.Atoi(field)
				if err != nil || seen[child] {
					continue
				}
				seen[child] = true
				queue = append(queue, child)
				if rollup, err := os.ReadFile(filepath.Join(proc, field, "smaps_rollup")); err == nil {
					total += parseKeyValues(string(rollup))["Pss"] * 1024
				}
			}
		}
	}
	return measured(total, "bytes")
}

// cgroupAnonFile splits the cgroup's charge, which includes the daemon's
// children, into anonymous memory and file-backed pages; memory.stat counts
// both in bytes.
func cgroupAnonFile(root string) map[string]Metric {
	stat := make(map[string]float64)
	if data, err := os.ReadFile(filepath.Join(root, "memory.stat")); err == nil {
		stat = parseKeyValues(string(data))
	}
	return map[string]Metric{
		"cgroup_anon_bytes": valueMetric(stat, "anon", "bytes", 1),
		"cgroup_file_bytes": valueMetric(stat, "file", "bytes", 1),
	}
}
