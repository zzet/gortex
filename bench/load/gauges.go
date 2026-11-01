package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"
)

// gaugeInterval is the daemon_health cadence a gauge series asks for. The
// daemon clamps it to at least a second.
var gaugeInterval = 10 * time.Second

// gaugeRow pairs one daemon_health push — the daemon's own Go, operating
// system and SQLite gauges and its live checkout coordinators — with the
// harness's external sample of the daemon process taken as the push arrived.
type gaugeRow struct {
	Received time.Time         `json:"received"`
	Process  map[string]Metric `json:"process"`
	Health   json.RawMessage   `json:"daemon_health"`
}

// gaugeSeries holds one dedicated session subscribed to daemon_health. The
// daemon runs its health ticker only while a session is subscribed, so a run
// that does not ask for the series costs the daemon nothing.
type gaugeSeries struct {
	client  *socketClient
	sampler processSampler
	pid     int
	group   string
	cancel  context.CancelFunc
	done    sync.WaitGroup

	mu    sync.Mutex
	rows  []gaugeRow
	watch *heapWindow
}

func startGaugeSeries(ctx context.Context, p *privateRun, sampler processSampler, group string) (*gaugeSeries, error) {
	client, err := openMCP(ctx, p.Socket, p.Repos[0].Path)
	if err != nil {
		return nil, err
	}
	if err := subscribeHealth(ctx, client, "subscribe"); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("daemon_health subscription: %w", err)
	}
	s := &gaugeSeries{client: client, sampler: sampler, pid: p.process.Process.Pid, group: group}
	recordCtx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	s.done.Go(func() {
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		for {
			s.record()
			select {
			case <-recordCtx.Done():
				return
			case <-ticker.C:
			}
		}
	})
	return s, nil
}

func subscribeHealth(ctx context.Context, client *socketClient, operation string) error {
	callCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	data, err := client.call(callCtx, "tools/call", map[string]any{"name": "session", "arguments": map[string]any{
		"operation": operation, "channel": "daemon_health", "arguments": map[string]any{"interval_ms": gaugeInterval.Milliseconds()},
	}})
	if err == nil {
		err = checkToolResult(data)
	}
	return err
}

// record turns every daemon_health push received since the last call into a
// row. The dedicated session receives nothing else worth keeping, so its
// notification buffer is drained rather than left to reach its cap.
func (s *gaugeSeries) record() {
	s.client.mu.Lock()
	received := s.client.notifies
	s.client.notifies = nil
	s.client.mu.Unlock()
	for _, notification := range received {
		var frame struct {
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if json.Unmarshal(notification.Payload, &frame) != nil || frame.Method != "notifications/daemon_health" {
			continue
		}
		row := gaugeRow{Received: notification.Received, Process: s.sample(), Health: frame.Params}
		s.mu.Lock()
		s.rows = append(s.rows, row)
		if s.watch != nil {
			s.watch.observe(row.Process)
		}
		s.mu.Unlock()
	}
}

// sample is the external view of the daemon process. On Linux it adds the
// proportional set of the daemon's descendants; in a cgroup it adds the
// group's whole charge, which includes them, split into anonymous and file
// pages.
func (s *gaugeSeries) sample() map[string]Metric {
	values := s.sampler.sample(s.pid)
	out := make(map[string]Metric)
	for _, key := range []string{"rss_bytes", "pss_bytes", "rss_anon_bytes", "phys_footprint_bytes"} {
		out[key] = values[key]
	}
	if runtime.GOOS == "linux" {
		out["children_pss_bytes"] = descendantPSS("/proc", s.pid)
	}
	if s.group != "" {
		out["cgroup_memory_current_bytes"] = cgroupMetrics(s.group, os.Getpagesize())["memory_current_bytes"]
		maps.Copy(out, cgroupAnonFile(s.group))
	}
	return out
}

// stop ends the subscription and returns every row recorded. It runs on
// cleanup paths too, so it does not inherit a cancelled run's context.
func (s *gaugeSeries) stop() ([]gaugeRow, error) {
	err := subscribeHealth(context.Background(), s.client, "unsubscribe")
	s.cancel()
	s.done.Wait()
	s.record()
	err = errors.Join(err, s.client.Close())
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rows, err
}

// heapWindow keeps two heap profiles of one load window: one taken as the
// load starts, and one at the highest daemon footprint the series sampled
// during it (or at the window's end, when that is higher).
type heapWindow struct {
	ctx    context.Context
	status json.RawMessage
	start  string
	peak   string
	high   float64
	err    error
}

// watchLoad takes the load-start profile now and the peak profile as rows
// arrive, until endLoad.
func (s *gaugeSeries) watchLoad(ctx context.Context, status json.RawMessage, base string) *heapWindow {
	w := &heapWindow{ctx: ctx, status: status, start: base + ".heap-load-start.pprof", peak: base + ".heap-load-peak.pprof", high: -1}
	w.err = captureHeapProfile(ctx, status, w.start)
	s.mu.Lock()
	s.watch = w
	s.mu.Unlock()
	return w
}

func (s *gaugeSeries) endLoad(w *heapWindow) (start, peak string, err error) {
	s.record()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.watch = nil
	w.observe(s.sample())
	return w.start, w.peak, w.err
}

// observe replaces the peak profile when this sample is the window's highest.
func (w *heapWindow) observe(process map[string]Metric) {
	value, ok := footprintOf(process)
	if !ok || value <= w.high {
		return
	}
	w.high = value
	partial := w.peak + ".partial"
	_ = os.Remove(partial)
	err := captureHeapProfile(w.ctx, w.status, partial)
	if err == nil {
		err = os.Rename(partial, w.peak)
	}
	if err != nil {
		// One failure is reported; retrying on every later row would only
		// repeat it.
		w.err = errors.Join(w.err, fmt.Errorf("peak heap profile %s: %w", filepath.Base(w.peak), err))
		w.high = math.Inf(1)
	}
}

// footprintOf picks the external memory figure peaks are judged by: PSS on
// Linux, the physical footprint on macOS, the resident set otherwise.
func footprintOf(process map[string]Metric) (float64, bool) {
	for _, key := range []string{"pss_bytes", "phys_footprint_bytes", "rss_bytes"} {
		if value := process[key].Value; value != nil {
			return *value, true
		}
	}
	return 0, false
}

// wantsGaugeSeries says whether a run records the gauge series: when asked,
// and always in step mode.
func wantsGaugeSeries(o runOptions) bool {
	return o.Gauges || len(o.WorktreeSteps) > 0
}

// watchLoadProfiles arms an ordinary load window's heap profiles when the run
// asked for them; nil otherwise. Peak profiles are captured while the load
// runs, so a series-only run is the one that measures what the gauges alone
// cost.
func (s *gaugeSeries) watchLoadProfiles(ctx context.Context, o runOptions, p *privateRun, phase *Phase) *heapWindow {
	if s == nil || !o.HeapProfiles {
		return nil
	}
	return s.watchLoad(ctx, lastStatus(phase), filepath.Join(p.Root, phase.Name))
}

// endLoadProfiles ends the load window and keeps its profiles as artifacts.
func (s *gaugeSeries) endLoadProfiles(w *heapWindow, artifacts map[string]string) error {
	if w == nil {
		return nil
	}
	start, peak, err := s.endLoad(w)
	artifacts["heap_profile_load_start"], artifacts["heap_profile_load_peak"] = start, peak
	return err
}

// stopInto ends the series and keeps its rows in the phase.
func (s *gaugeSeries) stopInto(phase *Phase) error {
	rows, err := s.stop()
	phase.Gate["gauge_series"], _ = json.Marshal(rows)
	return err
}

// lastStatus is the latest daemon status receipt, which names the pprof
// listener; nil when status was never captured.
func lastStatus(phase *Phase) json.RawMessage {
	if len(phase.Status) == 0 {
		return nil
	}
	return phase.Status[len(phase.Status)-1]
}
