package indexer

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/semantic"
)

func TestWatcherStormDispatchesOneBatchForThousandPaths(t *testing.T) {
	idx := newTestIndexer(graph.New())
	idx.rootPath = t.TempDir()
	watcher, err := NewWatcher(idx, config.WatchConfig{}, zap.NewNop())
	require.NoError(t, err)

	const count = 1000
	watcher.stormBatch = make(map[string]ChangeKind, count)
	for i := count - 1; i >= 0; i-- {
		path := filepath.Join(idx.rootPath, fmt.Sprintf("f%04d.go", i))
		watcher.stormBatch[path] = ChangeModified
	}

	calls := 0
	var gotPaths []string
	watcher.batchReindex = func(paths []string) (*IndexResult, error) {
		calls++
		gotPaths = append([]string(nil), paths...)
		return &IndexResult{StaleFileCount: len(paths)}, nil
	}
	drained := 0
	watcher.stormDrained = func(n int) { drained = n }

	watcher.drainStorm()

	require.Equal(t, 1, calls, "a storm must enter the batch pipeline once")
	require.Len(t, gotPaths, count)
	require.True(t, sort.StringsAreSorted(gotPaths), "storm paths must be deterministic")
	require.Equal(t, count, drained)
	require.Empty(t, watcher.stormBatch)
	require.False(t, watcher.stormActive)
}

func TestWatcherStormTakeoverCompletesSupersededMutationTickets(t *testing.T) {
	tests := []struct {
		name       string
		batch      *IndexResult
		wantErr    bool
		wantReason string
	}{
		{name: "success", batch: &IndexResult{StaleFileCount: 1}},
		{
			name:       "path failure",
			batch:      &IndexResult{},
			wantErr:    true,
			wantReason: "storm mutation batch failed to index",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir, _, watcher := inertTestWatcher(t, "main.go", "package main\n\nfunc Value() int { return 0 }\n")
			path := filepath.Join(dir, "main.go")
			watcher.config.DebounceMs = 60_000
			watcher.config.StormQuietPeriodMs = 60_000
			if tt.wantErr {
				tt.batch.FailedFiles = []string{path}
			}

			var gotPaths []string
			watcher.batchReindex = func(paths []string) (*IndexResult, error) {
				gotPaths = append([]string(nil), paths...)
				return tt.batch, nil
			}
			callbacks := 0
			watcher.OnSymbolChange(func(string, []*graph.Node, []*graph.Node) { callbacks++ })

			tickets := make([]*MutationTicket, 0, 2)
			for range 2 {
				ticket, err := watcher.EnqueueFileMutation(context.Background(), path)
				require.NoError(t, err)
				require.NotNil(t, ticket)
				tickets = append(tickets, ticket)
			}
			watcher.recordInStorm(path, ChangeModified)
			watcher.stormMu.Lock()
			require.NotNil(t, watcher.stormTimer)
			watcher.stopStormTimerLocked()
			watcher.stormMu.Unlock()

			watcher.drainStorm()

			require.Equal(t, []string{path}, gotPaths)
			for _, ticket := range tickets {
				select {
				case result, ok := <-ticket.Done:
					require.True(t, ok)
					require.Equal(t, ticket.Generation, result.RequestedGeneration)
					require.Equal(t, tickets[1].Generation, result.AppliedGeneration)
					require.Equal(t, !tt.wantErr, result.Reindexed)
					if tt.wantErr {
						require.ErrorContains(t, result.Err, tt.wantReason)
					} else {
						require.NoError(t, result.Err)
					}
				default:
					t.Fatalf("storm-adopted ticket generation %d did not complete synchronously", ticket.Generation)
				}
				_, open := <-ticket.Done
				require.False(t, open, "each adopted waiter must close exactly once")
			}

			watcher.mu.Lock()
			_, generationPending := watcher.pendingGeneration[path]
			_, waitersPending := watcher.mutationWaiters[path]
			watcher.mu.Unlock()
			require.False(t, generationPending)
			require.False(t, waitersPending)
			require.Zero(t, callbacks, "ticket handoff must not synthesize callback semantics")
			require.Empty(t, watcher.History(), "ticket handoff must not synthesize point-patch events")
		})
	}
}

func TestWatcherStormDrainWaitsForClaimedPointPatchLane(t *testing.T) {
	dir, idx, watcher := inertTestWatcher(t, "main.go", "package main\n\nfunc value() int { return 0 }\n")
	path := filepath.Join(dir, "main.go")
	writeTestFile(t, path, "package main\n\nfunc value() int { return 1 }\nfunc added() {}\n")

	pointTailEntered := make(chan struct{})
	releasePoint := make(chan struct{})
	released := false
	defer func() {
		if !released {
			close(releasePoint)
		}
	}()
	watcher.pointReindexRaw = func(filePath string, _ *PublicationStamps) (*IndexResult, error) {
		result, err := idx.incrementalPointWatcherPath(idx.rootPath, filePath)
		close(pointTailEntered)
		<-releasePoint
		return result, err
	}
	pointDone := make(chan error, 1)
	go func() {
		pointDone <- watcher.patchGraph(path, ChangeModified)
	}()

	select {
	case <-pointTailEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("point patch did not reach its coordinated tail")
	}

	watcher.stormMu.Lock()
	watcher.stormBatch[path] = ChangeModified
	watcher.stormMu.Unlock()
	stormAtAdmission := make(chan struct{})
	watcher.stormBeforeLock = func() { close(stormAtAdmission) }
	stormDone := make(chan struct{})
	go func() {
		watcher.drainStorm()
		close(stormDone)
	}()

	select {
	case <-stormAtAdmission:
	case <-time.After(2 * time.Second):
		t.Fatal("storm drain did not reach repository-lane admission")
	}
	select {
	case <-stormDone:
		t.Fatal("storm batch overlapped an admitted point patch")
	case <-time.After(100 * time.Millisecond):
	}

	close(releasePoint)
	released = true
	select {
	case err := <-pointDone:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("point patch did not finish after releasing its tail")
	}
	select {
	case <-stormDone:
	case <-time.After(2 * time.Second):
		t.Fatal("storm batch did not acquire the repository lane")
	}
}

func TestWatcherStopCancelsQueuedStormAndFailsAdoptedTickets(t *testing.T) {
	dir, _, watcher := inertTestWatcher(t, "main.go", "package main\n\nfunc Value() int { return 0 }\n")
	watcher.degradedNoFsnotify = true // Stop without a started fsnotify loop.
	watcher.config.DebounceMs = 60_000
	watcher.config.StormQuietPeriodMs = 60_000
	path := filepath.Join(dir, "main.go")

	batchEntered := make(chan struct{}, 1)
	watcher.batchReindex = func([]string) (*IndexResult, error) {
		batchEntered <- struct{}{}
		return &IndexResult{}, nil
	}
	tickets := make([]*MutationTicket, 0, 2)
	for range 2 {
		ticket, err := watcher.EnqueueFileMutation(context.Background(), path)
		require.NoError(t, err)
		require.NotNil(t, ticket)
		tickets = append(tickets, ticket)
	}
	watcher.recordInStorm(path, ChangeModified)

	require.NoError(t, watcher.Stop())
	for _, ticket := range tickets {
		result, ok := <-ticket.Done
		require.True(t, ok)
		require.Equal(t, ticket.Generation, result.RequestedGeneration)
		require.False(t, result.Reindexed)
		require.ErrorIs(t, result.Err, errWatcherStopped)
		_, open := <-ticket.Done
		require.False(t, open, "Stop must close each adopted ticket exactly once")
	}

	watcher.stormMu.Lock()
	require.True(t, watcher.stormStopped)
	require.Nil(t, watcher.stormTimer)
	require.Empty(t, watcher.stormBatch)
	require.Empty(t, watcher.stormGenerations)
	watcher.stormMu.Unlock()
	watcher.drainStorm() // A callback already queued by the runtime is inert.
	select {
	case <-batchEntered:
		t.Fatal("a storm canceled by Stop must not enter the batch pipeline")
	default:
	}
}

func TestWatcherStopJoinsInFlightStormDrain(t *testing.T) {
	dir, _, watcher := inertTestWatcher(t, "main.go", "package main\n\nfunc Value() int { return 0 }\n")
	watcher.degradedNoFsnotify = true // Stop without a started fsnotify loop.
	watcher.config.DebounceMs = 60_000
	watcher.config.StormQuietPeriodMs = 0
	path := filepath.Join(dir, "main.go")

	batchEntered := make(chan struct{})
	releaseBatch := make(chan struct{})
	batchFinished := make(chan struct{})
	watcher.batchReindex = func([]string) (*IndexResult, error) {
		close(batchEntered)
		<-releaseBatch
		close(batchFinished)
		return &IndexResult{StaleFileCount: 1}, nil
	}
	ticket, err := watcher.EnqueueFileMutation(context.Background(), path)
	require.NoError(t, err)
	require.NotNil(t, ticket)
	watcher.recordInStorm(path, ChangeModified)
	<-batchEntered

	stopDone := make(chan error, 1)
	go func() { stopDone <- watcher.Stop() }()
	result, ok := <-ticket.Done
	require.True(t, ok)
	require.Equal(t, ticket.Generation, result.RequestedGeneration)
	require.False(t, result.Reindexed)
	require.ErrorIs(t, result.Err, errWatcherStopped)
	_, open := <-ticket.Done
	require.False(t, open, "Stop and the drain must not both close an adopted ticket")
	select {
	case err := <-stopDone:
		t.Fatalf("Stop returned before the in-flight storm completed: %v", err)
	default:
	}

	close(releaseBatch)
	require.NoError(t, <-stopDone)
	select {
	case <-batchFinished:
	default:
		t.Fatal("Stop returned before the batch left graph/SQLite work")
	}
}

func TestIncrementalResolutionFrontierOnlySkipsAuthoritativeIrrelevantReceipt(t *testing.T) {
	result := &IndexResult{
		StaleFileCount: 1,
		DerivedInvalidation: DerivedInvalidationPlan{
			Files: []string{"repo/changed.go"},
		},
	}

	files, needed, exact := incrementalResolutionFrontier(result, &graph.MutationReceipt{
		Complete: false,
	})
	require.True(t, needed)
	require.False(t, exact)
	require.Equal(t, []string{"repo/changed.go"}, files,
		"an incomplete store receipt must take one scoped successful-file fallback")

	files, needed, exact = incrementalResolutionFrontier(result, &graph.MutationReceipt{
		Complete: true, ResolutionRelevant: false,
	})
	require.False(t, needed)
	require.True(t, exact)
	require.Empty(t, files, "only a complete irrelevant receipt may skip catch-up")

	files, needed, exact = incrementalResolutionFrontier(result, &graph.MutationReceipt{
		Complete: true, ResolutionRelevant: true,
		ChangedFiles:    []string{"repo/changed.go"},
		UnresolvedFiles: []string{"repo/changed.go"},
	})
	require.True(t, needed)
	require.True(t, exact)
	require.Equal(t, []string{"repo/changed.go"}, files)
}

func TestWatcherStormBatchDeletionFailureIsolationAndOneResolve(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("an unreadable-file test is meaningless as root")
	}

	dir := t.TempDir()
	good := filepath.Join(dir, "good.go")
	deleted := filepath.Join(dir, "deleted.go")
	bad := filepath.Join(dir, "bad.go")
	writeFile(t, good, "package storm\n\nfunc Good() {}\n")
	writeFile(t, deleted, "package storm\n\nfunc Deleted() {}\n")
	writeFile(t, bad, "package storm\n\nfunc Bad() {}\n")

	g := openTestSqlite(t)
	idx := newTestIndexer(g)
	_, err := idx.Index(dir)
	require.NoError(t, err)
	oldBadMtime := idx.FileMtimes()[idx.relKey(bad)]

	bumpMtime(t, good, "package storm\n\nfunc Good() {}\nfunc GoodCommitted() { MissingTarget() }\n")
	require.NoError(t, os.Remove(deleted))
	// Stale first, unreadable second: denyFileRead may hold the file open
	// exclusively, and Chtimes needs its own write handle.
	future := time.Now().Add(4 * time.Second)
	require.NoError(t, os.Chtimes(bad, future, future))
	denyFileRead(t, bad)

	resolveCalls := 0
	var resolvedFiles []string
	idx.incrementalResolveFilesHook = func(files []string) {
		resolveCalls++
		resolvedFiles = append([]string(nil), files...)
	}

	watcher, err := NewWatcher(idx, config.WatchConfig{}, zap.NewNop())
	require.NoError(t, err)
	watcher.stormBatch = map[string]ChangeKind{
		good:    ChangeModified,
		deleted: ChangeDeleted,
		bad:     ChangeModified,
	}
	drained := 0
	watcher.stormDrained = func(n int) { drained = n }
	watcher.drainStorm()

	require.Equal(t, 1, resolveCalls, "the surviving mutation frontier must resolve once")
	require.NotEmpty(t, resolvedFiles)
	require.Equal(t, 3, drained)
	require.NotEmpty(t, g.FindNodesByName("GoodCommitted"),
		"a readable sibling must commit even when another file fails")
	require.Empty(t, g.FindNodesByName("Deleted"), "deleted symbols must be evicted")
	require.NotEmpty(t, g.FindNodesByName("Bad"),
		"an unreadable file must retain its prior graph state")
	require.Equal(t, oldBadMtime, idx.FileMtimes()[idx.relKey(bad)],
		"a failed file must retain its retry watermark")
	_, trackedDeleted := idx.FileMtimes()[idx.relKey(deleted)]
	require.False(t, trackedDeleted, "a storm delete must prune its mtime receipt")
}

func TestMultiWatcherBatchCallbackRunsOneExactResolveAndDerivedCatchup(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "a.go")
	writeFile(t, file, "package callback\n\nfunc A() {}\n")

	g := graph.New()
	idx := newTestIndexer(g)
	idx.SetRepoPrefix("repo")
	_, err := idx.Index(dir)
	require.NoError(t, err)
	bumpMtime(t, file, "package callback\n\nfunc A() {}\nfunc Added() { MissingTarget() }\n")

	resolveCalls := 0
	var frontier []string
	idx.incrementalResolveFilesHook = func(files []string) {
		resolveCalls++
		frontier = append([]string(nil), files...)
	}
	crossRepoCalls := 0
	var crossRepoFrontier []string
	idx.incrementalCatchupHook = func(kind string, files []string) {
		if kind != "cross_repo" {
			return
		}
		crossRepoCalls++
		crossRepoFrontier = append([]string(nil), files...)
	}

	core, observed := observer.New(zap.InfoLevel)
	logger := zap.New(core)
	mi := &MultiIndexer{
		graph:    g,
		repos:    map[string]*RepoMetadata{"repo": {RepoPrefix: "repo", RootPath: dir, FileMtimes: idx.FileMtimes()}},
		indexers: map[string]*Indexer{"repo": idx},
		logger:   logger,
	}
	mw, err := NewMultiWatcher(mi, map[string]config.WatchConfig{"repo": {}}, logger)
	require.NoError(t, err)
	watcher := mw.watchers["repo"]
	require.NotNil(t, watcher)
	require.NotNil(t, watcher.batchReindex)

	result, err := watcher.batchReindex([]string{file})
	require.NoError(t, err)
	require.Equal(t, 1, result.StaleFileCount)
	require.Equal(t, 1, resolveCalls)
	require.NotEmpty(t, frontier)
	for _, graphPath := range frontier {
		require.True(t, strings.HasPrefix(graphPath, "repo/"),
			"receipt frontier must use repo-prefixed graph paths: %q", graphPath)
	}
	require.Equal(t, 1, crossRepoCalls)
	require.NotEmpty(t, crossRepoFrontier)
	for _, graphPath := range crossRepoFrontier {
		require.True(t, strings.HasPrefix(graphPath, "repo/"),
			"cross-repo frontier must use repo-prefixed graph paths: %q", graphPath)
	}
	require.Equal(t, 1, observed.FilterMessage("incremental derived passes complete").Len(),
		"one batch must trigger one derived catch-up")
}

func TestMultiWatcherThreeChunksRunEveryCatchupTailOnce(t *testing.T) {
	dir := t.TempDir()
	targetPath := filepath.Join(dir, "target.go")
	callerPath := filepath.Join(dir, "caller.go")
	writeFile(t, targetPath, "package tails\n\nfunc Target(x int) int { return x }\n")
	writeFile(t, callerPath, "package tails\n\nfunc Caller() int { return Target(1) }\n")

	const fillerCount = incrementalBatchFiles*2 + 1
	paths := make([]string, 0, fillerCount+1)
	paths = append(paths, targetPath)
	for i := 0; i < fillerCount; i++ {
		path := filepath.Join(dir, fmt.Sprintf("f%03d.go", i))
		writeFile(t, path, fmt.Sprintf("package tails\n\nfunc F%03d() int { return %d }\n", i, i))
		paths = append(paths, path)
	}

	g := openTestSqlite(t)
	idx := newTestIndexer(g)
	idx.SetRepoPrefix("repo")
	_, err := idx.Index(dir)
	require.NoError(t, err)
	callerID := fnNodeID(t, g, "repo/caller.go", "Caller")
	require.Equal(t, fnNodeID(t, g, "repo/target.go", "Target"), callTargetFrom(t, g, callerID))

	// The baseline full index deliberately ran without semantic providers. Clear
	// its legacy full-work marker before installing a watch provider so this
	// mutation proves one exact file batch rather than a repository fallback.
	idx.pendingEnrich.Store(false)
	idx.deferredEnrichMu.Lock()
	idx.deferredEnrichFiles = nil
	idx.deferredEnrichFull = false
	idx.deferredEnrichMu.Unlock()
	provider := &deferredBatchProvider{}
	manager := semantic.NewManager(semantic.Config{
		Enabled:       true,
		EnrichOnWatch: true,
		Providers: []semantic.ProviderConfig{{
			Name: provider.Name(), Languages: provider.Languages(), Priority: 1, Enabled: true,
		}},
	}, zap.NewNop())
	manager.RegisterProvider(provider)
	idx.SetSemanticManager(manager)

	tailCalls := make(map[string]int)
	var tailFiles = make(map[string][]string)
	idx.incrementalCatchupHook = func(kind string, files []string) {
		tailCalls[kind]++
		tailFiles[kind] = append([]string(nil), files...)
	}

	bumpMtime(t, targetPath, "package tails\n\nfunc Target(x int, y int) int { return x + y }\n")
	for i, path := range paths[1:] {
		bumpMtime(t, path, fmt.Sprintf(
			"package tails\n\nfunc F%03d() int { return %d }\nfunc Added%03d() {}\n", i, i+1, i,
		))
	}

	mi := &MultiIndexer{
		graph: g,
		repos: map[string]*RepoMetadata{"repo": {
			RepoPrefix: "repo", RootPath: dir, FileMtimes: idx.FileMtimes(),
		}},
		indexers: map[string]*Indexer{"repo": idx},
		logger:   zap.NewNop(),
	}
	idx.attachRepositoryMutationCoordinator(mi.repositoryMutationCoordinator("repo"))
	result, err := mi.IncrementalReindexRepo("repo", paths)
	require.NoError(t, err)
	require.Equal(t, len(paths), result.StaleFileCount)
	require.Empty(t, result.FailedFiles)

	for _, tail := range []string{"resolve", "dataflow", "affected_by", "ref_facts", "semantic", "cross_repo", "derived"} {
		require.Equalf(t, 1, tailCalls[tail], "%s tail must run once for the complete batch", tail)
	}
	require.Len(t, tailFiles["resolve"], len(paths),
		"the scoped fallback must contain exactly the successful mutation files")
	require.Equal(t, []string{"repo/caller.go"}, tailFiles["affected_by"],
		"the surviving caller must be handled once by the separate affected frontier")
	require.Len(t, tailFiles["cross_repo"], len(paths)+1,
		"the cross-repo batch must union the changed and affected file frontiers")
	require.Contains(t, tailFiles["cross_repo"], "repo/caller.go",
		"the cross-repo batch must include incoming edges owned by affected callers")
	require.Equal(t, 1, provider.batchCalls)
	require.Zero(t, provider.singleCalls)
	require.Zero(t, provider.fullCalls)
	require.Len(t, provider.batches, 1)
	require.Len(t, provider.batches[0], len(paths))

	require.Equal(t, fnNodeID(t, g, "repo/target.go", "Target"), callTargetFrom(t, g, callerID),
		"the one affected-by catch-up must preserve the caller binding")
}

func TestGitWatcherLargeBranchReconcileBatchesOnceAndPreservesFailureRetry(t *testing.T) {
	dir, baseSHA, targetSHA, expected := makeWatcherBranchFixture(t, 101)
	idx := newTestIndexer(graph.New())
	idx.rootPath = dir

	oldThreshold := gitWatcherScopedResolveMaxFiles
	gitWatcherScopedResolveMaxFiles = 10
	t.Cleanup(func() { gitWatcherScopedResolveMaxFiles = oldThreshold })

	t.Run("large branch parity", func(t *testing.T) {
		batchCalls := 0
		var got []string
		drained := 0
		gw := &GitWatcher{
			repoPath: dir,
			indexer:  idx,
			logger:   zap.NewNop(),
			lastSHA:  baseSHA,
			batchReindex: func(paths []string) (*IndexResult, error) {
				batchCalls++
				got = append([]string(nil), paths...)
				return &IndexResult{StaleFileCount: len(paths)}, nil
			},
			drained: func(n int) { drained = n },
		}

		gw.reconcile("branch-switch-test")

		require.Equal(t, 1, batchCalls)
		require.Greater(t, len(got), gitWatcherScopedResolveMaxFiles)
		require.Equal(t, len(got), drained)
		require.Equal(t, targetSHA, gw.lastSHA)
		gotSet := make(map[string]struct{}, len(got))
		for _, path := range got {
			gotSet[path] = struct{}{}
		}
		require.Len(t, gotSet, len(expected), "batch must deduplicate rename endpoints")
		for rel := range expected {
			_, ok := gotSet[filepath.Join(dir, rel)]
			require.True(t, ok, "missing branch-diff path %q", rel)
		}
	})

	t.Run("batch failure retains old sha", func(t *testing.T) {
		batchCalls := 0
		drained := 0
		gw := &GitWatcher{
			repoPath: dir,
			indexer:  idx,
			logger:   zap.NewNop(),
			lastSHA:  baseSHA,
			batchReindex: func(paths []string) (*IndexResult, error) {
				batchCalls++
				return nil, errors.New("injected batch failure")
			},
			drained: func(n int) { drained++ },
		}

		gw.reconcile("branch-switch-failure-test")

		require.Equal(t, 1, batchCalls)
		require.Equal(t, baseSHA, gw.lastSHA,
			"a failed batch must remain retryable from the prior commit")
		require.Zero(t, drained)
	})

	t.Run("partial batch failure retains old sha", func(t *testing.T) {
		batchCalls := 0
		drained := 0
		finalizeLookups := 0
		gw := &GitWatcher{
			repoPath: dir,
			indexer:  idx,
			currentIndexer: func() *Indexer {
				finalizeLookups++
				return idx
			},
			logger:  zap.NewNop(),
			lastSHA: baseSHA,
			batchReindex: func(paths []string) (*IndexResult, error) {
				batchCalls++
				return &IndexResult{
					StaleFileCount: len(paths) - 1,
					FailedFiles:    []string{paths[len(paths)-1]},
				}, nil
			},
			drained: func(n int) { drained++ },
		}

		gw.reconcile("branch-switch-partial-failure-test")

		require.Equal(t, 1, batchCalls)
		require.Equal(t, baseSHA, gw.lastSHA,
			"a partial batch failure must remain retryable from the prior commit")
		require.Zero(t, finalizeLookups, "failed files must skip freshness finalization")
		require.Zero(t, drained)
	})
}

func makeWatcherBranchFixture(t *testing.T, modified int) (dir, baseSHA, targetSHA string, expected map[string]struct{}) {
	t.Helper()
	dir = t.TempDir()
	runWatcherGit(t, dir, "init", "-q")
	runWatcherGit(t, dir, "config", "user.email", "watcher@example.test")
	runWatcherGit(t, dir, "config", "user.name", "Watcher Test")

	for i := 0; i < modified; i++ {
		name := fmt.Sprintf("f%03d.go", i)
		writeFile(t, filepath.Join(dir, name),
			fmt.Sprintf("package branch\n\nfunc F%03d() int { return %d }\n", i, i))
	}
	writeFile(t, filepath.Join(dir, "old.go"), "package branch\n\nfunc OldName() {}\n")
	writeFile(t, filepath.Join(dir, "deleted.go"), "package branch\n\nfunc DeletedName() {}\n")
	runWatcherGit(t, dir, "add", "-A")
	runWatcherGit(t, dir, "commit", "-q", "-m", "base")
	baseSHA = strings.TrimSpace(runWatcherGit(t, dir, "rev-parse", "HEAD"))
	runWatcherGit(t, dir, "branch", "base")
	runWatcherGit(t, dir, "checkout", "-q", "-b", "feature")

	expected = make(map[string]struct{}, modified+4)
	for i := 0; i < modified; i++ {
		name := fmt.Sprintf("f%03d.go", i)
		writeFile(t, filepath.Join(dir, name),
			fmt.Sprintf("package branch\n\nfunc F%03d() int { return %d }\nfunc Added%03d() {}\n", i, i+1, i))
		expected[name] = struct{}{}
	}
	require.NoError(t, os.Rename(filepath.Join(dir, "old.go"), filepath.Join(dir, "renamed.go")))
	require.NoError(t, os.Remove(filepath.Join(dir, "deleted.go")))
	writeFile(t, filepath.Join(dir, "added.go"), "package branch\n\nfunc BrandNewUnique() {}\n")
	expected["old.go"] = struct{}{}
	expected["renamed.go"] = struct{}{}
	expected["deleted.go"] = struct{}{}
	expected["added.go"] = struct{}{}

	runWatcherGit(t, dir, "add", "-A")
	runWatcherGit(t, dir, "commit", "-q", "-m", "feature")
	targetSHA = strings.TrimSpace(runWatcherGit(t, dir, "rev-parse", "HEAD"))
	// Exercise the same old→new HEAD movement a real branch checkout produces.
	runWatcherGit(t, dir, "checkout", "-q", "base")
	runWatcherGit(t, dir, "checkout", "-q", "feature")
	return dir, baseSHA, targetSHA, expected
}

func runWatcherGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "git %v: %s", args, out)
	return string(out)
}
