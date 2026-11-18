//go:build acceptance && !linux

package main

import (
	"context"
	"time"
)

type unavailableCohortPeakObserver struct{}

func newCohortPeakObserver() cohortPeakObserver    { return unavailableCohortPeakObserver{} }
func (unavailableCohortPeakObserver) close() error { return nil }
func (unavailableCohortPeakObserver) observe(_ context.Context, phase string, daemonPID int) cohortPeakBookend {
	now := time.Now()
	return cohortPeakBookend{Version: 1, Phase: phase, StartedAt: now, ObservedAt: now, DaemonPID: daemonPID, UnavailableReason: "Linux cgroup v2 required"}
}
