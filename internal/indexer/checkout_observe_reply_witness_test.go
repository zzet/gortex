package indexer

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/gitstate"
	"github.com/zzet/gortex/internal/pathkey"
)

// Characterization of the fixture's historical-reply assumption, not a
// product failure: in_progress is a bounded request result, not a worker lease.
func TestObserveCheckoutHistoricalInProgressIsNotARetainedWorker(t *testing.T) {
	lc, _, primary, family, _, _ := newCheckoutObservationFixture(t)
	worktree := filepath.Join(filepath.Dir(primary), "late")
	builderGit(t, primary, "worktree", "add", "-b", "late", worktree)
	initial := filepath.Join(worktree, "initial")
	require.NoError(t, os.Mkdir(initial, 0o700))
	entered := make(chan string, checkoutObservationFamilyCapacity+1)
	release := make(chan struct{})
	t.Cleanup(func() { close(release) }) // Before lifecycle's already registered join.
	lc.observationInventory = func(ctx context.Context, path string) (*gitstate.FamilyInventory, error) {
		entered <- path
		if pathkey.EqualPaths(path, initial) {
			<-ctx.Done()
			return nil, ctx.Err()
		}
		<-release // The other real admitted workers deliberately ignore cancellation.
		return nil, context.Canceled
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	_, found, err := lc.ObserveCheckoutPath(ctx, initial)
	require.False(t, found)
	var pending *CheckoutDiscoveryPendingError
	require.ErrorAs(t, err, &pending)
	require.Equal(t, checkoutDiscoveryInProgress, pending.Reason)
	select {
	case path := <-entered:
		require.True(t, pathkey.EqualPaths(initial, path))
	case <-ctx.Done():
		t.Fatal("initial worker never entered actual inventory seam")
	}
	// No timer/sleep inference: Done is after forgetCheckoutObservation, so this
	// joins the exact real worker and proves its historical reply outlived it.
	finished := make(chan struct{})
	go func() { lc.observationWG.Wait(); close(finished) }()
	select {
	case <-finished:
	case <-ctx.Done():
		t.Fatal("initial bounded worker did not actually finish")
	}
	lc.observationMu.Lock()
	retained := lc.observationJobs[pathkey.CanonicalExistingRoot(initial)]
	lc.observationMu.Unlock()
	require.Nil(t, retained, "a historical in_progress reply cannot certify current worker ownership")
	results := make(chan error, checkoutObservationFamilyCapacity)
	for i := range checkoutObservationFamilyCapacity {
		path := filepath.Join(worktree, fmt.Sprintf("held-%d", i))
		require.NoError(t, os.Mkdir(path, 0o700))
		go func() { _, _, err := lc.ObserveCheckoutPath(ctx, path); results <- err }()
	}
	observed := map[string]bool{}
	for range checkoutObservationFamilyCapacity {
		select {
		case path := <-entered:
			require.False(t, observed[path])
			observed[path] = true
		case <-ctx.Done():
			t.Fatal("eight actual discovery workers did not enter inventory")
		}
	}
	// Every slot is now backed by an actual held worker in this real family.
	lc.observationMu.Lock()
	slots := 0
	for _, job := range lc.observationJobs {
		if job.familyID == family && job.holdsSlot() {
			slots++
		}
	}
	lc.observationMu.Unlock()
	require.Equal(t, checkoutObservationFamilyCapacity, slots)
	_, found, err = lc.ObserveCheckoutPath(ctx, initial)
	require.False(t, found)
	pending = nil
	require.ErrorAs(t, err, &pending)
	require.Equal(t, checkoutDiscoveryFamilyCapacity, pending.Reason)
	require.True(t, pathkey.EqualPaths(worktree, pending.Root))
	require.Equal(t, family, pending.Family)
	require.True(t, errors.Is(err, ErrCheckoutMutationBusy))
	for range checkoutObservationFamilyCapacity {
		select {
		case err := <-results:
			var p *CheckoutDiscoveryPendingError
			require.ErrorAs(t, err, &p)
			require.Equal(t, checkoutDiscoveryInProgress, p.Reason)
		case <-ctx.Done():
			t.Fatal("held-worker callers did not return their bounded pending replies")
		}
	}
}
