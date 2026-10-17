//go:build performance

package indexer

import (
	"os"
	"slices"
	"testing"
	"time"
)

// TestPerformanceCheckoutFreshProofLatencyProbe measures the proof against the ticket it
// replaces on a settled route, with the coordinator's loop running at the
// product-default quiet window. It always asserts the proof is fresh and
// sub-second and logs both distributions; the tens-of-milliseconds bound and
// the proof-beats-ticket comparison apply only when
// GORTEX_FRESH_PROOF_LATENCY_GATE=1, because a loaded CI host can stretch one
// git status well past them.
func TestPerformanceCheckoutFreshProofLatencyProbe(t *testing.T) {
	f := newCoordinatorFixture(t)
	gate := NewViewBuildGate()
	gate.Open()
	c := f.coordinator(t, CheckoutCoordinatorConfig{Gate: gate, Debounce: 150 * time.Millisecond})
	if out := c.reconcile(t.Context()); out.Err != nil || out.DirtyGenerationID == 0 {
		t.Fatalf("initial reconcile: %+v", out)
	}
	l := &CheckoutLifecycle{catalog: f.catalog, store: f.store, coordinators: map[string]*CheckoutCoordinator{f.checkoutID: c}}

	const rounds = 15
	proofs := make([]time.Duration, 0, rounds)
	samples := make([]time.Duration, 0, rounds)
	for range rounds {
		started := time.Now()
		proof, err := l.ProveCheckoutFresh(t.Context(), f.checkoutID, f.worktree)
		elapsed := time.Since(started)
		if err != nil || !proof.Fresh {
			t.Fatalf("a settled route was not proven fresh: %+v err=%v", proof, err)
		}
		proofs = append(proofs, elapsed)
		samples = append(samples, proof.Sample)
	}
	const ticketRounds = 5
	tickets := make([]time.Duration, 0, ticketRounds)
	for range ticketRounds {
		started := time.Now()
		ticket, err := l.RequestCheckoutRefresh(t.Context(), f.checkoutID, f.worktree)
		if err != nil {
			t.Fatalf("RequestCheckoutRefresh: %v", err)
		}
		if result := awaitCheckoutRefresh(t, ticket); result.Err != nil || !result.Reindexed {
			t.Fatalf("the ticket on a settled route failed: %+v", result)
		}
		tickets = append(tickets, time.Since(started))
	}
	proofP50, proofMax := freshProofPercentile(proofs, 50), slices.Max(proofs)
	sampleP50 := freshProofPercentile(samples, 50)
	ticketP50, ticketMax := freshProofPercentile(tickets, 50), slices.Max(tickets)
	t.Logf("fresh proof on a settled route: p50=%v max=%v (sample p50=%v, n=%d); ticket: p50=%v max=%v (n=%d)",
		proofP50, proofMax, sampleP50, rounds, ticketP50, ticketMax, ticketRounds)
	// A catastrophic regression fails this explicit comparison: the proof and
	// the ticket both scale with one git status, and a loaded host moves both.
	if proofP50 >= time.Second {
		t.Fatalf("the proof's p50 %v is not sub-second", proofP50)
	}
	if os.Getenv("GORTEX_FRESH_PROOF_LATENCY_GATE") != "1" {
		return
	}
	if proofP50 >= 100*time.Millisecond {
		t.Fatalf("the proof's p50 %v is not in the tens of milliseconds", proofP50)
	}
	if proofP50 >= ticketP50 {
		t.Fatalf("the proof (p50 %v) is not cheaper than the ticket it replaces (p50 %v)", proofP50, ticketP50)
	}
}

func freshProofPercentile(values []time.Duration, p int) time.Duration {
	sorted := slices.Clone(values)
	slices.Sort(sorted)
	index := (len(sorted)*p + 99) / 100
	if index > 0 {
		index--
	}
	return sorted[index]
}
