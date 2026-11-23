package resolver

import (
	"context"
	"path/filepath"
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
				{ID: "b/other.go::T.Foo", Kind: graph.KindMethod, Name: "Foo", FilePath: "b/other.go", Language: "go"},
				{ID: "c/other.go", Kind: graph.KindFile, Name: "other.go", FilePath: "c/other.go", Language: "go"},
				{ID: "c/other.go::Dep", Kind: graph.KindFunction, Name: "Dep", FilePath: "c/other.go", Language: "go"},
			}, []*graph.Edge{
				{From: "a/caller.go", To: "c/other.go", Kind: graph.EdgeImports, FilePath: "a/caller.go", Line: 1},
				{From: "a/caller.go::Run", To: "unresolved::*.Foo", Kind: graph.EdgeCalls, FilePath: "a/caller.go", Line: 10},
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
