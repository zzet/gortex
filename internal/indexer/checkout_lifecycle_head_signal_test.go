package indexer

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// newHeadSignalCoordinator starts the real coordinator loop with polling
// disabled. Its preflight settles every cycle without touching Git or SQLite,
// so a completed cycle can only have come from the lifecycle signal under test.
func newHeadSignalCoordinator(
	t *testing.T, checkoutID, graphID string,
) (*CheckoutCoordinator, <-chan struct{}) {
	t.Helper()
	gate := NewViewBuildGate()
	gate.Open()
	lifetime, cancel := context.WithCancel(context.Background())
	cycles := make(chan struct{}, 8)
	coordinator := &CheckoutCoordinator{
		checkoutID:     checkoutID,
		gate:           gate,
		logger:         zap.NewNop(),
		quiet:          time.Millisecond,
		poll:           -1,
		signal:         make(chan struct{}, 1),
		stop:           make(chan struct{}),
		done:           make(chan struct{}),
		lifetime:       lifetime,
		cancelLifetime: cancel,
		backlog:        map[int64]struct{}{},
		cyclePreflight: func(context.Context) (CheckoutCycle, bool) {
			return CheckoutCycle{}, true
		},
		cycleDone: func(CheckoutCycle) {
			cycles <- struct{}{}
		},
	}
	go coordinator.run()
	t.Cleanup(func() { require.NoError(t, coordinator.Close()) })
	return coordinator, cycles
}

func requireNoHeadSignalCycle(t *testing.T, cycles <-chan struct{}) {
	t.Helper()
	select {
	case <-cycles:
		t.Fatal("stable or ineligible lifecycle reconciliation woke the coordinator")
	case <-time.After(20 * time.Millisecond):
	}
}

func TestLifecycleAcceptedHeadChangeSignalsCoordinatorWithoutPolling(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*store_sqlite.Checkout)
	}{
		{
			name: "branch switch",
			mutate: func(checkout *store_sqlite.Checkout) {
				checkout.HeadRef = "refs/heads/feature"
				checkout.HeadCommit = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
			},
		},
		{
			name: "active ref advance",
			mutate: func(checkout *store_sqlite.Checkout) {
				checkout.HeadCommit = "cccccccccccccccccccccccccccccccccccccccc"
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			const checkoutID = "checkout-1"
			const graphID = "graph-1"
			coordinator, cycles := newHeadSignalCoordinator(t, checkoutID, graphID)
			lifecycle := &CheckoutLifecycle{
				coordinators:     map[string]*CheckoutCoordinator{},
				coordinatorHeads: map[string]checkoutHeadIdentity{},
			}
			checkout := store_sqlite.Checkout{
				CheckoutID:    checkoutID,
				EffectiveMode: store_sqlite.CheckoutModeAutomatic,
				HeadRef:       "refs/heads/main",
				HeadCommit:    "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			}
			require.True(t, lifecycle.installCoordinatorAtHead(checkout, coordinator))
			requireNoHeadSignalCycle(t, cycles)

			tt.mutate(&checkout)
			started := time.Now()
			lifecycle.ensureCoordinator(context.Background(), graphID, checkout)
			select {
			case <-cycles:
				assert.Less(t, time.Since(started), 250*time.Millisecond)
			case <-time.After(250 * time.Millisecond):
				t.Fatal("accepted HEAD change waited for disabled polling instead of signaling")
			}

			// A stable inventory may be reconciled repeatedly. None of those
			// passes may enqueue another cycle after the changed identity was
			// accepted once.
			for range 64 {
				lifecycle.ensureCoordinator(context.Background(), graphID, checkout)
			}
			requireNoHeadSignalCycle(t, cycles)
		})
	}
}

func TestLifecycleDedicatedHeadChangeDoesNotSignalAutomaticLane(t *testing.T) {
	const checkoutID = "checkout-dedicated"
	const graphID = "graph-dedicated"
	coordinator, cycles := newHeadSignalCoordinator(t, checkoutID, graphID)
	lifecycle := &CheckoutLifecycle{
		coordinators:     map[string]*CheckoutCoordinator{},
		coordinatorHeads: map[string]checkoutHeadIdentity{},
	}
	checkout := store_sqlite.Checkout{
		CheckoutID:    checkoutID,
		EffectiveMode: store_sqlite.CheckoutModeAutomatic,
		HeadRef:       "refs/heads/main",
		HeadCommit:    "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	}
	require.True(t, lifecycle.installCoordinatorAtHead(checkout, coordinator))

	checkout.EffectiveMode = store_sqlite.CheckoutModeDedicated
	checkout.HeadRef = "refs/heads/dedicated"
	checkout.HeadCommit = "dddddddddddddddddddddddddddddddddddddddd"
	lifecycle.ensureCoordinator(context.Background(), graphID, checkout)
	requireNoHeadSignalCycle(t, cycles)

	// The dedicated observation still advances the baseline. Demoting the same
	// accepted identity cannot replay a stale automatic-lane wake.
	checkout.EffectiveMode = store_sqlite.CheckoutModeAutomatic
	lifecycle.ensureCoordinator(context.Background(), graphID, checkout)
	requireNoHeadSignalCycle(t, cycles)
}

func BenchmarkCheckoutLifecycleStableHeadIdentity(b *testing.B) {
	const checkoutID = "checkout-benchmark"
	const graphID = "graph-benchmark"
	coordinator := &CheckoutCoordinator{
		checkoutID: checkoutID,
		done:       make(chan struct{}),
		signal:     make(chan struct{}, 1),
	}
	identity := checkoutHeadIdentity{
		ref:    "refs/heads/main",
		commit: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	}
	lifecycle := &CheckoutLifecycle{
		coordinators:     map[string]*CheckoutCoordinator{checkoutID: coordinator},
		coordinatorHeads: map[string]checkoutHeadIdentity{checkoutID: identity},
	}
	checkout := store_sqlite.Checkout{
		CheckoutID:    checkoutID,
		EffectiveMode: store_sqlite.CheckoutModeAutomatic,
		HeadRef:       identity.ref,
		HeadCommit:    identity.commit,
	}
	ctx := context.Background()

	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		lifecycle.ensureCoordinator(ctx, graphID, checkout)
	}
	b.StopTimer()
	if got := len(coordinator.signal); got != 0 {
		b.Fatalf("stable reconciliation enqueued %d coordinator signals", got)
	}
}
