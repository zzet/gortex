package indexer

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
)

// newSQLiteIndexer builds an indexer over a sqlite-backed graph, the
// configuration the affected-by pass's persisted reverse lookup runs on.
func newSQLiteIndexer(t *testing.T) (*Indexer, *store_sqlite.Store) {
	t.Helper()
	store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "g.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })

	reg := parser.NewRegistry()
	languages.RegisterAll(reg)
	cfg := config.Default().Index
	cfg.Workers = 1
	return New(store, reg, cfg, zap.NewNop()), store
}

// TestAffectedBy_SignatureChange_ReresolvesCaller is the headline case
// on the in-memory backend (whose reverse lookup is the pre-evict
// in-edge snapshot): b.go calls F defined in a.go; changing F's
// SIGNATURE re-resolves b.go — its call edge lands on the fresh F node.
func TestAffectedBy_SignatureChange_ReresolvesCaller(t *testing.T) {
	dir := t.TempDir()
	aPath := filepath.Join(dir, "a.go")
	bPath := filepath.Join(dir, "b.go")
	writeFile(t, aPath, "package p\n\nfunc F(x int) int { return x }\n")
	writeFile(t, bPath, "package p\n\nfunc Caller() int { return F(1) }\n")

	g := graph.New()
	idx := newTestIndexer(g)
	_, err := idx.Index(dir)
	require.NoError(t, err)

	fID := fnNodeID(t, g, "a.go", "F")
	callerID := fnNodeID(t, g, "b.go", "Caller")
	require.Equal(t, fID, callTargetFrom(t, g, callerID),
		"baseline: Caller's call must resolve to F")

	bumpMtime(t, aPath, "package p\n\nfunc F(x int, y int) int { return x + y }\n")
	_, err = idx.IncrementalReindexPaths(dir, []string{aPath})
	require.NoError(t, err)

	newFID := fnNodeID(t, g, "a.go", "F")
	assert.Equal(t, newFID, callTargetFrom(t, g, callerID),
		"after F's signature changed, Caller's edge must be re-resolved to the fresh F")
}

// TestAffectedBy_BodyOnlyEdit_NoFanout proves the gate: an edit that
// changes only a function BODY produces no signature delta and must not
// fan out — the whole point of delta detection is that the common case
// (a body edit) costs nothing beyond the changed file itself. Driven
// through whole-root IncrementalReindexPaths to cover that sync route too.
func TestAffectedBy_BodyOnlyEdit_NoFanout(t *testing.T) {
	dir := t.TempDir()
	aPath := filepath.Join(dir, "a.go")
	writeFile(t, aPath, "package p\n\nfunc F(x int) int { return x }\n")
	writeFile(t, filepath.Join(dir, "b.go"), "package p\n\nfunc Caller() int { return F(1) }\n")

	g := graph.New()
	idx := newTestIndexer(g)
	_, err := idx.Index(dir)
	require.NoError(t, err)
	callerID := fnNodeID(t, g, "b.go", "Caller")

	snap := idx.snapshotAffectedBy("a.go")
	require.NotNil(t, snap)
	bumpMtime(t, aPath, "package p\n\nfunc F(x int) int { return x + 1 }\n")
	res, err := idx.IncrementalReindexPaths(dir, nil)
	require.NoError(t, err)
	require.Equal(t, 1, res.StaleFileCount)

	var fresh []*graph.Node
	for _, node := range g.AllNodes() {
		if node != nil && node.FilePath == "a.go" {
			fresh = append(fresh, node)
		}
	}
	assert.Empty(t, affectedByDelta(g, snap, fresh),
		"a body-only edit must not produce an affected-by frontier")

	// The caller's edge still survives the definition re-index via the
	// existing restub + reverse-resolve pair.
	assert.Equal(t, fnNodeID(t, g, "a.go", "F"), callTargetFrom(t, g, callerID),
		"the caller edge must still be bound after a body-only re-index")
}

// TestAffectedBy_PerSaveIndexFile_ReresolvesCaller drives the same
// signature change through IndexFile directly — the watcher's per-save
// patch path — proving every sync route shares the hook.
func TestAffectedBy_PerSaveIndexFile_ReresolvesCaller(t *testing.T) {
	dir := t.TempDir()
	aPath := filepath.Join(dir, "a.go")
	writeFile(t, aPath, "package p\n\nfunc F() int { return 0 }\n")
	writeFile(t, filepath.Join(dir, "b.go"), "package p\n\nfunc Caller() int { return F() }\n")

	g := graph.New()
	idx := newTestIndexer(g)
	_, err := idx.Index(dir)
	require.NoError(t, err)
	callerID := fnNodeID(t, g, "b.go", "Caller")

	writeFile(t, aPath, "package p\n\nfunc F(n int) int { return n }\n")
	require.NoError(t, idx.IndexFile(aPath))

	assert.Equal(t, fnNodeID(t, g, "a.go", "F"), callTargetFrom(t, g, callerID),
		"the per-save IndexFile route must re-resolve the caller")
}

// TestAffectedBy_RemovedSymbol_SQLite removes a called symbol from its
// definition file on a sqlite-backed graph: the caller's edge must
// degrade to the resolver's normal unresolved stub (no dangling old-ID
// edge), and the caller's now-stale persisted reference fact must be
// dropped by the pass's re-persist.
func TestAffectedBy_RemovedSymbol_SQLite(t *testing.T) {
	dir := t.TempDir()
	aPath := filepath.Join(dir, "a.go")
	writeFile(t, aPath, "package p\n\nfunc F() {}\n\nfunc Keep() {}\n")
	writeFile(t, filepath.Join(dir, "b.go"), "package p\n\nfunc Caller() { F() }\n")

	idx, store := newSQLiteIndexer(t)
	_, err := idx.Index(dir)
	require.NoError(t, err)
	idx.ResolveAll() // seeds the persisted ref-facts sidecar

	g := idx.Graph()
	fID := fnNodeID(t, g, "a.go", "F")
	callerID := fnNodeID(t, g, "b.go", "Caller")
	require.Equal(t, fID, callTargetFrom(t, g, callerID))

	// The reverse lookup must already know b.go references F.
	byFile, err := store.LoadRefFactsByTargets("", []string{fID})
	require.NoError(t, err)
	require.Contains(t, byFile, "b.go",
		"the seeded sidecar must answer the by-target reverse lookup")

	bumpMtime(t, aPath, "package p\n\nfunc Keep() {}\n")
	_, err = idx.IncrementalReindexPaths(dir, []string{aPath})
	require.NoError(t, err)

	target := callTargetFrom(t, g, callerID)
	assert.True(t, graph.IsUnresolvedTarget(target),
		"the caller's edge must degrade to an unresolved stub, got %q", target)
	assert.Equal(t, "F", graph.UnresolvedName(target))

	facts, err := store.LoadRefFactsByFiles("", []string{"b.go"})
	require.NoError(t, err)
	for _, f := range facts {
		assert.NotEqual(t, fID, f.ToID,
			"the stale fact pointing at the removed symbol must be re-persisted away")
	}
}

// TestAffectedBy_SidecarDiscovery_SQLite proves the persisted reverse
// lookup is a real discovery source, not just a mirror of the live
// graph: the caller's in-edge is parked on an unresolved stub BEFORE
// the change (so the pre-evict snapshot sees no in-edge for F), and the
// affected file is still found — via LoadRefFactsByTargets — and
// re-resolved.
func TestAffectedBy_SidecarDiscovery_SQLite(t *testing.T) {
	dir := t.TempDir()
	aPath := filepath.Join(dir, "a.go")
	writeFile(t, aPath, "package p\n\nfunc F() int { return 0 }\n")
	writeFile(t, filepath.Join(dir, "b.go"), "package p\n\nfunc Caller() int { return F() }\n")

	idx, _ := newSQLiteIndexer(t)
	_, err := idx.Index(dir)
	require.NoError(t, err)
	idx.ResolveAll()

	g := idx.Graph()
	callerID := fnNodeID(t, g, "b.go", "Caller")
	require.Equal(t, fnNodeID(t, g, "a.go", "F"), callTargetFrom(t, g, callerID))

	// Park the caller's live edge on a stub — the state a prior evict
	// leaves behind — so only the sidecar can name b.go as affected.
	_, err = idx.restubIncomingRefs("a.go")
	require.NoError(t, err)
	require.True(t, graph.IsUnresolvedTarget(callTargetFrom(t, g, callerID)),
		"precondition: the live in-edge must be parked on a stub")

	bumpMtime(t, aPath, "package p\n\nfunc F(n int) int { return n }\n")
	_, err = idx.IncrementalReindexPaths(dir, []string{aPath})
	require.NoError(t, err)

	assert.Equal(t, fnNodeID(t, g, "a.go", "F"), callTargetFrom(t, g, callerID),
		"the sidecar-discovered caller must be re-resolved to the fresh F")
}

// TestAffectedBy_CapBoundsFanout configures a fan-out cap of 1 with
// three referencing files. The live truncation warning must report the
// exact bounded set and the two files it dropped.
func TestAffectedBy_CapBoundsFanout(t *testing.T) {
	dir := t.TempDir()
	aPath := filepath.Join(dir, "a.go")
	writeFile(t, aPath, "package p\n\nfunc F(x int) int { return x }\n")
	writeFile(t, filepath.Join(dir, "b.go"), "package p\n\nfunc CallerB() int { return F(1) }\n")
	writeFile(t, filepath.Join(dir, "c.go"), "package p\n\nfunc CallerC() int { return F(2) }\n")
	writeFile(t, filepath.Join(dir, "d.go"), "package p\n\nfunc CallerD() int { return F(3) }\n")

	g := graph.New()
	reg := parser.NewRegistry()
	reg.Register(languages.NewGoExtractor())
	cfg := config.Default().Index
	cfg.Workers = 1
	cfg.AffectedByReresolveMax = 1
	core, observed := observer.New(zap.DebugLevel)
	idx := New(g, reg, cfg, zap.New(core))
	_, err := idx.Index(dir)
	require.NoError(t, err)

	bumpMtime(t, aPath, "package p\n\nfunc F(x int, y int) int { return x + y }\n")
	_, err = idx.IncrementalReindexPaths(dir, []string{aPath})
	require.NoError(t, err)

	entries := observed.FilterMessage("affected-by: re-resolve set truncated").All()
	require.Len(t, entries, 1)
	fields := entries[0].ContextMap()
	assert.EqualValues(t, 3, fields["affected"])
	assert.EqualValues(t, 1, fields["cap"])
	assert.EqualValues(t, 2, fields["dropped"])
}

// TestAffectedBy_DeferredBatchPath_NoFanout proves the batch guard: a
// caller that defers global passes (warmup, ReconcileAll) runs one
// resolve at the end of the batch, so the per-file affected-by pass
// must stay off even for a genuine signature change.
func TestAffectedBy_DeferredBatchPath_NoFanout(t *testing.T) {
	dir := t.TempDir()
	aPath := filepath.Join(dir, "a.go")
	writeFile(t, aPath, "package p\n\nfunc F(x int) int { return x }\n")
	writeFile(t, filepath.Join(dir, "b.go"), "package p\n\nfunc CallerB() int { return F(1) }\n")
	writeFile(t, filepath.Join(dir, "c.go"), "package p\n\nfunc CallerC() int { return F(2) }\n")

	g := graph.New()
	reg := parser.NewRegistry()
	reg.Register(languages.NewGoExtractor())
	cfg := config.Default().Index
	cfg.Workers = 1
	cfg.AffectedByReresolveMax = 1
	core, observed := observer.New(zap.DebugLevel)
	idx := New(g, reg, cfg, zap.New(core))
	_, err := idx.Index(dir)
	require.NoError(t, err)

	idx.SetDeferGlobalPasses(true)
	bumpMtime(t, aPath, "package p\n\nfunc F(x int, y int) int { return x + y }\n")
	require.NoError(t, idx.IndexFile(aPath))

	assert.Empty(t, observed.FilterMessage("affected-by: re-resolve set truncated").All(),
		"deferred-batch indexing must not fan out per file — the batch caller resolves once at the end")
}
