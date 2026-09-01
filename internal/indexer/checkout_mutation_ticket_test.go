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
	"github.com/zzet/gortex/internal/reconcile"
)

func newCheckoutMutationTicketHarness(t testing.TB) (*CheckoutCoordinator, store_sqlite.CheckoutRoute) {
	t.Helper()
	fixture := newCoordinatorFixture(t)
	dedicated, found, err := fixture.catalog.GetDedicatedGraph(context.Background(), fixture.graphID)
	if err != nil || !found || dedicated.ActiveGenerationID <= 0 {
		t.Fatalf("read fixture dedicated generation: found=%v graph=%+v err=%v", found, dedicated, err)
	}
	commitGenerationID, err := fixture.catalog.CreateViewGeneration(context.Background(), store_sqlite.ViewGeneration{
		OwnerKind:         checkoutLayerOwnerKind,
		GraphID:           fixture.graphID,
		LayerID:           "mutation-ticket-commit",
		CheckoutID:        fixture.checkoutID,
		GenerationKind:    CommitLayerGenerationKind,
		BaseGenerationID:  dedicated.ActiveGenerationID,
		TreeOID:           "mutation-ticket-tree",
		ConfigHash:        "mutation-ticket-config",
		ExtractorVersions: "mutation-ticket-extractors",
		ResolverVersion:   commitLayerPipelineEpoch,
		State:             store_sqlite.ViewGenerationReady,
	})
	if err != nil {
		t.Fatalf("seed mutation commit generation: %v", err)
	}
	dirtyGenerationID, err := fixture.catalog.CreateViewGeneration(context.Background(), store_sqlite.ViewGeneration{
		OwnerKind:         checkoutLayerOwnerKind,
		GraphID:           fixture.graphID,
		LayerID:           "mutation-ticket-dirty",
		CheckoutID:        fixture.checkoutID,
		GenerationKind:    DirtyLayerGenerationKind,
		BaseGenerationID:  commitGenerationID,
		TreeOID:           "mutation-ticket-tree",
		ConfigHash:        "mutation-ticket-config",
		ExtractorVersions: "mutation-ticket-extractors",
		ResolverVersion:   checkoutResolverVersion,
		State:             store_sqlite.ViewGenerationReady,
	})
	if err != nil {
		t.Fatalf("seed mutation dirty generation: %v", err)
	}
	route := store_sqlite.CheckoutRoute{
		CheckoutID:         fixture.checkoutID,
		GraphID:            fixture.graphID,
		CommitGenerationID: commitGenerationID,
		DirtyGenerationID:  dirtyGenerationID,
		RouteEpoch:         7,
		State:              store_sqlite.RouteActive,
	}
	if err := fixture.catalog.UpsertCheckoutRoute(context.Background(), route); err != nil {
		t.Fatalf("seed ready mutation route: %v", err)
	}
	coordinator := &CheckoutCoordinator{
		checkoutID: fixture.checkoutID,
		root:       fixture.worktree,
		graphID:    fixture.graphID,
		catalog:    fixture.catalog,
		signal:     make(chan struct{}, 1),
		done:       make(chan struct{}),
	}
	t.Cleanup(func() {
		coordinator.failMutationWaiters(errors.New("test cleanup"))
		select {
		case <-coordinator.done:
		default:
			close(coordinator.done)
		}
	})
	return coordinator, route
}

func checkoutMutationLifecycleHarness(
	t testing.TB, coordinator *CheckoutCoordinator,
) *CheckoutLifecycle {
	t.Helper()
	return &CheckoutLifecycle{
		catalog: coordinator.catalog,
		coordinators: map[string]*CheckoutCoordinator{
			coordinator.checkoutID: coordinator,
		},
		mutationFences: newCheckoutMutationFences(),
	}
}

func publishCheckoutMutationRoute(
	t testing.TB,
	coordinator *CheckoutCoordinator,
	route *store_sqlite.CheckoutRoute,
) {
	t.Helper()
	if err := coordinator.catalog.FlipCheckoutRoute(context.Background(), store_sqlite.FlipCheckoutRouteRequest{
		CheckoutID:         route.CheckoutID,
		ExpectedRouteEpoch: route.RouteEpoch,
		GraphID:            route.GraphID,
		CommitGenerationID: route.CommitGenerationID,
		DirtyGenerationID:  route.DirtyGenerationID,
		State:              route.State,
	}); err != nil {
		t.Fatalf("publish checkout mutation route: %v", err)
	}
	route.RouteEpoch++
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
	lifecycle := checkoutMutationLifecycleHarness(t, coordinator)
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

func TestCheckoutMutationTokenDrainsBeforeTopology(t *testing.T) {
	coordinator, _ := newCheckoutMutationTicketHarness(t)
	lifecycle := checkoutMutationLifecycleHarness(t, coordinator)
	token, err := lifecycle.AcquireCheckoutMutation(
		context.Background(), coordinator.checkoutID, coordinator.root,
	)
	if err != nil {
		t.Fatalf("acquire mutation token: %v", err)
	}

	attempted := make(chan struct{})
	acquired := make(chan *CheckoutTopologyToken, 1)
	go func() {
		close(attempted)
		topology, acquireErr := lifecycle.AcquireCheckoutTopology(
			context.Background(), coordinator.checkoutID,
		)
		if acquireErr != nil {
			acquired <- nil
			return
		}
		acquired <- topology
	}()
	<-attempted
	select {
	case topology := <-acquired:
		if topology != nil {
			topology.Release()
		}
		t.Fatal("topology advanced while an unused mutation token was held")
	case <-time.After(20 * time.Millisecond):
	}

	token.Release()
	token.Release()
	select {
	case topology := <-acquired:
		if topology == nil {
			t.Fatal("topology acquisition failed after mutation token release")
		}
		topology.Release()
	case <-time.After(time.Second):
		t.Fatal("topology did not resume after mutation token release")
	}
}

func TestCheckoutMutationTokenTransfersUntilPublication(t *testing.T) {
	coordinator, route := newCheckoutMutationTicketHarness(t)
	lifecycle := checkoutMutationLifecycleHarness(t, coordinator)
	token, err := lifecycle.AcquireCheckoutMutation(
		context.Background(), coordinator.checkoutID, coordinator.root,
	)
	if err != nil {
		t.Fatalf("acquire mutation token: %v", err)
	}
	ticket, err := lifecycle.EnqueueCheckoutMutation(
		context.Background(), token, filepath.Join(coordinator.root, "transferred.go"),
	)
	if err != nil {
		t.Fatalf("transfer mutation token: %v", err)
	}
	// Request cleanup after admission must not release the transferred lease.
	token.Release()

	attempted := make(chan struct{})
	acquired := make(chan *CheckoutTopologyToken, 1)
	go func() {
		close(attempted)
		topology, _ := lifecycle.AcquireCheckoutTopology(context.Background(), coordinator.checkoutID)
		acquired <- topology
	}()
	<-attempted
	select {
	case topology := <-acquired:
		if topology != nil {
			topology.Release()
		}
		t.Fatal("topology advanced before mutation publication terminated")
	case <-time.After(20 * time.Millisecond):
	}

	claim := coordinator.mutationClaim()
	publishCheckoutMutationRoute(t, coordinator, &route)
	coordinator.completeMutationClaim(context.Background(), claim, CheckoutCycle{
		CommitGenerationID: route.CommitGenerationID,
		DirtyGenerationID:  route.DirtyGenerationID,
	})
	if result := awaitCheckoutMutationResult(t, ticket); result.Err != nil || !result.Reindexed {
		t.Fatalf("transferred mutation did not publish: %+v", result)
	}
	select {
	case topology := <-acquired:
		if topology == nil {
			t.Fatal("topology acquisition failed after publication")
		}
		topology.Release()
	case <-time.After(time.Second):
		t.Fatal("topology did not resume after publication")
	}
}

func TestCheckoutLifecycleCloseTerminatesTransferredMutationAndReleasesTopology(t *testing.T) {
	coordinator, _ := newCheckoutMutationTicketHarness(t)
	coordinator.stop = make(chan struct{})
	go func() {
		<-coordinator.stop
		select {
		case <-coordinator.done:
		default:
			close(coordinator.done)
		}
	}()
	lifecycle := checkoutMutationLifecycleHarness(t, coordinator)
	token, err := lifecycle.AcquireCheckoutMutation(
		context.Background(), coordinator.checkoutID, coordinator.root,
	)
	if err != nil {
		t.Fatalf("acquire mutation token: %v", err)
	}
	ticket, err := lifecycle.EnqueueCheckoutMutation(
		context.Background(), token, filepath.Join(coordinator.root, "never-published.go"),
	)
	if err != nil {
		t.Fatalf("transfer mutation token: %v", err)
	}

	closed := make(chan error, 1)
	go func() { closed <- lifecycle.Close() }()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("close lifecycle with transferred ticket: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("lifecycle close waited for a publication cycle that can never run")
	}

	result := awaitCheckoutMutationResult(t, ticket)
	if result.Err == nil || result.Reindexed {
		t.Fatalf("shutdown did not terminate the unpublished ticket: %+v", result)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	topology, err := lifecycle.AcquireCheckoutTopology(ctx, coordinator.checkoutID)
	if err != nil {
		t.Fatalf("shutdown leaked the transferred mutation topology lease: %v", err)
	}
	topology.Release()
}

func TestCheckoutLifecycleCloseUnblocksFamilyRetryWaitingOnTransferredMutation(t *testing.T) {
	coordinator, _ := newCheckoutMutationTicketHarness(t)
	coordinator.stop = make(chan struct{})
	go func() {
		<-coordinator.stop
		select {
		case <-coordinator.done:
		default:
			close(coordinator.done)
		}
	}()
	lifecycle := checkoutMutationLifecycleHarness(t, coordinator)
	token, err := lifecycle.AcquireCheckoutMutation(
		context.Background(), coordinator.checkoutID, coordinator.root,
	)
	if err != nil {
		t.Fatalf("acquire mutation token: %v", err)
	}
	ticket, err := lifecycle.EnqueueCheckoutMutation(
		context.Background(), token, filepath.Join(coordinator.root, "retry-blocked.go"),
	)
	if err != nil {
		t.Fatalf("transfer mutation token: %v", err)
	}

	const (
		familyID = "family-retry-blocked"
		deadline = int64(17)
	)
	lifecycle.familyRetries = map[string]familyRetry{
		familyID: {deadline: deadline},
	}
	retryEntered := make(chan struct{})
	retryDrained := make(chan struct{})
	lifecycle.familyRetryExecute = func(ctx context.Context, _ string) (reconcile.FamilyReport, error) {
		close(retryEntered)
		topology, acquireErr := lifecycle.AcquireCheckoutTopology(ctx, coordinator.checkoutID)
		if acquireErr != nil {
			return reconcile.FamilyReport{}, acquireErr
		}
		topology.Release()
		close(retryDrained)
		return reconcile.FamilyReport{}, nil
	}
	go lifecycle.runFamilyRetry(familyID, deadline)
	select {
	case <-retryEntered:
	case <-time.After(time.Second):
		t.Fatal("family retry did not enter topology convergence")
	}

	closed := make(chan error, 1)
	go func() { closed <- lifecycle.Close() }()
	result := awaitCheckoutMutationResult(t, ticket)
	if result.Err == nil || result.Reindexed {
		t.Fatalf("shutdown did not terminate the retry-blocking ticket: %+v", result)
	}
	select {
	case <-retryDrained:
	case <-time.After(time.Second):
		t.Fatal("family retry remained blocked behind the terminated mutation")
	}
	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("close lifecycle with blocked family retry: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("lifecycle close waited for a retry whose token only close could release")
	}
}

func TestCheckoutLifecycleCloseDrainsCheckoutGatesIndividually(t *testing.T) {
	lifecycle := &CheckoutLifecycle{
		coordinators:     map[string]*CheckoutCoordinator{"checkout-a": nil, "checkout-b": nil},
		coordinatorHeads: map[string]checkoutHeadIdentity{},
		started:          map[string][]*CheckoutCoordinator{},
		familyRetries:    map[string]familyRetry{},
		watcherRetries:   map[string]*watcherRetry{},
		mutationFences:   newCheckoutMutationFences(),
	}
	outerB, err := lifecycle.AcquireCheckoutTopology(context.Background(), "checkout-b")
	if err != nil {
		t.Fatalf("hold outer checkout B: %v", err)
	}
	var releaseOuterB sync.Once
	releaseB := func() { releaseOuterB.Do(outerB.Release) }
	defer releaseB()

	aDrained := make(chan struct{})
	continueClose := make(chan struct{})
	lifecycle.checkoutCloseDrainBarrier = func(checkoutID string) {
		if checkoutID == "checkout-a" {
			close(aDrained)
			<-continueClose
		}
	}
	closed := make(chan error, 1)
	go func() { closed <- lifecycle.Close() }()
	select {
	case <-aDrained:
	case <-time.After(time.Second):
		t.Fatal("close did not drain checkout A before waiting on checkout B")
	}

	nestedDone := make(chan error, 1)
	go func() {
		nestedA, acquireErr := lifecycle.AcquireCheckoutTopology(context.Background(), "checkout-a")
		if acquireErr == nil {
			nestedA.Release()
			releaseB()
		}
		nestedDone <- acquireErr
	}()
	close(continueClose)
	select {
	case err := <-nestedDone:
		if err != nil {
			t.Fatalf("nested topology could not acquire released checkout A: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("close retained checkout A while waiting for checkout B")
	}
	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("close after nested topology release: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("close did not resume after checkout B was released")
	}
}

func TestCheckoutTopologyHasNoCrossCheckoutHeadOfLineBlocking(t *testing.T) {
	coordinator, _ := newCheckoutMutationTicketHarness(t)
	lifecycle := checkoutMutationLifecycleHarness(t, coordinator)
	token, err := lifecycle.AcquireCheckoutMutation(
		context.Background(), coordinator.checkoutID, coordinator.root,
	)
	if err != nil {
		t.Fatalf("acquire checkout A mutation token: %v", err)
	}
	defer token.Release()

	aWaiting := make(chan struct{})
	aDone := make(chan *CheckoutTopologyToken, 1)
	go func() {
		close(aWaiting)
		topology, _ := lifecycle.AcquireCheckoutTopology(context.Background(), coordinator.checkoutID)
		aDone <- topology
	}()
	<-aWaiting

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	bTopology, err := lifecycle.AcquireCheckoutTopology(ctx, "unrelated-checkout-b")
	if err != nil {
		t.Fatalf("unrelated checkout B was blocked behind checkout A: %v", err)
	}
	bTopology.Release()
	select {
	case topology := <-aDone:
		if topology != nil {
			topology.Release()
		}
		t.Fatal("checkout A topology unexpectedly acquired while its mutation was held")
	default:
	}

	token.Release()
	select {
	case topology := <-aDone:
		if topology != nil {
			topology.Release()
		}
	case <-time.After(time.Second):
		t.Fatal("checkout A topology did not resume")
	}
}

func TestCheckoutFamilyTopologySerializesOnlyItsOwnFamily(t *testing.T) {
	coordinator, _ := newCheckoutMutationTicketHarness(t)
	lifecycle := checkoutMutationLifecycleHarness(t, coordinator)
	checkout, found, err := coordinator.catalog.GetCheckout(
		context.Background(), coordinator.checkoutID,
	)
	if err != nil || !found {
		t.Fatalf("read mutation checkout family: found=%v err=%v", found, err)
	}
	familyB := checkout.FamilyID
	familyA := familyB + "-independent"

	heldA, err := lifecycle.AcquireCheckoutFamilyTopology(context.Background(), familyA)
	if err != nil {
		t.Fatalf("acquire family A topology: %v", err)
	}
	defer heldA.Release()

	mutationB, err := lifecycle.AcquireCheckoutMutation(
		context.Background(), coordinator.checkoutID, coordinator.root,
	)
	if err != nil {
		t.Fatalf("family A topology blocked family B mutation: %v", err)
	}
	mutationB.Release()
	topologyB, err := lifecycle.AcquireCheckoutFamilyTopology(context.Background(), familyB)
	if err != nil {
		t.Fatalf("family A topology blocked family B topology: %v", err)
	}
	topologyB.Release()

	blockedCtx, cancelBlocked := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancelBlocked()
	if topology, err := lifecycle.AcquireCheckoutFamilyTopology(blockedCtx, familyA); !errors.Is(err, context.DeadlineExceeded) {
		if topology != nil {
			topology.Release()
		}
		t.Fatalf("same-family topology was not serialized: %v", err)
	}

	heldA.Release()
	resumedCtx, cancelResumed := context.WithTimeout(context.Background(), time.Second)
	defer cancelResumed()
	resumed, err := lifecycle.AcquireCheckoutFamilyTopology(resumedCtx, familyA)
	if err != nil {
		t.Fatalf("same-family topology did not resume after release: %v", err)
	}
	resumed.Release()
}

func TestCheckoutGraphTopologyDrainsOnlyItsBaseGraph(t *testing.T) {
	coordinator, _ := newCheckoutMutationTicketHarness(t)
	lifecycle := checkoutMutationLifecycleHarness(t, coordinator)
	token, err := lifecycle.AcquireCheckoutMutation(
		context.Background(), coordinator.checkoutID, coordinator.root,
	)
	if err != nil {
		t.Fatalf("acquire graph-bound mutation token: %v", err)
	}

	sameGraph := make(chan *CheckoutGraphTopologyToken, 1)
	go func() {
		topology, _ := lifecycle.AcquireCheckoutGraphTopology(context.Background(), coordinator.graphID)
		sameGraph <- topology
	}()
	select {
	case topology := <-sameGraph:
		if topology != nil {
			topology.Release()
		}
		t.Fatal("base graph topology advanced while a composed mutation was held")
	case <-time.After(20 * time.Millisecond):
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	unrelated, err := lifecycle.AcquireCheckoutGraphTopology(ctx, "unrelated-graph")
	if err != nil {
		t.Fatalf("unrelated graph topology was blocked: %v", err)
	}
	unrelated.Release()

	token.Release()
	select {
	case topology := <-sameGraph:
		if topology == nil {
			t.Fatal("same-graph topology acquisition failed after release")
		}
		topology.Release()
	case <-time.After(time.Second):
		t.Fatal("same-graph topology did not resume")
	}
}

func TestCheckoutMutationAdmissionFailureReleasesTransferredToken(t *testing.T) {
	coordinator, _ := newCheckoutMutationTicketHarness(t)
	lifecycle := checkoutMutationLifecycleHarness(t, coordinator)
	token, err := lifecycle.AcquireCheckoutMutation(
		context.Background(), coordinator.checkoutID, coordinator.root,
	)
	if err != nil {
		t.Fatalf("acquire mutation token: %v", err)
	}
	coordinator.mutationMu.Lock()
	coordinator.mutationClosed = true
	coordinator.mutationMu.Unlock()
	if _, err := lifecycle.EnqueueCheckoutMutation(
		context.Background(), token, filepath.Join(coordinator.root, "closed.go"),
	); err == nil {
		t.Fatal("closed coordinator admitted a transferred mutation token")
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	topology, err := lifecycle.AcquireCheckoutTopology(ctx, coordinator.checkoutID)
	if err != nil {
		t.Fatalf("failed admission leaked mutation token: %v", err)
	}
	topology.Release()
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
		publishCheckoutMutationRoute(b, coordinator, &route)
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
	lifecycle := checkoutMutationLifecycleHarness(b, coordinator)

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
	b.Run("topology_token_acquire_release", func(b *testing.B) {
		ctx := context.Background()
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			token, err := lifecycle.AcquireCheckoutMutation(ctx, coordinator.checkoutID, coordinator.root)
			if err != nil {
				b.Fatal(err)
			}
			token.Release()
		}
	})
	b.Run("family_topology_acquire_release", func(b *testing.B) {
		ctx := context.Background()
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			topology, err := lifecycle.AcquireCheckoutFamilyTopology(ctx, "benchmark-family")
			if err != nil {
				b.Fatal(err)
			}
			topology.Release()
		}
	})
}
