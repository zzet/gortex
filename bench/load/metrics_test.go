package main

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestProcessCollectorExplicitAvailability(t *testing.T) {
	metrics := newProcessSampler().sample(os.Getpid())
	for name, m := range metrics {
		if err := validMetric(m); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if runtime.GOOS == "darwin" || runtime.GOOS == "linux" {
		if metrics["rss_bytes"].Value == nil || metrics["cpu_seconds"].Value == nil {
			t.Fatalf("native collector missing process observations: %v", metrics)
		}
	}
	if runtime.GOOS == "darwin" && metrics["phys_footprint_bytes"].Value != nil {
		if metrics["logical_written_bytes"].Value == nil || metrics["diskio_written_bytes"].Value == nil || metrics["pss_bytes"].Value != nil {
			t.Fatalf("macOS collector conflates logical/disk writes or PSS: %v", metrics)
		}
	}
}

func TestMachCPUConvertsAbsoluteTicks(t *testing.T) {
	m := machCPUSeconds(12_000_000, 12_000_000, 125, 3)
	if m.Value == nil || *m.Value != 1 {
		t.Fatalf("non-1:1 Mach timebase: %+v", m)
	}
	if m := machCPUSeconds(1, 2, 125, 0); m.Value != nil {
		t.Fatal("unknown Mach timebase fabricated CPU")
	}
}

func TestCgroupSwapIOUsesPagesAndRetainsUnavailable(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{
		"memory.stat": "pswpin 3\npswpout 7\n", "memory.events": "oom 2\noom_kill 1\n",
		"memory.swap.current": "4096\n", "memory.current": "10000\n", "memory.peak": "12000\n", "cpu.max": "800000 100000\n",
	}
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(root, name), []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	metrics := cgroupMetrics(root, 16384)
	for key, want := range map[string]float64{"swap_in_bytes": 3 * 16384, "swap_out_bytes": 7 * 16384, "swap_current_bytes": 4096, "oom_kills": 1} {
		if m := metrics[key]; m.Value == nil || *m.Value != want {
			t.Fatalf("%s = %+v, want %g", key, m, want)
		}
	}
	if m := readCPUQuota(root); m.Value == nil || *m.Value != 8 {
		t.Fatalf("cpu quota %+v", m)
	}
	if err := os.WriteFile(filepath.Join(root, "memory.stat"), []byte("anon 1234\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	metrics = cgroupMetrics(root, 4096)
	if metrics["swap_in_bytes"].Value != nil || metrics["swap_out_bytes"].Value != nil || metrics["swap_current_bytes"].Value == nil {
		t.Fatalf("swap occupancy fabricated unavailable I/O: %v", metrics)
	}
	if m := counterDelta(measured(5, "bytes"), measured(2, "bytes")); m.Value != nil {
		t.Fatal("counter reset became negative/zero measurement")
	}
}

func TestReferenceProfileRejectsNominalInsufficientCapacity(t *testing.T) {
	h := Host{OS: "linux", Profile: "reference", Limits: map[string]Metric{
		"cpu_quota": measured(8, "cores"), "logical_cpus": measured(8, "cores"), "host_memory": measured(32*gib, "bytes"), "memory_max": measured(16*gib, "bytes"), "disk_capacity_bytes": measured(60*gib, "bytes"),
	}}
	if err := validateHostProfile(h); err != nil {
		t.Fatal(err)
	}
	h.Limits["host_memory"] = measured(8*gib, "bytes")
	if err := validateHostProfile(h); err == nil {
		t.Fatal("8 GiB host masqueraded as reference via nominal memory.max")
	}
	h.Limits["host_memory"] = measured(32*gib, "bytes")
	h.Limits["disk_capacity_bytes"] = measured(1024*gib, "bytes")
	if err := validateHostProfile(h); err == nil {
		t.Fatal("large shared disk/free space masqueraded as enforced quota")
	}
	h.Limits["disk_capacity_bytes"] = measured(30*gib, "bytes")
	if err := validateHostProfile(h); err == nil {
		t.Fatal("30GiB OS filesystem masqueraded as 60GiB reference data disk")
	}
	h.OS = "darwin"
	if err := validateHostProfile(h); err == nil {
		t.Fatal("native Mac masqueraded as Linux reference")
	}
}

func TestEffectiveCgroupLimitsIncludeTighterAncestors(t *testing.T) {
	root := t.TempDir()
	child := filepath.Join(root, "parent", "child")
	if err := os.MkdirAll(child, 0o700); err != nil {
		t.Fatal(err)
	}
	for dir, values := range map[string][2]string{root: {"max 100000", "max"}, filepath.Dir(child): {"400000 100000", "8589934592"}, child: {"800000 100000", "17179869184"}} {
		for i, name := range []string{"cpu.max", "memory.max"} {
			if err := os.WriteFile(filepath.Join(dir, name), []byte(values[i]), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	cpu, memory := effectiveCgroupLimits(child, root)
	if cpu.Value == nil || *cpu.Value != 4 || memory.Value == nil || *memory.Value != 8*gib {
		t.Fatalf("ancestor limits ignored: %+v %+v", cpu, memory)
	}
	h := Host{OS: "linux", Profile: "reference", Limits: map[string]Metric{"cpu_quota": cpu, "memory_max": memory, "logical_cpus": measured(8, "cores"), "host_memory": measured(20*gib, "bytes"), "disk_capacity_bytes": measured(60*gib, "bytes")}}
	if err := validateHostProfile(h); err == nil {
		t.Fatal("tighter parent accepted as reference")
	}
	for dir, limit := range map[string]string{root: "max", filepath.Dir(child): "4294967296", child: "8589934592"} {
		if err := os.WriteFile(filepath.Join(dir, "memory.swap.max"), []byte(limit), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if swap := effectiveCgroupFileLimit(child, root, "memory.swap.max"); swap.Value == nil || *swap.Value != 4*gib {
		t.Fatalf("tighter ancestor swap cap ignored: %+v", swap)
	}
}
