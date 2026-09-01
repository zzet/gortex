package indexer

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/parser"
	"go.uber.org/zap"
)

func waitForRepositoryMutationStats(
	t *testing.T,
	coordinator *repositoryMutationCoordinator,
	predicate func(repositoryMutationCoordinatorStats) bool,
) repositoryMutationCoordinatorStats {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		stats := coordinator.stats()
		if predicate(stats) {
			return stats
		}
		time.Sleep(time.Millisecond)
	}
	stats := coordinator.stats()
	t.Fatalf("coordinator state did not converge: %+v", stats)
	return stats
}

func TestRepositoryMutationScopeUnionAndFullEscalation(t *testing.T) {
	var scope repositoryMutationScope
	if err := scope.merge([]string{"b.go", "a.go", "a.go"}); err != nil {
		t.Fatal(err)
	}
	if got, want := scope.take(), []string{"a.go", "b.go"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("union = %#v, want %#v", got, want)
	}

	if err := scope.merge([]string{"one.go"}); err != nil {
		t.Fatal(err)
	}
	if err := scope.merge(nil); err != nil {
		t.Fatal(err)
	}
	if got := scope.take(); got != nil {
		t.Fatalf("explicit full scope = %#v, want nil", got)
	}

	paths := make([]string, repositoryMutationPathCap+1)
	for i := range paths {
		paths[i] = time.Unix(int64(i), 0).Format("20060102150405.000000000")
	}
	if err := scope.merge(paths); err != nil {
		t.Fatal(err)
	}
	if got := scope.take(); got != nil {
		t.Fatalf("cap-escalated scope retained %d paths", len(got))
	}
}

func TestRepositoryMutationCoordinatorUnionsDirtyFollowup(t *testing.T) {
	var mu sync.Mutex
	var calls [][]string
	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	coordinator := newRepositoryMutationCoordinator(func(paths []string) (*IndexResult, error) {
		mu.Lock()
		call := len(calls) + 1
		calls = append(calls, append([]string(nil), paths...))
		mu.Unlock()
		if call == 1 {
			close(firstStarted)
			<-releaseFirst
		}
		return &IndexResult{FileCount: call}, nil
	})

	type response struct {
		result *IndexResult
		err    error
	}
	responses := make(chan response, 3)
	go func() {
		result, err := coordinator.reconcile(context.Background(), []string{"a.go"})
		responses <- response{result: result, err: err}
	}()
	<-firstStarted
	go func() {
		result, err := coordinator.reconcile(context.Background(), []string{"c.go"})
		responses <- response{result: result, err: err}
	}()
	go func() {
		result, err := coordinator.reconcile(context.Background(), []string{"b.go", "c.go"})
		responses <- response{result: result, err: err}
	}()
	waitForRepositoryMutationStats(t, coordinator, func(stats repositoryMutationCoordinatorStats) bool {
		return stats.RequestedGeneration == 3 && stats.PendingPaths == 2
	})
	close(releaseFirst)

	for range 3 {
		response := <-responses
		if response.err != nil || response.result == nil {
			t.Fatalf("response = result:%#v err:%v", response.result, response.err)
		}
	}
	if err := coordinator.closeAndWait(context.Background()); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if got, want := calls, [][]string{{"a.go"}, {"b.go", "c.go"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("executions = %#v, want %#v", got, want)
	}
}

func TestRepositoryMutationCoordinatorFullDominatesQueuedPaths(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	var gotPaths []string
	coordinator := newRepositoryMutationCoordinator(func(paths []string) (*IndexResult, error) {
		calls.Add(1)
		gotPaths = append([]string(nil), paths...)
		return &IndexResult{}, nil
	})
	exclusiveDone := make(chan error, 1)
	go func() {
		exclusiveDone <- coordinator.runExclusive(context.Background(), func() error {
			close(entered)
			<-release
			return nil
		})
	}()
	<-entered

	results := make(chan error, 2)
	go func() {
		_, err := coordinator.reconcile(context.Background(), []string{"one.go"})
		results <- err
	}()
	go func() {
		_, err := coordinator.reconcile(context.Background(), nil)
		results <- err
	}()
	waitForRepositoryMutationStats(t, coordinator, func(stats repositoryMutationCoordinatorStats) bool {
		return stats.RequestedGeneration == 2 && stats.PendingFull
	})
	close(release)
	if err := <-exclusiveDone; err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 1 || gotPaths != nil {
		t.Fatalf("full execution = calls:%d paths:%#v, want 1/nil", calls.Load(), gotPaths)
	}
}

func TestRepositoryMutationCoordinatorReturnsStoragePanicAndRunsDirtyFollowup(t *testing.T) {
	_, storageErr := indexCtxRawStorageError(t)
	started := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	coordinator := newRepositoryMutationCoordinator(func(paths []string) (*IndexResult, error) {
		call := calls.Add(1)
		if call == 1 {
			close(started)
			<-release
			panic(storageErr)
		}
		return &IndexResult{FileCount: int(call)}, nil
	})

	type response struct {
		result *IndexResult
		err    error
	}
	first := make(chan response, 1)
	second := make(chan response, 1)
	go func() {
		result, err := coordinator.reconcile(context.Background(), []string{"first.go"})
		first <- response{result: result, err: err}
	}()
	<-started
	go func() {
		result, err := coordinator.reconcile(context.Background(), []string{"second.go"})
		second <- response{result: result, err: err}
	}()
	waitForRepositoryMutationStats(t, coordinator, func(stats repositoryMutationCoordinatorStats) bool {
		return stats.RequestedGeneration == 2 && stats.PendingPaths == 1
	})
	close(release)

	if got := <-first; got.result != nil {
		t.Fatalf("storage panic result = %#v, want nil", got.result)
	} else {
		var typed *store_sqlite.StorageError
		if !errors.As(got.err, &typed) || typed != storageErr {
			t.Fatalf("storage panic error = %T %v, want original StorageError %p", got.err, got.err, storageErr)
		}
	}
	if got := <-second; got.err != nil || got.result == nil || got.result.FileCount != 2 {
		t.Fatalf("dirty followup = result:%#v err:%v", got.result, got.err)
	}
	if calls.Load() != 2 {
		t.Fatalf("executor calls = %d, want 2", calls.Load())
	}
}

func TestRepositoryMutationCoordinatorDeliversStoragePanicToCoalescedWaiters(t *testing.T) {
	_, storageErr := indexCtxRawStorageError(t)
	var calls atomic.Int32
	coordinator := newRepositoryMutationCoordinator(func([]string) (*IndexResult, error) {
		calls.Add(1)
		panic(storageErr)
	})

	entered := make(chan struct{})
	release := make(chan struct{})
	exclusiveDone := make(chan error, 1)
	go func() {
		exclusiveDone <- coordinator.runExclusive(context.Background(), func() error {
			close(entered)
			<-release
			return nil
		})
	}()
	<-entered

	results := make(chan error, 2)
	for _, path := range []string{"a.go", "b.go"} {
		go func(path string) {
			result, err := coordinator.reconcile(context.Background(), []string{path})
			if result != nil {
				results <- fmt.Errorf("result for %s = %#v, want nil", path, result)
				return
			}
			results <- err
		}(path)
	}
	waitForRepositoryMutationStats(t, coordinator, func(stats repositoryMutationCoordinatorStats) bool {
		return stats.RequestedGeneration == 2 && stats.PendingPaths == 2
	})
	close(release)
	if err := <-exclusiveDone; err != nil {
		t.Fatalf("exclusive holder: %v", err)
	}

	for range 2 {
		err := <-results
		var typed *store_sqlite.StorageError
		if !errors.As(err, &typed) || typed != storageErr {
			t.Fatalf("coalesced storage panic error = %T %v, want original StorageError %p", err, err, storageErr)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("coalesced executor calls = %d, want 1", calls.Load())
	}
}

func TestExecuteRepositoryMutationRepanicsArbitraryPanic(t *testing.T) {
	wantPanic := &struct{ label string }{label: "programmer panic"}
	var recovered any
	func() {
		defer func() { recovered = recover() }()
		_ = executeRepositoryMutation(func([]string) (*IndexResult, error) {
			panic(wantPanic)
		}, []string{"a.go"})
	}()
	if recovered != wantPanic {
		t.Fatalf("recovered panic = %#v, want original %#v", recovered, wantPanic)
	}
}

func TestRepositoryMutationExclusiveReturnsStoragePanicAndReleasesLane(t *testing.T) {
	_, storageErr := indexCtxRawStorageError(t)
	coordinator := newRepositoryMutationCoordinator(nil)
	err := coordinator.runExclusive(context.Background(), func() error {
		panic(storageErr)
	})
	var typed *store_sqlite.StorageError
	if !errors.As(err, &typed) || typed != storageErr {
		t.Fatalf("exclusive storage panic error = %T %v, want original StorageError %p", err, err, storageErr)
	}
	if err := coordinator.runExclusive(context.Background(), func() error { return nil }); err != nil {
		t.Fatalf("lane remained unavailable after storage panic: %v", err)
	}
}

func TestRepositoryMutationExclusiveRepanicsArbitraryPanicAndReleasesLane(t *testing.T) {
	wantPanic := &struct{ label string }{label: "programmer panic"}
	coordinator := newRepositoryMutationCoordinator(nil)
	var recovered any
	func() {
		defer func() { recovered = recover() }()
		_ = coordinator.runExclusive(context.Background(), func() error {
			panic(wantPanic)
		})
	}()
	if recovered != wantPanic {
		t.Fatalf("recovered panic = %#v, want original %#v", recovered, wantPanic)
	}
	if err := coordinator.runExclusive(context.Background(), func() error { return nil }); err != nil {
		t.Fatalf("lane remained unavailable after arbitrary panic: %v", err)
	}
}

func BenchmarkExecuteRepositoryMutationStoragePanicBoundarySuccess(b *testing.B) {
	want := &IndexResult{FileCount: 1}
	executor := func([]string) (*IndexResult, error) { return want, nil }
	paths := []string{"a.go"}
	b.ReportAllocs()
	for b.Loop() {
		outcome := executeRepositoryMutation(executor, paths)
		if outcome.result != want || outcome.err != nil {
			b.Fatalf("outcome = result:%p err:%v, want result:%p", outcome.result, outcome.err, want)
		}
	}
}

func TestRepositoryMutationCoordinatorSerializesMixedWork(t *testing.T) {
	var active atomic.Int32
	var maximum atomic.Int32
	enter := func() {
		now := active.Add(1)
		for {
			old := maximum.Load()
			if now <= old || maximum.CompareAndSwap(old, now) {
				break
			}
		}
		time.Sleep(time.Millisecond)
		active.Add(-1)
	}
	coordinator := newRepositoryMutationCoordinator(func(paths []string) (*IndexResult, error) {
		enter()
		return &IndexResult{}, nil
	})

	var wait sync.WaitGroup
	for i := 0; i < 32; i++ {
		wait.Add(2)
		go func() {
			defer wait.Done()
			_, _ = coordinator.reconcile(context.Background(), []string{"same.go"})
		}()
		go func() {
			defer wait.Done()
			_ = coordinator.runExclusive(context.Background(), func() error {
				enter()
				return nil
			})
		}()
	}
	wait.Wait()
	if maximum.Load() != 1 {
		t.Fatalf("maximum concurrent mutation pipelines = %d, want 1", maximum.Load())
	}
}

func TestRepositoryMutationCoordinatorRejectsPreCanceledWork(t *testing.T) {
	var executions atomic.Int32
	coordinator := newRepositoryMutationCoordinator(func(paths []string) (*IndexResult, error) {
		executions.Add(1)
		return &IndexResult{}, nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if result, err := coordinator.reconcile(ctx, []string{"a.go"}); result != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-canceled reconcile = result:%#v err:%v", result, err)
	}
	if err := coordinator.runExclusive(ctx, func() error {
		executions.Add(1)
		return nil
	}); !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-canceled exclusive error = %v", err)
	}
	if executions.Load() != 0 {
		t.Fatalf("pre-canceled executions = %d, want 0", executions.Load())
	}
}

func TestRepositoryMutationCancellationDoesNotCancelAdmittedWork(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	finished := make(chan struct{})
	coordinator := newRepositoryMutationCoordinator(func(paths []string) (*IndexResult, error) {
		close(started)
		<-release
		close(finished)
		return &IndexResult{}, nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	result, err := coordinator.reconcile(ctx, []string{"a.go"})
	if result != nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("canceled wait = result:%#v err:%v", result, err)
	}
	<-started
	close(release)
	select {
	case <-finished:
	case <-time.After(2 * time.Second):
		t.Fatal("admitted mutation was canceled with its waiter")
	}
	if err := coordinator.closeAndWait(context.Background()); err != nil {
		t.Fatal(err)
	}
	stats := coordinator.stats()
	if stats.CompletedGeneration != 1 || stats.Running {
		t.Fatalf("completed state = %+v", stats)
	}
	if _, err := coordinator.reconcile(context.Background(), nil); !errors.Is(err, errRepositoryMutationCoordinatorClosed) {
		t.Fatalf("closed reconcile error = %v", err)
	}
	if err := coordinator.runExclusive(context.Background(), func() error { return nil }); !errors.Is(err, errRepositoryMutationCoordinatorClosed) {
		t.Fatalf("closed exclusive error = %v", err)
	}
}

func TestIndexCtxWaitsForRepositoryMutationLane(t *testing.T) {
	cfg := config.Default()
	idx := New(graph.New(), parser.NewRegistry(), cfg.Index, zap.NewNop())

	entered := make(chan struct{})
	release := make(chan struct{})
	holderDone := make(chan error, 1)
	go func() {
		holderDone <- idx.coordinateRepositoryMutation(context.Background(), func() error {
			close(entered)
			<-release
			return nil
		})
	}()
	<-entered

	ctx, cancel := context.WithCancel(context.Background())
	root := t.TempDir()
	indexDone := make(chan error, 1)
	go func() {
		_, err := idx.IndexCtx(ctx, root)
		indexDone <- err
	}()
	select {
	case err := <-indexDone:
		t.Fatalf("full index bypassed repository mutation lane: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	cancel()
	if err := <-indexDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled full index error = %v, want context.Canceled", err)
	}

	close(release)
	if err := <-holderDone; err != nil {
		t.Fatalf("lane holder: %v", err)
	}
}

func TestRepositoryMutationLaneSurvivesIndexerReplacement(t *testing.T) {
	const prefix = "repo"
	mi := &MultiIndexer{indexers: make(map[string]*Indexer)}

	original := &Indexer{repositoryMutationOwner: mi}
	original.SetRepoPrefix(prefix)
	mi.indexers[prefix] = original

	enteredOriginal := make(chan struct{})
	releaseOriginal := make(chan struct{})
	originalDone := make(chan error, 1)
	go func() {
		originalDone <- original.coordinateRepositoryMutation(context.Background(), func() error {
			close(enteredOriginal)
			<-releaseOriginal
			return nil
		})
	}()
	<-enteredOriginal

	replacement := &Indexer{repositoryMutationOwner: mi}
	replacement.SetRepoPrefix(prefix)
	mi.mu.Lock()
	mi.indexers[prefix] = replacement
	mi.mu.Unlock()
	if original.repositoryMutations() != replacement.repositoryMutations() {
		t.Fatal("replacement Indexer attached a second repository mutation lane")
	}

	enteredReplacement := make(chan struct{})
	replacementDone := make(chan error, 1)
	go func() {
		replacementDone <- replacement.coordinateRepositoryMutation(context.Background(), func() error {
			close(enteredReplacement)
			return nil
		})
	}()
	select {
	case <-enteredReplacement:
		t.Fatal("replacement mutation overlapped the original Indexer mutation")
	case <-time.After(20 * time.Millisecond):
	}

	close(releaseOriginal)
	if err := <-originalDone; err != nil {
		t.Fatalf("original mutation: %v", err)
	}
	select {
	case <-enteredReplacement:
	case <-time.After(2 * time.Second):
		t.Fatal("replacement mutation did not enter the shared lane")
	}
	if err := <-replacementDone; err != nil {
		t.Fatalf("replacement mutation: %v", err)
	}
}

func TestRepositoryMutationKeepsExistingWatcherOnSharedLaneAfterReplacement(t *testing.T) {
	const prefix = "repo"
	mi := &MultiIndexer{indexers: make(map[string]*Indexer)}
	existing := &Indexer{repositoryMutationOwner: mi}
	existing.SetRepoPrefix(prefix)
	replacement := &Indexer{repositoryMutationOwner: mi}
	replacement.SetRepoPrefix(prefix)
	mi.indexers[prefix] = replacement

	var called atomic.Bool
	err := existing.coordinateRepositoryMutation(context.Background(), func() error {
		called.Store(true)
		return nil
	})
	if err != nil {
		t.Fatalf("existing watcher mutation after replacement: %v", err)
	}
	if !called.Load() {
		t.Fatal("existing watcher stopped mutating after Indexer replacement")
	}
	if existing.repositoryMutations() != replacement.repositoryMutations() {
		t.Fatal("existing watcher no longer shares the replacement repository lane")
	}
}

func TestUntrackRepoWaitsForMutationLaneAndRejectsAdmission(t *testing.T) {
	const prefix = "repo"
	mi := &MultiIndexer{
		graph:    graph.New(),
		repos:    map[string]*RepoMetadata{prefix: {RepoPrefix: prefix}},
		indexers: map[string]*Indexer{prefix: nil},
		logger:   zap.NewNop(),
	}
	coordinator := mi.repositoryMutationCoordinator(prefix)
	entered := make(chan struct{})
	release := make(chan struct{})
	mutationDone := make(chan error, 1)
	go func() {
		mutationDone <- coordinator.runExclusive(context.Background(), func() error {
			close(entered)
			<-release
			return nil
		})
	}()
	<-entered

	type untrackResult struct {
		nodes int
		edges int
	}
	untrackDone := make(chan untrackResult, 1)
	go func() {
		nodes, edges := mi.UntrackRepo(prefix)
		untrackDone <- untrackResult{nodes: nodes, edges: edges}
	}()

	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	for {
		coordinator.mu.Lock()
		closed := coordinator.closed
		coordinator.mu.Unlock()
		if closed {
			break
		}
		select {
		case <-untrackDone:
			t.Fatal("UntrackRepo returned before closing and draining the repository mutation lane")
		case <-deadline.C:
			t.Fatal("UntrackRepo did not close the repository mutation lane")
		case <-time.After(time.Millisecond):
		}
	}
	if current := mi.repositoryMutationCoordinator(prefix); current != coordinator {
		t.Fatal("UntrackRepo detached the closed lane before repository teardown completed")
	} else if err := current.runExclusive(context.Background(), func() error { return nil }); !errors.Is(err, errRepositoryMutationCoordinatorClosed) {
		t.Fatalf("mutation admitted after untrack started: %v", err)
	}
	select {
	case <-untrackDone:
		t.Fatal("UntrackRepo returned before the active repository mutation completed")
	case <-time.After(20 * time.Millisecond):
	}

	close(release)
	if err := <-mutationDone; err != nil {
		t.Fatalf("active mutation: %v", err)
	}
	select {
	case <-untrackDone:
	case <-time.After(2 * time.Second):
		t.Fatal("UntrackRepo did not finish after the active mutation drained")
	}
}

func TestRepositoryMutationConditionalDetachPreservesRetrackedLane(t *testing.T) {
	const prefix = "repo"
	mi := &MultiIndexer{}

	old := mi.repositoryMutationCoordinator(prefix)
	if got := mi.existingRepositoryMutationCoordinator(prefix); got != old {
		t.Fatalf("existing coordinator = %p, want old slot %p", got, old)
	}
	if !mi.detachRepositoryMutationCoordinator(prefix, old) {
		t.Fatal("exact old coordinator was not detached")
	}

	replacement := mi.repositoryMutationCoordinator(prefix)
	if replacement == old {
		t.Fatal("retrack reused the detached coordinator")
	}
	// This is the delayed tail of the old teardown. It must not remove the
	// replacement generation that now owns the same prefix.
	if mi.detachRepositoryMutationCoordinator(prefix, old) {
		t.Fatal("stale teardown detached the retracked coordinator")
	}
	if got := mi.existingRepositoryMutationCoordinator(prefix); got != replacement {
		t.Fatalf("coordinator after stale detach = %p, want replacement %p", got, replacement)
	}
	if err := replacement.runExclusive(context.Background(), func() error { return nil }); err != nil {
		t.Fatalf("replacement coordinator is not open: %v", err)
	}
}

func TestConcurrentUntrackRepoLeavesFreshRetrackLaneOpen(t *testing.T) {
	const prefix = "repo"
	cfg := config.Default()
	store := graph.New()
	registry := parser.NewRegistry()
	mi := &MultiIndexer{
		graph:    store,
		registry: registry,
		repos: map[string]*RepoMetadata{
			prefix: {RepoPrefix: prefix},
		},
		indexers: make(map[string]*Indexer),
		logger:   zap.NewNop(),
	}
	oldIndexer := New(store, registry, cfg.Index, zap.NewNop())
	oldIndexer.repositoryMutationOwner = mi
	oldIndexer.SetRepoPrefix(prefix)
	mi.indexers[prefix] = oldIndexer
	old := mi.existingRepositoryMutationCoordinator(prefix)
	if old == nil {
		t.Fatal("old repository coordinator was not installed")
	}

	entered := make(chan struct{})
	release := make(chan struct{})
	holderDone := make(chan error, 1)
	go func() {
		holderDone <- old.runExclusive(context.Background(), func() error {
			close(entered)
			<-release
			return nil
		})
	}()
	<-entered

	start := make(chan struct{})
	ready := make(chan struct{}, 2)
	untracked := make(chan struct{}, 2)
	for range 2 {
		go func() {
			ready <- struct{}{}
			<-start
			mi.UntrackRepo(prefix)
			untracked <- struct{}{}
		}()
	}
	<-ready
	<-ready
	close(start)

	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	for {
		old.mu.Lock()
		closed := old.closed
		old.mu.Unlock()
		if closed {
			break
		}
		select {
		case <-deadline.C:
			t.Fatal("concurrent untrack did not close the old coordinator")
		case <-time.After(time.Millisecond):
		}
	}
	close(release)
	if err := <-holderDone; err != nil {
		t.Fatalf("lane holder: %v", err)
	}
	for range 2 {
		select {
		case <-untracked:
		case <-time.After(2 * time.Second):
			t.Fatal("concurrent untrack did not finish")
		}
	}
	if got := mi.existingRepositoryMutationCoordinator(prefix); got != nil {
		t.Fatalf("double untrack stranded coordinator %p", got)
	}

	// Re-track the same prefix and prove the new generation is neither the old
	// closed lane nor a closed lane manufactured by the losing untrack.
	replacementIndexer := New(store, registry, cfg.Index, zap.NewNop())
	replacementIndexer.repositoryMutationOwner = mi
	replacementIndexer.SetRepoPrefix(prefix)
	replacement := mi.existingRepositoryMutationCoordinator(prefix)
	if replacement == nil || replacement == old {
		t.Fatalf("replacement coordinator = %p, old = %p", replacement, old)
	}
	mi.mu.Lock()
	mi.repos[prefix] = &RepoMetadata{RepoPrefix: prefix}
	mi.indexers[prefix] = replacementIndexer
	mi.mu.Unlock()
	if err := replacement.runExclusive(context.Background(), func() error { return nil }); err != nil {
		t.Fatalf("fresh retrack lane rejected mutation: %v", err)
	}
}
