package resolver

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

type refusingReindexStore struct {
	*store_sqlite.Store
	mu         sync.Mutex
	refuseNext bool
	refuseWhen func(call int, batch []graph.EdgeReindex) bool
	calls      int
	refused    int
}

func (s *refusingReindexStore) ReindexEdges(batch []graph.EdgeReindex) {
	s.ReindexEdgesApplied(batch)
}

func (s *refusingReindexStore) ReindexEdgesApplied(batch []graph.EdgeReindex) bool {
	s.mu.Lock()
	s.calls++
	call := s.calls
	refuse := false
	switch {
	case s.refuseWhen != nil:
		refuse = s.refuseWhen(call, batch)
	case s.refuseNext:
		s.refuseNext = false
		refuse = true
	}
	if refuse {
		s.refused++
		s.mu.Unlock()
		return false
	}
	s.mu.Unlock()
	return s.Store.ReindexEdgesApplied(batch)
}

type refusingMemoryStore struct {
	*graph.Graph
	mu         sync.Mutex
	refuseWhen func(call int, batch []graph.EdgeReindex) bool
	calls      int
	refused    int
}

func (s *refusingMemoryStore) ReindexEdges(batch []graph.EdgeReindex) {
	s.ReindexEdgesApplied(batch)
}

func (s *refusingMemoryStore) ReindexEdgesApplied(batch []graph.EdgeReindex) bool {
	s.mu.Lock()
	s.calls++
	call := s.calls
	refuse := s.refuseWhen != nil && s.refuseWhen(call, batch)
	if refuse {
		s.refused++
		s.mu.Unlock()
		return false
	}
	s.mu.Unlock()
	s.Graph.ReindexEdges(batch)
	return true
}

func openRefusingReindexStore(t *testing.T) *refusingReindexStore {
	t.Helper()
	s, err := store_sqlite.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	return &refusingReindexStore{Store: s, refuseNext: true}
}

func TestResolveAllContextReturnsErrorOnRefusedReindexWrite(t *testing.T) {
	store := openRefusingReindexStore(t)
	store.AddNode(&graph.Node{ID: "caller.ts::run", Kind: graph.KindFunction, Name: "run", FilePath: "caller.ts", Language: "typescript"})
	store.AddNode(&graph.Node{ID: "target.ts::helper", Kind: graph.KindFunction, Name: "helper", FilePath: "target.ts", Language: "typescript"})
	store.AddEdge(&graph.Edge{
		From: "caller.ts::run", To: "unresolved::helper",
		Kind: graph.EdgeCalls, FilePath: "caller.ts", Line: 2,
	})

	r := New(store)
	computeDone := false
	r.OnComputeDone = func() { computeDone = true }

	stats, err := r.ResolveAllContext(context.Background())
	require.ErrorIs(t, err, ErrReindexWriteRefused)
	assert.True(t, stats.WriteRefused)
	assert.Equal(t, 1, store.refused)
	assert.False(t, computeDone, "compute readiness must not publish after a refused page write")
	edges := store.GetOutEdges("caller.ts::run")
	require.Len(t, edges, 1)
	assert.Equal(t, "unresolved::helper", edges[0].To)

	stats, err = r.ResolveAllContext(context.Background())
	require.NoError(t, err)
	assert.False(t, stats.WriteRefused)
	assert.True(t, computeDone)
	edges = store.GetOutEdges("caller.ts::run")
	require.Len(t, edges, 1)
	assert.Equal(t, "target.ts::helper", edges[0].To)
}

func TestResolveFilesAndIncomingFlagsRefusedReindexWrite(t *testing.T) {
	store := openRefusingReindexStore(t)
	store.AddNode(&graph.Node{ID: "caller.ts::run", Kind: graph.KindFunction, Name: "run", FilePath: "caller.ts", Language: "typescript"})
	store.AddNode(&graph.Node{ID: "target.ts::helper", Kind: graph.KindFunction, Name: "helper", FilePath: "target.ts", Language: "typescript"})
	store.AddEdge(&graph.Edge{
		From: "caller.ts::run", To: "unresolved::helper",
		Kind: graph.EdgeCalls, FilePath: "caller.ts", Line: 2,
	})

	r := New(store)

	stats := r.ResolveFilesAndIncoming([]string{"caller.ts"})
	assert.True(t, stats.WriteRefused)
	assert.Equal(t, 1, store.refused)
	edges := store.GetOutEdges("caller.ts::run")
	require.Len(t, edges, 1)
	assert.Equal(t, "unresolved::helper", edges[0].To)

	stats = r.ResolveFilesAndIncoming([]string{"caller.ts"})
	assert.False(t, stats.WriteRefused)
	edges = store.GetOutEdges("caller.ts::run")
	require.Len(t, edges, 1)
	assert.Equal(t, "target.ts::helper", edges[0].To)
}

func TestCrossRepoResolveAllContextReturnsErrorOnRefusedReindexWrite(t *testing.T) {
	store := openRefusingReindexStore(t)
	store.AddNode(&graph.Node{ID: "a/caller.ts", Kind: graph.KindFile, Name: "caller.ts", FilePath: "a/caller.ts", RepoPrefix: "a", Language: "typescript"})
	store.AddNode(&graph.Node{ID: "b/target.ts", Kind: graph.KindFile, Name: "target.ts", FilePath: "b/target.ts", RepoPrefix: "b", Language: "typescript"})
	store.AddNode(&graph.Node{ID: "a/caller.ts::run", Kind: graph.KindFunction, Name: "run", FilePath: "a/caller.ts", RepoPrefix: "a", Language: "typescript"})
	store.AddNode(&graph.Node{ID: "b/target.ts::helper", Kind: graph.KindFunction, Name: "helper", FilePath: "b/target.ts", RepoPrefix: "b", Language: "typescript"})
	store.AddEdge(&graph.Edge{
		From: "a/caller.ts", To: "b/target.ts",
		Kind: graph.EdgeImports, FilePath: "a/caller.ts", Line: 1, CrossRepo: true,
	})
	store.AddEdge(&graph.Edge{
		From: "a/caller.ts::run", To: "unresolved::helper",
		Kind: graph.EdgeCalls, FilePath: "a/caller.ts", Line: 2,
	})

	cr := NewCrossRepo(store)

	stats, err := cr.ResolveAllContext(context.Background())
	require.ErrorIs(t, err, ErrReindexWriteRefused)
	assert.True(t, stats.WriteRefused)
	assert.Equal(t, 1, store.refused)
	for _, e := range store.GetOutEdges("a/caller.ts::run") {
		assert.NotEqual(t, graph.EdgeCrossRepoCalls, e.Kind, "derived cross-repo parallel must not publish after a refused write")
	}

	stats, err = cr.ResolveAllContext(context.Background())
	require.NoError(t, err)
	assert.False(t, stats.WriteRefused)
	var sawBound bool
	for _, e := range store.GetOutEdges("a/caller.ts::run") {
		if e.Kind == graph.EdgeCalls && e.To == "b/target.ts::helper" {
			sawBound = true
		}
	}
	assert.True(t, sawBound)
}

func TestResolveAllContextContinuesAfterRefusedChunkAndKeepsCountersAccurate(t *testing.T) {
	t.Setenv("GORTEX_RESOLVE_CHUNK_SIZE", "1")
	for _, backend := range []string{"sqlite", "memory"} {
		t.Run(backend, func(t *testing.T) {
			var (
				store   graph.Store
				refused func() int
			)
			if backend == "sqlite" {
				s := openRefusingReindexStore(t)
				s.refuseNext = false
				s.refuseWhen = func(call int, _ []graph.EdgeReindex) bool { return call == 1 }
				store = s
				refused = func() int { return s.refused }
			} else {
				s := &refusingMemoryStore{
					Graph:      graph.New(),
					refuseWhen: func(call int, _ []graph.EdgeReindex) bool { return call == 1 },
				}
				store = s
				refused = func() int { return s.refused }
			}
			store.AddBatch([]*graph.Node{
				{ID: "caller1.ts::run1", Kind: graph.KindFunction, Name: "run1", FilePath: "caller1.ts", Language: "typescript"},
				{ID: "caller2.ts::run2", Kind: graph.KindFunction, Name: "run2", FilePath: "caller2.ts", Language: "typescript"},
				{ID: "target1.ts::helper1", Kind: graph.KindFunction, Name: "helper1", FilePath: "target1.ts", Language: "typescript"},
				{ID: "target2.ts::helper2", Kind: graph.KindFunction, Name: "helper2", FilePath: "target2.ts", Language: "typescript"},
			}, []*graph.Edge{
				{From: "caller1.ts::run1", To: "unresolved::helper1", Kind: graph.EdgeCalls, FilePath: "caller1.ts", Line: 2},
				{From: "caller2.ts::run2", To: "unresolved::helper2", Kind: graph.EdgeCalls, FilePath: "caller2.ts", Line: 2},
			})

			r := New(store)
			computeDone := false
			r.OnComputeDone = func() { computeDone = true }

			stats, err := r.ResolveAllContext(context.Background())
			require.ErrorIs(t, err, ErrReindexWriteRefused)
			assert.True(t, stats.WriteRefused)
			assert.Equal(t, 1, refused())
			assert.False(t, computeDone, "OnComputeDone must be withheld when any chunk write was refused")
			assert.Equal(t, 1, stats.Resolved, "only the committed chunk may count as resolved")
			assert.Equal(t, 1, stats.Unresolved, "refused chunk must count as unresolved")

			e1 := store.GetOutEdges("caller1.ts::run1")
			e2 := store.GetOutEdges("caller2.ts::run2")
			require.Len(t, e1, 1)
			require.Len(t, e2, 1)
			resolvedCount := 0
			unresolvedCount := 0
			for _, e := range []*graph.Edge{e1[0], e2[0]} {
				if graph.IsUnresolvedTarget(e.To) {
					unresolvedCount++
				} else {
					resolvedCount++
				}
			}
			assert.Equal(t, 1, resolvedCount, "page 2 must still resolve after page 1 refusal")
			assert.Equal(t, 1, unresolvedCount, "refused page edge must stay unresolved without in-place mutation")

			stats, err = r.ResolveAllContext(context.Background())
			require.NoError(t, err)
			assert.False(t, stats.WriteRefused)
			assert.True(t, computeDone)
			assert.Equal(t, "target1.ts::helper1", store.GetOutEdges("caller1.ts::run1")[0].To)
			assert.Equal(t, "target2.ts::helper2", store.GetOutEdges("caller2.ts::run2")[0].To)
		})
	}
}

func TestResolveAllContextDeferredLSPSite1AndSite2Refusals(t *testing.T) {
	t.Run("site1_refusal_carries_only_reindex_entries_without_pk_corruption", func(t *testing.T) {
		store := openRefusingReindexStore(t)
		store.refuseNext = false
		store.refuseWhen = func(call int, batch []graph.EdgeReindex) bool {
			return call == 1 && len(batch) > 0 && batch[0].Edge != nil && batch[0].Edge.Origin == graph.OriginLSPResolved
		}

		store.AddBatch([]*graph.Node{
			{ID: "src/caller.ts", Kind: graph.KindFile, Name: "caller.ts", FilePath: "src/caller.ts", Language: "typescript"},
			{ID: "src/caller.ts::run", Kind: graph.KindFunction, Name: "run", FilePath: "src/caller.ts", StartLine: 1, EndLine: 8, Language: "typescript"},
			{ID: "a/real.ts", Kind: graph.KindFile, Name: "real.ts", FilePath: "a/real.ts", Language: "typescript"},
			// Different symbol name so the name-only heuristic cannot bind it;
			// only deferred LSP binds it and promotes EdgeReads -> EdgeReferences.
			{ID: "a/real.ts::renamedHandler", Kind: graph.KindFunction, Name: "renamedHandler", FilePath: "a/real.ts", StartLine: 5, EndLine: 7, Language: "typescript"},
		}, []*graph.Edge{
			{From: "src/caller.ts::run", To: "unresolved::handler", Kind: graph.EdgeReads, FilePath: "src/caller.ts", Line: 4},
			// No-op for LSP -> must NOT be carried in spool after Site 1 refusal.
			{From: "src/caller.ts::run", To: "unresolved::missingFn", Kind: graph.EdgeCalls, FilePath: "src/caller.ts", Line: 5},
		})

		helper := &fakeLSPHelper{
			exts: []string{".ts"},
			defs: map[lspKey]lspAnswer{
				{path: "src/caller.ts", line: 4, name: "handler"}: {defPath: "a/real.ts", defLine: 5},
			},
		}
		r := New(store)
		r.SetLSPHelper(helper)

		stats, err := r.ResolveAllContext(context.Background())
		require.ErrorIs(t, err, ErrReindexWriteRefused)
		assert.True(t, stats.WriteRefused)
		assert.Zero(t, stats.LSPResolved, "refused deferred LSP batch must not credit LSPResolved")
		assert.Equal(t, 1, store.refused)
		assert.Equal(t, 1, store.calls)
		require.NotNil(t, r.lspDeferredSpool, "refused reindexBatch entry must stay carried in spool")
		assert.Equal(t, 1, r.lspDeferredSpool.count(), "only the refused reindexBatch entry (not the no-op edge) should remain carried")

		var handlerEdge *graph.Edge
		for _, e := range store.GetOutEdges("src/caller.ts::run") {
			if e.Line == 4 {
				handlerEdge = e
			}
		}
		require.NotNil(t, handlerEdge)
		assert.Equal(t, graph.EdgeReads, handlerEdge.Kind, "refused promotion must not mutate EdgeReads to EdgeReferences")
		assert.Equal(t, "unresolved::handler", handlerEdge.To)

		// Second pass commits cleanly and drains the carried spool entry.
		stats, err = r.ResolveAllContext(context.Background())
		require.NoError(t, err)
		assert.False(t, stats.WriteRefused)
		assert.Equal(t, 1, stats.LSPResolved)
		assert.Nil(t, r.lspDeferredSpool)

		handlerEdge = nil
		for _, e := range store.GetOutEdges("src/caller.ts::run") {
			if e.Line == 4 {
				handlerEdge = e
			}
		}
		require.NotNil(t, handlerEdge)
		assert.Equal(t, graph.EdgeReferences, handlerEdge.Kind)
		assert.Equal(t, "a/real.ts::renamedHandler", handlerEdge.To)
	})

	t.Run("site2_placeholder_refusal_preserves_committed_lsp_and_deletes_original_pk", func(t *testing.T) {
		store := openRefusingReindexStore(t)
		store.refuseNext = false
		store.refuseWhen = func(call int, batch []graph.EdgeReindex) bool {
			return call == 2 && len(batch) > 0 && batch[0].RefreshIdentity
		}

		store.AddBatch([]*graph.Node{
			{ID: "src/caller.ts", Kind: graph.KindFile, Name: "caller.ts", FilePath: "src/caller.ts", Language: "typescript"},
			{ID: "src/caller.ts::run", Kind: graph.KindFunction, Name: "run", FilePath: "src/caller.ts", StartLine: 1, EndLine: 8, Language: "typescript"},
			{ID: "src/caller.ts::sink", Kind: graph.KindFunction, Name: "sink", FilePath: "src/caller.ts", StartLine: 10, EndLine: 12, Language: "typescript"},
			{ID: "a/real.ts", Kind: graph.KindFile, Name: "real.ts", FilePath: "a/real.ts", Language: "typescript"},
			{ID: "a/real.ts::renamedHandler", Kind: graph.KindFunction, Name: "renamedHandler", FilePath: "a/real.ts", StartLine: 5, EndLine: 7, Language: "typescript"},
		}, []*graph.Edge{
			{From: "src/caller.ts::run", To: "unresolved::handler", Kind: graph.EdgeReads, FilePath: "src/caller.ts", Line: 4},
			{From: "unresolved::handler", To: "src/caller.ts::sink", Kind: graph.EdgeArgOf, FilePath: "src/caller.ts", Line: 4},
		})

		helper := &fakeLSPHelper{
			exts: []string{".ts"},
			defs: map[lspKey]lspAnswer{
				{path: "src/caller.ts", line: 4, name: "handler"}: {defPath: "a/real.ts", defLine: 5},
			},
		}
		r := New(store)
		r.SetLSPHelper(helper)

		stats, err := r.ResolveAllContext(context.Background())
		require.ErrorIs(t, err, ErrReindexWriteRefused)
		assert.True(t, stats.WriteRefused)
		assert.Equal(t, 2, store.calls, "call 1 must be deferred LSP reindex and call 2 its placeholder reconcile")
		assert.Equal(t, 1, store.refused)
		assert.Equal(t, 1, stats.LSPResolved, "committed deferred LSP bind must stay counted on Site 2 refusal")
		assert.Nil(t, r.lspDeferredSpool, "committed EdgeReads->EdgeReferences entry must delete its original EdgeReads spool PK")

		var handlerEdge *graph.Edge
		for _, e := range store.GetOutEdges("src/caller.ts::run") {
			if e.Line == 4 {
				handlerEdge = e
			}
		}
		require.NotNil(t, handlerEdge)
		assert.Equal(t, graph.EdgeReferences, handlerEdge.Kind)
		assert.Equal(t, "a/real.ts::renamedHandler", handlerEdge.To)
		assert.Len(t, store.GetOutEdges("unresolved::handler"), 1, "refused placeholder move must remain at original From")
	})
}

func TestReconcileIndexedPlaceholderSourcesAppliedContinuesAfterPage1Refusal(t *testing.T) {
	const (
		count    = placeholderSourceReindexBatchSize + 4
		oldFrom  = "unresolved::*.id"
		filePath = "repo/use.go"
	)
	mem := &refusingMemoryStore{
		Graph:      graph.New(),
		refuseWhen: func(call int, _ []graph.EdgeReindex) bool { return call == 1 },
	}
	edges := make([]*graph.Edge, 0, count)
	moves := make([]placeholderSourceMove, 0, count)
	for i := 0; i < count; i++ {
		e := &graph.Edge{
			From: oldFrom, To: fmt.Sprintf("repo/callee-%03d", i), Kind: graph.EdgeArgOf,
			FilePath: filePath, Line: i + 1,
		}
		edges = append(edges, e)
		moves = append(moves, placeholderSourceMove{
			identity: graph.EdgeIdentityFor(e),
			newFrom:  fmt.Sprintf("repo/field-%03d", i),
		})
	}
	mem.AddBatch(nil, edges)

	moved, ok := reconcileIndexedPlaceholderSourcesApplied(mem, mem, moves)
	assert.False(t, ok)
	assert.Equal(t, 1, mem.refused)
	assert.Equal(t, 4, moved, "page 2 must commit after page 1 refusal")
	assert.Len(t, mem.GetOutEdges(oldFrom), placeholderSourceReindexBatchSize, "page 1 edges must stay at oldFrom")
	for i := 0; i < placeholderSourceReindexBatchSize; i++ {
		assert.Equal(t, oldFrom, edges[i].From, "refused page 1 live edge %d must not be mutated in place", i)
		assert.Empty(t, mem.GetOutEdges(fmt.Sprintf("repo/field-%03d", i)))
	}
	for i := placeholderSourceReindexBatchSize; i < count; i++ {
		assert.Len(t, mem.GetOutEdges(fmt.Sprintf("repo/field-%03d", i)), 1)
	}
}

func TestCrossRepoResolveAllContextRefusedNpmRestubAndChunkContinuation(t *testing.T) {
	t.Run("refused_npm_restub_skips_import_update_and_continues_to_pending", func(t *testing.T) {
		store := openRefusingReindexStore(t)
		store.refuseNext = false
		store.refuseWhen = func(call int, _ []graph.EdgeReindex) bool { return call == 1 }

		store.AddBatch([]*graph.Node{
			{ID: "app/src/app.ts", Kind: graph.KindFile, Name: "app.ts", FilePath: "app/src/app.ts", RepoPrefix: "app", Language: "typescript"},
			{ID: "app/src/app.ts::run", Kind: graph.KindFunction, Name: "run", FilePath: "app/src/app.ts", RepoPrefix: "app", Language: "typescript"},
			{ID: "app/src/other.ts", Kind: graph.KindFile, Name: "other.ts", FilePath: "app/src/other.ts", RepoPrefix: "app", Language: "typescript"},
			{ID: "app/src/other.ts::go", Kind: graph.KindFunction, Name: "go", FilePath: "app/src/other.ts", RepoPrefix: "app", Language: "typescript"},
			{ID: "lib1/src/index.ts", Kind: graph.KindFile, Name: "index.ts", FilePath: "lib1/src/index.ts", RepoPrefix: "lib1", Language: "typescript"},
			{ID: "lib1/src/index.ts::f", Kind: graph.KindFunction, Name: "f", FilePath: "lib1/src/index.ts", RepoPrefix: "lib1", Language: "typescript", Meta: map[string]any{"visibility": "public"}},
			{ID: "lib2/src/index.ts", Kind: graph.KindFile, Name: "index.ts", FilePath: "lib2/src/index.ts", RepoPrefix: "lib2", Language: "typescript"},
			{ID: "lib2/src/index.ts::f", Kind: graph.KindFunction, Name: "f", FilePath: "lib2/src/index.ts", RepoPrefix: "lib2", Language: "typescript", Meta: map[string]any{"visibility": "public"}},
			{ID: "lib2/src/other.ts", Kind: graph.KindFile, Name: "other.ts", FilePath: "lib2/src/other.ts", RepoPrefix: "lib2", Language: "typescript"},
			{ID: "lib2/src/other.ts::g", Kind: graph.KindFunction, Name: "g", FilePath: "lib2/src/other.ts", RepoPrefix: "lib2", Language: "typescript"},
		}, []*graph.Edge{
			// Existing cross-repo npm import pointing at lib1, about to move to lib2.
			{From: "app/src/app.ts", To: "lib1/src/index.ts::f", Kind: graph.EdgeImports, FilePath: "app/src/app.ts", Line: 1, CrossRepo: true, Meta: map[string]any{NpmImportSpecifierMeta: "p::f"}},
			{From: "app/src/app.ts::run", To: "lib1/src/index.ts::f", Kind: graph.EdgeCalls, FilePath: "app/src/app.ts", Line: 2, CrossRepo: true},
			{From: "app/src/app.ts::run", To: "lib1/src/index.ts::f", Kind: graph.EdgeCrossRepoCalls, FilePath: "app/src/app.ts", Line: 2, CrossRepo: true},
			// Unrelated pending cross-repo call that should still resolve after npm restub refusal.
			{From: "app/src/other.ts", To: "lib2/src/other.ts", Kind: graph.EdgeImports, FilePath: "app/src/other.ts", Line: 1, CrossRepo: true},
			{From: "app/src/other.ts::go", To: "unresolved::g", Kind: graph.EdgeCalls, FilePath: "app/src/other.ts", Line: 2},
		})

		cr := NewCrossRepo(store)
		cr.SetNpmDependencyLookup(func(_, spec string) bool { return spec == "p" })
		cr.SetNpmPackageLookup(func(spec string) map[string]NpmPackageTarget {
			if spec != "p" {
				return nil
			}
			return map[string]NpmPackageTarget{"lib2": {Entries: []string{"lib2/src/index.ts"}}}
		})

		stats, err := cr.ResolveAllContext(context.Background())
		require.ErrorIs(t, err, ErrReindexWriteRefused)
		assert.True(t, stats.WriteRefused)
		assert.Equal(t, 1, store.refused)
		assert.Equal(t, 1, stats.Resolved, "only the committed pending edge should count as resolved")
		assert.Equal(t, 1, stats.CrossRepoEdges)
		assert.Equal(t, 1, stats.ByRepo["lib2"])

		// Import must stay at lib1 because restub was refused.
		appImports := store.GetOutEdges("app/src/app.ts")
		require.Len(t, appImports, 1)
		assert.Equal(t, "lib1/src/index.ts::f", appImports[0].To)

		// Both EdgeCalls and its parallel EdgeCrossRepoCalls must stay intact.
		var sawCall, sawParallel bool
		for _, e := range store.GetOutEdges("app/src/app.ts::run") {
			if e.Kind == graph.EdgeCalls && e.To == "lib1/src/index.ts::f" && e.CrossRepo {
				sawCall = true
			}
			if e.Kind == graph.EdgeCrossRepoCalls && e.To == "lib1/src/index.ts::f" {
				sawParallel = true
			}
		}
		assert.True(t, sawCall, "refused restub must preserve live EdgeCalls")
		assert.True(t, sawParallel, "refused restub must not delete EdgeCrossRepoCalls")

		// Pending call in other.ts still resolved.
		var otherBound bool
		for _, e := range store.GetOutEdges("app/src/other.ts::go") {
			if e.Kind == graph.EdgeCalls && e.To == "lib2/src/other.ts::g" {
				otherBound = true
			}
		}
		assert.True(t, otherBound, "pending cross-repo edges must still resolve after npm restub refusal")
	})

	t.Run("refused_chunk1_resolves_chunk2_with_accurate_stats", func(t *testing.T) {
		t.Setenv("GORTEX_RESOLVE_CHUNK_SIZE", "1")
		store := openRefusingReindexStore(t)
		store.refuseNext = false
		store.refuseWhen = func(call int, _ []graph.EdgeReindex) bool { return call == 1 }

		store.AddBatch([]*graph.Node{
			{ID: "a/c1.ts", Kind: graph.KindFile, Name: "c1.ts", FilePath: "a/c1.ts", RepoPrefix: "a", Language: "typescript"},
			{ID: "a/c1.ts::run1", Kind: graph.KindFunction, Name: "run1", FilePath: "a/c1.ts", RepoPrefix: "a", Language: "typescript"},
			{ID: "a/c2.ts", Kind: graph.KindFile, Name: "c2.ts", FilePath: "a/c2.ts", RepoPrefix: "a", Language: "typescript"},
			{ID: "a/c2.ts::run2", Kind: graph.KindFunction, Name: "run2", FilePath: "a/c2.ts", RepoPrefix: "a", Language: "typescript"},
			{ID: "b/t1.ts", Kind: graph.KindFile, Name: "t1.ts", FilePath: "b/t1.ts", RepoPrefix: "b", Language: "typescript"},
			{ID: "b/t1.ts::h1", Kind: graph.KindFunction, Name: "h1", FilePath: "b/t1.ts", RepoPrefix: "b", Language: "typescript"},
			{ID: "b/t2.ts", Kind: graph.KindFile, Name: "t2.ts", FilePath: "b/t2.ts", RepoPrefix: "b", Language: "typescript"},
			{ID: "b/t2.ts::h2", Kind: graph.KindFunction, Name: "h2", FilePath: "b/t2.ts", RepoPrefix: "b", Language: "typescript"},
		}, []*graph.Edge{
			{From: "a/c1.ts", To: "b/t1.ts", Kind: graph.EdgeImports, FilePath: "a/c1.ts", Line: 1, CrossRepo: true},
			{From: "a/c1.ts::run1", To: "unresolved::h1", Kind: graph.EdgeCalls, FilePath: "a/c1.ts", Line: 2},
			{From: "a/c2.ts", To: "b/t2.ts", Kind: graph.EdgeImports, FilePath: "a/c2.ts", Line: 1, CrossRepo: true},
			{From: "a/c2.ts::run2", To: "unresolved::h2", Kind: graph.EdgeCalls, FilePath: "a/c2.ts", Line: 2},
		})

		cr := NewCrossRepo(store)
		stats, err := cr.ResolveAllContext(context.Background())
		require.ErrorIs(t, err, ErrReindexWriteRefused)
		assert.True(t, stats.WriteRefused)
		assert.Equal(t, 1, store.refused)
		assert.Equal(t, 1, stats.Resolved)
		assert.Equal(t, 1, stats.Unresolved)
		assert.Equal(t, 1, stats.CrossRepoEdges)
		assert.Equal(t, 1, stats.ByRepo["b"])

		boundCount := 0
		parallelCount := 0
		for _, caller := range []string{"a/c1.ts::run1", "a/c2.ts::run2"} {
			for _, e := range store.GetOutEdges(caller) {
				if e.Kind == graph.EdgeCalls && !graph.IsUnresolvedTarget(e.To) {
					boundCount++
				}
				if e.Kind == graph.EdgeCrossRepoCalls {
					parallelCount++
				}
			}
		}
		assert.Equal(t, 1, boundCount, "chunk 2 must resolve while chunk 1 stays unresolved")
		assert.Equal(t, 1, parallelCount, "only the committed chunk may emit EdgeCrossRepoCalls")
	})

	t.Run("no_pending_after_refused_npm_import_returns_refused_error", func(t *testing.T) {
		store := openRefusingReindexStore(t)
		store.AddBatch([]*graph.Node{
			{ID: "app/src/app.ts", Kind: graph.KindFile, Name: "app.ts", FilePath: "app/src/app.ts", RepoPrefix: "app", Language: "typescript"},
			{ID: "lib/src/index.ts", Kind: graph.KindFile, Name: "index.ts", FilePath: "lib/src/index.ts", RepoPrefix: "lib", Language: "typescript"},
			{ID: "lib/src/index.ts::f", Kind: graph.KindFunction, Name: "f", FilePath: "lib/src/index.ts", RepoPrefix: "lib", Language: "typescript", Meta: map[string]any{"visibility": "public"}},
		}, []*graph.Edge{
			{From: "app/src/app.ts", To: "external::p::f", Kind: graph.EdgeImports, FilePath: "app/src/app.ts", Line: 1},
		})

		cr := NewCrossRepo(store)
		cr.SetNpmDependencyLookup(func(_, spec string) bool { return spec == "p" })
		cr.SetNpmPackageLookup(func(spec string) map[string]NpmPackageTarget {
			if spec != "p" {
				return nil
			}
			return map[string]NpmPackageTarget{"lib": {Entries: []string{"lib/src/index.ts"}}}
		})

		stats, err := cr.ResolveAllContext(context.Background())
		require.ErrorIs(t, err, ErrReindexWriteRefused)
		assert.True(t, stats.WriteRefused)
		assert.Zero(t, stats.Resolved)
		assert.Zero(t, stats.CrossRepoEdges)
		assert.Empty(t, stats.ByRepo)
		require.Len(t, store.GetOutEdges("app/src/app.ts"), 1)
		assert.Equal(t, "external::p::f", store.GetOutEdges("app/src/app.ts")[0].To)
	})
}
