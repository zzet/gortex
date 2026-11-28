package indexer

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"github.com/zzet/gortex/internal/resolver"
	"github.com/zzet/gortex/internal/search"
)

type countingRefuseReindexStore struct {
	*store_sqlite.Store
	mu                sync.Mutex
	refuseFunc        func(call int, batch []graph.EdgeReindex) bool
	calls             int
	refused           int
	repoPrefixesCalls atomic.Int64
}

func (s *countingRefuseReindexStore) ReindexEdges(batch []graph.EdgeReindex) {
	s.ReindexEdgesTryApplied(batch, 0)
}

func (s *countingRefuseReindexStore) ReindexEdgesApplied(batch []graph.EdgeReindex) bool {
	return s.ReindexEdgesTryApplied(batch, 0)
}

func (s *countingRefuseReindexStore) ReindexEdgesTryApplied(batch []graph.EdgeReindex, maxWait time.Duration) bool {
	s.mu.Lock()
	s.calls++
	call := s.calls
	fn := s.refuseFunc
	s.mu.Unlock()
	if fn != nil && fn(call, batch) {
		s.mu.Lock()
		s.refused++
		s.mu.Unlock()
		return false
	}
	return s.Store.ReindexEdgesTryApplied(batch, maxWait)
}

func (s *countingRefuseReindexStore) RepoPrefixes() []string {
	s.repoPrefixesCalls.Add(1)
	return s.Store.RepoPrefixes()
}

func (s *countingRefuseReindexStore) setRefuseFunc(fn func(int, []graph.EdgeReindex) bool) {
	s.mu.Lock()
	s.refuseFunc = fn
	s.mu.Unlock()
}

func (s *countingRefuseReindexStore) refusedCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.refused
}

func (s *countingRefuseReindexStore) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func refuseOnceMatching(pred func([]graph.EdgeReindex) bool) func(int, []graph.EdgeReindex) bool {
	var fired atomic.Bool
	return func(_ int, batch []graph.EdgeReindex) bool {
		if pred == nil || !pred(batch) {
			return false
		}
		return fired.CompareAndSwap(false, true)
	}
}

func batchHasEdge(pred func(graph.EdgeReindex) bool) func([]graph.EdgeReindex) bool {
	return func(batch []graph.EdgeReindex) bool {
		for _, ri := range batch {
			if pred(ri) {
				return true
			}
		}
		return false
	}
}

func batchHasCallTo(target string) func([]graph.EdgeReindex) bool {
	return batchHasEdge(func(ri graph.EdgeReindex) bool {
		return ri.Edge != nil && ri.Edge.Kind == graph.EdgeCalls && ri.Edge.To == target
	})
}

func batchHasPlaceholderMove(oldFrom, newFrom string) func([]graph.EdgeReindex) bool {
	return batchHasEdge(func(ri graph.EdgeReindex) bool {
		return ri.OldFrom == oldFrom && ri.Edge != nil && ri.Edge.From == newFrom
	})
}

func batchHasGuardRevert(callerID string) func([]graph.EdgeReindex) bool {
	return batchHasEdge(func(ri graph.EdgeReindex) bool {
		return ri.Edge != nil && ri.Edge.From == callerID && ri.Edge.Meta != nil && ri.Edge.Meta["guard_reverted"] == true
	})
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
	mi, store := newTestMultiIndexerWithRefusingStore(t, refuseOnceMatching(batchHasCallTo("repo/target.ts::helper")))
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
	assert.Equal(t, 1, store.refusedCount())

	store.setRefuseFunc(nil)
	mode, ok = mi.resolveDeferredMutations(receipt, false, nil, false)
	assert.Equal(t, deferredResolveExact, mode)
	assert.True(t, ok)
}

func TestMultiIndexer_RunPreEnrichResolveFilesReturnsErrOnWriteRefused(t *testing.T) {
	t.Run("master_refused_before_onComputeDone", func(t *testing.T) {
		mi, store := newTestMultiIndexerWithRefusingStore(t, refuseOnceMatching(batchHasCallTo("repo/target.ts::helper")))
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
		assert.Equal(t, 1, store.refusedCount())
	})

	t.Run("cross_repo_refused", func(t *testing.T) {
		mi, store := newTestMultiIndexerWithRefusingStore(t, refuseOnceMatching(batchHasCallTo("b/target.ts::helper")))
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
		assert.Equal(t, 1, store.refusedCount())
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
	onRelationsRefused := false
	_, err = idx.incrementalWatcherPaths(root, []string{configPath}, incrementalPathMode{
		detectDeletions:    true,
		forceExplicitFiles: true,
		onRelations: func(refused bool) {
			onRelationsCalled = true
			onRelationsRefused = refused
		},
	})
	idx.resolver.SetGraph(store)

	require.ErrorIs(t, err, resolver.ErrReindexWriteRefused)
	assert.True(t, onRelationsCalled, "onRelations must report refused rebind state")
	assert.True(t, onRelationsRefused, "onRelations must signal refused=true so RelationsAt stays unobserved")
}

func TestMultiIndexer_RunPreEnrichResolveCarriesPlaceholderRepointsAndGuardRevertsAcrossFreshResolvers(t *testing.T) {
	t.Run("placeholder_repoints", func(t *testing.T) {
		mi, store := newTestMultiIndexerWithRefusingStore(t, refuseOnceMatching(batchHasPlaceholderMove("repo/unresolved::first", "repo/callee.ts::first")))
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
		assert.Equal(t, 1, store.refusedCount())
		assert.Equal(t, 2, store.callCount())
		assert.Len(t, store.GetOutEdges("repo/unresolved::first"), 1)

		store.setRefuseFunc(nil)
		require.NoError(t, mi.RunPreEnrichResolve(context.Background(), nil, nil))
		assert.Empty(t, store.GetOutEdges("repo/unresolved::first"), "fresh master resolver on retry must inherit and apply carried placeholder repoints")
		moved := store.GetOutEdges("repo/callee.ts::first")
		require.Len(t, moved, 1)
		assert.Equal(t, graph.EdgeArgOf, moved[0].Kind)
	})

	t.Run("guard_reverts", func(t *testing.T) {
		mi, store := newTestMultiIndexerWithRefusingStore(t, refuseOnceMatching(batchHasGuardRevert("repo/a/caller.go::Run")))
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
		assert.Equal(t, 1, store.refusedCount())

		store.setRefuseFunc(nil)
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
		configPath := aliasRebindFixture(t, root)
		callerPath := filepath.Join(root, "pages", "index.ts")
		idx, store := newSQLiteIndexer(t)
		_, err := idx.Index(root)
		require.NoError(t, err)
		require.Equal(t, "old/auth.ts::useRedirect", callTargetForName(t, store, "pages/index.ts", "run"))

		w, err := NewWatcher(idx, config.WatchConfig{DebounceMs: 1, StormThreshold: -1, StormQuietPeriodMs: 1}, zap.NewNop())
		require.NoError(t, err)

		writeFileMk(t, configPath, `{"compilerOptions":{"paths":{"~/*":["new/*"]}}}`)

		restoreTimeout := store.SetBusyRetryTimeoutForTest(40 * time.Millisecond)
		defer restoreTimeout()
		contended := &gateHoldingReindexStore{Store: store}
		idx.resolver.SetGraph(contended)

		// Attempt 1 (receiptChecked=false): uncertified alias edit surfaces ErrReindexWriteRefused and records pending retry.
		err = w.patchGraphWithReceiptStateObserved(configPath, ChangeModified, 0, false, nil, nil)
		idx.resolver.SetGraph(store)
		require.ErrorIs(t, err, resolver.ErrReindexWriteRefused, "uncertified alias edit must surface ErrReindexWriteRefused so watcher retries")
		assert.True(t, idx.hasPendingResolutionRetry())
		assert.Equal(t, "unresolved::useRedirect", callTargetForName(t, store, "pages/index.ts", "run"))

		// Attempt 2 on unchanged callerPath with receiptChecked=false exercises watcher.go's !idx.hasPendingResolutionRetry() gate directly.
		dup, _ := idx.pointDuplicateSourceContent(context.Background(), callerPath)
		require.True(t, dup, "callerPath bytes must match persisted file_meta so the duplicate gate is armed")
		err = w.patchGraphWithReceiptStateObserved(callerPath, ChangeModified, 0, false, nil, nil)
		require.NoError(t, err)
		assert.False(t, idx.hasPendingResolutionRetry())
		assert.Equal(t, "new/auth.ts::useRedirect", callTargetForName(t, store, "pages/index.ts", "run"))
		digest, err := store.TSAliasDigest(idx.repoPrefix)
		require.NoError(t, err)
		assert.NotEmpty(t, digest)
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

		// Update tsconfig.json behind the scenes while leaving callerPath unchanged on disk:
		// pointDuplicateSourceContent(callerPath) is provably true and hasPendingResolutionRetry() is false,
		// so ONLY the retry flag (retryAttempt > 0) can bypass the duplicate gate; receiptChecked stays false.
		writeFileMk(t, configPath, `{"compilerOptions":{"paths":{"~/*":["new/*"]}}}`)
		dup, _ := idx.pointDuplicateSourceContent(context.Background(), callerPath)
		require.True(t, dup, "callerPath bytes must match persisted file_meta so the duplicate gate is armed")
		require.False(t, idx.hasPendingResolutionRetry())

		w.runPointMutationObserved(callerPath, ChangeModified, 0, 1, nil)
		assert.Equal(t, "new/auth.ts::useRedirect", callTargetForName(t, store, "pages/index.ts", "run"))
		digest, err := store.TSAliasDigest(idx.repoPrefix)
		require.NoError(t, err)
		assert.NotEmpty(t, digest)
	})
}

func TestMultiIndexer_RunMasterResolveNamesPreservesPendingRetryState(t *testing.T) {
	mi, store := newTestMultiIndexerWithRefusingStore(t, refuseOnceMatching(batchHasPlaceholderMove("repo/unresolved::helper", "repo/target.ts::helper")))
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
	assert.Equal(t, 1, store.refusedCount())
	assert.Len(t, store.GetOutEdges("repo/unresolved::helper"), 1)

	store.setRefuseFunc(nil)
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
	store.AddBatch([]*graph.Node{
		{ID: "repoB/other.ts", Kind: graph.KindFile, Name: "other.ts", FilePath: "repoB/other.ts", RepoPrefix: "repoB", Language: "typescript"},
	}, nil)
	mi.mu.Lock()
	mi.repos["repoB"] = &RepoMetadata{RepoPrefix: "repoB"}
	mi.mu.Unlock()
	mi.savePendingMasterRetry(resolver.MasterRetryState{
		PlaceholderRepoints: []graph.PlaceholderRepoint{
			{OldFrom: "repoB/unresolved::x", FilePath: "repoB/a.ts", Line: 2, NewFrom: "repoB/b.ts::x", RepoPrefix: "repoB"},
		},
	}, resolver.RetryBridgeSaveMeta{Refused: true})

	atLock := make(chan struct{})
	var signalOnce sync.Once
	mi.beforeMasterResolveLock = func() {
		signalOnce.Do(func() { close(atLock) })
	}
	master := mi.newMasterResolver(false)
	require.NotNil(t, master)
	master.SetRetryBridgeRepoPrefix("repoB")
	require.Equal(t, 1, mi.PendingRetryBacklogStats().PlaceholderRepoints, "constructing a fresh master resolver must not dequeue pending retry state")

	// Hold ResolveMutex so the competing same-repo lane blocks strictly at r.mu.Lock().
	mu := store.ResolveMutex()
	mu.Lock()
	done := make(chan struct{})
	go func() {
		defer close(done)
		master.ResolveFilesAndIncoming([]string{"repoB/other.ts"})
	}()
	<-atLock

	queuedWhileBlocked := mi.PendingRetryBacklogStats().PlaceholderRepoints
	mu.Unlock()
	<-done

	assert.Equal(t, 1, queuedWhileBlocked, "competing repo lane must not dequeue mi pending retry state before acquiring ResolveMutex")
	assert.Equal(t, 0, mi.PendingRetryBacklogStats().PlaceholderRepoints, "after ResolveMutex unlocks, same-repo lane must dequeue and drain the queued repoint")
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
	aPath := filepath.Join(root, "a", "a.go")
	bPath := filepath.Join(root, "b.go")
	cPath := filepath.Join(root, "c.go")
	writeFileMk(t, aPath, "package a\nfunc Foo() int { return 1 }\n")
	writeFileMk(t, bPath, "package demo\nfunc Foo() int { return 2 }\n")
	writeFileMk(t, cPath, "package demo\nfunc Run() int { return Foo() }\n")

	idx, store := newSQLiteIndexer(t)
	_, err := idx.Index(root)
	require.NoError(t, err)
	require.Equal(t, "b.go::Foo", callTargetForName(t, store, "c.go", "Run"))

	// Force c.go::Run -> a/a.go::Foo so removing Foo from a/a.go parks c.go::Run on
	// unresolved::Foo and exercises resolveReceiptNamePendings(receipt).
	var runCall *graph.Edge
	for _, e := range store.GetOutEdges("c.go::Run") {
		if e.Kind == graph.EdgeCalls {
			runCall = e
			break
		}
	}
	require.NotNil(t, runCall)
	prevTo := runCall.To
	runCall.To = "a/a.go::Foo"
	require.True(t, store.ReindexEdgesApplied([]graph.EdgeReindex{{Edge: runCall, OldTo: prevTo}}))

	// Pure Go repo (no tsconfig / alias pass): remove Foo from a/a.go and refuse
	// the vanished-name rebind write by content.
	writeFileMk(t, aPath, "package a\nfunc Bar() int { return 1 }\n")
	refusing := &countingRefuseReindexStore{
		Store:      store,
		refuseFunc: refuseOnceMatching(batchHasCallTo("b.go::Foo")),
	}
	idx.resolver.SetGraph(refusing)
	res1, err := idx.incrementalWatcherPaths(root, []string{aPath}, incrementalPathMode{
		detectDeletions:    true,
		forceExplicitFiles: true,
	})
	idx.resolver.SetGraph(store)
	require.NoError(t, err)
	require.NotNil(t, res1)
	assert.True(t, res1.ResolutionWriteRefused)
	assert.Equal(t, 1, refusing.refusedCount())
	assert.Equal(t, "unresolved::Foo", callTargetForName(t, store, "c.go", "Run"))

	// Attempt 2 on unchanged a/a.go (inert reparse): must run full resolution and rebind c.go::Run -> b.go::Foo.
	res, err := idx.incrementalWatcherPaths(root, []string{aPath}, incrementalPathMode{
		detectDeletions:    true,
		forceExplicitFiles: true,
	})
	require.NoError(t, err)
	require.NotNil(t, res)
	assert.False(t, res.ResolutionWriteRefused)
	assert.True(t, res.retriedResolution)
	assert.True(t, incrementalTopologyChanged(res), "inert retry pass that completed resolution must signal topologyChanged")
	assert.Equal(t, "b.go::Foo", callTargetForName(t, store, "c.go", "Run"))
}

func TestIncrementalWatcherPaths_InertRetryPreservesDerivedInvalidationAndTopologyChange(t *testing.T) {
	root := t.TempDir()
	writeFileMk(t, filepath.Join(root, "lib.go"), "package demo\nfunc Helper() int { return 1 }\n")
	callerPath := filepath.Join(root, "caller.go")
	writeFileMk(t, callerPath, "package demo\nfunc Run() int { return 0 }\n")

	idx, store := newSQLiteIndexer(t)
	_, err := idx.Index(root)
	require.NoError(t, err)

	// Pure Go repo (no tsconfig / alias pass): edit caller.go and hold the SQLite writer gate.
	writeFileMk(t, callerPath, "package demo\nfunc Run() int { return Helper() }\n")
	restoreTimeout := store.SetBusyRetryTimeoutForTest(40 * time.Millisecond)
	defer restoreTimeout()
	idx.resolver.SetGraph(&gateHoldingReindexStore{Store: store})
	res1, err := idx.incrementalWatcherPaths(root, []string{callerPath}, incrementalPathMode{
		detectDeletions:    true,
		forceExplicitFiles: true,
	})
	idx.resolver.SetGraph(store)
	require.NoError(t, err)
	require.NotNil(t, res1)
	assert.True(t, res1.ResolutionWriteRefused)

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
	assert.False(t, res.ResolutionWriteRefused)
	assert.True(t, res.retriedResolution)
	assert.True(t, incrementalTopologyChanged(res), "successful inert retry must invalidate reachability topology")
	assert.Contains(t, res.DerivedInvalidation.Files, "caller.go")
	assert.True(t, res.DerivedInvalidation.Flags.Has(DerivedInvalidatesTests))
	assert.Contains(t, derivedFiles, "caller.go")
	assert.Equal(t, "lib.go::Helper", callTargetForName(t, store, "caller.go", "Run"))
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
	returned, _, _ := mi.resolveIncrementalRepoMutationMode("repoA", res, nil, batch, false)
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
	mi, store := newTestMultiIndexerWithRefusingStore(t, refuseOnceMatching(batchHasPlaceholderMove("repo/unresolved::helper", "repo/target.ts::helper")))
	idx := mi.newPerRepoIndexerGuardedWithMode(config.IndexConfig{}, multiIndexerBatchMode{})
	idx.SetRepoPrefix("repo")
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
	assert.Equal(t, 1, store.refusedCount())
	assert.Len(t, store.GetOutEdges("repo/unresolved::helper"), 1)

	// Attempt 2 runs through a fresh master resolver on MultiIndexer; it must inherit
	// the repoint queued by idx.resolver via SetRetryStateBridge.
	store.setRefuseFunc(nil)
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

	// Pure Go repo (no tsconfig / alias pass): change Calc's signature (a genuine
	// contract change putting caller.go into batch.deferredAffectedPlan().files),
	// AND add an unresolved call to helper.go::Other in caller.go (untouched by
	// def.go's incoming leg, so ONLY ResolveFilesOutgoing on affected_by resolves it).
	writeFileMk(t, defPath, "package demo\n\nfunc Calc(x, y int) int { return x + y }\n")
	store.AddBatch(nil, []*graph.Edge{
		{From: "caller.go::Run", To: "unresolved::Other", Kind: graph.EdgeCalls, FilePath: "caller.go", Line: 99},
	})

	// Refuse writes only when affected_by resolves caller.go::Run -> helper.go::Other.
	var observedAffected []string
	idx.incrementalCatchupHook = func(kind string, files []string) {
		if kind == "affected_by" {
			observedAffected = append(observedAffected, files...)
		}
	}
	refusing := &countingRefuseReindexStore{
		Store:      store,
		refuseFunc: refuseOnceMatching(batchHasCallTo("helper.go::Other")),
	}
	idx.resolver.SetGraph(refusing)
	res1, err := idx.incrementalWatcherPaths(root, []string{defPath}, incrementalPathMode{
		detectDeletions:    true,
		forceExplicitFiles: true,
	})
	idx.resolver.SetGraph(store)
	require.NotEmpty(t, observedAffected, "signature change must enqueue caller.go in affected_by plan")
	require.NoError(t, err)
	require.NotNil(t, res1)
	assert.True(t, res1.ResolutionWriteRefused)
	assert.Equal(t, 1, refusing.refusedCount())

	// Attempt 2 on unchanged def.go (inert reparse): affected_by plan is now empty,
	// so ONLY retaining factFiles from Attempt 1 re-resolves caller.go's edge!
	idx.incrementalCatchupHook = nil
	res, err := idx.incrementalWatcherPaths(root, []string{defPath}, incrementalPathMode{
		detectDeletions:    true,
		forceExplicitFiles: true,
	})
	require.NoError(t, err)
	require.NotNil(t, res)
	assert.False(t, res.ResolutionWriteRefused)
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
	mi.mu.Lock()
	mi.repos["repoA"] = &RepoMetadata{RepoPrefix: "repoA"}
	mi.mu.Unlock()
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
			{OldFrom: "repoA/unresolved::helper", FilePath: "repoA/caller.ts", Line: 2, NewFrom: "repoA/target.ts::helper", RepoPrefix: "repoA"},
		},
		GuardReverts: []resolver.PendingGuardRevert{
			{RepoPrefix: "repoA", Reindexes: []graph.EdgeReindex{{Edge: &graph.Edge{From: "repoA/a.go::F", To: "unresolved::G", Kind: graph.EdgeCalls, FilePath: "repoA/a.go", Line: 5}, OldTo: "repoA/b.go::G"}}},
		},
		TailRetryAll:     true,
		TailRetryScopes:  []string{"repoA"},
		AttributionFiles: []string{"repoA/ext.go"},
	}, resolver.RetryBridgeSaveMeta{Refused: true})

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
	// shadow resolution uses inMemShadow's mutex and never touches mi's retry map.
	idxB.resolver.SetGraph(inMemShadow)
	storeMu := store.ResolveMutex()
	storeMu.Lock()
	_, err := idxB.resolver.ResolveAllContext(context.Background())
	storeMu.Unlock()
	require.NoError(t, err)
	idxB.resolver.SetGraph(store)

	// Every pending retry field must survive the shadow ResolveAllContext completely untouched.
	backlog := mi.PendingRetryBacklogStats()
	assert.Equal(t, 1, backlog.PlaceholderRepoints, "shadow build must not steal or clear durable PlaceholderRepoints")
	assert.Equal(t, 1, backlog.GuardReverts, "shadow build must not steal or clear durable GuardReverts")
	assert.True(t, backlog.TailRetryAll, "shadow build must not wipe durable TailRetryAll")
	assert.Equal(t, 1, backlog.TailRetryScopes, "shadow build must not wipe durable TailRetryScopes")
	assert.Equal(t, 1, backlog.AttributionFiles, "shadow build must not wipe durable AttributionFiles")
	assert.True(t, backlog.ByRepo["repoA"].TailRetryScope)

	// Once SetGraph(store) restores the durable store, the bridge resumes automatically.
	stats := idxB.resolver.ResolveFilesOutgoing([]string{"repoA/caller.ts"})
	require.NotNil(t, stats)
	assert.False(t, stats.WriteRefused)
	assert.Empty(t, store.GetOutEdges("repoA/unresolved::helper"), "restored durable resolver must resume retry bridge and flush repoints")
	assert.Len(t, store.GetOutEdges("repoA/target.ts::helper"), 1)
}

func TestIncrementalWatcherPaths_OverlappingBodyEditOnPendingRetryStillInfersOverrides(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, "tsconfig.json")
	writeFileMk(t, configPath, `{"compilerOptions":{"paths":{"~dummy/*":["v1/*"]}}}`)
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

	// Attempt 1 adds extends Base under an uncertified alias pass and resolution write refusal:
	// EdgeExtends stays unresolved::Base, so Attempt 1's derived pass produces 0 EdgeOverrides.
	writeFileMk(t, configPath, `{"compilerOptions":{"paths":{"~dummy/*":["v2/*"]}}}`)
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

// Vanished receipt names widen the master resolve (the frontier returned is
// nil) but, with no carried full retry, the cross-repo pass stays scoped to the
// mutation frontier. Only a carried full retry escalates it to the whole graph.
func TestMultiIndexer_VanishedNamesKeepCrossRepoPassScoped(t *testing.T) {
	for _, tc := range []struct {
		name      string
		retryFull bool
	}{
		{name: "vanished names only"},
		{name: "carried full retry", retryFull: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mi, store := newTestMultiIndexerWithRefusingStore(t, nil)
			idxA := mi.newPerRepoIndexerGuardedWithMode(config.IndexConfig{}, multiIndexerBatchMode{})
			idxA.SetRepoPrefix("repoA")
			mi.mu.Lock()
			mi.indexers["repoA"] = idxA
			mi.mu.Unlock()

			store.AddBatch([]*graph.Node{
				{ID: "repoA/b.ts", Kind: graph.KindFile, Name: "b.ts", FilePath: "repoA/b.ts", RepoPrefix: "repoA", WorkspaceID: "ws", Language: "typescript"},
				{ID: "repoA/c.ts", Kind: graph.KindFile, Name: "c.ts", FilePath: "repoA/c.ts", RepoPrefix: "repoA", WorkspaceID: "ws", Language: "typescript"},
				{ID: "repoA/c.ts::run", Kind: graph.KindFunction, Name: "run", FilePath: "repoA/c.ts", RepoPrefix: "repoA", WorkspaceID: "ws", Language: "typescript"},
				{ID: "repoB/target.ts", Kind: graph.KindFile, Name: "target.ts", FilePath: "repoB/target.ts", RepoPrefix: "repoB", WorkspaceID: "ws", Language: "typescript"},
				{ID: "repoB/target.ts::helper", Kind: graph.KindFunction, Name: "helper", FilePath: "repoB/target.ts", RepoPrefix: "repoB", WorkspaceID: "ws", Language: "typescript"},
			}, []*graph.Edge{
				{From: "repoA/c.ts", To: "repoB/target.ts", Kind: graph.EdgeImports, FilePath: "repoA/c.ts", Line: 1, CrossRepo: true},
				{From: "repoA/c.ts::run", To: "unresolved::helper", Kind: graph.EdgeCalls, FilePath: "repoA/c.ts", Line: 2},
			})
			if tc.retryFull {
				idxA.recordPendingResolutionRetry([]string{"repoA/b.ts"}, true, DerivedInvalidationPlan{Files: []string{"repoA/b.ts"}})
			}

			var crossRepo [][]string
			graphPasses := 0
			idxA.incrementalCatchupHook = func(kind string, files []string) {
				switch kind {
				case "cross_repo":
					crossRepo = append(crossRepo, files)
				case "cross_repo_graph":
					graphPasses++
				}
			}
			res := &IndexResult{DerivedInvalidation: DerivedInvalidationPlan{Files: []string{"repoA/b.ts"}}}
			receipt := &graph.MutationReceipt{
				Complete:           true,
				ResolutionRelevant: true,
				UnresolvedFiles:    []string{"repoA/b.ts"},
				EvictedNames:       []string{"gone"},
			}
			batch := &reparsePendingEnrichmentBatch{deferResolverCatchup: true}
			returned, _, _ := mi.resolveIncrementalRepoMutationMode("repoA", res, receipt, batch, false)
			assert.Nil(t, returned, "a pass that reached past its frontier returns a nil frontier")
			require.NotEmpty(t, crossRepo)

			var callTarget string
			for _, e := range store.GetOutEdges("repoA/c.ts::run") {
				if e.Kind == graph.EdgeCalls {
					callTarget = e.To
				}
			}
			if tc.retryFull {
				assert.Equal(t, 1, graphPasses, "a carried full retry runs the whole-graph cross-repo pass")
				assert.Equal(t, "repoB/target.ts::helper", callTarget)
				return
			}
			assert.Zero(t, graphPasses, "vanished names alone must not run the whole-graph cross-repo pass")
			assert.Contains(t, crossRepo[0], "repoA/b.ts")
			assert.Equal(t, "unresolved::helper", callTarget, "a scoped pass must not touch c.ts, which is outside the frontier")
		})
	}
}

func TestIncrementalWatcherPaths_CertifiedAliasRepoStillRetriesRefusedWrite(t *testing.T) {
	root := t.TempDir()
	_ = aliasRebindFixture(t, root)
	callerPath := filepath.Join(root, "pages", "index.ts")
	idx, store := newSQLiteIndexer(t)
	_, err := idx.Index(root)
	require.NoError(t, err)
	require.Equal(t, "old/auth.ts::useRedirect", callTargetForName(t, store, "pages/index.ts", "run"))
	// Alias digest is already certified; tsconfig.json is NOT modified.
	// Add a new caller function so structural reparse emits a fresh unresolved::useRedirect
	// edge rather than preserving run()'s existing resolved edge via body-only edit delta.
	writeFileMk(t, callerPath, "import { useRedirect } from '~/auth'\nexport function run() { return useRedirect() }\nexport function run2() { return useRedirect() }\n")

	refusing := &countingRefuseReindexStore{
		Store:      store,
		refuseFunc: refuseOnceMatching(batchHasCallTo("old/auth.ts::useRedirect")),
	}
	idx.resolver.SetGraph(refusing)
	res1, err := idx.incrementalWatcherPaths(root, []string{callerPath}, incrementalPathMode{
		detectDeletions:    true,
		forceExplicitFiles: true,
	})
	idx.resolver.SetGraph(store)
	require.NoError(t, err)
	require.NotNil(t, res1)
	assert.True(t, res1.ResolutionWriteRefused, "certified alias repo must flag ResolutionWriteRefused")
	assert.Equal(t, 1, refusing.refusedCount())
	assert.True(t, idx.hasPendingResolutionRetry())
	assert.Equal(t, "unresolved::useRedirect", callTargetForName(t, store, "pages/index.ts", "run2"))

	// Retry on unchanged bytes heals the refused write.
	res, err := idx.incrementalWatcherPaths(root, []string{callerPath}, incrementalPathMode{
		detectDeletions:    true,
		forceExplicitFiles: true,
	})
	require.NoError(t, err)
	require.NotNil(t, res)
	assert.False(t, res.ResolutionWriteRefused)
	assert.True(t, res.retriedResolution)
	assert.False(t, idx.hasPendingResolutionRetry())
	assert.Equal(t, "old/auth.ts::useRedirect", callTargetForName(t, store, "pages/index.ts", "run2"))
}

func TestMultiIndexer_GoOnlyIncrementalRefusalRetriesWithoutAliasChange(t *testing.T) {
	root := t.TempDir()
	libPath := filepath.Join(root, "lib.go")
	callerPath := filepath.Join(root, "caller.go")
	writeFileMk(t, libPath, "package demo\nfunc Helper() int { return 1 }\n")
	writeFileMk(t, callerPath, "package demo\nfunc Run() int { return 0 }\n")

	mi, store := newTestMultiIndexerWithRefusingStore(t, nil)
	_, err := mi.TrackRepoCtx(context.Background(), config.RepoEntry{Path: root, Name: "repo"})
	require.NoError(t, err)

	writeFileMk(t, callerPath, "package demo\nfunc Run() int { return Helper() }\n")
	store.setRefuseFunc(refuseOnceMatching(batchHasCallTo("repo/lib.go::Helper")))
	res1, err := mi.incrementalReindexRepoRawMode("repo", []string{callerPath}, incrementalPathMode{
		detectDeletions:    true,
		forceExplicitFiles: true,
	})
	require.NoError(t, err)
	require.NotNil(t, res1)
	assert.True(t, res1.ResolutionWriteRefused)
	assert.Equal(t, 1, store.refusedCount())
	assert.True(t, mi.GetIndexer("repo").hasPendingResolutionRetry())
	assert.Equal(t, "unresolved::Helper", callTargetForName(t, store, "repo/caller.go", "Run"))

	// Opt-in returnResolutionRefusal mode (used by Watcher point/storm retries) surfaces ErrReindexWriteRefused.
	store.setRefuseFunc(refuseOnceMatching(batchHasCallTo("repo/lib.go::Helper")))
	resOptIn, err := mi.incrementalReindexRepoRawMode("repo", []string{callerPath}, incrementalPathMode{
		detectDeletions:         true,
		forceExplicitFiles:      true,
		returnResolutionRefusal: true,
	})
	require.ErrorIs(t, err, resolver.ErrReindexWriteRefused)
	require.NotNil(t, resOptIn)
	assert.True(t, resOptIn.ResolutionWriteRefused)
	assert.Equal(t, 2, store.refusedCount())
	assert.True(t, mi.GetIndexer("repo").hasPendingResolutionRetry())

	store.setRefuseFunc(nil)
	res, err := mi.incrementalReindexRepoRawMode("repo", []string{callerPath}, incrementalPathMode{
		detectDeletions:    true,
		forceExplicitFiles: true,
	})
	require.NoError(t, err)
	require.NotNil(t, res)
	assert.False(t, res.ResolutionWriteRefused)
	assert.True(t, res.retriedResolution)
	assert.False(t, mi.GetIndexer("repo").hasPendingResolutionRetry())
	assert.Equal(t, "repo/lib.go::Helper", callTargetForName(t, store, "repo/caller.go", "Run"))
}

func TestWatcherPointRetry_GoOnlyRefusedResolutionRetries(t *testing.T) {
	root := t.TempDir()
	libPath := filepath.Join(root, "lib.go")
	callerPath := filepath.Join(root, "caller.go")
	writeFileMk(t, libPath, "package demo\nfunc Helper() int { return 1 }\n")
	writeFileMk(t, callerPath, "package demo\nfunc Run() int { return 0 }\n")

	idx, store := newSQLiteIndexer(t)
	_, err := idx.Index(root)
	require.NoError(t, err)

	w, err := NewWatcher(idx, config.WatchConfig{DebounceMs: 1, StormThreshold: -1, StormQuietPeriodMs: 1}, zap.NewNop())
	require.NoError(t, err)

	writeFileMk(t, callerPath, "package demo\nfunc Run() int { return Helper() }\n")
	refusing := &countingRefuseReindexStore{
		Store:      store,
		refuseFunc: refuseOnceMatching(batchHasCallTo("lib.go::Helper")),
	}
	idx.resolver.SetGraph(refusing)
	err = w.patchGraphWithReceiptStateObserved(callerPath, ChangeModified, 0, false, nil, nil)
	idx.resolver.SetGraph(store)
	require.ErrorIs(t, err, resolver.ErrReindexWriteRefused)
	assert.Equal(t, 1, refusing.refusedCount())
	assert.True(t, idx.hasPendingResolutionRetry())
	assert.Equal(t, "unresolved::Helper", callTargetForName(t, store, "caller.go", "Run"))

	// Same bytes on retry must bypass duplicate gate via hasPendingResolutionRetry() and bind Helper().
	dup, _ := idx.pointDuplicateSourceContent(context.Background(), callerPath)
	require.True(t, dup)
	err = w.patchGraphWithReceiptStateObserved(callerPath, ChangeModified, 0, false, nil, nil)
	require.NoError(t, err)
	assert.False(t, idx.hasPendingResolutionRetry())
	assert.Equal(t, "lib.go::Helper", callTargetForName(t, store, "caller.go", "Run"))
}

func TestMultiIndexer_IncompleteReceiptRefusalRetriesFull(t *testing.T) {
	mi, store := newTestMultiIndexerWithRefusingStore(t, nil)
	idxA := mi.newPerRepoIndexerGuardedWithMode(config.IndexConfig{}, multiIndexerBatchMode{})
	idxA.SetRepoPrefix("repoA")
	mi.mu.Lock()
	mi.indexers["repoA"] = idxA
	mi.repos["repoA"] = &RepoMetadata{RepoPrefix: "repoA"}
	mi.mu.Unlock()

	store.AddBatch([]*graph.Node{
		{ID: "repoA/a.go", Kind: graph.KindFile, Name: "a.go", FilePath: "repoA/a.go", RepoPrefix: "repoA", Language: "go"},
		{ID: "repoA/a.go::Run", Kind: graph.KindFunction, Name: "Run", FilePath: "repoA/a.go", RepoPrefix: "repoA", Language: "go"},
		{ID: "repoA/lib.go", Kind: graph.KindFile, Name: "lib.go", FilePath: "repoA/lib.go", RepoPrefix: "repoA", Language: "go"},
		{ID: "repoA/lib.go::Helper", Kind: graph.KindFunction, Name: "Helper", FilePath: "repoA/lib.go", RepoPrefix: "repoA", Language: "go"},
		{ID: "repoA/untouched.go", Kind: graph.KindFile, Name: "untouched.go", FilePath: "repoA/untouched.go", RepoPrefix: "repoA", Language: "go"},
		{ID: "repoA/untouched.go::Other", Kind: graph.KindFunction, Name: "Other", FilePath: "repoA/untouched.go", RepoPrefix: "repoA", Language: "go"},
	}, []*graph.Edge{
		{From: "repoA/a.go::Run", To: "unresolved::Helper", Kind: graph.EdgeCalls, FilePath: "repoA/a.go", Line: 5},
		{From: "repoA/untouched.go::Other", To: "unresolved::Helper", Kind: graph.EdgeCalls, FilePath: "repoA/untouched.go", Line: 5},
	})

	store.setRefuseFunc(refuseOnceMatching(batchHasCallTo("repoA/lib.go::Helper")))
	res := &IndexResult{DerivedInvalidation: DerivedInvalidationPlan{Files: []string{"repoA/a.go"}}}
	incompleteReceipt := &graph.MutationReceipt{Complete: false}
	batch := &reparsePendingEnrichmentBatch{deferResolverCatchup: true}
	_, _, _ = mi.resolveIncrementalRepoMutationMode("repoA", res, incompleteReceipt, batch, false)
	require.ErrorIs(t, res.resolutionErr, resolver.ErrReindexWriteRefused)
	assert.Equal(t, 1, store.refusedCount())

	// Incomplete receipt (!exact) must escalate pendingResolutionFull so the next save resolves untouched.go too.
	idxA.pendingResolutionMu.Lock()
	queuedFull := idxA.pendingResolutionFull
	idxA.pendingResolutionMu.Unlock()
	assert.True(t, queuedFull, "incomplete receipt refusal must escalate retryFull=true")

	store.setRefuseFunc(nil)
	res2 := &IndexResult{DerivedInvalidation: DerivedInvalidationPlan{Files: []string{"repoA/a.go"}}}
	_, _, repairedAll := mi.resolveIncrementalRepoMutationMode("repoA", res2, &graph.MutationReceipt{Complete: true}, &reparsePendingEnrichmentBatch{deferResolverCatchup: true}, false)
	require.NoError(t, res2.resolutionErr)
	assert.True(t, repairedAll)
	assert.Equal(t, "repoA/lib.go::Helper", callTargetForName(t, store, "repoA/untouched.go", "Other"))
}

func TestIndexer_RetainedFullPassBacklogAndRefusedNewFileSaveStillRecordsAndResolves(t *testing.T) {
	root := t.TempDir()
	writeFileMk(t, filepath.Join(root, "old.go"), "package demo\nfunc OldHelper(x int) int { return x }\nfunc OldCaller() int { return OldHelper(1) }\n")
	writeFileMk(t, filepath.Join(root, "new_target.go"), "package demo\nfunc NewHelper() int { return 2 }\n")
	otherPath := filepath.Join(root, "other.go")
	writeFileMk(t, otherPath, "package demo\nfunc Other() int { return 3 }\n")

	idx, store := newSQLiteIndexer(t)
	_, err := idx.Index(root)
	require.NoError(t, err)

	// Attach the refusing wrapper once so SetGraph does not wipe retained
	// placeholderSrcIdx state between the seeded full-pass backlog and the saves.
	refusing := &countingRefuseReindexStore{Store: store}
	idx.resolver.SetGraph(refusing)

	// Retain a placeholder repoint from an earlier refused full pass.
	store.AddEdge(&graph.Edge{
		From: "unresolved::OldHelper", To: "old.go::OldCaller",
		Kind: graph.EdgeArgOf, FilePath: "old.go", Line: 3,
	})
	idx.resolver.InheritPendingPlaceholderRepoints([]graph.PlaceholderRepoint{
		{OldFrom: "unresolved::OldHelper", FilePath: "old.go", Line: 3, NewFrom: "old.go::OldHelper"},
	})

	// Add new.go and refuse its own call write by content; new.go must be recorded
	// unconditionally in pendingResolutionRetry even though old.go's repoint is queued.
	newPath := filepath.Join(root, "new.go")
	writeFileMk(t, newPath, "package demo\nfunc NewCaller() int { return NewHelper() }\n")
	refusing.setRefuseFunc(refuseOnceMatching(batchHasCallTo("new_target.go::NewHelper")))
	res1, err := idx.incrementalWatcherPaths(root, []string{newPath}, incrementalPathMode{
		detectDeletions:    true,
		forceExplicitFiles: true,
	})
	refusing.setRefuseFunc(nil)
	require.NoError(t, err)
	require.NotNil(t, res1)
	assert.True(t, res1.ResolutionWriteRefused)
	assert.Equal(t, 1, refusing.refusedCount())
	assert.True(t, idx.hasPendingResolutionRetry())
	assert.Equal(t, "unresolved::NewHelper", callTargetForName(t, store, "new.go", "NewCaller"))

	// Clean save of unrelated other.go opportunistically resolves new.go AND drains old.go's repoint.
	writeFileMk(t, otherPath, "package demo\nfunc Other() int { return 4 }\n")
	res, err := idx.incrementalWatcherPaths(root, []string{otherPath}, incrementalPathMode{
		detectDeletions:    true,
		forceExplicitFiles: true,
	})
	require.NoError(t, err)
	require.NotNil(t, res)
	assert.False(t, res.ResolutionWriteRefused)
	assert.True(t, res.retriedResolution)
	assert.False(t, idx.hasPendingResolutionRetry())
	assert.Equal(t, "new_target.go::NewHelper", callTargetForName(t, store, "new.go", "NewCaller"))
	assert.Empty(t, store.GetOutEdges("unresolved::OldHelper"))
}

func TestMultiIndexer_PerRepoRetryBacklogIsolatesSiblingRepos(t *testing.T) {
	mi, store := newTestMultiIndexerWithRefusingStore(t, nil)
	idxA := mi.newPerRepoIndexerGuardedWithMode(config.IndexConfig{}, multiIndexerBatchMode{})
	idxA.SetRepoPrefix("repoA")
	idxB := mi.newPerRepoIndexerGuardedWithMode(config.IndexConfig{}, multiIndexerBatchMode{})
	idxB.SetRepoPrefix("repoB")
	mi.mu.Lock()
	mi.indexers["repoA"] = idxA
	mi.indexers["repoB"] = idxB
	mi.repos["repoA"] = &RepoMetadata{RepoPrefix: "repoA"}
	mi.repos["repoB"] = &RepoMetadata{RepoPrefix: "repoB"}
	mi.mu.Unlock()

	store.AddBatch([]*graph.Node{
		{ID: "repoA/a.go", Kind: graph.KindFile, Name: "a.go", FilePath: "repoA/a.go", RepoPrefix: "repoA", WorkspaceID: "ws", Language: "go"},
		{ID: "repoA/a.go::Run", Kind: graph.KindFunction, Name: "Run", FilePath: "repoA/a.go", RepoPrefix: "repoA", WorkspaceID: "ws", Language: "go"},
		{ID: "repoA/t.go", Kind: graph.KindFile, Name: "t.go", FilePath: "repoA/t.go", RepoPrefix: "repoA", WorkspaceID: "ws", Language: "go"},
		{ID: "repoA/t.go::HelperA", Kind: graph.KindFunction, Name: "HelperA", FilePath: "repoA/t.go", RepoPrefix: "repoA", WorkspaceID: "ws", Language: "go"},
		{ID: "repoA/decoy.go", Kind: graph.KindFile, Name: "decoy.go", FilePath: "repoA/decoy.go", RepoPrefix: "repoA", WorkspaceID: "ws", Language: "go"},
		{ID: "repoA/decoy.go::SecretA", Kind: graph.KindFunction, Name: "SecretA", FilePath: "repoA/decoy.go", RepoPrefix: "repoA", WorkspaceID: "ws", Language: "go"},
		{ID: "repoB/b.go", Kind: graph.KindFile, Name: "b.go", FilePath: "repoB/b.go", RepoPrefix: "repoB", WorkspaceID: "ws", Language: "go"},
		{ID: "repoB/b.go::RunB", Kind: graph.KindFunction, Name: "RunB", FilePath: "repoB/b.go", RepoPrefix: "repoB", WorkspaceID: "ws", Language: "go"},
		{ID: "repoB/t.go", Kind: graph.KindFile, Name: "t.go", FilePath: "repoB/t.go", RepoPrefix: "repoB", WorkspaceID: "ws", Language: "go"},
		{ID: "repoB/t.go::HelperB", Kind: graph.KindFunction, Name: "HelperB", FilePath: "repoB/t.go", RepoPrefix: "repoB", WorkspaceID: "ws", Language: "go"},
		{ID: "repoC/lib.go", Kind: graph.KindFile, Name: "lib.go", FilePath: "repoC/lib.go", RepoPrefix: "repoC", WorkspaceID: "ws", Language: "go"},
		{ID: "repoC/lib.go::XTarget", Kind: graph.KindFunction, Name: "XTarget", FilePath: "repoC/lib.go", RepoPrefix: "repoC", WorkspaceID: "ws", Language: "go"},
	}, []*graph.Edge{
		{From: "repoA/unresolved::HelperA", To: "repoA/a.go::Run", Kind: graph.EdgeArgOf, FilePath: "repoA/a.go", Line: 5},
		{From: "repoA/a.go::Run", To: "repoA/decoy.go::SecretA", Kind: graph.EdgeCalls, FilePath: "repoA/a.go", Line: 8, Confidence: 0.4, Origin: graph.OriginTextMatched},
		{From: "repoA/unresolved::XTarget", To: "repoA/a.go::Run", Kind: graph.EdgeArgOf, FilePath: "repoA/a.go", Line: 12},
		{From: "repoB/b.go::RunB", To: "unresolved::HelperB", Kind: graph.EdgeCalls, FilePath: "repoB/b.go", Line: 5},
	})

	// Seed Repo A backlog across both master (repoint + guard) and cross-repo layers.
	mi.savePendingMasterRetry(resolver.MasterRetryState{
		PlaceholderRepoints: []graph.PlaceholderRepoint{
			{OldFrom: "repoA/unresolved::HelperA", FilePath: "repoA/a.go", Line: 5, NewFrom: "repoA/t.go::HelperA", RepoPrefix: "repoA"},
		},
		GuardReverts: []resolver.PendingGuardRevert{{
			RepoPrefix: "repoA",
			Reindexes: []graph.EdgeReindex{{
				Edge:  &graph.Edge{From: "repoA/a.go::Run", To: "unresolved::SecretA", Kind: graph.EdgeCalls, FilePath: "repoA/a.go", Line: 8, Meta: map[string]any{"guard_reverted": true}},
				OldTo: "repoA/decoy.go::SecretA",
			}},
		}},
	}, resolver.RetryBridgeSaveMeta{Refused: true})
	mi.savePendingCrossRepoRepoints(resolver.CrossRepoRetryState{
		Repoints: []graph.PlaceholderRepoint{
			{OldFrom: "repoA/unresolved::XTarget", FilePath: "repoA/a.go", Line: 12, NewFrom: "repoC/lib.go::XTarget", RepoPrefix: "repoA"},
		},
	}, resolver.RetryBridgeSaveMeta{Refused: true})

	// Sibling Repo B save (including a vanished name) must NOT touch Repo A's backlog
	// AND must incur ZERO RepoPrefixes() SQL calls!
	store.repoPrefixesCalls.Store(0)
	resB := &IndexResult{DerivedInvalidation: DerivedInvalidationPlan{Files: []string{"repoB/b.go"}}}
	receiptB := &graph.MutationReceipt{
		Complete:           true,
		ResolutionRelevant: true,
		ChangedFiles:       []string{"repoB/b.go"},
		UnresolvedFiles:    []string{"repoB/b.go"},
		EvictedNames:       []string{"GoneNameB"},
	}
	_, _, _ = mi.resolveIncrementalRepoMutationMode("repoB", resB, receiptB, &reparsePendingEnrichmentBatch{deferResolverCatchup: true}, false)
	require.NoError(t, resB.resolutionErr)
	assert.Equal(t, int64(0), store.repoPrefixesCalls.Load(), "sibling-repo save must perform zero RepoPrefixes() SQL calls while Repo A has a backlog")
	assert.Equal(t, "repoB/t.go::HelperB", callTargetForName(t, store, "repoB/b.go", "RunB"))

	stats := mi.PendingRetryBacklogStats()
	assert.Equal(t, 1, stats.ByRepo["repoA"].PlaceholderRepoints, "Repo B save must not touch Repo A's master repoints")
	assert.Equal(t, 1, stats.ByRepo["repoA"].GuardReverts, "Repo B save must not touch Repo A's guard reverts")
	assert.Equal(t, 1, stats.ByRepo["repoA"].CrossRepoRepoints, "Repo B save must not touch Repo A's cross-repo repoints")

	// Repo A save drains Repo A's backlog cleanly.
	resA := &IndexResult{DerivedInvalidation: DerivedInvalidationPlan{Files: []string{"repoA/a.go"}}}
	receiptA := &graph.MutationReceipt{Complete: true, ResolutionRelevant: true, ChangedFiles: []string{"repoA/a.go"}}
	_, _, _ = mi.resolveIncrementalRepoMutationMode("repoA", resA, receiptA, &reparsePendingEnrichmentBatch{deferResolverCatchup: true}, false)
	require.NoError(t, resA.resolutionErr)
	stats = mi.PendingRetryBacklogStats()
	assert.Equal(t, 0, stats.PlaceholderRepoints)
	assert.Equal(t, 0, stats.GuardReverts)
	assert.Equal(t, 0, stats.CrossRepoRepoints)
	assert.Empty(t, store.GetOutEdges("repoA/unresolved::HelperA"))
	assert.Empty(t, store.GetOutEdges("repoA/unresolved::XTarget"))

	// Unprefixed single-repo MultiIndexer safety: items stored in bucket "" drain cleanly.
	t.Run("unprefixed_single_repo_multi_indexer", func(t *testing.T) {
		miSingle, sSingle := newTestMultiIndexerWithRefusingStore(t, nil)
		sSingle.AddBatch([]*graph.Node{
			{ID: "main.go", Kind: graph.KindFile, Name: "main.go", FilePath: "main.go", Language: "go"},
			{ID: "main.go::Run", Kind: graph.KindFunction, Name: "Run", FilePath: "main.go", Language: "go"},
			{ID: "lib.go", Kind: graph.KindFile, Name: "lib.go", FilePath: "lib.go", Language: "go"},
			{ID: "lib.go::Fn", Kind: graph.KindFunction, Name: "Fn", FilePath: "lib.go", Language: "go"},
		}, []*graph.Edge{
			{From: "unresolved::Fn", To: "main.go::Run", Kind: graph.EdgeArgOf, FilePath: "main.go", Line: 5},
		})
		miSingle.savePendingMasterRetry(resolver.MasterRetryState{
			PlaceholderRepoints: []graph.PlaceholderRepoint{
				{OldFrom: "unresolved::Fn", FilePath: "main.go", Line: 5, NewFrom: "lib.go::Fn"},
			},
		}, resolver.RetryBridgeSaveMeta{Refused: true})
		require.Equal(t, 1, miSingle.PendingRetryBacklogStats().PlaceholderRepoints)
		miSingle.runMasterResolveFiles([]string{"main.go"}, false)
		assert.Equal(t, 0, miSingle.PendingRetryBacklogStats().PlaceholderRepoints)
		assert.Empty(t, sSingle.GetOutEdges("unresolved::Fn"))
		assert.Len(t, sSingle.GetOutEdges("lib.go::Fn"), 1)
	})
}

func TestMultiIndexer_UntrackRepoClearsPerRepoRetryBacklog(t *testing.T) {
	rootA := t.TempDir()
	rootB := t.TempDir()
	writeFileMk(t, filepath.Join(rootA, "a.go"), "package a\nfunc A() {}\n")
	writeFileMk(t, filepath.Join(rootB, "b.go"), "package b\nfunc B() {}\n")

	mi, _ := newTestMultiIndexerWithRefusingStore(t, nil)
	_, err := mi.TrackRepoCtx(context.Background(), config.RepoEntry{Path: rootA, Name: "repoA"})
	require.NoError(t, err)
	_, err = mi.TrackRepoCtx(context.Background(), config.RepoEntry{Path: rootB, Name: "repoB"})
	require.NoError(t, err)

	mi.savePendingMasterRetry(resolver.MasterRetryState{
		PlaceholderRepoints: []graph.PlaceholderRepoint{
			{OldFrom: "repoA/unresolved::X", FilePath: "repoA/a.go", Line: 2, NewFrom: "repoA/a.go::A", RepoPrefix: "repoA"},
			{OldFrom: "repoB/unresolved::Y", FilePath: "repoB/b.go", Line: 2, NewFrom: "repoB/b.go::B", RepoPrefix: "repoB"},
		},
	}, resolver.RetryBridgeSaveMeta{Refused: true})
	mi.savePendingCrossRepoRepoints(resolver.CrossRepoRetryState{
		Repoints: []graph.PlaceholderRepoint{
			{OldFrom: "repoA/unresolved::Z", FilePath: "repoA/a.go", Line: 3, NewFrom: "repoB/b.go::B", RepoPrefix: "repoA"},
		},
	}, resolver.RetryBridgeSaveMeta{Refused: true})

	mi.UntrackRepo("repoA")
	stats := mi.PendingRetryBacklogStats()
	assert.Zero(t, stats.ByRepo["repoA"].PlaceholderRepoints)
	assert.Zero(t, stats.ByRepo["repoA"].CrossRepoRepoints)
	assert.Equal(t, 1, stats.ByRepo["repoB"].PlaceholderRepoints, "untracking repoA must preserve repoB's backlog")
}

func TestMultiIndexer_UntrackDuringYieldingResolveAllDoesNotReinsertBacklog(t *testing.T) {
	t.Setenv("GORTEX_RESOLVE_CHUNK", "1")
	t.Setenv("GORTEX_RESOLVE_CHUNK_SIZE", "1")

	rootA := t.TempDir()
	rootB := t.TempDir()
	writeFileMk(t, filepath.Join(rootA, "a.go"), "package a\nfunc HelperA() {}\nfunc RunA() { HelperA() }\n")
	writeFileMk(t, filepath.Join(rootB, "b.go"), "package b\nfunc HelperB() {}\nfunc RunB() { HelperB() }\n")

	mi, store := newTestMultiIndexerWithRefusingStore(t, nil)
	_, err := mi.TrackRepoCtx(context.Background(), config.RepoEntry{Path: rootA, Name: "repoA"})
	require.NoError(t, err)
	_, err = mi.TrackRepoCtx(context.Background(), config.RepoEntry{Path: rootB, Name: "repoB"})
	require.NoError(t, err)

	// Re-park both calls on unresolved::* plus a dataflow placeholder in repoA so
	// chunked ResolveAllContext commits chunk 1 (repoA), yields, and then runs chunk 2 (repoB).
	restubCall := func(callerID, unresolved string) {
		for _, e := range store.GetOutEdges(callerID) {
			if e.Kind == graph.EdgeCalls {
				oldTo := e.To
				e.To = unresolved
				require.True(t, store.ReindexEdgesApplied([]graph.EdgeReindex{{Edge: e, OldTo: oldTo}}))
				return
			}
		}
		t.Fatalf("missing call edge on %s", callerID)
	}
	restubCall("repoA/a.go::RunA", "unresolved::HelperA")
	restubCall("repoB/b.go::RunB", "unresolved::HelperB")
	store.AddEdge(&graph.Edge{
		From: "repoA/unresolved::HelperA", To: "repoA/a.go::RunA",
		Kind: graph.EdgeArgOf, FilePath: "repoA/a.go", Line: 3,
	})

	// When chunk 2 (repoB) tries to commit, untrack repoA concurrently with the
	// in-flight ResolveAllContext and refuse chunk 2 so bridge.finish() attempts
	// to save repoA's deferred repoint back under the pre-untrack TakeEpoch.
	epochBefore := mi.retryClearEpoch.Load()
	untracked := make(chan struct{})
	var untrackOnce sync.Once
	store.setRefuseFunc(func(_ int, batch []graph.EdgeReindex) bool {
		if !batchHasCallTo("repoB/b.go::HelperB")(batch) {
			return false
		}
		untrackOnce.Do(func() {
			go func() {
				defer close(untracked)
				mi.UntrackRepo("repoA")
			}()
			// UntrackRepo purges repoA and bumps retryClearEpoch BEFORE acquiring
			// store.ResolveMutex() for RetryGoModuleDeps, so waiting for the epoch
			// here is deterministic and deadlock-free.
			for mi.retryClearEpoch.Load() == epochBefore {
				runtime.Gosched()
			}
		})
		return true
	})

	err = mi.RunPreEnrichResolve(context.Background(), nil, nil)
	require.ErrorIs(t, err, resolver.ErrReindexWriteRefused)
	<-untracked

	stats := mi.PendingRetryBacklogStats()
	assert.Zero(t, stats.ByRepo["repoA"].PlaceholderRepoints, "in-flight ResolveAllContext finishing after UntrackRepo must not re-insert repoA backlog")
	assert.Zero(t, stats.ByRepo["repoA"].GuardReverts)

	// Re-track repoA and trigger a fresh refused write: non-zero TakeEpoch (== repoClearedEpoch)
	// must NOT be falsely dropped!
	store.setRefuseFunc(nil)
	_, err = mi.TrackRepoCtx(context.Background(), config.RepoEntry{Path: rootA, Name: "repoA"})
	require.NoError(t, err)
	restubCall("repoA/a.go::RunA", "unresolved::HelperA")
	store.AddEdge(&graph.Edge{
		From: "repoA/unresolved::HelperA", To: "repoA/a.go::RunA",
		Kind: graph.EdgeArgOf, FilePath: "repoA/a.go", Line: 3,
	})
	store.setRefuseFunc(refuseOnceMatching(batchHasPlaceholderMove("repoA/unresolved::HelperA", "repoA/a.go::HelperA")))
	err = mi.RunPreEnrichResolve(context.Background(), nil, nil)
	require.ErrorIs(t, err, resolver.ErrReindexWriteRefused)
	assert.Equal(t, 1, mi.PendingRetryBacklogStats().ByRepo["repoA"].PlaceholderRepoints, "re-tracked repoA refusal with TakeEpoch == repoClearedEpoch must be retained")

	store.setRefuseFunc(nil)
	require.NoError(t, mi.RunPreEnrichResolve(context.Background(), nil, nil))
	assert.Zero(t, mi.PendingRetryBacklogStats().PlaceholderRepoints)
}

func TestMultiIndexer_RefusedMasterResolveSkipsCrossRepoForRestubbedGoCall(t *testing.T) {
	mi, store := newTestMultiIndexerWithRefusingStore(t, nil)
	idxA := mi.newPerRepoIndexerGuardedWithMode(config.IndexConfig{}, multiIndexerBatchMode{})
	idxA.SetRepoPrefix("repoA")
	mi.mu.Lock()
	mi.indexers["repoA"] = idxA
	mi.repos["repoA"] = &RepoMetadata{RepoPrefix: "repoA"}
	mi.mu.Unlock()

	producer := &graph.Node{ID: "repoA/b/producer.go::Produce", Kind: graph.KindFunction, Name: "Produce", FilePath: "repoA/b/producer.go", RepoPrefix: "repoA", WorkspaceID: "ws", Language: "go"}
	store.AddBatch([]*graph.Node{
		{ID: "repoA/a/caller.go", Kind: graph.KindFile, Name: "caller.go", FilePath: "repoA/a/caller.go", RepoPrefix: "repoA", WorkspaceID: "ws", Language: "go"},
		{ID: "repoA/a/caller.go::Run", Kind: graph.KindFunction, Name: "Run", FilePath: "repoA/a/caller.go", RepoPrefix: "repoA", WorkspaceID: "ws", Language: "go"},
		{ID: "repoA/b/producer.go", Kind: graph.KindFile, Name: "producer.go", FilePath: "repoA/b/producer.go", RepoPrefix: "repoA", WorkspaceID: "ws", Language: "go"},
		producer,
		{ID: "repoA/c/decoy.go", Kind: graph.KindFile, Name: "decoy.go", FilePath: "repoA/c/decoy.go", RepoPrefix: "repoA", WorkspaceID: "ws", Language: "go"},
		{ID: "repoA/c/decoy.go::Produce", Kind: graph.KindFunction, Name: "Produce", FilePath: "repoA/c/decoy.go", RepoPrefix: "repoA", WorkspaceID: "ws", Language: "go"},
	}, []*graph.Edge{
		{From: "repoA/a/caller.go", To: "repoA/b/producer.go", Kind: graph.EdgeImports, FilePath: "repoA/a/caller.go", Line: 1},
		{From: "repoA/a/caller.go::Run", To: "unresolved::Produce", Kind: graph.EdgeCalls, FilePath: "repoA/a/caller.go", Line: 5, Meta: map[string]any{"guard_reverted": true}},
	})

	// Step 1: bind to real imported producer (clears guard_reverted).
	require.NoError(t, mi.RunPreEnrichResolve(context.Background(), nil, nil))
	edges := store.GetOutEdges("repoA/a/caller.go::Run")
	require.Len(t, edges, 1)
	require.Equal(t, "repoA/b/producer.go::Produce", edges[0].To)
	require.Nil(t, edges[0].Meta["guard_reverted"])

	// Step 2: withdraw producer and restub call without guard_reverted.
	prevEdge := *edges[0]
	if edges[0].Meta != nil {
		prevEdge.Meta = make(map[string]any, len(edges[0].Meta))
		for k, v := range edges[0].Meta {
			prevEdge.Meta[k] = v
		}
	}
	store.EvictFile("repoA/b/producer.go")
	store.AddNode(&graph.Node{ID: "repoA/b/producer.go", Kind: graph.KindFile, Name: "producer.go", FilePath: "repoA/b/producer.go", RepoPrefix: "repoA", WorkspaceID: "ws", Language: "go"})
	store.AddEdge(&graph.Edge{From: "repoA/a/caller.go", To: "repoA/b/producer.go", Kind: graph.EdgeImports, FilePath: "repoA/a/caller.go", Line: 1})
	graph.StashRestubProvenance(&prevEdge)
	prevEdge.To = "unresolved::Produce"
	if cur := store.GetOutEdges("repoA/a/caller.go::Run"); len(cur) == 1 {
		require.True(t, store.ReindexEdgesApplied([]graph.EdgeReindex{{Edge: &prevEdge, OldTo: cur[0].To}}))
	} else {
		store.AddEdge(&prevEdge)
	}

	// Step 3: refuse master resolve write on repoA/a/caller.go -> CrossRepoResolver MUST be skipped!
	var sawCrossRepo bool
	idxA.incrementalCatchupHook = func(kind string, _ []string) {
		if kind == "cross_repo" || kind == "cross_repo_graph" {
			sawCrossRepo = true
		}
	}
	store.setRefuseFunc(refuseOnceMatching(batchHasCallTo("repoA/c/decoy.go::Produce")))
	res := &IndexResult{DerivedInvalidation: DerivedInvalidationPlan{Files: []string{"repoA/a/caller.go"}}}
	receipt := &graph.MutationReceipt{Complete: true, ResolutionRelevant: true, ChangedFiles: []string{"repoA/a/caller.go"}, UnresolvedFiles: []string{"repoA/a/caller.go"}}
	_, _, _ = mi.resolveIncrementalRepoMutationMode("repoA", res, receipt, &reparsePendingEnrichmentBatch{deferResolverCatchup: true}, false)
	require.ErrorIs(t, res.resolutionErr, resolver.ErrReindexWriteRefused)
	assert.Equal(t, 1, store.refusedCount())
	assert.False(t, sawCrossRepo, "CrossRepoResolver must be skipped when master resolve failed")
	edges = store.GetOutEdges("repoA/a/caller.go::Run")
	require.Len(t, edges, 1)
	assert.Equal(t, "unresolved::Produce", edges[0].To, "unimported decoy must never bind when master resolve was refused")

	// Step 4: retry succeeds, re-stamps guard_reverted, and CrossRepoResolver respects it.
	store.setRefuseFunc(nil)
	res2 := &IndexResult{DerivedInvalidation: DerivedInvalidationPlan{Files: []string{"repoA/a/caller.go"}}}
	_, _, _ = mi.resolveIncrementalRepoMutationMode("repoA", res2, &graph.MutationReceipt{Complete: true}, &reparsePendingEnrichmentBatch{deferResolverCatchup: true}, false)
	require.NoError(t, res2.resolutionErr)
	assert.True(t, sawCrossRepo)
	edges = store.GetOutEdges("repoA/a/caller.go::Run")
	require.Len(t, edges, 1)
	assert.Equal(t, "unresolved::Produce", edges[0].To, "retry must keep restubbed Go call guarded against unimported same-repo decoy")
	assert.Equal(t, true, edges[0].Meta["guard_reverted"])
}

func TestMultiIndexer_RetryBacklogCountersAndLogsOnGrowthAndDrain(t *testing.T) {
	assert.Zero(t, (*MultiIndexer)(nil).PendingRetryBacklogStats())

	t.Setenv("GORTEX_RESOLVE_CHUNK", "1")
	t.Setenv("GORTEX_RESOLVE_CHUNK_SIZE", "1")

	core, recorded := observer.New(zapcore.InfoLevel)
	obsLogger := zap.New(core)
	mi, store := newTestMultiIndexerWithRefusingStore(t, nil)
	mi.logger = obsLogger
	mi.mu.Lock()
	mi.repos["repoA"] = &RepoMetadata{RepoPrefix: "repoA"}
	mi.mu.Unlock()

	store.AddBatch([]*graph.Node{
		{ID: "repoA/caller.ts", Kind: graph.KindFile, Name: "caller.ts", FilePath: "repoA/caller.ts", RepoPrefix: "repoA", Language: "typescript"},
		{ID: "repoA/caller.ts::run", Kind: graph.KindFunction, Name: "run", FilePath: "repoA/caller.ts", RepoPrefix: "repoA", Language: "typescript"},
		{ID: "repoA/target.ts", Kind: graph.KindFile, Name: "target.ts", FilePath: "repoA/target.ts", RepoPrefix: "repoA", Language: "typescript"},
		{ID: "repoA/target.ts::one", Kind: graph.KindFunction, Name: "one", FilePath: "repoA/target.ts", RepoPrefix: "repoA", Language: "typescript"},
		{ID: "repoA/target.ts::two", Kind: graph.KindFunction, Name: "two", FilePath: "repoA/target.ts", RepoPrefix: "repoA", Language: "typescript"},
	}, []*graph.Edge{
		{From: "repoA/caller.ts", To: "repoA/target.ts", Kind: graph.EdgeImports, FilePath: "repoA/caller.ts", Line: 1},
		{From: "repoA/caller.ts::run", To: "unresolved::one", Kind: graph.EdgeCalls, FilePath: "repoA/caller.ts", Line: 2},
		{From: "repoA/unresolved::one", To: "repoA/caller.ts::run", Kind: graph.EdgeArgOf, FilePath: "repoA/caller.ts", Line: 2},
		{From: "repoA/caller.ts::run", To: "unresolved::two", Kind: graph.EdgeCalls, FilePath: "repoA/caller.ts", Line: 3},
	})

	// 1. Refused chunk 2 queues repoA's repoint -> emits Warn and increments BacklogGrowthEvents.
	store.setRefuseFunc(refuseOnceMatching(batchHasCallTo("repoA/target.ts::two")))
	err := mi.RunPreEnrichResolve(context.Background(), nil, nil)
	require.ErrorIs(t, err, resolver.ErrReindexWriteRefused)
	stats := mi.PendingRetryBacklogStats()
	assert.Equal(t, uint64(1), stats.BacklogGrowthEvents)
	assert.Equal(t, uint64(0), stats.BacklogDrainEvents)
	assert.Equal(t, 1, recorded.FilterMessage("indexer: resolver retry backlog grew or refused").Len())

	// 2. Healthy retry drains repoA's backlog -> emits Info and increments BacklogDrainEvents, with zero extra Warn on healthy yield.
	store.setRefuseFunc(nil)
	recorded.TakeAll()
	require.NoError(t, mi.RunPreEnrichResolve(context.Background(), nil, nil))
	stats = mi.PendingRetryBacklogStats()
	assert.Equal(t, uint64(1), stats.BacklogGrowthEvents, "healthy chunked yield/drain must not increment growth events")
	assert.Equal(t, uint64(1), stats.BacklogDrainEvents)
	assert.Zero(t, recorded.FilterMessage("indexer: resolver retry backlog grew or refused").Len(), "healthy yield/drain must emit zero Warn spam")
	assert.Equal(t, 1, recorded.FilterMessage("indexer: resolver retry backlog drained").Len())
}

func TestReconcileRepoCtx_RefusedResolutionReturnsResultFlaggedAndRecovers(t *testing.T) {
	root := t.TempDir()
	libPath := filepath.Join(root, "lib.go")
	callerPath := filepath.Join(root, "caller.go")
	extraPath := filepath.Join(root, "extra.go")
	writeFileMk(t, libPath, "package demo\nfunc Helper() int { return 1 }\n")
	writeFileMk(t, callerPath, "package demo\nfunc Run() int { return 0 }\n")
	writeFileMk(t, extraPath, "package demo\nfunc Extra() int { return 2 }\n")

	seed, baseStore := newSQLiteIndexer(t)
	configFile := filepath.Join(t.TempDir(), "config.yaml")
	gc := &config.GlobalConfig{}
	gc.SetConfigPath(configFile)
	require.NoError(t, gc.Save())
	cm, err := config.NewConfigManager(configFile)
	require.NoError(t, err)

	entry := config.RepoEntry{Path: root, Name: "repo"}
	mi1 := NewMultiIndexer(baseStore, seed.registry, search.NewNull(), cm, zap.NewNop())
	_, err = mi1.TrackRepoCtx(context.Background(), entry)
	require.NoError(t, err)
	prior := mi1.FileMtimes("repo")
	require.NotZero(t, prior["caller.go"])

	// Mutate 1 of 3 files (33% <= 40%) and bump mtime explicitly so ReconcileRepoCtx
	// takes the scoped incremental route (!FullRetrack) deterministically.
	writeFileMk(t, callerPath, "package demo\nfunc Run() int { return Helper() }\n")
	bumped := time.Unix(0, prior["caller.go"]).Add(2 * time.Second)
	require.NoError(t, os.Chtimes(callerPath, bumped, bumped))

	refusing := &countingRefuseReindexStore{
		Store:      baseStore,
		refuseFunc: refuseOnceMatching(batchHasCallTo("repo/lib.go::Helper")),
	}
	mi2 := NewMultiIndexer(refusing, seed.registry, search.NewNull(), cm, zap.NewNop())
	res, err := mi2.ReconcileRepoCtx(context.Background(), entry, prior)
	require.NoError(t, err)
	require.NotNil(t, res)
	assert.False(t, res.FullRetrack)
	assert.True(t, res.ResolutionWriteRefused)
	assert.Equal(t, 1, refusing.refusedCount())
	assert.True(t, mi2.GetIndexer("repo").hasPendingResolutionRetry())
	assert.Equal(t, "unresolved::Helper", callTargetForName(t, baseStore, "repo/caller.go", "Run"))

	refusing.setRefuseFunc(nil)
	res2, err := mi2.IncrementalReindexRepo("repo", nil)
	require.NoError(t, err)
	require.NotNil(t, res2)
	assert.False(t, res2.ResolutionWriteRefused)
	assert.False(t, mi2.GetIndexer("repo").hasPendingResolutionRetry())
	assert.Equal(t, "repo/lib.go::Helper", callTargetForName(t, baseStore, "repo/caller.go", "Run"))
}

func copyBenchmarkDBFile(b *testing.B, src, dst string) {
	b.Helper()
	in, err := os.Open(src)
	if err != nil {
		b.Fatal(err)
	}
	defer func() { _ = in.Close() }()
	out, err := os.Create(dst)
	if err != nil {
		b.Fatal(err)
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		b.Fatal(err)
	}
	if err := out.Close(); err != nil {
		b.Fatal(err)
	}
}

func removeBenchmarkDBFiles(path string) {
	_ = os.Remove(path)
	_ = os.Remove(path + "-wal")
	_ = os.Remove(path + "-shm")
}

func BenchmarkMultiIndexerFirstSaveAfterRefusedFullPass(b *testing.B) {
	const (
		bgFiles                 = 100
		bgEdgesPerFile          = 100 // 10,000 background arg_of edges in repoA
		queuedRepointCount      = 400
		queuedGuardCount        = 400
		queuedCrossRepointCount = 200
	)

	tmplDir := b.TempDir()
	tmplDB := filepath.Join(tmplDir, "template.sqlite")
	store, err := store_sqlite.Open(tmplDB)
	if err != nil {
		b.Fatal(err)
	}

	nodes := make([]*graph.Node, 0, bgFiles*2+queuedRepointCount*4+queuedGuardCount*4+queuedCrossRepointCount*4+8)
	edges := make([]*graph.Edge, 0, bgFiles*bgEdgesPerFile+queuedRepointCount+queuedGuardCount+queuedCrossRepointCount+2)
	masterRepoints := make([]graph.PlaceholderRepoint, 0, queuedRepointCount)
	guardReindexes := make([]graph.EdgeReindex, 0, queuedGuardCount)
	crossRepoints := make([]graph.PlaceholderRepoint, 0, queuedCrossRepointCount)

	for f := 0; f < bgFiles; f++ {
		fp := fmt.Sprintf("repoA/bg/f%03d.go", f)
		callerID := fp + "::fn"
		nodes = append(nodes,
			&graph.Node{ID: fp, Kind: graph.KindFile, Name: fmt.Sprintf("f%03d.go", f), FilePath: fp, RepoPrefix: "repoA", WorkspaceID: "ws", Language: "go"},
			&graph.Node{ID: callerID, Kind: graph.KindFunction, Name: "fn", FilePath: fp, RepoPrefix: "repoA", WorkspaceID: "ws", Language: "go"},
		)
		for e := 0; e < bgEdgesPerFile; e++ {
			edges = append(edges, &graph.Edge{
				From:     fmt.Sprintf("repoA/bg/t%03d.go::arg_%03d", f, e),
				To:       callerID,
				Kind:     graph.EdgeArgOf,
				FilePath: fp,
				Line:     e + 1,
			})
		}
	}

	for i := 0; i < queuedRepointCount; i++ {
		fp := fmt.Sprintf("repoA/pkg/f%03d.go", i)
		tfp := fmt.Sprintf("repoA/pkg/t%03d.go", i)
		fnName := fmt.Sprintf("fn_%03d", i)
		callerID := fp + "::caller"
		targetID := tfp + "::" + fnName
		oldFrom := "repoA/" + graph.UnresolvedMarker + fnName
		nodes = append(nodes,
			&graph.Node{ID: fp, Kind: graph.KindFile, Name: fmt.Sprintf("f%03d.go", i), FilePath: fp, RepoPrefix: "repoA", WorkspaceID: "ws", Language: "go"},
			&graph.Node{ID: callerID, Kind: graph.KindFunction, Name: "caller", FilePath: fp, RepoPrefix: "repoA", WorkspaceID: "ws", Language: "go"},
			&graph.Node{ID: tfp, Kind: graph.KindFile, Name: fmt.Sprintf("t%03d.go", i), FilePath: tfp, RepoPrefix: "repoA", WorkspaceID: "ws", Language: "go"},
			&graph.Node{ID: targetID, Kind: graph.KindFunction, Name: fnName, FilePath: tfp, RepoPrefix: "repoA", WorkspaceID: "ws", Language: "go"},
		)
		edges = append(edges, &graph.Edge{
			From:     oldFrom,
			To:       callerID,
			Kind:     graph.EdgeArgOf,
			FilePath: fp,
			Line:     10,
		})
		masterRepoints = append(masterRepoints, graph.PlaceholderRepoint{
			OldFrom:    oldFrom,
			FilePath:   fp,
			Line:       10,
			NewFrom:    targetID,
			RepoPrefix: "repoA",
		})
	}

	for i := 0; i < queuedGuardCount; i++ {
		fp := fmt.Sprintf("repoA/pkg/g%03d.go", i)
		dfp := fmt.Sprintf("repoA/unimp/d%03d.go", i)
		fnName := fmt.Sprintf("g_%03d", i)
		callerID := fp + "::caller"
		decoyID := dfp + "::" + fnName
		nodes = append(nodes,
			&graph.Node{ID: fp, Kind: graph.KindFile, Name: fmt.Sprintf("g%03d.go", i), FilePath: fp, RepoPrefix: "repoA", WorkspaceID: "ws", Language: "go"},
			&graph.Node{ID: callerID, Kind: graph.KindFunction, Name: "caller", FilePath: fp, RepoPrefix: "repoA", WorkspaceID: "ws", Language: "go"},
			&graph.Node{ID: dfp, Kind: graph.KindFile, Name: fmt.Sprintf("d%03d.go", i), FilePath: dfp, RepoPrefix: "repoA", WorkspaceID: "ws", Language: "go"},
			&graph.Node{ID: decoyID, Kind: graph.KindFunction, Name: fnName, FilePath: dfp, RepoPrefix: "repoA", WorkspaceID: "ws", Language: "go"},
		)
		edges = append(edges, &graph.Edge{
			From:       callerID,
			To:         decoyID,
			Kind:       graph.EdgeCalls,
			FilePath:   fp,
			Line:       20,
			Confidence: 0.4,
			Origin:     graph.OriginTextMatched,
		})
		guardReindexes = append(guardReindexes, graph.EdgeReindex{
			Edge: &graph.Edge{
				From:     callerID,
				To:       graph.UnresolvedMarker + fnName,
				Kind:     graph.EdgeCalls,
				FilePath: fp,
				Line:     20,
				Meta:     map[string]any{"guard_reverted": true},
			},
			OldTo: decoyID,
		})
	}

	for i := 0; i < queuedCrossRepointCount; i++ {
		fp := fmt.Sprintf("repoA/xpkg/f%03d.go", i)
		tfp := fmt.Sprintf("repoB/xt%03d.go", i)
		fnName := fmt.Sprintf("xfn_%03d", i)
		callerID := fp + "::caller"
		targetID := tfp + "::" + fnName
		oldFrom := "repoA/" + graph.UnresolvedMarker + fnName
		nodes = append(nodes,
			&graph.Node{ID: fp, Kind: graph.KindFile, Name: fmt.Sprintf("f%03d.go", i), FilePath: fp, RepoPrefix: "repoA", WorkspaceID: "ws", Language: "go"},
			&graph.Node{ID: callerID, Kind: graph.KindFunction, Name: "caller", FilePath: fp, RepoPrefix: "repoA", WorkspaceID: "ws", Language: "go"},
			&graph.Node{ID: tfp, Kind: graph.KindFile, Name: fmt.Sprintf("xt%03d.go", i), FilePath: tfp, RepoPrefix: "repoB", WorkspaceID: "ws", Language: "go"},
			&graph.Node{ID: targetID, Kind: graph.KindFunction, Name: fnName, FilePath: tfp, RepoPrefix: "repoB", WorkspaceID: "ws", Language: "go"},
		)
		edges = append(edges, &graph.Edge{
			From:     oldFrom,
			To:       callerID,
			Kind:     graph.EdgeArgOf,
			FilePath: fp,
			Line:     10,
		})
		crossRepoints = append(crossRepoints, graph.PlaceholderRepoint{
			OldFrom:    oldFrom,
			FilePath:   fp,
			Line:       10,
			NewFrom:    targetID,
			RepoPrefix: "repoA",
		})
	}

	nodes = append(nodes,
		&graph.Node{ID: "repoA/save/edit.go", Kind: graph.KindFile, Name: "edit.go", FilePath: "repoA/save/edit.go", RepoPrefix: "repoA", WorkspaceID: "ws", Language: "go"},
		&graph.Node{ID: "repoA/save/edit.go::Run", Kind: graph.KindFunction, Name: "Run", FilePath: "repoA/save/edit.go", RepoPrefix: "repoA", WorkspaceID: "ws", Language: "go"},
		&graph.Node{ID: "repoA/save/helper.go", Kind: graph.KindFile, Name: "helper.go", FilePath: "repoA/save/helper.go", RepoPrefix: "repoA", WorkspaceID: "ws", Language: "go"},
		&graph.Node{ID: "repoA/save/helper.go::SaveHelperA", Kind: graph.KindFunction, Name: "SaveHelperA", FilePath: "repoA/save/helper.go", RepoPrefix: "repoA", WorkspaceID: "ws", Language: "go"},
		&graph.Node{ID: "repoB/save/edit.go", Kind: graph.KindFile, Name: "edit.go", FilePath: "repoB/save/edit.go", RepoPrefix: "repoB", WorkspaceID: "ws", Language: "go"},
		&graph.Node{ID: "repoB/save/edit.go::Run", Kind: graph.KindFunction, Name: "Run", FilePath: "repoB/save/edit.go", RepoPrefix: "repoB", WorkspaceID: "ws", Language: "go"},
		&graph.Node{ID: "repoB/save/helper.go", Kind: graph.KindFile, Name: "helper.go", FilePath: "repoB/save/helper.go", RepoPrefix: "repoB", WorkspaceID: "ws", Language: "go"},
		&graph.Node{ID: "repoB/save/helper.go::SaveHelperB", Kind: graph.KindFunction, Name: "SaveHelperB", FilePath: "repoB/save/helper.go", RepoPrefix: "repoB", WorkspaceID: "ws", Language: "go"},
	)
	edges = append(edges,
		&graph.Edge{From: "repoA/save/edit.go::Run", To: graph.UnresolvedMarker + "SaveHelperA", Kind: graph.EdgeCalls, FilePath: "repoA/save/edit.go", Line: 5},
		&graph.Edge{From: "repoB/save/edit.go::Run", To: graph.UnresolvedMarker + "SaveHelperB", Kind: graph.EdgeCalls, FilePath: "repoB/save/edit.go", Line: 5},
	)

	store.AddBatch(nodes, edges)
	if err := store.CheckpointWAL(); err != nil {
		b.Fatal(err)
	}
	if err := store.Close(); err != nil {
		b.Fatal(err)
	}
	_ = os.Remove(tmplDB + "-wal")
	_ = os.Remove(tmplDB + "-shm")

	registry := parser.NewRegistry()
	registry.Register(languages.NewGoExtractor())
	configFile := filepath.Join(tmplDir, "config.yaml")
	gc := &config.GlobalConfig{}
	gc.SetConfigPath(configFile)
	if err := gc.Save(); err != nil {
		b.Fatal(err)
	}
	cm, err := config.NewConfigManager(configFile)
	if err != nil {
		b.Fatal(err)
	}

	cases := []struct {
		name        string
		targetRepo  string
		saveFile    string
		withBacklog bool
	}{
		{name: "clean_baseline_same_repo", targetRepo: "repoA", saveFile: "repoA/save/edit.go", withBacklog: false},
		{name: "first_same_repo_save_after_refused_full_pass", targetRepo: "repoA", saveFile: "repoA/save/edit.go", withBacklog: true},
		{name: "clean_baseline_sibling_repo", targetRepo: "repoB", saveFile: "repoB/save/edit.go", withBacklog: false},
		{name: "first_sibling_repo_save_after_refused_full_pass", targetRepo: "repoB", saveFile: "repoB/save/edit.go", withBacklog: true},
	}
	for _, tc := range cases {
		b.Run(tc.name, func(b *testing.B) {
			iterDir := b.TempDir()
			iterDB := filepath.Join(iterDir, "iter.sqlite")
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				b.StopTimer()
				removeBenchmarkDBFiles(iterDB)
				copyBenchmarkDBFile(b, tmplDB, iterDB)
				s, openErr := store_sqlite.Open(iterDB)
				if openErr != nil {
					b.Fatal(openErr)
				}
				if i == 0 && s.GetNode(tc.saveFile+"::Run") == nil {
					_ = s.Close()
					b.Fatal("template DB missing expected nodes after checkpoint")
				}
				mi := NewMultiIndexer(s, registry, search.NewNull(), cm, zap.NewNop())
				idxA := mi.newPerRepoIndexerGuardedWithMode(config.IndexConfig{}, multiIndexerBatchMode{})
				idxA.SetRepoPrefix("repoA")
				idxB := mi.newPerRepoIndexerGuardedWithMode(config.IndexConfig{}, multiIndexerBatchMode{})
				idxB.SetRepoPrefix("repoB")
				mi.mu.Lock()
				mi.indexers["repoA"] = idxA
				mi.indexers["repoB"] = idxB
				mi.repos["repoA"] = &RepoMetadata{RepoPrefix: "repoA"}
				mi.repos["repoB"] = &RepoMetadata{RepoPrefix: "repoB"}
				mi.mu.Unlock()
				if tc.withBacklog {
					mi.savePendingMasterRetry(resolver.MasterRetryState{
						PlaceholderRepoints: append([]graph.PlaceholderRepoint(nil), masterRepoints...),
						GuardReverts:        []resolver.PendingGuardRevert{{RepoPrefix: "repoA", Reindexes: append([]graph.EdgeReindex(nil), guardReindexes...)}},
					}, resolver.RetryBridgeSaveMeta{Refused: true})
					mi.savePendingCrossRepoRepoints(resolver.CrossRepoRetryState{
						Repoints: append([]graph.PlaceholderRepoint(nil), crossRepoints...),
					}, resolver.RetryBridgeSaveMeta{Refused: true})
				}
				res := &IndexResult{DerivedInvalidation: DerivedInvalidationPlan{Files: []string{tc.saveFile}}}
				receipt := &graph.MutationReceipt{
					Complete:           true,
					ResolutionRelevant: true,
					ChangedFiles:       []string{tc.saveFile},
					UnresolvedFiles:    []string{tc.saveFile},
				}
				batch := &reparsePendingEnrichmentBatch{deferResolverCatchup: true}
				b.StartTimer()
				_, _, _ = mi.resolveIncrementalRepoMutationMode(tc.targetRepo, res, receipt, batch, false)
				b.StopTimer()
				if res.resolutionErr != nil {
					idxA.Close()
					idxB.Close()
					_ = s.Close()
					b.Fatalf("expected clean resolve, got %v", res.resolutionErr)
				}
				idxA.Close()
				idxB.Close()
				if closeErr := s.Close(); closeErr != nil {
					b.Fatal(closeErr)
				}
				removeBenchmarkDBFiles(iterDB)
			}
		})
	}
}
