package graphview

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// A noncancelable context keeps earlier SQL calls free of row-cancel watchers.
// Only the full legacy ancestry lease holds generation zero; observing it in
// Done gates the final QueryRowContext before its real catalog route read.
// This changes neither production SQL nor the materializer's call sequence.
type routePinRereadContext struct {
	context.Context
	leases  *LeaseManager
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (c *routePinRereadContext) Done() <-chan struct{} {
	if c.leases.InUse(BaseCorpusGeneration) {
		c.once.Do(func() {
			close(c.entered)
			<-c.release
		})
	}
	return c.Context.Done()
}

func TestPinCheckoutRoutePreservesBothExactMovementOutcomes(t *testing.T) {
	for _, finalReread := range []bool{false, true} {
		name := "initial reread after old payload retirement"
		if finalReread {
			name = "final reread with complete ancestry held"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			store := openStackStore(t, "route-pin-movement")
			oldCommit, oldDirty := seedRoutedStack(t, store)
			m := newTestMaterializer(store)
			require.Zero(t, m.Leases.Held())
			old, found, err := store.Catalog().GetCheckoutRoute(ctx, testCheckoutID)
			require.NoError(t, err)
			require.True(t, found)
			newCommit := writeStackCommitGeneration(t, store)
			newDirty := writeStackDirtyGeneration(t, store, newCommit)
			flip := func() {
				require.NoError(t, store.Catalog().FlipCheckoutRoute(ctx, store_sqlite.FlipCheckoutRouteRequest{
					CheckoutID: testCheckoutID, ExpectedRouteEpoch: old.RouteEpoch,
					GraphID: testGraphID, CommitGenerationID: newCommit, DirtyGenerationID: newDirty, State: store_sqlite.RouteActive,
				}))
			}
			type result struct {
				lease *Lease
				moved bool
				err   error
			}
			var got result
			if !finalReread {
				flip()
				require.NoError(t, store.RetirePayloadGeneration(ctx, oldDirty, m.Leases.InUse))
				require.NoError(t, store.RetirePayloadGeneration(ctx, oldCommit, m.Leases.InUse))
				got.lease, got.moved, got.err = m.PinCheckoutRoute(ctx, testCheckoutID, old)
			} else {
				barrier := &routePinRereadContext{Context: ctx, leases: m.Leases, entered: make(chan struct{}), release: make(chan struct{})}
				var releaseOnce sync.Once
				release := func() { releaseOnce.Do(func() { close(barrier.release) }) }
				done, exited := make(chan result, 1), make(chan struct{})
				go func() {
					defer close(exited)
					lease, moved, err := m.PinCheckoutRoute(barrier, testCheckoutID, old)
					done <- result{lease, moved, err}
				}()
				defer func() {
					release()
					select {
					case <-exited:
					case <-time.After(5 * time.Second):
						t.Error("route-pin reader did not drain")
					}
				}()
				select {
				case <-barrier.entered:
				case early := <-done:
					t.Fatalf("pin returned before the final reread barrier: %+v", early)
				case <-time.After(5 * time.Second):
					t.Fatal("pin did not reach the complete-ancestry reread")
				}
				for _, id := range []int64{BaseCorpusGeneration, oldCommit, oldDirty} {
					require.True(t, m.Leases.InUse(id), "full ancestry must be pinned at the final reread")
				}
				flip()
				release()
				select {
				case got = <-done:
				case <-time.After(5 * time.Second):
					t.Fatal("pin did not return after the actual route flip")
				}
			}
			if got.lease != nil {
				got.lease.Release()
			}
			require.Nil(t, got.lease)
			require.True(t, got.moved)
			require.ErrorIs(t, got.err, ErrViewBuilding)
			var refusal *ViewError
			require.ErrorAs(t, got.err, &refusal)
			require.Equal(t, "checkout route moved before ancestry pin", refusal.Message)
			require.Zero(t, m.Leases.Held(), "both provisional and full pins must drain on movement")
			view, err := m.MaterializeCheckout(ctx, testCheckoutID)
			require.NoError(t, err)
			defer view.Close()
			require.Equal(t, []int64{newCommit, newDirty}, view.Generations())
		})
	}
}

func TestPinCheckoutRouteDoesNotMisclassifyOtherRefusals(t *testing.T) {
	for _, kind := range []string{"invalid route", "nil context", "invalid materializer", "canceled", "missing route", "retired route", "building ancestry", "retiring ancestry", "catalog failure"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			store := openStackStore(t, "route-pin-refusals")
			commit, dirty := seedRoutedStack(t, store)
			m := newTestMaterializer(store)
			route, found, err := store.Catalog().GetCheckoutRoute(ctx, testCheckoutID)
			require.NoError(t, err)
			require.True(t, found)
			var cause error
			wantCode := CodeCheckoutInaccessible
			switch kind {
			case "invalid route":
				route.State = store_sqlite.RoutePending
				wantCode = CodeViewBuilding
			case "nil context":
				ctx = nil
				wantCode = CodeViewBuilding
			case "invalid materializer":
				m.Store = nil
				wantCode = CodeInvalidViewSelector
			case "canceled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
				cause = context.Canceled
			case "missing route":
				require.NoError(t, store.Catalog().DeleteCheckoutRoute(ctx, testCheckoutID))
			case "retired route":
				require.NoError(t, store.Catalog().FlipCheckoutRoute(ctx, store_sqlite.FlipCheckoutRouteRequest{CheckoutID: testCheckoutID, ExpectedRouteEpoch: route.RouteEpoch, GraphID: testGraphID, CommitGenerationID: commit, DirtyGenerationID: dirty, State: store_sqlite.RouteRetired}))
			case "building ancestry":
				require.NoError(t, store.Catalog().SetViewGenerationState(ctx, dirty, store_sqlite.ViewGenerationBuilding, store_sqlite.ViewGenerationReady))
				wantCode = CodeViewBuilding
			case "retiring ancestry":
				setGenerationState(t, store, dirty, store_sqlite.ViewGenerationRetiring, store_sqlite.ViewGenerationReady)
			case "catalog failure":
				require.NoError(t, store.Close())
				_, _, cause = store.Catalog().GetCheckoutRoute(ctx, testCheckoutID)
				require.Error(t, cause)
			}
			lease, moved, err := m.PinCheckoutRoute(ctx, testCheckoutID, route)
			if lease != nil {
				lease.Release()
			}
			require.Nil(t, lease)
			require.False(t, moved, "a generic refusal must never become exact route movement")
			require.Error(t, err)
			var refusal *ViewError
			require.ErrorAs(t, err, &refusal)
			require.Equal(t, wantCode, refusal.Code)
			if cause != nil {
				require.True(t, errors.Is(err, cause), "original cause must remain inspectable: %v", err)
			}
			require.Zero(t, m.Leases.Held())
		})
	}
}
