package indexer

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// The fresh proof answers require_fresh on a route that already describes the
// working copy with one sample taken after the request arrived, instead of a
// refresh ticket (quiet window plus a settle cycle). Every test here pins one
// half of that claim: it proves exactly the settled case, and anything else
// is reported with a reason and left to the ticket path.

func setFreshProofSampled(t *testing.T, hook func()) {
	t.Helper()
	freshProofSampled = hook
	t.Cleanup(func() { freshProofSampled = nil })
}

func TestCheckoutFreshProofSettlesAPublishedRouteWithoutATicket(t *testing.T) {
	f, c, l := newCheckoutMutationFixture(t)
	route := f.route()
	dirty, found := f.generation(route.DirtyGenerationID)
	if !found {
		t.Fatalf("routed dirty generation %d is not in the catalog", route.DirtyGenerationID)
	}

	arrived := time.Now()
	proof, err := l.ProveCheckoutFresh(t.Context(), f.checkoutID, f.worktree)
	if err != nil {
		t.Fatalf("ProveCheckoutFresh: %v", err)
	}
	if !proof.Fresh || proof.Reason != "" {
		t.Fatalf("a settled route was not proven fresh: %+v", proof)
	}
	if proof.CommitGenerationID != route.CommitGenerationID || proof.DirtyGenerationID != route.DirtyGenerationID || proof.RouteEpoch != route.RouteEpoch {
		t.Fatalf("the proof names %d/%d@%d, the route is %d/%d@%d",
			proof.CommitGenerationID, proof.DirtyGenerationID, proof.RouteEpoch,
			route.CommitGenerationID, route.DirtyGenerationID, route.RouteEpoch)
	}
	if proof.Incarnation != "incarnation-worktree" {
		t.Fatalf("the proof names incarnation %q", proof.Incarnation)
	}
	// The whole claim: the sample that decided it was taken after the
	// request arrived, and it is the fingerprint the dirty generation was
	// built from.
	if proof.SampledAt.Before(arrived) {
		t.Fatalf("the deciding sample started %v before the request arrived", arrived.Sub(proof.SampledAt))
	}
	if proof.Sample <= 0 {
		t.Fatalf("the proof reports no sample duration: %+v", proof)
	}
	if proof.Fingerprint == "" || proof.Fingerprint != dirty.LowerViewFingerprint {
		t.Fatalf("sample fingerprint %q, dirty generation built from %q", proof.Fingerprint, dirty.LowerViewFingerprint)
	}
	// No ticket, no cycle, no publication.
	if through := c.checkoutRefreshHighWater(); through != 0 {
		t.Fatalf("the proof admitted a refresh ticket (high water %d)", through)
	}
	if after := f.route(); after != route {
		t.Fatalf("the proof moved the route: before=%+v after=%+v", route, after)
	}
}

func TestCheckoutFreshProofRefusesAChangedTreeAndTheTicketPathRecovers(t *testing.T) {
	f, c, l := newCheckoutMutationFixture(t)
	before := f.route()
	builderWriteFile(t, f.worktree, "helper.go", "package fixture\n\nfunc ProofHelper() {}\n")

	arrived := time.Now()
	proof, err := l.ProveCheckoutFresh(t.Context(), f.checkoutID, f.worktree)
	if err != nil {
		t.Fatalf("ProveCheckoutFresh: %v", err)
	}
	if proof.Fresh || proof.Reason != FreshProofSnapshotDiffers {
		t.Fatalf("an edited tree was not refused as snapshot_differs: %+v", proof)
	}
	if proof.SampledAt.Before(arrived) {
		t.Fatal("the refusing sample was taken before the request arrived")
	}
	dirty, _ := f.generation(before.DirtyGenerationID)
	if proof.Fingerprint == "" || proof.Fingerprint == dirty.LowerViewFingerprint {
		t.Fatalf("the sample fingerprint %q does not show the edit (route built from %q)", proof.Fingerprint, dirty.LowerViewFingerprint)
	}
	if after := f.route(); after != before {
		t.Fatal("a refused proof published something")
	}

	// The fallback the MCP wait takes: a ticket, which the coordinator's
	// cycle completes with a new dirty generation.
	ticket, err := l.RequestCheckoutRefresh(t.Context(), f.checkoutID, f.worktree)
	if err != nil {
		t.Fatalf("RequestCheckoutRefresh: %v", err)
	}
	c.cycle(t.Context())
	if result := awaitCheckoutRefresh(t, ticket); result.Err != nil || !result.Reindexed {
		t.Fatalf("the ticket path did not publish the edit: %+v", result)
	}
	published := f.route()
	if published.DirtyGenerationID == before.DirtyGenerationID {
		t.Fatal("the ticket completed without a new dirty generation")
	}

	again, err := l.ProveCheckoutFresh(t.Context(), f.checkoutID, f.worktree)
	if err != nil {
		t.Fatalf("ProveCheckoutFresh after publication: %v", err)
	}
	if !again.Fresh || again.DirtyGenerationID != published.DirtyGenerationID || again.Fingerprint != proof.Fingerprint {
		t.Fatalf("the published edit is not proven fresh: %+v (route %+v, edited fingerprint %q)", again, published, proof.Fingerprint)
	}
}

func TestCheckoutFreshProofRefusesARouteThatMovedDuringTheProof(t *testing.T) {
	f, _, l := newCheckoutMutationFixture(t)
	route := f.route()
	setFreshProofSampled(t, func() {
		// Same generations, next epoch: a route that moved away and back is
		// not the route the sample was compared with.
		if err := f.catalog.FlipCheckoutRoute(context.Background(), store_sqlite.FlipCheckoutRouteRequest{
			CheckoutID: f.checkoutID, GraphID: route.GraphID,
			CommitGenerationID: route.CommitGenerationID, DirtyGenerationID: route.DirtyGenerationID,
			State: route.State, ExpectedRouteEpoch: route.RouteEpoch,
		}); err != nil {
			t.Errorf("flip the route: %v", err)
		}
	})
	proof, err := l.ProveCheckoutFresh(t.Context(), f.checkoutID, f.worktree)
	if err != nil {
		t.Fatalf("ProveCheckoutFresh: %v", err)
	}
	if proof.Fresh || proof.Reason != FreshProofRouteMoved {
		t.Fatalf("a route that moved under the proof was not refused: %+v", proof)
	}
	if proof.RouteEpoch != route.RouteEpoch {
		t.Fatalf("the refusal names epoch %d, the proof read %d", proof.RouteEpoch, route.RouteEpoch)
	}
}

func TestCheckoutFreshProofRefusesAChangedCheckoutRow(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(t *testing.T, f *coordinatorFixture, row store_sqlite.Checkout)
		want   string
	}{
		{
			name: "replaced_incarnation",
			mutate: func(t *testing.T, f *coordinatorFixture, row store_sqlite.Checkout) {
				row.Incarnation = "incarnation-worktree-recreated"
				if err := f.catalog.UpsertCheckout(context.Background(), row); err != nil {
					t.Errorf("replace the incarnation: %v", err)
				}
			},
			want: FreshProofCheckoutChanged,
		},
		{
			name: "left_ready",
			mutate: func(t *testing.T, f *coordinatorFixture, row store_sqlite.Checkout) {
				if err := f.catalog.UpdateCheckoutState(context.Background(), store_sqlite.UpdateCheckoutStateRequest{
					CheckoutID: row.CheckoutID, Incarnation: row.Incarnation,
					State:       store_sqlite.CheckoutStateAvailabilityGrace,
					DesiredMode: row.DesiredMode, EffectiveMode: row.EffectiveMode,
				}); err != nil {
					t.Errorf("move the checkout out of ready: %v", err)
				}
			},
			want: FreshProofCheckoutNotReady,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, _, l := newCheckoutMutationFixture(t)
			row, found, err := f.catalog.GetCheckout(t.Context(), f.checkoutID)
			if err != nil || !found {
				t.Fatalf("read the checkout: found=%v err=%v", found, err)
			}
			setFreshProofSampled(t, func() { tc.mutate(t, f, row) })
			proof, err := l.ProveCheckoutFresh(t.Context(), f.checkoutID, f.worktree)
			if err != nil {
				t.Fatalf("ProveCheckoutFresh: %v", err)
			}
			if proof.Fresh || proof.Reason != tc.want {
				t.Fatalf("a checkout row changed under the proof was not refused as %s: %+v", tc.want, proof)
			}
		})
	}

	t.Run("not_ready_before_the_proof", func(t *testing.T) {
		f, _, l := newCheckoutMutationFixture(t)
		row, _, err := f.catalog.GetCheckout(t.Context(), f.checkoutID)
		if err != nil {
			t.Fatal(err)
		}
		if err := f.catalog.UpdateCheckoutState(t.Context(), store_sqlite.UpdateCheckoutStateRequest{
			CheckoutID: row.CheckoutID, Incarnation: row.Incarnation,
			State:       store_sqlite.CheckoutStateReconciling,
			DesiredMode: row.DesiredMode, EffectiveMode: row.EffectiveMode,
		}); err != nil {
			t.Fatal(err)
		}
		proof, err := l.ProveCheckoutFresh(t.Context(), f.checkoutID, f.worktree)
		if err != nil {
			t.Fatalf("ProveCheckoutFresh: %v", err)
		}
		if proof.Fresh || proof.Reason != FreshProofCheckoutNotReady || !proof.SampledAt.IsZero() {
			t.Fatalf("a checkout that is not ready was sampled or proven: %+v", proof)
		}
	})

	t.Run("other_root", func(t *testing.T) {
		f, _, l := newCheckoutMutationFixture(t)
		_, err := l.ProveCheckoutFresh(t.Context(), f.checkoutID, f.primary)
		if !errors.Is(err, ErrCheckoutMutationStale) {
			t.Fatalf("a proof against another root was not refused as stale: %v", err)
		}
	})
}

func TestCheckoutFreshProofRefusesAKnownStaleCohortWithoutSampling(t *testing.T) {
	f, c, l := newCheckoutMutationFixture(t)
	c.InvalidateDependencyCohort("test: a sibling repository was registered")
	proof, err := l.ProveCheckoutFresh(t.Context(), f.checkoutID, f.worktree)
	if err != nil {
		t.Fatalf("ProveCheckoutFresh: %v", err)
	}
	if proof.Fresh || proof.Reason != FreshProofCohortStale {
		t.Fatalf("a stale cohort was not refused: %+v", proof)
	}
	if !proof.SampledAt.IsZero() || proof.Sample != 0 {
		t.Fatalf("the proof sampled a checkout whose cohort it already knew was stale: %+v", proof)
	}
}

func TestCheckoutFreshProofWithoutACoordinatorDefersToTheTicket(t *testing.T) {
	f, _, _ := newCheckoutMutationFixture(t)
	l := &CheckoutLifecycle{catalog: f.catalog, store: f.store, coordinators: map[string]*CheckoutCoordinator{}}
	proof, err := l.ProveCheckoutFresh(t.Context(), f.checkoutID, f.worktree)
	if err != nil {
		t.Fatalf("ProveCheckoutFresh: %v", err)
	}
	if proof.Fresh || proof.Reason != FreshProofNoCoordinator {
		t.Fatalf("a checkout with no coordinator was proven: %+v", proof)
	}
}

// TestCheckoutFreshProofLatencyProbe measures the proof against the ticket it
// replaces on a settled route, with the coordinator's loop running at the
// product-default quiet window. It always asserts the proof is fresh and
// sub-second and logs both distributions; the tens-of-milliseconds bound and
// the proof-beats-ticket comparison apply only when
// GORTEX_FRESH_PROOF_LATENCY_GATE=1, because a loaded CI host can stretch one
// git status well past them.
func TestCheckoutFreshProofLatencyProbe(t *testing.T) {
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
	// Only a catastrophic regression fails the ordinary suite: the proof and
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

// A refused proof's fallback costs exactly one working-copy sample before
// admission: the proof's own, which was taken after the request arrived. The
// ticket admitted against it completes only through the cycle's post-admission
// verification, like any other ticket. The plain ticket path, for contrast,
// samples again.
func TestCheckoutFreshProofRefusalCostsOneSampleBeforeAdmission(t *testing.T) {
	f, c, l := newCheckoutMutationFixture(t)
	before := f.route()
	builderWriteFile(t, f.worktree, "helper.go", "package fixture\n\nfunc ReusedSampleHelper() {}\n")

	taken := c.sampler.SamplesTaken()
	proof, err := l.ProveCheckoutFresh(t.Context(), f.checkoutID, f.worktree)
	if err != nil {
		t.Fatalf("ProveCheckoutFresh: %v", err)
	}
	if proof.Fresh || proof.Reason != FreshProofSnapshotDiffers || !proof.ReusableSample() {
		t.Fatalf("an edited tree was not refused with a reusable sample: %+v", proof)
	}
	ticket, err := l.RequestCheckoutRefreshAfterProof(t.Context(), f.checkoutID, f.worktree, proof)
	if err != nil {
		t.Fatalf("RequestCheckoutRefreshAfterProof: %v", err)
	}
	if got := c.sampler.SamplesTaken() - taken; got != 1 {
		t.Fatalf("proof plus admission took %d working-copy samples, want exactly the proof's one", got)
	}
	c.refreshMu.Lock()
	admitted := c.refreshWaiters[ticket.Ticket.Generation]
	c.refreshMu.Unlock()
	if admitted == nil || admitted.fingerprint != proof.Fingerprint {
		t.Fatalf("the ticket was not admitted against the proof's sample: %+v (proof fingerprint %q)", admitted, proof.Fingerprint)
	}

	c.cycle(t.Context())
	if result := awaitCheckoutRefresh(t, ticket); result.Err != nil || !result.Reindexed {
		t.Fatalf("the ticket admitted after the proof did not publish: %+v", result)
	}
	published := f.route()
	if published.DirtyGenerationID == before.DirtyGenerationID {
		t.Fatal("the ticket completed without a new dirty generation")
	}
	again, err := l.ProveCheckoutFresh(t.Context(), f.checkoutID, f.worktree)
	if err != nil || !again.Fresh || again.Fingerprint != proof.Fingerprint {
		t.Fatalf("the published edit is not proven fresh at the proof's fingerprint: %+v err=%v", again, err)
	}

	// Contrast: the ordinary ticket path takes a sample of its own.
	builderWriteFile(t, f.worktree, "helper.go", "package fixture\n\nfunc ResampledHelper() {}\n")
	taken = c.sampler.SamplesTaken()
	plain, err := l.RequestCheckoutRefresh(t.Context(), f.checkoutID, f.worktree)
	if err != nil {
		t.Fatalf("RequestCheckoutRefresh: %v", err)
	}
	if got := c.sampler.SamplesTaken() - taken; got != 1 {
		t.Fatalf("a plain ticket took %d samples at admission, want 1", got)
	}
	c.cycle(t.Context())
	if result := awaitCheckoutRefresh(t, plain); result.Err != nil || !result.Reindexed {
		t.Fatalf("the plain ticket did not publish: %+v", result)
	}
}

// Only a snapshot_differs refusal of the same checkout lends its sample; any
// other proof admits an ordinary, freshly sampled ticket.
func TestCheckoutFreshProofOnlyASnapshotRefusalLendsItsSample(t *testing.T) {
	f, c, l := newCheckoutMutationFixture(t)
	settled, err := l.ProveCheckoutFresh(t.Context(), f.checkoutID, f.worktree)
	if err != nil || !settled.Fresh {
		t.Fatalf("a settled route was not proven fresh: %+v err=%v", settled, err)
	}
	if settled.ReusableSample() {
		t.Fatal("a fresh proof offers its sample for admission")
	}
	if (CheckoutFreshProof{Reason: FreshProofSnapshotDiffers}).ReusableSample() {
		t.Fatal("a proof carrying no sample claims to be reusable")
	}
	taken := c.sampler.SamplesTaken()
	ticket, err := l.RequestCheckoutRefreshAfterProof(t.Context(), f.checkoutID, f.worktree, settled)
	if err != nil {
		t.Fatalf("RequestCheckoutRefreshAfterProof: %v", err)
	}
	if got := c.sampler.SamplesTaken() - taken; got != 1 {
		t.Fatalf("a ticket after a non-refusing proof took %d samples, want its own 1", got)
	}
	c.cycle(t.Context())
	if result := awaitCheckoutRefresh(t, ticket); result.Err != nil || !result.Reindexed {
		t.Fatalf("the ticket did not settle: %+v", result)
	}
}

// A record handed to the admission on its context is bound, and marked
// ticket_enqueued, before the call returns — and so before the cycle the
// ticket wakes can mark it.
func TestCheckoutFreshProofFallbackBindsTheContextRecordAtAdmission(t *testing.T) {
	f, c, l := newCheckoutMutationFixture(t)
	builderWriteFile(t, f.worktree, "helper.go", "package fixture\n\nfunc BoundHelper() {}\n")
	proof, err := l.ProveCheckoutFresh(t.Context(), f.checkoutID, f.worktree)
	if err != nil || proof.Reason != FreshProofSnapshotDiffers {
		t.Fatalf("an edited tree was not refused: %+v err=%v", proof, err)
	}
	record := DefaultPublicationPhases().Begin(f.checkoutID, fmt.Sprintf("after-proof-%d", time.Now().UnixNano()), PublicationSourceFreshRequest, time.Now())
	ticket, err := l.RequestCheckoutRefreshAfterProof(WithPublicationRecord(t.Context(), record), f.checkoutID, f.worktree, proof)
	if err != nil {
		t.Fatalf("RequestCheckoutRefreshAfterProof: %v", err)
	}
	bound := record.Snapshot()
	if bound.Ticket != ticket.Ticket.Generation {
		t.Fatalf("the record is bound to ticket %d at admission, want %d", bound.Ticket, ticket.Ticket.Generation)
	}
	if _, ok := phaseOffsets(bound)[PublicationTicketEnqueued]; !ok {
		t.Fatalf("admission did not mark ticket_enqueued: %+v", bound.Phases)
	}
	c.cycle(t.Context())
	if result := awaitCheckoutRefresh(t, ticket); result.Err != nil || !result.Reindexed {
		t.Fatalf("the ticket did not publish: %+v", result)
	}
	offsets := phaseOffsets(record.Snapshot())
	for _, want := range []PublicationPhase{PublicationCycleStarted, PublicationAdmitted, PublicationPublished} {
		if _, ok := offsets[want]; !ok {
			t.Errorf("the served record lacks %s: %+v", want, record.Snapshot().Phases)
		}
	}
}

// Concurrent require_fresh requests of one checkout that all arrived before
// any sample began are answered by ONE working-copy sample: each proof accepts
// any sample whose git status began at or after its own request's arrival.
func TestCheckoutFreshProofConcurrentRequestsShareOneSampleBegunAfterEveryArrival(t *testing.T) {
	f, c, l := newCheckoutMutationFixture(t)
	arrived := time.Now()
	ctx := WithFreshRequestArrival(t.Context(), arrived)
	taken := c.sampler.SamplesTaken()
	const requests = 6
	proofs := make([]CheckoutFreshProof, requests)
	errs := make([]error, requests)
	start := make(chan struct{})
	done := make(chan int, requests)
	for i := 0; i < requests; i++ {
		go func() {
			<-start
			proofs[i], errs[i] = l.ProveCheckoutFresh(ctx, f.checkoutID, f.worktree)
			done <- i
		}()
	}
	close(start)
	for i := 0; i < requests; i++ {
		<-done
	}
	for i := range proofs {
		if errs[i] != nil || !proofs[i].Fresh {
			t.Fatalf("proof %d: %+v err=%v", i, proofs[i], errs[i])
		}
		if !proofs[i].Since.Equal(arrived) {
			t.Fatalf("proof %d was bounded by %v, the requests arrived at %v", i, proofs[i].Since, arrived)
		}
		if proofs[i].Fingerprint != proofs[0].Fingerprint {
			t.Fatalf("proof %d decided on fingerprint %q, proof 0 on %q", i, proofs[i].Fingerprint, proofs[0].Fingerprint)
		}
	}
	if got := c.sampler.SamplesTaken() - taken; got != 1 {
		t.Fatalf("%d concurrent proofs took %d working-copy samples, want 1 shared", requests, got)
	}
	if last := c.sampler.LastSampleStarted(); last.Before(arrived) {
		t.Fatalf("the shared sample began %v before the requests arrived", arrived.Sub(last))
	}
}

// A sample that began before a request arrived never answers it: a later
// request pays for its own sample, while a request that arrived before that
// sample began may reuse it.
func TestCheckoutFreshProofNeverSharesASampleBegunBeforeTheRequestArrived(t *testing.T) {
	f, c, l := newCheckoutMutationFixture(t)
	early := time.Now()
	first, err := l.ProveCheckoutFresh(WithFreshRequestArrival(t.Context(), early), f.checkoutID, f.worktree)
	if err != nil || !first.Fresh {
		t.Fatalf("first proof: %+v err=%v", first, err)
	}
	began := c.sampler.LastSampleStarted()
	if began.Before(early) {
		t.Fatalf("the first sample began %v before its request arrived", early.Sub(began))
	}

	// Arrived after that sample began: it must take a new one, and the tree
	// it sees is the edited one.
	builderWriteFile(t, f.worktree, "helper.go", "package fixture\n\nfunc ArrivedLaterHelper() {}\n")
	late := time.Now()
	taken := c.sampler.SamplesTaken()
	second, err := l.ProveCheckoutFresh(WithFreshRequestArrival(t.Context(), late), f.checkoutID, f.worktree)
	if err != nil {
		t.Fatalf("second proof: %v", err)
	}
	if got := c.sampler.SamplesTaken() - taken; got != 1 {
		t.Fatalf("a request that arrived after the last sample began took %d samples, want its own 1", got)
	}
	if second.Fresh || second.Reason != FreshProofSnapshotDiffers || second.Fingerprint == first.Fingerprint {
		t.Fatalf("the later request was answered by the earlier sample: first=%+v second=%+v", first, second)
	}
	if last := c.sampler.LastSampleStarted(); last.Before(late) {
		t.Fatalf("the later request's sample began %v before it arrived", late.Sub(last))
	}

	// Arrived before the second sample began (at `early`): the second sample
	// answers it, at no cost.
	taken = c.sampler.SamplesTaken()
	third, err := l.ProveCheckoutFresh(WithFreshRequestArrival(t.Context(), early), f.checkoutID, f.worktree)
	if err != nil {
		t.Fatalf("third proof: %v", err)
	}
	if got := c.sampler.SamplesTaken() - taken; got != 0 {
		t.Fatalf("a request that arrived before the latest sample began took %d new samples, want 0", got)
	}
	if third.Fingerprint != second.Fingerprint {
		t.Fatalf("the reused sample is not the latest: %q vs %q", third.Fingerprint, second.Fingerprint)
	}

	// Without a recorded arrival the proof is bounded by its own start, so it
	// samples again.
	taken = c.sampler.SamplesTaken()
	if _, err := l.ProveCheckoutFresh(t.Context(), f.checkoutID, f.worktree); err != nil {
		t.Fatalf("unbounded proof: %v", err)
	}
	if got := c.sampler.SamplesTaken() - taken; got != 1 {
		t.Fatalf("a proof with no recorded arrival took %d samples, want 1", got)
	}
}

// A proof refused before it sampled (here: a route_not_active refusal, as
// when an edit's own build is in flight) lends no sample; with the request's
// arrival on the context the fallback ticket is admitted against any sample
// begun after that arrival — shared, not a new one — and still publishes
// only through the cycle's own verification. Without an arrival it samples.
func TestCheckoutFreshProofSamplelessRefusalAdmitsAgainstASampleSinceArrival(t *testing.T) {
	f, c, l := newCheckoutMutationFixture(t)
	before := f.route()
	builderWriteFile(t, f.worktree, "helper.go", "package fixture\n\nfunc SharedAdmissionHelper() {}\n")
	arrived := time.Now()
	ctx := WithFreshRequestArrival(t.Context(), arrived)
	// A concurrent request of the same checkout sampled after the arrival.
	sibling, err := l.ProveCheckoutFresh(ctx, f.checkoutID, f.worktree)
	if err != nil || sibling.Reason != FreshProofSnapshotDiffers {
		t.Fatalf("sibling proof: %+v err=%v", sibling, err)
	}
	sampleless := CheckoutFreshProof{Reason: FreshProofRouteNotActive}
	if sampleless.ReusableSample() {
		t.Fatal("a sample-less refusal claims a reusable sample")
	}
	taken := c.sampler.SamplesTaken()
	ticket, err := l.RequestCheckoutRefreshAfterProof(ctx, f.checkoutID, f.worktree, sampleless)
	if err != nil {
		t.Fatalf("RequestCheckoutRefreshAfterProof: %v", err)
	}
	if got := c.sampler.SamplesTaken() - taken; got != 0 {
		t.Fatalf("admission after a sample-less refusal took %d samples, want the shared post-arrival one", got)
	}
	c.refreshMu.Lock()
	admitted := c.refreshWaiters[ticket.Ticket.Generation]
	c.refreshMu.Unlock()
	if admitted == nil || admitted.fingerprint != sibling.Fingerprint {
		t.Fatalf("the ticket was not admitted against the shared sample: %+v (want fingerprint %q)", admitted, sibling.Fingerprint)
	}
	c.cycle(t.Context())
	if result := awaitCheckoutRefresh(t, ticket); result.Err != nil || !result.Reindexed {
		t.Fatalf("the ticket did not publish: %+v", result)
	}
	if after := f.route(); after.DirtyGenerationID == before.DirtyGenerationID {
		t.Fatal("the ticket completed without publishing the edit")
	}

	// No arrival: the ordinary path samples for itself.
	builderWriteFile(t, f.worktree, "helper.go", "package fixture\n\nfunc UnsharedAdmissionHelper() {}\n")
	taken = c.sampler.SamplesTaken()
	plain, err := l.RequestCheckoutRefreshAfterProof(t.Context(), f.checkoutID, f.worktree, sampleless)
	if err != nil {
		t.Fatalf("RequestCheckoutRefreshAfterProof without arrival: %v", err)
	}
	if got := c.sampler.SamplesTaken() - taken; got != 1 {
		t.Fatalf("admission without an arrival took %d samples, want 1", got)
	}
	c.cycle(t.Context())
	if result := awaitCheckoutRefresh(t, plain); result.Err != nil || !result.Reindexed {
		t.Fatalf("the plain ticket did not publish: %+v", result)
	}
}
