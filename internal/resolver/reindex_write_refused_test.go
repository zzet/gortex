package resolver

import (
	"context"
	"fmt"
	"io"
	"iter"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

type refusingReindexStore struct {
	*store_sqlite.Store
	refuseFunc       func(call int, batch []graph.EdgeReindex) bool
	refuseTargetFunc func(call int, batch []graph.UnresolvedEdgeTargetReindex) bool
	calls            int
	targetCalls      int
	refused          int
}

func (s *refusingReindexStore) ReindexEdges(batch []graph.EdgeReindex) {
	s.ReindexEdgesTryApplied(batch, 0)
}

func (s *refusingReindexStore) ReindexEdgesApplied(batch []graph.EdgeReindex) bool {
	return s.ReindexEdgesTryApplied(batch, 0)
}

func (s *refusingReindexStore) ReindexEdgesTryApplied(batch []graph.EdgeReindex, maxWait time.Duration) bool {
	s.calls++
	if s.refuseFunc != nil && s.refuseFunc(s.calls, batch) {
		s.refused++
		return false
	}
	return s.Store.ReindexEdgesTryApplied(batch, maxWait)
}

func refuseOnceMatching(pred func([]graph.EdgeReindex) bool) func(int, []graph.EdgeReindex) bool {
	fired := false
	return func(_ int, batch []graph.EdgeReindex) bool {
		if fired || pred == nil || !pred(batch) {
			return false
		}
		fired = true
		return true
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

func batchHasImportFrom(fileID string) func([]graph.EdgeReindex) bool {
	return batchHasEdge(func(ri graph.EdgeReindex) bool {
		return ri.Edge != nil && ri.Edge.Kind == graph.EdgeImports && ri.Edge.From == fileID
	})
}

func (s *refusingReindexStore) ReindexUnresolvedEdgeTargets(batch []graph.UnresolvedEdgeTargetReindex) {
	s.ReindexUnresolvedEdgeTargetsApplied(batch)
}

func (s *refusingReindexStore) ReindexUnresolvedEdgeTargetsApplied(batch []graph.UnresolvedEdgeTargetReindex) bool {
	s.targetCalls++
	if s.refuseTargetFunc != nil && s.refuseTargetFunc(s.targetCalls, batch) {
		s.refused++
		return false
	}
	return s.Store.ReindexUnresolvedEdgeTargetsApplied(batch)
}

func openRefusingReindexStore(t *testing.T) *refusingReindexStore {
	t.Helper()
	s, err := store_sqlite.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	return &refusingReindexStore{Store: s}
}

func TestResolveAllContextReturnsErrorOnRefusedReindexWrite(t *testing.T) {
	store := openRefusingReindexStore(t)
	store.refuseFunc = refuseOnceMatching(batchHasCallTo("target.ts::helper"))
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
	store.refuseFunc = refuseOnceMatching(batchHasCallTo("target.ts::helper"))
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
	store.refuseFunc = refuseOnceMatching(batchHasCallTo("b/target.ts::helper"))
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

func TestRefusedOutgoingIncrementalDoesNotLeakMutatedFrontierIntoAttribution(t *testing.T) {
	for _, mode := range []string{"files_and_incoming", "files_outgoing"} {
		t.Run(mode, func(t *testing.T) {
			store := openRefusingReindexStore(t)
			store.refuseFunc = refuseOnceMatching(batchHasCallTo("b/other.go::T.Foo"))
			store.AddBatch([]*graph.Node{
				{ID: "a/caller.go", Kind: graph.KindFile, Name: "caller.go", FilePath: "a/caller.go", Language: "go"},
				{ID: "a/caller.go::Run", Kind: graph.KindFunction, Name: "Run", FilePath: "a/caller.go", Language: "go"},
				{ID: "a/caller.go::Run#local:x", Kind: graph.KindLocal, Name: "x", FilePath: "a/caller.go", Language: "go", StartLine: 2},
				{ID: "b/other.go", Kind: graph.KindFile, Name: "other.go", FilePath: "b/other.go", Language: "go"},
				{ID: "b/other.go::T.Foo", Kind: graph.KindMethod, Name: "Foo", FilePath: "b/other.go", Language: "go", Meta: map[string]any{"receiver": "T"}},
				// Second same-name method makes untyped EdgeArgOf ambiguous during outgoing
				// resolution so it stays unresolved::*.Foo and would be misbound by
				// bindDataflowCalleeRefsForFile if mutated EdgeCalls leaked into attribution.
				{ID: "d/other.go", Kind: graph.KindFile, Name: "other.go", FilePath: "d/other.go", Language: "go"},
				{ID: "d/other.go::U.Foo", Kind: graph.KindMethod, Name: "Foo", FilePath: "d/other.go", Language: "go", Meta: map[string]any{"receiver": "U"}},
				{ID: "c/other.go", Kind: graph.KindFile, Name: "other.go", FilePath: "c/other.go", Language: "go"},
				{ID: "c/other.go::Dep", Kind: graph.KindFunction, Name: "Dep", FilePath: "c/other.go", Language: "go"},
			}, []*graph.Edge{
				{From: "a/caller.go", To: "c/other.go", Kind: graph.EdgeImports, FilePath: "a/caller.go", Line: 1},
				{From: "a/caller.go::Run", To: "unresolved::*.Foo", Kind: graph.EdgeCalls, FilePath: "a/caller.go", Line: 10, Meta: map[string]any{"receiver_type": "T"}},
				{From: "a/caller.go::Run#local:x", To: "unresolved::*.Foo", Kind: graph.EdgeArgOf, FilePath: "a/caller.go", Line: 10},
			})

			r := New(store)
			var stats *ResolveStats
			if mode == "files_and_incoming" {
				stats = r.ResolveFilesAndIncoming([]string{"a/caller.go"})
			} else {
				stats = r.ResolveFilesOutgoing([]string{"a/caller.go"})
			}
			assert.True(t, stats.WriteRefused)
			assert.Equal(t, 1, store.refused)

			argEdges := store.GetOutEdges("a/caller.go::Run#local:x")
			require.Len(t, argEdges, 1)
			assert.Equal(t, "unresolved::*.Foo", argEdges[0].To, "refused outgoing write must not leak mutated frontier edges into attribution")
		})
	}
}

func TestRefusedChunk2RetainsPlaceholderRepointsWithoutExtraApplyDeferred(t *testing.T) {
	t.Setenv("GORTEX_RESOLVE_CHUNK", "1")
	t.Setenv("GORTEX_RESOLVE_CHUNK_SIZE", "1")

	store := openRefusingReindexStore(t)
	store.refuseFunc = refuseOnceMatching(batchHasCallTo("callee.ts::second"))
	store.AddBatch([]*graph.Node{
		{ID: "caller.ts", Kind: graph.KindFile, Name: "caller.ts", FilePath: "caller.ts", Language: "typescript"},
		{ID: "caller.ts::run", Kind: graph.KindFunction, Name: "run", FilePath: "caller.ts", Language: "typescript"},
		{ID: "callee.ts", Kind: graph.KindFile, Name: "callee.ts", FilePath: "callee.ts", Language: "typescript"},
		{ID: "callee.ts::first", Kind: graph.KindFunction, Name: "first", FilePath: "callee.ts", Language: "typescript"},
		{ID: "callee.ts::second", Kind: graph.KindFunction, Name: "second", FilePath: "callee.ts", Language: "typescript"},
	}, []*graph.Edge{
		{From: "caller.ts", To: "callee.ts", Kind: graph.EdgeImports, FilePath: "caller.ts", Line: 1},
		{From: "caller.ts::run", To: "unresolved::first", Kind: graph.EdgeCalls, FilePath: "caller.ts", Line: 2},
		{From: "caller.ts::run", To: "unresolved::second", Kind: graph.EdgeCalls, FilePath: "caller.ts", Line: 3},
		{From: "unresolved::first", To: "caller.ts::run", Kind: graph.EdgeArgOf, FilePath: "caller.ts", Line: 2},
	})

	r := New(store)
	stats, err := r.ResolveAllContext(context.Background())
	require.ErrorIs(t, err, ErrReindexWriteRefused)
	assert.True(t, stats.WriteRefused)
	assert.Equal(t, 1, store.refused)
	assert.Equal(t, 2, store.calls, "refused chunk abort must not fire unchecked applyDeferred into the contended gate")
	assert.Len(t, store.GetOutEdges("unresolved::first"), 1)

	store.refuseFunc = nil
	stats, err = r.ResolveAllContext(context.Background())
	require.NoError(t, err)
	assert.False(t, stats.WriteRefused)
	assert.Empty(t, store.GetOutEdges("unresolved::first"), "carried placeholder repoint must move dataflow source on retry")
	moved := store.GetOutEdges("callee.ts::first")
	require.Len(t, moved, 1)
	assert.Equal(t, graph.EdgeArgOf, moved[0].Kind)
	assert.Equal(t, "caller.ts::run", moved[0].To)
}

func TestRefusedApplyDeferredRetainsPlaceholderRepointsWhenNoPendingEdgesRemain(t *testing.T) {
	store := openRefusingReindexStore(t)
	store.refuseFunc = refuseOnceMatching(batchHasPlaceholderMove("unresolved::first", "callee.ts::first"))
	store.AddBatch([]*graph.Node{
		{ID: "caller.ts", Kind: graph.KindFile, Name: "caller.ts", FilePath: "caller.ts", Language: "typescript"},
		{ID: "caller.ts::run", Kind: graph.KindFunction, Name: "run", FilePath: "caller.ts", Language: "typescript"},
		{ID: "callee.ts", Kind: graph.KindFile, Name: "callee.ts", FilePath: "callee.ts", Language: "typescript"},
		{ID: "callee.ts::first", Kind: graph.KindFunction, Name: "first", FilePath: "callee.ts", Language: "typescript"},
	}, []*graph.Edge{
		{From: "caller.ts", To: "callee.ts", Kind: graph.EdgeImports, FilePath: "caller.ts", Line: 1},
		{From: "caller.ts::run", To: "unresolved::first", Kind: graph.EdgeCalls, FilePath: "caller.ts", Line: 2},
		{From: "unresolved::first", To: "caller.ts::run", Kind: graph.EdgeArgOf, FilePath: "caller.ts", Line: 2},
	})

	r := New(store)
	stats, err := r.ResolveAllContext(context.Background())
	require.ErrorIs(t, err, ErrReindexWriteRefused)
	assert.True(t, stats.WriteRefused)
	assert.Equal(t, 1, store.refused)
	assert.Equal(t, 2, store.calls, "refused applyDeferredApplied must not re-fire in resolveError")

	store.refuseFunc = nil
	stats, err = r.ResolveAllContext(context.Background())
	require.NoError(t, err)
	assert.False(t, stats.WriteRefused)
	assert.Empty(t, store.GetOutEdges("unresolved::first"))
	moved := store.GetOutEdges("callee.ts::first")
	require.Len(t, moved, 1)
	assert.Equal(t, graph.EdgeArgOf, moved[0].Kind)
}

func TestCrossRepoScopedAbortsAfterRefusedNpmImportWrite(t *testing.T) {
	store := openRefusingReindexStore(t)
	store.refuseFunc = refuseOnceMatching(batchHasImportFrom("app/src/app.ts"))
	store.AddBatch([]*graph.Node{
		{ID: "app/src/app.ts", Kind: graph.KindFile, FilePath: "app/src/app.ts", RepoPrefix: "app", Language: "typescript"},
		{ID: "app/src/app.ts::run", Kind: graph.KindFunction, Name: "run", FilePath: "app/src/app.ts", RepoPrefix: "app", Language: "typescript"},
		{ID: "lib/src/index.ts", Kind: graph.KindFile, FilePath: "lib/src/index.ts", RepoPrefix: "lib", Language: "typescript"},
		{ID: "lib/src/index.ts::f", Kind: graph.KindFunction, Name: "f", FilePath: "lib/src/index.ts", RepoPrefix: "lib", Language: "typescript", Meta: map[string]any{"visibility": "public"}},
		{ID: "other/src/index.ts", Kind: graph.KindFile, FilePath: "other/src/index.ts", RepoPrefix: "other", Language: "typescript"},
		{ID: "other/src/index.ts::f", Kind: graph.KindFunction, Name: "f", FilePath: "other/src/index.ts", RepoPrefix: "other", Language: "typescript", Meta: map[string]any{"visibility": "public"}},
	}, []*graph.Edge{
		{From: "app/src/app.ts", To: "external::p::f", Kind: graph.EdgeImports, FilePath: "app/src/app.ts", Line: 1},
		{From: "app/src/app.ts", To: "other/src/index.ts", Kind: graph.EdgeImports, FilePath: "app/src/app.ts", Line: 2, CrossRepo: true},
		{From: "app/src/app.ts::run", To: "unresolved::f", Kind: graph.EdgeCalls, FilePath: "app/src/app.ts", Line: 3},
	})

	cr := NewCrossRepo(store)
	cr.SetNpmDependencyLookup(func(_, spec string) bool { return spec == "p" })
	cr.SetNpmPackageLookup(func(spec string) map[string]NpmPackageTarget {
		if spec != "p" {
			return nil
		}
		return map[string]NpmPackageTarget{"lib": {Entries: []string{"lib/src/index.ts", "lib"}}}
	})

	stats := cr.ResolveFilesAndIncoming([]string{"app/src/app.ts"})
	assert.True(t, stats.WriteRefused)
	assert.Equal(t, 1, store.refused)
	calls := store.GetOutEdges("app/src/app.ts::run")
	require.Len(t, calls, 1)
	assert.Equal(t, "unresolved::f", calls[0].To, "scoped cross-repo must abort instead of binding fallback call from stale imports")

	stats = cr.ResolveFilesAndIncoming([]string{"app/src/app.ts"})
	assert.False(t, stats.WriteRefused)
	var sawLibCall bool
	for _, e := range store.GetOutEdges("app/src/app.ts::run") {
		if e.Kind == graph.EdgeCalls && e.To == "lib/src/index.ts::f" {
			sawLibCall = true
		}
	}
	assert.True(t, sawLibCall)
}

func TestResolveRazorUsingsPreservesMarkersOnRefusedWrite(t *testing.T) {
	store := openRefusingReindexStore(t)
	store.refuseFunc = refuseOnceMatching(batchHasEdge(func(ri graph.EdgeReindex) bool {
		return ri.Edge != nil && ri.Edge.To == "App/Widgets/Counter.razor::Counter"
	}))
	store.AddBatch([]*graph.Node{
		{ID: "Components/_Imports.razor", Kind: graph.KindFile, Name: "_Imports.razor", FilePath: "Components/_Imports.razor", Language: "razor"},
		{ID: "Components/Page.razor", Kind: graph.KindFile, Name: "Page.razor", FilePath: "Components/Page.razor", Language: "razor"},
		{ID: "App/Widgets/Counter.razor", Kind: graph.KindFile, Name: "Counter.razor", FilePath: "App/Widgets/Counter.razor", Language: "razor"},
		{ID: "Components/Page.razor::Page", Kind: graph.KindType, Name: "Page", FilePath: "Components/Page.razor", Language: "razor", Meta: map[string]any{"component": true, "scope_ns": "Components"}},
		{ID: "App/Widgets/Counter.razor::Counter", Kind: graph.KindType, Name: "Counter", FilePath: "App/Widgets/Counter.razor", Language: "razor", Meta: map[string]any{"component": true, "scope_ns": "App.Widgets"}},
		// Add a second same-name component in another namespace so phase 1 leaves Counter ambiguous for resolveRazorUsings.
		{ID: "Other/Counter.razor", Kind: graph.KindFile, Name: "Counter.razor", FilePath: "Other/Counter.razor", Language: "razor"},
		{ID: "Other/Counter.razor::Counter", Kind: graph.KindType, Name: "Counter", FilePath: "Other/Counter.razor", Language: "razor", Meta: map[string]any{"component": true, "scope_ns": "Other"}},
	}, []*graph.Edge{
		{From: "Components/_Imports.razor", To: "unresolved::razor_using::App.Widgets", Kind: graph.EdgeImports, FilePath: "Components/_Imports.razor", Line: 1},
		{From: "Components/Page.razor::Page", To: "unresolved::Counter", Kind: graph.EdgeReferences, FilePath: "Components/Page.razor", Line: 2},
	})

	r := New(store)
	stats, err := r.ResolveAllContext(context.Background())
	require.ErrorIs(t, err, ErrReindexWriteRefused)
	assert.True(t, stats.WriteRefused)
	assert.Equal(t, 1, store.refused)
	markers := store.GetOutEdges("Components/_Imports.razor")
	require.Len(t, markers, 1, "marker edge must survive a refused razor_using write")
	assert.Equal(t, "unresolved::razor_using::App.Widgets", markers[0].To)

	stats, err = r.ResolveAllContext(context.Background())
	require.NoError(t, err)
	assert.False(t, stats.WriteRefused)
	assert.Empty(t, store.GetOutEdges("Components/_Imports.razor"))
	refs := store.GetOutEdges("Components/Page.razor::Page")
	require.Len(t, refs, 1)
	assert.Equal(t, "App/Widgets/Counter.razor::Counter", refs[0].To)
}

func TestRestubCallsForChangedNpmImportsPreservesParallelAndImportOnRefusedWrite(t *testing.T) {
	store := openRefusingReindexStore(t)
	store.refuseFunc = refuseOnceMatching(batchHasEdge(func(ri graph.EdgeReindex) bool {
		return ri.Edge != nil && ri.Edge.Kind == graph.EdgeCalls && ri.OldTo == "oldlib/src/index.ts::f"
	}))
	store.AddBatch([]*graph.Node{
		{ID: "app/src/app.ts", Kind: graph.KindFile, FilePath: "app/src/app.ts", RepoPrefix: "app", Language: "typescript"},
		{ID: "app/src/app.ts::run", Kind: graph.KindFunction, Name: "run", FilePath: "app/src/app.ts", RepoPrefix: "app", Language: "typescript"},
		{ID: "oldlib/src/index.ts", Kind: graph.KindFile, FilePath: "oldlib/src/index.ts", RepoPrefix: "oldlib", Language: "typescript"},
		{ID: "oldlib/src/index.ts::f", Kind: graph.KindFunction, Name: "f", FilePath: "oldlib/src/index.ts", RepoPrefix: "oldlib", Language: "typescript", Meta: map[string]any{"visibility": "public"}},
		{ID: "newlib/src/index.ts", Kind: graph.KindFile, FilePath: "newlib/src/index.ts", RepoPrefix: "newlib", Language: "typescript"},
		{ID: "newlib/src/index.ts::f", Kind: graph.KindFunction, Name: "f", FilePath: "newlib/src/index.ts", RepoPrefix: "newlib", Language: "typescript", Meta: map[string]any{"visibility": "public"}},
	}, []*graph.Edge{
		{
			From: "app/src/app.ts", To: "oldlib/src/index.ts::f",
			Kind: graph.EdgeImports, FilePath: "app/src/app.ts", Line: 1, CrossRepo: true,
			Meta: map[string]any{NpmImportSpecifierMeta: "p::f"},
		},
		{From: "app/src/app.ts::run", To: "oldlib/src/index.ts::f", Kind: graph.EdgeCalls, FilePath: "app/src/app.ts", Line: 2, CrossRepo: true},
		{From: "app/src/app.ts::run", To: "oldlib/src/index.ts::f", Kind: graph.EdgeCrossRepoCalls, FilePath: "app/src/app.ts", Line: 2, CrossRepo: true},
	})

	cr := NewCrossRepo(store)
	cr.SetNpmDependencyLookup(func(_, spec string) bool { return spec == "p" })
	cr.SetNpmPackageLookup(func(spec string) map[string]NpmPackageTarget {
		if spec != "p" {
			return nil
		}
		return map[string]NpmPackageTarget{"newlib": {Entries: []string{"newlib/src/index.ts", "newlib"}}}
	})

	stats, err := cr.ResolveAllContext(context.Background())
	require.ErrorIs(t, err, ErrReindexWriteRefused)
	assert.True(t, stats.WriteRefused)
	assert.Equal(t, 1, store.refused)

	imports := store.GetOutEdges("app/src/app.ts")
	require.Len(t, imports, 1)
	assert.Equal(t, "oldlib/src/index.ts::f", imports[0].To, "import retarget must not commit when call restub is refused")
	var sawOldParallel bool
	for _, e := range store.GetOutEdges("app/src/app.ts::run") {
		if e.Kind == graph.EdgeCrossRepoCalls && e.To == "oldlib/src/index.ts::f" {
			sawOldParallel = true
		}
	}
	assert.True(t, sawOldParallel, "EdgeCrossRepoCalls must not be deleted when restub write is refused")

	stats, err = cr.ResolveAllContext(context.Background())
	require.NoError(t, err)
	assert.False(t, stats.WriteRefused)
	imports = store.GetOutEdges("app/src/app.ts")
	require.Len(t, imports, 1)
	assert.Equal(t, "newlib/src/index.ts::f", imports[0].To)
	var sawNewCall, sawNewParallel, sawOldCall bool
	for _, e := range store.GetOutEdges("app/src/app.ts::run") {
		if e.Kind == graph.EdgeCalls && e.To == "newlib/src/index.ts::f" {
			sawNewCall = true
		}
		if e.Kind == graph.EdgeCrossRepoCalls && e.To == "newlib/src/index.ts::f" {
			sawNewParallel = true
		}
		if e.To == "oldlib/src/index.ts::f" {
			sawOldCall = true
		}
	}
	assert.True(t, sawNewCall)
	assert.True(t, sawNewParallel)
	assert.False(t, sawOldCall)
}

func TestRefusedBareNameBindHaltsBeforeGoBuiltins(t *testing.T) {
	store := openRefusingReindexStore(t)
	var refusedBare atomic.Bool
	store.refuseTargetFunc = func(_ int, batch []graph.UnresolvedEdgeTargetReindex) bool {
		for _, ri := range batch {
			if ri.NewTo == "pkg/shadow.go::F#local:len" {
				return refusedBare.CompareAndSwap(false, true)
			}
		}
		return false
	}
	store.AddBatch([]*graph.Node{
		{ID: "pkg/shadow.go", Kind: graph.KindFile, Name: "shadow.go", FilePath: "pkg/shadow.go", Language: "go"},
		{ID: "pkg/shadow.go::F", Kind: graph.KindFunction, Name: "F", FilePath: "pkg/shadow.go", Language: "go", StartLine: 1},
		{ID: "pkg/shadow.go::F#local:len", Kind: graph.KindLocal, Name: "len", FilePath: "pkg/shadow.go", Language: "go", StartLine: 2},
	}, []*graph.Edge{
		{From: "pkg/shadow.go::F", To: "unresolved::len", Kind: graph.EdgeReads, FilePath: "pkg/shadow.go", Line: 5},
	})

	r := New(store)
	stats, err := r.ResolveAllContext(context.Background())
	require.ErrorIs(t, err, ErrReindexWriteRefused)
	assert.True(t, stats.WriteRefused)
	assert.Equal(t, 1, store.refused)
	edges := store.GetOutEdges("pkg/shadow.go::F")
	require.Len(t, edges, 1)
	assert.Equal(t, "unresolved::len", edges[0].To, "refused bare-name bind must halt before attributeGoBuiltins misbinds shadowed len")

	store.refuseTargetFunc = nil
	stats, err = r.ResolveAllContext(context.Background())
	require.NoError(t, err)
	assert.False(t, stats.WriteRefused)
	edges = store.GetOutEdges("pkg/shadow.go::F")
	require.Len(t, edges, 1)
	assert.Equal(t, "pkg/shadow.go::F#local:len", edges[0].To)
}

func TestRefusedCrossPackageGuardRevertCompletesOnRetry(t *testing.T) {
	store := openRefusingReindexStore(t)
	store.refuseFunc = refuseOnceMatching(batchHasGuardRevert("a/caller.go::Run"))
	store.AddBatch([]*graph.Node{
		{ID: "a/caller.go", Kind: graph.KindFile, Name: "caller.go", FilePath: "a/caller.go", Language: "go"},
		{ID: "a/caller.go::Run", Kind: graph.KindFunction, Name: "Run", FilePath: "a/caller.go", Language: "go"},
		{ID: "b/unimported.go", Kind: graph.KindFile, Name: "unimported.go", FilePath: "b/unimported.go", Language: "go"},
		{ID: "b/unimported.go::Secret", Kind: graph.KindFunction, Name: "Secret", FilePath: "b/unimported.go", Language: "go"},
		{ID: "c/imported.go", Kind: graph.KindFile, Name: "imported.go", FilePath: "c/imported.go", Language: "go"},
		{ID: "c/imported.go::Other", Kind: graph.KindFunction, Name: "Other", FilePath: "c/imported.go", Language: "go"},
	}, []*graph.Edge{
		{From: "a/caller.go", To: "c/imported.go", Kind: graph.EdgeImports, FilePath: "a/caller.go", Line: 1},
		{From: "a/caller.go::Run", To: "unresolved::Secret", Kind: graph.EdgeCalls, FilePath: "a/caller.go", Line: 5},
	})

	r := New(store)
	stats, err := r.ResolveAllContext(context.Background())
	require.ErrorIs(t, err, ErrReindexWriteRefused)
	assert.True(t, stats.WriteRefused)
	assert.Equal(t, 1, store.refused)

	store.refuseFunc = nil
	stats, err = r.ResolveAllContext(context.Background())
	require.NoError(t, err)
	assert.False(t, stats.WriteRefused)
	edges := store.GetOutEdges("a/caller.go::Run")
	require.Len(t, edges, 1)
	assert.Equal(t, "unresolved::Secret", edges[0].To, "retry must finish reverting the weak-tier binding without reparsing")
	assert.Equal(t, true, edges[0].Meta["guard_reverted"])
}

func TestReusedResolverPlaceholderRepointsSurviveAfterStartupResolveAll(t *testing.T) {
	store := openRefusingReindexStore(t)
	store.AddBatch([]*graph.Node{
		{ID: "init.ts", Kind: graph.KindFile, Name: "init.ts", FilePath: "init.ts", Language: "typescript"},
		{ID: "init.ts::boot", Kind: graph.KindFunction, Name: "boot", FilePath: "init.ts", Language: "typescript"},
		{ID: "util.ts", Kind: graph.KindFile, Name: "util.ts", FilePath: "util.ts", Language: "typescript"},
		{ID: "util.ts::initUtil", Kind: graph.KindFunction, Name: "initUtil", FilePath: "util.ts", Language: "typescript"},
	}, []*graph.Edge{
		{From: "init.ts", To: "util.ts", Kind: graph.EdgeImports, FilePath: "init.ts", Line: 1},
		{From: "init.ts::boot", To: "unresolved::initUtil", Kind: graph.EdgeCalls, FilePath: "init.ts", Line: 2},
		{From: "unresolved::initUtil", To: "init.ts::boot", Kind: graph.EdgeArgOf, FilePath: "init.ts", Line: 2},
	})

	r := New(store)
	// Startup ResolveAll populates placeholderSrcIdx and succeeds.
	_, err := r.ResolveAllContext(context.Background())
	require.NoError(t, err)
	require.Empty(t, store.GetOutEdges("unresolved::initUtil"))

	// Later incremental save adds a new call + dataflow edge; call commits, placeholder move is refused.
	store.AddBatch([]*graph.Node{
		{ID: "inc.ts", Kind: graph.KindFile, Name: "inc.ts", FilePath: "inc.ts", Language: "typescript"},
		{ID: "inc.ts::run", Kind: graph.KindFunction, Name: "run", FilePath: "inc.ts", Language: "typescript"},
		{ID: "inc_target.ts", Kind: graph.KindFile, Name: "inc_target.ts", FilePath: "inc_target.ts", Language: "typescript"},
		{ID: "inc_target.ts::incHelper", Kind: graph.KindFunction, Name: "incHelper", FilePath: "inc_target.ts", Language: "typescript"},
	}, []*graph.Edge{
		{From: "inc.ts", To: "inc_target.ts", Kind: graph.EdgeImports, FilePath: "inc.ts", Line: 1},
		{From: "inc.ts::run", To: "unresolved::incHelper", Kind: graph.EdgeCalls, FilePath: "inc.ts", Line: 2},
		{From: "unresolved::incHelper", To: "inc.ts::run", Kind: graph.EdgeArgOf, FilePath: "inc.ts", Line: 2},
	})

	store.refuseFunc = refuseOnceMatching(batchHasPlaceholderMove("unresolved::incHelper", "inc_target.ts::incHelper"))
	stats := r.ResolveFilesAndIncoming([]string{"inc.ts"})
	assert.True(t, stats.WriteRefused)
	assert.Equal(t, 1, store.refused)
	assert.Len(t, store.GetOutEdges("unresolved::incHelper"), 1)

	store.refuseFunc = nil
	stats = r.ResolveFilesAndIncoming([]string{"inc.ts"})
	assert.False(t, stats.WriteRefused)
	assert.Empty(t, store.GetOutEdges("unresolved::incHelper"), "reused resolver must preserve and flush retained repoints")
	moved := store.GetOutEdges("inc_target.ts::incHelper")
	require.Len(t, moved, 1)
	assert.Equal(t, graph.EdgeArgOf, moved[0].Kind)
}

func TestIncrementalRetryDoesNotBindDataflowOntoGuardRejectedTarget(t *testing.T) {
	store := openRefusingReindexStore(t)
	store.refuseFunc = refuseOnceMatching(batchHasPlaceholderMove("unresolved::Secret", "b/unimported.go::Secret"))
	store.AddBatch([]*graph.Node{
		{ID: "a/caller.go", Kind: graph.KindFile, Name: "caller.go", FilePath: "a/caller.go", Language: "go"},
		{ID: "a/caller.go::Run", Kind: graph.KindFunction, Name: "Run", FilePath: "a/caller.go", Language: "go"},
		{ID: "b/unimported.go", Kind: graph.KindFile, Name: "unimported.go", FilePath: "b/unimported.go", Language: "go"},
		{ID: "b/unimported.go::Secret", Kind: graph.KindFunction, Name: "Secret", FilePath: "b/unimported.go", Language: "go"},
		{ID: "c/imported.go", Kind: graph.KindFile, Name: "imported.go", FilePath: "c/imported.go", Language: "go"},
		{ID: "c/imported.go::Other", Kind: graph.KindFunction, Name: "Other", FilePath: "c/imported.go", Language: "go"},
	}, []*graph.Edge{
		{From: "a/caller.go", To: "c/imported.go", Kind: graph.EdgeImports, FilePath: "a/caller.go", Line: 1},
		{From: "a/caller.go::Run", To: "unresolved::Secret", Kind: graph.EdgeCalls, FilePath: "a/caller.go", Line: 5},
		{From: "unresolved::Secret", To: "a/caller.go::Run", Kind: graph.EdgeArgOf, FilePath: "a/caller.go", Line: 5},
	})

	r := New(store)
	stats := r.ResolveFilesAndIncoming([]string{"a/caller.go"})
	assert.True(t, stats.WriteRefused)
	assert.Equal(t, 1, store.refused)

	store.refuseFunc = nil
	stats = r.ResolveFilesAndIncoming([]string{"a/caller.go"})
	assert.False(t, stats.WriteRefused)
	assert.Empty(t, store.GetOutEdges("b/unimported.go::Secret"), "dataflow edge must never move onto a guard-rejected target")
	assert.Len(t, store.GetOutEdges("unresolved::Secret"), 1)
	calls := store.GetOutEdges("a/caller.go::Run")
	require.Len(t, calls, 1)
	assert.Equal(t, "unresolved::Secret", calls[0].To)
}

func TestPendingGuardRevertDoesNotClobberUpgradedOriginOrRunStaleSourceReverts(t *testing.T) {
	store := openRefusingReindexStore(t)
	store.refuseFunc = refuseOnceMatching(batchHasGuardRevert("a/caller.go::Run"))
	store.AddBatch([]*graph.Node{
		{ID: "a/caller.go", Kind: graph.KindFile, Name: "caller.go", FilePath: "a/caller.go", Language: "go"},
		{ID: "a/caller.go::Run", Kind: graph.KindFunction, Name: "Run", FilePath: "a/caller.go", Language: "go"},
		{ID: "b/unimported.go", Kind: graph.KindFile, Name: "unimported.go", FilePath: "b/unimported.go", Language: "go"},
		{ID: "b/unimported.go::Secret", Kind: graph.KindFunction, Name: "Secret", FilePath: "b/unimported.go", Language: "go"},
		{ID: "c/imported.go", Kind: graph.KindFile, Name: "imported.go", FilePath: "c/imported.go", Language: "go"},
		{ID: "c/imported.go::Other", Kind: graph.KindFunction, Name: "Other", FilePath: "c/imported.go", Language: "go"},
	}, []*graph.Edge{
		{From: "a/caller.go", To: "c/imported.go", Kind: graph.EdgeImports, FilePath: "a/caller.go", Line: 1},
		{From: "a/caller.go::Run", To: "unresolved::Secret", Kind: graph.EdgeCalls, FilePath: "a/caller.go", Line: 5},
		{From: "unresolved::Secret", To: "a/caller.go::Run", Kind: graph.EdgeArgOf, FilePath: "a/caller.go", Line: 5},
	})

	r := New(store)
	_, err := r.ResolveAllContext(context.Background())
	require.ErrorIs(t, err, ErrReindexWriteRefused)
	assert.Equal(t, 1, store.refused)
	require.True(t, r.hasPendingGuardReverts())

	// Simulate an LSP upgrade promoting the edge before retry runs.
	store.refuseFunc = nil
	upgraded := store.GetOutEdges("a/caller.go::Run")[0]
	upgraded.Origin = graph.OriginLSPResolved
	store.ReindexEdges([]graph.EdgeReindex{{Edge: upgraded, OldTo: upgraded.To}})

	_, err = r.ResolveAllContext(context.Background())
	require.NoError(t, err)
	calls := store.GetOutEdges("a/caller.go::Run")
	require.Len(t, calls, 1)
	assert.Equal(t, "b/unimported.go::Secret", calls[0].To, "queued weak-tier guard revert must not clobber OriginLSPResolved")
	assert.Equal(t, graph.OriginLSPResolved, calls[0].Origin)
	assert.Len(t, store.GetOutEdges("b/unimported.go::Secret"), 1, "stale SourceReverts must not run when edge was not reverted")
}

func TestCrossRepoMutationFrontierFlushesCarriedRepointsWhenPendingEmpty(t *testing.T) {
	store := openRefusingReindexStore(t)
	store.refuseFunc = refuseOnceMatching(batchHasPlaceholderMove("a/unresolved::helper", "b/target.ts::helper"))
	store.AddBatch([]*graph.Node{
		{ID: "a/caller.ts", Kind: graph.KindFile, Name: "caller.ts", FilePath: "a/caller.ts", RepoPrefix: "a", Language: "typescript"},
		{ID: "a/caller.ts::run", Kind: graph.KindFunction, Name: "run", FilePath: "a/caller.ts", RepoPrefix: "a", Language: "typescript"},
		{ID: "b/target.ts", Kind: graph.KindFile, Name: "target.ts", FilePath: "b/target.ts", RepoPrefix: "b", Language: "typescript"},
		{ID: "b/target.ts::helper", Kind: graph.KindFunction, Name: "helper", FilePath: "b/target.ts", RepoPrefix: "b", Language: "typescript"},
	}, []*graph.Edge{
		{From: "a/caller.ts", To: "b/target.ts", Kind: graph.EdgeImports, FilePath: "a/caller.ts", Line: 1, CrossRepo: true},
		{From: "a/caller.ts::run", To: "unresolved::helper", Kind: graph.EdgeCalls, FilePath: "a/caller.ts", Line: 2},
		{From: "a/unresolved::helper", To: "a/caller.ts::run", Kind: graph.EdgeArgOf, FilePath: "a/caller.ts", Line: 2},
	})

	cr := NewCrossRepo(store)
	stats := cr.ResolveMutationFrontiers([]string{"a/caller.ts"}, []string{"a/caller.ts"}, []string{"a/caller.ts"})
	assert.True(t, stats.WriteRefused)
	assert.Equal(t, 1, store.refused)
	assert.Len(t, store.GetOutEdges("a/unresolved::helper"), 1)

	store.refuseFunc = nil
	stats = cr.ResolveMutationFrontiers([]string{"a/caller.ts"}, []string{"a/caller.ts"}, []string{"a/caller.ts"})
	assert.False(t, stats.WriteRefused)
	assert.Empty(t, store.GetOutEdges("a/unresolved::helper"), "scoped cross-repo retry with len(pending)==0 must flush carried placeholder repoints")
	var sawMoved bool
	for _, e := range store.GetOutEdges("b/target.ts::helper") {
		if e.Kind == graph.EdgeArgOf && e.To == "a/caller.ts::run" {
			sawMoved = true
		}
	}
	assert.True(t, sawMoved)
}

func TestIncrementalGuardRevertRetryBindsShadowedLocalBeforeGoBuiltins(t *testing.T) {
	store := openRefusingReindexStore(t)
	store.refuseFunc = refuseOnceMatching(batchHasEdge(func(ri graph.EdgeReindex) bool {
		return ri.Edge != nil && ri.Edge.From == "a/shadow.go::F" && ri.OldTo == "b/pkg.go::min"
	}))
	store.AddBatch([]*graph.Node{
		{ID: "a/shadow.go", Kind: graph.KindFile, Name: "shadow.go", FilePath: "a/shadow.go", Language: "go"},
		{ID: "a/shadow.go::F", Kind: graph.KindFunction, Name: "F", FilePath: "a/shadow.go", Language: "go", StartLine: 1},
		{ID: "a/shadow.go::F#local:min", Kind: graph.KindLocal, Name: "min", FilePath: "a/shadow.go", Language: "go", StartLine: 2},
		{ID: "b/pkg.go", Kind: graph.KindFile, Name: "pkg.go", FilePath: "b/pkg.go", Language: "go"},
		{ID: "b/pkg.go::min", Kind: graph.KindFunction, Name: "min", FilePath: "b/pkg.go", Language: "go"},
		{ID: "c/imp.go", Kind: graph.KindFile, Name: "imp.go", FilePath: "c/imp.go", Language: "go"},
		{ID: "c/imp.go::Other", Kind: graph.KindFunction, Name: "Other", FilePath: "c/imp.go", Language: "go"},
	}, []*graph.Edge{
		{From: "a/shadow.go", To: "c/imp.go", Kind: graph.EdgeImports, FilePath: "a/shadow.go", Line: 1},
		{From: "a/shadow.go::F", To: "unresolved::min", Kind: graph.EdgeReads, FilePath: "a/shadow.go", Line: 5},
		{From: "a/shadow.go::F", To: "unresolved::Other", Kind: graph.EdgeCalls, FilePath: "a/shadow.go", Line: 6},
	})

	r := New(store)
	stats := r.ResolveFilesAndIncoming([]string{"a/shadow.go"})
	assert.True(t, stats.WriteRefused)
	assert.Equal(t, 1, store.refused)

	store.refuseFunc = nil
	stats = r.ResolveFilesAndIncoming([]string{"a/shadow.go"})
	assert.False(t, stats.WriteRefused)
	var minTarget string
	for _, e := range store.GetOutEdges("a/shadow.go::F") {
		if e.Line == 5 {
			minTarget = e.To
		}
	}
	assert.Equal(t, "a/shadow.go::F#local:min", minTarget, "flushing guard reverts before frontier snapshot must let bare-name bind shadow builtin::go::min")
}

func TestPreGuardChunk2RefusalDrainsGuardSpoolSoChunk1WeakCallIsGuardedOnRetry(t *testing.T) {
	t.Setenv("GORTEX_RESOLVE_CHUNK", "1")
	t.Setenv("GORTEX_RESOLVE_CHUNK_SIZE", "1")

	store := openRefusingReindexStore(t)
	store.refuseFunc = refuseOnceMatching(batchHasCallTo("c/imported.go::Allowed"))
	store.AddBatch([]*graph.Node{
		{ID: "a/caller.go", Kind: graph.KindFile, Name: "caller.go", FilePath: "a/caller.go", Language: "go"},
		{ID: "a/caller.go::Run", Kind: graph.KindFunction, Name: "Run", FilePath: "a/caller.go", Language: "go"},
		{ID: "b/unimported.go", Kind: graph.KindFile, Name: "unimported.go", FilePath: "b/unimported.go", Language: "go"},
		{ID: "b/unimported.go::Secret", Kind: graph.KindFunction, Name: "Secret", FilePath: "b/unimported.go", Language: "go"},
		{ID: "c/imported.go", Kind: graph.KindFile, Name: "imported.go", FilePath: "c/imported.go", Language: "go"},
		{ID: "c/imported.go::Allowed", Kind: graph.KindFunction, Name: "Allowed", FilePath: "c/imported.go", Language: "go"},
	}, []*graph.Edge{
		{From: "a/caller.go", To: "c/imported.go", Kind: graph.EdgeImports, FilePath: "a/caller.go", Line: 1},
		{From: "a/caller.go::Run", To: "unresolved::Secret", Kind: graph.EdgeCalls, FilePath: "a/caller.go", Line: 5},
		{From: "a/caller.go::Run", To: "unresolved::Allowed", Kind: graph.EdgeCalls, FilePath: "a/caller.go", Line: 6},
	})

	r := New(store)
	_, err := r.ResolveAllContext(context.Background())
	require.ErrorIs(t, err, ErrReindexWriteRefused)
	assert.Equal(t, 1, store.refused)
	assert.Equal(t, 2, store.calls, "draining guard spool on refusal must not fire additional writes into contended gate")

	store.refuseFunc = nil
	r.chunkYieldHook = nil
	_, err = r.ResolveAllContext(context.Background())
	require.NoError(t, err)
	var secretEdge, allowedEdge *graph.Edge
	for _, e := range store.GetOutEdges("a/caller.go::Run") {
		if e.Line == 5 {
			secretEdge = e
		}
		if e.Line == 6 {
			allowedEdge = e
		}
	}
	require.NotNil(t, secretEdge)
	require.NotNil(t, allowedEdge)
	assert.Equal(t, "unresolved::Secret", secretEdge.To, "chunk-1 weak-tier call must still be guarded after chunk-2 refusal")
	assert.Equal(t, true, secretEdge.Meta["guard_reverted"])
	assert.Equal(t, "c/imported.go::Allowed", allowedEdge.To)
}

func TestResolveAllRetryWithZeroPendingStillCompletesAbortedTailPasses(t *testing.T) {
	store := openRefusingReindexStore(t)
	store.refuseFunc = refuseOnceMatching(batchHasImportFrom("app.py"))
	store.AddBatch([]*graph.Node{
		{ID: "app.py", Kind: graph.KindFile, Name: "app.py", FilePath: "app.py", Language: "python"},
		{ID: "app.py::run", Kind: graph.KindFunction, Name: "run", FilePath: "app.py", Language: "python"},
		{ID: "util.py", Kind: graph.KindFile, Name: "util.py", FilePath: "util.py", Language: "python"},
		{ID: "util.py::helper", Kind: graph.KindFunction, Name: "helper", FilePath: "util.py", Language: "python"},
	}, []*graph.Edge{
		{From: "app.py", To: "util.py", Kind: graph.EdgeImports, FilePath: "app.py", Line: 1},
		{From: "app.py", To: "external::requests", Kind: graph.EdgeImports, FilePath: "app.py", Line: 2},
		{From: "app.py::run", To: "unresolved::helper", Kind: graph.EdgeCalls, FilePath: "app.py", Line: 4},
	})

	r := New(store)
	_, err := r.ResolveAllContext(context.Background())
	require.ErrorIs(t, err, ErrReindexWriteRefused)
	assert.Equal(t, 1, store.refused)

	store.refuseFunc = nil
	r.chunkYieldHook = nil
	_, err = r.ResolveAllContext(context.Background())
	require.NoError(t, err)
	var attributedTo string
	for _, e := range store.GetOutEdges("app.py") {
		if e.Kind == graph.EdgeImports && e.Line == 2 {
			attributedTo = e.To
		}
	}
	assert.NotEqual(t, "external::requests", attributedTo, "retry with zero pending unresolved edges must still complete aborted non-unresolved tail passes")
	assert.NotEmpty(t, attributedTo)
}

func TestRestubCallsForChangedNpmImportsChunksWithoutOrphaningParallels(t *testing.T) {
	store := openRefusingReindexStore(t)
	store.refuseFunc = refuseOnceMatching(func(batch []graph.EdgeReindex) bool {
		return len(batch) == 2 && batchHasEdge(func(ri graph.EdgeReindex) bool {
			return ri.Edge != nil && ri.Edge.Kind == graph.EdgeCalls && ri.OldTo == "oldlib/src/index.ts::f"
		})(batch)
	})
	const totalCalls = 2050
	nodes := make([]*graph.Node, 0, totalCalls+5)
	edges := make([]*graph.Edge, 0, totalCalls*2+1)
	nodes = append(nodes,
		&graph.Node{ID: "app/src/app.ts", Kind: graph.KindFile, FilePath: "app/src/app.ts", RepoPrefix: "app", Language: "typescript"},
		&graph.Node{ID: "oldlib/src/index.ts", Kind: graph.KindFile, FilePath: "oldlib/src/index.ts", RepoPrefix: "oldlib", Language: "typescript"},
		&graph.Node{ID: "oldlib/src/index.ts::f", Kind: graph.KindFunction, Name: "f", FilePath: "oldlib/src/index.ts", RepoPrefix: "oldlib", Language: "typescript", Meta: map[string]any{"visibility": "public"}},
		&graph.Node{ID: "newlib/src/index.ts", Kind: graph.KindFile, FilePath: "newlib/src/index.ts", RepoPrefix: "newlib", Language: "typescript"},
		&graph.Node{ID: "newlib/src/index.ts::f", Kind: graph.KindFunction, Name: "f", FilePath: "newlib/src/index.ts", RepoPrefix: "newlib", Language: "typescript", Meta: map[string]any{"visibility": "public"}},
	)
	edges = append(edges, &graph.Edge{
		From: "app/src/app.ts", To: "oldlib/src/index.ts::f",
		Kind: graph.EdgeImports, FilePath: "app/src/app.ts", Line: 1, CrossRepo: true,
		Meta: map[string]any{NpmImportSpecifierMeta: "p::f"},
	})
	for i := 0; i < totalCalls; i++ {
		fnID := fmt.Sprintf("app/src/app.ts::run%d", i)
		nodes = append(nodes, &graph.Node{
			ID: fnID, Kind: graph.KindFunction, Name: fmt.Sprintf("run%d", i),
			FilePath: "app/src/app.ts", RepoPrefix: "app", Language: "typescript",
		})
		edges = append(edges,
			&graph.Edge{From: fnID, To: "oldlib/src/index.ts::f", Kind: graph.EdgeCalls, FilePath: "app/src/app.ts", Line: i + 2, CrossRepo: true},
			&graph.Edge{From: fnID, To: "oldlib/src/index.ts::f", Kind: graph.EdgeCrossRepoCalls, FilePath: "app/src/app.ts", Line: i + 2, CrossRepo: true},
		)
	}
	store.AddBatch(nodes, edges)

	cr := NewCrossRepo(store)
	cr.SetNpmDependencyLookup(func(_, spec string) bool { return spec == "p" })
	cr.SetNpmPackageLookup(func(spec string) map[string]NpmPackageTarget {
		if spec != "p" {
			return nil
		}
		return map[string]NpmPackageTarget{"newlib": {Entries: []string{"newlib/src/index.ts", "newlib"}}}
	})

	_, err := cr.ResolveAllContext(context.Background())
	require.ErrorIs(t, err, ErrReindexWriteRefused)
	assert.Equal(t, 1, store.refused)
	// Chunk 1's 2048 parallels were removed immediately; only chunk 2's 2 calls + 2 parallels remain on oldlib.
	var oldCalls, oldParallels int
	for _, e := range store.GetInEdges("oldlib/src/index.ts::f") {
		switch e.Kind {
		case graph.EdgeCalls:
			oldCalls++
		case graph.EdgeCrossRepoCalls:
			oldParallels++
		}
	}
	assert.Equal(t, 2, oldCalls)
	assert.Equal(t, 2, oldParallels, "chunked restubbing must not orphan chunk-1 parallels when chunk 2 is refused")

	store.refuseFunc = nil
	_, err = cr.ResolveAllContext(context.Background())
	require.NoError(t, err)
	assert.Empty(t, store.GetInEdges("oldlib/src/index.ts::f"), "retry must drain all remaining oldlib calls and parallels")
}

func TestResolveAllContextReleasesNonInFlightRetryStateAcrossChunkYield(t *testing.T) {
	t.Setenv("GORTEX_RESOLVE_CHUNK", "1")
	t.Setenv("GORTEX_RESOLVE_CHUNK_SIZE", "1")

	store := openRefusingReindexStore(t)
	store.AddBatch([]*graph.Node{
		{ID: "repoA/ext.go", Kind: graph.KindFile, Name: "ext.go", FilePath: "repoA/ext.go", RepoPrefix: "repoA", Language: "go"},
		{ID: "repoA/ext.go::Run", Kind: graph.KindFunction, Name: "Run", FilePath: "repoA/ext.go", RepoPrefix: "repoA", Language: "go"},
		{ID: "repoB/caller.go", Kind: graph.KindFile, Name: "caller.go", FilePath: "repoB/caller.go", RepoPrefix: "repoB", Language: "go"},
		{ID: "repoB/caller.go::Main", Kind: graph.KindFunction, Name: "Main", FilePath: "repoB/caller.go", RepoPrefix: "repoB", Language: "go"},
		{ID: "repoB/callee.go", Kind: graph.KindFile, Name: "callee.go", FilePath: "repoB/callee.go", RepoPrefix: "repoB", Language: "go"},
		{ID: "repoB/callee.go::One", Kind: graph.KindFunction, Name: "One", FilePath: "repoB/callee.go", RepoPrefix: "repoB", Language: "go"},
		{ID: "repoB/callee.go::Two", Kind: graph.KindFunction, Name: "Two", FilePath: "repoB/callee.go", RepoPrefix: "repoB", Language: "go"},
	}, []*graph.Edge{
		{From: "repoA/ext.go::Run", To: "stdlib::fmt::Println", Kind: graph.EdgeCalls, FilePath: "repoA/ext.go", Line: 5},
		{From: "repoB/caller.go", To: "repoB/callee.go", Kind: graph.EdgeImports, FilePath: "repoB/caller.go", Line: 1},
		{From: "repoB/caller.go::Main", To: "unresolved::One", Kind: graph.EdgeCalls, FilePath: "repoB/caller.go", Line: 5},
		{From: "repoB/caller.go::Main", To: "unresolved::Two", Kind: graph.EdgeCalls, FilePath: "repoB/caller.go", Line: 6},
	})

	var bridgeMu sync.Mutex
	bridge := MasterRetryState{AttributionFiles: []string{"repoA/ext.go"}}
	take := func(opts RetryTakeOpts) (MasterRetryState, uint64) {
		bridgeMu.Lock()
		defer bridgeMu.Unlock()
		var out MasterRetryState
		if opts.TakeTailAll {
			out.TailRetryAll = bridge.TailRetryAll
			bridge.TailRetryAll = false
		} else if opts.PeekTailAll {
			out.TailRetryAll = bridge.TailRetryAll
		}
		if opts.AllRepos {
			out.PlaceholderRepoints = bridge.PlaceholderRepoints
			out.GuardReverts = bridge.GuardReverts
			out.TailRetryScopes = bridge.TailRetryScopes
			out.AttributionFiles = bridge.AttributionFiles
			bridge.PlaceholderRepoints = nil
			bridge.GuardReverts = nil
			bridge.TailRetryScopes = nil
			bridge.AttributionFiles = nil
			return out, 1
		}
		var keepAttrib []string
		for _, fp := range bridge.AttributionFiles {
			if _, ok := opts.Repos[graph.RepoPrefixOfID(fp)]; ok {
				out.AttributionFiles = append(out.AttributionFiles, fp)
			} else {
				keepAttrib = append(keepAttrib, fp)
			}
		}
		bridge.AttributionFiles = keepAttrib
		return out, 1
	}
	save := func(s MasterRetryState, _ RetryBridgeSaveMeta) {
		bridgeMu.Lock()
		defer bridgeMu.Unlock()
		bridge.PlaceholderRepoints = append(bridge.PlaceholderRepoints, s.PlaceholderRepoints...)
		bridge.GuardReverts = append(bridge.GuardReverts, s.GuardReverts...)
		bridge.TailRetryAll = bridge.TailRetryAll || s.TailRetryAll
		bridge.TailRetryScopes = append(bridge.TailRetryScopes, s.TailRetryScopes...)
		bridge.AttributionFiles = append(bridge.AttributionFiles, s.AttributionFiles...)
	}

	m1 := New(store)
	m1.SetScope(map[string]struct{}{"repoB": {}})
	m1.SetRetryStateBridge(take, save, nil, nil)
	interleaved := false
	m1.chunkYieldHook = func() {
		if interleaved {
			return
		}
		interleaved = true
		m2 := New(store)
		m2.SetRetryStateBridge(take, save, nil, nil)
		stats := m2.ResolveFilesAndIncoming([]string{"repoA/ext.go"})
		assert.False(t, stats.WriteRefused)
		// repoA/ext.go had zero unresolved::* edges, so it must have seen its
		// bridged pendingAttribFiles during m1's inter-chunk yield.
		assert.NotNil(t, store.GetNode("stdlib::fmt::Println"), "interleaving repoA retry must observe bridged pendingAttribFiles during repoB chunk yield")
	}

	_, err := m1.ResolveAllContext(context.Background())
	require.NoError(t, err)
	assert.True(t, interleaved)
	assert.Empty(t, bridge.AttributionFiles, "completed repoA attribution must not be spuriously re-queued after repoB finishes")
}

func TestCrossRepoResolveAllFlushesInheritedRepointsBeforeChunking(t *testing.T) {
	t.Setenv("GORTEX_RESOLVE_CHUNK", "1")
	t.Setenv("GORTEX_RESOLVE_CHUNK_SIZE", "1")

	store := openRefusingReindexStore(t)
	store.AddBatch([]*graph.Node{
		{ID: "a/caller.ts", Kind: graph.KindFile, Name: "caller.ts", FilePath: "a/caller.ts", RepoPrefix: "a", WorkspaceID: "ws", Language: "typescript"},
		{ID: "a/caller.ts::run", Kind: graph.KindFunction, Name: "run", FilePath: "a/caller.ts", RepoPrefix: "a", WorkspaceID: "ws", Language: "typescript"},
		{ID: "b/caller.ts", Kind: graph.KindFile, Name: "caller.ts", FilePath: "b/caller.ts", RepoPrefix: "b", WorkspaceID: "ws", Language: "typescript"},
		{ID: "b/caller.ts::use", Kind: graph.KindFunction, Name: "use", FilePath: "b/caller.ts", RepoPrefix: "b", WorkspaceID: "ws", Language: "typescript"},
		{ID: "c/target.ts", Kind: graph.KindFile, Name: "target.ts", FilePath: "c/target.ts", RepoPrefix: "c", WorkspaceID: "ws", Language: "typescript"},
		{ID: "c/target.ts::one", Kind: graph.KindFunction, Name: "one", FilePath: "c/target.ts", RepoPrefix: "c", WorkspaceID: "ws", Language: "typescript"},
		{ID: "c/target.ts::two", Kind: graph.KindFunction, Name: "two", FilePath: "c/target.ts", RepoPrefix: "c", WorkspaceID: "ws", Language: "typescript"},
		{ID: "c/target.ts::helper", Kind: graph.KindFunction, Name: "helper", FilePath: "c/target.ts", RepoPrefix: "c", WorkspaceID: "ws", Language: "typescript"},
	}, []*graph.Edge{
		{From: "a/caller.ts", To: "c/target.ts", Kind: graph.EdgeImports, FilePath: "a/caller.ts", Line: 1, CrossRepo: true},
		{From: "a/caller.ts::run", To: "unresolved::one", Kind: graph.EdgeCalls, FilePath: "a/caller.ts", Line: 2},
		{From: "a/caller.ts::run", To: "unresolved::two", Kind: graph.EdgeCalls, FilePath: "a/caller.ts", Line: 3},
		{From: "b/caller.ts::use", To: "c/target.ts::helper", Kind: graph.EdgeCalls, FilePath: "b/caller.ts", Line: 2, CrossRepo: true},
		{From: "b/unresolved::helper", To: "b/caller.ts::use", Kind: graph.EdgeArgOf, FilePath: "b/caller.ts", Line: 2},
	})

	queued := []graph.PlaceholderRepoint{
		{OldFrom: "b/unresolved::helper", FilePath: "b/caller.ts", Line: 2, NewFrom: "c/target.ts::helper", RepoPrefix: "b"},
	}
	take := func(opts RetryTakeOpts) (CrossRepoRetryState, uint64) {
		if !opts.AllRepos && len(opts.Repos) == 0 {
			return CrossRepoRetryState{}, 1
		}
		out := queued
		queued = nil
		return CrossRepoRetryState{Repoints: out}, 1
	}
	save := func(state CrossRepoRetryState, _ RetryBridgeSaveMeta) {
		queued = append(queued, state.Repoints...)
	}

	// Refuse chunk 1 call reindex while allowing initial flush of inherited repoint.
	store.refuseFunc = refuseOnceMatching(batchHasEdge(func(ri graph.EdgeReindex) bool {
		return ri.Edge != nil && ri.Edge.Kind == graph.EdgeCalls && ri.Edge.From == "a/caller.ts::run"
	}))
	cr := NewCrossRepo(store)
	cr.SetRetryStateBridge(take, save, nil, nil)
	_, err := cr.ResolveAllContext(context.Background())
	require.ErrorIs(t, err, ErrReindexWriteRefused)
	assert.Equal(t, 1, store.refused)
	assert.Empty(t, store.GetOutEdges("b/unresolved::helper"), "inherited repoint must be flushed before streaming/chunking")
	assert.Len(t, store.GetOutEdges("c/target.ts::helper"), 1)
	assert.Empty(t, queued)
}

func TestScopedResolveAllPreservesOtherScopeAndUnscopedTailRetry(t *testing.T) {
	store := openRefusingReindexStore(t)
	store.AddBatch([]*graph.Node{
		{ID: "repoA/app.py", Kind: graph.KindFile, Name: "app.py", FilePath: "repoA/app.py", RepoPrefix: "repoA", Language: "python"},
		{ID: "repoA/ext.go", Kind: graph.KindFile, Name: "ext.go", FilePath: "repoA/ext.go", RepoPrefix: "repoA", Language: "go"},
		{ID: "repoA/ext.go::Run", Kind: graph.KindFunction, Name: "Run", FilePath: "repoA/ext.go", RepoPrefix: "repoA", Language: "go"},
		{ID: "repoB/ok.go", Kind: graph.KindFile, Name: "ok.go", FilePath: "repoB/ok.go", RepoPrefix: "repoB", Language: "go"},
	}, []*graph.Edge{
		{From: "repoA/app.py", To: "external::requests", Kind: graph.EdgeImports, FilePath: "repoA/app.py", Line: 1},
		{From: "repoA/ext.go::Run", To: "stdlib::fmt::Println", Kind: graph.EdgeCalls, FilePath: "repoA/ext.go", Line: 5},
	})

	bridge := MasterRetryState{
		TailRetryAll:     true,
		TailRetryScopes:  []string{"repoA"},
		AttributionFiles: []string{"repoA/ext.go"},
	}
	take := func(opts RetryTakeOpts) (MasterRetryState, uint64) {
		var out MasterRetryState
		if opts.TakeTailAll {
			out.TailRetryAll = bridge.TailRetryAll
			bridge.TailRetryAll = false
		} else if opts.PeekTailAll {
			out.TailRetryAll = bridge.TailRetryAll
		}
		if opts.AllRepos {
			out.PlaceholderRepoints = bridge.PlaceholderRepoints
			out.GuardReverts = bridge.GuardReverts
			out.TailRetryScopes = bridge.TailRetryScopes
			out.AttributionFiles = bridge.AttributionFiles
			bridge.PlaceholderRepoints = nil
			bridge.GuardReverts = nil
			bridge.TailRetryScopes = nil
			bridge.AttributionFiles = nil
			return out, 1
		}
		var keepScopes []string
		for _, sc := range bridge.TailRetryScopes {
			if _, ok := opts.Repos[sc]; ok {
				out.TailRetryScopes = append(out.TailRetryScopes, sc)
			} else {
				keepScopes = append(keepScopes, sc)
			}
		}
		bridge.TailRetryScopes = keepScopes
		var keepAttrib []string
		for _, fp := range bridge.AttributionFiles {
			if _, ok := opts.Repos[graph.RepoPrefixOfID(fp)]; ok {
				out.AttributionFiles = append(out.AttributionFiles, fp)
			} else {
				keepAttrib = append(keepAttrib, fp)
			}
		}
		bridge.AttributionFiles = keepAttrib
		return out, 1
	}
	save := func(s MasterRetryState, _ RetryBridgeSaveMeta) {
		bridge.PlaceholderRepoints = append(bridge.PlaceholderRepoints, s.PlaceholderRepoints...)
		bridge.GuardReverts = append(bridge.GuardReverts, s.GuardReverts...)
		bridge.TailRetryAll = bridge.TailRetryAll || s.TailRetryAll
		bridge.TailRetryScopes = append(bridge.TailRetryScopes, s.TailRetryScopes...)
		bridge.AttributionFiles = append(bridge.AttributionFiles, s.AttributionFiles...)
	}

	// repoB-scoped ResolveAllContext must NOT clear unscoped TailRetryAll or repoA state.
	rB := New(store)
	rB.SetScope(map[string]struct{}{"repoB": {}})
	rB.SetRetryStateBridge(take, save, nil, nil)
	_, err := rB.ResolveAllContext(context.Background())
	require.NoError(t, err)
	assert.Nil(t, store.GetNode("stdlib::fmt::Println"), "repoB-scoped pass must skip repoA go_attribution")
	assert.True(t, bridge.TailRetryAll, "scoped pass must not clear unscoped TailRetryAll")
	assert.Equal(t, []string{"repoA"}, bridge.TailRetryScopes, "repoB pass must not clear repoA TailRetryScopes")
	assert.Equal(t, []string{"repoA/ext.go"}, bridge.AttributionFiles)

	// repoA-scoped ResolveAllContext with zero pending must still run repoA go_attribution,
	// clear repoA's scope/files, and preserve unscoped TailRetryAll.
	rA := New(store)
	rA.SetScope(map[string]struct{}{"repoA": {}})
	rA.SetRetryStateBridge(take, save, nil, nil)
	_, err = rA.ResolveAllContext(context.Background())
	require.NoError(t, err)
	assert.NotNil(t, store.GetNode("stdlib::fmt::Println"), "repoA-scoped retry must complete repoA go_attribution")
	assert.True(t, bridge.TailRetryAll, "repoA-scoped pass must still preserve unscoped TailRetryAll")
	assert.Empty(t, bridge.TailRetryScopes)
	assert.Empty(t, bridge.AttributionFiles)

	// Unscoped ResolveAllContext with zero pending edges must clear TailRetryAll.
	rAll := New(store)
	rAll.SetStampTerminal(true)
	rAll.SetRetryStateBridge(take, save, nil, nil)
	_, err = rAll.ResolveAllContext(context.Background())
	require.NoError(t, err)
	assert.False(t, bridge.TailRetryAll)
	assert.Empty(t, bridge.TailRetryScopes)
	assert.Empty(t, bridge.AttributionFiles)
}

func TestResolveAllWithZeroPendingDrainsPendingAttribFiles(t *testing.T) {
	store := openRefusingReindexStore(t)
	store.AddBatch([]*graph.Node{
		{ID: "a/ext.go", Kind: graph.KindFile, Name: "ext.go", FilePath: "a/ext.go", Language: "go"},
		{ID: "a/ext.go::Run", Kind: graph.KindFunction, Name: "Run", FilePath: "a/ext.go", Language: "go"},
	}, []*graph.Edge{
		{From: "a/ext.go::Run", To: "stdlib::fmt::Println", Kind: graph.EdgeCalls, FilePath: "a/ext.go", Line: 5},
	})

	r := New(store)
	r.notePendingAttributionFilesLocked([]string{"a/ext.go"})

	_, err := r.ResolveAllContext(context.Background())
	require.NoError(t, err)
	assert.NotNil(t, store.GetNode("stdlib::fmt::Println"), "ResolveAllContext must not skip go_attribution when pendingAttribFiles is non-empty")
	assert.Empty(t, r.pendingAttribFiles)
}

func TestPreAttributionIncrementalWriteRefusedNotesPendingAttribFiles(t *testing.T) {
	for _, mode := range []string{"files_and_incoming", "files_outgoing"} {
		t.Run(mode, func(t *testing.T) {
			store := openRefusingReindexStore(t)
			store.refuseFunc = refuseOnceMatching(batchHasPlaceholderMove("unresolved::Helper", "a/foo.go::Helper"))
			store.AddBatch([]*graph.Node{
				{ID: "a/foo.go", Kind: graph.KindFile, Name: "foo.go", FilePath: "a/foo.go", Language: "go"},
				{ID: "a/foo.go::Run", Kind: graph.KindFunction, Name: "Run", FilePath: "a/foo.go", Language: "go"},
				{ID: "a/foo.go::Helper", Kind: graph.KindFunction, Name: "Helper", FilePath: "a/foo.go", Language: "go"},
			}, []*graph.Edge{
				{From: "a/foo.go::Run", To: "unresolved::Helper", Kind: graph.EdgeCalls, FilePath: "a/foo.go", Line: 5},
				{From: "unresolved::Helper", To: "a/foo.go::Run", Kind: graph.EdgeArgOf, FilePath: "a/foo.go", Line: 5},
				{From: "a/foo.go::Run", To: "unresolved::extern::fmt::Println", Kind: graph.EdgeCalls, FilePath: "a/foo.go", Line: 6},
			})

			r := New(store)
			var stats *ResolveStats
			if mode == "files_and_incoming" {
				stats = r.ResolveFilesAndIncoming([]string{"a/foo.go"})
			} else {
				stats = r.ResolveFilesOutgoing([]string{"a/foo.go"})
			}
			assert.True(t, stats.WriteRefused)
			assert.Equal(t, 1, store.refused)
			assert.Nil(t, store.GetNode("stdlib::fmt::Println"), "attribution must not run on refused pass")

			store.refuseFunc = nil
			if mode == "files_and_incoming" {
				stats = r.ResolveFilesAndIncoming([]string{"a/foo.go"})
			} else {
				stats = r.ResolveFilesOutgoing([]string{"a/foo.go"})
			}
			assert.False(t, stats.WriteRefused)
			assert.Empty(t, store.GetOutEdges("unresolved::Helper"), "retry must apply retained placeholder repoint")
			assert.NotNil(t, store.GetNode("stdlib::fmt::Println"), "retry with zero unresolved::* edges must still run per-file attribution")
		})
	}
}

func TestChunkedGuardRevertPreservesChunk1SourceRevertsWhenChunk2Refused(t *testing.T) {
	setup := func(t *testing.T) (*refusingReindexStore, *Resolver, []reindexJob, map[string]map[string]struct{}, int) {
		t.Helper()
		store := openRefusingReindexStore(t)

		const totalCalls = guardRevertChunkSize + 2
		nodes := []*graph.Node{
			{ID: "a/caller.go", Kind: graph.KindFile, Name: "caller.go", FilePath: "a/caller.go", Language: "go"},
			{ID: "a/caller.go::Run", Kind: graph.KindFunction, Name: "Run", FilePath: "a/caller.go", Language: "go"},
			{ID: "b/unimported.go", Kind: graph.KindFile, Name: "unimported.go", FilePath: "b/unimported.go", Language: "go"},
			{ID: "b/unimported.go::Secret", Kind: graph.KindFunction, Name: "Secret", FilePath: "b/unimported.go", Language: "go"},
			{ID: "c/imported.go", Kind: graph.KindFile, Name: "imported.go", FilePath: "c/imported.go", Language: "go"},
			{ID: "other/noop.go", Kind: graph.KindFile, Name: "noop.go", FilePath: "other/noop.go", Language: "go"},
		}
		edges := make([]*graph.Edge, 0, 1+totalCalls*2)
		edges = append(edges, &graph.Edge{From: "a/caller.go", To: "c/imported.go", Kind: graph.EdgeImports, FilePath: "a/caller.go", Line: 1})
		for i := 0; i < totalCalls; i++ {
			line := 10 + i
			edges = append(edges,
				&graph.Edge{From: "a/caller.go::Run", To: "b/unimported.go::Secret", Kind: graph.EdgeCalls, FilePath: "a/caller.go", Line: line, Confidence: 0.5, Origin: graph.OriginTextMatched},
				&graph.Edge{From: "b/unimported.go::Secret", To: "a/caller.go::Run", Kind: graph.EdgeArgOf, FilePath: "a/caller.go", Line: line},
			)
		}
		store.AddBatch(nodes, edges)

		r := New(store)
		var jobs []reindexJob
		for _, e := range store.GetOutEdges("a/caller.go::Run") {
			if e.Kind == graph.EdgeCalls {
				jobs = append(jobs, reindexJob{
					edge:       e,
					oldTo:      "unresolved::Secret",
					newTo:      e.To,
					kind:       e.Kind,
					confidence: e.Confidence,
					origin:     e.Origin,
				})
			}
		}
		require.Len(t, jobs, totalCalls)
		return store, r, jobs, r.buildImportClosure(), totalCalls
	}

	t.Run("guard_pass_chunk2_refused_flushed_via_unrelated_file", func(t *testing.T) {
		store, r, jobs, closure, totalCalls := setup(t)
		// Refuse chunk 2's 2-edge guard revert batch by content and size.
		store.refuseFunc = refuseOnceMatching(func(batch []graph.EdgeReindex) bool {
			return len(batch) == 2 && batchHasGuardRevert("a/caller.go::Run")(batch)
		})
		var refused bool
		restore := r.trackPassWriteRefused(&refused)
		reverted := r.guardCrossPackageCallEdges(jobs, closure)
		restore()
		assert.True(t, refused)
		assert.Equal(t, 1, store.refused)
		assert.Equal(t, guardRevertChunkSize, reverted)
		assert.Len(t, store.GetOutEdges("b/unimported.go::Secret"), 2)
		assert.Len(t, store.GetOutEdges("unresolved::Secret"), guardRevertChunkSize)

		// Flush via an unrelated file so a/caller.go is NEVER re-resolved or re-guarded:
		// ONLY Step 3 bounded guard-revert drain reverts Chunk 2's 2 calls and 2 SourceReverts.
		store.refuseFunc = nil
		stats := r.ResolveFilesAndIncoming([]string{"other/noop.go"})
		assert.False(t, stats.WriteRefused)
		assert.Empty(t, store.GetOutEdges("b/unimported.go::Secret"), "Step 3 bounded flush must revert chunk 2's remaining 2 SourceReverts")
		assert.Len(t, store.GetOutEdges("unresolved::Secret"), totalCalls)
		for _, e := range store.GetOutEdges("a/caller.go::Run") {
			assert.Equal(t, "unresolved::Secret", e.To)
		}
	})

	t.Run("flush_pending_multi_chunk_refuses_chunk2_then_completes_via_resolve_all", func(t *testing.T) {
		store, r, jobs, closure, totalCalls := setup(t)
		// Refuse chunk 1 in guardCrossPackageCallEdges so all 2050 items (> guardRevertChunkSize)
		// are queued in r.pendingGuardReverts.
		store.refuseFunc = refuseOnceMatching(func(batch []graph.EdgeReindex) bool {
			return len(batch) == guardRevertChunkSize && batchHasGuardRevert("a/caller.go::Run")(batch)
		})
		var refused bool
		restore := r.trackPassWriteRefused(&refused)
		reverted := r.guardCrossPackageCallEdges(jobs, closure)
		restore()
		assert.True(t, refused)
		assert.Equal(t, 1, store.refused)
		assert.Equal(t, 0, reverted)
		assert.Len(t, store.GetOutEdges("b/unimported.go::Secret"), totalCalls)

		// Flush the 2050-item full-pass guard queue via ResolveAllContext:
		// allow chunk 1 reindex + chunk 1 SourceReverts, refuse chunk 2 reindex by content.
		store.refused = 0
		store.refuseFunc = refuseOnceMatching(func(batch []graph.EdgeReindex) bool {
			return len(batch) == 2 && batchHasGuardRevert("a/caller.go::Run")(batch)
		})
		stats, err := r.ResolveAllContext(context.Background())
		require.ErrorIs(t, err, ErrReindexWriteRefused)
		assert.True(t, stats.WriteRefused)
		assert.Equal(t, 1, store.refused)
		assert.Len(t, store.GetOutEdges("b/unimported.go::Secret"), 2, "flushPendingGuardRevertsLocked must commit chunk 1's 2048 SourceReverts before chunk 2 refusal")
		assert.Len(t, store.GetOutEdges("unresolved::Secret"), guardRevertChunkSize)

		// Second ResolveAllContext completes the remaining 2 reindexes + 2 SourceReverts.
		store.refuseFunc = nil
		stats, err = r.ResolveAllContext(context.Background())
		require.NoError(t, err)
		assert.False(t, stats.WriteRefused)
		assert.Empty(t, store.GetOutEdges("b/unimported.go::Secret"))
		assert.Len(t, store.GetOutEdges("unresolved::Secret"), totalCalls)
	})

	t.Run("flush_pending_chunk1_source_revert_refused_retains_chunk1_sources_and_chunk2_reindexes", func(t *testing.T) {
		store, r, jobs, closure, totalCalls := setup(t)
		store.refuseFunc = refuseOnceMatching(func(batch []graph.EdgeReindex) bool {
			return len(batch) == guardRevertChunkSize && batchHasGuardRevert("a/caller.go::Run")(batch)
		})
		var refused bool
		restore := r.trackPassWriteRefused(&refused)
		_ = r.guardCrossPackageCallEdges(jobs, closure)
		restore()
		require.True(t, refused)
		require.Equal(t, 1, store.refused)

		// Refuse chunk 1's SourceReverts inside flushPendingGuardRevertsLocked during ResolveAllContext.
		store.refused = 0
		store.refuseFunc = refuseOnceMatching(batchHasPlaceholderMove("b/unimported.go::Secret", "unresolved::Secret"))
		stats, err := r.ResolveAllContext(context.Background())
		require.ErrorIs(t, err, ErrReindexWriteRefused)
		assert.True(t, stats.WriteRefused)
		assert.Equal(t, 1, store.refused)
		assert.Len(t, store.GetOutEdges("b/unimported.go::Secret"), totalCalls, "chunk 1 source reverts were refused and must remain queued")

		store.refuseFunc = nil
		stats, err = r.ResolveAllContext(context.Background())
		require.NoError(t, err)
		assert.False(t, stats.WriteRefused)
		assert.Empty(t, store.GetOutEdges("b/unimported.go::Secret"), "retry must drain chunk 1 reindexesApplied SourceReverts and chunk 2 reindexes+SourceReverts")
		assert.Len(t, store.GetOutEdges("unresolved::Secret"), totalCalls)
	})
}

func TestMidComputeRefusalPreservesGuardRevertedAgainstCrossRepoAndClearsOnResolve(t *testing.T) {
	t.Run("mid_compute_drain_blocks_cross_repo_same_repo_decoy", func(t *testing.T) {
		t.Setenv("GORTEX_RESOLVE_CHUNK", "1")
		t.Setenv("GORTEX_RESOLVE_CHUNK_SIZE", "1")

		store := openRefusingReindexStore(t)
		store.refuseFunc = refuseOnceMatching(batchHasCallTo("repoA/c/imported.go::Allowed"))
		store.AddBatch([]*graph.Node{
			{ID: "repoA/a/caller.go", Kind: graph.KindFile, Name: "caller.go", FilePath: "repoA/a/caller.go", RepoPrefix: "repoA", WorkspaceID: "ws", Language: "go"},
			{ID: "repoA/a/caller.go::Run", Kind: graph.KindFunction, Name: "Run", FilePath: "repoA/a/caller.go", RepoPrefix: "repoA", WorkspaceID: "ws", Language: "go"},
			{ID: "repoA/b/unimported.go", Kind: graph.KindFile, Name: "unimported.go", FilePath: "repoA/b/unimported.go", RepoPrefix: "repoA", WorkspaceID: "ws", Language: "go"},
			{ID: "repoA/b/unimported.go::Secret", Kind: graph.KindFunction, Name: "Secret", FilePath: "repoA/b/unimported.go", RepoPrefix: "repoA", WorkspaceID: "ws", Language: "go"},
			{ID: "repoA/c/imported.go", Kind: graph.KindFile, Name: "imported.go", FilePath: "repoA/c/imported.go", RepoPrefix: "repoA", WorkspaceID: "ws", Language: "go"},
			{ID: "repoA/c/imported.go::Allowed", Kind: graph.KindFunction, Name: "Allowed", FilePath: "repoA/c/imported.go", RepoPrefix: "repoA", WorkspaceID: "ws", Language: "go"},
			{ID: "repoA/noop.go", Kind: graph.KindFile, Name: "noop.go", FilePath: "repoA/noop.go", RepoPrefix: "repoA", WorkspaceID: "ws", Language: "go"},
			{ID: "repoB/noop.go", Kind: graph.KindFile, Name: "noop.go", FilePath: "repoB/noop.go", RepoPrefix: "repoB", WorkspaceID: "ws", Language: "go"},
		}, []*graph.Edge{
			{From: "repoA/a/caller.go", To: "repoA/c/imported.go", Kind: graph.EdgeImports, FilePath: "repoA/a/caller.go", Line: 1},
			{From: "repoA/a/caller.go::Run", To: "unresolved::Secret", Kind: graph.EdgeCalls, FilePath: "repoA/a/caller.go", Line: 5},
			{From: "repoA/a/caller.go::Run", To: "unresolved::Allowed", Kind: graph.EdgeCalls, FilePath: "repoA/a/caller.go", Line: 6},
		})

		r := New(store)
		_, err := r.ResolveAllContext(context.Background())
		require.ErrorIs(t, err, ErrReindexWriteRefused)
		assert.Equal(t, 1, store.refused)
		require.Equal(t, 1, r.PendingRetryStats().GuardReverts)

		// Sibling repoB must NOT touch repoA's pending guard revert.
		store.refuseFunc = nil
		stats := r.ResolveFilesAndIncoming([]string{"repoB/noop.go"})
		assert.False(t, stats.WriteRefused)
		assert.Equal(t, 1, r.PendingRetryStats().GuardReverts, "repoB incremental pass must not touch repoA guard reverts")

		// Same-repo repoA/noop.go drains repoA's guard revert; CrossRepoResolver must still respect guard_reverted.
		stats = r.ResolveFilesAndIncoming([]string{"repoA/noop.go"})
		assert.False(t, stats.WriteRefused)
		assert.Equal(t, 0, r.PendingRetryStats().GuardReverts)
		_, err = NewCrossRepo(store).ResolveAllContext(context.Background())
		require.NoError(t, err)

		var secretEdge *graph.Edge
		for _, e := range store.GetOutEdges("repoA/a/caller.go::Run") {
			if e.Line == 5 {
				secretEdge = e
			}
		}
		require.NotNil(t, secretEdge)
		assert.Equal(t, "unresolved::Secret", secretEdge.To, "mid-compute drain must keep guard_reverted so CrossRepoResolver never binds an unimported same-repo Go decoy")
		assert.Equal(t, true, secretEdge.Meta["guard_reverted"])
	})

	t.Run("mid_compute_drain_then_import_retry_clears_guard_reverted", func(t *testing.T) {
		t.Setenv("GORTEX_RESOLVE_CHUNK", "1")
		t.Setenv("GORTEX_RESOLVE_CHUNK_SIZE", "1")

		store := openRefusingReindexStore(t)
		store.refuseFunc = refuseOnceMatching(batchHasImportFrom("a/caller.go"))
		store.AddBatch([]*graph.Node{
			{ID: "a/caller.go", Kind: graph.KindFile, Name: "caller.go", FilePath: "a/caller.go", Language: "go"},
			{ID: "a/caller.go::Run", Kind: graph.KindFunction, Name: "Run", FilePath: "a/caller.go", Language: "go"},
			{ID: "b/target.go", Kind: graph.KindFile, Name: "target.go", FilePath: "b/target.go", Language: "go"},
			{ID: "b/target.go::Helper", Kind: graph.KindFunction, Name: "Helper", FilePath: "b/target.go", Language: "go"},
			{ID: "other/noop.go", Kind: graph.KindFile, Name: "noop.go", FilePath: "other/noop.go", Language: "go"},
		}, []*graph.Edge{
			{From: "a/caller.go::Run", To: "unresolved::Helper", Kind: graph.EdgeCalls, FilePath: "a/caller.go", Line: 5},
			{From: "a/caller.go", To: "unresolved::import::b", Kind: graph.EdgeImports, FilePath: "a/caller.go", Line: 1},
		})

		r := New(store)
		_, err := r.ResolveAllContext(context.Background())
		require.ErrorIs(t, err, ErrReindexWriteRefused)
		assert.Equal(t, 1, store.refused)

		store.refuseFunc = nil
		stats := r.ResolveFilesAndIncoming([]string{"other/noop.go"})
		assert.False(t, stats.WriteRefused)

		// Once retry resolves the deferred import and re-resolves the call,
		// guard_reverted must be cleared.
		_, err = r.ResolveAllContext(context.Background())
		require.NoError(t, err)
		imports := store.GetOutEdges("a/caller.go")
		require.Len(t, imports, 1)
		require.Equal(t, "b/target.go", imports[0].To, "chunk-2 import must resolve on retry")
		edges := store.GetOutEdges("a/caller.go::Run")
		require.Len(t, edges, 1)
		assert.Equal(t, "b/target.go::Helper", edges[0].To)
		assert.Nil(t, edges[0].Meta["guard_reverted"], "re-resolving after chunk-2 import lands must clear guard_reverted")
	})

	t.Run("resolving_edge_clears_stale_guard_reverted_stamp", func(t *testing.T) {
		store := openRefusingReindexStore(t)
		store.AddBatch([]*graph.Node{
			{ID: "a/caller.go", Kind: graph.KindFile, Name: "caller.go", FilePath: "a/caller.go", Language: "go"},
			{ID: "a/caller.go::Run", Kind: graph.KindFunction, Name: "Run", FilePath: "a/caller.go", Language: "go"},
			{ID: "c/imported.go", Kind: graph.KindFile, Name: "imported.go", FilePath: "c/imported.go", Language: "go"},
			{ID: "c/imported.go::Allowed", Kind: graph.KindFunction, Name: "Allowed", FilePath: "c/imported.go", Language: "go"},
		}, []*graph.Edge{
			{From: "a/caller.go", To: "c/imported.go", Kind: graph.EdgeImports, FilePath: "a/caller.go", Line: 1},
			{From: "a/caller.go::Run", To: "unresolved::Allowed", Kind: graph.EdgeCalls, FilePath: "a/caller.go", Line: 5, Meta: map[string]any{"guard_reverted": true}},
		})

		r := New(store)
		_, err := r.ResolveAllContext(context.Background())
		require.NoError(t, err)
		edges := store.GetOutEdges("a/caller.go::Run")
		require.Len(t, edges, 1)
		assert.Equal(t, "c/imported.go::Allowed", edges[0].To)
		assert.Nil(t, edges[0].Meta["guard_reverted"], "re-resolving an edge must clear stale guard_reverted metadata")
	})

	t.Run("cross_repo_resolve_clears_stale_guard_reverted_on_imported_target", func(t *testing.T) {
		store := openRefusingReindexStore(t)
		store.AddBatch([]*graph.Node{
			{ID: "repoA/a/caller.go", Kind: graph.KindFile, Name: "caller.go", FilePath: "repoA/a/caller.go", RepoPrefix: "repoA", WorkspaceID: "ws", Language: "go"},
			{ID: "repoA/a/caller.go::Run", Kind: graph.KindFunction, Name: "Run", FilePath: "repoA/a/caller.go", RepoPrefix: "repoA", WorkspaceID: "ws", Language: "go"},
			{ID: "repoB/b/target.go", Kind: graph.KindFile, Name: "target.go", FilePath: "repoB/b/target.go", RepoPrefix: "repoB", WorkspaceID: "ws", Language: "go"},
			{ID: "repoB/b/target.go::Helper", Kind: graph.KindFunction, Name: "Helper", FilePath: "repoB/b/target.go", RepoPrefix: "repoB", WorkspaceID: "ws", Language: "go"},
		}, []*graph.Edge{
			{From: "repoA/a/caller.go", To: "repoB/b/target.go", Kind: graph.EdgeImports, FilePath: "repoA/a/caller.go", Line: 1, CrossRepo: true},
			{From: "repoA/a/caller.go::Run", To: "unresolved::Helper", Kind: graph.EdgeCalls, FilePath: "repoA/a/caller.go", Line: 5, Meta: map[string]any{"guard_reverted": true}},
		})

		_, err := NewCrossRepo(store).ResolveAllContext(context.Background())
		require.NoError(t, err)
		var callEdge *graph.Edge
		for _, e := range store.GetOutEdges("repoA/a/caller.go::Run") {
			if e.Kind == graph.EdgeCalls {
				callEdge = e
			}
		}
		require.NotNil(t, callEdge)
		assert.Equal(t, "repoB/b/target.go::Helper", callEdge.To)
		assert.Nil(t, callEdge.Meta["guard_reverted"], "CrossRepoResolver must clear stale guard_reverted when binding a real target")
	})
}

func TestCrossRepoResolveAllReleasesChunkDeferredRepointsAcrossYield(t *testing.T) {
	t.Setenv("GORTEX_RESOLVE_CHUNK", "1")
	t.Setenv("GORTEX_RESOLVE_CHUNK_SIZE", "1")

	store := openRefusingReindexStore(t)
	store.AddBatch([]*graph.Node{
		{ID: "repoA/caller.ts", Kind: graph.KindFile, Name: "caller.ts", FilePath: "repoA/caller.ts", RepoPrefix: "repoA", WorkspaceID: "ws", Language: "typescript"},
		{ID: "repoA/caller.ts::run", Kind: graph.KindFunction, Name: "run", FilePath: "repoA/caller.ts", RepoPrefix: "repoA", WorkspaceID: "ws", Language: "typescript"},
		{ID: "repoB/caller.ts", Kind: graph.KindFile, Name: "caller.ts", FilePath: "repoB/caller.ts", RepoPrefix: "repoB", WorkspaceID: "ws", Language: "typescript"},
		{ID: "repoB/caller.ts::use", Kind: graph.KindFunction, Name: "use", FilePath: "repoB/caller.ts", RepoPrefix: "repoB", WorkspaceID: "ws", Language: "typescript"},
		{ID: "repoC/target.ts", Kind: graph.KindFile, Name: "target.ts", FilePath: "repoC/target.ts", RepoPrefix: "repoC", WorkspaceID: "ws", Language: "typescript"},
		{ID: "repoC/target.ts::helper", Kind: graph.KindFunction, Name: "helper", FilePath: "repoC/target.ts", RepoPrefix: "repoC", WorkspaceID: "ws", Language: "typescript"},
		{ID: "repoC/target.ts::other", Kind: graph.KindFunction, Name: "other", FilePath: "repoC/target.ts", RepoPrefix: "repoC", WorkspaceID: "ws", Language: "typescript"},
	}, []*graph.Edge{
		{From: "repoA/caller.ts", To: "repoC/target.ts", Kind: graph.EdgeImports, FilePath: "repoA/caller.ts", Line: 1, CrossRepo: true},
		{From: "repoA/caller.ts::run", To: "unresolved::helper", Kind: graph.EdgeCalls, FilePath: "repoA/caller.ts", Line: 2},
		{From: "repoA/unresolved::helper", To: "repoA/caller.ts::run", Kind: graph.EdgeArgOf, FilePath: "repoA/caller.ts", Line: 2},
		{From: "repoB/caller.ts", To: "repoC/target.ts", Kind: graph.EdgeImports, FilePath: "repoB/caller.ts", Line: 1, CrossRepo: true},
		{From: "repoB/caller.ts::use", To: "unresolved::other", Kind: graph.EdgeCalls, FilePath: "repoB/caller.ts", Line: 2},
	})

	var bridgeMu sync.Mutex
	var queued []graph.PlaceholderRepoint
	take := func(opts RetryTakeOpts) (CrossRepoRetryState, uint64) {
		bridgeMu.Lock()
		defer bridgeMu.Unlock()
		if !opts.AllRepos && len(opts.Repos) == 0 {
			return CrossRepoRetryState{}, 1
		}
		if opts.AllRepos {
			out := queued
			queued = nil
			return CrossRepoRetryState{Repoints: out}, 1
		}
		var out, keep []graph.PlaceholderRepoint
		for _, rp := range queued {
			repo := rp.RepoPrefix
			if repo == "" {
				repo = graph.RepoPrefixOfID(rp.FilePath)
			}
			if _, ok := opts.Repos[repo]; ok {
				out = append(out, rp)
			} else {
				keep = append(keep, rp)
			}
		}
		queued = keep
		return CrossRepoRetryState{Repoints: out}, 1
	}
	save := func(state CrossRepoRetryState, _ RetryBridgeSaveMeta) {
		bridgeMu.Lock()
		defer bridgeMu.Unlock()
		queued = append(queued, state.Repoints...)
	}

	cr1 := NewCrossRepo(store)
	cr1.SetRetryStateBridge(take, save, nil, nil)
	interleaved := false
	cr1.chunkYieldHook = func() {
		if interleaved {
			return
		}
		interleaved = true
		// Concurrent repoA lane enters during cr1's inter-chunk yield after Chunk 1
		// committed repoA/caller.ts::run -> repoC/target.ts::helper. It must observe
		// and apply Chunk 1's deferred repoint before certifying repoA fresh.
		cr2 := NewCrossRepo(store)
		cr2.SetRetryStateBridge(take, save, nil, nil)
		stats := cr2.ResolveMutationFrontiers([]string{"repoA/caller.ts"}, []string{"repoA/caller.ts"}, nil)
		assert.False(t, stats.WriteRefused)
		assert.Empty(t, store.GetOutEdges("repoA/unresolved::helper"), "interleaving repoA pass must observe and flush Chunk 1 deferred repoints during yield")
		assert.Len(t, store.GetOutEdges("repoC/target.ts::helper"), 1)
	}

	_, err := cr1.ResolveAllContext(context.Background())
	require.NoError(t, err)
	assert.True(t, interleaved)
	assert.Empty(t, queued, "bridge queue must be empty after both passes complete")
}

func TestCancelWithRefusedDeferredFlushStillDrainsGuardSpool(t *testing.T) {
	t.Setenv("GORTEX_RESOLVE_CHUNK", "1")
	t.Setenv("GORTEX_RESOLVE_CHUNK_SIZE", "1")

	store := openRefusingReindexStore(t)
	store.AddBatch([]*graph.Node{
		{ID: "a/caller.go", Kind: graph.KindFile, Name: "caller.go", FilePath: "a/caller.go", Language: "go"},
		{ID: "a/caller.go::Run", Kind: graph.KindFunction, Name: "Run", FilePath: "a/caller.go", Language: "go"},
		{ID: "b/unimported.go", Kind: graph.KindFile, Name: "unimported.go", FilePath: "b/unimported.go", Language: "go"},
		{ID: "b/unimported.go::Secret", Kind: graph.KindFunction, Name: "Secret", FilePath: "b/unimported.go", Language: "go"},
		{ID: "c/imported.go", Kind: graph.KindFile, Name: "imported.go", FilePath: "c/imported.go", Language: "go"},
		{ID: "c/imported.go::Allowed", Kind: graph.KindFunction, Name: "Allowed", FilePath: "c/imported.go", Language: "go"},
	}, []*graph.Edge{
		{From: "a/caller.go", To: "c/imported.go", Kind: graph.EdgeImports, FilePath: "a/caller.go", Line: 1},
		{From: "a/caller.go::Run", To: "unresolved::Secret", Kind: graph.EdgeCalls, FilePath: "a/caller.go", Line: 5},
		{From: "unresolved::Secret", To: "a/caller.go::Run", Kind: graph.EdgeArgOf, FilePath: "a/caller.go", Line: 5},
		{From: "a/caller.go::Run", To: "unresolved::Allowed", Kind: graph.EdgeCalls, FilePath: "a/caller.go", Line: 6},
	})

	ctx, cancel := context.WithCancel(context.Background())
	// Chunk 1 commits Secret; chunkYieldHook cancels ctx AND arms refusal on Secret's
	// placeholder move so resolveError's applyDeferredForScopeApplied is refused.
	r := New(store)
	r.chunkYieldHook = func() {
		store.refuseFunc = refuseOnceMatching(batchHasPlaceholderMove("unresolved::Secret", "b/unimported.go::Secret"))
		cancel()
	}

	stats, err := r.ResolveAllContext(ctx)
	require.ErrorIs(t, err, context.Canceled)
	assert.True(t, stats.WriteRefused, "refused applyDeferredForScopeApplied inside cancel resolveError must set WriteRefused")
	assert.Equal(t, 1, store.refused)

	store.refuseFunc = nil
	r.chunkYieldHook = nil
	_, err = r.ResolveAllContext(context.Background())
	require.NoError(t, err)
	var secretEdge *graph.Edge
	for _, e := range store.GetOutEdges("a/caller.go::Run") {
		if e.Line == 5 {
			secretEdge = e
		}
	}
	require.NotNil(t, secretEdge)
	assert.Equal(t, "unresolved::Secret", secretEdge.To, "guardSpool must still drain when cancel's deferred flush is refused")
}

func TestRefusedInheritedBacklogDoesNotSkipSaveResolution(t *testing.T) {
	for _, mode := range []string{"resolve_file", "files_and_incoming", "files_outgoing"} {
		t.Run(mode, func(t *testing.T) {
			store := openRefusingReindexStore(t)
			store.AddBatch([]*graph.Node{
				{ID: "old.go", Kind: graph.KindFile, Name: "old.go", FilePath: "old.go", Language: "go"},
				{ID: "old.go::OldRun", Kind: graph.KindFunction, Name: "OldRun", FilePath: "old.go", Language: "go"},
				{ID: "old_target.go", Kind: graph.KindFile, Name: "old_target.go", FilePath: "old_target.go", Language: "go"},
				{ID: "old_target.go::OldHelper", Kind: graph.KindFunction, Name: "OldHelper", FilePath: "old_target.go", Language: "go"},
			}, []*graph.Edge{
				{From: "old.go::OldRun", To: "unresolved::OldHelper", Kind: graph.EdgeCalls, FilePath: "old.go", Line: 5},
				{From: "unresolved::OldHelper", To: "old.go::OldRun", Kind: graph.EdgeArgOf, FilePath: "old.go", Line: 5},
			})

			// Refuse placeholder repoint during full pass so old.go's repoint is retained.
			store.refuseFunc = refuseOnceMatching(batchHasPlaceholderMove("unresolved::OldHelper", "old_target.go::OldHelper"))
			r := New(store)
			_, err := r.ResolveAllContext(context.Background())
			require.ErrorIs(t, err, ErrReindexWriteRefused)
			assert.Equal(t, 1, store.refused)
			require.Equal(t, 1, r.PendingRetryStats().PlaceholderRepoints)

			// Add new.go with a bindable call and clean no-pending other.go.
			store.AddBatch([]*graph.Node{
				{ID: "new.go", Kind: graph.KindFile, Name: "new.go", FilePath: "new.go", Language: "go"},
				{ID: "new.go::NewRun", Kind: graph.KindFunction, Name: "NewRun", FilePath: "new.go", Language: "go"},
				{ID: "new_target.go", Kind: graph.KindFile, Name: "new_target.go", FilePath: "new_target.go", Language: "go"},
				{ID: "new_target.go::NewHelper", Kind: graph.KindFunction, Name: "NewHelper", FilePath: "new_target.go", Language: "go"},
				{ID: "other.go", Kind: graph.KindFile, Name: "other.go", FilePath: "other.go", Language: "go"},
			}, []*graph.Edge{
				{From: "new.go::NewRun", To: "unresolved::NewHelper", Kind: graph.EdgeCalls, FilePath: "new.go", Line: 7},
			})

			// Refuse old.go's repoint batch by content while saving new.go.
			store.refused = 0
			store.refuseFunc = refuseOnceMatching(batchHasPlaceholderMove("unresolved::OldHelper", "old_target.go::OldHelper"))
			var stats *ResolveStats
			switch mode {
			case "resolve_file":
				stats = r.ResolveFile("new.go")
			case "files_and_incoming":
				stats = r.ResolveFilesAndIncoming([]string{"new.go"})
			case "files_outgoing":
				stats = r.ResolveFilesOutgoing([]string{"new.go"})
			}
			assert.False(t, stats.WriteRefused, "refused non-frontier Step 3 backlog must not poison new.go's clean save")
			assert.Equal(t, 1, store.refused)
			newEdges := store.GetOutEdges("new.go::NewRun")
			require.Len(t, newEdges, 1)
			assert.Equal(t, "new_target.go::NewHelper", newEdges[0].To, "new.go must resolve immediately despite refused inherited backlog")
			assert.Equal(t, 1, r.PendingRetryStats().PlaceholderRepoints, "refused old.go repoint must remain queued")

			// A later clean zero-pending save on other.go drains old.go's repoint.
			store.refuseFunc = nil
			stats = r.ResolveFilesAndIncoming([]string{"other.go"})
			assert.False(t, stats.WriteRefused)
			assert.Equal(t, 0, r.PendingRetryStats().PlaceholderRepoints)
			assert.Empty(t, store.GetOutEdges("unresolved::OldHelper"))
			assert.Len(t, store.GetOutEdges("old_target.go::OldHelper"), 1)
		})
	}

	t.Run("cross_repo_mutation_frontiers", func(t *testing.T) {
		store := openRefusingReindexStore(t)
		store.AddBatch([]*graph.Node{
			{ID: "repoA/old.ts", Kind: graph.KindFile, Name: "old.ts", FilePath: "repoA/old.ts", RepoPrefix: "repoA", WorkspaceID: "ws", Language: "typescript"},
			{ID: "repoA/old.ts::run", Kind: graph.KindFunction, Name: "run", FilePath: "repoA/old.ts", RepoPrefix: "repoA", WorkspaceID: "ws", Language: "typescript"},
			{ID: "repoB/old_target.ts", Kind: graph.KindFile, Name: "old_target.ts", FilePath: "repoB/old_target.ts", RepoPrefix: "repoB", WorkspaceID: "ws", Language: "typescript"},
			{ID: "repoB/old_target.ts::oldHelper", Kind: graph.KindFunction, Name: "oldHelper", FilePath: "repoB/old_target.ts", RepoPrefix: "repoB", WorkspaceID: "ws", Language: "typescript"},
		}, []*graph.Edge{
			{From: "repoA/old.ts", To: "repoB/old_target.ts", Kind: graph.EdgeImports, FilePath: "repoA/old.ts", Line: 1, CrossRepo: true},
			{From: "repoA/old.ts::run", To: "unresolved::oldHelper", Kind: graph.EdgeCalls, FilePath: "repoA/old.ts", Line: 2},
			{From: "repoA/unresolved::oldHelper", To: "repoA/old.ts::run", Kind: graph.EdgeArgOf, FilePath: "repoA/old.ts", Line: 2},
		})

		store.refuseFunc = refuseOnceMatching(batchHasPlaceholderMove("repoA/unresolved::oldHelper", "repoB/old_target.ts::oldHelper"))
		cr := NewCrossRepo(store)
		_, err := cr.ResolveAllContext(context.Background())
		require.ErrorIs(t, err, ErrReindexWriteRefused)
		assert.Equal(t, 1, store.refused)

		store.AddBatch([]*graph.Node{
			{ID: "repoA/new.ts", Kind: graph.KindFile, Name: "new.ts", FilePath: "repoA/new.ts", RepoPrefix: "repoA", WorkspaceID: "ws", Language: "typescript"},
			{ID: "repoA/new.ts::run", Kind: graph.KindFunction, Name: "run", FilePath: "repoA/new.ts", RepoPrefix: "repoA", WorkspaceID: "ws", Language: "typescript"},
			{ID: "repoA/other.ts", Kind: graph.KindFile, Name: "other.ts", FilePath: "repoA/other.ts", RepoPrefix: "repoA", WorkspaceID: "ws", Language: "typescript"},
			{ID: "repoB/new_target.ts", Kind: graph.KindFile, Name: "new_target.ts", FilePath: "repoB/new_target.ts", RepoPrefix: "repoB", WorkspaceID: "ws", Language: "typescript"},
			{ID: "repoB/new_target.ts::newHelper", Kind: graph.KindFunction, Name: "newHelper", FilePath: "repoB/new_target.ts", RepoPrefix: "repoB", WorkspaceID: "ws", Language: "typescript"},
		}, []*graph.Edge{
			{From: "repoA/new.ts", To: "repoB/new_target.ts", Kind: graph.EdgeImports, FilePath: "repoA/new.ts", Line: 1, CrossRepo: true},
			{From: "repoA/new.ts::run", To: "unresolved::newHelper", Kind: graph.EdgeCalls, FilePath: "repoA/new.ts", Line: 2},
		})

		store.refused = 0
		store.refuseFunc = refuseOnceMatching(batchHasPlaceholderMove("repoA/unresolved::oldHelper", "repoB/old_target.ts::oldHelper"))
		stats := cr.ResolveMutationFrontiers([]string{"repoA/new.ts"}, []string{"repoA/new.ts"}, []string{"repoA/new.ts"})
		assert.False(t, stats.WriteRefused)
		assert.Equal(t, 1, store.refused)
		var sawNew bool
		for _, e := range store.GetOutEdges("repoA/new.ts::run") {
			if e.Kind == graph.EdgeCalls && e.To == "repoB/new_target.ts::newHelper" {
				sawNew = true
			}
		}
		assert.True(t, sawNew, "repoA/new.ts must resolve immediately despite refused non-frontier repoint")

		store.refuseFunc = nil
		stats = cr.ResolveMutationFrontiers([]string{"repoA/other.ts"}, []string{"repoA/other.ts"}, []string{"repoA/other.ts"})
		assert.False(t, stats.WriteRefused)
		assert.Empty(t, store.GetOutEdges("repoA/unresolved::oldHelper"))
		assert.Len(t, store.GetOutEdges("repoB/old_target.ts::oldHelper"), 1)
	})
}

type scanCountingSQLiteStore struct {
	*store_sqlite.Store
	dataflowScans atomic.Int64
}

func hasDataflowKind(kinds ...graph.EdgeKind) bool {
	for _, k := range kinds {
		if k == graph.EdgeArgOf || k == graph.EdgeValueFlow {
			return true
		}
	}
	return false
}

func (s *scanCountingSQLiteStore) EdgesByKind(kind graph.EdgeKind) iter.Seq[*graph.Edge] {
	if hasDataflowKind(kind) {
		s.dataflowScans.Add(1)
	}
	return s.Store.EdgesByKind(kind)
}

func (s *scanCountingSQLiteStore) EdgesLightSeq(kinds ...graph.EdgeKind) iter.Seq[*graph.Edge] {
	if hasDataflowKind(kinds...) {
		s.dataflowScans.Add(1)
	}
	return s.Store.EdgesLightSeq(kinds...)
}

func TestIncrementalRepointFlushUsesDirectProbesWithoutWholeGraphScan(t *testing.T) {
	base, err := store_sqlite.Open(filepath.Join(t.TempDir(), "scan-count.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, base.Close()) })
	store := &scanCountingSQLiteStore{Store: base}

	store.AddBatch([]*graph.Node{
		{ID: "repoA/a.go", Kind: graph.KindFile, Name: "a.go", FilePath: "repoA/a.go", RepoPrefix: "repoA", WorkspaceID: "ws", Language: "go"},
		{ID: "repoA/a.go::Run", Kind: graph.KindFunction, Name: "Run", FilePath: "repoA/a.go", RepoPrefix: "repoA", WorkspaceID: "ws", Language: "go"},
		{ID: "repoA/t.go", Kind: graph.KindFile, Name: "t.go", FilePath: "repoA/t.go", RepoPrefix: "repoA", WorkspaceID: "ws", Language: "go"},
		{ID: "repoA/t.go::Helper", Kind: graph.KindFunction, Name: "Helper", FilePath: "repoA/t.go", RepoPrefix: "repoA", WorkspaceID: "ws", Language: "go"},
		{ID: "repoA/other.go", Kind: graph.KindFile, Name: "other.go", FilePath: "repoA/other.go", RepoPrefix: "repoA", WorkspaceID: "ws", Language: "go"},
		{ID: "repoB/x.ts", Kind: graph.KindFile, Name: "x.ts", FilePath: "repoB/x.ts", RepoPrefix: "repoB", WorkspaceID: "ws", Language: "typescript"},
		{ID: "repoB/x.ts::fn", Kind: graph.KindFunction, Name: "fn", FilePath: "repoB/x.ts", RepoPrefix: "repoB", WorkspaceID: "ws", Language: "typescript"},
		{ID: "repoB/other.ts", Kind: graph.KindFile, Name: "other.ts", FilePath: "repoB/other.ts", RepoPrefix: "repoB", WorkspaceID: "ws", Language: "typescript"},
		{ID: "repoC/lib.ts", Kind: graph.KindFile, Name: "lib.ts", FilePath: "repoC/lib.ts", RepoPrefix: "repoC", WorkspaceID: "ws", Language: "typescript"},
		{ID: "repoC/lib.ts::target", Kind: graph.KindFunction, Name: "target", FilePath: "repoC/lib.ts", RepoPrefix: "repoC", WorkspaceID: "ws", Language: "typescript"},
		{ID: "repoC/lib.ts::t1", Kind: graph.KindFunction, Name: "t1", FilePath: "repoC/lib.ts", RepoPrefix: "repoC", WorkspaceID: "ws", Language: "typescript"},
		{ID: "repoC/lib.ts::t2", Kind: graph.KindFunction, Name: "t2", FilePath: "repoC/lib.ts", RepoPrefix: "repoC", WorkspaceID: "ws", Language: "typescript"},
	}, []*graph.Edge{
		{From: "repoA/unresolved::Helper", To: "repoA/a.go::Run", Kind: graph.EdgeArgOf, FilePath: "repoA/a.go", Line: 5},
		{From: "repoB/unresolved::target", To: "repoB/x.ts::fn", Kind: graph.EdgeArgOf, FilePath: "repoB/x.ts", Line: 5},
		{From: "repoB/x.ts", To: "repoC/lib.ts", Kind: graph.EdgeImports, FilePath: "repoB/x.ts", Line: 1, CrossRepo: true},
		{From: "repoB/x.ts::fn", To: "unresolved::t1", Kind: graph.EdgeCalls, FilePath: "repoB/x.ts", Line: 10},
		{From: "repoB/x.ts::fn", To: "unresolved::t2", Kind: graph.EdgeCalls, FilePath: "repoB/x.ts", Line: 11},
	})

	r := New(store)
	r.InheritPendingPlaceholderRepoints([]graph.PlaceholderRepoint{
		{OldFrom: "repoA/unresolved::Helper", FilePath: "repoA/a.go", Line: 5, NewFrom: "repoA/t.go::Helper", RepoPrefix: "repoA"},
	})
	store.dataflowScans.Store(0)
	stats := r.ResolveFilesAndIncoming([]string{"repoA/other.go"})
	assert.False(t, stats.WriteRefused)
	assert.Equal(t, int64(0), store.dataflowScans.Load(), "incremental Resolver repoint flush must use direct probes with zero whole-graph scans")
	assert.Empty(t, store.GetOutEdges("repoA/unresolved::Helper"))
	assert.Len(t, store.GetOutEdges("repoA/t.go::Helper"), 1)

	cr := NewCrossRepo(store)
	cr.InheritPendingPlaceholderRepoints([]graph.PlaceholderRepoint{
		{OldFrom: "repoB/unresolved::target", FilePath: "repoB/x.ts", Line: 5, NewFrom: "repoC/lib.ts::target", RepoPrefix: "repoB"},
	})
	store.dataflowScans.Store(0)
	cstats := cr.ResolveMutationFrontiers([]string{"repoB/other.ts"}, []string{"repoB/other.ts"}, []string{"repoB/other.ts"})
	assert.False(t, cstats.WriteRefused)
	assert.Equal(t, int64(0), store.dataflowScans.Load(), "incremental CrossRepoResolver repoint flush must use direct probes with zero whole-graph scans")
	assert.Empty(t, store.GetOutEdges("repoB/unresolved::target"))
	assert.Len(t, store.GetOutEdges("repoC/lib.ts::target"), 1)

	// Clean multi-chunk CrossRepoResolver.ResolveAllContext with yield bridge must scan at most once.
	t.Setenv("GORTEX_RESOLVE_CHUNK", "1")
	t.Setenv("GORTEX_RESOLVE_CHUNK_SIZE", "1")
	var bridgeMu sync.Mutex
	var held CrossRepoRetryState
	take := func(opts RetryTakeOpts) (CrossRepoRetryState, uint64) {
		bridgeMu.Lock()
		defer bridgeMu.Unlock()
		out := held
		held = CrossRepoRetryState{}
		return out, 1
	}
	save := func(s CrossRepoRetryState, _ RetryBridgeSaveMeta) {
		bridgeMu.Lock()
		defer bridgeMu.Unlock()
		held = s
	}
	crChunked := NewCrossRepo(store)
	crChunked.SetRetryStateBridge(take, save, nil, nil)
	store.dataflowScans.Store(0)
	_, err = crChunked.ResolveAllContext(context.Background())
	require.NoError(t, err)
	assert.LessOrEqual(t, store.dataflowScans.Load(), int64(1), "clean multi-chunk CrossRepoResolver.ResolveAllContext must preserve census across !mutated yields")
}

func TestBoundedIncrementalSaveLeavesLargeFullPassBacklogForResolveAll(t *testing.T) {
	store := openRefusingReindexStore(t)
	const (
		repointCount     = 220
		guardCount       = 220
		appliedSrcCount  = 20
		unpairedCount    = 10
		staleLSPCount    = 110
		coalescedEntries = 5
	)

	nodes := []*graph.Node{
		{ID: "repoA/other.go", Kind: graph.KindFile, Name: "other.go", FilePath: "repoA/other.go", RepoPrefix: "repoA", Language: "go"},
	}
	var edges []*graph.Edge
	var repoints []graph.PlaceholderRepoint
	for i := 0; i < repointCount; i++ {
		fp := fmt.Sprintf("repoA/r%03d.go", i)
		tfp := fmt.Sprintf("repoA/rt%03d.go", i)
		fn := fmt.Sprintf("rfn_%03d", i)
		oldFrom := "repoA/unresolved::" + fn
		caller := fp + "::Caller"
		target := tfp + "::" + fn
		nodes = append(nodes,
			&graph.Node{ID: fp, Kind: graph.KindFile, Name: fp, FilePath: fp, RepoPrefix: "repoA", Language: "go"},
			&graph.Node{ID: caller, Kind: graph.KindFunction, Name: "Caller", FilePath: fp, RepoPrefix: "repoA", Language: "go"},
			&graph.Node{ID: tfp, Kind: graph.KindFile, Name: tfp, FilePath: tfp, RepoPrefix: "repoA", Language: "go"},
			&graph.Node{ID: target, Kind: graph.KindFunction, Name: fn, FilePath: tfp, RepoPrefix: "repoA", Language: "go"},
		)
		edges = append(edges, &graph.Edge{From: oldFrom, To: caller, Kind: graph.EdgeArgOf, FilePath: fp, Line: 10})
		repoints = append(repoints, graph.PlaceholderRepoint{OldFrom: oldFrom, FilePath: fp, Line: 10, NewFrom: target, RepoPrefix: "repoA"})
	}

	// Build guard reverts covering: paired reindexesApplied==false, reindexesApplied==true SourceReverts,
	// unpaired Reindexes, coalesced entries, and >100 stale/LSP-promoted no-ops.
	var guardReindexes []graph.EdgeReindex
	var guardSources []graph.PlaceholderRepoint
	for i := 0; i < guardCount; i++ {
		fp := fmt.Sprintf("repoA/g%03d.go", i)
		dfp := fmt.Sprintf("repoA/gd%03d.go", i)
		fn := fmt.Sprintf("gfn_%03d", i)
		caller := fp + "::Caller"
		decoy := dfp + "::" + fn
		nodes = append(nodes,
			&graph.Node{ID: fp, Kind: graph.KindFile, Name: fp, FilePath: fp, RepoPrefix: "repoA", Language: "go"},
			&graph.Node{ID: caller, Kind: graph.KindFunction, Name: "Caller", FilePath: fp, RepoPrefix: "repoA", Language: "go"},
			&graph.Node{ID: dfp, Kind: graph.KindFile, Name: dfp, FilePath: dfp, RepoPrefix: "repoA", Language: "go"},
			&graph.Node{ID: decoy, Kind: graph.KindFunction, Name: fn, FilePath: dfp, RepoPrefix: "repoA", Language: "go"},
		)
		edges = append(edges,
			&graph.Edge{From: caller, To: decoy, Kind: graph.EdgeCalls, FilePath: fp, Line: 20, Confidence: 0.4, Origin: graph.OriginTextMatched},
			&graph.Edge{From: decoy, To: caller, Kind: graph.EdgeArgOf, FilePath: fp, Line: 20},
		)
		guardReindexes = append(guardReindexes, graph.EdgeReindex{
			Edge:  &graph.Edge{From: caller, To: graph.UnresolvedMarker + fn, Kind: graph.EdgeCalls, FilePath: fp, Line: 20, Meta: map[string]any{"guard_reverted": true}},
			OldTo: decoy,
		})
		guardSources = append(guardSources, graph.PlaceholderRepoint{
			OldFrom: decoy, FilePath: fp, Line: 20, NewFrom: "repoA/" + graph.UnresolvedMarker + fn, RepoPrefix: "repoA",
		})
	}

	var appliedSources []graph.PlaceholderRepoint
	for i := 0; i < appliedSrcCount; i++ {
		fp := fmt.Sprintf("repoA/as%03d.go", i)
		dfp := fmt.Sprintf("repoA/asd%03d.go", i)
		fn := fmt.Sprintf("asfn_%03d", i)
		caller := fp + "::Caller"
		decoy := dfp + "::" + fn
		nodes = append(nodes,
			&graph.Node{ID: fp, Kind: graph.KindFile, Name: fp, FilePath: fp, RepoPrefix: "repoA", Language: "go"},
			&graph.Node{ID: caller, Kind: graph.KindFunction, Name: "Caller", FilePath: fp, RepoPrefix: "repoA", Language: "go"},
			&graph.Node{ID: dfp, Kind: graph.KindFile, Name: dfp, FilePath: dfp, RepoPrefix: "repoA", Language: "go"},
			&graph.Node{ID: decoy, Kind: graph.KindFunction, Name: fn, FilePath: dfp, RepoPrefix: "repoA", Language: "go"},
		)
		edges = append(edges, &graph.Edge{From: decoy, To: caller, Kind: graph.EdgeArgOf, FilePath: fp, Line: 25})
		appliedSources = append(appliedSources, graph.PlaceholderRepoint{
			OldFrom: decoy, FilePath: fp, Line: 25, NewFrom: "repoA/" + graph.UnresolvedMarker + fn, RepoPrefix: "repoA",
		})
	}

	var unpairedReindexes []graph.EdgeReindex
	for i := 0; i < unpairedCount; i++ {
		fp := fmt.Sprintf("repoA/up%03d.go", i)
		dfp := fmt.Sprintf("repoA/upd%03d.go", i)
		fn := fmt.Sprintf("upfn_%03d", i)
		caller := fp + "::Caller"
		decoy := dfp + "::" + fn
		nodes = append(nodes,
			&graph.Node{ID: fp, Kind: graph.KindFile, Name: fp, FilePath: fp, RepoPrefix: "repoA", Language: "go"},
			&graph.Node{ID: caller, Kind: graph.KindFunction, Name: "Caller", FilePath: fp, RepoPrefix: "repoA", Language: "go"},
			&graph.Node{ID: dfp, Kind: graph.KindFile, Name: dfp, FilePath: dfp, RepoPrefix: "repoA", Language: "go"},
			&graph.Node{ID: decoy, Kind: graph.KindFunction, Name: fn, FilePath: dfp, RepoPrefix: "repoA", Language: "go"},
		)
		edges = append(edges, &graph.Edge{From: caller, To: decoy, Kind: graph.EdgeCalls, FilePath: fp, Line: 30, Confidence: 0.4, Origin: graph.OriginTextMatched})
		unpairedReindexes = append(unpairedReindexes, graph.EdgeReindex{
			Edge:  &graph.Edge{From: caller, To: graph.UnresolvedMarker + fn, Kind: graph.EdgeCalls, FilePath: fp, Line: 30, Meta: map[string]any{"guard_reverted": true}},
			OldTo: decoy,
		})
	}

	var staleLSPReindexes []graph.EdgeReindex
	for i := 0; i < staleLSPCount; i++ {
		fp := fmt.Sprintf("repoA/lsp%03d.go", i)
		dfp := fmt.Sprintf("repoA/lspd%03d.go", i)
		fn := fmt.Sprintf("lspfn_%03d", i)
		caller := fp + "::Caller"
		decoy := dfp + "::" + fn
		nodes = append(nodes,
			&graph.Node{ID: fp, Kind: graph.KindFile, Name: fp, FilePath: fp, RepoPrefix: "repoA", Language: "go"},
			&graph.Node{ID: caller, Kind: graph.KindFunction, Name: "Caller", FilePath: fp, RepoPrefix: "repoA", Language: "go"},
			&graph.Node{ID: dfp, Kind: graph.KindFile, Name: dfp, FilePath: dfp, RepoPrefix: "repoA", Language: "go"},
			&graph.Node{ID: decoy, Kind: graph.KindFunction, Name: fn, FilePath: dfp, RepoPrefix: "repoA", Language: "go"},
		)
		edges = append(edges, &graph.Edge{From: caller, To: decoy, Kind: graph.EdgeCalls, FilePath: fp, Line: 35, Confidence: 0.99, Origin: graph.OriginLSPResolved})
		staleLSPReindexes = append(staleLSPReindexes, graph.EdgeReindex{
			Edge:  &graph.Edge{From: caller, To: graph.UnresolvedMarker + fn, Kind: graph.EdgeCalls, FilePath: fp, Line: 35, Meta: map[string]any{"guard_reverted": true}},
			OldTo: decoy,
		})
	}

	store.AddBatch(nodes, edges)

	// Sub-check 1: coalescing multiple single-item refusals stays <= 2 write txs per Step 3 drain.
	t.Run("coalesced_multi_refusal_bounded_write_txs", func(t *testing.T) {
		r := New(store)
		for i := 0; i < coalescedEntries; i++ {
			r.appendPendingGuardRevertLocked(PendingGuardRevert{
				RepoPrefix:    "repoA",
				Reindexes:     []graph.EdgeReindex{guardReindexes[i]},
				SourceReverts: []graph.PlaceholderRepoint{guardSources[i]},
			})
		}
		require.Len(t, r.pendingGuardReverts, 1, "repeated single-item paired refusals must coalesce into one entry")
		callsBefore := store.calls
		stats := r.ResolveFilesAndIncoming([]string{"repoA/other.go"})
		assert.False(t, stats.WriteRefused)
		assert.LessOrEqual(t, store.calls-callsBefore, 2, "coalesced Step 3 drain must commit in <= 2 write transactions")
		assert.Equal(t, 0, r.PendingRetryStats().GuardReverts)
	})

	// Sub-check 2: >100 stale/LSP-promoted entries still count against the 64-item probed budget.
	t.Run("stale_lsp_entries_respect_probed_item_budget", func(t *testing.T) {
		r := New(store)
		r.InheritPendingGuardReverts([]PendingGuardRevert{{RepoPrefix: "repoA", Reindexes: append([]graph.EdgeReindex(nil), staleLSPReindexes...)}})
		stats := r.ResolveFilesAndIncoming([]string{"repoA/other.go"})
		assert.False(t, stats.WriteRefused)
		rem := r.PendingRetryStats().GuardReverts
		assert.Equal(t, staleLSPCount-incrementalFlushGuardRevertBudget, rem, "probed-item budget must stop after 64 stale/LSP-promoted entries")
		_, err := r.ResolveAllContext(context.Background())
		require.NoError(t, err)
		assert.Equal(t, 0, r.PendingRetryStats().GuardReverts)
	})

	// Sub-check 3: large full-pass backlog bounded on single save, remainder preserved with RepoPrefix, drained 100% by ResolveAllContext.
	r := New(store)
	r.InheritPendingPlaceholderRepoints(repoints)
	r.InheritPendingGuardReverts([]PendingGuardRevert{
		{RepoPrefix: "repoA", Reindexes: guardReindexes[coalescedEntries:], SourceReverts: guardSources[coalescedEntries:]},
		{RepoPrefix: "repoA", SourceReverts: appliedSources, reindexesApplied: true},
		{RepoPrefix: "repoA", Reindexes: unpairedReindexes},
	})
	totalGuardsBefore := r.PendingRetryStats().GuardReverts
	stats := r.ResolveFilesAndIncoming([]string{"repoA/other.go"})
	assert.False(t, stats.WriteRefused)
	remRepoints := r.PendingRetryStats().PlaceholderRepoints
	remGuards := r.PendingRetryStats().GuardReverts
	assert.Equal(t, repointCount-incrementalFlushRepointBudget, remRepoints, "incremental save must drain at most 64 repoints")
	assert.Equal(t, totalGuardsBefore-incrementalFlushGuardRevertBudget, remGuards, "incremental save must probe at most 64 guard-revert items")
	for _, rp := range r.placeholderSrcIdx.deferred {
		assert.Equal(t, "repoA", rp.RepoPrefix, "preserved tail repoints must retain RepoPrefix")
	}
	for _, gr := range r.pendingGuardReverts {
		assert.Equal(t, "repoA", gr.RepoPrefix, "preserved tail guard reverts must retain RepoPrefix")
	}

	_, err := r.ResolveAllContext(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 0, r.PendingRetryStats().PlaceholderRepoints)
	assert.Equal(t, 0, r.PendingRetryStats().GuardReverts)
	for i := 0; i < repointCount; i++ {
		fn := fmt.Sprintf("rfn_%03d", i)
		assert.Empty(t, store.GetOutEdges("repoA/unresolved::"+fn))
	}
	for i := 0; i < appliedSrcCount; i++ {
		dfp := fmt.Sprintf("repoA/asd%03d.go", i)
		fn := fmt.Sprintf("asfn_%03d", i)
		assert.Empty(t, store.GetOutEdges(dfp+"::"+fn))
	}
}

func TestStep3OpportunisticDrainFastFailsUnderSQLiteWriterContention(t *testing.T) {
	store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "step3-fastfail.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })

	store.AddBatch([]*graph.Node{
		{ID: "old.go", Kind: graph.KindFile, Name: "old.go", FilePath: "old.go", Language: "go"},
		{ID: "old.go::Caller", Kind: graph.KindFunction, Name: "Caller", FilePath: "old.go", Language: "go"},
		{ID: "target.go", Kind: graph.KindFile, Name: "target.go", FilePath: "target.go", Language: "go"},
		{ID: "target.go::Target", Kind: graph.KindFunction, Name: "Target", FilePath: "target.go", Language: "go"},
		{ID: "decoy.go", Kind: graph.KindFile, Name: "decoy.go", FilePath: "decoy.go", Language: "go"},
		{ID: "decoy.go::Decoy", Kind: graph.KindFunction, Name: "Decoy", FilePath: "decoy.go", Language: "go"},
		{ID: "other.go", Kind: graph.KindFile, Name: "other.go", FilePath: "other.go", Language: "go"},
	}, []*graph.Edge{
		{From: "unresolved::Target", To: "old.go::Caller", Kind: graph.EdgeArgOf, FilePath: "old.go", Line: 5},
		{From: "old.go::Caller", To: "decoy.go::Decoy", Kind: graph.EdgeCalls, FilePath: "old.go", Line: 8, Confidence: 0.4, Origin: graph.OriginTextMatched},
	})

	core, recorded := observer.New(zapcore.InfoLevel)
	r := New(store)
	r.SetLogger(zap.New(core))
	r.InheritPendingPlaceholderRepoints([]graph.PlaceholderRepoint{
		{OldFrom: "unresolved::Target", FilePath: "old.go", Line: 5, NewFrom: "target.go::Target"},
	})
	r.InheritPendingGuardReverts([]PendingGuardRevert{{
		Reindexes: []graph.EdgeReindex{{
			Edge:  &graph.Edge{From: "old.go::Caller", To: "unresolved::Decoy", Kind: graph.EdgeCalls, FilePath: "old.go", Line: 8, Meta: map[string]any{"guard_reverted": true}},
			OldTo: "decoy.go::Decoy",
		}},
	}})

	// Hold the real SQLite writer gate with default 15s busy timeout intact.
	// Unbridged Resolver Step 3 fast-fail emits 1 Warn and 0 bridged Info logs.
	release, err := store.HoldWriteGate(context.Background())
	require.NoError(t, err)
	start := time.Now()
	stats := r.ResolveFilesAndIncoming([]string{"other.go"})
	elapsed := time.Since(start)
	release()

	assert.Less(t, elapsed, 500*time.Millisecond, "Step 3 opportunistic drain must fast-fail under writer contention instead of waiting 15s")
	assert.False(t, stats.WriteRefused, "clean zero-pending save must not report WriteRefused when Step 3 fast-fails")
	assert.Equal(t, 1, r.PendingRetryStats().PlaceholderRepoints, "Step 3 fast-fail must preserve queued repoints")
	assert.Equal(t, 1, r.PendingRetryStats().GuardReverts, "Step 3 fast-fail must preserve queued guard reverts")
	assert.Equal(t, 1, recorded.FilterLevelExact(zapcore.WarnLevel).FilterMessage("resolver: opportunistic retry drain refused under contention; keeping tail queued").Len())
	assert.Zero(t, recorded.FilterMessage("resolver: opportunistic retry drain deferred under contention").Len())
	assert.Zero(t, recorded.FilterMessage("resolver: bounded incremental pass deferred under contention").Len())

	// Bridged Resolver Step 3 fast-fail emits 1 Info and 0 Warn logs.
	recorded.TakeAll()
	bridgedState := MasterRetryState{
		PlaceholderRepoints: []graph.PlaceholderRepoint{
			{OldFrom: "unresolved::Target", FilePath: "old.go", Line: 5, NewFrom: "target.go::Target"},
		},
		GuardReverts: []PendingGuardRevert{{
			Reindexes: []graph.EdgeReindex{{
				Edge:  &graph.Edge{From: "old.go::Caller", To: "unresolved::Decoy", Kind: graph.EdgeCalls, FilePath: "old.go", Line: 8, Meta: map[string]any{"guard_reverted": true}},
				OldTo: "decoy.go::Decoy",
			}},
		}},
	}
	rBridged := New(store)
	rBridged.SetLogger(zap.New(core))
	rBridged.SetRetryStateBridge(
		func(_ RetryTakeOpts) (MasterRetryState, uint64) {
			taken := bridgedState
			bridgedState = MasterRetryState{}
			return taken, 1
		},
		func(st MasterRetryState, _ RetryBridgeSaveMeta) { bridgedState = st },
		nil,
		nil,
	)
	release, err = store.HoldWriteGate(context.Background())
	require.NoError(t, err)
	statsBridged := rBridged.ResolveFilesAndIncoming([]string{"other.go"})
	release()
	assert.False(t, statsBridged.WriteRefused)
	assert.Len(t, bridgedState.PlaceholderRepoints, 1)
	assert.Equal(t, 1, recorded.FilterLevelExact(zapcore.InfoLevel).FilterMessage("resolver: opportunistic retry drain deferred under contention").Len())
	assert.Zero(t, recorded.FilterLevelExact(zapcore.WarnLevel).Len())
	assert.Zero(t, recorded.FilterMessage("resolver: bounded incremental pass deferred under contention").Len())

	// Bounded Resolver WithTimeout pass emits 1 Info on WriteRefused and 0 Step 3 logs.
	store.AddBatch([]*graph.Node{
		{ID: "bounded.go", Kind: graph.KindFile, Name: "bounded.go", FilePath: "bounded.go", Language: "go"},
		{ID: "bounded.go::Run", Kind: graph.KindFunction, Name: "Run", FilePath: "bounded.go", Language: "go"},
	}, []*graph.Edge{
		{From: "bounded.go::Run", To: "unresolved::Target", Kind: graph.EdgeCalls, FilePath: "bounded.go", Line: 3},
	})
	recorded.TakeAll()
	rBounded := New(store)
	rBounded.SetLogger(zap.New(core))
	release, err = store.HoldWriteGate(context.Background())
	require.NoError(t, err)
	statsBounded := rBounded.ResolveFilesAndIncomingObservedWithTimeout([]string{"bounded.go"}, nil, 50*time.Millisecond)
	release()
	assert.True(t, statsBounded.WriteRefused)
	assert.Equal(t, 1, recorded.FilterLevelExact(zapcore.InfoLevel).FilterMessage("resolver: bounded incremental pass deferred under contention").Len())
	assert.Zero(t, recorded.FilterMessage("resolver: opportunistic retry drain deferred under contention").Len())
	assert.Zero(t, recorded.FilterMessage("resolver: opportunistic retry drain refused under contention; keeping tail queued").Len())

	// Once contention clears, a clean save drains both.
	stats = r.ResolveFilesAndIncoming([]string{"other.go"})
	assert.False(t, stats.WriteRefused)
	assert.Equal(t, 0, r.PendingRetryStats().PlaceholderRepoints)
	assert.Equal(t, 0, r.PendingRetryStats().GuardReverts)
	assert.Empty(t, store.GetOutEdges("unresolved::Target"))
	assert.Len(t, store.GetOutEdges("target.go::Target"), 1)

	t.Run("cross_repo_resolver_logging", func(t *testing.T) {
		crStore, crErr := store_sqlite.Open(filepath.Join(t.TempDir(), "step3-cross-repo.sqlite"))
		require.NoError(t, crErr)
		t.Cleanup(func() { require.NoError(t, crStore.Close()) })

		crStore.AddBatch([]*graph.Node{
			{ID: "repoA/old.go", Kind: graph.KindFile, Name: "old.go", FilePath: "repoA/old.go", RepoPrefix: "repoA", WorkspaceID: "ws", Language: "go"},
			{ID: "repoA/old.go::OldCaller", Kind: graph.KindFunction, Name: "OldCaller", FilePath: "repoA/old.go", RepoPrefix: "repoA", WorkspaceID: "ws", Language: "go"},
			{ID: "repoA/other.go", Kind: graph.KindFile, Name: "other.go", FilePath: "repoA/other.go", RepoPrefix: "repoA", WorkspaceID: "ws", Language: "go"},
			{ID: "repoA/caller.go", Kind: graph.KindFile, Name: "caller.go", FilePath: "repoA/caller.go", RepoPrefix: "repoA", WorkspaceID: "ws", Language: "go"},
			{ID: "repoA/caller.go::Run", Kind: graph.KindFunction, Name: "Run", FilePath: "repoA/caller.go", RepoPrefix: "repoA", WorkspaceID: "ws", Language: "go"},
			{ID: "repoB/lib.go", Kind: graph.KindFile, Name: "lib.go", FilePath: "repoB/lib.go", RepoPrefix: "repoB", WorkspaceID: "ws", Language: "go"},
			{ID: "repoB/lib.go::OldTarget", Kind: graph.KindFunction, Name: "OldTarget", FilePath: "repoB/lib.go", RepoPrefix: "repoB", WorkspaceID: "ws", Language: "go"},
			{ID: "repoB/lib.go::CrossTarget", Kind: graph.KindFunction, Name: "CrossTarget", FilePath: "repoB/lib.go", RepoPrefix: "repoB", WorkspaceID: "ws", Language: "go"},
		}, []*graph.Edge{
			{From: "repoA/unresolved::OldTarget", To: "repoA/old.go::OldCaller", Kind: graph.EdgeArgOf, FilePath: "repoA/old.go", Line: 5},
			{From: "repoA/caller.go", To: "repoB/lib.go", Kind: graph.EdgeImports, FilePath: "repoA/caller.go", Line: 1, CrossRepo: true},
			{From: "repoA/caller.go::Run", To: "unresolved::CrossTarget", Kind: graph.EdgeCalls, FilePath: "repoA/caller.go", Line: 4},
		})

		crCore, crRecorded := observer.New(zapcore.InfoLevel)
		crRepoint := graph.PlaceholderRepoint{
			OldFrom: "repoA/unresolved::OldTarget", FilePath: "repoA/old.go", Line: 5, NewFrom: "repoB/lib.go::OldTarget", RepoPrefix: "repoA",
		}

		// 1. Unbridged CrossRepoResolver Step 3 fast-fail emits 1 Warn.
		crUnbridged := NewCrossRepo(crStore)
		crUnbridged.SetLogger(zap.New(crCore))
		crUnbridged.InheritPendingPlaceholderRepoints([]graph.PlaceholderRepoint{crRepoint})
		rel, holdErr := crStore.HoldWriteGate(context.Background())
		require.NoError(t, holdErr)
		crStats := crUnbridged.ResolveMutationFrontiers([]string{"repoA/other.go"}, []string{"repoA/other.go"}, []string{"repoA/other.go"})
		rel()
		assert.False(t, crStats.WriteRefused)
		assert.Equal(t, 1, crRecorded.FilterLevelExact(zapcore.WarnLevel).FilterMessage("cross-repo resolver: opportunistic retry drain refused under contention; keeping tail queued").Len())
		assert.Zero(t, crRecorded.FilterMessage("cross-repo resolver: opportunistic retry drain deferred under contention").Len())

		// 2. Bridged CrossRepoResolver Step 3 fast-fail emits 1 Info and 0 Warn.
		crRecorded.TakeAll()
		savedCR := CrossRepoRetryState{Repoints: []graph.PlaceholderRepoint{crRepoint}}
		crBridged := NewCrossRepo(crStore)
		crBridged.SetLogger(zap.New(crCore))
		crBridged.SetRetryStateBridge(
			func(_ RetryTakeOpts) (CrossRepoRetryState, uint64) {
				taken := savedCR
				savedCR = CrossRepoRetryState{}
				return taken, 1
			},
			func(st CrossRepoRetryState, _ RetryBridgeSaveMeta) { savedCR = st },
			nil,
			func() []string { return []string{"repoA", "repoB"} },
		)
		rel, holdErr = crStore.HoldWriteGate(context.Background())
		require.NoError(t, holdErr)
		crStatsBridged := crBridged.ResolveMutationFrontiers([]string{"repoA/other.go"}, []string{"repoA/other.go"}, []string{"repoA/other.go"})
		rel()
		assert.False(t, crStatsBridged.WriteRefused)
		assert.Len(t, savedCR.Repoints, 1)
		assert.Equal(t, 1, crRecorded.FilterLevelExact(zapcore.InfoLevel).FilterMessage("cross-repo resolver: opportunistic retry drain deferred under contention").Len())
		assert.Zero(t, crRecorded.FilterLevelExact(zapcore.WarnLevel).Len())

		// 3. Bounded CrossRepoResolver WithTimeout pass emits 1 Info on WriteRefused and 0 Step 3 logs.
		crRecorded.TakeAll()
		crBounded := NewCrossRepo(crStore)
		crBounded.SetLogger(zap.New(crCore))
		rel, holdErr = crStore.HoldWriteGate(context.Background())
		require.NoError(t, holdErr)
		crStatsBounded := crBounded.ResolveMutationFrontiersObservedWithTimeout([]string{"repoA/caller.go"}, []string{"repoA/caller.go"}, []string{"repoA/caller.go"}, nil, 50*time.Millisecond)
		rel()
		assert.True(t, crStatsBounded.WriteRefused)
		assert.Equal(t, 1, crRecorded.FilterLevelExact(zapcore.InfoLevel).FilterMessage("cross-repo resolver: bounded incremental pass deferred under contention").Len())
		assert.Zero(t, crRecorded.FilterMessage("cross-repo resolver: opportunistic retry drain deferred under contention").Len())
		assert.Zero(t, crRecorded.FilterMessage("cross-repo resolver: opportunistic retry drain refused under contention; keeping tail queued").Len())
	})
}

func TestFrontierGuardRevertMatchesWildcardAndSlashPrefixedRepoints(t *testing.T) {
	store := openRefusingReindexStore(t)
	store.AddBatch([]*graph.Node{
		{ID: "repoA/a/caller.go", Kind: graph.KindFile, Name: "caller.go", FilePath: "repoA/a/caller.go", RepoPrefix: "repoA", Language: "go"},
		{ID: "repoA/a/caller.go::Run", Kind: graph.KindFunction, Name: "Run", FilePath: "repoA/a/caller.go", RepoPrefix: "repoA", Language: "go"},
		{ID: "repoA/b/decoy.go", Kind: graph.KindFile, Name: "decoy.go", FilePath: "repoA/b/decoy.go", RepoPrefix: "repoA", Language: "go"},
		{ID: "repoA/b/decoy.go::Foo", Kind: graph.KindFunction, Name: "Foo", FilePath: "repoA/b/decoy.go", RepoPrefix: "repoA", Language: "go"},
		{ID: "repoA/b/decoy.go::Bar", Kind: graph.KindFunction, Name: "Bar", FilePath: "repoA/b/decoy.go", RepoPrefix: "repoA", Language: "go"},
		{ID: "repoA/c/imported.go", Kind: graph.KindFile, Name: "imported.go", FilePath: "repoA/c/imported.go", RepoPrefix: "repoA", Language: "go"},
	}, []*graph.Edge{
		{From: "repoA/a/caller.go", To: "repoA/c/imported.go", Kind: graph.EdgeImports, FilePath: "repoA/a/caller.go", Line: 1},
		{From: "repoA/a/caller.go::Run", To: "repoA/b/decoy.go::Foo", Kind: graph.EdgeCalls, FilePath: "repoA/a/caller.go", Line: 10, Confidence: 0.4, Origin: graph.OriginTextMatched},
		{From: "repoA/b/decoy.go::Foo", To: "repoA/a/caller.go::Run", Kind: graph.EdgeArgOf, FilePath: "repoA/a/caller.go", Line: 10},
		{From: "repoA/b/decoy.go::Bar", To: "repoA/a/caller.go::Run", Kind: graph.EdgeArgOf, FilePath: "repoA/a/caller.go", Line: 20},
	})

	r := New(store)
	// Queue wildcard + slash-prefixed deferred repoints at the same sites, plus both
	// reindexesApplied==false (wildcard Foo) and reindexesApplied==true (slash-prefixed Bar) guard reverts.
	r.InheritPendingPlaceholderRepoints([]graph.PlaceholderRepoint{
		{OldFrom: "unresolved::*.Foo", FilePath: "repoA/a/caller.go", Line: 10, NewFrom: "repoA/b/decoy.go::Foo", RepoPrefix: "repoA"},
		{OldFrom: "repoA/unresolved::Foo", FilePath: "repoA/a/caller.go", Line: 10, NewFrom: "repoA/b/decoy.go::Foo", RepoPrefix: "repoA"},
		{OldFrom: "repoA/unresolved::*.Bar", FilePath: "repoA/a/caller.go", Line: 20, NewFrom: "repoA/b/decoy.go::Bar", RepoPrefix: "repoA"},
	})
	r.InheritPendingGuardReverts([]PendingGuardRevert{
		{
			RepoPrefix: "repoA",
			Reindexes: []graph.EdgeReindex{{
				Edge:  &graph.Edge{From: "repoA/a/caller.go::Run", To: "unresolved::*.Foo", Kind: graph.EdgeCalls, FilePath: "repoA/a/caller.go", Line: 10, Meta: map[string]any{"guard_reverted": true}},
				OldTo: "repoA/b/decoy.go::Foo",
			}},
			SourceReverts: []graph.PlaceholderRepoint{
				{OldFrom: "repoA/b/decoy.go::Foo", FilePath: "repoA/a/caller.go", Line: 10, NewFrom: "repoA/unresolved::*.Foo", RepoPrefix: "repoA"},
			},
		},
		{
			RepoPrefix:       "repoA",
			reindexesApplied: true,
			SourceReverts: []graph.PlaceholderRepoint{
				{OldFrom: "repoA/b/decoy.go::Bar", FilePath: "repoA/a/caller.go", Line: 20, NewFrom: "repoA/unresolved::Bar", RepoPrefix: "repoA"},
			},
		},
	})

	stats := r.ResolveFilesAndIncoming([]string{"repoA/a/caller.go"})
	assert.False(t, stats.WriteRefused)
	assert.Equal(t, 0, r.PendingRetryStats().GuardReverts)
	assert.Equal(t, 0, r.PendingRetryStats().PlaceholderRepoints, "all wildcard and slash-prefixed deferred repoints matching guard reverts must be pruned")
	assert.Empty(t, store.GetOutEdges("repoA/b/decoy.go::Foo"), "wildcard guard revert must move Foo's dataflow source back to placeholder")
	assert.Empty(t, store.GetOutEdges("repoA/b/decoy.go::Bar"), "reindexesApplied==true SourceReverts must move Bar's dataflow source back to placeholder")
	assert.Len(t, store.GetOutEdges("repoA/unresolved::*.Foo"), 1)
	assert.Len(t, store.GetOutEdges("repoA/unresolved::Bar"), 1)
}

type chunk2RefusingMemStore struct {
	*graph.Graph
	refused int
}

func (s *chunk2RefusingMemStore) ReindexEdgesTryApplied(batch []graph.EdgeReindex, _ time.Duration) bool {
	if len(batch) == 2 {
		s.refused++
		return false
	}
	s.ReindexEdges(batch)
	return true
}

func (s *chunk2RefusingMemStore) ReindexEdgesApplied(batch []graph.EdgeReindex) bool {
	return s.ReindexEdgesTryApplied(batch, 0)
}

func TestReconcilePlaceholderSourcesRefusedChunkDoesNotCorruptInMemoryGraph(t *testing.T) {
	mem := graph.New()
	const total = 2050
	var repoints []graph.PlaceholderRepoint
	for i := 0; i < total; i++ {
		oldFrom := fmt.Sprintf("unresolved::fn_%04d", i)
		newFrom := fmt.Sprintf("t.go::fn_%04d", i)
		mem.AddEdge(&graph.Edge{
			From:     oldFrom,
			To:       "caller.go::Run",
			Kind:     graph.EdgeArgOf,
			FilePath: "caller.go",
			Line:     i + 1,
		})
		repoints = append(repoints, graph.PlaceholderRepoint{
			OldFrom:  oldFrom,
			FilePath: "caller.go",
			Line:     i + 1,
			NewFrom:  newFrom,
		})
	}

	wrap := &chunk2RefusingMemStore{Graph: mem}
	_, ok := graph.ReconcilePlaceholderSourcesApplied(wrap, repoints)
	assert.False(t, ok)
	assert.Equal(t, 1, wrap.refused)

	// Chunk 2's 2 edges must remain completely uncorrupted under their original From buckets.
	for i := 2048; i < total; i++ {
		oldFrom := fmt.Sprintf("unresolved::fn_%04d", i)
		out := mem.GetOutEdges(oldFrom)
		require.Len(t, out, 1, "refused chunk 2 edge %d must stay in original adjacency bucket", i)
		assert.Equal(t, oldFrom, out[0].From, "refused chunk 2 edge %d must not have its From mutated in place", i)
	}
}

func TestClearedGuardRevertedEdgeStillProtectedAfterLaterRestub(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			var store graph.Store
			if backend == "memory" {
				store = graph.New()
			} else {
				s, err := store_sqlite.Open(filepath.Join(t.TempDir(), "restub.sqlite"))
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, s.Close()) })
				store = s
			}

			producerNode := &graph.Node{ID: "repoA/b/producer.go::Produce", Kind: graph.KindFunction, Name: "Produce", FilePath: "repoA/b/producer.go", RepoPrefix: "repoA", WorkspaceID: "ws", Language: "go"}
			store.AddBatch([]*graph.Node{
				{ID: "repoA/a/caller.go", Kind: graph.KindFile, Name: "caller.go", FilePath: "repoA/a/caller.go", RepoPrefix: "repoA", WorkspaceID: "ws", Language: "go"},
				{ID: "repoA/a/caller.go::Run", Kind: graph.KindFunction, Name: "Run", FilePath: "repoA/a/caller.go", RepoPrefix: "repoA", WorkspaceID: "ws", Language: "go"},
				{ID: "repoA/b/producer.go", Kind: graph.KindFile, Name: "producer.go", FilePath: "repoA/b/producer.go", RepoPrefix: "repoA", WorkspaceID: "ws", Language: "go"},
				{ID: "repoA/c/decoy.go", Kind: graph.KindFile, Name: "decoy.go", FilePath: "repoA/c/decoy.go", RepoPrefix: "repoA", WorkspaceID: "ws", Language: "go"},
				{ID: "repoA/c/decoy.go::Produce", Kind: graph.KindFunction, Name: "Produce", FilePath: "repoA/c/decoy.go", RepoPrefix: "repoA", WorkspaceID: "ws", Language: "go"},
			}, []*graph.Edge{
				{From: "repoA/a/caller.go", To: "repoA/b/producer.go", Kind: graph.EdgeImports, FilePath: "repoA/a/caller.go", Line: 1},
				{From: "repoA/a/caller.go::Run", To: "unresolved::Produce", Kind: graph.EdgeCalls, FilePath: "repoA/a/caller.go", Line: 5},
			})

			// Step 1: producer.go::Produce absent -> guard reverts weak bind to decoy and stamps guard_reverted=true.
			r := New(store)
			_, err := r.ResolveAllContext(context.Background())
			require.NoError(t, err)
			edges := store.GetOutEdges("repoA/a/caller.go::Run")
			require.Len(t, edges, 1)
			assert.Equal(t, "unresolved::Produce", edges[0].To)
			assert.Equal(t, true, edges[0].Meta["guard_reverted"])

			// Step 2: restore producer.go::Produce -> rebind clears guard_reverted.
			store.AddNode(producerNode)
			_, err = r.ResolveAllContext(context.Background())
			require.NoError(t, err)
			edges = store.GetOutEdges("repoA/a/caller.go::Run")
			require.Len(t, edges, 1)
			assert.Equal(t, "repoA/b/producer.go::Produce", edges[0].To)
			assert.Nil(t, edges[0].Meta["guard_reverted"], "concrete rebind must clear stale guard_reverted")

			// Step 3: withdraw producer.go::Produce and restub edge via StashRestubProvenance.
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
				store.ReindexEdges([]graph.EdgeReindex{{Edge: &prevEdge, OldTo: cur[0].To}})
			} else {
				store.AddEdge(&prevEdge)
			}

			// Step 4: per-repo resolve re-stamps guard_reverted=true before CrossRepoResolver runs.
			stats := r.ResolveFilesAndIncoming([]string{"repoA/a/caller.go"})
			assert.False(t, stats.WriteRefused)
			edges = store.GetOutEdges("repoA/a/caller.go::Run")
			require.Len(t, edges, 1)
			assert.Equal(t, "unresolved::Produce", edges[0].To)
			assert.Equal(t, true, edges[0].Meta["guard_reverted"])

			// Step 5: CrossRepoResolver must respect re-stamped guard_reverted and refuse unimported decoy.
			_, err = NewCrossRepo(store).ResolveAllContext(context.Background())
			require.NoError(t, err)
			edges = store.GetOutEdges("repoA/a/caller.go::Run")
			require.Len(t, edges, 1)
			assert.Equal(t, "unresolved::Produce", edges[0].To, "CrossRepoResolver must not bind unimported same-repo decoy after restub+guard")
			assert.Equal(t, true, edges[0].Meta["guard_reverted"])
		})
	}
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

func BenchmarkFirstSaveAfterRefusedFullPass(b *testing.B) {
	const (
		bgFiles            = 100
		bgEdgesPerFile     = 100 // 10,000 background arg_of edges
		queuedRepointCount = 400
		queuedGuardCount   = 400
	)

	tmplDir := b.TempDir()
	tmplDB := filepath.Join(tmplDir, "template.sqlite")
	store, err := store_sqlite.Open(tmplDB)
	if err != nil {
		b.Fatal(err)
	}

	nodes := make([]*graph.Node, 0, bgFiles*2+queuedRepointCount*2+queuedGuardCount*2+4)
	edges := make([]*graph.Edge, 0, bgFiles*bgEdgesPerFile+queuedRepointCount+queuedGuardCount+1)
	repoints := make([]graph.PlaceholderRepoint, 0, queuedRepointCount)
	guardReindexes := make([]graph.EdgeReindex, 0, queuedGuardCount)

	for f := 0; f < bgFiles; f++ {
		fp := fmt.Sprintf("bg/f%03d.go", f)
		callerID := fp + "::fn"
		nodes = append(nodes,
			&graph.Node{ID: fp, Kind: graph.KindFile, Name: fmt.Sprintf("f%03d.go", f), FilePath: fp, Language: "go"},
			&graph.Node{ID: callerID, Kind: graph.KindFunction, Name: "fn", FilePath: fp, Language: "go"},
		)
		for e := 0; e < bgEdgesPerFile; e++ {
			edges = append(edges, &graph.Edge{
				From:     fmt.Sprintf("bg/t%03d.go::arg_%03d", f, e),
				To:       callerID,
				Kind:     graph.EdgeArgOf,
				FilePath: fp,
				Line:     e + 1,
			})
		}
	}

	for i := 0; i < queuedRepointCount; i++ {
		fp := fmt.Sprintf("pkg/f%03d.go", i)
		tfp := fmt.Sprintf("pkg/t%03d.go", i)
		fnName := fmt.Sprintf("fn_%03d", i)
		callerID := fp + "::caller"
		targetID := tfp + "::" + fnName
		oldFrom := graph.UnresolvedMarker + fnName
		nodes = append(nodes,
			&graph.Node{ID: fp, Kind: graph.KindFile, Name: fmt.Sprintf("f%03d.go", i), FilePath: fp, Language: "go"},
			&graph.Node{ID: callerID, Kind: graph.KindFunction, Name: "caller", FilePath: fp, Language: "go"},
			&graph.Node{ID: tfp, Kind: graph.KindFile, Name: fmt.Sprintf("t%03d.go", i), FilePath: tfp, Language: "go"},
			&graph.Node{ID: targetID, Kind: graph.KindFunction, Name: fnName, FilePath: tfp, Language: "go"},
		)
		edges = append(edges, &graph.Edge{
			From:     oldFrom,
			To:       callerID,
			Kind:     graph.EdgeArgOf,
			FilePath: fp,
			Line:     10,
		})
		repoints = append(repoints, graph.PlaceholderRepoint{
			OldFrom:  oldFrom,
			FilePath: fp,
			Line:     10,
			NewFrom:  targetID,
		})
	}

	for i := 0; i < queuedGuardCount; i++ {
		fp := fmt.Sprintf("pkg/g%03d.go", i)
		dfp := fmt.Sprintf("unimp/d%03d.go", i)
		fnName := fmt.Sprintf("g_%03d", i)
		callerID := fp + "::caller"
		decoyID := dfp + "::" + fnName
		nodes = append(nodes,
			&graph.Node{ID: fp, Kind: graph.KindFile, Name: fmt.Sprintf("g%03d.go", i), FilePath: fp, Language: "go"},
			&graph.Node{ID: callerID, Kind: graph.KindFunction, Name: "caller", FilePath: fp, Language: "go"},
			&graph.Node{ID: dfp, Kind: graph.KindFile, Name: fmt.Sprintf("d%03d.go", i), FilePath: dfp, Language: "go"},
			&graph.Node{ID: decoyID, Kind: graph.KindFunction, Name: fnName, FilePath: dfp, Language: "go"},
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

	nodes = append(nodes,
		&graph.Node{ID: "save/edit.go", Kind: graph.KindFile, Name: "edit.go", FilePath: "save/edit.go", Language: "go"},
		&graph.Node{ID: "save/edit.go::Run", Kind: graph.KindFunction, Name: "Run", FilePath: "save/edit.go", Language: "go"},
		&graph.Node{ID: "save/helper.go", Kind: graph.KindFile, Name: "helper.go", FilePath: "save/helper.go", Language: "go"},
		&graph.Node{ID: "save/helper.go::SaveHelper", Kind: graph.KindFunction, Name: "SaveHelper", FilePath: "save/helper.go", Language: "go"},
	)
	edges = append(edges, &graph.Edge{
		From:     "save/edit.go::Run",
		To:       graph.UnresolvedMarker + "SaveHelper",
		Kind:     graph.EdgeCalls,
		FilePath: "save/edit.go",
		Line:     5,
	})

	store.AddBatch(nodes, edges)
	if err := store.CheckpointWAL(); err != nil {
		b.Fatal(err)
	}
	if err := store.Close(); err != nil {
		b.Fatal(err)
	}
	_ = os.Remove(tmplDB + "-wal")
	_ = os.Remove(tmplDB + "-shm")

	cases := []struct {
		name        string
		withBacklog bool
	}{
		{name: "clean_baseline", withBacklog: false},
		{name: "first_same_repo_save_after_refused_full_pass", withBacklog: true},
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
				if i == 0 && s.GetNode("save/edit.go::Run") == nil {
					_ = s.Close()
					b.Fatal("template DB missing expected nodes after checkpoint")
				}
				r := New(s)
				if tc.withBacklog {
					rpCopy := append([]graph.PlaceholderRepoint(nil), repoints...)
					grCopy := append([]graph.EdgeReindex(nil), guardReindexes...)
					r.InheritPendingPlaceholderRepoints(rpCopy)
					r.InheritPendingGuardReverts([]PendingGuardRevert{{Reindexes: grCopy}})
				}
				b.StartTimer()
				stats := r.ResolveFilesAndIncoming([]string{"save/edit.go"})
				b.StopTimer()
				if stats == nil || stats.WriteRefused || stats.Resolved == 0 {
					_ = s.Close()
					b.Fatalf("expected clean resolve of save/edit.go, got %+v", stats)
				}
				if closeErr := s.Close(); closeErr != nil {
					b.Fatal(closeErr)
				}
				removeBenchmarkDBFiles(iterDB)
			}
		})
	}
}

