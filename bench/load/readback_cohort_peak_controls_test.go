//go:build acceptance

package main

import (
	"context"
	"encoding/json"
	"testing"
)

type cohortPeakTestObserver struct {
	calls  []string
	closed bool
	reason string
}

func (o *cohortPeakTestObserver) observe(ctx context.Context, phase string, _ int) cohortPeakBookend {
	o.calls = append(o.calls, phase)
	value := uint64(9007199254740993)
	reason := o.reason
	if ctx.Err() != nil {
		reason = ctx.Err().Error()
	}
	return cohortPeakBookend{Version: 1, Phase: phase, PeakBytes: &value, UnavailableReason: reason}
}
func (o *cohortPeakTestObserver) close() error { o.closed = true; return nil }

func TestReadbackCohortPeakDefaultOff(t *testing.T) {
	report := readbackReferenceCaseReport{}
	called := false
	state := beginCohortPeakEvidence(context.Background(), false, &report, func() cohortPeakObserver { called = true; return &cohortPeakTestObserver{} }, 32)
	finishCohortPeakEvidence(context.Background(), state, &report)
	if called || state != nil || len(report.Readiness) != 0 {
		t.Fatal("default-off touched collector or report")
	}
	if *readbackCohortPeakEvidence {
		t.Fatal("cohort collector must default false")
	}
}
func TestReadbackCohortPeakBookendsAndUnavailable(t *testing.T) {
	for _, reason := range []string{"", "unit cgroup unavailable"} {
		t.Run(reason, func(t *testing.T) {
			report := readbackReferenceCaseReport{DaemonPID: 42}
			observer := &cohortPeakTestObserver{reason: reason}
			state := beginCohortPeakEvidence(context.Background(), true, &report, func() cohortPeakObserver { return observer }, 32)
			finishCohortPeakEvidence(context.Background(), state, &report)
			if len(observer.calls) != 2 || observer.calls[0] != "before" || observer.calls[1] != "after" || !observer.closed {
				t.Fatal("bookend order/closure missing")
			}
			for _, phase := range []string{"before", "after"} {
				var got cohortPeakBookend
				if err := json.Unmarshal(report.Readiness["owned_cohort_peak."+phase], &got); err != nil {
					t.Fatal(err)
				}
				if len(got.AgentConnections) != 32 {
					t.Fatal("initialized actor census missing")
				}
				if got.PeakBytes == nil || *got.PeakBytes != 9007199254740993 || got.UnavailableReason != reason {
					t.Fatal("raw integer or unavailability lost")
				}
			}
			if len(report.Errors) != 0 {
				t.Fatal("optional metric changed functional outcome")
			}
		})
	}
}
func TestReadbackCohortPeakCallerCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	report := readbackReferenceCaseReport{}
	observer := &cohortPeakTestObserver{}
	state := beginCohortPeakEvidence(ctx, true, &report, func() cohortPeakObserver { return observer }, 32)
	finishCohortPeakEvidence(ctx, state, &report)
	var got cohortPeakBookend
	if err := json.Unmarshal(report.Readiness["owned_cohort_peak.after"], &got); err != nil {
		t.Fatal(err)
	}
	if got.UnavailableReason == "" || !observer.closed {
		t.Fatal("cancelled collector claimed available or leaked")
	}
}
