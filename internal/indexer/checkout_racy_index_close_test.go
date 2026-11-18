package indexer

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/gitstate"
	"github.com/zzet/gortex/internal/graphview"
)

func TestCoordinatorCloseJoinsItsStartTimeIndexHeal(t *testing.T) {
	f := newCoordinatorFixture(t)
	index := builderGit(t, f.worktree, "rev-parse", "--path-format=absolute", "--git-path", "index")
	lockPath := filepath.Clean(index) + ".lock"
	entered := make(chan error, 1)
	canceled := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	old := racyIndexRefresh
	// Own a real fixture index.lock through the production refresh seam. This
	// controls teardown independently of Git/process scheduling, not a claim
	// that the real Git subprocess race was deterministically reproduced.
	racyIndexRefresh = func(ctx context.Context, _ *gitstate.DirtySampler) (gitstate.RacyIndexReport, gitstate.RacyIndexReport, bool, error) {
		lock, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
		entered <- err
		if err != nil {
			return gitstate.RacyIndexReport{}, gitstate.RacyIndexReport{}, false, err
		}
		defer func() { _ = lock.Close(); _ = os.Remove(lockPath) }()
		<-ctx.Done()
		close(canceled)
		<-release
		return gitstate.RacyIndexReport{}, gitstate.RacyIndexReport{}, true, ctx.Err()
	}
	t.Cleanup(func() { racyIndexRefresh = old })
	c := f.coordinator(t, CheckoutCoordinatorConfig{Debounce: time.Hour, debounceDemand: true})
	t.Cleanup(func() {
		releaseOnce.Do(func() { close(release) })
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := c.CloseContext(ctx); err != nil {
			t.Errorf("join cleanup: %v", err)
		}
		// Join the seam even in the fail-before control where old Close returned
		// early, so the diagnostic cannot leak its owned lock or callback.
		select {
		case <-c.racyHeal.done:
		case <-ctx.Done():
			t.Errorf("refresh cleanup did not finish: %v", ctx.Err())
		}
	})
	select {
	case err := <-entered:
		if err != nil {
			t.Fatalf("own index lock: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("start-time refresh did not begin")
	}
	short, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	err := c.CloseContext(short)
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("CloseContext = %v while refresh still owns index lock, want deadline", err)
	}
	select {
	case <-canceled:
	case <-time.After(5 * time.Second):
		t.Fatal("CloseContext did not cancel start-time refresh")
	}
	if _, err := os.Stat(lockPath); err != nil {
		t.Fatalf("controlled refresh lost its lock before release: %v", err)
	}
	if c.admitSourceMutation() {
		c.releaseSourceMutation()
		t.Fatal("timed-out CloseContext reopened source admission")
	}
	joined := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		joined <- c.CloseContext(ctx)
	}()
	select {
	case err := <-joined:
		t.Fatalf("second CloseContext returned %v before refresh teardown", err)
	case <-time.After(30 * time.Millisecond):
	}
	releaseOnce.Do(func() { close(release) })
	select {
	case err := <-joined:
		if err != nil {
			t.Fatalf("join after release: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("CloseContext did not join released refresh")
	}
	if _, err := os.Stat(lockPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("successful CloseContext left its index lock: %v", err)
	}
	select {
	case <-c.racyHeal.done:
	default:
		t.Fatal("CloseContext returned before refresh completion")
	}
}

func TestCheckoutLifecycleSeedHealersStopAndJoinOnClose(t *testing.T) {
	f := newLifecycleFixture(t)
	root := f.gitRepo("seed-heal-close")
	require.NoError(t, f.cm.Global().AddRepo(config.RepoEntry{Path: root, Name: "seed-heal-close"}))
	type requestKey struct{}
	ctx, cancel := context.WithCancel(context.WithValue(t.Context(), requestKey{}, "seed-value"))
	defer cancel()
	entered := make(chan context.Context, 2)
	canceled := make(chan struct{})
	release := make(chan struct{})
	finished := make(chan struct{})
	var releaseOnce sync.Once
	old := racyIndexRefresh
	racyIndexRefresh = func(ctx context.Context, _ *gitstate.DirtySampler) (gitstate.RacyIndexReport, gitstate.RacyIndexReport, bool, error) {
		entered <- ctx
		select {
		case <-ctx.Done():
			close(canceled)
		case <-release:
		}
		<-release
		close(finished)
		return gitstate.RacyIndexReport{}, gitstate.RacyIndexReport{}, true, ctx.Err()
	}
	t.Cleanup(func() { racyIndexRefresh = old })
	t.Cleanup(func() {
		releaseOnce.Do(func() { close(release) })
		f.close()
		select {
		case <-finished:
		case <-time.After(5 * time.Second):
			t.Error("seed healer did not finish cleanup")
		}
	})
	require.NoError(t, f.lc.Seed(ctx))
	var healCtx context.Context
	select {
	case healCtx = <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("Seed did not start a healer")
	}
	cancel()
	require.NoError(t, healCtx.Err(), "caller cancellation must not stop the seed healer")
	require.Equal(t, "seed-value", healCtx.Value(requestKey{}))
	joined := make(chan error, 1)
	go func() { joined <- f.lc.Close() }()
	select {
	case <-canceled:
	case err := <-joined:
		t.Fatalf("Close returned %v without canceling its seed healer", err)
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not cancel its seed healer")
	}
	select {
	case err := <-joined:
		t.Fatalf("Close returned %v before the seed healer finished", err)
	case <-time.After(30 * time.Millisecond):
	}
	releaseOnce.Do(func() { close(release) })
	select {
	case err := <-joined:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not join the released healer")
	}
	select {
	case <-finished:
	default:
		t.Fatal("Close returned before the refresh finished")
	}
	// Seed itself refuses the closed repository admission before its heal loop.
	require.ErrorIs(t, f.lc.Seed(t.Context()), graphview.ErrRepositoryAdmissionsStopped)
}

func TestCheckoutLifecycleClosedAdmissionRefusesSeedHealer(t *testing.T) {
	f := newCoordinatorFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	l := &CheckoutLifecycle{catalog: f.catalog, transitionCtx: ctx}
	refreshes := 0
	old := racyIndexRefresh
	racyIndexRefresh = func(context.Context, *gitstate.DirtySampler) (gitstate.RacyIndexReport, gitstate.RacyIndexReport, bool, error) {
		refreshes++
		return gitstate.RacyIndexReport{}, gitstate.RacyIndexReport{}, false, nil
	}
	t.Cleanup(func() { racyIndexRefresh = old })
	// Both ready checkouts must reach the refresh seam with a live context.
	l.healFamilyRacyIndexes(ctx, f.familyID)
	require.Equal(t, 2, refreshes)
	refreshes = 0
	l.transitionMu.Lock()
	l.transitionClosed = true
	l.transitionMu.Unlock()
	l.startFamilyRacyIndexHeal(ctx, f.familyID)
	l.transitionWG.Wait()
	require.NoError(t, ctx.Err(), "only closed admission may refuse this healer")
	require.Zero(t, refreshes, "closed transition admission started a healer")
}
