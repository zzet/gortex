package main

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/daemon"
	"github.com/zzet/gortex/internal/indexer"
)

func TestControllerReadinessRequiresFrozenStartupViews(t *testing.T) {
	c := &realController{}
	c.setStartupViewReadiness(startupViewReadiness{Expected: 2, Building: 2})
	c.MarkReady(2 * time.Second)
	require.False(t, c.IsReady(), "reference warmup cannot bypass exact startup views")

	c.MarkEnriched(3 * time.Second)
	require.False(t, c.IsReady(), "the enrichment fallback cannot bypass exact startup views")
	require.False(t, c.IsEnriched(), "reference enrichment cannot bypass exact startup views")

	c.setStartupViewReadiness(startupViewReadiness{Expected: 2, Ready: 1, Failed: 1})
	phase, ready, extra := c.filterReadinessPhase("enrichment_complete", true, nil)
	assert.Equal(t, "degraded", phase)
	assert.False(t, ready)
	assert.Equal(t, 2, extra["startup_views_expected"])
	assert.Equal(t, 1, extra["startup_views_ready"])
	assert.Equal(t, 1, extra["startup_views_failed"])
	assert.Equal(t, false, extra["queryable"])
	assert.Equal(t, false, extra["enriched"])

	c.setStartupViewReadiness(startupViewReadiness{Expected: 2, Ready: 2})
	assert.True(t, c.IsReady())
	assert.True(t, c.IsEnriched())
	phase, ready, _ = c.filterReadinessPhase("enrichment_complete", true, nil)
	assert.Equal(t, "enrichment_complete", phase)
	assert.True(t, ready)
}

func TestControllerReadinessExpectedZeroPreservesLegacy(t *testing.T) {
	c := &realController{}
	c.setStartupViewReadiness(startupViewReadiness{})
	c.MarkReady(time.Second)
	assert.True(t, c.IsReady())

	phase, ready, extra := c.filterReadinessPhase("ready", true, map[string]any{"queryable": true})
	assert.Equal(t, "ready", phase)
	assert.True(t, ready)
	assert.Equal(t, map[string]any{"queryable": true}, extra)
}

func TestStartupViewReadinessMonitorFreezesCohortAndCoalesces(t *testing.T) {
	monitor := newStartupViewReadinessMonitor([]string{"/catalog-gap", "/ready", "/building"})
	states := map[string]daemon.TrackReadiness{
		"/catalog-gap": {State: daemon.TrackReadinessLegacy},
		"/ready":       {State: daemon.TrackReadinessReady, View: &daemon.ProbeView{CheckoutID: "ready-id", Exact: true}},
		"/building":    {State: daemon.TrackReadinessBuilding, View: &daemon.ProbeView{CheckoutID: "building-id"}},
	}
	probe := func(_ context.Context, path string) (daemon.TrackReadiness, error) {
		return states[path], nil
	}

	initial := monitor.snapshot(context.Background(), probe)
	assert.Equal(t, startupViewReadiness{Expected: 3, Ready: 1, Building: 2}, initial,
		"a Git root assigned to the exact lane must not disappear because its catalog row is temporarily unmatched")

	// Once frozen, a legacy-looking answer cannot silently shrink the cohort.
	states["/building"] = daemon.TrackReadiness{State: daemon.TrackReadinessLegacy}
	monitor.observe(indexer.ModeTransitionEvent{CheckoutID: "building-id", Failed: true})
	for range 100 {
		monitor.observe(indexer.ModeTransitionEvent{CheckoutID: "unrelated", Failed: false})
	}
	assert.Len(t, monitor.changed, 1, "transition bursts must coalesce to one refresh edge")
	failed := monitor.snapshot(context.Background(), probe)
	assert.Equal(t, startupViewReadiness{Expected: 3, Ready: 1, Building: 1, Failed: 1}, failed)

	monitor.observe(indexer.ModeTransitionEvent{CheckoutID: "building-id", Failed: false})
	states["/building"] = daemon.TrackReadiness{
		State: daemon.TrackReadinessReady,
		View:  &daemon.ProbeView{CheckoutID: "building-id", Exact: true},
	}
	states["/catalog-gap"] = daemon.TrackReadiness{
		State: daemon.TrackReadinessReady,
		View:  &daemon.ProbeView{CheckoutID: "catalog-gap-id", Exact: true},
	}
	complete := monitor.snapshot(context.Background(), probe)
	assert.Equal(t, startupViewReadiness{Expected: 3, Ready: 3}, complete)
	assert.True(t, complete.complete())
}

func TestStartupViewReadinessMonitorProbeErrorRemainsTransient(t *testing.T) {
	monitor := newStartupViewReadinessMonitor([]string{"/broken"})
	snapshot := monitor.snapshot(context.Background(), func(context.Context, string) (daemon.TrackReadiness, error) {
		return daemon.TrackReadiness{}, errors.New("catalog unavailable")
	})
	assert.Equal(t, startupViewReadiness{Expected: 1, Building: 1, ProbeErrors: 1}, snapshot)
	assert.False(t, snapshot.terminal(), "one failed probe is not an authoritative promotion failure")
}

func TestStartupViewReadinessWatcherRetriesProbeWithoutTransitionEdge(t *testing.T) {
	monitor := newStartupViewReadinessMonitor([]string{"/repo"})
	monitor.retryInitial = time.Millisecond
	monitor.retryMax = 4 * time.Millisecond
	var calls atomic.Int32
	probe := func(context.Context, string) (daemon.TrackReadiness, error) {
		if calls.Add(1) < 3 {
			return daemon.TrackReadiness{}, errors.New("catalog temporarily busy")
		}
		return daemon.TrackReadiness{
			State: daemon.TrackReadinessReady,
			View:  &daemon.ProbeView{CheckoutID: "checkout-1", Exact: true},
		}, nil
	}
	controller := &realController{}
	controller.setStartupViewReadiness(monitor.snapshot(context.Background(), probe))
	controller.MarkEnriched(time.Second)
	monitor.onConfirmedComplete(func() {})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		watchStartupViewReadiness(ctx, nil, controller, monitor, probe)
	}()
	monitor.finishInitialSnapshot()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("bounded retry did not recover a transient probe without an observer edge")
	}
	require.GreaterOrEqual(t, calls.Load(), int32(3))
	require.True(t, controller.IsReady())
	require.True(t, controller.IsEnriched())
}

func TestStartupViewReadinessWatcherStopsRetryTimerOnCancellation(t *testing.T) {
	monitor := newStartupViewReadinessMonitor([]string{"/repo"})
	monitor.retryInitial = time.Millisecond
	monitor.retryMax = 2 * time.Millisecond
	var calls atomic.Int32
	probe := func(context.Context, string) (daemon.TrackReadiness, error) {
		calls.Add(1)
		return daemon.TrackReadiness{}, errors.New("catalog unavailable")
	}
	controller := &realController{}
	controller.setStartupViewReadiness(monitor.snapshot(context.Background(), probe))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		watchStartupViewReadiness(ctx, nil, controller, monitor, probe)
	}()
	monitor.finishInitialSnapshot()
	require.Eventually(t, func() bool { return calls.Load() >= 3 }, time.Second, time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("readiness watcher did not stop after cancellation")
	}
	stoppedAt := calls.Load()
	time.Sleep(10 * time.Millisecond)
	require.Equal(t, stoppedAt, calls.Load(), "retry timer fired after watcher shutdown")
}

func TestStartupViewReadinessEarlyCompletionWaitsForJoinedCallback(t *testing.T) {
	monitor := newStartupViewReadinessMonitor(nil)
	controller := &realController{}
	controller.setStartupViewReadiness(monitor.snapshot(context.Background(), func(context.Context, string) (daemon.TrackReadiness, error) {
		return daemon.TrackReadiness{}, nil
	}))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		watchStartupViewReadiness(ctx, nil, controller, monitor, func(context.Context, string) (daemon.TrackReadiness, error) {
			return daemon.TrackReadiness{}, nil
		})
	}()
	monitor.finishInitialSnapshot()
	require.Eventually(t, func() bool {
		monitor.mu.Lock()
		defer monitor.mu.Unlock()
		return monitor.confirmed
	}, time.Second, time.Millisecond)
	select {
	case <-done:
		t.Fatal("watcher exited before the post-publication callback was registered")
	default:
	}
	callbackRan := make(chan struct{})
	monitor.onConfirmedComplete(func() { close(callbackRan) })
	select {
	case <-callbackRan:
	case <-time.After(time.Second):
		t.Fatal("joined watcher did not run the retained completion callback")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("watcher did not exit after running the completion callback")
	}
}

func TestStartupViewReadinessWaitTerminalIsLevelTriggered(t *testing.T) {
	monitor := newStartupViewReadinessMonitor([]string{"/repo"})
	state := daemon.TrackReadiness{
		State: daemon.TrackReadinessBuilding,
		View:  &daemon.ProbeView{CheckoutID: "checkout-1"},
	}
	probe := func(context.Context, string) (daemon.TrackReadiness, error) { return state, nil }
	require.Equal(t, startupViewReadiness{Expected: 1, Building: 1},
		monitor.snapshot(context.Background(), probe))

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	done := make(chan startupViewReadiness, 1)
	go func() {
		terminal, _ := monitor.waitTerminal(ctx)
		done <- terminal
	}()
	select {
	case <-done:
		t.Fatal("building cohort released the terminal waiter")
	case <-time.After(10 * time.Millisecond):
	}

	state = daemon.TrackReadiness{
		State: daemon.TrackReadinessReady,
		View:  &daemon.ProbeView{CheckoutID: "checkout-1", Exact: true},
	}
	ready := monitor.snapshot(context.Background(), probe)
	monitor.onConfirmedComplete(func() {})
	monitor.confirmComplete(ready)
	select {
	case terminal := <-done:
		require.Equal(t, startupViewReadiness{Expected: 1, Ready: 1}, terminal)
	case <-ctx.Done():
		t.Fatal("confirmed ready cohort did not release the terminal waiter")
	}
}

func TestStartupViewReadinessWaitTerminalEmptyCohortIsImmediate(t *testing.T) {
	monitor := newStartupViewReadinessMonitor(nil)
	empty := monitor.snapshot(context.Background(), func(context.Context, string) (daemon.TrackReadiness, error) {
		t.Fatal("empty cohort must not probe")
		return daemon.TrackReadiness{}, nil
	})
	monitor.onConfirmedComplete(func() {})
	monitor.confirmComplete(empty)
	terminal, err := monitor.waitTerminal(context.Background())
	require.NoError(t, err)
	require.True(t, terminal.complete())
}

func TestStartupViewReadinessWaitTerminalReturnsDurableFailure(t *testing.T) {
	monitor := newStartupViewReadinessMonitor([]string{"/failed"})
	failed := monitor.snapshot(context.Background(), func(context.Context, string) (daemon.TrackReadiness, error) {
		return daemon.TrackReadiness{State: daemon.TrackReadinessFailed}, nil
	})
	require.Equal(t, startupViewReadiness{Expected: 1, Failed: 1}, failed)
	terminal, err := monitor.waitTerminal(context.Background())
	require.NoError(t, err)
	require.Equal(t, startupViewReadiness{Expected: 1, Failed: 1}, terminal)
	require.False(t, terminal.complete())
}

func TestStartupViewReadinessWaitTerminalRunsFinalizerFirst(t *testing.T) {
	monitor := newStartupViewReadinessMonitor(nil)
	ready := monitor.snapshot(context.Background(), func(context.Context, string) (daemon.TrackReadiness, error) {
		return daemon.TrackReadiness{}, nil
	})
	callbackStarted := make(chan struct{})
	releaseCallback := make(chan struct{})
	monitor.onConfirmedComplete(func() {
		close(callbackStarted)
		<-releaseCallback
	})
	confirmed := make(chan struct{})
	go func() {
		monitor.confirmComplete(ready)
		close(confirmed)
	}()
	<-callbackStarted

	waitDone := make(chan struct{})
	go func() {
		_, _ = monitor.waitTerminal(context.Background())
		close(waitDone)
	}()
	select {
	case <-waitDone:
		t.Fatal("terminal waiter escaped before post-publication finalization")
	case <-time.After(10 * time.Millisecond):
	}
	close(releaseCallback)
	select {
	case <-confirmed:
	case <-time.After(time.Second):
		t.Fatal("completion confirmation did not finish")
	}
	select {
	case <-waitDone:
	case <-time.After(time.Second):
		t.Fatal("terminal waiter did not release after finalization")
	}
}

func TestStartupViewReadinessWaitTerminalRespectsCancellation(t *testing.T) {
	monitor := newStartupViewReadinessMonitor([]string{"/building"})
	monitor.snapshot(context.Background(), func(context.Context, string) (daemon.TrackReadiness, error) {
		return daemon.TrackReadiness{State: daemon.TrackReadinessBuilding}, nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := monitor.waitTerminal(ctx)
	require.ErrorIs(t, err, context.Canceled)
}

func TestStartupViewReadinessBackfillsCheckoutIDAfterFreeze(t *testing.T) {
	monitor := newStartupViewReadinessMonitor([]string{"/late-catalog-row"})
	var mu sync.RWMutex
	current := trackViewBuilding("", "", "catalog identity pending")
	probe := func(context.Context, string) (daemon.TrackReadiness, error) {
		mu.RLock()
		defer mu.RUnlock()
		return current, nil
	}
	controller := &realController{}
	controller.setStartupViewReadiness(monitor.snapshot(context.Background(), probe))
	controller.MarkReady(time.Second)
	monitor.onConfirmedComplete(func() {})
	require.False(t, controller.IsReady())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		watchStartupViewReadiness(ctx, nil, controller, monitor, probe)
	}()
	monitor.finishInitialSnapshot()

	mu.Lock()
	current = daemon.TrackReadiness{
		State: daemon.TrackReadinessReady,
		View:  &daemon.ProbeView{CheckoutID: "late-checkout", Exact: true},
	}
	mu.Unlock()
	// The ID was unknown when the cohort froze. Its first real outcome must
	// still wake the monitor; the confirming probe then backfills the ID.
	monitor.observe(indexer.ModeTransitionEvent{CheckoutID: "late-checkout"})
	require.Eventually(t, controller.IsReady, time.Second, time.Millisecond)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("watcher did not finish after backfilling the checkout identity")
	}
	monitor.mu.Lock()
	assert.Equal(t, "late-checkout", monitor.checkoutIDs["/late-catalog-row"])
	monitor.mu.Unlock()
}

func TestStartupViewReadinessWatcherFlipsReadyOnTransitionEdge(t *testing.T) {
	monitor := newStartupViewReadinessMonitor([]string{"/repo"})
	var mu sync.RWMutex
	current := daemon.TrackReadiness{
		State: daemon.TrackReadinessBuilding,
		View:  &daemon.ProbeView{CheckoutID: "checkout-1"},
	}
	probe := func(context.Context, string) (daemon.TrackReadiness, error) {
		mu.RLock()
		defer mu.RUnlock()
		return current, nil
	}
	controller := &realController{}
	controller.setStartupViewReadiness(monitor.snapshot(context.Background(), probe))
	controller.MarkEnriched(time.Second)
	monitor.onConfirmedComplete(func() {})
	require.False(t, controller.IsReady())

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		watchStartupViewReadiness(ctx, nil, controller, monitor, probe)
	}()
	monitor.finishInitialSnapshot()
	mu.Lock()
	current = daemon.TrackReadiness{
		State: daemon.TrackReadinessReady,
		View:  &daemon.ProbeView{CheckoutID: "checkout-1", Exact: true},
	}
	mu.Unlock()
	monitor.observe(indexer.ModeTransitionEvent{CheckoutID: "checkout-1"})
	require.Eventually(t, controller.IsReady, time.Second, time.Millisecond)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("startup readiness watcher did not stop after completing its frozen cohort")
	}
	cancel()
}

func TestStartupViewReadinessInitialSnapshotRetainsRacingTransitionEdges(t *testing.T) {
	monitor := newStartupViewReadinessMonitor([]string{"/before", "/during"})
	controller := &realController{}
	monitor.onConfirmedComplete(func() {})
	var statesMu sync.RWMutex
	states := map[string]daemon.TrackReadiness{
		"/before": {
			State: daemon.TrackReadinessBuilding,
			View:  &daemon.ProbeView{CheckoutID: "before-id"},
		},
		"/during": {
			State: daemon.TrackReadinessBuilding,
			View:  &daemon.ProbeView{CheckoutID: "during-id"},
		},
	}
	duringProbe := make(chan struct{})
	releaseProbe := make(chan struct{})
	var blockOnce sync.Once
	probe := func(ctx context.Context, path string) (daemon.TrackReadiness, error) {
		if path == "/during" {
			blockOnce.Do(func() {
				close(duringProbe)
				select {
				case <-releaseProbe:
				case <-ctx.Done():
				}
			})
		}
		statesMu.RLock()
		defer statesMu.RUnlock()
		return states[path], nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		watchStartupViewReadiness(ctx, nil, controller, monitor, probe)
	}()
	select {
	case <-monitor.watching:
	case <-time.After(time.Second):
		t.Fatal("startup watcher did not reach its initialization barrier")
	}

	// This edge predates the first snapshot. It may fill the coalescing channel,
	// but the watcher must remain behind initialized and cannot freeze the cohort.
	monitor.observe(indexer.ModeTransitionEvent{CheckoutID: "before-id", Failed: true})
	monitor.mu.Lock()
	assert.False(t, monitor.frozen, "a pre-Seed event must not freeze the startup cohort")
	monitor.mu.Unlock()

	initialDone := make(chan startupViewReadiness, 1)
	go func() {
		initialDone <- monitor.snapshot(ctx, probe)
	}()
	select {
	case <-duringProbe:
	case <-time.After(time.Second):
		t.Fatal("initial snapshot did not reach deterministic probe barrier")
	}
	monitor.observe(indexer.ModeTransitionEvent{CheckoutID: "during-id", Failed: true})
	monitor.mu.Lock()
	assert.False(t, monitor.frozen, "an event during the initial probe must not publish a partial freeze")
	monitor.mu.Unlock()
	close(releaseProbe)
	initial := <-initialDone
	controller.setStartupViewReadiness(initial)
	controller.MarkEnriched(time.Second)
	monitor.finishInitialSnapshot()

	require.Eventually(t, func() bool {
		return controller.startupViewReadiness().Failed == 2
	}, time.Second, time.Millisecond, "the confirming snapshot must retain both racing failure edges")
	require.False(t, controller.IsReady())

	statesMu.Lock()
	states["/before"] = daemon.TrackReadiness{
		State: daemon.TrackReadinessReady,
		View:  &daemon.ProbeView{CheckoutID: "before-id", Exact: true},
	}
	states["/during"] = daemon.TrackReadiness{
		State: daemon.TrackReadinessReady,
		View:  &daemon.ProbeView{CheckoutID: "during-id", Exact: true},
	}
	statesMu.Unlock()
	monitor.observe(indexer.ModeTransitionEvent{CheckoutID: "before-id"})
	monitor.observe(indexer.ModeTransitionEvent{CheckoutID: "during-id"})
	require.Eventually(t, controller.IsReady, time.Second, time.Millisecond,
		"matching completions must re-evaluate the frozen cohort")
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("watcher did not stop after both matching completions became exact")
	}
	cancel()
}

func BenchmarkStartupViewReadinessSnapshot256(b *testing.B) {
	paths := make([]string, 256)
	states := make(map[string]daemon.TrackReadiness, len(paths))
	for i := range paths {
		path := fmt.Sprintf("/repo/%03d", i)
		paths[i] = path
		states[path] = daemon.TrackReadiness{
			State: daemon.TrackReadinessReady,
			View:  &daemon.ProbeView{CheckoutID: fmt.Sprintf("checkout-%03d", i), Exact: true},
		}
	}
	monitor := newStartupViewReadinessMonitor(paths)
	probe := func(_ context.Context, path string) (daemon.TrackReadiness, error) {
		return states[path], nil
	}
	require.True(b, monitor.snapshot(context.Background(), probe).complete())

	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if snapshot := monitor.snapshot(context.Background(), probe); !snapshot.complete() {
			b.Fatal("ready cohort regressed")
		}
	}
}

func BenchmarkStartupViewReadinessWaitTerminal256(b *testing.B) {
	paths := make([]string, 256)
	states := make(map[string]daemon.TrackReadiness, len(paths))
	for i := range paths {
		path := fmt.Sprintf("/repo/%03d", i)
		paths[i] = path
		states[path] = daemon.TrackReadiness{
			State: daemon.TrackReadinessReady,
			View:  &daemon.ProbeView{CheckoutID: fmt.Sprintf("checkout-%03d", i), Exact: true},
		}
	}
	monitor := newStartupViewReadinessMonitor(paths)
	ready := monitor.snapshot(context.Background(), func(_ context.Context, path string) (daemon.TrackReadiness, error) {
		return states[path], nil
	})
	monitor.onConfirmedComplete(func() {})
	monitor.confirmComplete(ready)

	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		terminal, err := monitor.waitTerminal(context.Background())
		if err != nil || !terminal.complete() {
			b.Fatalf("terminal=%+v err=%v", terminal, err)
		}
	}
}

var startupViewReadinessRetrySink time.Duration

func BenchmarkStartupViewReadinessRetryBackoff(b *testing.B) {
	var delay time.Duration
	b.ReportAllocs()
	for range b.N {
		delay = 250 * time.Millisecond
		for range 8 {
			delay = nextStartupViewReadinessRetry(delay, 15*time.Second)
		}
	}
	startupViewReadinessRetrySink = delay
}
