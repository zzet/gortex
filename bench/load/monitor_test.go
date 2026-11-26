package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type absentSampler struct{}

func (absentSampler) sample(int) map[string]Metric { return absentProcessMetrics("test sampler") }

// TestMonitorReportsWALP95 samples a store whose WAL sits at 1000 bytes but
// for one burst: the phase reports the burst as the peak and the steady size
// as the p95, counting only samples that found a WAL file.
func TestMonitorReportsWALP95(t *testing.T) {
	store := filepath.Join(t.TempDir(), "store.db")
	self, err := os.FindProcess(os.Getpid())
	require.NoError(t, err)
	m := &phaseMonitor{
		p:       &privateRun{Store: store, process: &exec.Cmd{Process: self}},
		sampler: absentSampler{},
		pid:     os.Getpid(),
		started: time.Now(),
		peaks:   make(map[string]Metric),
		cancel:  func() {},
	}
	setWAL := func(size int) {
		require.NoError(t, os.WriteFile(store+"-wal", make([]byte, size), 0o600))
	}
	m.observe() // no WAL yet: not a sample
	for i := range 40 {
		setWAL(1000)
		m.observe()
		if i == 20 {
			setWAL(1 << 20)
			m.observe()
		}
	}
	setWAL(1000) // finish takes one last sample
	phase := &Phase{Metrics: make(map[string]Metric)}
	m.finish(phase)

	require.NotNil(t, phase.Metrics["peak_wal_bytes"].Value)
	require.Equal(t, float64(1<<20), *phase.Metrics["peak_wal_bytes"].Value)
	p95 := phase.Metrics["p95_wal_bytes"]
	require.NotNil(t, p95.Value, p95.Reason)
	require.Equal(t, float64(1000), *p95.Value)
	require.Equal(t, "bytes", p95.Unit)

	t.Run("a phase that never found a WAL", func(t *testing.T) {
		m := &phaseMonitor{
			p:       &privateRun{Store: filepath.Join(t.TempDir(), "store.db"), process: &exec.Cmd{Process: self}},
			sampler: absentSampler{},
			pid:     os.Getpid(),
			started: time.Now(),
			peaks:   make(map[string]Metric),
			cancel:  func() {},
		}
		m.observe()
		phase := &Phase{Metrics: make(map[string]Metric)}
		m.finish(phase)
		require.Nil(t, phase.Metrics["p95_wal_bytes"].Value)
		require.NotEmpty(t, phase.Metrics["p95_wal_bytes"].Reason)
	})
}

func TestMonitorWALRawCensusRetainsActualFileSizesAndMissingSamples(t *testing.T) {
	store := filepath.Join(t.TempDir(), "store.db")
	self, err := os.FindProcess(os.Getpid())
	require.NoError(t, err)
	m := &phaseMonitor{p: &privateRun{Store: store, process: &exec.Cmd{Process: self}}, sampler: absentSampler{}, pid: os.Getpid(), started: time.Now(), peaks: make(map[string]Metric), cancel: func() {}}
	require.NoError(t, os.WriteFile(store+"-wal", make([]byte, 1234), 0600))
	m.observe()
	require.NoError(t, os.WriteFile(store+"-wal", make([]byte, 5678), 0600))
	p := &Phase{Metrics: make(map[string]Metric)}
	m.finish(p)
	var raw struct {
		Version int `json:"schema_version"`
		Count   int `json:"count"`
		Samples []struct {
			Bytes *float64 `json:"bytes"`
			Error string   `json:"error"`
		} `json:"samples"`
	}
	require.NoError(t, json.Unmarshal(p.Gate["wal_samples_v1"], &raw))
	require.Equal(t, 1, raw.Version)
	require.Equal(t, 2, raw.Count)
	require.Len(t, raw.Samples, 2)
	require.Equal(t, float64(1234), *raw.Samples[0].Bytes)
	require.Equal(t, float64(5678), *raw.Samples[1].Bytes)
	require.NoError(t, os.Remove(store+"-wal"))
	m.observe()
	p = &Phase{Metrics: make(map[string]Metric)}
	m.finish(p)
	require.NoError(t, json.Unmarshal(p.Gate["wal_samples_v1"], &raw))
	require.Equal(t, 4, raw.Count)
	require.Nil(t, raw.Samples[2].Bytes)
	require.NotEmpty(t, raw.Samples[2].Error)
}

// Pressure and major faults come from the run's cgroup files here; the host
// files exist only on Linux. A missing file, line or counter is unavailable.
func TestMonitorPressureAndMajorFaultsNeverInferZero(t *testing.T) {
	for _, tc := range []struct {
		stat  string
		value *float64
	}{
		{"123 (cmd with ) paren) S 1 2 3 4 5 6 7 8 42 9 250 50", new(42.0)},
		{"malformed", nil},
		{"123 (cmd) S 1 2", nil},
		{"123 (cmd) S 1 2 3 4 5 6 7 8 x 9", nil},
	} {
		got := procStatMajorFaults(tc.stat)
		if (got.Value == nil) != (tc.value == nil) || tc.value != nil && *got.Value != *tc.value || got.Unit != "faults" || got.Value == nil && got.Reason == "" {
			t.Fatalf("stat %q: %+v", tc.stat, got)
		}
	}
	group := t.TempDir()
	setPressure := func(resource, body string) {
		require.NoError(t, os.WriteFile(filepath.Join(group, resource+".pressure"), []byte(body), 0o600))
	}
	setPressure("cpu", "some avg10=1.50 avg60=0.00 avg300=0.00 total=1000\nfull avg10=0.00 avg60=0.00 avg300=0.00 total=10\n")
	setPressure("memory", "some avg10=0.00 avg60=0.00 avg300=0.00 total=500\n") // no full line
	self, err := os.FindProcess(os.Getpid())
	require.NoError(t, err)
	m := &phaseMonitor{p: &privateRun{Store: filepath.Join(t.TempDir(), "store.db"), process: &exec.Cmd{Process: self}}, sampler: absentSampler{}, group: group, pid: os.Getpid(), started: time.Now(), peaks: make(map[string]Metric), cancel: func() {}}
	m.pressureBefore = m.pressure()
	m.observe()
	setPressure("cpu", "some avg10=7.25 avg60=0.00 avg300=0.00 total=4000\nfull avg10=0.00 avg60=0.00 avg300=0.00 total=10\n")
	m.observe()
	setPressure("cpu", "some avg10=2.00 avg60=0.00 avg300=0.00 total=5000\nfull avg10=0.00 avg60=0.00 avg300=0.00 total=10\n")
	phase := &Phase{Metrics: make(map[string]Metric)}
	m.finish(phase)

	value := func(name string) float64 {
		metric := phase.Metrics[name]
		require.NotNil(t, metric.Value, "%s: %+v", name, metric)
		return *metric.Value
	}
	require.Equal(t, 4000.0, value("cgroup_psi_cpu_some_stall_us"))
	require.Equal(t, 7.25, value("cgroup_psi_cpu_some_peak_avg10_pct"))
	require.Equal(t, 0.0, value("cgroup_psi_cpu_full_stall_us")) // an observed zero delta
	require.Equal(t, 0.0, value("cgroup_psi_memory_some_stall_us"))
	for _, name := range []string{"cgroup_psi_memory_full_stall_us", "cgroup_psi_memory_full_peak_avg10_pct", "cgroup_psi_io_some_stall_us", "cgroup_psi_io_full_peak_avg10_pct", "daemon_major_faults"} {
		metric, present := phase.Metrics[name]
		require.True(t, present, name)
		require.Nil(t, metric.Value, "%s inferred a value: %+v", name, metric)
		require.NotEmpty(t, metric.Reason, name)
	}
	if runtime.GOOS != "linux" {
		for _, resource := range pressureResources {
			for _, kind := range []string{"some", "full"} {
				for _, suffix := range []string{"_stall_us", "_peak_avg10_pct"} {
					metric := phase.Metrics["host_psi_"+resource+"_"+kind+suffix]
					require.Nil(t, metric.Value, "host pressure off Linux: %+v", metric)
					require.NotEmpty(t, metric.Reason)
				}
			}
		}
	}
}
