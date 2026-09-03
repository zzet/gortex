package indexer

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

type boundedGateAcquireResult struct {
	name    string
	release func()
	err     error
}

func awaitBoundedGateStats(
	tb testing.TB,
	gate *ViewBuildGate,
	predicate func(ViewBuildGateStats) bool,
) ViewBuildGateStats {
	tb.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		stats := gate.Stats()
		if predicate(stats) {
			return stats
		}
		if time.Now().After(deadline) {
			tb.Fatalf("timed out waiting for view-build gate state; last=%+v", stats)
		}
		runtime.Gosched()
	}
}

func TestViewBuildGateRejectsAtCapacityAndRetries(t *testing.T) {
	gate := newViewBuildGateWithLimits(1, 1)
	gate.Open()
	hold, err := gate.Acquire(context.Background(), ViewBuildBackground)
	if err != nil {
		t.Fatal(err)
	}
	defer hold()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	acquired := make(chan boundedGateAcquireResult, 1)
	go func() {
		release, err := gate.Acquire(ctx, ViewBuildBackground)
		acquired <- boundedGateAcquireResult{release: release, err: err}
	}()
	awaitBoundedGateStats(t, gate, func(stats ViewBuildGateStats) bool {
		return stats.BackgroundQueued == 1
	})

	release, err := gate.Acquire(context.Background(), ViewBuildBackground)
	if release != nil {
		release()
		t.Fatal("capacity rejection returned a release function")
	}
	if !errors.Is(err, ErrViewBuildQueueFull) {
		t.Fatalf("Acquire() error = %v, want queue-full", err)
	}
	var full *ViewBuildQueueFullError
	if !errors.As(err, &full) || full.Priority != ViewBuildBackground || full.Limit != 1 {
		t.Fatalf("Acquire() typed error = %#v", full)
	}

	hold()
	select {
	case result := <-acquired:
		if result.err != nil {
			t.Fatal(result.err)
		}
		result.release()
	case <-time.After(5 * time.Second):
		t.Fatal("queued acquire was not admitted")
	}
	stats := awaitBoundedGateStats(t, gate, func(stats ViewBuildGateStats) bool {
		return !stats.Active && stats.BackgroundQueued == 0
	})
	if stats.BackgroundHighWater != 1 || stats.RejectedBackground != 1 {
		t.Fatalf("gate stats = %+v", stats)
	}
}

func TestViewBuildGateCancellationFreesCapacity(t *testing.T) {
	gate := newViewBuildGateWithLimits(1, 1)
	gate.Open()
	hold, err := gate.Acquire(context.Background(), ViewBuildBackground)
	if err != nil {
		t.Fatal(err)
	}
	defer hold()

	queuedCtx, cancelQueued := context.WithCancel(context.Background())
	queuedErr := make(chan error, 1)
	go func() {
		_, err := gate.Acquire(queuedCtx, ViewBuildBackground)
		queuedErr <- err
	}()
	awaitBoundedGateStats(t, gate, func(stats ViewBuildGateStats) bool {
		return stats.BackgroundQueued == 1
	})
	cancelQueued()
	if err := <-queuedErr; !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled Acquire() error = %v", err)
	}
	awaitBoundedGateStats(t, gate, func(stats ViewBuildGateStats) bool {
		return stats.BackgroundQueued == 0 && stats.CanceledBackground == 1
	})

	retryCtx, cancelRetry := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelRetry()
	retry := make(chan boundedGateAcquireResult, 1)
	go func() {
		release, err := gate.Acquire(retryCtx, ViewBuildBackground)
		retry <- boundedGateAcquireResult{release: release, err: err}
	}()
	awaitBoundedGateStats(t, gate, func(stats ViewBuildGateStats) bool {
		return stats.BackgroundQueued == 1
	})
	hold()
	result := <-retry
	if result.err != nil {
		t.Fatal(result.err)
	}
	result.release()
	awaitBoundedGateStats(t, gate, func(stats ViewBuildGateStats) bool {
		return !stats.Active && stats.BackgroundQueued == 0
	})
}

func TestViewBuildGateBoundedQueuesPreserveFairness(t *testing.T) {
	gate := newViewBuildGateWithLimits(8, 8)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	results := make(chan boundedGateAcquireResult, 6)
	enqueue := func(name string, priority ViewBuildPriority) {
		go func() {
			release, err := gate.Acquire(ctx, priority)
			results <- boundedGateAcquireResult{name: name, release: release, err: err}
		}()
	}

	enqueue("background", ViewBuildBackground)
	awaitBoundedGateStats(t, gate, func(stats ViewBuildGateStats) bool {
		return stats.BackgroundQueued == 1
	})
	for i := 1; i <= 5; i++ {
		enqueue(fmt.Sprintf("interactive-%d", i), ViewBuildInteractive)
		want := i
		awaitBoundedGateStats(t, gate, func(stats ViewBuildGateStats) bool {
			return stats.InteractiveQueued == want
		})
	}

	gate.Open()
	for _, want := range []string{
		"interactive-1",
		"interactive-2",
		"interactive-3",
		"interactive-4",
		"background",
		"interactive-5",
	} {
		select {
		case result := <-results:
			if result.err != nil {
				t.Fatal(result.err)
			}
			result.release()
			if result.name != want {
				t.Fatalf("admitted %q, want %q", result.name, want)
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	stats := awaitBoundedGateStats(t, gate, func(stats ViewBuildGateStats) bool {
		return !stats.Active && stats.InteractiveQueued == 0 && stats.BackgroundQueued == 0
	})
	if stats.AdmittedInteractive != 5 || stats.AdmittedBackground != 1 {
		t.Fatalf("gate stats = %+v", stats)
	}
}

func TestViewBuildGateNeverAdmitsMoreThanOneBuild(t *testing.T) {
	const callers = 64
	gate := newViewBuildGateWithLimits(callers, callers)
	gate.Open()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	start := make(chan struct{})
	var wg sync.WaitGroup
	var active atomic.Int32
	var maxActive atomic.Int32
	errs := make(chan error, callers)

	for i := 0; i < callers; i++ {
		priority := ViewBuildBackground
		if i%2 == 0 {
			priority = ViewBuildInteractive
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			release, err := gate.Acquire(ctx, priority)
			if err != nil {
				errs <- err
				return
			}
			current := active.Add(1)
			for {
				old := maxActive.Load()
				if current <= old || maxActive.CompareAndSwap(old, current) {
					break
				}
			}
			runtime.Gosched()
			active.Add(-1)
			release()
		}()
	}
	close(start)
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if got := maxActive.Load(); got != 1 {
		t.Fatalf("maximum simultaneously active builds = %d", got)
	}
	stats := gate.Stats()
	if stats.Active || stats.InteractiveQueued != 0 || stats.BackgroundQueued != 0 {
		t.Fatalf("gate stats = %+v", stats)
	}
}

func TestViewBuildGateCanceledWaitersAllExit(t *testing.T) {
	const waiters = 64
	gate := newViewBuildGateWithLimits(waiters, waiters)
	gate.Open()
	hold, err := gate.Acquire(context.Background(), ViewBuildBackground)
	if err != nil {
		t.Fatal(err)
	}
	defer hold()

	cancels := make([]context.CancelFunc, 0, waiters)
	errs := make(chan error, waiters)
	var wg sync.WaitGroup
	for i := 0; i < waiters; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		cancels = append(cancels, cancel)
		wg.Add(1)
		go func(ctx context.Context) {
			defer wg.Done()
			_, err := gate.Acquire(ctx, ViewBuildBackground)
			errs <- err
		}(ctx)
	}
	awaitBoundedGateStats(t, gate, func(stats ViewBuildGateStats) bool {
		return stats.BackgroundQueued == waiters
	})
	for _, cancel := range cancels {
		cancel()
	}
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("canceled waiters did not exit")
	}
	close(errs)
	for err := range errs {
		if !errors.Is(err, context.Canceled) {
			t.Errorf("canceled Acquire() error = %v", err)
		}
	}
	hold()
	stats := awaitBoundedGateStats(t, gate, func(stats ViewBuildGateStats) bool {
		return !stats.Active && stats.BackgroundQueued == 0
	})
	if stats.CanceledBackground != waiters {
		t.Fatalf("gate stats = %+v", stats)
	}
}

func TestViewBuildGateRejectsNegativeLimits(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("newViewBuildGateWithLimits() did not panic")
		}
	}()
	_ = newViewBuildGateWithLimits(-1, 0)
}

func TestRefViewCapacityDeferralReleasesBuildClaim(t *testing.T) {
	f := newRefViewFixture(t)
	commitB, _ := f.commitTree(builderTreeB(), "B")
	f.setRef("refs/heads/feature", commitB)
	viewID := f.viewID("refs/heads/feature")

	gate := newViewBuildGateWithLimits(0, 0)
	gate.Open()
	hold, err := gate.Acquire(context.Background(), ViewBuildBackground)
	if err != nil {
		t.Fatal(err)
	}
	defer hold()
	var builds atomic.Int64
	manager := f.managerTuned(t, func() { builds.Add(1) }, func(cfg *RefViewManagerConfig) {
		cfg.Gate = gate
	})

	deferred, err := manager.EnsureRefView(context.Background(), f.request("refs/heads/feature"))
	if err != nil {
		t.Fatal(err)
	}
	if deferred.State != store_sqlite.RefViewBuilding || deferred.Built || deferred.BuildToken != "" {
		t.Fatalf("deferred result = %+v", deferred)
	}
	if builds.Load() != 0 {
		t.Fatalf("physical builds = %d, want 0", builds.Load())
	}
	rows := f.builds(viewID)
	if len(rows) != 1 || rows[0].State != store_sqlite.ViewGenerationFailed {
		t.Fatalf("build rows after rejection = %+v", rows)
	}

	hold()
	ready, err := manager.EnsureRefView(context.Background(), f.request("refs/heads/feature"))
	if err != nil {
		t.Fatal(err)
	}
	if ready.State != store_sqlite.RefViewReady || !ready.Built || ready.GenerationID == 0 {
		t.Fatalf("ready result = %+v", ready)
	}
	if builds.Load() != 1 {
		t.Fatalf("physical builds = %d, want 1", builds.Load())
	}
	rows = f.builds(viewID)
	failed, complete := 0, 0
	for _, row := range rows {
		switch row.State {
		case store_sqlite.ViewGenerationFailed:
			failed++
		case store_sqlite.ViewGenerationReady:
			complete++
		}
	}
	if len(rows) != 2 || failed != 1 || complete != 1 {
		t.Fatalf("build rows after retry = %+v", rows)
	}
}

func BenchmarkViewBuildGateBoundedAdmission(b *testing.B) {
	for _, callers := range []int{1, 8, 64} {
		b.Run(fmt.Sprintf("callers=%d", callers), func(b *testing.B) {
			const capacity = 8
			b.ReportAllocs()
			var queueHighWater int64
			var rejected int64
			var wait time.Duration
			var samples uint64

			for i := 0; i < b.N; i++ {
				gate := newViewBuildGateWithLimits(capacity, capacity)
				gate.Open()
				hold, err := gate.Acquire(context.Background(), ViewBuildBackground)
				if err != nil {
					b.Fatal(err)
				}
				remaining := callers - 1
				var wg sync.WaitGroup
				unexpected := make(chan error, remaining)
				for j := 0; j < remaining; j++ {
					wg.Add(1)
					go func() {
						defer wg.Done()
						release, err := gate.Acquire(context.Background(), ViewBuildBackground)
						if err != nil {
							if !errors.Is(err, ErrViewBuildQueueFull) {
								unexpected <- err
							}
							return
						}
						release()
					}()
				}
				awaitBoundedGateStats(b, gate, func(stats ViewBuildGateStats) bool {
					return stats.BackgroundQueued+int(stats.RejectedBackground) == remaining
				})
				hold()
				wg.Wait()
				close(unexpected)
				for err := range unexpected {
					b.Fatal(err)
				}
				stats := gate.Stats()
				if stats.BackgroundHighWater > capacity || stats.Active || stats.BackgroundQueued != 0 {
					b.Fatalf("gate stats = %+v", stats)
				}
				queueHighWater += int64(stats.BackgroundHighWater)
				rejected += int64(stats.RejectedBackground)
				wait += stats.TotalWait
				samples += stats.WaitSamples
			}

			b.StopTimer()
			b.ReportMetric(capacity, "queue_capacity")
			b.ReportMetric(float64(queueHighWater)/float64(b.N), "max_queue/op")
			b.ReportMetric(float64(rejected)/float64(b.N), "rejected/op")
			if samples > 0 {
				b.ReportMetric(float64(wait.Nanoseconds())/float64(samples), "avg_wait_ns")
			}
		})
	}
}
