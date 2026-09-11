package indexer

import (
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
)

// restubReindexRecord snapshots one EdgeReindex AT CALL TIME. The batch holds
// live edge pointers that a later resolver pass mutates back to a resolved
// target, so a post-hoc read of r.Edge.To cannot tell a restub write from a
// rebind write.
type restubReindexRecord struct {
	from  string
	to    string
	oldTo string
	kind  graph.EdgeKind
}

// restubCountingStore is the write audit for the restub frontier: every
// ReindexEdges row the incremental commit issues, in order.
type restubCountingStore struct {
	graph.Store

	mu        sync.Mutex
	reindexes []restubReindexRecord
}

func newRestubCountingStore() *restubCountingStore {
	return &restubCountingStore{Store: graph.New()}
}

func (s *restubCountingStore) ReindexEdges(batch []graph.EdgeReindex) {
	s.mu.Lock()
	for _, r := range batch {
		if r.Edge == nil {
			continue
		}
		s.reindexes = append(s.reindexes, restubReindexRecord{
			from: r.Edge.From, to: r.Edge.To, oldTo: r.OldTo, kind: r.Edge.Kind,
		})
	}
	s.mu.Unlock()
	s.Store.ReindexEdges(batch)
}

// restubWrites counts the durable rows that parked a previously-resolved edge
// under `unresolved::<name>` — i.e. the restub half of the write
// amplification, excluding the resolver's rebind rows.
func (s *restubCountingStore) restubWrites(name string) int {
	stub := graph.UnresolvedMarker + name
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, r := range s.reindexes {
		if r.to == stub && !graph.IsUnresolvedTarget(r.oldTo) {
			n++
		}
	}
	return n
}

func (s *restubCountingStore) reset() {
	s.mu.Lock()
	s.reindexes = nil
	s.mu.Unlock()
}

const (
	restubDefPath    = "def.go"
	restubCallerPath = "caller.go"
	restubCallerID   = "caller.go::Bar"
)

func restubCallerNode() *graph.Node {
	return &graph.Node{
		ID: restubCallerID, Kind: graph.KindFunction, Name: "Bar", FilePath: restubCallerPath,
	}
}

func restubCallEdge(targetID string) *graph.Edge {
	return &graph.Edge{
		From: restubCallerID, To: targetID, Kind: graph.EdgeCalls,
		FilePath: restubCallerPath, Line: 3,
		Origin: graph.OriginLSPResolved, Tier: graph.ResolvedBy(graph.OriginLSPResolved),
		Confidence: 1.0,
	}
}

// runRestubStage drives the exact production sequence
// commitStructuralIncrementalBatch runs around the restub — prior view ->
// restub -> evict -> AddBatch(fresh payload + carried in-edges) — against a
// write-audited store, and reports the audit plus the carried set.
func runRestubStage(
	t *testing.T,
	prior []*graph.Node,
	fresh []*graph.Node,
	freshEdges []*graph.Edge,
	inEdges []*graph.Edge,
	withExtraction bool,
) (*restubCountingStore, []*graph.Edge) {
	t.Helper()
	st := newRestubCountingStore()
	seed := append([]*graph.Node{restubCallerNode()}, prior...)
	st.AddBatch(seed, inEdges)

	stage := &incrementalBatchStage{graphPath: restubDefPath, priorNodes: prior}
	if withExtraction {
		stage.result = &parser.ExtractionResult{Nodes: fresh, Edges: freshEdges}
	}
	stages := []*incrementalBatchStage{stage}
	view := loadIncrementalPriorView(st, stages)

	carried := restubIncomingRefsFromView(st, stages, view)

	evictFilesBatched(st, []string{restubDefPath})
	st.AddBatch(fresh, append(append([]*graph.Edge{}, freshEdges...), carried...))
	return st, carried
}

func fnNode(id, name string, meta map[string]any) *graph.Node {
	return &graph.Node{
		ID: id, Kind: graph.KindFunction, Name: name, FilePath: restubDefPath, Meta: meta,
	}
}

// An unchanged contract must cost ZERO restub rows, and the in-edge must
// still be there afterwards. Both halves are load-bearing: eviction deletes
// every edge incident to a doomed node, so a gate that merely skips the write
// would silently destroy the caller's edge.
func TestRestubFrontierCarriesUnchangedContractInEdges(t *testing.T) {
	prior := []*graph.Node{fnNode("def.go::Foo", "Foo", map[string]any{"signature": "func Foo()"})}
	fresh := []*graph.Node{fnNode("def.go::Foo", "Foo", map[string]any{"signature": "func Foo()"})}
	edge := restubCallEdge("def.go::Foo")

	st, carried := runRestubStage(t, prior, fresh, nil, []*graph.Edge{edge}, true)

	assert.Equal(t, 0, st.restubWrites("Foo"),
		"an unchanged contract must not park its surviving in-edges under a stub")
	require.Len(t, carried, 1, "the unrestubbed in-edge must be carried across the eviction")

	in := incomingCallEdges(t, st, "def.go::Foo")
	require.Len(t, in, 1, "the carried in-edge was lost across evict + AddBatch")
	assert.Equal(t, restubCallerID, in[0].From)
	assert.Equal(t, graph.ResolvedBy(graph.OriginLSPResolved), in[0].Tier,
		"carrying the edge must preserve the resolved tier the restub round trip stashes")
	assert.False(t, graph.HasRestubProvenance(in[0]),
		"a carried edge was never parked, so it must not carry a restub stash")
}

// A signature change still restubs — exactly the affected edge, and the
// carried set stays empty.
func TestRestubFrontierRestubsSignatureChange(t *testing.T) {
	prior := []*graph.Node{fnNode("def.go::Foo", "Foo", map[string]any{"signature": "func Foo()"})}
	fresh := []*graph.Node{fnNode("def.go::Foo", "Foo", map[string]any{"signature": "func Foo(n int)"})}
	edge := restubCallEdge("def.go::Foo")

	st, carried := runRestubStage(t, prior, fresh, nil, []*graph.Edge{edge}, true)

	assert.Equal(t, 1, st.restubWrites("Foo"), "a signature change must restub its in-edges")
	assert.Empty(t, carried)
	assert.Equal(t, graph.UnresolvedMarker+"Foo", edge.To)
	assert.True(t, graph.HasRestubProvenance(edge))
}

// A rename is a removed stable key. The old name's in-edges must be parked so
// the incoming pass can degrade them honestly.
func TestRestubFrontierRestubsRenamedSymbol(t *testing.T) {
	prior := []*graph.Node{fnNode("def.go::Foo", "Foo", nil)}
	fresh := []*graph.Node{fnNode("def.go::Renamed", "Renamed", nil)}
	edge := restubCallEdge("def.go::Foo")

	st, carried := runRestubStage(t, prior, fresh, nil, []*graph.Edge{edge}, true)

	assert.Equal(t, 1, st.restubWrites("Foo"), "a removed symbol must restub its in-edges")
	assert.Empty(t, carried)
}

// The "too eager" direction. stableSymbolKey is line-insensitive on purpose,
// so a body edit above a `name@<line>` definition keeps the key and the shape
// while rewriting the node ID. The in-edge points at the DEAD id, so the
// frontier must restub it anyway.
func TestRestubFrontierRestubsLineShiftedNodeID(t *testing.T) {
	prior := []*graph.Node{{
		ID: "def.go::Owner.member@3", Kind: graph.KindMethod, Name: "Owner.member",
		FilePath: restubDefPath,
	}}
	fresh := []*graph.Node{{
		ID: "def.go::Owner.member@7", Kind: graph.KindMethod, Name: "Owner.member",
		FilePath: restubDefPath,
	}}
	edge := restubCallEdge("def.go::Owner.member@3")

	st, carried := runRestubStage(t, prior, fresh, nil, []*graph.Edge{edge}, true)

	assert.Equal(t, 1, st.restubWrites("Owner.member"),
		"an in-edge pointing at a node id the reparse did not reproduce must be restubbed")
	assert.Empty(t, carried)
}

// Visibility is not part of the affected-by shape, but it decides whether a
// referrer in another file may bind here at all.
func TestRestubFrontierRestubsVisibilityChange(t *testing.T) {
	prior := []*graph.Node{fnNode("def.go::Foo", "Foo", map[string]any{"visibility": "public"})}
	fresh := []*graph.Node{fnNode("def.go::Foo", "Foo", map[string]any{"visibility": "private"})}
	edge := restubCallEdge("def.go::Foo")

	st, carried := runRestubStage(t, prior, fresh, nil, []*graph.Edge{edge}, true)

	assert.Equal(t, 1, st.restubWrites("Foo"),
		"a visibility change must restub even though the call-site shape is identical")
	assert.Empty(t, carried)
}

// A newly ADDED definition of the same name is never part of the affected-by
// delta (nothing can hold a stale reference to a symbol that did not exist),
// but it changes the candidate set an existing referrer of that name should be
// re-offered.
func TestRestubFrontierRestubsNewSameNameDefinition(t *testing.T) {
	prior := []*graph.Node{fnNode("def.go::Foo", "Foo", nil)}
	fresh := []*graph.Node{
		fnNode("def.go::Foo", "Foo", nil),
		{ID: "def.go::Foo#type", Kind: graph.KindType, Name: "Foo", FilePath: restubDefPath},
	}
	edge := restubCallEdge("def.go::Foo")

	st, carried := runRestubStage(t, prior, fresh, nil, []*graph.Edge{edge}, true)

	assert.Equal(t, 1, st.restubWrites("Foo"),
		"a second definition of the name must re-open the referrer's binding")
	assert.Empty(t, carried)
}

// The conservative fallback: with no fresh extraction to compare against, the
// frontier cannot be computed and the full restub stands. This is the shape
// evictDeletedFilesBatched relies on — its stages carry priorNodes and no
// result — so the classification itself is asserted, not just its effect.
func TestRestubFrontierConservativeWithoutExtraction(t *testing.T) {
	prior := []*graph.Node{fnNode("def.go::Foo", "Foo", map[string]any{"signature": "func Foo()"})}
	edge := restubCallEdge("def.go::Foo")

	probe := newRestubCountingStore()
	probe.AddBatch(append([]*graph.Node{restubCallerNode()}, prior...), []*graph.Edge{edge})
	stage := &incrementalBatchStage{graphPath: restubDefPath, priorNodes: prior}
	view := loadIncrementalPriorView(probe, []*incrementalBatchStage{stage})
	require.True(t, restubFrontierForStage(stage, view).conservative,
		"a stage with no fresh extraction must classify conservative")

	st, carried := runRestubStage(t, prior, nil, nil, []*graph.Edge{edge}, false)

	assert.Equal(t, 1, st.restubWrites("Foo"),
		"an uncomputable frontier must fall back to the full restub, never drop it")
	assert.Empty(t, carried)
}

// The deletion path through the production entrypoint: a deleted definition
// must still park its referrers, and the gate must not reach it.
func TestIncrementalBatchDeletedFileStillRestubsIncoming(t *testing.T) {
	idx, st, dir, defPath := restubPipelineFixture(t)

	require.NoError(t, os.Remove(defPath))
	_, err := idx.IncrementalReindexPaths(dir, []string{defPath})
	require.NoError(t, err)

	assert.GreaterOrEqual(t, st.restubWrites("Foo"), 1,
		"a deleted definition must still park its referrers under a stub")
	assert.True(t, graph.IsUnresolvedTarget(
		callTargetFrom(t, st, fnNodeID(t, st, "caller.go", "Bar"))),
		"the caller's edge must degrade to an unresolved stub once the definition is gone")
}

// --- production entrypoint -------------------------------------------------

// restubPipelineFixture writes the two-file Go corpus the pipeline tests share
// and returns the indexer, the write-audited store and the def.go path.
func restubPipelineFixture(t *testing.T) (*Indexer, *restubCountingStore, string, string) {
	t.Helper()
	dir := t.TempDir()
	defPath := filepath.Join(dir, "def.go")
	callerPath := filepath.Join(dir, "caller.go")
	writeFile(t, defPath, "package p\n\nfunc helper() {}\n\nfunc Foo() {}\n")
	writeFile(t, callerPath, "package p\n\nfunc Bar() { Foo() }\n")

	st := newRestubCountingStore()
	idx := newTestIndexer(st)
	_, err := idx.Index(dir)
	require.NoError(t, err)
	idx.ResolveAll()

	require.Equal(t, fnNodeID(t, st, "def.go", "Foo"),
		callTargetFrom(t, st, fnNodeID(t, st, "caller.go", "Bar")),
		"fixture precondition: the caller must be bound to def.go::Foo")
	st.reset()
	return idx, st, dir, defPath
}

// The n-th body-only edit of a referenced file: the edit is structural (it
// adds a call edge, so the semantic fingerprint moves and the metadata-only
// arm is not taken), yet no in-edge of Foo may be parked under a stub.
//
// This is the production trace for the frontier: IncrementalReindexPaths ->
// reindexIncrementalFilesBatched -> commitStructuralIncrementalBatch ->
// restubIncomingRefsFromView.
func TestIncrementalBatchBodyOnlyEditSkipsIncomingRestub(t *testing.T) {
	idx, st, dir, defPath := restubPipelineFixture(t)

	bumpMtime(t, defPath, "package p\n\nfunc helper() {}\n\nfunc Foo() { helper() }\n")
	_, err := idx.IncrementalReindexPaths(dir, []string{defPath})
	require.NoError(t, err)

	fooID := fnNodeID(t, st, "def.go", "Foo")
	helperID := fnNodeID(t, st, "def.go", "helper")

	// The reparse really was structural — the new body call landed.
	var bodyCall bool
	for _, e := range st.GetOutEdges(fooID) {
		if e != nil && e.Kind == graph.EdgeCalls && e.To == helperID {
			bodyCall = true
		}
	}
	require.True(t, bodyCall,
		"fixture precondition: the body edit must have re-extracted def.go structurally")

	assert.Equal(t, 0, st.restubWrites("Foo"),
		"a body-only edit must not park the caller's in-edge under a stub")

	in := incomingCallEdges(t, st, fooID)
	require.Len(t, in, 1, "the caller's in-edge must survive the reparse")
	assert.Equal(t, fnNodeID(t, st, "caller.go", "Bar"), in[0].From)
	assert.Equal(t, fooID, callTargetFrom(t, st, fnNodeID(t, st, "caller.go", "Bar")))
}

// sqliteRestubFixture indexes the two-file corpus into the PRODUCTION backend
// and stamps the caller's edge compiler-grade, so any loss across the reparse
// is visible.
func sqliteRestubFixture(t *testing.T) (*Indexer, graph.Store, string, string) {
	t.Helper()
	dir := t.TempDir()
	defPath := filepath.Join(dir, "def.go")
	callerPath := filepath.Join(dir, "caller.go")
	writeFile(t, defPath, "package p\n\nfunc helper() {}\n\nfunc Foo() {}\n")
	writeFile(t, callerPath, "package p\n\nfunc Bar() { Foo() }\n")

	g := newSqliteGraph(t)
	idx := newTestIndexer(g)
	_, err := idx.Index(dir)
	require.NoError(t, err)
	idx.ResolveAll()

	barID := fnNodeID(t, g, "caller.go", "Bar")
	require.Equal(t, fnNodeID(t, g, "def.go", "Foo"), callTargetFrom(t, g, barID))
	stampLSP(t, g, callEdgeFrom(t, g, barID))
	return idx, g, dir, defPath
}

// The carry on the PRODUCTION backend. The in-memory tests above count the
// writes; this one proves the round trip is lossless where it actually
// matters: SQLite hands out freshly-decoded edge structs, so a carried edge is
// re-inserted from that decode rather than updated in place. Everything the
// edge row carries — target, origin, tier, confidence — has to come back.
func TestSQLiteIncrementalBodyOnlyEditCarriesIncomingEdgeExactly(t *testing.T) {
	idx, g, dir, defPath := sqliteRestubFixture(t)

	bumpMtime(t, defPath, "package p\n\nfunc helper() {}\n\nfunc Foo() { helper() }\n")
	_, err := idx.IncrementalReindexPaths(dir, []string{defPath})
	require.NoError(t, err)

	barID := fnNodeID(t, g, "caller.go", "Bar")
	edge := callEdgeFrom(t, g, barID)
	assert.Equal(t, fnNodeID(t, g, "def.go", "Foo"), edge.To,
		"the carried in-edge must still bind the definition")
	assert.Equal(t, graph.OriginLSPResolved, edge.Origin)
	assert.Equal(t, graph.ResolvedBy(graph.OriginLSPResolved), edge.Tier)
	assert.InDelta(t, 1.0, edge.Confidence, 1e-9)
	assert.False(t, graph.HasRestubProvenance(edge),
		"a carried edge must not be left holding a restub stash")
}

// Carrying an edge changes what the eviction destroys and what the AddBatch
// re-states, so the mutation receipt must still describe the mutation exactly
// — otherwise the commit falls back to a whole-graph resolve, which is the
// cost this whole path exists to avoid.
func TestSQLiteIncrementalBodyOnlyEditKeepsMutationReceiptExact(t *testing.T) {
	idx, g, dir, defPath := sqliteRestubFixture(t)

	bumpMtime(t, defPath, "package p\n\nfunc helper() {}\n\nfunc Foo() { helper() }\n")
	result, receipt, _, err := idx.incrementalReindexPathsWithReceiptMode(
		dir, []string{defPath}, incrementalPathMode{})
	require.NoError(t, err)
	require.Empty(t, result.FailedFiles)
	require.NotNil(t, receipt)
	assert.Truef(t, receipt.Complete,
		"carrying an in-edge across the eviction voided the mutation receipt (%s)",
		receipt.IncompleteReason)

	// The edge itself must still exist and still point at the definition: the
	// eviction deletes every edge incident to a doomed node, so a gate that
	// only skipped the write would leave nothing to find here.
	barID := fnNodeID(t, g, "caller.go", "Bar")
	assert.Equal(t, fnNodeID(t, g, "def.go", "Foo"), callEdgeFrom(t, g, barID).To)
}

// The other direction through the same production entrypoint: a signature
// change still restubs, and the caller still ends up bound to the new
// definition.
func TestIncrementalBatchSignatureChangeRestubsIncoming(t *testing.T) {
	idx, st, dir, defPath := restubPipelineFixture(t)

	bumpMtime(t, defPath, "package p\n\nfunc helper() {}\n\nfunc Foo(n int) int { return n }\n")
	_, err := idx.IncrementalReindexPaths(dir, []string{defPath})
	require.NoError(t, err)

	assert.GreaterOrEqual(t, st.restubWrites("Foo"), 1,
		"a signature change must still park the caller's in-edge for the incoming pass")

	fooID := fnNodeID(t, st, "def.go", "Foo")
	assert.Equal(t, fooID, callTargetFrom(t, st, fnNodeID(t, st, "caller.go", "Bar")),
		"the restubbed caller edge must be re-bound to the new definition")
}
