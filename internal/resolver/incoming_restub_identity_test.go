package resolver

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// The per-save incoming leg must leave a reference in the row a whole index
// of the same tree holds. These tests pin the three places it did not.

func openIdentitySQLite(t *testing.T) graph.Store {
	t.Helper()
	store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "identity.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// storedOutEdge re-reads the one out-edge of from with the given kind from the
// store: a disk store answers with fresh copies, so this is the persisted row.
func storedOutEdge(t *testing.T, g graph.Store, from string, kind graph.EdgeKind) *graph.Edge {
	t.Helper()
	var found []*graph.Edge
	for _, e := range g.GetOutEdges(from) {
		if e.Kind == kind {
			found = append(found, e)
		}
	}
	require.Len(t, found, 1)
	return found[0]
}

// restubbedCall is a reference the re-parse of the declaring file parked: it
// was bound to prevTo, the restub stashed that provenance and moved it to the
// bare name stub.
func restubbedCall(from, file string, line int, name, prevTo, prevOrigin string, prevConf float64) *graph.Edge {
	e := &graph.Edge{From: from, To: prevTo, Kind: graph.EdgeCalls, FilePath: file, Line: line,
		Origin: prevOrigin, Confidence: prevConf, Meta: map[string]any{"return_usage": "returned"}}
	graph.StashRestubProvenance(e)
	e.To = graph.UnresolvedMarker + name
	return e
}

// A definition renamed away (or deleted) leaves its referrers parked under a
// name nothing declares any more. A whole index leaves such a reference
// unresolved with no restub bookkeeping. The incoming leg must reach it (the
// name is no longer one the changed file declares) and persist the dropped
// stash — on a store that answers reads with copies, the in-place drop is
// otherwise lost.
func TestIncomingLegClearsTheRestubStashOfAReferenceThatStaysUnresolved(t *testing.T) {
	for name, open := range map[string]func(t *testing.T) graph.Store{
		"memory": func(t *testing.T) graph.Store { return graph.New() },
		"sqlite": openIdentitySQLite,
	} {
		t.Run(name, func(t *testing.T) {
			g := open(t)
			renamed := &graph.Node{ID: "pkg/changed.go::Renamed", Kind: graph.KindFunction, Name: "Renamed",
				FilePath: "pkg/changed.go", Language: "go"}
			before := &graph.Node{ID: "pkg/changed.go::Gone", Kind: graph.KindFunction, Name: "Gone",
				FilePath: "pkg/changed.go", Language: "go"}
			g.AddBatch([]*graph.Node{
				{ID: "pkg/changed.go", Kind: graph.KindFile, Name: "changed.go", FilePath: "pkg/changed.go", Language: "go"},
				{ID: "pkg/caller.go", Kind: graph.KindFile, Name: "caller.go", FilePath: "pkg/caller.go", Language: "go"},
				{ID: "pkg/caller.go::Caller", Kind: graph.KindFunction, Name: "Caller", FilePath: "pkg/caller.go", Language: "go"},
				renamed,
			}, []*graph.Edge{
				restubbedCall("pkg/caller.go::Caller", "pkg/caller.go", 3, "Gone", before.ID, graph.OriginASTResolved, 0.9),
			})

			r := scopedResolver(t, g)
			r.SetPriorDeclarations(map[string]DeclarationSurface{"pkg/changed.go": DeclarationSurfaceOf([]*graph.Node{before})})
			r.ResolveFilesAndIncoming([]string{"pkg/changed.go"})
			r.SetPriorDeclarations(nil)

			got := storedOutEdge(t, g, "pkg/caller.go::Caller", graph.EdgeCalls)
			assert.Equal(t, "unresolved::Gone", got.To, "nothing declares Gone: the reference stays unresolved")
			assert.False(t, graph.HasRestubProvenance(got), "the stash of a reference that stays unresolved is dropped: %v", got.Meta)
			assert.Empty(t, got.Origin, "an unresolved reference carries no resolved provenance")
			assert.Equal(t, "returned", got.Meta["return_usage"], "the extractor's own metadata stays")
		})
	}
}

// guardFixture is a caller in pkg/a whose file imports pkg/c but not pkg/b,
// and a function Target in pkg/b the name-only fallback can bind the caller's
// bare reference to — a binding the cross-package guard reverts.
func guardFixture(t *testing.T, g graph.Store, call *graph.Edge) {
	t.Helper()
	g.AddBatch([]*graph.Node{
		{ID: "pkg/a/a.go", Kind: graph.KindFile, Name: "a.go", FilePath: "pkg/a/a.go", Language: "go"},
		{ID: "pkg/b/b.go", Kind: graph.KindFile, Name: "b.go", FilePath: "pkg/b/b.go", Language: "go"},
		{ID: "pkg/c/c.go", Kind: graph.KindFile, Name: "c.go", FilePath: "pkg/c/c.go", Language: "go"},
		{ID: "pkg/a/a.go::Caller", Kind: graph.KindFunction, Name: "Caller", FilePath: "pkg/a/a.go", Language: "go"},
		{ID: "pkg/b/b.go::Target", Kind: graph.KindFunction, Name: "Target", FilePath: "pkg/b/b.go", Language: "go"},
		{ID: "pkg/c/c.go::Other", Kind: graph.KindFunction, Name: "Other", FilePath: "pkg/c/c.go", Language: "go"},
	}, []*graph.Edge{
		{From: "pkg/a/a.go", To: "pkg/c/c.go", Kind: graph.EdgeImports, FilePath: "pkg/a/a.go", Line: 2},
		call,
	})
}

// A re-parse that parks a reference and binds it back to the very target it
// held re-states a binding the whole index already judged — with the
// extractor's own target, which the restub's bare placeholder is not. The
// cross-package guard must not re-judge it on the placeholder: the whole index
// keeps the binding, so the per-save leg keeps it too. A reference that binds
// somewhere new is still guarded.
func TestIncomingLegGuardLeavesARestubRoundTripAlone(t *testing.T) {
	cases := []struct {
		name   string
		prevTo string
		want   string
	}{
		{"round trip keeps the binding", "pkg/b/b.go::Target", "pkg/b/b.go::Target"},
		{"a new binding is still guarded", "pkg/elsewhere.go::Target", "unresolved::Target"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			g := graph.New()
			// A whole index binds a package-qualified call through the
			// extractor's import-pinned placeholder, which the guard never
			// polices, and stores it without origin or confidence (the
			// restub stash the fixture's chain calls carry).
			call := restubbedCall("pkg/a/a.go::Caller", "pkg/a/a.go", 5, "Target", c.prevTo, "", 0)
			guardFixture(t, g, call)
			target := g.GetNode("pkg/b/b.go::Target")
			r := scopedResolver(t, g)
			r.SetPriorDeclarations(map[string]DeclarationSurface{"pkg/b/b.go": DeclarationSurfaceOf([]*graph.Node{target})})
			r.ResolveFilesAndIncoming([]string{"pkg/b/b.go"})
			r.SetPriorDeclarations(nil)
			got := storedOutEdge(t, g, "pkg/a/a.go::Caller", graph.EdgeCalls)
			assert.Equal(t, c.want, got.To)
		})
	}
}

// A guard revert persists the reverted edge struct. The reverted row must not
// keep the abandoned bind's origin on any backend: a whole index (whose guard
// reverts in memory) leaves it empty.
func TestGuardRevertPersistsAnEmptyOrigin(t *testing.T) {
	for name, open := range map[string]func(t *testing.T) graph.Store{
		"memory": func(t *testing.T) graph.Store { return graph.New() },
		"sqlite": openIdentitySQLite,
	} {
		t.Run(name, func(t *testing.T) {
			g := open(t)
			call := &graph.Edge{From: "pkg/a/a.go::Caller", To: "unresolved::Target", Kind: graph.EdgeCalls,
				FilePath: "pkg/a/a.go", Line: 5}
			guardFixture(t, g, call)
			r := scopedResolver(t, g)
			r.ResolveFilesAndIncoming([]string{"pkg/b/b.go"})
			got := storedOutEdge(t, g, "pkg/a/a.go::Caller", graph.EdgeCalls)
			require.Equal(t, "unresolved::Target", got.To, "the guard reverts the unreachable bind")
			assert.Equal(t, true, got.Meta["guard_reverted"])
			assert.Empty(t, got.Origin, "the reverted row keeps no origin")
			assert.Zero(t, got.Confidence)
		})
	}
}

// The declaration surface is what candidate selection reads. Body-derived
// metadata — the clone signature a whole index stamps and a fresh extraction
// does not yet carry, the body metrics, body texts — is not part of it: a body
// edit that changes only those must leave every name's parked references
// carried, not re-attempted (and re-written) across the repository.
func TestDeclarationSurfaceIgnoresBodyDerivedMetadata(t *testing.T) {
	whole := &graph.Node{ID: "pkg/a.go::Load", Kind: graph.KindFunction, Name: "Load", FilePath: "pkg/a.go",
		Language: "go", Meta: map[string]any{
			"signature": "func Load(path string) error", "clone_sig": "BGw/AN/Rp", "cognitive": 6,
			"loop_depth": 1, "max_access_depth": 4, "alloc_in_loop": true, "linear_scan_in_loop": true,
			"recursion_in_loop": true, "body_hash": "h1", "snippet": "return nil",
		}}
	fresh := *whole
	fresh.Meta = map[string]any{"signature": "func Load(path string) error", "cognitive": 7, "max_access_depth": 5}
	assert.Equal(t, DeclarationSurfaceOf([]*graph.Node{whole}), DeclarationSurfaceOf([]*graph.Node{&fresh}))

	changed := fresh
	changed.Meta = map[string]any{"signature": "func Load(path string, strict bool) error"}
	assert.NotEqual(t, DeclarationSurfaceOf([]*graph.Node{whole}), DeclarationSurfaceOf([]*graph.Node{&changed}),
		"a signature change is still a surface change")
}

// End to end on the incoming leg: the whole index's declaration carries a clone
// signature, the re-parsed one does not; the parked member references on the
// name are not re-attempted.
func TestIncomingLegCarriesParkedReferencesAcrossACloneSignatureChange(t *testing.T) {
	g, parked := declarationFixture(t, map[string]any{"signature": "func Target()"})
	before := &graph.Node{ID: "pkg/changed.go::Target", Kind: graph.KindFunction, Name: "Target",
		FilePath: "pkg/changed.go", Language: "go", StartLine: 10, EndLine: 12,
		Meta: map[string]any{"signature": "func Target()", "clone_sig": "sig-before"}}
	r := scopedResolver(t, g)
	r.SetPriorDeclarations(priorOf(before))
	stats := r.ResolveFilesAndIncoming([]string{"pkg/changed.go"})
	r.SetPriorDeclarations(nil)
	assert.Equal(t, "unresolved::Target", parked.To, "an unchanged surface carries its parked references")
	assert.Zero(t, stats.Resolved)
}

// The per-save out-edge reuse binds a re-parsed file's reference in memory; the
// dataflow edges keyed from its placeholder at the same site take the bound
// source, exactly the move a resolution batch makes.
func TestRepointPlaceholderSourcesInBatchFollowsTheReusedReference(t *testing.T) {
	const placeholder = "unresolved::extern::example.com/m/c::C"
	call := &graph.Edge{From: "repo/b/b.go::B", To: "repo/c/c.go::C", Kind: graph.EdgeCalls, FilePath: "repo/b/b.go", Line: 7}
	flow := &graph.Edge{From: "repo/" + placeholder, To: "repo/b/b.go::B", Kind: graph.EdgeValueFlow, FilePath: "repo/b/b.go", Line: 7}
	bare := &graph.Edge{From: placeholder, To: "repo/b/b.go::B#param:p0", Kind: graph.EdgeArgOf, FilePath: "repo/b/b.go", Line: 7}
	otherLine := &graph.Edge{From: "repo/" + placeholder, To: "repo/b/b.go::B", Kind: graph.EdgeValueFlow, FilePath: "repo/b/b.go", Line: 9}
	otherKind := &graph.Edge{From: "repo/" + placeholder, To: "repo/b/b.go::B", Kind: graph.EdgeReads, FilePath: "repo/b/b.go", Line: 7}
	edges := []*graph.Edge{call, flow, bare, otherLine, otherKind}

	moved := RepointPlaceholderSourcesInBatch(edges, []graph.EdgeReindex{{Edge: call, OldTo: placeholder}})
	assert.Equal(t, 2, moved)
	assert.Equal(t, "repo/c/c.go::C", flow.From, "the prefixed placeholder form moves")
	assert.Equal(t, "repo/c/c.go::C", bare.From, "the bare placeholder form moves")
	assert.Equal(t, "repo/"+placeholder, otherLine.From, "another site keeps its own placeholder")
	assert.Equal(t, "repo/"+placeholder, otherKind.From, "only dataflow kinds move")

	t.Setenv("GORTEX_RESOLVE_FROM_RECONCILE", "0")
	flow.From = "repo/" + placeholder
	assert.Zero(t, RepointPlaceholderSourcesInBatch(edges, []graph.EdgeReindex{{Edge: call, OldTo: placeholder}}),
		"the reconciliation switch covers the batch form too")
}
