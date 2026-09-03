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
