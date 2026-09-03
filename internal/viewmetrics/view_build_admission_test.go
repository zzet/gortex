package viewmetrics

import (
	"testing"
	"time"
)

func TestViewBuildAndDeferredMetricsKeepFixedVocabulary(t *testing.T) {
	recorder := New()
	recorder.Count(
		ViewBuildAdmissionTotal,
		BuildPriorityInteractive,
		BuildAdmissionRejected,
	)
	recorder.Count(CoordinatorCycleTotal, OutcomeDeferred)
	recorder.Count(RefViewSelectionTotal, RefViewDeferred)
	recorder.AddGauge(ViewBuildQueue, 1, BuildPriorityBackground)
	recorder.Observe(ViewBuildWaitSeconds, 5*time.Millisecond, BuildPriorityInteractive)

	snapshot := recorder.Snapshot()
	counters := snapshot.Counters
	for _, key := range []string{
		"views_build_admission_total{priority=interactive,outcome=rejected}",
		"views_coordinator_cycle_total{outcome=deferred}",
		"views_ref_view_selection_total{outcome=deferred}",
	} {
		if got := counters[key]; got != 1 {
			t.Errorf("counter %q = %d, want 1; snapshot=%v", key, got, counters)
		}
	}
	for _, key := range []string{
		"views_build_admission_total{priority=other,outcome=other}",
		"views_coordinator_cycle_total{outcome=other}",
		"views_ref_view_selection_total{outcome=other}",
	} {
		if got := counters[key]; got != 0 {
			t.Errorf("counter %q = %d, want 0; snapshot=%v", key, got, counters)
		}
	}
	if got := snapshot.Gauges["views_build_queue{priority=background}"]; got != 1 {
		t.Errorf("view-build queue gauge = %d, want 1; snapshot=%v", got, snapshot.Gauges)
	}
	duration := snapshot.Durations["views_build_wait_seconds{priority=interactive}"]
	if duration.Count != 1 || duration.Total != 5*time.Millisecond || duration.Longest != 5*time.Millisecond {
		t.Errorf("view-build wait duration = %+v, want count=1 total=longest=5ms", duration)
	}
}
