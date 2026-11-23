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

