//go:build acceptance

package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"time"
)

var readbackCohortPeakEvidence = flag.Bool("readback-reference-cohort-peak-evidence", false, "record raw owned-unit lifetime memory.peak bookends; qualification requires matching fresh single-build unit and closure evidence")

type cohortFileIdentity struct {
	Device  uint64 `json:"device"`
	Inode   uint64 `json:"inode"`
	MountID uint64 `json:"mount_id"`
	FSType  uint64 `json:"fs_type"`
}

type cohortProcess struct {
	PID        int    `json:"pid"`
	StatBefore string `json:"stat_before"`
	StatAfter  string `json:"stat_after"`
	Cgroup     string `json:"cgroup"`
}

type cohortPeakBookend struct {
	Version           int                `json:"version"`
	Phase             string             `json:"phase"`
	StartedAt         time.Time          `json:"started_at"`
	MonotonicNS       uint64             `json:"monotonic_ns"`
	ObservedAt        time.Time          `json:"observed_at"`
	Scope             string             `json:"scope"`
	Unit              string             `json:"unit"`
	Group             string             `json:"group"`
	BootID            string             `json:"boot_id"`
	DriverPID         int                `json:"driver_pid"`
	DaemonPID         int                `json:"daemon_pid"`
	SelfCgroup        string             `json:"self_cgroup"`
	Cgroup            cohortFileIdentity `json:"cgroup"`
	PeakFile          cohortFileIdentity `json:"peak_file"`
	RawMemoryPeak     string             `json:"raw_memory_peak"`
	PeakBytes         *uint64            `json:"peak_bytes"`
	Members           []cohortProcess    `json:"members"`
	AgentConnections  []string           `json:"agent_connections"`
	UnavailableReason string             `json:"unavailable_reason"`
}

type cohortPeakObserver interface {
	observe(context.Context, string, int) cohortPeakBookend
	close() error
}

type cohortPeakEvidence struct {
	observer cohortPeakObserver
	agents   []string
}

func retainCohortPeakBookend(report *readbackReferenceCaseReport, value cohortPeakBookend) {
	// No interface values or floating-point conversion: raw uint64 bytes survive JSON.
	raw, err := json.Marshal(value)
	if err != nil {
		return
	} // Typed fields cannot fail; this optional metric never rewrites functional results.
	if report.Readiness == nil {
		report.Readiness = make(map[string]json.RawMessage)
	}
	report.Readiness["owned_cohort_peak."+value.Phase] = raw
}

func beginCohortPeakEvidence(ctx context.Context, enabled bool, report *readbackReferenceCaseReport, factory func() cohortPeakObserver, initializedClients int) *cohortPeakEvidence {
	if !enabled {
		return nil
	}
	observer := factory()
	agents := make([]string, initializedClients)
	for i := range agents {
		agents[i] = fmt.Sprintf("fixed-editor-%d", i)
	}
	value := observer.observe(ctx, "before", report.DaemonPID)
	value.AgentConnections = agents
	retainCohortPeakBookend(report, value)
	return &cohortPeakEvidence{observer: observer, agents: agents}
}

func finishCohortPeakEvidence(ctx context.Context, state *cohortPeakEvidence, report *readbackReferenceCaseReport) {
	if state == nil {
		return
	}
	value := state.observer.observe(ctx, "after", report.DaemonPID)
	value.AgentConnections = state.agents
	if err := state.observer.close(); err != nil {
		value.PeakBytes = nil
		value.UnavailableReason = "held cgroup FD close: " + err.Error()
	}
	retainCohortPeakBookend(report, value)
}
