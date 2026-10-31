package main

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"sync"
	"time"
)

type phaseMonitor struct {
	p            *privateRun
	sampler      processSampler
	group        string
	pid          int
	started      time.Time
	daemonBefore map[string]Metric
	clientBefore map[string]Metric
	groupBefore  map[string]Metric
	peaks        map[string]Metric
	cancel       context.CancelFunc
	done         sync.WaitGroup
}

func startMonitor(p *privateRun, sampler processSampler, group string) *phaseMonitor {
	m := &phaseMonitor{p: p, sampler: sampler, group: group, pid: p.process.Process.Pid, started: time.Now(), peaks: make(map[string]Metric)}
	m.daemonBefore, m.clientBefore = sampler.sample(m.pid), sampler.sample(os.Getpid())
	if group != "" {
		m.groupBefore = cgroupMetrics(group, os.Getpagesize())
	}
	ctx, cancel := context.WithCancel(context.Background())
	m.cancel = cancel
	m.observe()
	m.done.Go(func() {
		ticker := time.NewTicker(500 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				m.observe()
			}
		}
	})
	return m
}

func (m *phaseMonitor) observe() {
	values := m.sampler.sample(m.pid)
	for key, path := range map[string]string{"wal_bytes": m.p.Store + "-wal", "store_bytes": m.p.Store} {
		values[key] = unavailable("bytes", "store file not present at sample")
		if info, err := os.Stat(path); err == nil {
			values[key] = measured(float64(info.Size()), "bytes")
		}
	}
	for _, key := range []string{"rss_bytes", "pss_bytes", "rss_anon_bytes", "phys_footprint_bytes", "wal_bytes", "store_bytes"} {
		value := values[key]
		old, exists := m.peaks[key]
		if !exists || (value.Value != nil && (old.Value == nil || *value.Value > *old.Value)) {
			m.peaks[key] = value
		}
	}
}

func (m *phaseMonitor) finish(phase *Phase) {
	m.cancel()
	m.done.Wait()
	m.observe()
	elapsed := time.Since(m.started).Seconds()
	counterEnd := time.Now()
	after := m.sampler.sample(m.pid)
	window, _ := json.Marshal(measurementWindow{Started: m.started, Ended: counterEnd, Method: "process/cgroup counter snapshots bracket this interval; sequential reads have small endpoint skew; workload, background work and delayed I/O can contribute; these are not sums of patch intervals"})
	if phase.Gate == nil {
		phase.Gate = make(map[string]json.RawMessage)
	}
	phase.Gate["process_counter_window"] = window
	clientAfter := m.sampler.sample(os.Getpid())
	for key, value := range m.peaks {
		phase.Metrics["peak_"+key] = value
	}
	successes := 0
	for _, row := range phase.Attempts {
		if row.Role == "workload" && row.Outcome == "success" {
			successes++
		}
	}
	for prefix, endpoints := range map[string][2]Metric{
		"daemon_cpu": {m.daemonBefore["cpu_seconds"], after["cpu_seconds"]},
		"client_cpu": {m.clientBefore["cpu_seconds"], clientAfter["cpu_seconds"]},
	} {
		delta := counterDelta(endpoints[0], endpoints[1])
		phase.Metrics[prefix+"_seconds"] = delta
		avg, perCall := unavailable("cores", delta.Reason), unavailable("s/call", "no successful workload calls or CPU endpoint")
		if delta.Value != nil {
			avg = measured(*delta.Value/elapsed, "cores")
			if successes > 0 {
				perCall = measured(*delta.Value/float64(successes), "s/call")
			}
		}
		phase.Metrics[prefix+"_avg_cores"], phase.Metrics[prefix+"_per_success"] = avg, perCall
	}
	// Total client CPU is a conservative upper bound for busy-handling CPU.
	// Never sum overlapping per-call process CPU intervals as attribution.
	phase.Metrics["client_busy_cpu_upper_bound_cores"] = phase.Metrics["client_cpu_avg_cores"]
	for _, key := range []string{"logical_written_bytes", "diskio_written_bytes", "syscall_written_bytes"} {
		delta := counterDelta(m.daemonBefore[key], after[key])
		phase.Metrics[key] = delta
		mib := unavailable("MiB", delta.Reason)
		if delta.Value != nil {
			mib = measured(*delta.Value/(1024*1024), "MiB")
		}
		phase.Metrics[key+"_mib"] = mib
	}
	groupAfter := make(map[string]Metric)
	if m.group != "" {
		groupAfter = cgroupMetrics(m.group, os.Getpagesize())
	}
	for _, key := range []string{"swap_in_bytes", "swap_out_bytes", "oom_kills", "oom_events"} {
		if m.group == "" {
			unit := "events"
			if strings.HasSuffix(key, "_bytes") {
				unit = "bytes"
			}
			phase.Metrics["cgroup_"+key] = unavailable(unit, "no Linux cgroup v2 metrics")
		} else {
			phase.Metrics["cgroup_"+key] = counterDelta(m.groupBefore[key], groupAfter[key])
		}
	}
	for _, key := range []string{"swap_current_bytes", "memory_current_bytes", "memory_peak_bytes"} {
		value := unavailable("bytes", "no Linux cgroup v2 metrics")
		if m.group != "" {
			value = groupAfter[key]
		}
		phase.Metrics["cgroup_end_"+key] = value
	}
}
