package indexer

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"testing"
	"time"
)

func mutationFenceLifecycle() (*CheckoutLifecycle, *checkoutMutationFences) {
	fences := newCheckoutMutationFences()
	return &CheckoutLifecycle{mutationFences: fences}, fences
}

// checkout preserves the existing test-only raw-gate seam used to model a
// mutation reader without constructing a catalog/coordinator fixture. Product
// acquisitions use lease/leaseMany so every semaphore waiter is reference
// counted before it can race retirement.
func (f *checkoutMutationFences) checkout(checkoutID string) *checkoutMutationGate {
	f.mu.Lock()
	defer f.mu.Unlock()
	gate := f.checkouts[checkoutID]
	if gate == nil {
		gate = newCheckoutMutationGate()
		f.checkouts[checkoutID] = gate
	}
	return gate
}

func waitForCheckoutGateState(
	t *testing.T,
	fences *checkoutMutationFences,
	checkoutID string,
	wantRefs uint64,
	wantRetiring bool,
) *checkoutMutationGate {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		fences.mu.Lock()
		gate := fences.checkouts[checkoutID]
		matches := gate != nil && gate.refs == wantRefs && gate.retiring == wantRetiring
		fences.mu.Unlock()
		if matches {
			return gate
		}
		if time.Now().After(deadline) {
			t.Fatalf(
				"checkout gate %q did not reach refs=%d retiring=%v",
				checkoutID,
				wantRefs,
				wantRetiring,
			)
		}
		runtime.Gosched()
	}
}

func TestCheckoutMutationFenceRetirementWaitsForPreAcquireLease(t *testing.T) {
	lifecycle, fences := mutationFenceLifecycle()
	const checkoutID = "checkout-pre-acquire"

	// Resolving the registry entry is deliberately separate from acquiring the
	// semaphore. Retirement must count this interval or it can install a fresh
	// gate while the waiter later acquires the old one.
	lease, err := fences.lease(context.Background(), fences.checkouts, checkoutID)
	if err != nil {
		t.Fatalf("lease checkout gate: %v", err)
	}
	oldGate := lease.gate

	retired := make(chan error, 1)
	go func() {
		reclaimed, retireErr := lifecycle.retireCheckoutMutationFence(
			context.Background(), checkoutID, nil,
		)
		if retireErr == nil && !reclaimed {
			retireErr = errors.New("retirement unexpectedly reactivated")
		}
		retired <- retireErr
	}()
	waitForCheckoutGateState(t, fences, checkoutID, 1, true)

	blockedCtx, cancelBlocked := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancelBlocked()
	if token, acquireErr := lifecycle.AcquireCheckoutTopology(blockedCtx, checkoutID); !errors.Is(acquireErr, context.DeadlineExceeded) {
		if token != nil {
			token.Release()
		}
		t.Fatalf("acquisition during retirement error = %v, want deadline", acquireErr)
	}

	select {
	case err := <-retired:
		t.Fatalf("retirement passed a pre-acquire lease: %v", err)
	default:
	}
	lease.releaseReference()
	if err := <-retired; err != nil {
		t.Fatalf("retire checkout gate: %v", err)
	}

	token, err := lifecycle.AcquireCheckoutTopology(context.Background(), checkoutID)
	if err != nil {
		t.Fatalf("acquire recreated checkout gate: %v", err)
	}
	if token.gates[0].gate == oldGate {
		t.Fatal("retired checkout gate was reused instead of recreated")
	}
	token.Release()
}

func TestCheckoutMutationFenceRetirementWaitsForOutstandingAndQueuedTokens(t *testing.T) {
	lifecycle, fences := mutationFenceLifecycle()
	const checkoutID = "checkout-queued"

	first, err := lifecycle.AcquireCheckoutTopology(context.Background(), checkoutID)
	if err != nil {
		t.Fatalf("acquire first topology token: %v", err)
	}
	oldGate := first.gates[0].gate

	queuedResult := make(chan *CheckoutTopologyToken, 1)
	queuedErr := make(chan error, 1)
	go func() {
		token, acquireErr := lifecycle.AcquireCheckoutTopology(context.Background(), checkoutID)
		if acquireErr != nil {
			queuedErr <- acquireErr
			return
		}
		queuedResult <- token
	}()
	// The queued acquisition already owns its registry reference even though
	// the first token still holds the exclusive semaphore.
	waitForCheckoutGateState(t, fences, checkoutID, 2, false)

	retired := make(chan error, 1)
	go func() {
		reclaimed, retireErr := lifecycle.retireCheckoutMutationFence(
			context.Background(), checkoutID, nil,
		)
		if retireErr == nil && !reclaimed {
			retireErr = errors.New("retirement unexpectedly reactivated")
		}
		retired <- retireErr
	}()
	waitForCheckoutGateState(t, fences, checkoutID, 2, true)

	first.Release()
	var queued *CheckoutTopologyToken
	select {
	case queued = <-queuedResult:
	case err := <-queuedErr:
		t.Fatalf("queued acquisition failed: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("queued acquisition did not resume")
	}
	if queued.gates[0].gate != oldGate {
		t.Fatal("pre-retirement waiter did not remain on its leased gate")
	}
	select {
	case err := <-retired:
		t.Fatalf("retirement passed a queued topology token: %v", err)
	default:
	}

	queued.Release()
	if err := <-retired; err != nil {
		t.Fatalf("retire queued checkout gate: %v", err)
	}
}

func TestCheckoutMutationFenceRetirementGuardLossReactivatesGate(t *testing.T) {
	lifecycle, fences := mutationFenceLifecycle()
	const checkoutID = "checkout-guard-loss"

	held, err := lifecycle.AcquireCheckoutTopology(context.Background(), checkoutID)
	if err != nil {
		t.Fatalf("acquire held topology token: %v", err)
	}
	oldGate := held.gates[0].gate

	retired := make(chan struct {
		reclaimed bool
		err       error
	}, 1)
	go func() {
		reclaimed, retireErr := lifecycle.retireCheckoutMutationFence(
			context.Background(),
			checkoutID,
			func(context.Context) (bool, error) { return false, nil },
		)
		retired <- struct {
			reclaimed bool
			err       error
		}{reclaimed: reclaimed, err: retireErr}
	}()
	waitForCheckoutGateState(t, fences, checkoutID, 1, true)
	held.Release()

	result := <-retired
	if result.err != nil {
		t.Fatalf("guarded retirement: %v", result.err)
	}
	if result.reclaimed {
		t.Fatal("guard loss reclaimed the checkout gate")
	}

	reused, err := lifecycle.AcquireCheckoutTopology(context.Background(), checkoutID)
	if err != nil {
		t.Fatalf("acquire reactivated checkout gate: %v", err)
	}
	if reused.gates[0].gate != oldGate {
		t.Fatal("guard loss replaced rather than reactivated the checkout gate")
	}
	reused.Release()
}

func TestCheckoutMutationFenceRetirementCancellationReactivatesGate(t *testing.T) {
	lifecycle, fences := mutationFenceLifecycle()
	const checkoutID = "checkout-cancelled-retirement"

	held, err := lifecycle.AcquireCheckoutTopology(context.Background(), checkoutID)
	if err != nil {
		t.Fatalf("acquire held topology token: %v", err)
	}
	oldGate := held.gates[0].gate

	retireCtx, cancelRetirement := context.WithCancel(context.Background())
	retired := make(chan error, 1)
	go func() {
		_, retireErr := lifecycle.retireCheckoutMutationFence(retireCtx, checkoutID, nil)
		retired <- retireErr
	}()
	waitForCheckoutGateState(t, fences, checkoutID, 1, true)
	cancelRetirement()
	if err := <-retired; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled retirement error = %v, want context.Canceled", err)
	}

	held.Release()
	reused, err := lifecycle.AcquireCheckoutTopology(context.Background(), checkoutID)
	if err != nil {
		t.Fatalf("acquire gate after cancelled retirement: %v", err)
	}
	if reused.gates[0].gate != oldGate {
		t.Fatal("cancelled retirement replaced rather than reactivated the checkout gate")
	}
	reused.Release()
}

func TestMutationFenceRetirementCoversEveryRegistry(t *testing.T) {
	lifecycle, fences := mutationFenceLifecycle()
	ctx := context.Background()

	checkout, err := lifecycle.AcquireCheckoutTopology(ctx, "checkout-registry")
	if err != nil {
		t.Fatalf("acquire checkout topology: %v", err)
	}
	checkout.Release()
	if reclaimed, retireErr := lifecycle.retireCheckoutMutationFence(
		ctx, "checkout-registry", nil,
	); retireErr != nil || !reclaimed {
		t.Fatalf("retire checkout registry: reclaimed=%v err=%v", reclaimed, retireErr)
	}

	graph, err := lifecycle.AcquireCheckoutGraphTopology(ctx, "graph-registry")
	if err != nil {
		t.Fatalf("acquire graph topology: %v", err)
	}
	graph.Release()
	if reclaimed, retireErr := lifecycle.retireGraphMutationFence(
		ctx, "graph-registry", nil,
	); retireErr != nil || !reclaimed {
		t.Fatalf("retire graph registry: reclaimed=%v err=%v", reclaimed, retireErr)
	}

	family, err := lifecycle.AcquireCheckoutFamilyTopology(ctx, "family-registry")
	if err != nil {
		t.Fatalf("acquire family topology: %v", err)
	}
	family.Release()
	if reclaimed, retireErr := lifecycle.retireFamilyMutationFence(
		ctx, "family-registry", nil,
	); retireErr != nil || !reclaimed {
		t.Fatalf("retire family registry: reclaimed=%v err=%v", reclaimed, retireErr)
	}

	fences.mu.Lock()
	defer fences.mu.Unlock()
	if len(fences.checkouts) != 0 || len(fences.graphs) != 0 || len(fences.families) != 0 {
		t.Fatalf(
			"retired registry sizes = checkouts:%d graphs:%d families:%d, want all zero",
			len(fences.checkouts),
			len(fences.graphs),
			len(fences.families),
		)
	}
}

func TestMutationFenceRetirementReclaimsTenThousandIDs(t *testing.T) {
	lifecycle, fences := mutationFenceLifecycle()
	ctx := context.Background()
	const gateCount = 10_000
	for i := 0; i < gateCount; i++ {
		checkoutID := fmt.Sprintf("checkout-churn-%05d", i)
		token, err := lifecycle.AcquireCheckoutTopology(ctx, checkoutID)
		if err != nil {
			t.Fatalf("acquire checkout %d: %v", i, err)
		}
		token.Release()
		reclaimed, err := lifecycle.retireCheckoutMutationFence(ctx, checkoutID, nil)
		if err != nil || !reclaimed {
			t.Fatalf("retire checkout %d: reclaimed=%v err=%v", i, reclaimed, err)
		}
	}

	fences.mu.Lock()
	remaining := len(fences.checkouts)
	fences.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("checkout registry retained %d entries after churn", remaining)
	}
}

func TestMutationFenceRetirementConcurrentAcquireAndRecreate(t *testing.T) {
	lifecycle, fences := mutationFenceLifecycle()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	const (
		workerCount = 8
		iterations  = 200
		keyCount    = 8
	)
	start := make(chan struct{})
	errs := make(chan error, workerCount)
	var wg sync.WaitGroup
	for worker := 0; worker < workerCount; worker++ {
		worker := worker
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for i := 0; i < iterations; i++ {
				checkoutID := fmt.Sprintf("checkout-race-%d", (worker+i)%keyCount)
				token, err := lifecycle.AcquireCheckoutTopology(ctx, checkoutID)
				if err != nil {
					errs <- fmt.Errorf("acquire %s: %w", checkoutID, err)
					return
				}
				runtime.Gosched()
				token.Release()
				if (worker+i)%3 == 0 {
					reclaimed, retireErr := lifecycle.retireCheckoutMutationFence(
						ctx, checkoutID, nil,
					)
					if retireErr != nil || !reclaimed {
						errs <- fmt.Errorf(
							"retire %s: reclaimed=%v err=%v",
							checkoutID,
							reclaimed,
							retireErr,
						)
						return
					}
				}
			}
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}

	for i := 0; i < keyCount; i++ {
		checkoutID := fmt.Sprintf("checkout-race-%d", i)
		if reclaimed, err := lifecycle.retireCheckoutMutationFence(
			ctx, checkoutID, nil,
		); err != nil || !reclaimed {
			t.Fatalf("final retire %s: reclaimed=%v err=%v", checkoutID, reclaimed, err)
		}
	}
	fences.mu.Lock()
	remaining := len(fences.checkouts)
	fences.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("checkout registry retained %d entries after concurrent churn", remaining)
	}
}

func BenchmarkCheckoutMutationFenceRegistryLifecycle(b *testing.B) {
	b.Run("steady_acquire_release", func(b *testing.B) {
		lifecycle, _ := mutationFenceLifecycle()
		ctx := context.Background()
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			token, err := lifecycle.AcquireCheckoutTopology(ctx, "benchmark-checkout")
			if err != nil {
				b.Fatal(err)
			}
			token.Release()
		}
	})

	b.Run("retire_recreate", func(b *testing.B) {
		lifecycle, _ := mutationFenceLifecycle()
		ctx := context.Background()
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			token, err := lifecycle.AcquireCheckoutTopology(ctx, "benchmark-checkout")
			if err != nil {
				b.Fatal(err)
			}
			token.Release()
			if reclaimed, retireErr := lifecycle.retireCheckoutMutationFence(
				ctx, "benchmark-checkout", nil,
			); retireErr != nil || !reclaimed {
				b.Fatalf("retire: reclaimed=%v err=%v", reclaimed, retireErr)
			}
		}
	})
}
