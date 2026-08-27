package store_sqlite

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func testProducerWithdrawalConfig() producerWithdrawalConfig {
	return producerWithdrawalConfig{
		attemptTimeout: 50 * time.Millisecond,
		initialBackoff: time.Millisecond,
		maxBackoff:     8 * time.Millisecond,
		shutdownBudget: 50 * time.Millisecond,
	}
}

func transientProducerWithdrawal(
	context.Context,
	int64,
	string,
	error,
) (producerWithdrawalDisposition, error) {
	return producerWithdrawalTransient, nil
}

func waitForProducerWithdrawals(t testing.TB, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for producer withdrawals")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestProducerWithdrawalRetriesTransientContention(t *testing.T) {
	var calls atomic.Int32
	manager := newProducerWithdrawalManager(
		func(context.Context, int64, string, string) error {
			if calls.Add(1) < 3 {
				return context.DeadlineExceeded
			}
			return nil
		},
		transientProducerWithdrawal,
		testProducerWithdrawalConfig(),
	)
	defer manager.close()

	if !manager.schedule(41, "source_snapshot", "missing blob") {
		t.Fatal("schedule rejected")
	}
	waitForProducerWithdrawals(t, func() bool { return manager.pending() == 0 })
	if got := calls.Load(); got != 3 {
		t.Fatalf("withdrawal calls = %d, want 3", got)
	}
}

func TestProducerWithdrawalSharesOneContextAcrossWithdrawAndClassification(t *testing.T) {
	failure := errors.New("stale writer snapshot")
	var withdrawCtx context.Context
	var classifyCtx context.Context
	manager := newProducerWithdrawalManager(
		func(ctx context.Context, _ int64, _, _ string) error {
			withdrawCtx = ctx
			return failure
		},
		func(ctx context.Context, _ int64, _ string, got error) (producerWithdrawalDisposition, error) {
			classifyCtx = ctx
			if !errors.Is(got, failure) {
				t.Fatalf("classification error = %v, want %v", got, failure)
			}
			return producerWithdrawalSatisfied, nil
		},
		testProducerWithdrawalConfig(),
	)
	defer manager.close()

	manager.schedule(1, "source_snapshot", "shared context")
	waitForProducerWithdrawals(t, func() bool { return manager.pending() == 0 })
	if withdrawCtx == nil || withdrawCtx != classifyCtx {
		t.Fatalf("withdraw/classify contexts differ: %p != %p", withdrawCtx, classifyCtx)
	}
}

func TestProducerWithdrawalCoalescesWithFirstReasonAndTombstone(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	var gotReason atomic.Value
	manager := newProducerWithdrawalManager(
		func(ctx context.Context, _ int64, _ string, reason string) error {
			gotReason.Store(reason)
			if calls.Add(1) == 1 {
				close(started)
			}
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		},
		transientProducerWithdrawal,
		testProducerWithdrawalConfig(),
	)
	defer manager.close()

	if !manager.schedule(7, "source_snapshot", "first") {
		t.Fatal("first schedule rejected")
	}
	<-started
	for i := 0; i < 100; i++ {
		if !manager.schedule(7, "source_snapshot", "last") {
			t.Fatal("duplicate schedule rejected")
		}
	}
	if got := manager.pending(); got != 1 {
		t.Fatalf("pending = %d, want 1", got)
	}
	close(release)
	waitForProducerWithdrawals(t, func() bool { return manager.pending() == 0 })
	if !manager.schedule(7, "source_snapshot", "after completion") {
		t.Fatal("completed immutable key was not accepted as a no-op")
	}
	time.Sleep(5 * time.Millisecond)
	if got := calls.Load(); got != 1 {
		t.Fatalf("withdrawal calls = %d, want tombstoned single call", got)
	}
	if got, _ := gotReason.Load().(string); got != "first" {
		t.Fatalf("withdrawal reason = %q, want first", got)
	}
}

func TestProducerWithdrawalUsesFirstNonEmptyReason(t *testing.T) {
	manager := newInertProducerWithdrawalManager()
	key := producerWithdrawalKey{generationID: 8, producer: "source_snapshot"}

	manager.schedule(key.generationID, key.producer, "")
	manager.schedule(key.generationID, key.producer, "first non-empty")
	manager.schedule(key.generationID, key.producer, "later duplicate")

	manager.mu.Lock()
	got := manager.tasks[key].reason
	manager.mu.Unlock()
	if got != "first non-empty" {
		t.Fatalf("coalesced reason = %q, want first non-empty", got)
	}
}

func TestProducerWithdrawalKeepsDistinctKeys(t *testing.T) {
	var mu sync.Mutex
	seen := make(map[string]bool)
	manager := newProducerWithdrawalManager(
		func(_ context.Context, generationID int64, producer, _ string) error {
			mu.Lock()
			seen[fmt.Sprintf("%d/%s", generationID, producer)] = true
			mu.Unlock()
			return nil
		},
		nil,
		testProducerWithdrawalConfig(),
	)
	defer manager.close()

	manager.schedule(1, "source_snapshot", "one")
	manager.schedule(1, "structural", "two")
	manager.schedule(2, "source_snapshot", "three")
	waitForProducerWithdrawals(t, func() bool { return manager.pending() == 0 })

	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 3 {
		t.Fatalf("distinct withdrawals = %d, want 3: %v", len(seen), seen)
	}
}

func TestProducerWithdrawalTreatsAbsentProducerAsSatisfied(t *testing.T) {
	withdrawErr := errors.New("stale catalog generation")
	var calls atomic.Int32
	var checks atomic.Int32
	manager := newProducerWithdrawalManager(
		func(context.Context, int64, string, string) error {
			calls.Add(1)
			return withdrawErr
		},
		func(context.Context, int64, string, error) (producerWithdrawalDisposition, error) {
			checks.Add(1)
			return producerWithdrawalSatisfied, nil
		},
		testProducerWithdrawalConfig(),
	)
	defer manager.close()

	manager.schedule(9, "source_snapshot", "pruned tree")
	waitForProducerWithdrawals(t, func() bool { return manager.pending() == 0 })
	if calls.Load() != 1 || checks.Load() != 1 {
		t.Fatalf("calls/checks = %d/%d, want 1/1", calls.Load(), checks.Load())
	}
}

func TestProducerWithdrawalTransientBackoffIsExponential(t *testing.T) {
	var calls atomic.Int32
	events := make(chan producerWithdrawalEvent, 4)
	config := testProducerWithdrawalConfig()
	config.observe = func(event producerWithdrawalEvent) { events <- event }
	manager := newProducerWithdrawalManager(
		func(context.Context, int64, string, string) error {
			if calls.Add(1) <= 3 {
				return errors.New("busy")
			}
			return nil
		},
		transientProducerWithdrawal,
		config,
	)
	defer manager.close()

	manager.schedule(10, "source_snapshot", "transient")
	waitForProducerWithdrawals(t, func() bool { return manager.pending() == 0 })
	wantBackoff := []time.Duration{time.Millisecond, 2 * time.Millisecond, 4 * time.Millisecond}
	for i, want := range wantBackoff {
		event := <-events
		if event.Disposition != producerWithdrawalTransient || event.Backoff != want || event.Final {
			t.Fatalf("event %d = %+v, want transient backoff %v", i, event, want)
		}
	}
	if event := <-events; event.Disposition != producerWithdrawalSatisfied || !event.Final {
		t.Fatalf("final event = %+v, want satisfied", event)
	}
}

func TestProducerWithdrawalPersistentUsesMaxBackoffAndMetadata(t *testing.T) {
	events := make(chan producerWithdrawalEvent, 16)
	config := testProducerWithdrawalConfig()
	config.maxBackoff = 25 * time.Millisecond
	config.observe = func(event producerWithdrawalEvent) { events <- event }
	manager := newProducerWithdrawalManager(
		func(context.Context, int64, string, string) error { return errors.New("catalog invariant") },
		func(context.Context, int64, string, error) (producerWithdrawalDisposition, error) {
			return producerWithdrawalPersistent, nil
		},
		config,
	)
	defer manager.close()

	manager.schedule(11, "source_snapshot", "persistent")
	event := <-events
	if event.Disposition != producerWithdrawalPersistent || event.Backoff != config.maxBackoff || event.Final {
		t.Fatalf("persistent event = %+v, want max-backoff retry metadata", event)
	}
}

func TestProducerWithdrawalPersistentKeyDoesNotStarveReadyKeys(t *testing.T) {
	firstHot := make(chan struct{})
	coldDone := make(chan string, 2)
	var hotOnce sync.Once
	config := testProducerWithdrawalConfig()
	config.maxBackoff = 25 * time.Millisecond
	manager := newProducerWithdrawalManager(
		func(_ context.Context, _ int64, producer, _ string) error {
			if producer == "hot" {
				hotOnce.Do(func() { close(firstHot) })
				return errors.New("persistent")
			}
			coldDone <- producer
			return nil
		},
		func(_ context.Context, _ int64, producer string, _ error) (producerWithdrawalDisposition, error) {
			if producer == "hot" {
				return producerWithdrawalPersistent, nil
			}
			return producerWithdrawalTransient, nil
		},
		config,
	)
	defer manager.close()

	manager.schedule(1, "hot", "first")
	<-firstHot
	manager.schedule(1, "cold-a", "second")
	manager.schedule(1, "cold-b", "third")
	for i := 0; i < 2; i++ {
		select {
		case <-coldDone:
		case <-time.After(20 * time.Millisecond):
			t.Fatal("persistent key starved a ready key")
		}
	}
}

func TestProducerWithdrawalCloseImmediatelyFlushesCanceledPersistentAttempt(t *testing.T) {
	started := make(chan struct{})
	shutdownAttempt := make(chan struct{}, 1)
	events := make(chan producerWithdrawalEvent, 4)
	var calls atomic.Int32
	config := testProducerWithdrawalConfig()
	config.attemptTimeout = time.Hour
	config.maxBackoff = time.Hour
	config.shutdownBudget = 500 * time.Millisecond
	config.observe = func(event producerWithdrawalEvent) { events <- event }
	manager := newProducerWithdrawalManager(
		func(ctx context.Context, _ int64, _, _ string) error {
			if calls.Add(1) == 1 {
				close(started)
				<-ctx.Done()
				return errors.New("normal attempt canceled")
			}
			shutdownAttempt <- struct{}{}
			return errors.New("shutdown attempt failed")
		},
		func(context.Context, int64, string, error) (producerWithdrawalDisposition, error) {
			return producerWithdrawalPersistent, nil
		},
		config,
	)
	manager.schedule(12, "source_snapshot", "close handoff")
	<-started

	closed := make(chan struct{})
	go func() {
		manager.close()
		close(closed)
	}()
	select {
	case <-shutdownAttempt:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("canceled persistent attempt waited for normal backoff instead of immediate shutdown flush")
	}
	select {
	case <-closed:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("failed shutdown attempt was requeued instead of finalized")
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("attempt calls = %d, want one normal plus one shutdown", got)
	}
	first := <-events
	second := <-events
	if first.Shutdown || first.Final || first.Backoff != 0 {
		t.Fatalf("normal handoff event = %+v, want immediate non-final retry", first)
	}
	if !second.Shutdown || !second.Final || second.Backoff != 0 {
		t.Fatalf("shutdown event = %+v, want final without requeue", second)
	}
}

func TestProducerWithdrawalCloseCancelsActiveAndUsesOneShutdownDeadline(t *testing.T) {
	firstStarted := make(chan context.Context, 1)
	finalContext := make(chan context.Context, 1)
	var calls atomic.Int32
	manager := newProducerWithdrawalManager(
		func(ctx context.Context, _ int64, _, _ string) error {
			if calls.Add(1) == 1 {
				firstStarted <- ctx
				<-ctx.Done()
				return ctx.Err()
			}
			finalContext <- ctx
			return nil
		},
		transientProducerWithdrawal,
		testProducerWithdrawalConfig(),
	)
	manager.schedule(12, "source_snapshot", "close")
	first := <-firstStarted

	closed := make(chan struct{})
	go func() {
		manager.close()
		close(closed)
	}()
	select {
	case <-first.Done():
	case <-time.After(50 * time.Millisecond):
		t.Fatal("Close did not cancel active normal attempt")
	}
	var final context.Context
	select {
	case final = <-finalContext:
	case <-time.After(50 * time.Millisecond):
		t.Fatal("final flush did not retry under shutdown context")
	}
	if _, ok := final.Deadline(); !ok {
		t.Fatal("final flush context has no shared shutdown deadline")
	}
	select {
	case <-closed:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("Close did not join worker")
	}
}

func TestProducerWithdrawalCloseFlushesThenRejectsAdmission(t *testing.T) {
	called := make(chan struct{}, 1)
	manager := newProducerWithdrawalManager(
		func(context.Context, int64, string, string) error {
			called <- struct{}{}
			return nil
		},
		nil,
		testProducerWithdrawalConfig(),
	)
	if !manager.schedule(13, "source_snapshot", "close") {
		t.Fatal("schedule rejected before close")
	}
	manager.close()
	select {
	case <-called:
	default:
		t.Fatal("Close returned before queued withdrawal was flushed")
	}
	if manager.schedule(14, "source_snapshot", "after close") {
		t.Fatal("schedule accepted after Close")
	}
}

func TestProducerWithdrawalCloseIsBoundedAndClearsTasks(t *testing.T) {
	config := testProducerWithdrawalConfig()
	config.attemptTimeout = 15 * time.Millisecond
	config.shutdownBudget = 30 * time.Millisecond
	manager := newProducerWithdrawalManager(
		func(ctx context.Context, _ int64, _, _ string) error {
			<-ctx.Done()
			return ctx.Err()
		},
		transientProducerWithdrawal,
		config,
	)
	manager.schedule(15, "source_snapshot", "persistent contention")
	started := time.Now()
	manager.close()
	if elapsed := time.Since(started); elapsed > 150*time.Millisecond {
		t.Fatalf("Close took %v, want bounded shutdown", elapsed)
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if len(manager.tasks) != 0 {
		t.Fatalf("tasks retained after final snapshot: %d", len(manager.tasks))
	}
}

func TestProducerWithdrawalCloseClearsCompletedTombstones(t *testing.T) {
	manager := newProducerWithdrawalManager(
		func(context.Context, int64, string, string) error { return nil },
		nil,
		testProducerWithdrawalConfig(),
	)
	manager.schedule(16, "source_snapshot", "complete")
	waitForProducerWithdrawals(t, func() bool { return manager.pending() == 0 })

	manager.mu.Lock()
	before := len(manager.completed)
	manager.mu.Unlock()
	if before != 1 {
		t.Fatalf("completed tombstones before Close = %d, want 1", before)
	}
	manager.close()
	manager.mu.Lock()
	after := len(manager.completed)
	manager.mu.Unlock()
	if after != 0 {
		t.Fatalf("completed tombstones after Close = %d, want 0", after)
	}
}

func TestProducerWithdrawalConcurrentCloseIsIdempotent(t *testing.T) {
	manager := newProducerWithdrawalManager(
		func(context.Context, int64, string, string) error { return nil },
		nil,
		testProducerWithdrawalConfig(),
	)
	manager.schedule(16, "source_snapshot", "concurrent close")

	const closers = 32
	var wg sync.WaitGroup
	wg.Add(closers)
	for i := 0; i < closers; i++ {
		go func() {
			defer wg.Done()
			manager.close()
		}()
	}
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("concurrent Close calls did not converge")
	}
	if manager.schedule(17, "source_snapshot", "closed") {
		t.Fatal("admission reopened after concurrent Close")
	}
}

func newInertProducerWithdrawalManager() *producerWithdrawalManager {
	return &producerWithdrawalManager{
		config:    testProducerWithdrawalConfig().normalized(),
		tasks:     make(map[producerWithdrawalKey]*producerWithdrawalTask),
		completed: make(map[producerWithdrawalKey]struct{}),
		accept:    true,
		wake:      make(chan struct{}, 1),
		done:      make(chan struct{}),
	}
}

func resetInertProducerWithdrawalManager(manager *producerWithdrawalManager) {
	manager.mu.Lock()
	manager.tasks = make(map[producerWithdrawalKey]*producerWithdrawalTask)
	manager.mu.Unlock()
	select {
	case <-manager.wake:
	default:
	}
}

func BenchmarkProducerWithdrawalEnqueue(b *testing.B) {
	manager := newInertProducerWithdrawalManager()
	b.ReportAllocs()
	b.ResetTimer()
	for completed := 0; completed < b.N; {
		batch := b.N - completed
		if batch > 4096 {
			batch = 4096
		}
		for i := 0; i < batch; i++ {
			manager.schedule(int64(completed+i+1), "source_snapshot", "benchmark")
		}
		completed += batch
		b.StopTimer()
		resetInertProducerWithdrawalManager(manager)
		b.StartTimer()
	}
	b.StopTimer()
}

func BenchmarkProducerWithdrawalCoalesce(b *testing.B) {
	manager := newInertProducerWithdrawalManager()
	manager.schedule(1, "source_snapshot", "first")
	select {
	case <-manager.wake:
	default:
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		manager.schedule(1, "source_snapshot", "ignored")
	}
}

func BenchmarkProducerWithdrawalDrain(b *testing.B) {
	completed := make(chan struct{}, 1)
	config := testProducerWithdrawalConfig()
	config.observe = func(event producerWithdrawalEvent) {
		if event.Final {
			completed <- struct{}{}
		}
	}
	manager := newProducerWithdrawalManager(
		func(context.Context, int64, string, string) error { return nil },
		nil,
		config,
	)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		manager.schedule(int64(i+1), "source_snapshot", "benchmark")
		<-completed
	}
	b.StopTimer()
	manager.close()
}
