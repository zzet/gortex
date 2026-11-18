package indexer

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/gitstate"
)

// TestRacyIndexRefreshWaitsOutTheLifecyclesGitWork: a coordinator activated
// while the lifecycle's own git work runs (a mode transition) does not take
// the checkout's index lock until that work ends, and the start-time refresh
// still runs afterwards.
func TestRacyIndexRefreshWaitsOutTheLifecyclesGitWork(t *testing.T) {
	f := newFamilyFixture(t, "racyserial")
	defer f.close()
	ctx := context.Background()

	var mu sync.Mutex
	var calls []bool
	old := racyIndexRefresh
	racyIndexRefresh = func(ctx context.Context, s *gitstate.DirtySampler) (gitstate.RacyIndexReport, gitstate.RacyIndexReport, bool, error) {
		mu.Lock()
		calls = append(calls, f.lc.gitWork.busy())
		mu.Unlock()
		return gitstate.RacyIndexReport{}, gitstate.RacyIndexReport{}, false, nil
	}
	t.Cleanup(func() { racyIndexRefresh = old })

	f.worktreeOf(f.main, "racyserial-second")
	release := f.lc.gitWork.hold()
	_, err := f.lc.Sweep(ctx)
	require.NoError(t, err)
	second := f.automaticCheckoutID(f.familyID, "racyserial-second")
	require.NotEmpty(t, second)
	if !f.lc.coordinatorRegistered(second) {
		require.True(t, f.lc.ActivateCheckout(second, "test activation"))
	}
	deadline := time.Now().Add(15 * time.Second)
	for !f.lc.coordinatorRegistered(second) {
		require.False(t, time.Now().After(deadline), "coordinator never came up")
		time.Sleep(time.Millisecond)
	}
	time.Sleep(200 * time.Millisecond)
	mu.Lock()
	during := len(calls)
	mu.Unlock()
	require.Zero(t, during, "the refresh ran while the lifecycle's git work was in flight")

	release()
	f.lc.coordMu.Lock()
	coordinator := f.lc.coordinators[second]
	f.lc.coordMu.Unlock()
	require.True(t, coordinator.awaitRacyIndexHeal(30*time.Second))
	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, []bool{false}, calls, "the refresh must run once, after the git work")
}

// TestLifecycleGitWorkWaitsForARefreshInFlight: the lifecycle's git work does
// not start while a refresh holds the index lock.
func TestLifecycleGitWorkWaitsForARefreshInFlight(t *testing.T) {
	var w checkoutGitWork
	release, ok := w.refresh(context.Background(), time.Second)
	require.True(t, ok)
	held := make(chan struct{})
	go func() {
		done := w.hold()
		close(held)
		done()
	}()
	select {
	case <-held:
		t.Fatal("git work started while a refresh held the index")
	case <-time.After(100 * time.Millisecond):
	}
	release()
	select {
	case <-held:
	case <-time.After(5 * time.Second):
		t.Fatal("git work never started after the refresh finished")
	}
}
