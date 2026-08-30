package main

import (
	"context"
	"errors"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/daemon"
)

type testSpawnLock struct {
	mu   sync.Mutex
	held bool
}

func (l *testSpawnLock) TryLock() (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.held {
		return false, nil
	}
	l.held = true
	return true, nil
}

func (l *testSpawnLock) release() {
	l.mu.Lock()
	l.held = false
	l.mu.Unlock()
}

type virtualSpawnClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *virtualSpawnClock) current() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *virtualSpawnClock) advance(d time.Duration) time.Time {
	c.mu.Lock()
	c.now = c.now.Add(d)
	now := c.now
	c.mu.Unlock()
	return now
}

func restoreSeams() {
	isDaemonRunning = daemon.IsRunning
	spawnDaemon = spawnBareDaemon
	stopIntentActive = daemon.StopIntentActive
}

// isolateSpawnLock points the spawn lock + fail marker at a fresh temp
// dir per test so concurrent runs and prior fail-markers don't interfere.
func isolateSpawnLock(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	_ = os.Remove(daemon.SpawnFailMarkerPath())
	t.Cleanup(func() {
		_ = os.Remove(daemon.SpawnFailMarkerPath())
		_ = os.Remove(daemon.SpawnLockPath())
	})
}

func TestEnsureDaemon_AlreadyRunning(t *testing.T) {
	defer restoreSeams()
	var spawned int32
	isDaemonRunning = func() bool { return true }
	spawnDaemon = func() error { atomic.AddInt32(&spawned, 1); return nil }
	if d := ensureDaemonReady(true); d != daemonReady {
		t.Fatalf("want daemonReady, got %d", d)
	}
	if atomic.LoadInt32(&spawned) != 0 {
		t.Fatal("a live daemon must not be re-spawned (and no lock taken)")
	}
}

func TestEnsureDaemon_AutostartOff(t *testing.T) {
	defer restoreSeams()
	isDaemonRunning = func() bool { return false }
	spawnDaemon = func() error { t.Fatal("no spawn when autostart is off"); return nil }
	if d := ensureDaemonReady(false); d != daemonUnavailable {
		t.Fatalf("want daemonUnavailable, got %d", d)
	}
}

func TestEnsureDaemon_StopIntentSuppressesAutostart(t *testing.T) {
	defer restoreSeams()
	var spawned int32
	isDaemonRunning = func() bool { return false }
	stopIntentActive = func() bool { return true }
	spawnDaemon = func() error { atomic.AddInt32(&spawned, 1); return nil }
	// Autostart is on, but the user explicitly stopped the daemon: it must
	// stay down rather than be resurrected by the proxy's autostart path.
	if d := ensureDaemonReady(true); d != daemonUnavailable {
		t.Fatalf("stop-intent must suppress autostart => daemonUnavailable, got %d", d)
	}
	if atomic.LoadInt32(&spawned) != 0 {
		t.Fatal("a deliberately-stopped daemon must not be auto-respawned")
	}
}

func TestEnsureDaemon_RealStopIntentMarkerSuppresses(t *testing.T) {
	isolateSpawnLock(t) // points XDG_CACHE_HOME at a fresh temp dir
	defer restoreSeams()
	stopIntentActive = daemon.StopIntentActive // exercise the real FS-backed check
	var spawned int32
	isDaemonRunning = func() bool { return false }
	spawnDaemon = func() error { atomic.AddInt32(&spawned, 1); return nil }
	if err := daemon.MarkStopIntent(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { daemon.ClearStopIntent() })
	// End-to-end: the real marker write + real read must agree on the path and
	// suppress the spawn — not just the stubbed seam.
	if d := ensureDaemonReady(true); d != daemonUnavailable {
		t.Fatalf("a real stop-intent marker must suppress autostart, got %d", d)
	}
	if atomic.LoadInt32(&spawned) != 0 {
		t.Fatal("must not spawn while a real stop-intent marker is present")
	}
}

func TestEnsureDaemon_SingleFlight(t *testing.T) {
	isolateSpawnLock(t)
	defer restoreSeams()
	var running atomic.Bool
	var spawnCount atomic.Int32
	isDaemonRunning = func() bool { return running.Load() }
	spawnDaemon = func() error {
		spawnCount.Add(1)
		time.Sleep(30 * time.Millisecond) // simulate the spawn window
		running.Store(true)
		return nil
	}
	const K = 8
	var wg sync.WaitGroup
	results := make([]daemonDecision, K)
	for i := 0; i < K; i++ {
		wg.Add(1)
		go func(i int) { defer wg.Done(); results[i] = ensureDaemonReady(true) }(i)
	}
	wg.Wait()
	if got := spawnCount.Load(); got != 1 {
		t.Fatalf("exactly one spawn across %d callers, got %d", K, got)
	}
	for i, r := range results {
		if r == daemonUnavailable {
			t.Fatalf("caller %d should not be unavailable when the spawn succeeded", i)
		}
	}
}

func TestEnsureDaemon_SpawnTimeout(t *testing.T) {
	isolateSpawnLock(t)
	defer restoreSeams()
	isDaemonRunning = func() bool { return false }
	spawnDaemon = func() error { return errors.New("spawn failed") }
	if d := ensureDaemonReady(true); d != daemonUnavailable {
		t.Fatalf("a failed spawn must yield daemonUnavailable, got %d", d)
	}
}

func TestEnsureDaemon_SpawnFailure_SingleAttempt(t *testing.T) {
	isolateSpawnLock(t)
	defer restoreSeams()
	var spawnCount atomic.Int32
	isDaemonRunning = func() bool { return false }
	spawnDaemon = func() error { spawnCount.Add(1); return errors.New("broken spawn") }
	const K = 8
	var wg sync.WaitGroup
	for i := 0; i < K; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _ = ensureDaemonReady(true) }()
	}
	wg.Wait()
	if got := spawnCount.Load(); got != 1 {
		t.Fatalf("a broken spawn must be attempted exactly once within the cooldown, got %d", got)
	}
}

func TestWaitForSpawnLockFreshHeartbeatOutlivesLegacy65SecondCeiling(t *testing.T) {
	lock := &testSpawnLock{held: true} // another caller owns the spawn
	clock := &virtualSpawnClock{now: time.Unix(1_000, 0)}
	reached := make(chan struct{})
	release := make(chan struct{})
	var reachedOnce, releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })

	done := make(chan struct {
		locked bool
		err    error
	}, 1)
	go func() {
		locked, err := waitForSpawnLock(context.Background(), lock, spawnLockWaitOptions{
			now: clock.current,
			wait: func(ctx context.Context, pause time.Duration) error {
				now := clock.advance(pause)
				if now.Sub(time.Unix(1_000, 0)) >= 70*time.Second {
					reachedOnce.Do(func() { close(reached) })
					select {
					case <-ctx.Done():
						return ctx.Err()
					case <-release:
					}
				}
				return nil
			},
			startupProgress: func(time.Time) bool { return true },
			inactivity:      60 * time.Second,
			poll:            time.Second,
		})
		done <- struct {
			locked bool
			err    error
		}{locked: locked, err: err}
	}()

	select {
	case <-reached:
	case result := <-done:
		t.Fatalf("wait returned before 70 virtual seconds: locked=%v err=%v", result.locked, result.err)
	case <-time.After(2 * time.Second):
		t.Fatal("wait did not reach 70 virtual seconds")
	}
	lock.release()
	releaseOnce.Do(func() { close(release) })
	result := <-done
	if result.err != nil || !result.locked {
		t.Fatalf("fresh-heartbeat loser failed after 70 virtual seconds: locked=%v err=%v", result.locked, result.err)
	}
}

func TestWaitForSpawnLockStaleHeartbeatTimesOutAndCancellationWins(t *testing.T) {
	t.Run("stale", func(t *testing.T) {
		lock := &testSpawnLock{held: true}
		clock := &virtualSpawnClock{now: time.Unix(2_000, 0)}
		locked, err := waitForSpawnLock(context.Background(), lock, spawnLockWaitOptions{
			now: clock.current,
			wait: func(_ context.Context, pause time.Duration) error {
				clock.advance(pause)
				return nil
			},
			startupProgress: func(time.Time) bool { return false },
			inactivity:      60 * time.Second,
			poll:            time.Second,
		})
		if locked || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("stale heartbeat: locked=%v err=%v", locked, err)
		}
		if elapsed := clock.current().Sub(time.Unix(2_000, 0)); elapsed != 60*time.Second {
			t.Fatalf("stale heartbeat waited %s, want exactly the inactivity budget", elapsed)
		}
	})

	t.Run("cancelled", func(t *testing.T) {
		lock := &testSpawnLock{held: true}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		locked, err := waitForSpawnLock(ctx, lock, spawnLockWaitOptions{})
		if locked || !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled wait: locked=%v err=%v", locked, err)
		}
	})

	t.Run("lock holder exits", func(t *testing.T) {
		lock := &testSpawnLock{held: true}
		clock := &virtualSpawnClock{now: time.Unix(3_000, 0)}
		waits := 0
		locked, err := waitForSpawnLock(context.Background(), lock, spawnLockWaitOptions{
			now: clock.current,
			wait: func(_ context.Context, pause time.Duration) error {
				clock.advance(pause)
				waits++
				if waits == 2 {
					lock.release()
				}
				return nil
			},
			startupProgress: func(time.Time) bool { return false },
			inactivity:      60 * time.Second,
			poll:            time.Second,
		})
		if err != nil || !locked {
			t.Fatalf("released lock was not acquired promptly: locked=%v err=%v", locked, err)
		}
		if elapsed := clock.current().Sub(time.Unix(3_000, 0)); elapsed != 2*time.Second {
			t.Fatalf("released lock was acquired after %s, want two poll intervals", elapsed)
		}
	})
}

func BenchmarkWaitForSpawnLockContendedHeartbeat(b *testing.B) {
	for i := 0; i < b.N; i++ {
		lock := &testSpawnLock{held: true}
		clock := &virtualSpawnClock{now: time.Unix(4_000, 0)}
		waits := 0
		locked, err := waitForSpawnLock(context.Background(), lock, spawnLockWaitOptions{
			now: clock.current,
			wait: func(_ context.Context, pause time.Duration) error {
				clock.advance(pause)
				waits++
				if waits == 700 { // 70 virtual seconds: beyond the old 65s ceiling.
					lock.release()
				}
				return nil
			},
			startupProgress: func(time.Time) bool { return true },
			inactivity:      60 * time.Second,
			poll:            100 * time.Millisecond,
		})
		if err != nil || !locked {
			b.Fatalf("wait: locked=%v err=%v", locked, err)
		}
	}
	b.ReportMetric(700, "polls/op")
	b.ReportMetric(70, "virtual-s/op")
}
