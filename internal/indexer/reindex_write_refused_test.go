package indexer

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/resolver"
	"github.com/zzet/gortex/internal/search"
)

type countingRefuseReindexStore struct {
	*store_sqlite.Store
	refuseFunc func(call int, batch []graph.EdgeReindex) bool
	calls      int
	refused    int
}

func (s *countingRefuseReindexStore) ReindexEdges(batch []graph.EdgeReindex) {
	s.ReindexEdgesApplied(batch)
}

func (s *countingRefuseReindexStore) ReindexEdgesApplied(batch []graph.EdgeReindex) bool {
	s.calls++
	if s.refuseFunc != nil && s.refuseFunc(s.calls, batch) {
		s.refused++
		return false
	}
	return s.Store.ReindexEdgesApplied(batch)
}

func newTestMultiIndexerWithRefusingStore(t *testing.T, refuseFunc func(int, []graph.EdgeReindex) bool) (*MultiIndexer, *countingRefuseReindexStore) {
	t.Helper()
	seed, baseStore := newSQLiteIndexer(t)
	store := &countingRefuseReindexStore{Store: baseStore, refuseFunc: refuseFunc}
	configFile := filepath.Join(t.TempDir(), "config.yaml")
	gc := &config.GlobalConfig{}
	gc.SetConfigPath(configFile)
	require.NoError(t, gc.Save())
	cm, err := config.NewConfigManager(configFile)
	require.NoError(t, err)
	mi := NewMultiIndexer(store, seed.registry, search.NewNull(), cm, zap.NewNop())
	return mi, store
}

func TestMultiIndexer_ResolveDeferredMutationsExactReceiptFailsClosedOnWriteRefused(t *testing.T) {
	mi, store := newTestMultiIndexerWithRefusingStore(t, func(call int, _ []graph.EdgeReindex) bool {
		return call == 1
	})
	store.AddBatch([]*graph.Node{
		{ID: "repo/caller.ts", Kind: graph.KindFile, Name: "caller.ts", FilePath: "repo/caller.ts", RepoPrefix: "repo", Language: "typescript"},
		{ID: "repo/caller.ts::run", Kind: graph.KindFunction, Name: "run", FilePath: "repo/caller.ts", RepoPrefix: "repo", Language: "typescript"},
		{ID: "repo/target.ts", Kind: graph.KindFile, Name: "target.ts", FilePath: "repo/target.ts", RepoPrefix: "repo", Language: "typescript"},
		{ID: "repo/target.ts::helper", Kind: graph.KindFunction, Name: "helper", FilePath: "repo/target.ts", RepoPrefix: "repo", Language: "typescript"},
	}, []*graph.Edge{
		{From: "repo/caller.ts", To: "repo/target.ts", Kind: graph.EdgeImports, FilePath: "repo/caller.ts", Line: 1},
		{From: "repo/caller.ts::run", To: "unresolved::helper", Kind: graph.EdgeCalls, FilePath: "repo/caller.ts", Line: 2},
	})

	receipt := &graph.MutationReceipt{
		Complete:           true,
		ResolutionRelevant: true,
		ChangedFiles:       []string{"repo/caller.ts"},
		DefinitionFiles:    []string{"repo/target.ts"},
	}

	mode, ok := mi.resolveDeferredMutations(receipt, false, nil, false)
	assert.Equal(t, deferredResolveFailed, mode)
	assert.False(t, ok, "refused write in exact-receipt branch must fail closed")
	assert.Equal(t, 1, store.refused)

	store.refuseFunc = nil
	mode, ok = mi.resolveDeferredMutations(receipt, false, nil, false)
	assert.Equal(t, deferredResolveExact, mode)
	assert.True(t, ok)
}

func TestMultiIndexer_RunPreEnrichResolveFilesReturnsErrOnWriteRefused(t *testing.T) {
	t.Run("master_refused_before_onComputeDone", func(t *testing.T) {
		mi, store := newTestMultiIndexerWithRefusingStore(t, func(call int, _ []graph.EdgeReindex) bool {
			return call == 1
		})
		store.AddBatch([]*graph.Node{
			{ID: "repo/caller.ts", Kind: graph.KindFile, Name: "caller.ts", FilePath: "repo/caller.ts", RepoPrefix: "repo", Language: "typescript"},
			{ID: "repo/caller.ts::run", Kind: graph.KindFunction, Name: "run", FilePath: "repo/caller.ts", RepoPrefix: "repo", Language: "typescript"},
			{ID: "repo/target.ts", Kind: graph.KindFile, Name: "target.ts", FilePath: "repo/target.ts", RepoPrefix: "repo", Language: "typescript"},
			{ID: "repo/target.ts::helper", Kind: graph.KindFunction, Name: "helper", FilePath: "repo/target.ts", RepoPrefix: "repo", Language: "typescript"},
		}, []*graph.Edge{
			{From: "repo/caller.ts", To: "repo/target.ts", Kind: graph.EdgeImports, FilePath: "repo/caller.ts", Line: 1},
			{From: "repo/caller.ts::run", To: "unresolved::helper", Kind: graph.EdgeCalls, FilePath: "repo/caller.ts", Line: 2},
		})

		computeDone := false
		err := mi.RunPreEnrichResolveFiles(context.Background(), []string{"repo/caller.ts"}, func() { computeDone = true })
		require.ErrorIs(t, err, resolver.ErrReindexWriteRefused)
		assert.False(t, computeDone, "readiness callback must not fire when master delta resolve refuses writes")
		assert.Equal(t, 1, store.refused)
	})

	t.Run("cross_repo_refused", func(t *testing.T) {
		mi, store := newTestMultiIndexerWithRefusingStore(t, func(call int, _ []graph.EdgeReindex) bool {
			return call == 1 // master has no same-repo match; cross-repo makes call #1
		})
		store.AddBatch([]*graph.Node{
			{ID: "a/caller.ts", Kind: graph.KindFile, Name: "caller.ts", FilePath: "a/caller.ts", RepoPrefix: "a", WorkspaceID: "a", Language: "typescript"},
			{ID: "a/caller.ts::run", Kind: graph.KindFunction, Name: "run", FilePath: "a/caller.ts", RepoPrefix: "a", WorkspaceID: "a", Language: "typescript"},
			{ID: "b/target.ts", Kind: graph.KindFile, Name: "target.ts", FilePath: "b/target.ts", RepoPrefix: "b", WorkspaceID: "a", Language: "typescript"},
			{ID: "b/target.ts::helper", Kind: graph.KindFunction, Name: "helper", FilePath: "b/target.ts", RepoPrefix: "b", WorkspaceID: "a", Language: "typescript"},
		}, []*graph.Edge{
			{From: "a/caller.ts", To: "b/target.ts", Kind: graph.EdgeImports, FilePath: "a/caller.ts", Line: 1, CrossRepo: true},
			{From: "a/caller.ts::run", To: "unresolved::helper", Kind: graph.EdgeCalls, FilePath: "a/caller.ts", Line: 2},
		})

		err := mi.RunPreEnrichResolveFiles(context.Background(), []string{"a/caller.ts"}, nil)
		require.ErrorIs(t, err, resolver.ErrReindexWriteRefused)
		assert.Equal(t, 1, store.refused)
	})
}

func TestIncrementalWatcherPaths_SkipsOnRelationsWhenResolutionRefused(t *testing.T) {
	root := t.TempDir()
	configPath := aliasRebindFixture(t, root)
	idx, store := newSQLiteIndexer(t)
	_, err := idx.Index(root)
	require.NoError(t, err)

	writeFileMk(t, configPath, `{"compilerOptions":{"paths":{"~/*":["new/*"]}}}`)
	restoreTimeout := store.SetBusyRetryTimeoutForTest(40 * time.Millisecond)
	defer restoreTimeout()
	contended := &gateHoldingReindexStore{Store: store}
	idx.resolver.SetGraph(contended)

	onRelationsCalled := false
	_, err = idx.incrementalWatcherPaths(root, []string{configPath}, incrementalPathMode{
		detectDeletions:    true,
		forceExplicitFiles: true,
		onRelations:        func() { onRelationsCalled = true },
	})
	idx.resolver.SetGraph(store)

	require.ErrorIs(t, err, resolver.ErrReindexWriteRefused)
	assert.False(t, onRelationsCalled, "onRelations must not stamp RelationsAt fresh when resolution refused writes")
}

func TestMultiIndexer_RunPreEnrichResolveCarriesPlaceholderRepointsAndGuardRevertsAcrossFreshResolvers(t *testing.T) {
	t.Run("placeholder_repoints", func(t *testing.T) {
		mi, store := newTestMultiIndexerWithRefusingStore(t, func(call int, _ []graph.EdgeReindex) bool {
			return call == 2 // chunk 1 commits call, applyDeferredApplied is refused
		})
		store.AddBatch([]*graph.Node{
			{ID: "repo/caller.ts", Kind: graph.KindFile, Name: "caller.ts", FilePath: "repo/caller.ts", RepoPrefix: "repo", Language: "typescript"},
			{ID: "repo/caller.ts::run", Kind: graph.KindFunction, Name: "run", FilePath: "repo/caller.ts", RepoPrefix: "repo", Language: "typescript"},
			{ID: "repo/callee.ts", Kind: graph.KindFile, Name: "callee.ts", FilePath: "repo/callee.ts", RepoPrefix: "repo", Language: "typescript"},
			{ID: "repo/callee.ts::first", Kind: graph.KindFunction, Name: "first", FilePath: "repo/callee.ts", RepoPrefix: "repo", Language: "typescript"},
		}, []*graph.Edge{
			{From: "repo/caller.ts", To: "repo/callee.ts", Kind: graph.EdgeImports, FilePath: "repo/caller.ts", Line: 1},
			{From: "repo/caller.ts::run", To: "unresolved::first", Kind: graph.EdgeCalls, FilePath: "repo/caller.ts", Line: 2},
			{From: "repo/unresolved::first", To: "repo/caller.ts::run", Kind: graph.EdgeArgOf, FilePath: "repo/caller.ts", Line: 2},
		})

		err := mi.RunPreEnrichResolve(context.Background(), nil, nil)
		require.ErrorIs(t, err, resolver.ErrReindexWriteRefused)
		assert.Equal(t, 2, store.calls)
		assert.Len(t, store.GetOutEdges("repo/unresolved::first"), 1)

		store.refuseFunc = nil
		require.NoError(t, mi.RunPreEnrichResolve(context.Background(), nil, nil))
		assert.Empty(t, store.GetOutEdges("repo/unresolved::first"), "fresh master resolver on retry must inherit and apply carried placeholder repoints")
		moved := store.GetOutEdges("repo/callee.ts::first")
		require.Len(t, moved, 1)
		assert.Equal(t, graph.EdgeArgOf, moved[0].Kind)
	})

	t.Run("guard_reverts", func(t *testing.T) {
		mi, store := newTestMultiIndexerWithRefusingStore(t, func(call int, _ []graph.EdgeReindex) bool {
			return call == 2 // phase 1 commits weak-tier call, guard revert is refused
		})
		store.AddBatch([]*graph.Node{
			{ID: "repo/a/caller.go", Kind: graph.KindFile, Name: "caller.go", FilePath: "repo/a/caller.go", RepoPrefix: "repo", Language: "go"},
			{ID: "repo/a/caller.go::Run", Kind: graph.KindFunction, Name: "Run", FilePath: "repo/a/caller.go", RepoPrefix: "repo", Language: "go"},
			{ID: "repo/b/unimported.go", Kind: graph.KindFile, Name: "unimported.go", FilePath: "repo/b/unimported.go", RepoPrefix: "repo", Language: "go"},
			{ID: "repo/b/unimported.go::Secret", Kind: graph.KindFunction, Name: "Secret", FilePath: "repo/b/unimported.go", RepoPrefix: "repo", Language: "go"},
			{ID: "repo/c/imported.go", Kind: graph.KindFile, Name: "imported.go", FilePath: "repo/c/imported.go", RepoPrefix: "repo", Language: "go"},
			{ID: "repo/c/imported.go::Other", Kind: graph.KindFunction, Name: "Other", FilePath: "repo/c/imported.go", RepoPrefix: "repo", Language: "go"},
		}, []*graph.Edge{
			{From: "repo/a/caller.go", To: "repo/c/imported.go", Kind: graph.EdgeImports, FilePath: "repo/a/caller.go", Line: 1},
			{From: "repo/a/caller.go::Run", To: "unresolved::Secret", Kind: graph.EdgeCalls, FilePath: "repo/a/caller.go", Line: 5},
		})

		err := mi.RunPreEnrichResolve(context.Background(), nil, nil)
		require.ErrorIs(t, err, resolver.ErrReindexWriteRefused)
		assert.Equal(t, 1, store.refused)

		store.refuseFunc = nil
		require.NoError(t, mi.RunPreEnrichResolve(context.Background(), nil, nil))
		edges := store.GetOutEdges("repo/a/caller.go::Run")
		require.Len(t, edges, 1)
		assert.Equal(t, "unresolved::Secret", edges[0].To, "fresh master resolver on retry must inherit and apply carried guard reverts")
		assert.Equal(t, true, edges[0].Meta["guard_reverted"])
	})
}

func TestWatcherPointRetryRefusedResolutionActuallyReresolves(t *testing.T) {
	t.Run("patch_receiptChecked_false_bypasses_duplicate_gate_via_pending_retry", func(t *testing.T) {
		root := t.TempDir()
		writeFileMk(t, filepath.Join(root, "lib.ts"), "export function helper() { return 1 }\n")
		callerPath := filepath.Join(root, "caller.ts")
		writeFileMk(t, callerPath, "import { helper } from './lib'\nexport function run() { return 0 }\n")

		idx, store := newSQLiteIndexer(t)
		_, err := idx.Index(root)
		require.NoError(t, err)

		w, err := NewWatcher(idx, config.WatchConfig{DebounceMs: 1, StormThreshold: -1, StormQuietPeriodMs: 1}, zap.NewNop())
		require.NoError(t, err)

		writeFileMk(t, callerPath, "import { helper } from './lib'\nexport function run() { return helper() }\n")

		restoreTimeout := store.SetBusyRetryTimeoutForTest(40 * time.Millisecond)
		defer restoreTimeout()
		contended := &gateHoldingReindexStore{Store: store}
		idx.resolver.SetGraph(contended)

		// Attempt 1 (receiptChecked=false): non-alias edit surfaces ErrReindexWriteRefused and records pending retry.
		err = w.patchGraphWithReceiptStateObserved(callerPath, ChangeModified, 0, false, nil, nil)
		idx.resolver.SetGraph(store)
		require.ErrorIs(t, err, resolver.ErrReindexWriteRefused, "non-alias edit must surface ErrReindexWriteRefused so watcher retries")
		assert.True(t, idx.hasPendingResolutionRetry())
		assert.Equal(t, "unresolved::helper", callTargetForName(t, store, "caller.ts", "run"))

		// Attempt 2 with receiptChecked=false exercises watcher.go's !idx.hasPendingResolutionRetry() gate directly.
		err = w.patchGraphWithReceiptStateObserved(callerPath, ChangeModified, 0, false, nil, nil)
		require.NoError(t, err)
		assert.False(t, idx.hasPendingResolutionRetry())
		assert.Equal(t, "lib.ts::helper", callTargetForName(t, store, "caller.ts", "run"))
	})

	t.Run("runPointMutationObserved_retryAttempt_bypasses_duplicate_gate_without_pending_retry", func(t *testing.T) {
		root := t.TempDir()
		configPath := aliasRebindFixture(t, root)
		callerPath := filepath.Join(root, "pages", "index.ts")
		idx, store := newSQLiteIndexer(t)
		_, err := idx.Index(root)
		require.NoError(t, err)
		require.Equal(t, "old/auth.ts::useRedirect", callTargetForName(t, store, "pages/index.ts", "run"))

		w, err := NewWatcher(idx, config.WatchConfig{DebounceMs: 1, StormThreshold: -1, StormQuietPeriodMs: 1}, zap.NewNop())
		require.NoError(t, err)

		// With callerPath and resolution inputs unchanged, pointDuplicateSourceContent(callerPath)
		// is provably true and hasPendingResolutionRetry() is false, so retryAttempt == 0 hits
		// the duplicate gate while retryAttempt > 0 -> receiptChecked=true bypasses it.
		dup, _ := idx.pointDuplicateSourceContent(context.Background(), callerPath)
		require.True(t, dup, "callerPath bytes must match persisted file_meta so the duplicate gate is armed")
		require.False(t, idx.hasPendingResolutionRetry())

		executorRan := false
		idx.incrementalCatchupHook = func(kind string, _ []string) {
			if kind == "derived" {
				executorRan = true
			}
		}
		w.runPointMutationObserved(callerPath, ChangeModified, 0, 0, nil)
		assert.False(t, executorRan, "retryAttempt == 0 must short-circuit at the duplicate gate")

		w.runPointMutationObserved(callerPath, ChangeModified, 0, 1, nil)
		assert.True(t, executorRan, "retryAttempt > 0 must bypass the duplicate gate without pendingResolutionRetry")
		idx.incrementalCatchupHook = nil

		// Updating tsconfig.json behind the scenes also invalidates fileMtimeMatches and rebinds on retry.
		writeFileMk(t, configPath, `{"compilerOptions":{"paths":{"~/*":["new/*"]}}}`)
		w.runPointMutationObserved(callerPath, ChangeModified, 0, 1, nil)
		assert.Equal(t, "new/auth.ts::useRedirect", callTargetForName(t, store, "pages/index.ts", "run"))
		digest, err := store.TSAliasDigest(idx.repoPrefix)
		require.NoError(t, err)
		assert.NotEmpty(t, digest)
	})
}

func TestMultiIndexer_RunMasterResolveNamesPreservesPendingRetryState(t *testing.T) {
	mi, store := newTestMultiIndexerWithRefusingStore(t, func(call int, _ []graph.EdgeReindex) bool {
		return call == 2 // #1 commits incoming call, #2 refuses reconcilePlaceholderSourcesApplied
	})
	store.AddBatch([]*graph.Node{
		{ID: "repo/caller.ts", Kind: graph.KindFile, Name: "caller.ts", FilePath: "repo/caller.ts", RepoPrefix: "repo", Language: "typescript"},
		{ID: "repo/caller.ts::run", Kind: graph.KindFunction, Name: "run", FilePath: "repo/caller.ts", RepoPrefix: "repo", Language: "typescript"},
		{ID: "repo/target.ts", Kind: graph.KindFile, Name: "target.ts", FilePath: "repo/target.ts", RepoPrefix: "repo", Language: "typescript"},
		{ID: "repo/target.ts::helper", Kind: graph.KindFunction, Name: "helper", FilePath: "repo/target.ts", RepoPrefix: "repo", Language: "typescript"},
	}, []*graph.Edge{
		{From: "repo/caller.ts", To: "repo/target.ts", Kind: graph.EdgeImports, FilePath: "repo/caller.ts", Line: 1},
		{From: "repo/caller.ts::run", To: "unresolved::helper", Kind: graph.EdgeCalls, FilePath: "repo/caller.ts", Line: 2},
		{From: "repo/unresolved::helper", To: "repo/caller.ts::run", Kind: graph.EdgeArgOf, FilePath: "repo/caller.ts", Line: 2},
	})

	stats := mi.runMasterResolveNames([]string{"helper"})
	require.NotNil(t, stats)
	assert.True(t, stats.WriteRefused)
	assert.Len(t, store.GetOutEdges("repo/unresolved::helper"), 1)

	store.refuseFunc = nil
	stats = mi.runMasterResolveNames([]string{"helper"})
	require.NotNil(t, stats)
	assert.False(t, stats.WriteRefused)
	assert.Empty(t, store.GetOutEdges("repo/unresolved::helper"), "runMasterResolveNames must hand off and flush retained placeholder repoints")
	moved := store.GetOutEdges("repo/target.ts::helper")
	require.Len(t, moved, 1)
	assert.Equal(t, graph.EdgeArgOf, moved[0].Kind)
}

func TestMultiIndexer_ConcurrentRepoLanesCannotStealPendingRetryStateOutsideResolveMutex(t *testing.T) {
	mi, store := newTestMultiIndexerWithRefusingStore(t, nil)
	mi.savePendingMasterRetry(resolver.MasterRetryState{
		PlaceholderRepoints: []graph.PlaceholderRepoint{
			{OldFrom: "repoB/unresolved::x", FilePath: "repoB/a.ts", Line: 2, NewFrom: "repoB/b.ts::x"},
		},
	})

	// Hold ResolveMutex so a competing repo lane blocks before entering resolution.
	mu := store.ResolveMutex()
	mu.Lock()
	entered := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		close(entered)
		mi.runMasterResolveFiles([]string{"repoA/other.ts"}, false)
	}()
	<-entered
	// Give the competing goroutine time to reach ResolveMutex.
	time.Sleep(25 * time.Millisecond)

	mi.pendingResolveRetryMu.Lock()
	queuedWhileBlocked := len(mi.pendingMasterRepoints)
	mi.pendingResolveRetryMu.Unlock()
	mu.Unlock()
	<-done

	assert.Equal(t, 1, queuedWhileBlocked, "competing repo lane must not dequeue mi pending retry state before acquiring ResolveMutex")
}

func TestIncrementalWatcherPaths_PendingRetryFullNotDowngradedByOverlappingEdit(t *testing.T) {
	root := t.TempDir()
	aPath := filepath.Join(root, "a.ts")
	bPath := filepath.Join(root, "b.ts")
	cPath := filepath.Join(root, "c.ts")
	writeFileMk(t, aPath, "export function a() { return 1 }\n")
	writeFileMk(t, bPath, "export function b() { return 2 }\n")
	writeFileMk(t, cPath, "import { a } from './a'\nexport function c() { return a() }\n")

	idx, store := newSQLiteIndexer(t)
	_, err := idx.Index(root)
	require.NoError(t, err)

	// Simulate an unrecovered full-graph resolution demand plus an unresolved edge in untouched c.ts.
	var cEdge *graph.Edge
	for _, e := range store.GetOutEdges("c.ts::c") {
		if e.Kind == graph.EdgeCalls {
			cEdge = e
			break
		}
	}
	require.NotNil(t, cEdge)
	oldTo := cEdge.To
	cEdge.To = "unresolved::a"
	require.True(t, store.ReindexEdgesApplied([]graph.EdgeReindex{{Edge: cEdge, OldTo: oldTo}}))

	idx.recordPendingResolutionRetry([]string{"a.ts"}, true, DerivedInvalidationPlan{Files: []string{"a.ts"}})

	// Overlapping edit to b.ts yields non-empty files=["b.ts"], yet retryFull must
	// still run full resolveAll() instead of downgrading to a file-scoped pass.
	writeFileMk(t, bPath, "export function b() { return 3 }\n")
	var resolveCatchups [][]string
	idx.incrementalCatchupHook = func(kind string, files []string) {
		if kind == "resolve" {
			resolveCatchups = append(resolveCatchups, files)
		}
	}
	res, err := idx.incrementalWatcherPaths(root, []string{bPath}, incrementalPathMode{
		detectDeletions:    true,
		forceExplicitFiles: true,
	})
	require.NoError(t, err)
	require.NotNil(t, res)
	assert.True(t, res.retriedResolution)
	assert.True(t, incrementalTopologyChanged(res))
	require.Len(t, resolveCatchups, 1)
	assert.Empty(t, resolveCatchups[0], "retryFull must dispatch whole-repo resolveAll() (nil frontier), not file-scoped resolution")
	assert.Equal(t, "a.ts::a", callTargetForName(t, store, "c.ts", "c"), "full resolveAll() retry must resolve untouched c.ts")
}

func TestIncrementalWatcherPaths_VanishedNameRefusalRetriesToFullResolution(t *testing.T) {
	root := t.TempDir()
	aPath := filepath.Join(root, "a.ts")
	bPath := filepath.Join(root, "b.ts")
	cPath := filepath.Join(root, "c.ts")
	writeFileMk(t, aPath, "export function foo() { return 1 }\n")
	writeFileMk(t, bPath, "export function foo() { return 2 }\n")
	writeFileMk(t, cPath, "import { foo } from './b'\nexport function run() { return foo() }\n")

	idx, store := newSQLiteIndexer(t)
	_, err := idx.Index(root)
	require.NoError(t, err)

	// Force c.ts::run -> a.ts::foo so removing foo from a.ts parks c.ts::run on
	// unresolved::foo and exercises resolveReceiptNamePendings(receipt).
	var runCall *graph.Edge
	for _, e := range store.GetOutEdges("c.ts::run") {
		if e.Kind == graph.EdgeCalls {
			runCall = e
			break
		}
	}
	require.NotNil(t, runCall)
	prevTo := runCall.To
	runCall.To = "a.ts::foo"
	require.True(t, store.ReindexEdgesApplied([]graph.EdgeReindex{{Edge: runCall, OldTo: prevTo}}))

	// Remove foo from a.ts; refuse writes during resolveReceiptNamePendings.
	writeFileMk(t, aPath, "export function bar() { return 1 }\n")
	refusing := &countingRefuseReindexStore{
		Store: store,
		refuseFunc: func(_ int, _ []graph.EdgeReindex) bool {
			return true
		},
	}
	idx.resolver.SetGraph(refusing)
	_, err = idx.incrementalWatcherPaths(root, []string{aPath}, incrementalPathMode{
		detectDeletions:    true,
		forceExplicitFiles: true,
	})
	idx.resolver.SetGraph(store)
	require.ErrorIs(t, err, resolver.ErrReindexWriteRefused)
	assert.Equal(t, "unresolved::foo", callTargetForName(t, store, "c.ts", "run"))

	// Attempt 2 on unchanged a.ts (inert reparse): must run full resolution and rebind c.ts::run -> b.ts::foo.
	res, err := idx.incrementalWatcherPaths(root, []string{aPath}, incrementalPathMode{
		detectDeletions:    true,
		forceExplicitFiles: true,
	})
	require.NoError(t, err)
	require.NotNil(t, res)
	assert.True(t, res.retriedResolution)
	assert.True(t, incrementalTopologyChanged(res), "inert retry pass that completed resolution must signal topologyChanged")
	assert.Equal(t, "b.ts::foo", callTargetForName(t, store, "c.ts", "run"))
}

func TestIncrementalWatcherPaths_InertRetryPreservesDerivedInvalidationAndTopologyChange(t *testing.T) {
	root := t.TempDir()
	writeFileMk(t, filepath.Join(root, "lib.ts"), "export function helper() { return 1 }\n")
	callerPath := filepath.Join(root, "caller.ts")
	writeFileMk(t, callerPath, "import { helper } from './lib'\nexport function run() { return 0 }\n")

	idx, store := newSQLiteIndexer(t)
	_, err := idx.Index(root)
	require.NoError(t, err)

	writeFileMk(t, callerPath, "import { helper } from './lib'\nexport function run() { return helper() }\n")
	restoreTimeout := store.SetBusyRetryTimeoutForTest(40 * time.Millisecond)
	defer restoreTimeout()
	idx.resolver.SetGraph(&gateHoldingReindexStore{Store: store})
	_, err = idx.incrementalWatcherPaths(root, []string{callerPath}, incrementalPathMode{
		detectDeletions:    true,
		forceExplicitFiles: true,
	})
	idx.resolver.SetGraph(store)
	require.ErrorIs(t, err, resolver.ErrReindexWriteRefused)

	var derivedFiles []string
	idx.incrementalCatchupHook = func(kind string, files []string) {
		if kind == "derived" {
			derivedFiles = append(derivedFiles, files...)
		}
	}
	// Attempt 2 sees identical bytes on disk (inert reparse) yet must still signal
	// topologyChanged and carry the retried file into DerivedInvalidation.
	res, err := idx.incrementalWatcherPaths(root, []string{callerPath}, incrementalPathMode{
		detectDeletions:    true,
		forceExplicitFiles: true,
	})
	require.NoError(t, err)
	require.NotNil(t, res)
	assert.True(t, res.retriedResolution)
	assert.True(t, incrementalTopologyChanged(res), "successful inert retry must invalidate reachability topology")
	assert.Contains(t, res.DerivedInvalidation.Files, "caller.ts")
	assert.True(t, res.DerivedInvalidation.Flags.Has(DerivedInvalidatesTests))
	assert.Contains(t, derivedFiles, "caller.ts")
	assert.Equal(t, "lib.ts::helper", callTargetForName(t, store, "caller.ts", "run"))
}

func TestMultiIndexer_RetryFullWideStillRunsWholeGraphCrossRepoAndRetryFileFacts(t *testing.T) {
	mi, store := newTestMultiIndexerWithRefusingStore(t, nil)
	idxA := mi.newPerRepoIndexerGuardedWithMode(config.IndexConfig{}, multiIndexerBatchMode{})
	idxA.SetRepoPrefix("repoA")
	mi.mu.Lock()
	mi.indexers["repoA"] = idxA
	mi.mu.Unlock()

	store.AddBatch([]*graph.Node{
		{ID: "repoA/a.ts", Kind: graph.KindFile, Name: "a.ts", FilePath: "repoA/a.ts", RepoPrefix: "repoA", WorkspaceID: "ws", Language: "typescript"},
		{ID: "repoA/b.ts", Kind: graph.KindFile, Name: "b.ts", FilePath: "repoA/b.ts", RepoPrefix: "repoA", WorkspaceID: "ws", Language: "typescript"},
		{ID: "repoA/c.ts", Kind: graph.KindFile, Name: "c.ts", FilePath: "repoA/c.ts", RepoPrefix: "repoA", WorkspaceID: "ws", Language: "typescript"},
		{ID: "repoA/c.ts::run", Kind: graph.KindFunction, Name: "run", FilePath: "repoA/c.ts", RepoPrefix: "repoA", WorkspaceID: "ws", Language: "typescript"},
		{ID: "repoB/target.ts", Kind: graph.KindFile, Name: "target.ts", FilePath: "repoB/target.ts", RepoPrefix: "repoB", WorkspaceID: "ws", Language: "typescript"},
		{ID: "repoB/target.ts::helper", Kind: graph.KindFunction, Name: "helper", FilePath: "repoB/target.ts", RepoPrefix: "repoB", WorkspaceID: "ws", Language: "typescript"},
	}, []*graph.Edge{
		{From: "repoA/c.ts", To: "repoB/target.ts", Kind: graph.EdgeImports, FilePath: "repoA/c.ts", Line: 1, CrossRepo: true},
		{From: "repoA/c.ts::run", To: "unresolved::helper", Kind: graph.EdgeCalls, FilePath: "repoA/c.ts", Line: 2},
	})

	idxA.recordPendingResolutionRetry([]string{"repoA/a.ts"}, true, DerivedInvalidationPlan{Files: []string{"repoA/a.ts"}})

	var refFactFiles []string
	idxA.incrementalCatchupHook = func(kind string, files []string) {
		if kind == "ref_facts" {
			refFactFiles = append(refFactFiles, files...)
		}
	}
	// Overlapping edit to repoA/b.ts populates result.DerivedInvalidation.Files = ["repoA/b.ts"].
	res := &IndexResult{
		DerivedInvalidation: DerivedInvalidationPlan{Files: []string{"repoA/b.ts"}},
	}
	batch := &reparsePendingEnrichmentBatch{deferResolverCatchup: true}
	returned := mi.resolveIncrementalRepoMutationMode("repoA", res, nil, batch, false)
	assert.Nil(t, returned, "wide retry must return nil frontier")
	assert.True(t, res.retriedResolution)
	assert.Contains(t, refFactFiles, "repoA/a.ts", "wide retry must preserve retryFiles for ref_facts refresh")
	var callTarget string
	for _, e := range store.GetOutEdges("repoA/c.ts::run") {
		if e.Kind == graph.EdgeCalls {
			callTarget = e.To
		}
	}
	assert.Equal(t, "repoB/target.ts::helper", callTarget, "wide retry must run whole-graph cross-repo resolve even when crossRepoFiles is non-empty")
}

func TestMultiIndexer_PerRepoIndexerResolverBridgesRetryStateToFreshMaster(t *testing.T) {
	mi, store := newTestMultiIndexerWithRefusingStore(t, func(call int, _ []graph.EdgeReindex) bool {
		return call == 2 // #1 commits outgoing call on idx.resolver, #2 refuses reconcilePlaceholderSourcesApplied
	})
	idx := mi.newPerRepoIndexerGuardedWithMode(config.IndexConfig{}, multiIndexerBatchMode{})
	store.AddBatch([]*graph.Node{
		{ID: "repo/caller.ts", Kind: graph.KindFile, Name: "caller.ts", FilePath: "repo/caller.ts", RepoPrefix: "repo", Language: "typescript"},
		{ID: "repo/caller.ts::run", Kind: graph.KindFunction, Name: "run", FilePath: "repo/caller.ts", RepoPrefix: "repo", Language: "typescript"},
		{ID: "repo/target.ts", Kind: graph.KindFile, Name: "target.ts", FilePath: "repo/target.ts", RepoPrefix: "repo", Language: "typescript"},
		{ID: "repo/target.ts::helper", Kind: graph.KindFunction, Name: "helper", FilePath: "repo/target.ts", RepoPrefix: "repo", Language: "typescript"},
	}, []*graph.Edge{
		{From: "repo/caller.ts", To: "repo/target.ts", Kind: graph.EdgeImports, FilePath: "repo/caller.ts", Line: 1},
		{From: "repo/caller.ts::run", To: "unresolved::helper", Kind: graph.EdgeCalls, FilePath: "repo/caller.ts", Line: 2},
		{From: "repo/unresolved::helper", To: "repo/caller.ts::run", Kind: graph.EdgeArgOf, FilePath: "repo/caller.ts", Line: 2},
	})

	// Simulates executeAffectedByPlan calling idx.resolver.ResolveFilesOutgoing directly.
	stats := idx.resolver.ResolveFilesOutgoing([]string{"repo/caller.ts"})
	require.NotNil(t, stats)
	assert.True(t, stats.WriteRefused)
	assert.Len(t, store.GetOutEdges("repo/unresolved::helper"), 1)

	// Attempt 2 runs through a fresh master resolver on MultiIndexer; it must inherit
	// the repoint queued by idx.resolver via SetRetryStateBridge.
	store.refuseFunc = nil
	require.NoError(t, mi.RunPreEnrichResolveFiles(context.Background(), []string{"repo/caller.ts"}, nil))
	assert.Empty(t, store.GetOutEdges("repo/unresolved::helper"), "fresh master resolver must inherit retry state bridged from per-repo idx.resolver")
	moved := store.GetOutEdges("repo/target.ts::helper")
	require.Len(t, moved, 1)
	assert.Equal(t, graph.EdgeArgOf, moved[0].Kind)
}

func TestIncrementalWatcherPaths_AffectedByRefusalRetainsAffectedFilesForRetry(t *testing.T) {
	root := t.TempDir()
	defPath := filepath.Join(root, "def.go")
	helperPath := filepath.Join(root, "helper.go")
	callerPath := filepath.Join(root, "caller.go")
	writeFileMk(t, defPath, "package demo\n\nfunc Calc(x int) int { return x }\n")
	writeFileMk(t, helperPath, "package demo\n\nfunc Other() int { return 2 }\n")
	writeFileMk(t, callerPath, "package demo\n\nfunc Run() int { return Calc(1) }\n")

	idx, store := newSQLiteIndexer(t)
	_, err := idx.Index(root)
	require.NoError(t, err)
	require.Equal(t, "def.go::Calc", callTargetForName(t, store, "caller.go", "Run"))

	// Change Calc's signature (a genuine contract change putting caller.go into
	// batch.deferredAffectedPlan().files) AND add an unresolved call to helper.go::Other
	// in caller.go (untouched by def.go's incoming leg, so ONLY ResolveFilesOutgoing
	// on affected_by resolves it).
	writeFileMk(t, defPath, "package demo\n\nfunc Calc(x, y int) int { return x + y }\n")
	store.AddBatch(nil, []*graph.Edge{
		{From: "caller.go::Run", To: "unresolved::Other", Kind: graph.EdgeCalls, FilePath: "caller.go", Line: 99},
	})

	// Refuse writes only when ResolveFilesOutgoing runs on affected_by (caller.go).
	var inAffected bool
	var observedAffected []string
	idx.incrementalCatchupHook = func(kind string, files []string) {
		inAffected = kind == "affected_by"
		if kind == "affected_by" {
			observedAffected = append(observedAffected, files...)
		}
	}
	refusing := &countingRefuseReindexStore{
		Store: store,
		refuseFunc: func(_ int, _ []graph.EdgeReindex) bool {
			return inAffected
		},
	}
	idx.resolver.SetGraph(refusing)
	_, err = idx.incrementalWatcherPaths(root, []string{defPath}, incrementalPathMode{
		detectDeletions:    true,
		forceExplicitFiles: true,
	})
	idx.resolver.SetGraph(store)
	require.NotEmpty(t, observedAffected, "signature change must enqueue caller.go in affected_by plan")
	require.ErrorIs(t, err, resolver.ErrReindexWriteRefused)

	// Attempt 2 on unchanged def.go (inert reparse): affected_by plan is now empty,
	// so ONLY retaining factFiles from Attempt 1 re-resolves caller.go's edge!
	idx.incrementalCatchupHook = nil
	res, err := idx.incrementalWatcherPaths(root, []string{defPath}, incrementalPathMode{
		detectDeletions:    true,
		forceExplicitFiles: true,
	})
	require.NoError(t, err)
	require.NotNil(t, res)
	assert.True(t, res.retriedResolution)
	var line99Target string
	for _, e := range store.GetOutEdges("caller.go::Run") {
		if e.Line == 99 {
			line99Target = e.To
		}
	}
	assert.Equal(t, "helper.go::Other", line99Target, "Attempt 2 must include affected.files retained from Attempt 1 refusal")
}

func TestMultiIndexer_ShadowBuildSetGraphDoesNotStealOrWipePendingMasterRetry(t *testing.T) {
	mi, store := newTestMultiIndexerWithRefusingStore(t, nil)
	store.AddBatch([]*graph.Node{
		{ID: "repoA/caller.ts", Kind: graph.KindFile, Name: "caller.ts", FilePath: "repoA/caller.ts", RepoPrefix: "repoA", Language: "typescript"},
		{ID: "repoA/caller.ts::run", Kind: graph.KindFunction, Name: "run", FilePath: "repoA/caller.ts", RepoPrefix: "repoA", Language: "typescript"},
		{ID: "repoA/target.ts", Kind: graph.KindFile, Name: "target.ts", FilePath: "repoA/target.ts", RepoPrefix: "repoA", Language: "typescript"},
		{ID: "repoA/target.ts::helper", Kind: graph.KindFunction, Name: "helper", FilePath: "repoA/target.ts", RepoPrefix: "repoA", Language: "typescript"},
	}, []*graph.Edge{
		{From: "repoA/caller.ts", To: "repoA/target.ts", Kind: graph.EdgeImports, FilePath: "repoA/caller.ts", Line: 1},
		{From: "repoA/caller.ts::run", To: "repoA/target.ts::helper", Kind: graph.EdgeCalls, FilePath: "repoA/caller.ts", Line: 2},
		{From: "repoA/unresolved::helper", To: "repoA/caller.ts::run", Kind: graph.EdgeArgOf, FilePath: "repoA/caller.ts", Line: 2},
	})

	mi.savePendingMasterRetry(resolver.MasterRetryState{
		PlaceholderRepoints: []graph.PlaceholderRepoint{
			{OldFrom: "repoA/unresolved::helper", FilePath: "repoA/caller.ts", Line: 2, NewFrom: "repoA/target.ts::helper"},
		},
		GuardReverts: []resolver.PendingGuardRevert{
			{Reindexes: []graph.EdgeReindex{{Edge: &graph.Edge{From: "repoA/a.go::F", To: "unresolved::G", Kind: graph.EdgeCalls, FilePath: "repoA/a.go", Line: 5}, OldTo: "repoA/b.go::G"}}},
		},
		TailRetryAll:     true,
		TailRetryScopes:  []string{"repoA"},
		AttributionFiles: []string{"repoA/ext.go"},
	})

	idxB := mi.newPerRepoIndexerGuardedWithMode(config.IndexConfig{}, multiIndexerBatchMode{})
	inMemShadow := graph.New()
	inMemShadow.AddBatch([]*graph.Node{
		{ID: "repoB/caller.ts", Kind: graph.KindFile, Name: "caller.ts", FilePath: "repoB/caller.ts", RepoPrefix: "repoB", Language: "typescript"},
		{ID: "repoB/caller.ts::run", Kind: graph.KindFunction, Name: "run", FilePath: "repoB/caller.ts", RepoPrefix: "repoB", Language: "typescript"},
		{ID: "repoB/target.ts", Kind: graph.KindFile, Name: "target.ts", FilePath: "repoB/target.ts", RepoPrefix: "repoB", Language: "typescript"},
		{ID: "repoB/target.ts::ok", Kind: graph.KindFunction, Name: "ok", FilePath: "repoB/target.ts", RepoPrefix: "repoB", Language: "typescript"},
	}, []*graph.Edge{
		{From: "repoB/caller.ts", To: "repoB/target.ts", Kind: graph.EdgeImports, FilePath: "repoB/caller.ts", Line: 1},
		{From: "repoB/caller.ts::run", To: "unresolved::ok", Kind: graph.EdgeCalls, FilePath: "repoB/caller.ts", Line: 2},
	})

	// Simulate idx.indexCtxRaw redirecting idxB.resolver onto inMemShadow, then
	// hold durable store.ResolveMutex() while inMemShadow resolves to prove
	// shadow resolution uses inMemShadow's mutex and never touches mi.pendingMaster*.
	idxB.resolver.SetGraph(inMemShadow)
	storeMu := store.ResolveMutex()
	storeMu.Lock()
	_, err := idxB.resolver.ResolveAllContext(context.Background())
	storeMu.Unlock()
	require.NoError(t, err)
	idxB.resolver.SetGraph(store)

	// Every pendingMaster* field must survive the shadow ResolveAllContext completely untouched.
	mi.pendingResolveRetryMu.Lock()
	assert.Len(t, mi.pendingMasterRepoints, 1, "shadow build must not steal or clear durable PlaceholderRepoints")
	assert.Len(t, mi.pendingMasterGuardReverts, 1, "shadow build must not steal or clear durable GuardReverts")
	assert.True(t, mi.pendingMasterTailRetryAll, "shadow build must not wipe durable TailRetryAll")
	assert.Contains(t, mi.pendingMasterTailScopes, "repoA", "shadow build must not wipe durable TailRetryScopes")
	assert.Contains(t, mi.pendingMasterAttribFiles, "repoA/ext.go", "shadow build must not wipe durable AttributionFiles")
	mi.pendingResolveRetryMu.Unlock()

	// Once SetGraph(store) restores the durable store, the bridge resumes automatically.
	stats := idxB.resolver.ResolveFilesOutgoing([]string{"repoA/caller.ts"})
	require.NotNil(t, stats)
	assert.False(t, stats.WriteRefused)
	assert.Empty(t, store.GetOutEdges("repoA/unresolved::helper"), "restored durable resolver must resume retry bridge and flush repoints")
	assert.Len(t, store.GetOutEdges("repoA/target.ts::helper"), 1)
}

func TestIncrementalWatcherPaths_OverlappingBodyEditOnPendingRetryStillInfersOverrides(t *testing.T) {
	root := t.TempDir()
	basePath := filepath.Join(root, "base.ts")
	subPath := filepath.Join(root, "sub.ts")
	writeFileMk(t, basePath, "export class Base {\n  run() { return 1 }\n}\n")
	writeFileMk(t, subPath, "export class Sub {\n  run() { return 0 }\n}\n")

	idx, store := newSQLiteIndexer(t)
	_, err := idx.Index(root)
	require.NoError(t, err)

	countOverrides := func() int {
		n := 0
		for range store.EdgesByKind(graph.EdgeOverrides) {
			n++
		}
		return n
	}

	// Attempt 1 adds extends Base under resolution write refusal: EdgeExtends stays
	// unresolved::Base, so Attempt 1's derived pass produces 0 EdgeOverrides.
	writeFileMk(t, subPath, "import { Base } from './base'\nexport class Sub extends Base {\n  run() { return 2 }\n}\n")
	restoreTimeout := store.SetBusyRetryTimeoutForTest(40 * time.Millisecond)
	defer restoreTimeout()
	idx.resolver.SetGraph(&gateHoldingReindexStore{Store: store})
	_, err = idx.incrementalWatcherPaths(root, []string{subPath}, incrementalPathMode{
		detectDeletions:    true,
		forceExplicitFiles: true,
	})
	idx.resolver.SetGraph(store)
	require.ErrorIs(t, err, resolver.ErrReindexWriteRefused)
	assert.Zero(t, countOverrides(), "unresolved extends on Attempt 1 produces no EdgeOverrides")

	// Overlapping body-only edit to sub.ts leaves its hierarchy fingerprint unchanged
	// relative to Attempt 1, arming HierarchyUnchanged + empty capabilityPrior.hierarchy.
	writeFileMk(t, subPath, "import { Base } from './base'\nexport class Sub extends Base {\n  run() { return 3 }\n}\n")
	res, err := idx.incrementalWatcherPaths(root, []string{subPath}, incrementalPathMode{
		detectDeletions:    true,
		forceExplicitFiles: true,
	})
	require.NoError(t, err)
	require.NotNil(t, res)
	assert.True(t, res.retriedResolution)
	assert.Empty(t, res.DerivedInvalidation.HierarchyUnchanged)
	assert.Nil(t, res.capabilityPrior)
	assert.Positive(t, countOverrides(), "Attempt 2 must clear stale HierarchyUnchanged/capabilityPrior so InferOverridesForFrontier emits EdgeOverrides")
}

func TestIncrementalTopologyChanged_TrueOnChainedRetriedResolutionRefusal(t *testing.T) {
	assert.True(t, incrementalTopologyChanged(&IndexResult{
		StaleFileCount: 1,
		DerivedInvalidation: DerivedInvalidationPlan{
			InertFiles: 1,
		},
		retriedResolution: true,
		resolutionErr:     resolver.ErrReindexWriteRefused,
	}), "chained Attempt 2 refusal on an inert reparse must still invalidate reachability topology")
}
