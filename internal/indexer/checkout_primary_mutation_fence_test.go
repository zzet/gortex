package indexer

import (
	"context"
	"errors"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/config"
)

func TestSetPrimaryDrainsDependentMutationBeforePrimaryCAS(t *testing.T) {
	f := newFamilyFixture(t, "setprimary-mutation-fence")
	defer f.close()
	ctx := context.Background()

	candidate := f.worktreeOf(f.main, "setprimary-mutation-fence-alt")
	runGit(t, candidate, "add", "-A")
	runGit(t, candidate, "commit", "-q", "-m", "alt")
	tracked, err := f.lc.Register(ctx, config.RepoEntry{Path: candidate}, TrackSourceCLI)
	require.NoError(t, err)
	require.NoError(t, tracked.CatalogErr)
	require.NotEqual(t, f.primaryGraph, tracked.GraphID)

	coordinator := lifecycleCoordinator(t, f.lc, f.automatic.CheckoutID)
	coordinator.cycleMu.Lock()
	cycle := coordinator.reconcile(ctx)
	coordinator.cycleMu.Unlock()
	require.NoError(t, cycle.Err)
	_, routed := f.routeOf(f.automatic.CheckoutID)
	require.True(t, routed, "the dependent needs a ready route before mutation admission")
	require.True(t, coordinator.Running())

	mutation, err := f.lc.AcquireCheckoutMutation(
		ctx, f.automatic.CheckoutID, f.automatic.RootPath,
	)
	require.NoError(t, err)
	defer mutation.Release()

	type setPrimaryResult struct {
		result SetPrimaryResult
		err    error
	}
	finished := make(chan setPrimaryResult, 1)
	go func() {
		result, setErr := f.lc.SetPrimary(ctx, tracked.GraphID)
		finished <- setPrimaryResult{result: result, err: setErr}
	}()

	// SetPrimary takes the family cut before it waits for the dependent's
	// mutation token. Observing that cut makes the assertions below independent
	// of goroutine scheduling: the transition has reached its publication path,
	// but cannot have dropped the coordinator or moved the primary epoch yet.
	deadline := time.Now().Add(2 * time.Second)
	for {
		probeCtx, cancel := context.WithTimeout(ctx, 10*time.Millisecond)
		probe, probeErr := f.lc.AcquireCheckoutFamilyTopology(probeCtx, f.familyID)
		cancel()
		if errors.Is(probeErr, context.DeadlineExceeded) {
			break
		}
		if probeErr != nil {
			t.Fatalf("probe set-primary family cut: %v", probeErr)
		}
		probe.Release()
		select {
		case early := <-finished:
			t.Fatalf("SetPrimary crossed a held dependent mutation: result=%+v err=%v",
				early.result, early.err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("SetPrimary did not reach its family topology cut")
		}
		runtime.Gosched()
	}

	select {
	case early := <-finished:
		t.Fatalf("SetPrimary crossed a held dependent mutation: result=%+v err=%v",
			early.result, early.err)
	default:
	}
	candidateBefore, found, err := f.catalog.GetDedicatedGraph(ctx, tracked.GraphID)
	require.NoError(t, err)
	require.True(t, found)
	assert.False(t, candidateBefore.IsPrimaryBase, "primary CAS ran before the mutation drained")
	incumbentBefore, found, err := f.catalog.GetDedicatedGraph(ctx, f.primaryGraph)
	require.NoError(t, err)
	require.True(t, found)
	assert.True(t, incumbentBefore.IsPrimaryBase)
	assert.Same(t, coordinator, liveCoordinatorOrNil(f.lc, f.automatic.CheckoutID),
		"the dependent coordinator was dropped before the whole cohort was fenced")

	mutation.Release()
	var moved setPrimaryResult
	select {
	case moved = <-finished:
	case <-time.After(30 * time.Second):
		t.Fatal("SetPrimary did not resume after the dependent mutation released")
	}
	require.NoError(t, moved.err)
	assert.Empty(t, moved.result.Errors)
	assert.Empty(t, moved.result.Stale)
	assert.Equal(t, []string{f.automatic.CheckoutID}, moved.result.Rebuilt)

	candidateAfter, found, err := f.catalog.GetDedicatedGraph(ctx, tracked.GraphID)
	require.NoError(t, err)
	require.True(t, found)
	assert.True(t, candidateAfter.IsPrimaryBase)
	route, routed := f.routeOf(f.automatic.CheckoutID)
	require.True(t, routed)
	assert.Equal(t, tracked.GraphID, route.GraphID)
	assert.NotZero(t, route.CommitGenerationID)
	assert.NotZero(t, route.DirtyGenerationID)
	replacement := liveCoordinatorOrNil(f.lc, f.automatic.CheckoutID)
	require.NotNil(t, replacement)
	assert.True(t, replacement.Running())
	assert.Equal(t, tracked.GraphID, replacement.graphID)
}
