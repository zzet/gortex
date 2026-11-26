package indexer

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/search"
)

// newSqliteGraph opens a temp-file sqlite-backed graph store for a test.
// A disk file (not :memory:) is required because the store pools
// connections and an in-memory sqlite DB is per-connection.
func newSqliteGraph(t *testing.T) graph.Store {
	t.Helper()
	st, err := store_sqlite.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// incomingCallEdges returns the EdgeCalls edges currently bound INTO node id
// (its resolved callers).
func incomingCallEdges(t *testing.T, g graph.Store, id string) []*graph.Edge {
	t.Helper()
	var out []*graph.Edge
	for _, e := range g.GetInEdges(id) {
		if e != nil && e.Kind == graph.EdgeCalls {
			out = append(out, e)
		}
	}
	return out
}

// stampLSP marks an edge compiler-grade and persists it (the tier the
// track-time semantic-enrichment pass mints once).
func stampLSP(t *testing.T, g graph.Store, e *graph.Edge) {
	t.Helper()
	g.SetEdgeProvenance(e, graph.OriginLSPResolved)
	e.Tier = graph.ResolvedBy(graph.OriginLSPResolved)
	e.Confidence = 1.0
	g.ReindexEdges([]graph.EdgeReindex{{Edge: e, OldTo: e.To}})
}

// TestIncrementalReindex_DoubleCycle_Sqlite_IntraFileCaller models the live
// staleness-probe REVERT conditions the in-memory double-cycle test omits: a
// sqlite backend (GetInEdges returns freshly-decoded edge pointers, so an
// in-place provenance restore that is not persisted is lost) AND an appended
// symbol that CALLS the target (an intra-file caller added on the ADD cycle and
// removed on the REVERT cycle). After BOTH cycles the cross-file caller edge
// must still resolve to the definition and keep its lsp_resolved provenance —
// find_usages hides text_matched by default, so a demotion here reads as a
// silent zero.
func TestIncrementalReindex_DoubleCycle_Sqlite_IntraFileCaller(t *testing.T) {
	dir := t.TempDir()
	defPath := filepath.Join(dir, "debug.go")
	callerPath := filepath.Join(dir, "caller.go")
	writeFile(t, defPath, "package p\n\nfunc Foo() {}\n")
	writeFile(t, callerPath, "package p\n\nfunc Bar() { Foo() }\n")

	g := newSqliteGraph(t)
	idx := New(g, newTestRegistry(), config.IndexConfig{Workers: 1}, zap.NewNop())
	idx.search = search.NewNull()
	idx.SetRootPath(dir)
	_, err := idx.IndexCtx(testCtx(), dir)
	require.NoError(t, err)

	fooID := fnNodeID(t, g, "debug.go", "Foo")
	barID := fnNodeID(t, g, "caller.go", "Bar")
	require.Equal(t, fooID, callTargetFrom(t, g, barID))

	stampLSP(t, g, callEdgeFrom(t, g, barID))

	cycles := []string{
		"package p\n\nfunc Foo() {}\n\nfunc Probe() { Foo() }\n", // append + intra-file caller
		"package p\n\nfunc Foo() {}\n",                           // revert (removes intra-file caller)
	}
	for i, content := range cycles {
		writeFile(t, defPath, content)
		require.NoError(t, idx.IndexFile(defPath))

		after := callEdgeFrom(t, g, fnNodeID(t, g, "caller.go", "Bar"))
		assert.Falsef(t, graph.IsUnresolvedTarget(after.To),
			"cycle %d: Bar's cross-file caller edge must not remain a stub", i)
		assert.Equalf(t, fnNodeID(t, g, "debug.go", "Foo"), after.To,
			"cycle %d: Bar's cross-file caller edge must still resolve to Foo", i)
		assert.Equalf(t, graph.OriginLSPResolved, after.Origin,
			"cycle %d: incoming edge must keep its lsp origin (no ratchet-down)", i)
	}
}

// TestReresolveFileScoped_RebindsDroppedIncomingEdges is the recovery half of
// the WS-1 self-heal: whatever mechanism drops a surviving definition's
// incoming callers to stubs, the forced scoped re-resolve the incoming-aware
// regression guard enqueues must rebind every one and restore its lsp
// provenance — on a disk backend, where the restore has to be persisted, not
// just mutated in place.
func TestReresolveFileScoped_RebindsDroppedIncomingEdges(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "def.go"), "package p\n\nfunc Foo() {}\n")
	callers := []string{"c0.go", "c1.go", "c2.go"}
	for i, c := range callers {
		writeFile(t, filepath.Join(dir, c), fmt.Sprintf("package p\n\nfunc Bar%d() { Foo() }\n", i))
	}

	g := newSqliteGraph(t)
	idx := New(g, newTestRegistry(), config.IndexConfig{Workers: 1}, zap.NewNop())
	idx.search = search.NewNull()
	idx.SetRootPath(dir)
	_, err := idx.IndexCtx(testCtx(), dir)
	require.NoError(t, err)

	fooID := fnNodeID(t, g, "def.go", "Foo")
	for i, c := range callers {
		e := callEdgeFrom(t, g, fnNodeID(t, g, c, fmt.Sprintf("Bar%d", i)))
		require.Equal(t, fooID, e.To)
		stampLSP(t, g, e)
	}
	require.Len(t, incomingCallEdges(t, g, fooID), len(callers), "baseline: all callers bound")

	// Simulate the live drop: restub Foo's incoming caller edges to
	// unresolved::Foo (what a re-parse of def.go does before it re-resolves).
	_, err = idx.restubIncomingRefs("def.go")
	require.NoError(t, err)
	require.Empty(t, incomingCallEdges(t, g, fooID), "precondition: callers dropped to stubs")

	// The forced scoped re-resolve must rebind every dropped caller AND restore
	// its lsp provenance on the sqlite backend.
	require.NoError(t, idx.ReresolveFileScoped("def.go"))
	got := incomingCallEdges(t, g, fooID)
	require.Len(t, got, len(callers), "every dropped caller rebound")
	for _, e := range got {
		assert.Equal(t, graph.OriginLSPResolved, e.Origin,
			"rebound caller must recover its lsp origin (restore persisted)")
	}
}

// TestGuardResolvedEdgeRegression_IncomingArm_FiresOnRevert asserts the
// incoming arm of the regression guard: an external revert removes the appended
// probe symbol (nodesAfter < nodesBefore, so the out-edge arm stays silent) yet
// the surviving definition lost every caller — the guard must still enqueue a
// forced scoped re-resolve so the drop self-heals.
func TestGuardResolvedEdgeRegression_IncomingArm_FiresOnRevert(t *testing.T) {
	g := newSqliteGraph(t)
	idx := New(g, newTestRegistry(), config.IndexConfig{Workers: 1}, zap.NewNop())
	w, err := NewWatcher(idx, config.WatchConfig{}, zap.NewNop())
	require.NoError(t, err)

	fired := make(chan map[string]struct{}, 1)
	w.reresolveFn = func(files map[string]struct{}) { fired <- files }

	w.guardResolvedEdgeRegression("def.go",
		/*nodesBefore*/ 6 /*nodesAfter*/, 5, // probe removed
		/*resolvedBefore*/ 0 /*resolvedAfter*/, 0, // out-edges never regressed
		/*incomingBefore*/ 8 /*incomingAfter*/, 0) // callers all dropped

	select {
	case files := <-fired:
		_, ok := files["def.go"]
		assert.True(t, ok, "revert must enqueue a forced re-resolve of the definition file")
	case <-time.After(2 * time.Second):
		t.Fatal("incoming-edge regression did not enqueue a re-resolve")
	}
}

// TestGuardResolvedEdgeRegression_NoFireWhenStable proves the guard does not
// churn: a re-parse that kept both its out-edges and its incoming callers must
// not enqueue anything.
func TestGuardResolvedEdgeRegression_NoFireWhenStable(t *testing.T) {
	g := newSqliteGraph(t)
	idx := New(g, newTestRegistry(), config.IndexConfig{Workers: 1}, zap.NewNop())
	w, err := NewWatcher(idx, config.WatchConfig{}, zap.NewNop())
	require.NoError(t, err)

	fired := make(chan struct{}, 1)
	w.reresolveFn = func(map[string]struct{}) { fired <- struct{}{} }

	w.guardResolvedEdgeRegression("def.go", 5, 5, 10, 10, 8, 8)

	select {
	case <-fired:
		t.Fatal("a stable re-parse must not enqueue a re-resolve")
	case <-time.After(300 * time.Millisecond):
	}
}
