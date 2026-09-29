package indexer

import (
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// The source-edit path through a checkout coordinator, counted in working-copy
// samples and coordinator cycles: what an edit lease (Begin, Prepare, disk
// write, EnqueueRefresh, Close) costs before its generation is published, and
// what require_fresh requests that arrive while that build is in flight cost.

// mutationEditSamples is where one lease edit's working-copy samples went.
type mutationEditSamples struct {
	Begin, Prepare, Enqueue, Cycle uint64
}

func (s mutationEditSamples) total() uint64 { return s.Begin + s.Prepare + s.Enqueue + s.Cycle }

// afterWrite is what the edit sampled once its bytes were on disk.
func (s mutationEditSamples) afterWrite() uint64 { return s.Enqueue + s.Cycle }

func TestCheckoutMutationEditSamplesTheWorkingCopyOnceAfterTheWrite(t *testing.T) {
	f, c, l := newCheckoutMutationFixture(t)
	ctx := t.Context()
	taken := c.sampler.SamplesTaken
	var got mutationEditSamples

	mark := taken()
	m, err := l.BeginCheckoutMutation(ctx, f.checkoutID, f.worktree, f.route().RouteEpoch)
	if err != nil {
		t.Fatal(err)
	}
	got.Begin, mark = taken()-mark, taken()
	if err := m.Prepare(ctx); err != nil {
		m.Close()
		t.Fatal(err)
	}
	got.Prepare, mark = taken()-mark, taken()
	builderWriteFile(t, f.worktree, "helper.go", chainHelperEdit)
	written := time.Now()
	ticket, err := m.EnqueueRefresh(ctx, filepath.Join(f.worktree, "helper.go"))
	m.Close()
	if err != nil {
		t.Fatal(err)
	}
	got.Enqueue, mark = taken()-mark, taken()

	cycles := 0
	c.cycleDone = func(CheckoutCycle) { cycles++ }
	c.cycle(ctx)
	result := awaitCheckoutRefresh(t, ticket)
	got.Cycle = taken() - mark
	published := time.Since(written)
	t.Logf("lease edit samples: begin=%d prepare=%d enqueue=%d cycle=%d total=%d after_write=%d cycles=%d write->ticket_completed=%v",
		got.Begin, got.Prepare, got.Enqueue, got.Cycle, got.total(), got.afterWrite(), cycles, published)

	if result.Err != nil || !result.Reindexed || int64(result.AppliedGeneration) != f.route().DirtyGenerationID {
		t.Fatalf("the edit's ticket = %+v, want the routed generation %d", result, f.route().DirtyGenerationID)
	}
	row, _ := f.generation(int64(result.AppliedGeneration))
	after, err := c.sampler.Sample(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if row.LowerViewFingerprint != after.Fingerprint {
		t.Fatalf("the published generation describes %s, the working copy %s", row.LowerViewFingerprint, after.Fingerprint)
	}
	// Admission samples nothing; Prepare validates the routed snapshot once,
	// right before the write (a stale tree, or an external edit after
	// admission, is refused there, TestCheckoutMutationRejectsWrongRootEpochAndExternalChanges);
	// the ticket's capture sample binds the exact post-commit snapshot and is
	// the serving cycle's decision sample, and the ticket's completion sample,
	// too. The build's pre-publish fence confirms its read set against that
	// sample without taking another (confirmDirtyBuildInputs).
	if got.Begin != 0 || got.Prepare != 1 || got.Enqueue != 1 || got.Cycle != 0 || cycles != 1 {
		t.Fatalf("lease edit samples = %+v in %d cycles, want begin=0 prepare=1 enqueue=1 cycle=0 in one cycle", got, cycles)
	}
	if before := got.Begin + got.Prepare; before != 1 {
		t.Fatalf("the edit sampled %d times before its write, want 1 (Prepare's)", before)
	}
	if got.afterWrite() != 1 {
		t.Fatalf("the edit sampled %d times after its write, want 1 (the capture sample the cycle shares; the pre-publish fence is the read set)", got.afterWrite())
	}
}

func TestFreshRequestsDuringAnEditBuildRideItsPublication(t *testing.T) {
	f, c, l := newCheckoutMutationFixture(t)
	ctx := t.Context()
	m, err := l.BeginCheckoutMutation(ctx, f.checkoutID, f.worktree, f.route().RouteEpoch)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Prepare(ctx); err != nil {
		m.Close()
		t.Fatal(err)
	}
	builderWriteFile(t, f.worktree, "helper.go", chainHelperEdit)
	edit, err := m.EnqueueRefresh(ctx, filepath.Join(f.worktree, "helper.go"))
	m.Close()
	if err != nil {
		t.Fatal(err)
	}

	// Hold the edit's build after its payload is written, before the
	// pre-publish fence re-samples the working copy.
	inBuild, release := make(chan struct{}), make(chan struct{})
	var armed atomic.Bool
	armed.Store(true)
	c.dirtyBarrier = func() {
		if armed.CompareAndSwap(true, false) {
			close(inBuild)
			<-release
		}
	}
	var cycles atomic.Int32
	c.cycleDone = func(CheckoutCycle) { cycles.Add(1) }
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		c.cycle(ctx)
	}()
	select {
	case <-inBuild:
	case <-time.After(30 * time.Second):
		t.Fatal("the edit's build never reached its payload")
	}

	// Six require_fresh requests arrive while the build is in flight: the
	// route is withdrawn, so each proof refuses (route_not_active) and each
	// request admits a ticket.
	const requests = 6
	before := c.sampler.SamplesTaken()
	arrival := time.Now()
	tickets := make([]*CheckoutRefreshTicket, requests)
	errs := make([]error, requests)
	reasons := make([]string, requests)
	records := make([]*PublicationPhaseRecord, requests)
	var wg sync.WaitGroup
	for i := range requests {
		records[i] = DefaultPublicationPhases().Begin(f.checkoutID, fmt.Sprintf("rider-%d-%d", arrival.UnixNano(), i), "fresh_request", arrival)
		wg.Add(1)
		go func() {
			defer wg.Done()
			rctx := WithPublicationRecord(WithFreshRequestArrival(ctx, arrival), records[i])
			proof, err := l.ProveCheckoutFresh(rctx, f.checkoutID, f.worktree)
			if err != nil {
				errs[i] = err
				return
			}
			reasons[i] = proof.Reason
			tickets[i], errs[i] = l.RequestCheckoutRefreshAfterProof(rctx, f.checkoutID, f.worktree, proof)
		}()
	}
	wg.Wait()
	admission := c.sampler.SamplesTaken() - before
	for i := range requests {
		if errs[i] != nil || tickets[i] == nil {
			t.Fatalf("request %d: proof %q, ticket %v, err %v", i, reasons[i], tickets[i], errs[i])
		}
		if reasons[i] != FreshProofRouteNotActive {
			t.Fatalf("request %d proof = %q, want %q while the edit's build is in flight", i, reasons[i], FreshProofRouteNotActive)
		}
	}
	close(release)
	select {
	case <-finished:
	case <-time.After(30 * time.Second):
		t.Fatal("the edit's cycle never finished")
	}

	editResult := awaitCheckoutRefresh(t, edit)
	if editResult.Err != nil || !editResult.Reindexed {
		t.Fatalf("the edit's ticket = %+v", editResult)
	}
	firstCycle := 0
	for _, ticket := range tickets {
		select {
		case result := <-ticket.Ticket.Done:
			if result.Err != nil || result.AppliedGeneration != editResult.AppliedGeneration {
				t.Fatalf("a riding request = %+v, want the edit's generation %d", result, editResult.AppliedGeneration)
			}
			firstCycle++
		default:
		}
	}
	// Whatever the build's own cycle left owed is served the old way: one
	// more cycle (measured, so a regression reports what it costs).
	if firstCycle < requests {
		c.cycle(ctx)
		for _, ticket := range tickets {
			select {
			case result, ok := <-ticket.Ticket.Done:
				if ok && result.Err != nil {
					t.Fatalf("a request completed by a second cycle = %+v", result)
				}
			case <-time.After(10 * time.Second):
			}
		}
	}
	samples := c.sampler.SamplesTaken() - before
	t.Logf("%d require_fresh requests during the build: admission samples=%d, samples until all answered=%d (the fence included), completed by the build's cycle=%d/%d, cycles=%d",
		requests, admission, samples, firstCycle, requests, cycles.Load())

	if admission != 0 {
		t.Fatalf("admitting the requests during the build took %d samples, want 0", admission)
	}
	if firstCycle != requests || cycles.Load() != 1 {
		t.Fatalf("the build's cycle completed %d of %d requests in %d cycles, want all of them in one", firstCycle, requests, cycles.Load())
	}
	if samples != 1 {
		t.Fatalf("the requests cost %d samples beyond the build, want only the build's own pre-publish fence (1)", samples)
	}
	// Each rider's record reads: enqueued, admitted (it joined a build past
	// its lane admission, so no wait of its own), completed — in that order.
	for i, record := range records {
		offsets := phaseOffsets(record.Snapshot())
		enqueued, okE := offsets[PublicationTicketEnqueued]
		admitted, okA := offsets[PublicationAdmitted]
		completed, okC := offsets[PublicationTicketCompleted]
		if !okE || !okA || !okC || admitted < enqueued || completed < admitted {
			t.Fatalf("rider %d record phases = %+v, want ticket_enqueued <= admitted <= ticket_completed", i, record.Snapshot().Phases)
		}
	}
}

func TestCheckoutMutationRefreshReportsTheLaneHolderItWaitedFor(t *testing.T) {
	f, c, l := newCheckoutMutationFixture(t)
	ctx := t.Context()
	m, err := l.BeginCheckoutMutation(ctx, f.checkoutID, f.worktree, f.route().RouteEpoch)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if err := m.Prepare(ctx); err != nil {
		t.Fatal(err)
	}
	builderWriteFile(t, f.worktree, "helper.go", chainHelperEdit)

	// Another checkout's build holds the lane when the lease queues for it.
	release, err := c.gate.Acquire(ctx, ViewBuildBackground)
	if err != nil {
		t.Fatal(err)
	}
	withdraw := c.gate.NoteHolder(ViewBuildLaneHolder{Kind: "checkout_cycle", CheckoutID: "another-checkout", Generation: 42})
	const held = 150 * time.Millisecond
	go func() {
		time.Sleep(held)
		withdraw()
		release()
	}()
	out, err := m.Refresh(ctx)
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	holder := out.Admission.LaneHeldBy
	t.Logf("the lease waited %v for the lane, held by %+v", out.Admission.Lane, holder)
	if out.Admission.Lane < held/2 || holder == nil || holder.Kind != "checkout_cycle" || holder.CheckoutID != "another-checkout" || holder.Generation != 42 {
		t.Fatalf("the lease's admission = %+v (holder %+v), want a lane wait attributed to another checkout's cycle", out.Admission, holder)
	}
}
