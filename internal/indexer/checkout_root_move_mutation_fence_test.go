package indexer

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

func TestRootMoveCollisionComponentDrainsOnlyParticipantMutations(t *testing.T) {
	f := newFamilyFixture(t, "move-mutation-fence")
	defer f.close()
	ctx := context.Background()

	secondRoot := f.worktreeOf(f.main, "move-mutation-fence-second")
	_, err := f.lc.Sweep(ctx)
	require.NoError(t, err)

	checkouts, err := f.catalog.ListCheckouts(ctx, f.familyID)
	require.NoError(t, err)
	var second, unrelated store_sqlite.Checkout
	for _, checkout := range checkouts {
		switch {
		case coordinatorRootEqual(checkout.RootPath, secondRoot):
			second = checkout
		case checkout.CheckoutID != f.automatic.CheckoutID:
			unrelated = checkout
		}
	}
	require.NotEmpty(t, second.CheckoutID)
	require.NotEmpty(t, unrelated.CheckoutID)

	held, err := f.lc.AcquireCheckoutMutation(
		ctx, f.automatic.CheckoutID, f.automatic.RootPath,
	)
	require.NoError(t, err)
	defer held.Release()

	participantIDs := map[string]struct{}{
		f.automatic.CheckoutID: {},
		second.CheckoutID:      {},
	}
	var phasesMu sync.Mutex
	phases := make([]string, 0, 6)
	f.lc.moveComponentBarrier = func(phase string, ids []string) {
		if len(ids) != len(participantIDs) {
			return
		}
		for _, id := range ids {
			if _, participant := participantIDs[id]; !participant {
				return
			}
		}
		phasesMu.Lock()
		phases = append(phases, phase)
		phasesMu.Unlock()
	}
	defer func() { f.lc.moveComponentBarrier = nil }()

	temporaryRoot := filepath.Join(f.dir, "move-mutation-fence-temporary")
	runGit(t, f.main, "worktree", "move", f.worktree, temporaryRoot)
	runGit(t, f.main, "worktree", "move", secondRoot, f.worktree)
	runGit(t, f.main, "worktree", "move", temporaryRoot, secondRoot)

	done := make(chan error, 1)
	go func() {
		_, reconcileErr := f.lc.ReconcileFamily(ctx, f.familyID)
		done <- reconcileErr
	}()

	// Reconciliation must be waiting for the participant's pre-existing disk
	// mutation before it can quiesce either coordinator or publish either shell.
	select {
	case err := <-done:
		t.Fatalf("root-move convergence crossed a held participant mutation: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	phasesMu.Lock()
	require.Empty(t, phases)
	phasesMu.Unlock()
	require.NotNil(t, liveCoordinatorOrNil(f.lc, f.automatic.CheckoutID))
	require.NotNil(t, liveCoordinatorOrNil(f.lc, second.CheckoutID))

	// The fence is checkout-scoped: the family's dedicated checkout is not a
	// participant in this root swap and remains available for mutation work.
	unrelatedCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	unrelatedToken, err := f.lc.AcquireCheckoutMutation(
		unrelatedCtx, unrelated.CheckoutID, unrelated.RootPath,
	)
	require.NoError(t, err)
	unrelatedToken.Release()

	held.Release()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("root-move convergence did not resume after mutation release")
	}

	phasesMu.Lock()
	require.Equal(t,
		[]string{"discovered", "quiesced", "revalidated", "published", "reinstalled", "completed"},
		phases,
	)
	phasesMu.Unlock()
	requireNoRootMoveJournal(t, f.catalog, f.automatic.CheckoutID)
	requireNoRootMoveJournal(t, f.catalog, second.CheckoutID)
}
