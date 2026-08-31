package indexer

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

func newCheckoutMutationTicketHarness(t testing.TB) (*CheckoutCoordinator, store_sqlite.CheckoutRoute) {
	t.Helper()
	fixture := newCoordinatorFixture(t)
	dedicated, found, err := fixture.catalog.GetDedicatedGraph(context.Background(), fixture.graphID)
	if err != nil || !found || dedicated.ActiveGenerationID <= 0 {
		t.Fatalf("read fixture dedicated generation: found=%v graph=%+v err=%v", found, dedicated, err)
	}
	route := store_sqlite.CheckoutRoute{
		CheckoutID:         fixture.checkoutID,
		GraphID:            fixture.graphID,
		CommitGenerationID: dedicated.ActiveGenerationID,
		DirtyGenerationID:  dedicated.ActiveGenerationID,
		RouteEpoch:         7,
		State:              store_sqlite.RouteActive,
	}
	if err := fixture.catalog.UpsertCheckoutRoute(context.Background(), route); err != nil {
		t.Fatalf("seed ready mutation route: %v", err)
	}
	coordinator := &CheckoutCoordinator{
		checkoutID: fixture.checkoutID,
		root:       fixture.worktree,
		catalog:    fixture.catalog,
		signal:     make(chan struct{}, 1),
		done:       make(chan struct{}),
	}
	t.Cleanup(func() {
		coordinator.failMutationWaiters(errors.New("test cleanup"))
		close(coordinator.done)
	})
	return coordinator, route
}

func publishCheckoutMutationRoute(
	t testing.TB,
	coordinator *CheckoutCoordinator,
	route *store_sqlite.CheckoutRoute,
) {
	t.Helper()
	route.RouteEpoch++
	if err := coordinator.catalog.UpsertCheckoutRoute(context.Background(), *route); err != nil {
		t.Fatalf("publish checkout mutation route: %v", err)
	}
}

func awaitCheckoutMutationResult(t testing.TB, ticket *MutationTicket) MutationResult {
	t.Helper()
	select {
	case result, ok := <-ticket.Done:
		if !ok {
			t.Fatal("checkout mutation ticket closed without a result")
		}
		return result
	case <-time.After(time.Second):
		t.Fatal("checkout mutation ticket did not resolve")
		return MutationResult{}
	}
}

func TestCheckoutMutationTicketPublishesSelectedRoute(t *testing.T) {
	coordinator, route := newCheckoutMutationTicketHarness(t)
	path := filepath.Join(coordinator.root, "pkg", "edited.go")
	ticket, err := coordinator.enqueueFileMutation(context.Background(), path)
	if err != nil {
		t.Fatalf("enqueue checkout mutation: %v", err)
	}
	if ticket.CheckoutID != coordinator.checkoutID || ticket.ObservedRouteEpoch != route.RouteEpoch {
		t.Fatalf("ticket is not bound to selected route: %+v route=%+v", ticket, route)
	}

	claim := coordinator.mutationClaim()
	publishCheckoutMutationRoute(t, coordinator, &route)
	coordinator.completeMutationClaim(context.Background(), claim, CheckoutCycle{
		CommitGenerationID: route.CommitGenerationID,
		DirtyGenerationID:  route.DirtyGenerationID,
	})
	result := awaitCheckoutMutationResult(t, ticket)
	if result.Err != nil || !result.Reindexed {
		t.Fatalf("published mutation failed: %+v", result)
	}
	if result.CheckoutID != coordinator.checkoutID || result.PublishedRouteEpoch != route.RouteEpoch {
		t.Fatalf("publication truth does not name selected route: %+v route=%+v", result, route)
	}
}

func TestCheckoutMutationReadyRequiresLiveReadyRoute(t *testing.T) {
	coordinator, route := newCheckoutMutationTicketHarness(t)
	lifecycle := &CheckoutLifecycle{
		catalog:      coordinator.catalog,
		coordinators: map[string]*CheckoutCoordinator{coordinator.checkoutID: coordinator},
	}
	if !lifecycle.CheckoutMutationReady(coordinator.checkoutID, coordinator.root) {
		t.Fatal("live coordinator with a ready exact route was not mutation-ready")
	}
	if lifecycle.CheckoutMutationReady(coordinator.checkoutID, filepath.Join(coordinator.root, "sibling")) {
		t.Fatal("coordinator admitted a different checkout root")
	}

	if err := coordinator.catalog.DeleteCheckoutRoute(context.Background(), coordinator.checkoutID); err != nil {
		t.Fatalf("delete checkout route: %v", err)
	}
	if lifecycle.CheckoutMutationReady(coordinator.checkoutID, coordinator.root) {
		t.Fatal("route-free checkout was admitted for a disk mutation")
	}

	route.State = store_sqlite.RoutePending
	if err := coordinator.catalog.UpsertCheckoutRoute(context.Background(), route); err != nil {
		t.Fatalf("restore pending checkout route: %v", err)
	}
	if lifecycle.CheckoutMutationReady(coordinator.checkoutID, coordinator.root) {
		t.Fatal("pending checkout route was admitted for a disk mutation")
	}
}

func TestCheckoutMutationTicketDoesNotLetOlderCycleCoverLaterEdit(t *testing.T) {
	coordinator, route := newCheckoutMutationTicketHarness(t)
	first, err := coordinator.enqueueFileMutation(context.Background(), filepath.Join(coordinator.root, "first.go"))
	if err != nil {
		t.Fatalf("enqueue first mutation: %v", err)
	}
	firstClaim := coordinator.mutationClaim()
	second, err := coordinator.enqueueFileMutation(context.Background(), filepath.Join(coordinator.root, "second.go"))
	if err != nil {
		t.Fatalf("enqueue second mutation: %v", err)
	}

	publishCheckoutMutationRoute(t, coordinator, &route)
	coordinator.completeMutationClaim(context.Background(), firstClaim, CheckoutCycle{
		CommitGenerationID: route.CommitGenerationID,
		DirtyGenerationID:  route.DirtyGenerationID,
	})
	if result := awaitCheckoutMutationResult(t, first); result.Err != nil || !result.Reindexed {
		t.Fatalf("first mutation did not publish: %+v", result)
	}
	select {
	case result := <-second.Done:
		t.Fatalf("older route publication falsely covered later mutation: %+v", result)
	default:
	}

	secondClaim := coordinator.mutationClaim()
	publishCheckoutMutationRoute(t, coordinator, &route)
	coordinator.completeMutationClaim(context.Background(), secondClaim, CheckoutCycle{
		CommitGenerationID: route.CommitGenerationID,
		DirtyGenerationID:  route.DirtyGenerationID,
	})
	if result := awaitCheckoutMutationResult(t, second); result.Err != nil || !result.Reindexed {
		t.Fatalf("second mutation did not publish in its own cycle: %+v", result)
	}
}

func TestCheckoutMutationTicketsTerminateOnFailureCloseAndCancellation(t *testing.T) {
	t.Run("build failure", func(t *testing.T) {
		coordinator, _ := newCheckoutMutationTicketHarness(t)
		ticket, err := coordinator.enqueueFileMutation(context.Background(), filepath.Join(coordinator.root, "failed.go"))
		if err != nil {
			t.Fatalf("enqueue mutation: %v", err)
		}
		want := errors.New("build failed")
		coordinator.completeMutationClaim(context.Background(), coordinator.mutationClaim(), CheckoutCycle{Err: want})
		if result := awaitCheckoutMutationResult(t, ticket); !errors.Is(result.Err, want) || result.Reindexed {
			t.Fatalf("failure was not terminal: %+v", result)
		}
	})

	t.Run("close", func(t *testing.T) {
		coordinator, _ := newCheckoutMutationTicketHarness(t)
		ticket, err := coordinator.enqueueFileMutation(context.Background(), filepath.Join(coordinator.root, "closed.go"))
		if err != nil {
			t.Fatalf("enqueue mutation: %v", err)
		}
		coordinator.failMutationWaiters(errors.New("checkout removed"))
		if result := awaitCheckoutMutationResult(t, ticket); result.Err == nil || result.Reindexed {
			t.Fatalf("close was not terminal: %+v", result)
		}
		if _, err := coordinator.enqueueFileMutation(context.Background(), filepath.Join(coordinator.root, "later.go")); err == nil {
			t.Fatal("closed coordinator admitted another mutation")
		}
	})

	t.Run("cancel before admission", func(t *testing.T) {
		coordinator, _ := newCheckoutMutationTicketHarness(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := coordinator.enqueueFileMutation(ctx, filepath.Join(coordinator.root, "cancelled.go")); !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled admission error = %v, want context.Canceled", err)
		}
	})
}

func TestCheckoutMutationDeferredCycleResignalsWithoutCompletingTicket(t *testing.T) {
	coordinator, _ := newCheckoutMutationTicketHarness(t)
	ticket, err := coordinator.enqueueFileMutation(context.Background(), filepath.Join(coordinator.root, "deferred.go"))
	if err != nil {
		t.Fatalf("enqueue mutation: %v", err)
	}
	// Consume the admission signal so the assertion below observes the retry
	// signal emitted by the deferred completion path, not the original one.
	select {
	case <-coordinator.signal:
	default:
		t.Fatal("mutation admission did not signal the coordinator")
	}
	coordinator.completeMutationClaim(context.Background(), coordinator.mutationClaim(), CheckoutCycle{Deferred: true})
	select {
	case <-coordinator.signal:
	case <-time.After(time.Second):
		t.Fatal("deferred mutation cycle did not schedule a retry")
	}
	select {
	case result := <-ticket.Done:
		t.Fatalf("deferred cycle completed an unpublished mutation: %+v", result)
	default:
	}
}

func TestCheckoutMutationCloseRaceResolvesEveryAdmittedTicket(t *testing.T) {
	coordinator, _ := newCheckoutMutationTicketHarness(t)
	const attempts = 32
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		tickets []*MutationTicket
	)
	start := make(chan struct{})
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			ticket, err := coordinator.enqueueFileMutation(
				context.Background(), filepath.Join(coordinator.root, fmt.Sprintf("race-%d.go", i)))
			if err == nil {
				mu.Lock()
				tickets = append(tickets, ticket)
				mu.Unlock()
			}
		}(i)
	}
	close(start)
	coordinator.failMutationWaiters(errors.New("checkout removed"))
	wg.Wait()
	for _, ticket := range tickets {
		if result := awaitCheckoutMutationResult(t, ticket); result.Err == nil {
			t.Fatalf("admitted ticket survived checkout removal: %+v", result)
		}
	}
}

func BenchmarkCheckoutMutationTicketAdmissionPublication(b *testing.B) {
	coordinator, route := newCheckoutMutationTicketHarness(b)
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ticket, err := coordinator.enqueueFileMutation(ctx, filepath.Join(coordinator.root, "bench.go"))
		if err != nil {
			b.Fatal(err)
		}
		claim := coordinator.mutationClaim()
		route.RouteEpoch++
		if err := coordinator.catalog.UpsertCheckoutRoute(ctx, route); err != nil {
			b.Fatal(err)
		}
		coordinator.completeMutationClaim(ctx, claim, CheckoutCycle{
			CommitGenerationID: route.CommitGenerationID,
			DirtyGenerationID:  route.DirtyGenerationID,
		})
		if result := <-ticket.Done; result.Err != nil || !result.Reindexed {
			b.Fatalf("publication failed: %+v", result)
		}
	}
}

func BenchmarkCheckoutMutationReady(b *testing.B) {
	coordinator, _ := newCheckoutMutationTicketHarness(b)
	lifecycle := &CheckoutLifecycle{
		catalog:      coordinator.catalog,
		coordinators: map[string]*CheckoutCoordinator{coordinator.checkoutID: coordinator},
	}

	b.Run("coordinator_only_baseline", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			lifecycle.coordMu.Lock()
			current := lifecycle.coordinators[coordinator.checkoutID]
			lifecycle.coordMu.Unlock()
			if current == nil || !current.Running() || filepath.Clean(current.root) != filepath.Clean(coordinator.root) {
				b.Fatal("ready coordinator was refused")
			}
		}
	})
	b.Run("ready_route_guard", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if !lifecycle.CheckoutMutationReady(coordinator.checkoutID, coordinator.root) {
				b.Fatal("ready checkout was refused")
			}
		}
	})
}
