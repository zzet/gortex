package indexer

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/gitstate"
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
