package viewmetrics

import (
	"testing"
	"time"
)

// The registry's contract, stated as tests: the catalog is an allow-list, the
// vocabularies are a clamp, and neither can be bypassed by a caller.

func TestRegistryRecordsDeclaredSeries(t *testing.T) {
	r := New()
	r.Count(CoordinatorCycleTotal, OutcomeBuiltCommit)
	r.Count(CoordinatorCycleTotal, OutcomeBuiltCommit)
	r.Count(CoordinatorCycleTotal, OutcomeSkipped)

	got := r.Snapshot().Counters
	if want := int64(2); got[CoordinatorCycleTotal+"{outcome=built_commit}"] != want {
		t.Fatalf("built_commit = %d, want %d (counters: %v)",
			got[CoordinatorCycleTotal+"{outcome=built_commit}"], want, got)
	}
	if want := int64(1); got[CoordinatorCycleTotal+"{outcome=skipped}"] != want {
		t.Fatalf("skipped = %d, want %d", got[CoordinatorCycleTotal+"{outcome=skipped}"], want)
	}
}

func TestRegistryDropsAnUndeclaredSeries(t *testing.T) {
	r := New()
	r.Count("views_not_a_series", "whatever")
	if got := len(r.Snapshot().Counters); got != 0 {
		t.Fatalf("an undeclared series recorded %d counters, want none", got)
	}
}

// TestRegistryDropsAMismatchedLabelList pins the shape check: a caller that
// passes the wrong number of labels is a programming error, and recording it
// under a half-built key would put a series in the registry that no reader
// could find.
func TestRegistryDropsAMismatchedLabelList(t *testing.T) {
	r := New()
	r.Count(CoordinatorCycleTotal)
	r.Count(CoordinatorCycleTotal, OutcomeSkipped, "extra")
	if got := len(r.Snapshot().Counters); got != 0 {
		t.Fatalf("a mismatched label list recorded %d counters, want none", got)
	}
}

// TestRegistryClampsAnUndeclaredLabelValue is the cardinality law in one case:
// a value that is not in the vocabulary — here a checkout id, which is exactly
// what must never become a series — lands in the other bucket.
func TestRegistryClampsAnUndeclaredLabelValue(t *testing.T) {
	r := New()
	r.Count(RequestServedTotal, "checkout-0193f1c2-9f7e-7c3a-8a1e-1b2c3d4e5f60")
	r.Count(RequestServedTotal, "/Users/someone/code/repo/.git/worktrees/feature")

	got := r.Snapshot().Counters
	if len(got) != 1 {
		t.Fatalf("two id-shaped values made %d series, want 1: %v", len(got), got)
	}
	if want := int64(2); got[RequestServedTotal+"{kind="+LabelOther+"}"] != want {
		t.Fatalf("other bucket = %v, want %d", got, want)
	}
}

func TestRemovalGraceIsALifecycleMetricState(t *testing.T) {
	r := New()
	r.Count(CheckoutTransitionTotal, StateReady, StateRemovalGrace, EvidenceAuthoritativeOmission)

	key := CheckoutTransitionTotal + "{from=checkout_ready,to=removal_grace,evidence=authoritative_omission}"
	got := r.Snapshot().Counters
	if got[key] != 1 {
		t.Fatalf("removal-grace transition = %v, want %q recorded once", got, key)
	}
	if got[CheckoutTransitionTotal+"{from=checkout_ready,to="+LabelOther+",evidence=authoritative_omission}"] != 0 {
		t.Fatalf("removal_grace collapsed into the other bucket: %v", got)
	}
}

func TestRegistryTracksGaugesBothWays(t *testing.T) {
	r := New()
	r.AddGauge(LeasesHeld, 3)
	r.AddGauge(LeasesHeld, -1)
	if got := r.Snapshot().Gauges[LeasesHeld]; got != 2 {
		t.Fatalf("leases held = %d, want 2", got)
	}
	r.SetGauge(LeasesHeld, 7)
	if got := r.Snapshot().Gauges[LeasesHeld]; got != 7 {
		t.Fatalf("leases held after set = %d, want 7", got)
	}
}

func TestRegistryAccumulatesDurations(t *testing.T) {
	r := New()
	r.Observe(CoordinatorBuildSeconds, 10*time.Millisecond, SlotCommit)
	r.Observe(CoordinatorBuildSeconds, 30*time.Millisecond, SlotCommit)
	// A negative observation is a clock going backwards, not a build.
	r.Observe(CoordinatorBuildSeconds, -time.Second, SlotCommit)

	stat := r.Snapshot().Durations[CoordinatorBuildSeconds+"{slot=commit}"]
	if stat.Count != 2 || stat.Total != 40*time.Millisecond || stat.Longest != 30*time.Millisecond {
		t.Fatalf("duration stat = %+v, want count 2 / total 40ms / longest 30ms", stat)
	}
}

// TestSnapshotFlatOmitsZeroSeries keeps the status block honest: a daemon that
// has served no view carries no view counters, so an empty map means "nothing
// happened" rather than "nothing is wired".
func TestSnapshotFlatOmitsZeroSeries(t *testing.T) {
	r := New()
	r.Count(RequestServedTotal, ViewBase)
	r.AddGauge(LeasesHeld, 1)
	r.AddGauge(LeasesHeld, -1)
	r.Observe(FamilyDiscoveryLagSeconds, 0)

	flat := r.Snapshot().Flat()
	if _, present := flat[LeasesHeld]; present {
		t.Fatalf("a gauge back at zero is in the flat view: %v", flat)
	}
	if flat[RequestServedTotal+"{kind=base}"] != 1 {
		t.Fatalf("served counter missing from the flat view: %v", flat)
	}
	if flat[FamilyDiscoveryLagSeconds+"|count"] != 1 {
		t.Fatalf("a zero-length observation is still an observation: %v", flat)
	}
}

func TestFallbackReasonCodeTakesTheLeadingToken(t *testing.T) {
	for reason, want := range map[string]string{
		"view_building": "view_building",
		"view_building: build 0193f1c2-9f7e-7c3a-8a1e-1b2c3d4e5f60 is producing the requested tree": "view_building",
		"checkout_inaccessible": "checkout_inaccessible",
		"":                      "",
	} {
		if got := FallbackReasonCode(reason); got != want {
			t.Errorf("FallbackReasonCode(%q) = %q, want %q", reason, got, want)
		}
	}
}

func TestProcessRegistryResets(t *testing.T) {
	Reset()
	Count(RequestServedTotal, ViewWorktree)
	if got := Read().Counters[RequestServedTotal+"{kind=worktree}"]; got != 1 {
		t.Fatalf("process counter = %d, want 1", got)
	}
	Reset()
	if got := len(Read().Counters); got != 0 {
		t.Fatalf("reset left %d counters", got)
	}
}
