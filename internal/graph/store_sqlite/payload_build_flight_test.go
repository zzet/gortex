package store_sqlite

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func payloadFlightHandles() (*Store, *Store) {
	core := &storeCore{}
	return &Store{storeCore: core}, &Store{storeCore: core}
}

func awaitPayloadFlightWaiters(t testing.TB, store *Store, generationID, want int64) {
	t.Helper()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		if got := store.PayloadBuildFlightWaiters(generationID); got == want {
			return
		}
		select {
		case <-deadline.C:
			t.Fatalf("payload flight %d waiters = %d, want %d",
				generationID, store.PayloadBuildFlightWaiters(generationID), want)
		case <-ticker.C:
		}
	}
}

func TestPayloadBuildFlightIsSharedByStoreCore(t *testing.T) {
	owner, sibling := payloadFlightHandles()
	leader, isLeader, ready, err := owner.JoinPayloadBuildFlight(context.Background(), 41, false)
	if err != nil || !isLeader || ready {
		t.Fatalf("leader join = (%v, %t, %t, %v)", leader, isLeader, ready, err)
	}
	follower, isLeader, ready, err := sibling.JoinPayloadBuildFlight(context.Background(), 41, true)
	if err != nil || isLeader || ready {
		t.Fatalf("follower join = (%v, %t, %t, %v)", follower, isLeader, ready, err)
	}

	waited := make(chan error, 1)
	go func() { waited <- follower.Wait(context.Background()) }()
	awaitPayloadFlightWaiters(t, owner, 41, 1)
	leader.Complete(nil)
	if err := <-waited; err != nil {
		t.Fatalf("follower wait: %v", err)
	}
	if got := owner.PayloadBuildFlightWaiters(41); got != 0 {
		t.Fatalf("completed flight waiters = %d, want 0", got)
	}

	retry, isLeader, ready, err := sibling.JoinPayloadBuildFlight(context.Background(), 41, false)
	if err != nil || !isLeader || ready {
		t.Fatalf("post-completion join = (%v, %t, %t, %v)", retry, isLeader, ready, err)
	}
	retry.Complete(nil)
}

func TestPayloadBuildFlightFollowerCancellationDoesNotCancelLeader(t *testing.T) {
	owner, sibling := payloadFlightHandles()
	leader, isLeader, _, err := owner.JoinPayloadBuildFlight(context.Background(), 52, false)
	if err != nil || !isLeader {
		t.Fatalf("leader join: leader=%t err=%v", isLeader, err)
	}
	follower, isLeader, _, err := sibling.JoinPayloadBuildFlight(context.Background(), 52, true)
	if err != nil || isLeader {
		t.Fatalf("follower join: leader=%t err=%v", isLeader, err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	waited := make(chan error, 1)
	go func() { waited <- follower.Wait(ctx) }()
	awaitPayloadFlightWaiters(t, owner, 52, 1)
	cancel()
	if err := <-waited; !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled follower wait = %v, want context canceled", err)
	}

	other, isLeader, _, err := sibling.JoinPayloadBuildFlight(context.Background(), 52, true)
	if err != nil || isLeader {
		t.Fatalf("leader flight vanished after follower cancellation: leader=%t err=%v", isLeader, err)
	}
	leader.Complete(nil)
	if err := other.Wait(context.Background()); err != nil {
		t.Fatalf("remaining follower wait: %v", err)
	}
}

func TestPayloadBuildFlightFailureAllowsRetry(t *testing.T) {
	owner, sibling := payloadFlightHandles()
	leader, isLeader, _, err := owner.JoinPayloadBuildFlight(context.Background(), 63, false)
	if err != nil || !isLeader {
		t.Fatalf("leader join: leader=%t err=%v", isLeader, err)
	}
	follower, isLeader, _, err := sibling.JoinPayloadBuildFlight(context.Background(), 63, true)
	if err != nil || isLeader {
		t.Fatalf("follower join: leader=%t err=%v", isLeader, err)
	}

	wantErr := errors.New("physical build failed")
	leader.Complete(wantErr)
	if err := follower.Wait(context.Background()); !errors.Is(err, wantErr) {
		t.Fatalf("follower error = %v, want %v", err, wantErr)
	}
	retry, isLeader, ready, err := sibling.JoinPayloadBuildFlight(context.Background(), 63, false)
	if err != nil || !isLeader || ready {
		t.Fatalf("retry join = (%v, %t, %t, %v)", retry, isLeader, ready, err)
	}
	retry.Complete(nil)
}

func TestPayloadBuildFlightsAreDistinctByGenerationAndStore(t *testing.T) {
	firstStore, _ := payloadFlightHandles()
	secondStore, _ := payloadFlightHandles()

	first, firstLeader, _, err := firstStore.JoinPayloadBuildFlight(context.Background(), 1, false)
	if err != nil || !firstLeader {
		t.Fatalf("first generation leader=%t err=%v", firstLeader, err)
	}
	secondGeneration, secondLeader, _, err := firstStore.JoinPayloadBuildFlight(context.Background(), 2, false)
	if err != nil || !secondLeader {
		t.Fatalf("second generation leader=%t err=%v", secondLeader, err)
	}
	secondStoreFlight, otherStoreLeader, _, err := secondStore.JoinPayloadBuildFlight(context.Background(), 1, false)
	if err != nil || !otherStoreLeader {
		t.Fatalf("second store leader=%t err=%v", otherStoreLeader, err)
	}
	first.Complete(nil)
	secondGeneration.Complete(nil)
	secondStoreFlight.Complete(nil)
}

func TestPayloadBuildFlightRechecksReadyAdoptionAfterFlightRemoval(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "ready-race.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	generationID, _, adopted, err := store.BeginPayloadGenerationWithStatus(ctx, PayloadGenerationRequest{
		OwnerKind:      "dedicated_graph",
		GraphID:        "graph-ready-race",
		LayerID:        "layer-ready-race",
		CheckoutID:     "checkout-ready-race",
		GenerationKind: "commit",
		TreeOID:        "tree-ready-race",
		CreatedAt:      time.Now().Unix(),
	})
	if err != nil || adopted {
		t.Fatalf("begin generation: adopted=%t err=%v", adopted, err)
	}
	// A building generation with no process-local flight is restart recovery:
	// the adopted caller becomes its new physical writer.
	former, isLeader, ready, err := store.JoinPayloadBuildFlight(ctx, generationID, true)
	if err != nil || !isLeader || ready {
		t.Fatalf("recovery flight = (%v, %t, %t, %v)", former, isLeader, ready, err)
	}
	if err := store.Catalog().PublishViewGeneration(ctx, generationID, time.Now().Unix()); err != nil {
		t.Fatalf("publish catalog generation: %v", err)
	}
	former.Complete(nil)

	flight, isLeader, ready, err := store.JoinPayloadBuildFlight(ctx, generationID, true)
	if err != nil || flight != nil || isLeader || !ready {
		t.Fatalf("ready recheck = (%v, %t, %t, %v)", flight, isLeader, ready, err)
	}
}

func payloadLifecycleRaceStore(t testing.TB, suffix string) (*Store, PayloadGenerationRequest) {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), suffix+".sqlite"))
	if err != nil {
		t.Fatalf("open payload lifecycle store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store, PayloadGenerationRequest{
		OwnerKind:      "dedicated_graph",
		GraphID:        "graph-" + suffix,
		LayerID:        "layer-" + suffix,
		CheckoutID:     "checkout-" + suffix,
		GenerationKind: "commit",
		TreeOID:        "tree-" + suffix,
		CreatedAt:      time.Now().Unix(),
	}
}

func TestPayloadBuildFlightStartExcludesRetirementClaim(t *testing.T) {
	store, request := payloadLifecycleRaceStore(t, "flight-wins")
	ctx := context.Background()
	generationID, _, adopted, err := store.BeginPayloadGenerationWithStatus(ctx, request)
	if err != nil || adopted {
		t.Fatalf("seed building generation: adopted=%t err=%v", adopted, err)
	}

	joinEntered := make(chan struct{})
	releaseJoin := make(chan struct{})
	type startResult struct {
		start PayloadBuildFlightStart
		err   error
	}
	started := make(chan startResult, 1)
	go func() {
		start, startErr := store.BeginPayloadBuildFlight(ctx, request, func(id int64, wasAdopted bool) {
			if id != generationID || !wasAdopted {
				return
			}
			close(joinEntered)
			<-releaseJoin
		})
		started <- startResult{start: start, err: startErr}
	}()
	select {
	case <-joinEntered:
	case <-time.After(10 * time.Second):
		t.Fatal("recovery join did not enter lifecycle critical section")
	}

	retired := make(chan error, 1)
	go func() { retired <- store.RetirePayloadGeneration(ctx, generationID, nil) }()
	select {
	case err := <-retired:
		t.Fatalf("retirement crossed an in-progress recovery join: %v", err)
	case <-time.After(25 * time.Millisecond):
	}

	close(releaseJoin)
	var result startResult
	select {
	case result = <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("recovery join did not finish")
	}
	if result.err != nil || !result.start.Adopted || !result.start.Leader || result.start.Ready {
		t.Fatalf("recovery start = (%+v, %v)", result.start, result.err)
	}
	select {
	case err := <-retired:
		if !errors.Is(err, ErrPayloadGenerationInUse) {
			t.Fatalf("retirement after recovery flight = %v, want in use", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("retirement did not observe installed recovery flight")
	}
	row, found, err := store.Catalog().GetViewGeneration(ctx, generationID)
	if err != nil || !found || row.State != ViewGenerationBuilding {
		t.Fatalf("protected generation = (%+v, %t, %v)", row, found, err)
	}

	result.start.Flight.Complete(nil)
	if err := store.RetirePayloadGeneration(ctx, generationID, nil); err != nil {
		t.Fatalf("retire after flight completion: %v", err)
	}
}

func TestPayloadRetirementClaimExcludesRecoveryFlightJoin(t *testing.T) {
	store, request := payloadLifecycleRaceStore(t, "retirement-wins")
	ctx := context.Background()
	generationID, _, adopted, err := store.BeginPayloadGenerationWithStatus(ctx, request)
	if err != nil || adopted {
		t.Fatalf("seed building generation: adopted=%t err=%v", adopted, err)
	}

	claimEntered := make(chan struct{})
	releaseClaim := make(chan struct{})
	retired := make(chan error, 1)
	go func() {
		retired <- store.retirePayloadGeneration(ctx, generationID, nil, func() {
			close(claimEntered)
			<-releaseClaim
		})
	}()
	select {
	case <-claimEntered:
	case <-time.After(10 * time.Second):
		t.Fatal("retirement did not enter lifecycle critical section")
	}

	type joinResult struct {
		flight *PayloadBuildFlight
		leader bool
		ready  bool
		err    error
	}
	joined := make(chan joinResult, 1)
	go func() {
		flight, leader, ready, joinErr := store.JoinPayloadBuildFlight(ctx, generationID, true)
		joined <- joinResult{flight: flight, leader: leader, ready: ready, err: joinErr}
	}()
	select {
	case result := <-joined:
		t.Fatalf("recovery join crossed an in-progress retirement claim: %+v", result)
	case <-time.After(25 * time.Millisecond):
	}

	close(releaseClaim)
	select {
	case err := <-retired:
		if err != nil {
			t.Fatalf("retire generation: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("retirement did not finish")
	}
	select {
	case result := <-joined:
		if result.err == nil || result.flight != nil || result.leader || result.ready {
			t.Fatalf("join after retirement claim = %+v", result)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("recovery join did not classify retired generation")
	}
	if store.PayloadBuildFlightActive(generationID) {
		t.Fatalf("retired generation %d retained a recovery flight", generationID)
	}
	_, found, err := store.Catalog().GetViewGeneration(ctx, generationID)
	if err != nil || found {
		t.Fatalf("retired generation row found=%t err=%v", found, err)
	}
}

func BenchmarkPayloadBuildFlight(b *testing.B) {
	for _, callers := range []int{1, 8, 64} {
		b.Run(fmt.Sprintf("%d_callers", callers), func(b *testing.B) {
			store, _ := payloadFlightHandles()
			var physicalBuilds atomic.Int64
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				generationID := int64(i + 1)
				leader, isLeader, _, err := store.JoinPayloadBuildFlight(context.Background(), generationID, false)
				if err != nil || !isLeader {
					b.Fatalf("leader join: leader=%t err=%v", isLeader, err)
				}
				physicalBuilds.Add(1)

				var joined sync.WaitGroup
				var done sync.WaitGroup
				joined.Add(callers - 1)
				done.Add(callers - 1)
				errs := make(chan error, callers-1)
				for n := 1; n < callers; n++ {
					go func() {
						defer done.Done()
						follower, followerLeader, _, joinErr := store.JoinPayloadBuildFlight(
							context.Background(), generationID, true,
						)
						if joinErr != nil || followerLeader {
							joined.Done()
							errs <- joinErr
							return
						}
						joined.Done()
						errs <- follower.Wait(context.Background())
					}()
				}
				joined.Wait()
				leader.Complete(nil)
				done.Wait()
				close(errs)
				for err := range errs {
					if err != nil {
						b.Fatalf("follower: %v", err)
					}
				}
			}
			b.ReportMetric(float64(physicalBuilds.Load())/float64(b.N), "leaders/op")
		})
	}
}
