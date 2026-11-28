package main

import (
	"strings"
	"testing"
)

// Compact text has no freshness field, so the report must never turn its
// stale-claimed-fresh count into a measured number, zero included. This holds
// for every selection that reports the compact column: a compact run, the
// gate rotation and a run in another mode. Counts that would read as a
// measured value in an observable mode are fed to the compact column too, so
// only the unobservable rule keeps it unavailable.
func TestEditProbeCompactStaleClaimedFreshIsReportedUnobservable(t *testing.T) {
	const reason = "mode cannot carry a freshness claim here"
	counts := func() map[string]editProbeModeCounts {
		gate := func() *editProbeGateCounts {
			return &editProbeGateCounts{answers: 5, claimsFresh: 3, stale: 1, edits: map[int]bool{1: true}, tools: map[string]*editProbeGateToolCounts{}}
		}
		return map[string]editProbeModeCounts{
			"compact": {answers: 5, claimsFresh: 3, stale: 1, gate: gate()},
			"json":    {answers: 5, claimsFresh: 3, stale: 1, gate: gate()},
		}
	}
	for _, selected := range []string{"compact", "gate", "json"} {
		t.Run(selected, func(t *testing.T) {
			coverage := editProbeOutputCoverage(selected, counts())
			compact, ok := coverage["compact"]
			if !ok {
				t.Fatalf("compact column missing: %+v", coverage)
			}
			if compact.ClaimsFreshObservable || !strings.Contains(compact.Reason, "no freshness field") {
				t.Fatalf("compact reported as able to carry a freshness claim: %+v", compact)
			}
			if compact.StaleClaimedFresh.Value != nil || compact.StaleClaimedFresh.Reason != reason {
				t.Fatalf("compact stale-claimed-fresh is not reported unobservable: %+v", compact.StaleClaimedFresh)
			}
			// Control: the same counts are measured in an observable mode.
			json := coverage["json"]
			if !json.ClaimsFreshObservable || json.StaleClaimedFresh.Value == nil || *json.StaleClaimedFresh.Value != 1 {
				t.Fatalf("observable JSON column is not measured from the same counts: %+v", json)
			}
		})
	}
}
