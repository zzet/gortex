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
	refuseNext       bool
	refuseFunc       func(call int, batch []graph.EdgeReindex) bool
	refuseTargetFunc func(call int, batch []graph.UnresolvedEdgeTargetReindex) bool
	calls            int
	targetCalls      int
	refused          int
}

func (s *refusingReindexStore) ReindexEdges(batch []graph.EdgeReindex) {
	s.ReindexEdgesApplied(batch)
}

func (s *refusingReindexStore) ReindexEdgesApplied(batch []graph.EdgeReindex) bool {
	s.calls++
	if s.refuseFunc != nil && s.refuseFunc(s.calls, batch) {
		s.refused++
		return false
	}
	if s.refuseNext {
		s.refuseNext = false
		s.refused++
		return false
	}
	return s.Store.ReindexEdgesApplied(batch)
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
	if s.refuseNext {
		s.refuseNext = false
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

func TestRefusedOutgoingIncrementalDoesNotLeakMutatedFrontierIntoAttribution(t *testing.T) {
	for _, mode := range []string{"files_and_incoming", "files_outgoing"} {
		t.Run(mode, func(t *testing.T) {
			store := openRefusingReindexStore(t)
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
	store.refuseNext = false
	store.refuseFunc = func(call int, _ []graph.EdgeReindex) bool {
		return call == 2 // chunk 1 commits, chunk 2 is refused
	}
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
	store.refuseNext = false
	store.refuseFunc = func(call int, _ []graph.EdgeReindex) bool {
		return call == 2 // chunk 1 commits call, applyDeferredApplied is refused
	}
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
	edges := store.GetOutEdges("pkg/shadow.go::F")
	require.Len(t, edges, 1)
	assert.Equal(t, "unresolved::len", edges[0].To, "refused bare-name bind must halt before attributeGoBuiltins misbinds shadowed len")

	stats, err = r.ResolveAllContext(context.Background())
	require.NoError(t, err)
	assert.False(t, stats.WriteRefused)
	edges = store.GetOutEdges("pkg/shadow.go::F")
	require.Len(t, edges, 1)
	assert.Equal(t, "pkg/shadow.go::F#local:len", edges[0].To)
}

func TestRefusedCrossPackageGuardRevertCompletesOnRetry(t *testing.T) {
	store := openRefusingReindexStore(t)
	store.refuseNext = false
	store.refuseFunc = func(call int, _ []graph.EdgeReindex) bool {
		return call == 2 // phase 1 commits weak-tier call, guard revert is refused
	}
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
	store.refuseNext = false
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

	callsBefore := store.calls
	store.refuseFunc = func(call int, _ []graph.EdgeReindex) bool {
		return call == callsBefore+2 // call #1 commits EdgeCalls, call #2 refuses placeholder move
	}
	stats := r.ResolveFilesAndIncoming([]string{"inc.ts"})
	assert.True(t, stats.WriteRefused)
	assert.Len(t, store.GetOutEdges("unresolved::incHelper"), 1)

	store.refuseFunc = nil
	stats = r.ResolveFilesAndIncoming([]string{"inc.ts"})
	assert.False(t, stats.WriteRefused)
	assert.Empty(t, store.GetOutEdges("unresolved::incHelper"), "reused resolver must rebuild placeholder census instead of erasing retained repoints")
	moved := store.GetOutEdges("inc_target.ts::incHelper")
	require.Len(t, moved, 1)
	assert.Equal(t, graph.EdgeArgOf, moved[0].Kind)
}

func TestIncrementalRetryDoesNotBindDataflowOntoGuardRejectedTarget(t *testing.T) {
	store := openRefusingReindexStore(t)
	store.refuseNext = false
	store.refuseFunc = func(call int, _ []graph.EdgeReindex) bool {
		return call == 2 // call #1 commits weak-tier call, call #2 refuses reconcilePlaceholderSourcesApplied
	}
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
	store.refuseNext = false
	store.refuseFunc = func(call int, _ []graph.EdgeReindex) bool {
		return call == 3 // #1 commits call, #2 moves placeholder From, #3 refuses guard revert
	}
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
	store.refuseNext = false
	store.refuseFunc = func(call int, _ []graph.EdgeReindex) bool {
		return call == 2 // #1 commits cross-repo call, #2 refuses reconcilePlaceholderSourcesApplied
	}
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
	store.refuseNext = false
	store.refuseFunc = func(call int, _ []graph.EdgeReindex) bool {
		return call == 2 // #1 commits weak-tier promotion to b/pkg.go::min, #2 refuses guard revert
	}
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
	store.refuseNext = false
	store.refuseFunc = func(call int, _ []graph.EdgeReindex) bool {
		return call == 2 // chunk 1 commits weak-tier cross-pkg call, chunk 2 is refused before guard runs
	}
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
	assert.Equal(t, 2, store.calls, "draining guard spool on refusal must not fire additional writes into contended gate")

	store.refuseFunc = nil
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
	store.refuseNext = false
	store.refuseFunc = func(call int, _ []graph.EdgeReindex) bool {
		return call == 2 // #1 commits unresolved::helper, #2 refuses attributeNonGoModuleImports on external::requests
	}
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
	store.refuseNext = false
	store.refuseFunc = func(call int, _ []graph.EdgeReindex) bool {
		return call == 2 // chunk 1 (2048 calls) commits, chunk 2 (2 calls) is refused
	}
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
	store.refuseNext = false
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
	take := func() MasterRetryState {
		bridgeMu.Lock()
		defer bridgeMu.Unlock()
		out := bridge
		bridge = MasterRetryState{}
		return out
	}
	save := func(s MasterRetryState) {
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
	m1.SetRetryStateBridge(take, save)
	interleaved := false
	m1.chunkYieldHook = func() {
		if interleaved {
			return
		}
		interleaved = true
		m2 := New(store)
		m2.SetRetryStateBridge(take, save)
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
	store.refuseNext = false
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
		{OldFrom: "b/unresolved::helper", FilePath: "b/caller.ts", Line: 2, NewFrom: "c/target.ts::helper"},
	}
	take := func() []graph.PlaceholderRepoint {
		out := queued
		queued = nil
		return out
	}
	save := func(repoints []graph.PlaceholderRepoint) {
		queued = append(queued, repoints...)
	}

	// Refuse call #2 (chunk 1 reindex) while allowing call #1 (initial flush of inherited repoint).
	store.refuseFunc = func(call int, _ []graph.EdgeReindex) bool {
		return call == 2
	}
	cr := NewCrossRepo(store)
	cr.SetRetryStateBridge(take, save)
	_, err := cr.ResolveAllContext(context.Background())
	require.ErrorIs(t, err, ErrReindexWriteRefused)
	assert.Empty(t, store.GetOutEdges("b/unresolved::helper"), "inherited repoint must be flushed before streaming/chunking")
	assert.Len(t, store.GetOutEdges("c/target.ts::helper"), 1)
	assert.Empty(t, queued)
}

func TestScopedResolveAllPreservesOtherScopeAndUnscopedTailRetry(t *testing.T) {
	store := openRefusingReindexStore(t)
	store.refuseNext = false
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
	take := func() MasterRetryState {
		out := bridge
		bridge = MasterRetryState{}
		return out
	}
	save := func(s MasterRetryState) {
		bridge.PlaceholderRepoints = append(bridge.PlaceholderRepoints, s.PlaceholderRepoints...)
		bridge.GuardReverts = append(bridge.GuardReverts, s.GuardReverts...)
		bridge.TailRetryAll = bridge.TailRetryAll || s.TailRetryAll
		bridge.TailRetryScopes = append(bridge.TailRetryScopes, s.TailRetryScopes...)
		bridge.AttributionFiles = append(bridge.AttributionFiles, s.AttributionFiles...)
	}

	// repoB-scoped ResolveAllContext must NOT clear unscoped TailRetryAll or repoA state.
	rB := New(store)
	rB.SetScope(map[string]struct{}{"repoB": {}})
	rB.SetRetryStateBridge(take, save)
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
	rA.SetRetryStateBridge(take, save)
	_, err = rA.ResolveAllContext(context.Background())
	require.NoError(t, err)
	assert.NotNil(t, store.GetNode("stdlib::fmt::Println"), "repoA-scoped retry must complete repoA go_attribution")
	assert.True(t, bridge.TailRetryAll, "repoA-scoped pass must still preserve unscoped TailRetryAll")
	assert.Empty(t, bridge.TailRetryScopes)
	assert.Empty(t, bridge.AttributionFiles)

	// Unscoped ResolveAllContext with zero pending edges must clear TailRetryAll.
	rAll := New(store)
	rAll.SetStampTerminal(true)
	rAll.SetRetryStateBridge(take, save)
	_, err = rAll.ResolveAllContext(context.Background())
	require.NoError(t, err)
	assert.False(t, bridge.TailRetryAll)
	assert.Empty(t, bridge.TailRetryScopes)
	assert.Empty(t, bridge.AttributionFiles)
}

func TestResolveAllWithZeroPendingDrainsPendingAttribFiles(t *testing.T) {
	store := openRefusingReindexStore(t)
	store.refuseNext = false
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
			store.refuseNext = false
			store.refuseFunc = func(call int, _ []graph.EdgeReindex) bool {
				return call == 2 // #1 commits outgoing reindex (including stdlib::fmt::Println), #2 refuses reconcilePlaceholderSourcesApplied before attribution
			}
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
		store.refuseNext = false

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
		// Call #1 = chunk 1 reindex (2048), call #2 = chunk 1 source reverts, call #3 = chunk 2 reindex (2).
		store.refuseFunc = func(call int, _ []graph.EdgeReindex) bool {
			return call == 3
		}
		var refused bool
		restore := r.trackPassWriteRefused(&refused)
		reverted := r.guardCrossPackageCallEdges(jobs, closure)
		restore()
		assert.True(t, refused)
		assert.Equal(t, guardRevertChunkSize, reverted)
		assert.Len(t, store.GetOutEdges("b/unimported.go::Secret"), 2)
		assert.Len(t, store.GetOutEdges("unresolved::Secret"), guardRevertChunkSize)

		// Flush via an unrelated file so a/caller.go is NEVER re-resolved or re-guarded:
		// ONLY flushPendingGuardRevertsLocked can revert Chunk 2's 2 calls and 2 SourceReverts.
		store.refuseFunc = nil
		stats := r.ResolveFilesAndIncoming([]string{"other/noop.go"})
		assert.False(t, stats.WriteRefused)
		assert.Empty(t, store.GetOutEdges("b/unimported.go::Secret"), "flushPendingGuardRevertsLocked must revert chunk 2's remaining 2 SourceReverts")
		assert.Len(t, store.GetOutEdges("unresolved::Secret"), totalCalls)
		for _, e := range store.GetOutEdges("a/caller.go::Run") {
			assert.Equal(t, "unresolved::Secret", e.To)
		}
	})

	t.Run("flush_pending_multi_chunk_refuses_chunk2_then_completes_via_unrelated_file", func(t *testing.T) {
		store, r, jobs, closure, totalCalls := setup(t)
		// Refuse call #1 in guardCrossPackageCallEdges so all 2050 items (> guardRevertChunkSize)
		// are queued in r.pendingGuardReverts.
		store.refuseFunc = func(call int, _ []graph.EdgeReindex) bool {
			return call == 1
		}
		var refused bool
		restore := r.trackPassWriteRefused(&refused)
		reverted := r.guardCrossPackageCallEdges(jobs, closure)
		restore()
		assert.True(t, refused)
		assert.Equal(t, 0, reverted)
		assert.Len(t, store.GetOutEdges("b/unimported.go::Secret"), totalCalls)

		// Now exercise flushPendingGuardRevertsLocked's multi-chunk loop:
		// reset call count, allow chunk 1 reindex (#1) + chunk 1 SourceReverts (#2), refuse chunk 2 reindex (#3).
		store.calls = 0
		store.refuseFunc = func(call int, _ []graph.EdgeReindex) bool {
			return call == 3
		}
		stats := r.ResolveFilesAndIncoming([]string{"other/noop.go"})
		assert.True(t, stats.WriteRefused)
		assert.Len(t, store.GetOutEdges("b/unimported.go::Secret"), 2, "flushPendingGuardRevertsLocked must commit chunk 1's 2048 SourceReverts before chunk 2 refusal")
		assert.Len(t, store.GetOutEdges("unresolved::Secret"), guardRevertChunkSize)

		// Second unrelated-file flush completes the remaining 2 reindexes + 2 SourceReverts.
		store.refuseFunc = nil
		stats = r.ResolveFilesAndIncoming([]string{"other/noop.go"})
		assert.False(t, stats.WriteRefused)
		assert.Empty(t, store.GetOutEdges("b/unimported.go::Secret"))
		assert.Len(t, store.GetOutEdges("unresolved::Secret"), totalCalls)
	})

	t.Run("flush_pending_chunk1_source_revert_refused_retains_chunk1_sources_and_chunk2_reindexes", func(t *testing.T) {
		store, r, jobs, closure, totalCalls := setup(t)
		store.refuseFunc = func(call int, _ []graph.EdgeReindex) bool {
			return call == 1
		}
		var refused bool
		restore := r.trackPassWriteRefused(&refused)
		_ = r.guardCrossPackageCallEdges(jobs, closure)
		restore()
		require.True(t, refused)

		// Refuse call #2 (ReconcilePlaceholderSourcesApplied for chunk 1) inside flushPendingGuardRevertsLocked.
		store.calls = 0
		store.refuseFunc = func(call int, _ []graph.EdgeReindex) bool {
			return call == 2
		}
		stats := r.ResolveFilesAndIncoming([]string{"other/noop.go"})
		assert.True(t, stats.WriteRefused)
		assert.Len(t, store.GetOutEdges("b/unimported.go::Secret"), totalCalls, "chunk 1 source reverts were refused and must remain queued")

		store.refuseFunc = nil
		stats = r.ResolveFilesAndIncoming([]string{"other/noop.go"})
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
		store.refuseNext = false
		store.refuseFunc = func(call int, _ []graph.EdgeReindex) bool {
			return call == 2 // chunk 1 commits weak-tier call, chunk 2 refuses before compute completes
		}
		store.AddBatch([]*graph.Node{
			{ID: "repoA/a/caller.go", Kind: graph.KindFile, Name: "caller.go", FilePath: "repoA/a/caller.go", RepoPrefix: "repoA", WorkspaceID: "ws", Language: "go"},
			{ID: "repoA/a/caller.go::Run", Kind: graph.KindFunction, Name: "Run", FilePath: "repoA/a/caller.go", RepoPrefix: "repoA", WorkspaceID: "ws", Language: "go"},
			{ID: "repoA/b/unimported.go", Kind: graph.KindFile, Name: "unimported.go", FilePath: "repoA/b/unimported.go", RepoPrefix: "repoA", WorkspaceID: "ws", Language: "go"},
			{ID: "repoA/b/unimported.go::Secret", Kind: graph.KindFunction, Name: "Secret", FilePath: "repoA/b/unimported.go", RepoPrefix: "repoA", WorkspaceID: "ws", Language: "go"},
			{ID: "repoA/c/imported.go", Kind: graph.KindFile, Name: "imported.go", FilePath: "repoA/c/imported.go", RepoPrefix: "repoA", WorkspaceID: "ws", Language: "go"},
			{ID: "repoA/c/imported.go::Allowed", Kind: graph.KindFunction, Name: "Allowed", FilePath: "repoA/c/imported.go", RepoPrefix: "repoA", WorkspaceID: "ws", Language: "go"},
			{ID: "repoB/noop.go", Kind: graph.KindFile, Name: "noop.go", FilePath: "repoB/noop.go", RepoPrefix: "repoB", WorkspaceID: "ws", Language: "go"},
		}, []*graph.Edge{
			{From: "repoA/a/caller.go", To: "repoA/c/imported.go", Kind: graph.EdgeImports, FilePath: "repoA/a/caller.go", Line: 1},
			{From: "repoA/a/caller.go::Run", To: "unresolved::Secret", Kind: graph.EdgeCalls, FilePath: "repoA/a/caller.go", Line: 5},
			{From: "repoA/a/caller.go::Run", To: "unresolved::Allowed", Kind: graph.EdgeCalls, FilePath: "repoA/a/caller.go", Line: 6},
		})

		r := New(store)
		_, err := r.ResolveAllContext(context.Background())
		require.ErrorIs(t, err, ErrReindexWriteRefused)

		// Sibling repoB flushes pending guard reverts and runs whole-graph CrossRepoResolver
		// before repoA retries: guard_reverted must block CrossRepoResolver from permanently
		// binding repoA/a/caller.go::Run to unimported same-repo decoy repoA/b/unimported.go::Secret.
		store.refuseFunc = nil
		stats := r.ResolveFilesAndIncoming([]string{"repoB/noop.go"})
		assert.False(t, stats.WriteRefused)
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
		store.refuseNext = false
		store.refuseFunc = func(call int, _ []graph.EdgeReindex) bool {
			return call == 2 // chunk 1 commits call, chunk 2 refuses import edge
		}
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
		store.refuseNext = false
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
		store.refuseNext = false
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
	store.refuseNext = false
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
	take := func() []graph.PlaceholderRepoint {
		bridgeMu.Lock()
		defer bridgeMu.Unlock()
		out := queued
		queued = nil
		return out
	}
	save := func(repoints []graph.PlaceholderRepoint) {
		bridgeMu.Lock()
		defer bridgeMu.Unlock()
		queued = append(queued, repoints...)
	}

	cr1 := NewCrossRepo(store)
	cr1.SetRetryStateBridge(take, save)
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
		cr2.SetRetryStateBridge(take, save)
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
	store.refuseNext = false
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
	// Call #1 commits chunk 1 (Secret); chunkYieldHook cancels ctx AND arms refusal
	// on call #2 so resolveError's applyDeferredApplied is refused.
	r := New(store)
	r.chunkYieldHook = func() {
		store.refuseFunc = func(call int, _ []graph.EdgeReindex) bool { return call == 2 }
		cancel()
	}

	stats, err := r.ResolveAllContext(ctx)
	require.ErrorIs(t, err, context.Canceled)
	assert.True(t, stats.WriteRefused, "refused applyDeferredApplied inside cancel resolveError must set WriteRefused")

	store.refuseFunc = nil
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

