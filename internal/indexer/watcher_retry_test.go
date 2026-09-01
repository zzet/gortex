package indexer

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"go.uber.org/zap"
)

type committedMutationTestError struct{ error }

func (committedMutationTestError) Committed() bool { return true }

func newMutationRetryTestWatcher(t testing.TB) *Watcher {
	t.Helper()
	w, err := NewWatcher(nil, config.WatchConfig{
		DebounceMs:         1,
		StormThreshold:     -1,
		StormQuietPeriodMs: 1,
	}, zap.NewNop())
	if err != nil {
		t.Fatalf("NewWatcher: %v", err)
	}
	w.degradedNoFsnotify = true
	t.Cleanup(func() {
		if !w.mutationAdmissionStopped() {
			if err := w.Stop(); err != nil {
				t.Errorf("Stop: %v", err)
			}
		}
	})
	return w
}

func awaitMutationRetryResult(t *testing.T, ticket *MutationTicket) MutationResult {
	t.Helper()
	select {
	case result := <-ticket.Done:
		return result
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for mutation result")
		return MutationResult{}
	}
}

func TestMutationRetrySurvivesCallerCancellation(t *testing.T) {
	w := newMutationRetryTestWatcher(t)
	requestCtx, cancel := context.WithCancel(context.Background())
	firstStarted := make(chan struct{})
	var attempts atomic.Int32
	w.mutationRetryDelayFn = func(int) time.Duration { return time.Millisecond }
	w.pointMutationPatch = func(string, ChangeKind, uint64) error {
		if attempts.Add(1) == 1 {
			close(firstStarted)
			<-requestCtx.Done()
			return fmt.Errorf("writer gate: %w", context.DeadlineExceeded)
		}
		return nil
	}

	ticket := w.scheduleFileMutation("retry.go", ChangeModified)
	<-firstStarted
	cancel()
	result := awaitMutationRetryResult(t, ticket)
	if !result.Reindexed || result.Err != nil {
		t.Fatalf("retry result = %+v", result)
	}
	if result.AppliedGeneration != ticket.Generation || attempts.Load() != 2 {
		t.Fatalf("generation/attempts = %d/%d, want %d/2", result.AppliedGeneration, attempts.Load(), ticket.Generation)
	}
}

func TestMutationRetryPermanentErrorIsTerminal(t *testing.T) {
	w := newMutationRetryTestWatcher(t)
	permanent := errors.New("parse failed")
	var attempts atomic.Int32
	w.mutationRetryDelayFn = func(int) time.Duration { return time.Millisecond }
	w.pointMutationPatch = func(string, ChangeKind, uint64) error {
		attempts.Add(1)
		return permanent
	}

	result := awaitMutationRetryResult(t, w.scheduleFileMutation("bad.go", ChangeModified))
	if result.Reindexed || !errors.Is(result.Err, permanent) {
		t.Fatalf("terminal result = %+v", result)
	}
	if attempts.Load() != 1 {
		t.Fatalf("attempts = %d, want 1", attempts.Load())
	}
}

func TestPointStoragePanicCompletesTicketWithTypedError(t *testing.T) {
	w := newMutationRetryTestWatcher(t)
	_, storageErr := indexCtxRawStorageError(t)
	var attempts atomic.Int32
	w.pointMutationPatch = func(string, ChangeKind, uint64) error {
		attempts.Add(1)
		panic(storageErr)
	}

	result := awaitMutationRetryResult(t, w.scheduleFileMutation("storage-full.go", ChangeModified))
	var typed *store_sqlite.StorageError
	if result.Reindexed || !errors.As(result.Err, &typed) || typed != storageErr {
		t.Fatalf("storage panic result = %+v, want original StorageError %p", result, storageErr)
	}
	if attempts.Load() != 1 {
		t.Fatalf("storage panic attempts = %d, want terminal single attempt", attempts.Load())
	}
}

func TestGuardWatcherPanicRecoversOnlyTypedStoragePanic(t *testing.T) {
	w := newMutationRetryTestWatcher(t)
	_, storageErr := indexCtxRawStorageError(t)
	func() {
		defer w.guardWatcherPanic("test background mutation")
		panic(storageErr)
	}()

	wantPanic := &struct{ label string }{label: "programmer panic"}
	var recovered any
	func() {
		defer func() { recovered = recover() }()
		func() {
			defer w.guardWatcherPanic("test background mutation")
			panic(wantPanic)
		}()
	}()
	if recovered != wantPanic {
		t.Fatalf("recovered panic = %#v, want original %#v", recovered, wantPanic)
	}
}

func TestPointMutationRepanicsArbitraryPanicWithoutCompletingTicket(t *testing.T) {
	w := newMutationRetryTestWatcher(t)
	const path = "programmer-panic.go"
	done := make(chan MutationResult, 1)
	w.mu.Lock()
	w.pendingGeneration[path] = 1
	w.mutationWaiters = map[string]map[uint64]chan MutationResult{path: {1: done}}
	w.mu.Unlock()
	wantPanic := &struct{ label string }{label: "programmer panic"}
	w.pointMutationPatch = func(string, ChangeKind, uint64) error {
		panic(wantPanic)
	}

	var recovered any
	func() {
		defer func() { recovered = recover() }()
		w.runPointMutation(path, ChangeModified, 1, 0)
	}()
	if recovered != wantPanic {
		t.Fatalf("recovered panic = %#v, want original %#v", recovered, wantPanic)
	}
	select {
	case result := <-done:
		t.Fatalf("arbitrary panic falsely completed ticket: %+v", result)
	default:
	}
	w.failMutationWaiters(errWatcherStopped)
}

func TestMutationRetryStopTerminatesPendingTimer(t *testing.T) {
	w := newMutationRetryTestWatcher(t)
	retryScheduled := make(chan struct{})
	w.mutationRetryDelayFn = func(int) time.Duration {
		close(retryScheduled)
		return time.Hour
	}
	w.pointMutationPatch = func(string, ChangeKind, uint64) error { return context.DeadlineExceeded }
	ticket := w.scheduleFileMutation("stop.go", ChangeModified)
	<-retryScheduled
	if err := w.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	result := awaitMutationRetryResult(t, ticket)
	if !errors.Is(result.Err, errWatcherStopped) {
		t.Fatalf("stop result = %+v", result)
	}
}

func TestMutationRetrySuccessorCoalescesWaiters(t *testing.T) {
	w := newMutationRetryTestWatcher(t)
	retryScheduled := make(chan struct{})
	var attempts atomic.Int32
	w.mutationRetryDelayFn = func(int) time.Duration {
		close(retryScheduled)
		return time.Hour
	}
	w.pointMutationPatch = func(string, ChangeKind, uint64) error {
		if attempts.Add(1) == 1 {
			return context.DeadlineExceeded
		}
		return nil
	}

	first := w.scheduleFileMutation("successor.go", ChangeModified)
	<-retryScheduled
	second := w.scheduleFileMutation("successor.go", ChangeDeleted)
	firstResult := awaitMutationRetryResult(t, first)
	secondResult := awaitMutationRetryResult(t, second)
	for name, result := range map[string]MutationResult{"first": firstResult, "second": secondResult} {
		if !result.Reindexed || result.Err != nil || result.AppliedGeneration != second.Generation {
			t.Fatalf("%s result = %+v, successor generation = %d", name, result, second.Generation)
		}
	}
	if attempts.Load() != 2 {
		t.Fatalf("attempts = %d, want 2", attempts.Load())
	}
}

func TestStormMutationRetryMergesNewerState(t *testing.T) {
	w := newMutationRetryTestWatcher(t)
	const path = "storm.go"
	firstDone := make(chan MutationResult, 1)
	secondDone := make(chan MutationResult, 1)
	w.mu.Lock()
	w.pendingGeneration[path] = 1
	w.mutationWaiters = map[string]map[uint64]chan MutationResult{path: {1: firstDone}}
	w.mu.Unlock()
	w.stormMu.Lock()
	w.stormBatch[path] = ChangeCreated
	w.stormGenerations[path] = 1
	w.stormMu.Unlock()

	var attempts atomic.Int32
	w.mutationRetryDelayFn = func(int) time.Duration { return time.Hour }
	w.batchReindex = func([]string) (*IndexResult, error) {
		if attempts.Add(1) == 1 {
			w.stormMu.Lock()
			w.stormBatch[path] = ChangeDeleted
			w.stormGenerations[path] = 2
			w.stormMu.Unlock()
			w.mu.Lock()
			w.pendingGeneration[path] = 2
			w.mutationWaiters[path][2] = secondDone
			w.mu.Unlock()
			return nil, fmt.Errorf("busy: %w", context.DeadlineExceeded)
		}
		return &IndexResult{}, nil
	}

	w.drainStorm()
	select {
	case result := <-firstDone:
		t.Fatalf("transient storm attempt completed early: %+v", result)
	default:
	}
	w.stormMu.Lock()
	if got := w.stormBatch[path]; got != ChangeDeleted {
		w.stormMu.Unlock()
		t.Fatalf("merged kind = %q, want %q", got, ChangeDeleted)
	}
	if got := w.stormGenerations[path]; got != 2 {
		w.stormMu.Unlock()
		t.Fatalf("merged generation = %d, want 2", got)
	}
	if w.stormTimer == nil || w.stormRetryAttempt != 1 {
		w.stormMu.Unlock()
		t.Fatal("storm retry did not publish exactly one first-attempt timer")
	}
	w.stopStormTimerLocked()
	w.stormMu.Unlock()

	w.drainStorm()
	for name, done := range map[string]chan MutationResult{"first": firstDone, "second": secondDone} {
		select {
		case result := <-done:
			if !result.Reindexed || result.Err != nil || result.AppliedGeneration != 2 {
				t.Fatalf("%s result = %+v", name, result)
			}
		case <-time.After(time.Second):
			t.Fatalf("timed out waiting for %s storm result", name)
		}
	}
}

func TestPointPermanentCompletionDefersToSuccessor(t *testing.T) {
	w := newMutationRetryTestWatcher(t)
	beforeComplete := make(chan struct{})
	releaseCompletion := make(chan struct{})
	successorStarted := make(chan struct{})
	releaseSuccessor := make(chan struct{})
	permanent := errors.New("parse failed")
	w.pointMutationPatch = func(_ string, _ ChangeKind, generation uint64) error {
		if generation == 1 {
			return permanent
		}
		close(successorStarted)
		<-releaseSuccessor
		return nil
	}
	w.mutationBeforeComplete = func(_ string, generation uint64) {
		if generation == 1 {
			close(beforeComplete)
			<-releaseCompletion
		}
	}

	first := w.scheduleFileMutation("point-race.go", ChangeModified)
	<-beforeComplete
	second := w.scheduleFileMutation("point-race.go", ChangeModified)
	close(releaseCompletion)
	<-successorStarted
	select {
	case result := <-first.Done:
		t.Fatalf("superseded permanent completion escaped early: %+v", result)
	default:
	}
	close(releaseSuccessor)
	for name, ticket := range map[string]*MutationTicket{"first": first, "second": second} {
		result := awaitMutationRetryResult(t, ticket)
		if !result.Reindexed || result.Err != nil || result.AppliedGeneration != second.Generation {
			t.Fatalf("%s result = %+v, successor generation = %d", name, result, second.Generation)
		}
	}
}

func TestStormPermanentCompletionDefersToSuccessor(t *testing.T) {
	w := newMutationRetryTestWatcher(t)
	const path = "storm-race.go"
	firstDone := make(chan MutationResult, 1)
	secondDone := make(chan MutationResult, 1)
	w.mu.Lock()
	w.pendingGeneration[path] = 1
	w.mutationWaiters = map[string]map[uint64]chan MutationResult{path: {1: firstDone}}
	w.mu.Unlock()
	w.stormMu.Lock()
	w.stormBatch[path] = ChangeModified
	w.stormGenerations[path] = 1
	w.stormMu.Unlock()

	beforeComplete := make(chan struct{})
	releaseCompletion := make(chan struct{})
	var attempts atomic.Int32
	w.batchReindex = func([]string) (*IndexResult, error) {
		if attempts.Add(1) == 1 {
			return nil, errors.New("permanent batch failure")
		}
		return &IndexResult{}, nil
	}
	w.stormDrained = func(int) {
		if attempts.Load() == 1 {
			close(beforeComplete)
			<-releaseCompletion
		}
	}
	drainDone := make(chan struct{})
	go func() {
		w.drainStorm()
		close(drainDone)
	}()
	<-beforeComplete
	w.mu.Lock()
	w.pendingGeneration[path] = 2
	w.mutationWaiters[path][2] = secondDone
	w.mu.Unlock()
	w.stormMu.Lock()
	w.stormBatch[path] = ChangeDeleted
	w.stormGenerations[path] = 2
	w.stormMu.Unlock()
	close(releaseCompletion)
	<-drainDone
	select {
	case result := <-firstDone:
		t.Fatalf("superseded storm completion escaped early: %+v", result)
	default:
	}

	w.drainStorm()
	for name, done := range map[string]chan MutationResult{"first": firstDone, "second": secondDone} {
		select {
		case result := <-done:
			if !result.Reindexed || result.Err != nil || result.AppliedGeneration != 2 {
				t.Fatalf("%s result = %+v", name, result)
			}
		case <-time.After(time.Second):
			t.Fatalf("timed out waiting for %s storm result", name)
		}
	}
}

func TestStormStoragePanicCompletesDetachedWaitersWithTypedError(t *testing.T) {
	w := newMutationRetryTestWatcher(t)
	_, storageErr := indexCtxRawStorageError(t)
	const path = "storm-panic.go"
	done := make(chan MutationResult, 1)
	w.mu.Lock()
	w.pendingGeneration[path] = 1
	w.mutationWaiters = map[string]map[uint64]chan MutationResult{path: {1: done}}
	w.mu.Unlock()
	w.stormMu.Lock()
	w.stormBatch[path] = ChangeModified
	w.stormGenerations[path] = 1
	w.stormMu.Unlock()
	w.batchReindex = func([]string) (*IndexResult, error) {
		panic(storageErr)
	}

	w.drainStorm()
	select {
	case result := <-done:
		var typed *store_sqlite.StorageError
		if result.Reindexed || !errors.As(result.Err, &typed) || typed != storageErr {
			t.Fatalf("storage panic result = %+v, want original StorageError %p", result, storageErr)
		}
	case <-time.After(time.Second):
		t.Fatal("panic stranded detached storm waiter")
	}
	w.mu.Lock()
	leaked := len(w.mutationWaiters[path])
	w.mu.Unlock()
	if leaked != 0 {
		t.Fatalf("storage panic leaked %d waiter(s)", leaked)
	}
}

func TestStormDrainRepanicsArbitraryPanicWithoutCompletingWaiters(t *testing.T) {
	w := newMutationRetryTestWatcher(t)
	const path = "storm-programmer-panic.go"
	done := make(chan MutationResult, 1)
	w.mu.Lock()
	w.pendingGeneration[path] = 1
	w.mutationWaiters = map[string]map[uint64]chan MutationResult{path: {1: done}}
	w.mu.Unlock()
	w.stormMu.Lock()
	w.stormBatch[path] = ChangeModified
	w.stormGenerations[path] = 1
	w.stormMu.Unlock()
	wantPanic := &struct{ label string }{label: "programmer panic"}
	w.batchReindex = func([]string) (*IndexResult, error) {
		panic(wantPanic)
	}

	var recovered any
	func() {
		defer func() { recovered = recover() }()
		w.drainStorm()
	}()
	if recovered != wantPanic {
		t.Fatalf("recovered panic = %#v, want original %#v", recovered, wantPanic)
	}
	select {
	case result := <-done:
		t.Fatalf("arbitrary panic falsely completed storm waiter: %+v", result)
	default:
	}
	w.failMutationWaiters(errWatcherStopped)
}

func TestRetryableMutationErrorClassifier(t *testing.T) {
	if retryableMutationError(nil) || retryableMutationError(errors.New("parse")) {
		t.Fatal("permanent errors must not retry")
	}
	if !retryableMutationError(context.DeadlineExceeded) {
		t.Fatal("deadline must retry")
	}
	if !retryableMutationError(fmt.Errorf("writer gate: %w", context.DeadlineExceeded)) {
		t.Fatal("wrapped deadline must retry")
	}
	if retryableMutationError(context.Canceled) {
		t.Fatal("cancellation must remain terminal")
	}
	committedDeadline := committedMutationTestError{error: fmt.Errorf("committed storage failure: %w", context.DeadlineExceeded)}
	if retryableMutationError(committedDeadline) {
		t.Fatal("committed storage failure must not retry even when it wraps a retryable deadline")
	}
	for attempt, want := range map[int]time.Duration{1: 100 * time.Millisecond, 2: 200 * time.Millisecond, 7: 5 * time.Second, 100: 5 * time.Second} {
		if got := mutationRetryBackoff(attempt); got != want {
			t.Fatalf("attempt %d delay = %s, want %s", attempt, got, want)
		}
	}
}

func BenchmarkScheduleFileMutationRetryCoalesced(b *testing.B) {
	w := newMutationRetryTestWatcher(b)
	const path = "bench.go"
	w.mutationRetryDelayFn = func(int) time.Duration { return time.Hour }
	w.mu.Lock()
	w.pendingGeneration[path] = 1
	w.mu.Unlock()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if !w.schedulePointMutationRetry(path, ChangeModified, 1, 1) {
			b.Fatal("retry was not retained")
		}
	}
}

func BenchmarkScheduleFileMutationRetryCycle(b *testing.B) {
	w := newMutationRetryTestWatcher(b)
	w.config.DebounceMs = 0
	w.mutationRetryDelayFn = func(int) time.Duration { return 0 }
	var attempts atomic.Uint64
	w.pointMutationPatch = func(string, ChangeKind, uint64) error {
		if attempts.Add(1)%2 == 1 {
			return context.DeadlineExceeded
		}
		return nil
	}
	runCycle := func() {
		ticket := w.scheduleFileMutation("cycle.go", ChangeModified)
		result := <-ticket.Done
		if !result.Reindexed || result.Err != nil {
			b.Fatalf("retry cycle result = %+v", result)
		}
	}
	runCycle()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		runCycle()
	}
}

func installBlockedMutationAdmission(w *Watcher) chan struct{} {
	slots := make(chan struct{}, 1)
	w.mutationSlotsOnce.Do(func() {
		w.mutationSlotsCh = slots
	})
	slots <- struct{}{}
	return slots
}

func TestMutationAdmissionTimeoutRetriesAndKeepsTicketPending(t *testing.T) {
	w := newMutationRetryTestWatcher(t)
	slots := installBlockedMutationAdmission(w)
	deferred := make(chan struct{})
	var deferredOnce sync.Once
	var patches atomic.Int32
	w.mutationAdmissionWaitFn = func() time.Duration { return time.Millisecond }
	w.mutationRetryDelayFn = func(int) time.Duration {
		deferredOnce.Do(func() { close(deferred) })
		return 25 * time.Millisecond
	}
	w.pointMutationPatch = func(string, ChangeKind, uint64) error {
		patches.Add(1)
		return nil
	}

	ticket := w.scheduleFileMutation("admission-retry.go", ChangeModified)
	<-deferred
	select {
	case result := <-ticket.Done:
		t.Fatalf("admission timeout completed ticket early: %+v", result)
	default:
	}
	<-slots
	result := awaitMutationRetryResult(t, ticket)
	if !result.Reindexed || result.Err != nil || result.AppliedGeneration != ticket.Generation {
		t.Fatalf("retry result = %+v", result)
	}
	if patches.Load() != 1 {
		t.Fatalf("patches = %d, want 1", patches.Load())
	}
}

func TestMutationAdmissionTimeoutCoalescesIntoNewerGeneration(t *testing.T) {
	w := newMutationRetryTestWatcher(t)
	slots := installBlockedMutationAdmission(w)
	deferred := make(chan struct{})
	var deferredOnce sync.Once
	var patches atomic.Int32
	w.mutationAdmissionWaitFn = func() time.Duration { return time.Millisecond }
	w.mutationRetryDelayFn = func(int) time.Duration {
		deferredOnce.Do(func() { close(deferred) })
		return time.Hour
	}
	w.pointMutationPatch = func(string, ChangeKind, uint64) error {
		patches.Add(1)
		return nil
	}

	first := w.scheduleFileMutation("admission-successor.go", ChangeModified)
	<-deferred
	select {
	case result := <-first.Done:
		t.Fatalf("deferred generation completed early: %+v", result)
	default:
	}
	second := w.scheduleFileMutation("admission-successor.go", ChangeDeleted)
	<-slots
	for name, ticket := range map[string]*MutationTicket{"first": first, "second": second} {
		result := awaitMutationRetryResult(t, ticket)
		if !result.Reindexed || result.Err != nil || result.AppliedGeneration != second.Generation {
			t.Fatalf("%s result = %+v, successor generation = %d", name, result, second.Generation)
		}
	}
	if patches.Load() != 1 {
		t.Fatalf("patches = %d, want 1", patches.Load())
	}
}

func TestMutationAdmissionTimeoutThenStopCompletesWatcherStopped(t *testing.T) {
	w := newMutationRetryTestWatcher(t)
	installBlockedMutationAdmission(w)
	deferred := make(chan struct{})
	var deferredOnce sync.Once
	w.mutationAdmissionWaitFn = func() time.Duration { return time.Millisecond }
	w.mutationRetryDelayFn = func(int) time.Duration {
		deferredOnce.Do(func() { close(deferred) })
		return time.Hour
	}

	ticket := w.scheduleFileMutation("admission-stop.go", ChangeModified)
	<-deferred
	if err := w.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	result := awaitMutationRetryResult(t, ticket)
	if !errors.Is(result.Err, errWatcherStopped) {
		t.Fatalf("stop result = %+v", result)
	}
}

func TestMutationAdmissionTimeoutDoesNotRetryPermanentPatchError(t *testing.T) {
	w := newMutationRetryTestWatcher(t)
	slots := installBlockedMutationAdmission(w)
	deferred := make(chan struct{})
	var deferredOnce sync.Once
	var attempts atomic.Int32
	permanent := errors.New("parse failed")
	w.mutationAdmissionWaitFn = func() time.Duration { return time.Millisecond }
	w.mutationRetryDelayFn = func(int) time.Duration {
		deferredOnce.Do(func() { close(deferred) })
		return 25 * time.Millisecond
	}
	w.pointMutationPatch = func(string, ChangeKind, uint64) error {
		attempts.Add(1)
		return permanent
	}

	ticket := w.scheduleFileMutation("admission-permanent.go", ChangeModified)
	<-deferred
	<-slots
	result := awaitMutationRetryResult(t, ticket)
	if result.Reindexed || !errors.Is(result.Err, permanent) {
		t.Fatalf("permanent result = %+v", result)
	}
	if attempts.Load() != 1 {
		t.Fatalf("attempts = %d, want 1", attempts.Load())
	}
}

func BenchmarkMutationAdmissionDeferredRetryCycle(b *testing.B) {
	w := newMutationRetryTestWatcher(b)
	w.config.DebounceMs = 0
	slots := make(chan struct{}, 1)
	w.mutationSlotsOnce.Do(func() {
		w.mutationSlotsCh = slots
	})
	w.mutationAdmissionWaitFn = func() time.Duration { return 0 }
	w.mutationRetryDelayFn = func(int) time.Duration {
		select {
		case <-slots:
		default:
		}
		return 0
	}
	w.pointMutationPatch = func(string, ChangeKind, uint64) error { return nil }

	runCycle := func() {
		slots <- struct{}{}
		result := <-w.scheduleFileMutation("admission-bench.go", ChangeModified).Done
		if !result.Reindexed || result.Err != nil {
			b.Fatalf("admission retry result = %+v", result)
		}
	}
	runCycle()
	b.ReportAllocs()
	b.ReportMetric(1, "tickets/op")
	b.ReportMetric(0, "terminal_failures/op")
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		runCycle()
	}
}
