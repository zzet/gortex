package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
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
