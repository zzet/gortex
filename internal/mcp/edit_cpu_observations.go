package mcp

import (
	"fmt"
	"os"
	"sync/atomic"
	"time"

	"github.com/zzet/gortex/internal/platform"
)

type editOperationCountSnapshot struct {
	Completed               uint64 `json:"completed"`
	CommittedWithError      uint64 `json:"committed_with_error"`
	FailedAfterSourceEffect uint64 `json:"failed_after_source_effect"`
	RolledBack              uint64 `json:"rolled_back"`
	UnknownSourceComparison uint64 `json:"unknown_source_comparison"`
}

func (c *editOperationCounters) snapshot() editOperationCountSnapshot {
	return editOperationCountSnapshot{
		Completed: c.completed.Load(), CommittedWithError: c.committedWithError.Load(),
		FailedAfterSourceEffect: c.failedAfterSourceEffect.Load(), RolledBack: c.rolledBack.Load(),
		UnknownSourceComparison: c.unknownSourceComparison.Load(),
	}
}

func (c editOperationCountSnapshot) follows(before editOperationCountSnapshot) bool {
	return c.Completed >= before.Completed && c.CommittedWithError >= before.CommittedWithError &&
		c.FailedAfterSourceEffect >= before.FailedAfterSourceEffect && c.RolledBack >= before.RolledBack &&
		c.UnknownSourceComparison >= before.UnknownSourceComparison
}

type editCPUSample struct {
	started, finished time.Duration
	boundsKnown       bool
	cpu               time.Duration
	available         bool
	reason            string
	before, after     editOperationCountSnapshot
}

type editCPUObservation struct {
	origin   time.Time
	epoch    string
	baseline editCPUSample // immutable: no reset or initialization during stats reads
}

type editCPUSampleWire struct {
	StartedSeconds  *float64                   `json:"sample_started_seconds"`
	FinishedSeconds *float64                   `json:"sample_finished_seconds"`
	CPUSeconds      *float64                   `json:"cpu_seconds"`
	CPUAvailable    bool                       `json:"cpu_available"`
	CPUReason       string                     `json:"cpu_reason,omitempty"`
	CountsBefore    editOperationCountSnapshot `json:"counts_before"`
	CountsAfter     editOperationCountSnapshot `json:"counts_after"`
}

type editProcessWindow struct {
	Scope             string             `json:"scope"`
	CountScope        string             `json:"count_scope"`
	CPUScope          string             `json:"cpu_scope"`
	Epoch             string             `json:"epoch"`
	Baseline          *editCPUSampleWire `json:"baseline"`
	Sample            editCPUSampleWire  `json:"sample"`
	CPUSecondsPerEdit *float64           `json:"cpu_seconds_per_edit"`
	AverageState      string             `json:"average_state"`
	AverageReason     string             `json:"average_reason,omitempty"`
}

var editCPUEpochSequence atomic.Uint64

// initializeEditCPUObservation runs only at the end of NewServer, before any
// caller can execute its registered tools. A literal Server has no baseline.
func (s *Server) initializeEditCPUObservation(sample func() (time.Duration, error)) {
	origin := time.Now()
	s.editCPU = &editCPUObservation{
		origin:   origin,
		epoch:    fmt.Sprintf("%d-%d-%d", os.Getpid(), origin.UnixNano(), editCPUEpochSequence.Add(1)),
		baseline: captureEditCPU(&s.editOperations, origin, sample),
	}
}

// captureEditCPU is called only for constructor/stats observations. All five
// outcome counters bracket ONE process sample. An unstable bracket is retained,
// never retried into a silently different window or presented as an exact count.
func captureEditCPU(c *editOperationCounters, origin time.Time, sample func() (time.Duration, error)) editCPUSample {
	result := editCPUSample{before: c.snapshot(), boundsKnown: !origin.IsZero()}
	if result.boundsKnown {
		result.started = time.Since(origin)
	}
	cpu, err := sample()
	if result.boundsKnown {
		result.finished = time.Since(origin)
	}
	result.after = c.snapshot()
	if err != nil {
		reason := []rune(err.Error())
		if len(reason) > 256 {
			reason = reason[:256]
		}
		result.reason = string(reason)
	} else if cpu < 0 {
		result.reason = "invalid negative process CPU sample"
	} else {
		result.cpu, result.available = cpu, true
	}
	return result
}

func (sample editCPUSample) wire() editCPUSampleWire {
	result := editCPUSampleWire{
		CPUAvailable: sample.available, CPUReason: sample.reason,
		CountsBefore: sample.before, CountsAfter: sample.after,
	}
	if sample.boundsKnown {
		started, finished := sample.started.Seconds(), sample.finished.Seconds()
		result.StartedSeconds, result.FinishedSeconds = &started, &finished
	}
	if sample.available {
		cpu := sample.cpu.Seconds()
		result.CPUSeconds = &cpu
	}
	return result
}

func (s *Server) editProcessWindowSnapshot(sample func() (time.Duration, error)) editProcessWindow {
	var origin time.Time
	if s.editCPU != nil {
		origin = s.editCPU.origin
	}
	return projectEditProcessWindow(s.editCPU, captureEditCPU(&s.editOperations, origin, sample))
}

func projectEditProcessWindow(observation *editCPUObservation, current editCPUSample) editProcessWindow {
	result := editProcessWindow{
		Scope: "process_window", CountScope: "server_all_sessions_views",
		CPUScope: "whole_process_including_background", Sample: current.wire(), AverageState: "unavailable",
	}
	if observation == nil {
		result.AverageReason = "baseline_uninitialized"
		return result
	}
	result.Epoch = observation.epoch
	baseline := observation.baseline
	wire := baseline.wire() // caller-owned scalar pointers; never expose the stored baseline
	result.Baseline = &wire
	switch {
	case !baseline.available:
		result.AverageReason = "baseline_cpu_unavailable"
	case baseline.before != (editOperationCountSnapshot{}) || baseline.after != (editOperationCountSnapshot{}):
		result.AverageReason = "baseline_counts_not_zero"
	case !current.available:
		result.AverageReason = "cpu_unavailable"
	case !baseline.boundsKnown || !current.boundsKnown || baseline.started < 0 || baseline.finished < baseline.started ||
		current.started < baseline.finished || current.finished < current.started || current.cpu < baseline.cpu:
		result.AverageReason = "invalid_order"
	case !current.before.follows(baseline.after) || !current.after.follows(current.before):
		result.AverageReason = "invalid_count_order"
	case current.before != current.after:
		result.AverageReason = "unstable_count_bracket"
	case current.after.Completed == 0:
		result.AverageReason = "no_completed_edits"
	case current.after.CommittedWithError != 0 || current.after.FailedAfterSourceEffect != 0 || current.after.UnknownSourceComparison != 0 || current.after.RolledBack != 0:
		result.AverageReason = "non_clean_window"
	default:
		average := (current.cpu - baseline.cpu).Seconds() / float64(current.after.Completed)
		result.CPUSecondsPerEdit, result.AverageState = &average, "measured_process_window"
	}
	return result
}

// processEditWindow is shared by graph_stats and gortex://stats. The graph part
// of those payloads is view-scoped; this explicit process-window block is not.
func (s *Server) processEditWindow() editProcessWindow {
	return s.editProcessWindowSnapshot(platform.ProcessCPUTime)
}
