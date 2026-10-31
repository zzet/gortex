package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

const gib = 1024 * 1024 * 1024

func inspectHost(profile, scratch string) (Host, string, error) {
	machine, err := os.Hostname()
	if err != nil {
		return Host{}, "", err
	}
	h := Host{Machine: machine, OS: runtime.GOOS, Arch: runtime.GOARCH, Profile: profile, Kernel: "unavailable", Limits: map[string]Metric{"logical_cpus": measured(float64(runtime.NumCPU()), "cores"), "disk_capacity_bytes": volumeCapacity(scratch)}, Availability: make(map[string]string)}
	h.Limits["page_size_bytes"] = measured(float64(os.Getpagesize()), "bytes")
	if data, err := exec.Command("uname", "-r").Output(); err == nil {
		h.Kernel = strings.TrimSpace(string(data))
	}
	group := ""
	h.Limits["cpu_quota"] = unavailable("cores", "no Linux cgroup v2 quota")
	h.Limits["memory_max"] = unavailable("bytes", "no Linux cgroup v2 memory limit")
	h.Limits["host_memory"] = unavailable("bytes", "host capacity collector unavailable")
	if runtime.GOOS == "linux" {
		if data, err := os.ReadFile("/proc/meminfo"); err == nil {
			h.Limits["host_memory"] = valueMetric(parseKeyValues(string(data)), "MemTotal", "bytes", 1024)
		}
		if data, err := os.ReadFile("/proc/self/cgroup"); err == nil {
			for line := range strings.SplitSeq(string(data), "\n") {
				if path, ok := strings.CutPrefix(line, "0::"); ok {
					group = filepath.Join("/sys/fs/cgroup", filepath.Clean("/"+path))
				}
			}
		}
		if group != "" {
			h.Limits["configured_cpu_quota"] = readCPUQuota(group)
			h.Limits["configured_memory_max"] = readLimit(filepath.Join(group, "memory.max"), "bytes")
			h.Limits["cpu_quota"], h.Limits["memory_max"] = effectiveCgroupLimits(group, "/sys/fs/cgroup")
			h.Availability["effective_limits"] = "minimum of all visible ancestors through the cgroup namespace mount root; hidden host ancestors are not observable"
			h.Limits["configured_swap_max"] = readLimit(filepath.Join(group, "memory.swap.max"), "bytes")
			h.Limits["swap_max"] = effectiveCgroupFileLimit(group, "/sys/fs/cgroup", "memory.swap.max")
		}
	}
	if profile == "native" {
		h.Availability["profile"] = "native/uncapped diagnostic; never reference or floor evidence"
		return h, group, nil
	}
	if profile != "reference" && profile != "floor" {
		return h, group, fmt.Errorf("unknown host profile %q", profile)
	}
	if err := validateHostProfile(h); err != nil {
		return h, group, err
	}
	h.Validated = true
	return h, group, nil
}

func readCPUQuota(root string) Metric {
	data, err := os.ReadFile(filepath.Join(root, "cpu.max"))
	if err != nil {
		return unavailable("cores", err.Error())
	}
	fields := strings.Fields(string(data))
	if len(fields) != 2 || fields[0] == "max" {
		return unavailable("cores", "cpu.max is unbounded or malformed")
	}
	quota, qerr := strconv.ParseFloat(fields[0], 64)
	period, perr := strconv.ParseFloat(fields[1], 64)
	if qerr != nil || perr != nil || quota <= 0 || period <= 0 {
		return unavailable("cores", "invalid cpu.max")
	}
	return measured(quota/period, "cores")
}

func readLimit(path, unit string) Metric {
	data, err := os.ReadFile(path)
	if err != nil {
		return unavailable(unit, err.Error())
	}
	value, err := strconv.ParseUint(strings.TrimSpace(string(data)), 10, 64)
	if err != nil {
		return unavailable(unit, "unbounded or invalid limit")
	}
	return measured(float64(value), unit)
}

func validateHostProfile(h Host) error {
	if h.OS != "linux" {
		return fmt.Errorf("%s profile requires real Linux cgroup v2; use native for diagnostics", h.Profile)
	}
	cpus, memory := float64(8), float64(16*gib)
	if h.Profile == "floor" {
		cpus, memory = 4, 8*gib
	}
	for key, want := range map[string]float64{"cpu_quota": cpus, "memory_max": memory} {
		m := h.Limits[key]
		if m.Value == nil || *m.Value != want {
			return fmt.Errorf("%s requires %s=%g, observed %+v", h.Profile, key, want, m)
		}
	}
	for key, minimum := range map[string]float64{"logical_cpus": cpus, "host_memory": memory} {
		m := h.Limits[key]
		if m.Value == nil || *m.Value < minimum {
			return fmt.Errorf("%s lacks actual %s capacity; a nominal cgroup limit is insufficient", h.Profile, key)
		}
	}
	if h.Profile == "reference" {
		disk := h.Limits["disk_capacity_bytes"]
		if disk.Value == nil || *disk.Value < .95*60*gib || *disk.Value > 60*gib {
			return errorsDiskQuota()
		}
	}
	return nil
}

func errorsDiskQuota() error {
	return fmt.Errorf("reference requires the intended 60 GiB filesystem (57–60 GiB usable capacity, allowing 5%% filesystem overhead); free space, a smaller OS disk, or a declared quota is not proof")
}

func effectiveCgroupLimits(current, root string) (Metric, Metric) {
	cpu, memory := unavailable("cores", "all visible CPU limits are unbounded"), unavailable("bytes", "all visible memory limits are unbounded")
	if !inside(root, current) {
		return unavailable("cores", "cgroup outside visible mount"), unavailable("bytes", "cgroup outside visible mount")
	}
	for path := filepath.Clean(current); ; path = filepath.Dir(path) {
		for _, entry := range []struct {
			name string
			dest *Metric
			read func(string) Metric
		}{
			{"cpu.max", &cpu, func(path string) Metric { return readCPUQuota(path) }},
			{"memory.max", &memory, func(path string) Metric { return readLimit(filepath.Join(path, "memory.max"), "bytes") }},
		} {
			data, err := os.ReadFile(filepath.Join(path, entry.name))
			if os.IsNotExist(err) && path == filepath.Clean(root) {
				continue
			} // root has no controller limit on some kernels
			if err != nil {
				return unavailable("cores", "ancestor limit unavailable: "+err.Error()), unavailable("bytes", "ancestor limit unavailable: "+err.Error())
			}
			fields := strings.Fields(string(data))
			if len(fields) > 0 && fields[0] == "max" {
				continue
			}
			value := entry.read(path)
			if value.Value == nil {
				return unavailable("cores", "invalid ancestor limit: "+value.Reason), unavailable("bytes", "invalid ancestor limit: "+value.Reason)
			}
			if entry.dest.Value == nil || *value.Value < *entry.dest.Value {
				*entry.dest = value
			}
		}
		if path == filepath.Clean(root) {
			break
		}
	}
	return cpu, memory
}

func effectiveCgroupFileLimit(current, root, name string) Metric {
	value := unavailable("bytes", "all visible "+name+" limits are unbounded")
	if !inside(root, current) {
		return unavailable("bytes", "cgroup outside visible mount")
	}
	for path := filepath.Clean(current); ; path = filepath.Dir(path) {
		data, err := os.ReadFile(filepath.Join(path, name))
		if os.IsNotExist(err) && path == filepath.Clean(root) {
			break
		}
		if err != nil {
			return unavailable("bytes", "ancestor limit unavailable: "+err.Error())
		}
		if strings.TrimSpace(string(data)) != "max" {
			next := readLimit(filepath.Join(path, name), "bytes")
			if next.Value == nil {
				return next
			}
			if value.Value == nil || *next.Value < *value.Value {
				value = next
			}
		}
		if path == filepath.Clean(root) {
			break
		}
	}
	return value
}

func cgroupMetrics(root string, pageSize int) map[string]Metric {
	values := make(map[string]Metric)
	for name, file := range map[string]string{"swap_current_bytes": "memory.swap.current", "memory_current_bytes": "memory.current", "memory_peak_bytes": "memory.peak"} {
		values[name] = readLimit(filepath.Join(root, file), "bytes")
	}
	stat := make(map[string]float64)
	if data, err := os.ReadFile(filepath.Join(root, "memory.stat")); err == nil {
		stat = parseKeyValues(string(data))
	}
	values["swap_in_bytes"] = valueMetric(stat, "pswpin", "bytes", float64(pageSize))
	values["swap_out_bytes"] = valueMetric(stat, "pswpout", "bytes", float64(pageSize))
	events := make(map[string]float64)
	if data, err := os.ReadFile(filepath.Join(root, "memory.events")); err == nil {
		events = parseKeyValues(string(data))
	}
	values["oom_kills"] = valueMetric(events, "oom_kill", "events", 1)
	values["oom_events"] = valueMetric(events, "oom", "events", 1)
	return values
}
