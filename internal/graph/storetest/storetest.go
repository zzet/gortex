// Package storetest provides a conformance test suite that every
// graph.Store implementation MUST pass. Each backend (in-memory,
// bbolt-on-disk, SQLite-on-disk, remote-network-client) has a thin
// _test.go that calls RunConformance(t, factory) and inherits the
// full battery.
//
// The contract this package encodes is the union of behaviour the
// rest of gortex depends on from *graph.Graph today. New Store
// implementations are expected to satisfy every test before they can
// be considered a drop-in replacement.
package storetest

import (
	"fmt"
	"path/filepath"
	"slices"
	"sort"
	"sync"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// Factory constructs a fresh, empty Store. RunConformance calls it
// many times across subtests; each invocation must yield an
// independent store with no leakage from previous runs. Backends with
// on-disk state should use t.TempDir() internally to isolate.
type Factory func(t *testing.T) graph.Store

// ReadSemantics names what a backend hands back from a read — the one
// behaviour the suite cannot discover by probing, because both answers are
// legitimate and neither is observable without knowing which to expect.
type ReadSemantics int

const (
	// ReadsDetached means every read materialises a fresh copy: mutating a
	// returned *Node or *Edge changes nothing in the store, and the change is
	// lost unless the caller hands it back through a write method. This is the
	// contract every backend the daemon actually runs on satisfies, and the
	// only one production code is allowed to assume.
	ReadsDetached ReadSemantics = iota

	// ReadsAliasStore means a read hands back the store's own live pointer, so
	// an in-place field mutation is immediately durable without any write call.
	// The in-memory staging graph behaves this way because it is a bag of
	// pointers, and the cold-index path leans on it as a scratch buffer.
	//
	// That is the ONE sanctioned exception. Production code must never rely on
	// aliasing: the same code running against a detached backend would silently
	// drop the mutation. Passes that need an attribute change to stick call the
	// explicit persistence capability (graph.EdgePersister and friends), which
	// is correct under both semantics.
	ReadsAliasStore
)

// Semantics declares the backend behaviours the conformance suite cannot infer
// from the Store interface alone. Every backend states its answer at the call
// site, so a divergence between backends is visible in source rather than
// buried in whichever implementation a reader happens to open first.
type Semantics struct {
	// Reads declares whether reads alias the store or return detached copies.
	Reads ReadSemantics

	// EvictsEmptyRepoPrefix declares what EvictRepo("") does. The empty prefix
	// is the standalone/single-repository namespace: nodes indexed without a
	// repository own it. The two backends disagree about whether it can be
	// evicted as a unit — the in-memory store treats the empty prefix as "not a
	// repository" and no-ops, while the SQLite store runs the ordinary
	// repo_prefix = ? delete and clears it.
	//
	// This field records that disagreement rather than resolving it. Changing
	// either side is a behaviour change for the standalone indexing path and
	// belongs in its own change, with its own justification; until then, no
	// caller should hand a user-supplied prefix to EvictRepo without checking
	// it is non-empty.
	EvictsEmptyRepoPrefix bool

	// AdjacencyLineOrdered declares what order GetOutEdges / GetInEdges hand
	// their edges back in. True means the backend sorts: out-edges ascending
	// by source line across kinds, in-edges grouped by kind. False means the
	// backend returns the edges in the order they were added.
	//
	// The SQLite store declares the order in SQL, riding the adjacency indexes
	// that already carry it. The in-memory staging graph hands back its
	// per-node append slice untouched: sorting it would tax the indexer's hot
	// staging path — the one place that store still exists to be fast — for a
	// property no staging caller needs.
	//
	// Both are stable within a backend, which is all a single-backend caller
	// needs. A caller that needs a line order ACROSS backends must sort the
	// result itself; the declaration exists so that requirement is visible
	// rather than discovered when a first-match helper picks a different edge
	// on one backend than the other.
	AdjacencyLineOrdered bool
}

// RunConformance runs the full conformance suite against the Store
// produced by factory. Backends invoke it from a _test.go in their
// own package, declaring their semantics alongside the factory.
func RunConformance(t *testing.T, factory Factory, semantics Semantics) {
	t.Helper()
	t.Run("AddGetNode", func(t *testing.T) { testAddGetNode(t, factory) })
	t.Run("AddGetEdge", func(t *testing.T) { testAddGetEdge(t, factory) })
	t.Run("AddNodeIdempotent", func(t *testing.T) { testAddNodeIdempotent(t, factory) })
	t.Run("AddEdgeIdempotent", func(t *testing.T) { testAddEdgeIdempotent(t, factory) })
	t.Run("AddEdgeLineDisambiguates", func(t *testing.T) { testAddEdgeLineDisambiguates(t, factory) })
	t.Run("AddBatch", func(t *testing.T) { testAddBatch(t, factory) })
	t.Run("RemoveEdge", func(t *testing.T) { testRemoveEdge(t, factory) })
	t.Run("EvictFile", func(t *testing.T) { testEvictFile(t, factory) })
	t.Run("EvictFile_NoNodes", func(t *testing.T) { testEvictFileNoNodes(t, factory) })
	t.Run("EvictRepo", func(t *testing.T) { testEvictRepo(t, factory) })
	t.Run("EvictRepo_NoNodes", func(t *testing.T) { testEvictRepoNoNodes(t, factory) })
	t.Run("NodeAndEdgeCount", func(t *testing.T) { testNodeAndEdgeCount(t, factory) })
	t.Run("AllNodesAndEdges", func(t *testing.T) { testAllNodesAndEdges(t, factory) })
	t.Run("FindNodesByName", func(t *testing.T) { testFindNodesByName(t, factory) })
	t.Run("FindNodesByNameInRepo", func(t *testing.T) { testFindNodesByNameInRepo(t, factory) })
	t.Run("FindNodesByNameContaining", func(t *testing.T) { testFindNodesByNameContaining(t, factory) })
	t.Run("GetFileNodes", func(t *testing.T) { testGetFileNodes(t, factory) })
	t.Run("GetRepoNodes", func(t *testing.T) { testGetRepoNodes(t, factory) })
	t.Run("GetRepoEdges", func(t *testing.T) { testGetRepoEdges(t, factory) })
	t.Run("GetNodeByQualName", func(t *testing.T) { testGetNodeByQualName(t, factory) })
	t.Run("DuplicateQualNameAcrossRepos", func(t *testing.T) { testDuplicateQualNameAcrossRepos(t, factory) })
	t.Run("DuplicateQualNameWithinRepo", func(t *testing.T) { testDuplicateQualNameWithinRepo(t, factory) })
	t.Run("Stats", func(t *testing.T) { testStats(t, factory) })
	t.Run("RepoStats", func(t *testing.T) { testRepoStats(t, factory) })
	t.Run("RepoPrefixes", func(t *testing.T) { testRepoPrefixes(t, factory) })
	t.Run("SetEdgeProvenance", func(t *testing.T) { testSetEdgeProvenance(t, factory) })
	t.Run("SetEdgeProvenanceBatch", func(t *testing.T) { testSetEdgeProvenanceBatch(t, factory) })
	t.Run("ReindexEdge", func(t *testing.T) { testReindexEdge(t, factory) })
	t.Run("ReindexEdges", func(t *testing.T) { testReindexEdges(t, factory) })
	t.Run("ReindexConvergencePayload", func(t *testing.T) { RunReindexConvergencePayload(t, factory) })
	t.Run("ReindexMovingSiblings", func(t *testing.T) { RunReindexMovingSiblings(t, factory) })
	t.Run("Concurrency", func(t *testing.T) { testConcurrency(t, factory) })
	t.Run("EdgeIdentityRevisions", func(t *testing.T) { testEdgeIdentityRevisions(t, factory) })
	t.Run("VerifyEdgeIdentities", func(t *testing.T) { testVerifyEdgeIdentities(t, factory) })
	t.Run("RepoMemoryEstimate", func(t *testing.T) { testRepoMemoryEstimate(t, factory) })
	t.Run("AllRepoMemoryEstimates", func(t *testing.T) { testAllRepoMemoryEstimates(t, factory) })
	t.Run("MetaPreserved", func(t *testing.T) { testMetaPreserved(t, factory) })
	t.Run("EmptyStore", func(t *testing.T) { testEmptyStore(t, factory) })
	t.Run("EdgesByKind", func(t *testing.T) { testEdgesByKind(t, factory) })
	t.Run("NodesByKind", func(t *testing.T) { testNodesByKind(t, factory) })
	t.Run("EdgesWithUnresolvedTarget", func(t *testing.T) { testEdgesWithUnresolvedTarget(t, factory) })
	t.Run("FnValuePlaceholderEdges", func(t *testing.T) { testFnValuePlaceholderEdges(t, factory) })
	t.Run("ValueRefPlaceholderEdges", func(t *testing.T) { testValueRefPlaceholderEdges(t, factory) })
	t.Run("NodeLightScanner", func(t *testing.T) { testNodeLightScanner(t, factory) })
	t.Run("LightEdgeScanner", func(t *testing.T) { testLightEdgeScanner(t, factory) })
	t.Run("GetNodesByIDs", func(t *testing.T) { testGetNodesByIDs(t, factory) })
	t.Run("FindNodesByNames", func(t *testing.T) { testFindNodesByNames(t, factory) })
	t.Run("GetEdgesByNodeIDs", func(t *testing.T) { testGetEdgesByNodeIDs(t, factory) })
	t.Run("GetRepoNodesByLanguage", func(t *testing.T) { testGetRepoNodesByLanguage(t, factory) })
	t.Run("DistinctExternalTargets", func(t *testing.T) { testDistinctExternalTargets(t, factory) })
	t.Run("SymbolBundleSearcher", func(t *testing.T) { testSymbolBundleSearcher(t, factory) })
	t.Run("DeadCodeCandidator", func(t *testing.T) { testDeadCodeCandidator(t, factory) })
	t.Run("ScopedDeadCodeReader", func(t *testing.T) { testScopedDeadCodeReader(t, factory) })
	t.Run("FilteredScopedDeadCodeReader", func(t *testing.T) { testFilteredScopedDeadCodeReader(t, factory) })
	t.Run("IfaceImplementsScanner", func(t *testing.T) { testIfaceImplementsScanner(t, factory) })
	t.Run("NodeDegreeAggregator", func(t *testing.T) { testNodeDegreeAggregator(t, factory) })
	t.Run("NodeFanAggregator", func(t *testing.T) { testNodeFanAggregator(t, factory) })
	t.Run("FileImporters", func(t *testing.T) { testFileImporters(t, factory) })
	t.Run("InEdgeCounter", func(t *testing.T) { testInEdgeCounter(t, factory) })
	t.Run("NodesInFilesByKindFinder", func(t *testing.T) { testNodesInFilesByKindFinder(t, factory) })
	t.Run("EdgesByKindsScanner", func(t *testing.T) { testEdgesByKindsScanner(t, factory) })
	t.Run("NodesByKindsScanner", func(t *testing.T) { testNodesByKindsScanner(t, factory) })
	t.Run("EdgeKindCounter", func(t *testing.T) { testEdgeKindCounter(t, factory) })
	t.Run("CrossRepoEdgeAggregator", func(t *testing.T) { testCrossRepoEdgeAggregator(t, factory) })
	t.Run("FilteredScopedOutline", func(t *testing.T) { RunFilteredScopedOutline(t, factory) })
	t.Run("ScopedOutline", func(t *testing.T) { RunScopedOutline(t, factory) })
	t.Run("FileImportAggregator", func(t *testing.T) { testFileImportAggregator(t, factory) })
	t.Run("InDegreeForNodes", func(t *testing.T) { testInDegreeForNodes(t, factory) })
	t.Run("ReachableForwardByKinds", func(t *testing.T) { testReachableForwardByKinds(t, factory) })
	t.Run("BFS", func(t *testing.T) { testBFS(t, factory) })
	t.Run("ThrowerErrorSurfacer", func(t *testing.T) { testThrowerErrorSurfacer(t, factory) })
	t.Run("EdgeAdjacencyForKinds", func(t *testing.T) { testEdgeAdjacencyForKinds(t, factory) })
	t.Run("CommunityCrossingsByKind", func(t *testing.T) { testCommunityCrossingsByKind(t, factory) })
	t.Run("NodeIDsByKinds", func(t *testing.T) { testNodeIDsByKinds(t, factory) })
	t.Run("MemberMethodsByType", func(t *testing.T) { testMemberMethodsByType(t, factory) })
	t.Run("StructuralParentEdges", func(t *testing.T) { testStructuralParentEdges(t, factory) })
	t.Run("CrossRepoCandidates", func(t *testing.T) { testCrossRepoCandidates(t, factory) })
	t.Run("ExtractCandidates", func(t *testing.T) { testExtractCandidates(t, factory) })
	t.Run("FileSymbolNamesByPaths", func(t *testing.T) { testFileSymbolNamesByPaths(t, factory) })
	t.Run("ClassHierarchyTraverser", func(t *testing.T) { testClassHierarchyTraverser(t, factory) })
	t.Run("FileEditingContext", func(t *testing.T) { testFileEditingContext(t, factory) })
	t.Run("NodeDegreeByKinds", func(t *testing.T) { testNodeDegreeByKinds(t, factory) })
	t.Run("PathPrefixNativeSeparators", func(t *testing.T) { testPathPrefixNativeSeparators(t, factory) })
	t.Run("CloneShingleSidecar", func(t *testing.T) { testCloneShingleSidecar(t, factory) })
	t.Run("ChurnEnrichmentSidecar", func(t *testing.T) { testChurnEnrichmentSidecar(t, factory) })
	t.Run("CoverageEnrichmentSidecar", func(t *testing.T) { testCoverageEnrichmentSidecar(t, factory) })
	t.Run("ReleaseEnrichmentSidecar", func(t *testing.T) { testReleaseEnrichmentSidecar(t, factory) })
	t.Run("BlameEnrichmentSidecar", func(t *testing.T) { testBlameEnrichmentSidecar(t, factory) })
	t.Run("ContractBridgeRoundTrip", func(t *testing.T) { testContractBridgeRoundTrip(t, factory) })
	t.Run("PrefixDiagnostics", func(t *testing.T) { testPrefixDiagnostics(t, factory) })
	t.Run("EdgePersistRoundTrip", func(t *testing.T) { testEdgePersistRoundTrip(t, factory) })
	t.Run("ReadAliasing", func(t *testing.T) { testReadAliasing(t, factory, semantics) })
	t.Run("IteratorReentrancy", func(t *testing.T) { testIteratorReentrancy(t, factory) })
	t.Run("ConcurrentReadsDuringWrite", func(t *testing.T) { testConcurrentReadsDuringWrite(t, factory) })
	t.Run("ReadOrdering", func(t *testing.T) { testReadOrdering(t, factory, semantics) })
	t.Run("AttributeOnlyReindex", func(t *testing.T) { testAttributeOnlyReindex(t, factory) })
	t.Run("MutationReceipts", func(t *testing.T) { testMutationReceipts(t, factory) })
	t.Run("EmptyRepoPrefix", func(t *testing.T) { testEmptyRepoPrefix(t, factory, semantics) })
}

// testPrefixDiagnostics is the cross-backend fence for the repo-ownership
// audit. Every backend must classify the same node set identically —
// store_sqlite answers with SQL predicates while the in-memory store walks
// shards through graph.ClassifyNodePrefix, and a divergence there would make
// the ghost-graph detector report clean on exactly the backend the daemon
// runs.
//
// Backends that implement neither the native capability nor AllNodes skip.
func testPrefixDiagnostics(t *testing.T, factory Factory) {
	t.Helper()
	s := factory(t)

	// One owned code node, one unowned (single-repo-mode) code node, one
	// misprefixed node whose stamped repo its identity does not carry, and
	// every intentional namespace/path exception that must not be audited.
	s.AddNode(mkRepoNode("r1/a.go::Owned", "Owned", "r1/a.go", "r1", graph.KindFunction))
	s.AddNode(mkNode("b.go::Unowned", "Unowned", "b.go", graph.KindFunction))
	s.AddNode(mkRepoNode("c.go::Misprefixed", "Misprefixed", "c.go", "r1", graph.KindFunction))
	s.AddNode(mkNode("dep::go.uber.org/zap::Logger", "Logger", "", graph.KindFunction))
	s.AddNode(mkNode("external::go:crypto/sha256::Size", "Size", "external::go:crypto/sha256", graph.KindConstant))
	s.AddNode(mkNode("external-call::npm:lodash", "lodash", "external-call::npm:lodash", graph.KindModule))
	s.AddNode(mkRepoNode("contract::http::GET::/health", "GET /health", "r1/api.go", "r1", graph.KindContract))
	s.AddNode(mkRepoNode("contract-bridge::request::response", "request → response", "contracts://bridges", "r1", graph.KindContractBridge))
	s.AddNode(mkRepoNode("topic::nats::go", "go", "r1/main.go", "r1", graph.KindTopic))
	// The memory projection stamps the owning repo but mints the identity as
	// `rationale::<memory-id>` over a virtual path, so a rationale node can
	// never carry its prefix. Auditing it reported a misprefixed node on every
	// healthy graph that had a repo-scoped memory.
	s.AddNode(mkRepoNode("rationale::mem3539475d73f80040", "why the resolver stubs res://", ".gortex/rationale", "r1", graph.KindRationale))

	d, ok := graph.ReadPrefixDiagnostics(s, 5)
	if !ok {
		t.Skip("backend implements neither PrefixDiagnosticsReader nor AllNodes")
	}

	if d.OwnedCodeNodes != 1 {
		t.Errorf("OwnedCodeNodes = %d, want 1 (%s)", d.OwnedCodeNodes, d.Summary())
	}
	if d.UnownedCodeNodes != 1 {
		t.Errorf("UnownedCodeNodes = %d, want 1 — synthetic virtual paths must not count (%s)",
			d.UnownedCodeNodes, d.Summary())
	}
	if d.MisprefixedNodes != 1 {
		t.Errorf("MisprefixedNodes = %d, want 1 (%s)", d.MisprefixedNodes, d.Summary())
	}
	if !d.Mixed() {
		t.Errorf("Mixed() = false with both populations present (%s)", d.Summary())
	}
	if d.Clean() {
		t.Errorf("Clean() = true for a mixed, misprefixed graph (%s)", d.Summary())
	}
	if len(d.UnownedSamples) != 1 || d.UnownedSamples[0] != "b.go::Unowned" {
		t.Errorf("UnownedSamples = %v, want [b.go::Unowned]", d.UnownedSamples)
	}
}

// -- fixture helpers ---------------------------------------------------

func mkNode(id, name, file string, kind graph.NodeKind) *graph.Node {
	return &graph.Node{
		ID:        id,
		Kind:      kind,
		Name:      name,
		FilePath:  file,
		StartLine: 1,
		EndLine:   10,
		Language:  "go",
	}
}

func mkRepoNode(id, name, file, repo string, kind graph.NodeKind) *graph.Node {
	n := mkNode(id, name, file, kind)
	n.RepoPrefix = repo
	return n
}

func mkEdge(from, to string, kind graph.EdgeKind) *graph.Edge {
	return &graph.Edge{
		From: from, To: to, Kind: kind,
		FilePath: "test.go", Line: 1,
		Confidence: 1.0,
		Origin:     graph.OriginASTResolved,
	}
}

func sortNodeIDs(nodes []*graph.Node) []string {
	ids := make([]string, 0, len(nodes))
	for _, n := range nodes {
		if n != nil {
			ids = append(ids, n.ID)
		}
	}
	sort.Strings(ids)
	return ids
}

func sortEdgeKeys(edges []*graph.Edge) []string {
	keys := make([]string, 0, len(edges))
	for _, e := range edges {
		if e != nil {
			keys = append(keys, fmt.Sprintf("%s|%s|%s|%d", e.From, e.To, e.Kind, e.Line))
		}
	}
	sort.Strings(keys)
	return keys
}

// -- individual subtests ----------------------------------------------

func testAddGetNode(t *testing.T, factory Factory) {
	t.Helper()
	s := factory(t)
	n := mkNode("a.go::Foo", "Foo", "a.go", graph.KindFunction)
	s.AddNode(n)
	got := s.GetNode("a.go::Foo")
	if got == nil {
		t.Fatalf("GetNode returned nil for inserted node")
		return
	}
	if got.Name != "Foo" || got.FilePath != "a.go" || got.Kind != graph.KindFunction {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
	if s.GetNode("missing") != nil {
		t.Fatalf("GetNode should return nil for missing key")
	}
}

func testAddGetEdge(t *testing.T, factory Factory) {
	t.Helper()
	s := factory(t)
	s.AddNode(mkNode("a", "A", "x.go", graph.KindFunction))
	s.AddNode(mkNode("b", "B", "x.go", graph.KindFunction))
	s.AddEdge(mkEdge("a", "b", graph.EdgeCalls))

	out := s.GetOutEdges("a")
	if len(out) != 1 || out[0].To != "b" {
		t.Fatalf("GetOutEdges(a) = %+v, want one edge to b", out)
	}
	in := s.GetInEdges("b")
	if len(in) != 1 || in[0].From != "a" {
		t.Fatalf("GetInEdges(b) = %+v, want one edge from a", in)
	}
}

func testAddNodeIdempotent(t *testing.T, factory Factory) {
	t.Helper()
	s := factory(t)
	n := mkNode("dup", "Dup", "x.go", graph.KindFunction)
	s.AddNode(n)
	s.AddNode(n)
	s.AddNode(n)
	if s.NodeCount() != 1 {
		t.Fatalf("NodeCount after 3x add = %d, want 1", s.NodeCount())
	}
}

func testAddEdgeIdempotent(t *testing.T, factory Factory) {
	t.Helper()
	s := factory(t)
	s.AddNode(mkNode("a", "A", "x.go", graph.KindFunction))
	s.AddNode(mkNode("b", "B", "x.go", graph.KindFunction))
	e := mkEdge("a", "b", graph.EdgeCalls)
	s.AddEdge(e)
	s.AddEdge(e)
	s.AddEdge(e)
	if got := len(s.GetOutEdges("a")); got != 1 {
		t.Fatalf("OutEdges after 3x add = %d, want 1", got)
	}
}

func testAddEdgeLineDisambiguates(t *testing.T, factory Factory) {
	t.Helper()
	s := factory(t)
	s.AddNode(mkNode("a", "A", "x.go", graph.KindFunction))
	s.AddNode(mkNode("b", "B", "x.go", graph.KindFunction))
	e1 := mkEdge("a", "b", graph.EdgeCalls)
	e1.Line = 1
	e2 := mkEdge("a", "b", graph.EdgeCalls)
	e2.Line = 5
	s.AddEdge(e1)
	s.AddEdge(e2)
	if got := len(s.GetOutEdges("a")); got != 2 {
		t.Fatalf("OutEdges with different lines = %d, want 2", got)
	}
}

func testAddBatch(t *testing.T, factory Factory) {
	t.Helper()
	s := factory(t)
	nodes := []*graph.Node{
		mkNode("a", "A", "x.go", graph.KindFunction),
		mkNode("b", "B", "x.go", graph.KindFunction),
		mkNode("c", "C", "y.go", graph.KindType),
	}
	edges := []*graph.Edge{
		mkEdge("a", "b", graph.EdgeCalls),
		mkEdge("b", "c", graph.EdgeReferences),
	}
	s.AddBatch(nodes, edges)
	if s.NodeCount() != 3 {
		t.Fatalf("NodeCount after AddBatch = %d, want 3", s.NodeCount())
	}
	if s.EdgeCount() != 2 {
		t.Fatalf("EdgeCount after AddBatch = %d, want 2", s.EdgeCount())
	}
}

func testRemoveEdge(t *testing.T, factory Factory) {
	t.Helper()
	s := factory(t)
	s.AddNode(mkNode("a", "A", "x.go", graph.KindFunction))
	s.AddNode(mkNode("b", "B", "x.go", graph.KindFunction))
	e := mkEdge("a", "b", graph.EdgeCalls)
	s.AddEdge(e)
	if !s.RemoveEdge("a", "b", graph.EdgeCalls) {
		t.Fatalf("RemoveEdge returned false for existing edge")
	}
	if len(s.GetOutEdges("a")) != 0 {
		t.Fatalf("OutEdges after RemoveEdge = nonzero")
	}
	if len(s.GetInEdges("b")) != 0 {
		t.Fatalf("InEdges after RemoveEdge = nonzero")
	}
	// Removing non-existent should report false but not panic.
	if s.RemoveEdge("a", "b", graph.EdgeCalls) {
		t.Fatalf("RemoveEdge returned true for missing edge")
	}
}

func testEvictFile(t *testing.T, factory Factory) {
	t.Helper()
	s := factory(t)
	s.AddNode(mkNode("a.go::Foo", "Foo", "a.go", graph.KindFunction))
	s.AddNode(mkNode("a.go::Bar", "Bar", "a.go", graph.KindFunction))
	s.AddNode(mkNode("b.go::Baz", "Baz", "b.go", graph.KindFunction))
	s.AddEdge(mkEdge("a.go::Foo", "a.go::Bar", graph.EdgeCalls))
	s.AddEdge(mkEdge("a.go::Bar", "b.go::Baz", graph.EdgeCalls))

	nodesRemoved, edgesRemoved := s.EvictFile("a.go")
	if nodesRemoved != 2 {
		t.Fatalf("EvictFile nodesRemoved = %d, want 2", nodesRemoved)
	}
	if edgesRemoved == 0 {
		t.Fatalf("EvictFile edgesRemoved should be > 0")
	}
	if s.GetNode("a.go::Foo") != nil {
		t.Fatalf("evicted node still present")
	}
	if s.GetNode("b.go::Baz") == nil {
		t.Fatalf("unrelated node was evicted")
	}
}

func testEvictFileNoNodes(t *testing.T, factory Factory) {
	t.Helper()
	s := factory(t)
	n, e := s.EvictFile("nonexistent.go")
	if n != 0 || e != 0 {
		t.Fatalf("EvictFile on empty file returned (%d, %d), want (0, 0)", n, e)
	}
}

func testEvictRepo(t *testing.T, factory Factory) {
	t.Helper()
	s := factory(t)
	s.AddNode(mkRepoNode("r1/a.go::Foo", "Foo", "r1/a.go", "r1", graph.KindFunction))
	s.AddNode(mkRepoNode("r1/b.go::Bar", "Bar", "r1/b.go", "r1", graph.KindFunction))
	s.AddNode(mkRepoNode("r2/x.go::Baz", "Baz", "r2/x.go", "r2", graph.KindFunction))
	s.AddEdge(mkEdge("r1/a.go::Foo", "r1/b.go::Bar", graph.EdgeCalls))

	nodesRemoved, edgesRemoved := s.EvictRepo("r1")
	if nodesRemoved != 2 {
		t.Fatalf("EvictRepo nodesRemoved = %d, want 2", nodesRemoved)
	}
	if edgesRemoved == 0 {
		t.Fatalf("EvictRepo edgesRemoved should be > 0")
	}
	if s.GetNode("r1/a.go::Foo") != nil {
		t.Fatalf("r1 node still present")
	}
	if s.GetNode("r2/x.go::Baz") == nil {
		t.Fatalf("r2 node was evicted")
	}
}

func testEvictRepoNoNodes(t *testing.T, factory Factory) {
	t.Helper()
	s := factory(t)
	n, e := s.EvictRepo("nonexistent-repo")
	if n != 0 || e != 0 {
		t.Fatalf("EvictRepo on missing repo returned (%d, %d), want (0, 0)", n, e)
	}
}

func testNodeAndEdgeCount(t *testing.T, factory Factory) {
	t.Helper()
	s := factory(t)
	if s.NodeCount() != 0 || s.EdgeCount() != 0 {
		t.Fatalf("empty store reports nonzero counts")
	}
	s.AddNode(mkNode("a", "A", "x.go", graph.KindFunction))
	s.AddNode(mkNode("b", "B", "x.go", graph.KindFunction))
	s.AddEdge(mkEdge("a", "b", graph.EdgeCalls))
	if s.NodeCount() != 2 {
		t.Fatalf("NodeCount = %d, want 2", s.NodeCount())
	}
	if s.EdgeCount() != 1 {
		t.Fatalf("EdgeCount = %d, want 1", s.EdgeCount())
	}
}

func testAllNodesAndEdges(t *testing.T, factory Factory) {
	t.Helper()
	s := factory(t)
	s.AddNode(mkNode("a", "A", "x.go", graph.KindFunction))
	s.AddNode(mkNode("b", "B", "y.go", graph.KindType))
	s.AddEdge(mkEdge("a", "b", graph.EdgeReferences))

	gotN := sortNodeIDs(s.AllNodes())
	wantN := []string{"a", "b"}
	if fmt.Sprint(gotN) != fmt.Sprint(wantN) {
		t.Fatalf("AllNodes = %v, want %v", gotN, wantN)
	}
	gotE := sortEdgeKeys(s.AllEdges())
	if len(gotE) != 1 {
		t.Fatalf("AllEdges = %v, want one entry", gotE)
	}
}

func testFindNodesByName(t *testing.T, factory Factory) {
	t.Helper()
	s := factory(t)
	s.AddNode(mkNode("a.go::Foo", "Foo", "a.go", graph.KindFunction))
	s.AddNode(mkNode("b.go::Foo", "Foo", "b.go", graph.KindFunction))
	s.AddNode(mkNode("c.go::Bar", "Bar", "c.go", graph.KindFunction))
	got := sortNodeIDs(s.FindNodesByName("Foo"))
	want := []string{"a.go::Foo", "b.go::Foo"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("FindNodesByName(Foo) = %v, want %v", got, want)
	}
	if len(s.FindNodesByName("MissingName")) != 0 {
		t.Fatalf("FindNodesByName for missing name should be empty")
	}
}

func testFindNodesByNameInRepo(t *testing.T, factory Factory) {
	t.Helper()
	s := factory(t)
	s.AddNode(mkRepoNode("r1/a.go::Foo", "Foo", "r1/a.go", "r1", graph.KindFunction))
	s.AddNode(mkRepoNode("r2/a.go::Foo", "Foo", "r2/a.go", "r2", graph.KindFunction))
	got := sortNodeIDs(s.FindNodesByNameInRepo("Foo", "r1"))
	want := []string{"r1/a.go::Foo"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("FindNodesByNameInRepo(Foo, r1) = %v, want %v", got, want)
	}
}

func testFindNodesByNameContaining(t *testing.T, factory Factory) {
	t.Helper()
	s := factory(t)
	// Three "log"-containing names + one unrelated.
	s.AddNode(mkNode("a.go::Login", "Login", "a.go", graph.KindFunction))
	s.AddNode(mkNode("b.go::LoginHandler", "LoginHandler", "b.go", graph.KindFunction))
	s.AddNode(mkNode("c.go::Logout", "Logout", "c.go", graph.KindFunction))
	s.AddNode(mkNode("d.go::Unrelated", "Unrelated", "d.go", graph.KindFunction))

	// Case-insensitive substring match should return exactly the 3
	// "log"-bearing nodes.
	got := sortNodeIDs(s.FindNodesByNameContaining("log", 10))
	want := []string{"a.go::Login", "b.go::LoginHandler", "c.go::Logout"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("FindNodesByNameContaining(log, 10) = %v, want %v", got, want)
	}

	// Mixed-case query — must still match (case-insensitive).
	gotUpper := sortNodeIDs(s.FindNodesByNameContaining("LOG", 10))
	if fmt.Sprint(gotUpper) != fmt.Sprint(want) {
		t.Fatalf("FindNodesByNameContaining(LOG, 10) = %v, want %v", gotUpper, want)
	}

	// Limit is honoured. Asking for 2 must return at most 2.
	gotLimited := s.FindNodesByNameContaining("log", 2)
	if len(gotLimited) != 2 {
		t.Fatalf("FindNodesByNameContaining(log, 2) returned %d, want 2", len(gotLimited))
	}

	// Empty needle returns nothing — never the whole graph.
	if got := s.FindNodesByNameContaining("", 10); len(got) != 0 {
		t.Fatalf("FindNodesByNameContaining(\"\") returned %d, want 0", len(got))
	}

	// No match — empty slice.
	if got := s.FindNodesByNameContaining("nonexistent_substring_xyz", 10); len(got) != 0 {
		t.Fatalf("FindNodesByNameContaining(no-match) returned %d, want 0", len(got))
	}
}

func testGetFileNodes(t *testing.T, factory Factory) {
	t.Helper()
	s := factory(t)
	s.AddNode(mkNode("a.go::Foo", "Foo", "a.go", graph.KindFunction))
	s.AddNode(mkNode("a.go::Bar", "Bar", "a.go", graph.KindFunction))
	s.AddNode(mkNode("b.go::Baz", "Baz", "b.go", graph.KindFunction))
	got := sortNodeIDs(s.GetFileNodes("a.go"))
	want := []string{"a.go::Bar", "a.go::Foo"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("GetFileNodes(a.go) = %v, want %v", got, want)
	}
}

func testGetRepoNodes(t *testing.T, factory Factory) {
	t.Helper()
	s := factory(t)
	s.AddNode(mkRepoNode("r1/a.go::Foo", "Foo", "r1/a.go", "r1", graph.KindFunction))
	s.AddNode(mkRepoNode("r1/b.go::Bar", "Bar", "r1/b.go", "r1", graph.KindFunction))
	s.AddNode(mkRepoNode("r2/x.go::Baz", "Baz", "r2/x.go", "r2", graph.KindFunction))
	got := sortNodeIDs(s.GetRepoNodes("r1"))
	want := []string{"r1/a.go::Foo", "r1/b.go::Bar"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("GetRepoNodes(r1) = %v, want %v", got, want)
	}
}

// testGetRepoEdges asserts that GetRepoEdges returns every edge whose
// SOURCE node carries the requested RepoPrefix, regardless of where
// the target lives — same-repo intra edges, cross-repo edges (source
// in r1 → target in r2), AND unresolved::* targets all count. Edges
// whose source is in a different repo (or unscoped) MUST NOT appear.
// Empty prefix returns nil so callers don't accidentally fall through
// to a full-graph scan.
func testGetRepoEdges(t *testing.T, factory Factory) {
	t.Helper()
	s := factory(t)
	// r1 has two nodes that originate outgoing edges; r2 has a target
	// node and one of its own source nodes.
	s.AddNode(mkRepoNode("r1/a.go::Foo", "Foo", "r1/a.go", "r1", graph.KindFunction))
	s.AddNode(mkRepoNode("r1/b.go::Bar", "Bar", "r1/b.go", "r1", graph.KindFunction))
	s.AddNode(mkRepoNode("r2/x.go::Baz", "Baz", "r2/x.go", "r2", graph.KindFunction))
	s.AddNode(mkRepoNode("r2/y.go::Qux", "Qux", "r2/y.go", "r2", graph.KindFunction))

	// r1-intra (Foo → Bar) — same repo.
	s.AddEdge(mkEdge("r1/a.go::Foo", "r1/b.go::Bar", graph.EdgeCalls))
	// r1 → r2 cross-repo (Foo → Baz).
	s.AddEdge(mkEdge("r1/a.go::Foo", "r2/x.go::Baz", graph.EdgeCalls))
	// r1 → unresolved (Bar → unresolved::Missing) — counts because
	// source is in r1.
	s.AddEdge(mkEdge("r1/b.go::Bar", "unresolved::Missing", graph.EdgeCalls))
	// r2-intra (Qux → Baz) — MUST NOT appear in r1's slice.
	s.AddEdge(mkEdge("r2/y.go::Qux", "r2/x.go::Baz", graph.EdgeCalls))
	// r2 → r1 cross-repo (Qux → Foo) — MUST NOT appear in r1's slice
	// because the source is in r2.
	s.AddEdge(mkEdge("r2/y.go::Qux", "r1/a.go::Foo", graph.EdgeCalls))

	gotR1 := sortEdgeKeys(s.GetRepoEdges("r1"))
	wantR1 := sortEdgeKeys([]*graph.Edge{
		mkEdge("r1/a.go::Foo", "r1/b.go::Bar", graph.EdgeCalls),
		mkEdge("r1/a.go::Foo", "r2/x.go::Baz", graph.EdgeCalls),
		mkEdge("r1/b.go::Bar", "unresolved::Missing", graph.EdgeCalls),
	})
	if fmt.Sprint(gotR1) != fmt.Sprint(wantR1) {
		t.Fatalf("GetRepoEdges(r1) =\n  %v\nwant\n  %v", gotR1, wantR1)
	}

	gotR2 := sortEdgeKeys(s.GetRepoEdges("r2"))
	wantR2 := sortEdgeKeys([]*graph.Edge{
		mkEdge("r2/y.go::Qux", "r2/x.go::Baz", graph.EdgeCalls),
		mkEdge("r2/y.go::Qux", "r1/a.go::Foo", graph.EdgeCalls),
	})
	if fmt.Sprint(gotR2) != fmt.Sprint(wantR2) {
		t.Fatalf("GetRepoEdges(r2) =\n  %v\nwant\n  %v", gotR2, wantR2)
	}

	// Empty prefix MUST return nothing (use AllEdges for the global
	// view). Disk backends must not fall through to a full scan.
	if got := s.GetRepoEdges(""); len(got) != 0 {
		t.Fatalf("GetRepoEdges(\"\") = %d edges, want 0", len(got))
	}

	// Unknown prefix MUST return empty (no panic, no fallthrough).
	if got := s.GetRepoEdges("nope"); len(got) != 0 {
		t.Fatalf("GetRepoEdges(nope) = %d edges, want 0", len(got))
	}
}

func testGetNodeByQualName(t *testing.T, factory Factory) {
	t.Helper()
	s := factory(t)
	n := mkNode("a.go::Foo", "Foo", "a.go", graph.KindFunction)
	n.QualName = "pkg.Foo"
	s.AddNode(n)
	got := s.GetNodeByQualName("pkg.Foo")
	if got == nil || got.ID != "a.go::Foo" {
		t.Fatalf("GetNodeByQualName(pkg.Foo) = %v, want a.go::Foo", got)
	}
	if s.GetNodeByQualName("missing.Qual") != nil {
		t.Fatalf("GetNodeByQualName missing should be nil")
	}
}

func testDuplicateQualNameAcrossRepos(t *testing.T, factory Factory) {
	t.Helper()
	s := factory(t)

	const qualName = "ClusterRole/prometheus"
	first := mkRepoNode(
		"repo-a/k8s::ClusterRole::_default::prometheus",
		"prometheus", "repo-a/deploy/rbac.yaml", "repo-a", graph.KindResource,
	)
	first.QualName = qualName
	second := mkRepoNode(
		"repo-b/k8s::ClusterRole::_default::prometheus",
		"prometheus", "repo-b/manifests/roles.yaml", "repo-b", graph.KindResource,
	)
	second.QualName = qualName

	s.AddNode(first)
	s.AddNode(second)

	for _, want := range []*graph.Node{first, second} {
		got := s.GetNode(want.ID)
		if got == nil {
			// Fatalf ends the test, but static analysis reasons about the
			// testing helper as an ordinary call and reads the field access
			// below as reachable with a nil pointer. The explicit return
			// makes the dereference provably safe instead of conventionally
			// safe — same rule at every bail-out-then-dereference below.
			t.Fatalf("GetNode(%q) = nil after adding the same qual_name in another repo", want.ID)
			return
		}
		if got.RepoPrefix != want.RepoPrefix {
			t.Fatalf("GetNode(%q).RepoPrefix = %q, want %q", want.ID, got.RepoPrefix, want.RepoPrefix)
		}
	}

	gotByName := s.FindNodesByName("prometheus")
	gotRepos := make(map[string]bool, len(gotByName))
	for _, got := range gotByName {
		if got != nil && got.QualName == qualName {
			gotRepos[got.RepoPrefix] = true
		}
	}
	for _, wantRepo := range []string{"repo-a", "repo-b"} {
		if !gotRepos[wantRepo] {
			t.Errorf("FindNodesByName(prometheus) repos = %v, missing %q", gotRepos, wantRepo)
		}
	}

	gotByQual := s.GetNodesByQualNames([]string{qualName, qualName, "", "missing/qualified-name"})
	hits := gotByQual[qualName]
	wantIDs := []string{first.ID, second.ID}
	if len(hits) != len(wantIDs) {
		t.Fatalf("GetNodesByQualNames duplicate input returned %d nodes, want %d: %v", len(hits), len(wantIDs), sortNodeIDs(hits))
	}
	for i, wantID := range wantIDs {
		if hits[i] == nil || hits[i].ID != wantID {
			t.Fatalf("GetNodesByQualNames[%q][%d] = %#v, want ID %q (stable ID order)", qualName, i, hits[i], wantID)
		}
	}
	for _, absent := range []string{"", "missing/qualified-name"} {
		if _, ok := gotByQual[absent]; ok {
			t.Errorf("GetNodesByQualNames unexpectedly returned absent key %q: %v", absent, sortNodeIDs(gotByQual[absent]))
		}
	}
}

func testDuplicateQualNameWithinRepo(t *testing.T, factory Factory) {
	t.Helper()
	s := factory(t)

	const qualName = "Deployment/api"
	for _, namespace := range []string{"blue", "green"} {
		n := mkRepoNode(
			"repo-a/k8s::Deployment::"+namespace+"::api",
			"api", "repo-a/manifests/"+namespace+".yaml", "repo-a", graph.KindResource,
		)
		n.QualName = qualName
		s.AddNode(n)
	}

	got := s.FindNodesByName("api")
	if len(got) != 2 {
		t.Fatalf("FindNodesByName(api) returned %d nodes, want both namespace-specific resources", len(got))
	}
	for _, namespace := range []string{"blue", "green"} {
		id := "repo-a/k8s::Deployment::" + namespace + "::api"
		if s.GetNode(id) == nil {
			t.Errorf("GetNode(%q) = nil after adding a duplicate qual_name in the same repo", id)
		}
	}
}

func testStats(t *testing.T, factory Factory) {
	t.Helper()
	s := factory(t)
	s.AddNode(mkNode("a", "A", "x.go", graph.KindFunction))
	s.AddNode(mkNode("b", "B", "y.go", graph.KindType))
	s.AddEdge(mkEdge("a", "b", graph.EdgeReferences))
	st := s.Stats()
	if st.TotalNodes != 2 || st.TotalEdges != 1 {
		t.Fatalf("Stats = %+v, want TotalNodes=2, TotalEdges=1", st)
	}
	if st.ByKind[string(graph.KindFunction)] != 1 || st.ByKind[string(graph.KindType)] != 1 {
		t.Fatalf("Stats.ByKind = %v, want one each", st.ByKind)
	}
}

func testRepoStats(t *testing.T, factory Factory) {
	t.Helper()
	s := factory(t)
	s.AddNode(mkRepoNode("r1/a.go::Foo", "Foo", "r1/a.go", "r1", graph.KindFunction))
	s.AddNode(mkRepoNode("r2/x.go::Baz", "Baz", "r2/x.go", "r2", graph.KindType))
	st := s.RepoStats()
	if len(st) != 2 {
		t.Fatalf("RepoStats has %d entries, want 2", len(st))
	}
	if st["r1"].TotalNodes != 1 {
		t.Fatalf("RepoStats[r1].TotalNodes = %d, want 1", st["r1"].TotalNodes)
	}
}

func testRepoPrefixes(t *testing.T, factory Factory) {
	t.Helper()
	s := factory(t)
	s.AddNode(mkRepoNode("r1/a.go::Foo", "Foo", "r1/a.go", "r1", graph.KindFunction))
	s.AddNode(mkRepoNode("r2/x.go::Baz", "Baz", "r2/x.go", "r2", graph.KindType))
	got := s.RepoPrefixes()
	sort.Strings(got)
	want := []string{"r1", "r2"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("RepoPrefixes = %v, want %v", got, want)
	}
}

func testSetEdgeProvenance(t *testing.T, factory Factory) {
	t.Helper()
	s := factory(t)
	s.AddNode(mkNode("a", "A", "x.go", graph.KindFunction))
	s.AddNode(mkNode("b", "B", "x.go", graph.KindFunction))
	e := mkEdge("a", "b", graph.EdgeCalls)
	e.Origin = graph.OriginTextMatched
	s.AddEdge(e)

	bumped := s.SetEdgeProvenance(e, graph.OriginLSPResolved)
	if !bumped {
		t.Fatalf("SetEdgeProvenance returned false for real upgrade")
	}
	out := s.GetOutEdges("a")
	if len(out) != 1 || out[0].Origin != graph.OriginLSPResolved {
		t.Fatalf("Origin did not propagate: %+v", out)
	}
}

func testReindexEdges(t *testing.T, factory Factory) {
	t.Helper()
	s := factory(t)
	// Build a small graph with three out-edges from "a" pointing at
	// three different targets, then re-bind all three to a fourth
	// target in one batched call.
	s.AddNode(mkNode("a", "A", "x.go", graph.KindFunction))
	s.AddNode(mkNode("b", "B", "x.go", graph.KindFunction))
	s.AddNode(mkNode("c", "C", "x.go", graph.KindFunction))
	s.AddNode(mkNode("d", "D", "x.go", graph.KindFunction))
	s.AddNode(mkNode("z", "Z", "x.go", graph.KindFunction))

	e1 := mkEdge("a", "b", graph.EdgeCalls)
	e1.Line = 1
	e2 := mkEdge("a", "c", graph.EdgeCalls)
	e2.Line = 2
	e3 := mkEdge("a", "d", graph.EdgeCalls)
	e3.Line = 3
	s.AddEdge(e1)
	s.AddEdge(e2)
	s.AddEdge(e3)

	// Mutate each edge's To, then hand the batch over. After the
	// call, all three edges must show as in-edges of z; none of the
	// originals must remain.
	e1.To, e2.To, e3.To = "z", "z", "z"
	s.ReindexEdges([]graph.EdgeReindex{
		{Edge: e1, OldTo: "b"},
		{Edge: e2, OldTo: "c"},
		{Edge: e3, OldTo: "d"},
	})

	for _, oldID := range []string{"b", "c", "d"} {
		if got := len(s.GetInEdges(oldID)); got != 0 {
			t.Fatalf("GetInEdges(%q) after batch reindex = %d, want 0", oldID, got)
		}
	}
	if got := len(s.GetInEdges("z")); got != 3 {
		t.Fatalf("GetInEdges(z) after batch reindex = %d, want 3", got)
	}
	if got := len(s.GetOutEdges("a")); got != 3 {
		t.Fatalf("GetOutEdges(a) after batch reindex = %d, want 3", got)
	}

	// Full-identity refresh updates source location/meta while preserving the
	// resolver-selected target and adjacency.
	oldLine := e1.Line
	refreshed := *e1
	refreshed.Line = 41
	refreshed.Meta = map[string]any{"doc": "shifted"}
	s.ReindexEdges([]graph.EdgeReindex{{
		Edge: &refreshed, OldTo: e1.To, OldFilePath: e1.FilePath,
		OldLine: oldLine, RefreshIdentity: true,
	}})
	var found *graph.Edge
	for _, edge := range s.GetOutEdges("a") {
		if edge.To == "z" && edge.Line == 41 {
			found = edge
		}
	}
	if found == nil {
		t.Fatal("full-identity refresh did not move edge line while preserving target")
	}

	// Empty batch is a no-op.
	s.ReindexEdges(nil)
	s.ReindexEdges([]graph.EdgeReindex{})
}

func testSetEdgeProvenanceBatch(t *testing.T, factory Factory) {
	t.Helper()
	s := factory(t)
	s.AddNode(mkNode("a", "A", "x.go", graph.KindFunction))
	s.AddNode(mkNode("b", "B", "x.go", graph.KindFunction))

	e1 := mkEdge("a", "b", graph.EdgeCalls)
	e1.Line = 1
	e1.Origin = graph.OriginTextMatched
	e2 := mkEdge("a", "b", graph.EdgeCalls)
	e2.Line = 2
	e2.Origin = graph.OriginTextMatched
	e3 := mkEdge("a", "b", graph.EdgeCalls)
	e3.Line = 3
	e3.Origin = graph.OriginLSPResolved // already at target tier — should be no-op
	s.AddEdge(e1)
	s.AddEdge(e2)
	s.AddEdge(e3)

	changed := s.SetEdgeProvenanceBatch([]graph.EdgeProvenanceUpdate{
		{Edge: e1, NewOrigin: graph.OriginLSPResolved},
		{Edge: e2, NewOrigin: graph.OriginLSPResolved},
		{Edge: e3, NewOrigin: graph.OriginLSPResolved},
	})
	if changed != 2 {
		t.Fatalf("SetEdgeProvenanceBatch reported %d changed, want 2 (one was already at target tier)", changed)
	}
	// Verify both promotions landed in the persisted edges.
	out := s.GetOutEdges("a")
	if len(out) != 3 {
		t.Fatalf("GetOutEdges(a) = %d, want 3", len(out))
	}
	for _, e := range out {
		if e.Origin != graph.OriginLSPResolved {
			t.Fatalf("edge %s->%s Origin = %q, want lsp_resolved", e.From, e.To, e.Origin)
		}
	}

	// Empty batch is a no-op and returns 0.
	if got := s.SetEdgeProvenanceBatch(nil); got != 0 {
		t.Fatalf("empty batch returned %d, want 0", got)
	}
	if got := s.SetEdgeProvenanceBatch([]graph.EdgeProvenanceUpdate{}); got != 0 {
		t.Fatalf("empty batch returned %d, want 0", got)
	}
}

func testReindexEdge(t *testing.T, factory Factory) {
	t.Helper()
	s := factory(t)
	s.AddNode(mkNode("a", "A", "x.go", graph.KindFunction))
	s.AddNode(mkNode("old", "Old", "x.go", graph.KindFunction))
	s.AddNode(mkNode("new", "New", "x.go", graph.KindFunction))
	e := mkEdge("a", "old", graph.EdgeCalls)
	s.AddEdge(e)

	e.To = "new"
	s.ReindexEdge(e, "old")

	if got := len(s.GetInEdges("old")); got != 0 {
		t.Fatalf("InEdges(old) after reindex = %d, want 0", got)
	}
	in := s.GetInEdges("new")
	if len(in) != 1 || in[0].From != "a" {
		t.Fatalf("InEdges(new) = %+v, want one edge from a", in)
	}
}

func testConcurrency(t *testing.T, factory Factory) {
	t.Helper()
	s := factory(t)
	const workers = 8
	const perWorker = 50
	var wg sync.WaitGroup
	for w := range workers {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := range perWorker {
				id := fmt.Sprintf("w%d/n%d", w, i)
				s.AddNode(mkNode(id, fmt.Sprintf("N%d", i), fmt.Sprintf("f%d.go", w), graph.KindFunction))
			}
		}(w)
	}
	wg.Wait()
	if got, want := s.NodeCount(), workers*perWorker; got != want {
		t.Fatalf("concurrent NodeCount = %d, want %d", got, want)
	}
}

func testEdgeIdentityRevisions(t *testing.T, factory Factory) {
	t.Helper()
	s := factory(t)
	// Just ensure the method exists and returns a non-negative int.
	// The semantic invariant ("bumps on origin change") is
	// implementation-defined; backends may return 0 if they don't
	// track this.
	if got := s.EdgeIdentityRevisions(); got < 0 {
		t.Fatalf("EdgeIdentityRevisions negative: %d", got)
	}
}

func testVerifyEdgeIdentities(t *testing.T, factory Factory) {
	t.Helper()
	s := factory(t)
	s.AddNode(mkNode("a", "A", "x.go", graph.KindFunction))
	s.AddNode(mkNode("b", "B", "x.go", graph.KindFunction))
	s.AddEdge(mkEdge("a", "b", graph.EdgeCalls))
	if err := s.VerifyEdgeIdentities(); err != nil {
		t.Fatalf("VerifyEdgeIdentities on consistent store: %v", err)
	}
}

func testRepoMemoryEstimate(t *testing.T, factory Factory) {
	t.Helper()
	s := factory(t)
	s.AddNode(mkRepoNode("r1/a.go::Foo", "Foo", "r1/a.go", "r1", graph.KindFunction))
	// Backends may return zero (disk/remote) or a real estimate
	// (in-memory). The contract is that the call succeeds and
	// NodeCount matches what we inserted.
	est := s.RepoMemoryEstimate("r1")
	if est.NodeCount != 1 {
		t.Fatalf("RepoMemoryEstimate NodeCount = %d, want 1", est.NodeCount)
	}
}

func testAllRepoMemoryEstimates(t *testing.T, factory Factory) {
	t.Helper()
	s := factory(t)
	s.AddNode(mkRepoNode("r1/a.go::Foo", "Foo", "r1/a.go", "r1", graph.KindFunction))
	s.AddNode(mkRepoNode("r2/x.go::Baz", "Baz", "r2/x.go", "r2", graph.KindFunction))

	// Both backends serve this from maintained counters rather than a corpus
	// walk, but they maintain them at different moments: the in-memory graph
	// updates shard counters inside AddNode, while a durable backend records
	// one row per repo when the index pass that produced those nodes
	// finishes. Stamping the row here is what the indexer does at the end of
	// a pass, so the case exercises each backend's real mechanism instead of
	// asserting the in-memory one against both.
	if w, ok := s.(graph.RepoIndexStateWriter); ok {
		for _, repo := range []string{"r1", "r2"} {
			if err := w.SetRepoIndexState(graph.RepoIndexState{
				RepoPrefix: repo, NodeCount: 1,
			}); err != nil {
				t.Fatalf("SetRepoIndexState(%s): %v", repo, err)
			}
		}
	}

	all := s.AllRepoMemoryEstimates()
	if len(all) != 2 {
		t.Fatalf("AllRepoMemoryEstimates len = %d, want 2", len(all))
	}
	for _, repo := range []string{"r1", "r2"} {
		if all[repo].NodeCount != 1 {
			t.Fatalf("estimate for %s = %d nodes, want 1", repo, all[repo].NodeCount)
		}
		if all[repo].NodeBytes == 0 {
			t.Fatalf("estimate for %s has no byte estimate", repo)
		}
	}
}

func testMetaPreserved(t *testing.T, factory Factory) {
	t.Helper()
	s := factory(t)
	n := mkNode("a.go::Foo", "Foo", "a.go", graph.KindFunction)
	n.Meta = map[string]any{
		"signature":  "func Foo(x int) error",
		"visibility": "public",
	}
	s.AddNode(n)
	got := s.GetNode("a.go::Foo")
	if got == nil {
		t.Fatalf("GetNode returned nil")
		return
	}
	if got.Meta == nil {
		t.Fatalf("Meta not preserved")
	}
	if got.Meta["signature"] != "func Foo(x int) error" {
		t.Fatalf("Meta[signature] = %v", got.Meta["signature"])
	}
	if got.Meta["visibility"] != "public" {
		t.Fatalf("Meta[visibility] = %v", got.Meta["visibility"])
	}
}

func testEdgesByKind(t *testing.T, factory Factory) {
	t.Helper()
	s := factory(t)
	s.AddNode(mkNode("a", "A", "x.go", graph.KindFunction))
	s.AddNode(mkNode("b", "B", "x.go", graph.KindFunction))
	s.AddNode(mkNode("c", "C", "y.go", graph.KindType))

	e1 := mkEdge("a", "b", graph.EdgeCalls)
	e1.Line = 1
	e2 := mkEdge("a", "b", graph.EdgeCalls)
	e2.Line = 2
	e3 := mkEdge("a", "c", graph.EdgeReferences)
	s.AddEdge(e1)
	s.AddEdge(e2)
	s.AddEdge(e3)

	var calls []*graph.Edge
	for e := range s.EdgesByKind(graph.EdgeCalls) {
		calls = append(calls, e)
	}
	if len(calls) != 2 {
		t.Fatalf("EdgesByKind(EdgeCalls) yielded %d, want 2", len(calls))
	}
	for _, e := range calls {
		if e.Kind != graph.EdgeCalls {
			t.Fatalf("yielded edge has wrong kind: %s", e.Kind)
		}
	}

	var refs []*graph.Edge
	for e := range s.EdgesByKind(graph.EdgeReferences) {
		refs = append(refs, e)
	}
	if len(refs) != 1 {
		t.Fatalf("EdgesByKind(EdgeReferences) yielded %d, want 1", len(refs))
	}

	// Unknown kind yields nothing.
	count := 0
	for range s.EdgesByKind(graph.EdgeKind("nonexistent")) {
		count++
	}
	if count != 0 {
		t.Fatalf("EdgesByKind(nonexistent) yielded %d, want 0", count)
	}

	// Early stop honours the contract.
	stopped := 0
	for range s.EdgesByKind(graph.EdgeCalls) {
		stopped++
		if stopped == 1 {
			break
		}
	}
	if stopped != 1 {
		t.Fatalf("early stop yielded %d before break, want 1", stopped)
	}
}

func testNodesByKind(t *testing.T, factory Factory) {
	t.Helper()
	s := factory(t)
	s.AddNode(mkNode("a", "A", "x.go", graph.KindFunction))
	s.AddNode(mkNode("b", "B", "x.go", graph.KindFunction))
	s.AddNode(mkNode("c", "C", "y.go", graph.KindType))

	var fns []*graph.Node
	for n := range s.NodesByKind(graph.KindFunction) {
		fns = append(fns, n)
	}
	if len(fns) != 2 {
		t.Fatalf("NodesByKind(KindFunction) yielded %d, want 2", len(fns))
	}
	for _, n := range fns {
		if n.Kind != graph.KindFunction {
			t.Fatalf("yielded node has wrong kind: %s", n.Kind)
		}
	}

	var types []*graph.Node
	for n := range s.NodesByKind(graph.KindType) {
		types = append(types, n)
	}
	if len(types) != 1 {
		t.Fatalf("NodesByKind(KindType) yielded %d, want 1", len(types))
	}

	// Early stop honours the contract.
	stopped := 0
	for range s.NodesByKind(graph.KindFunction) {
		stopped++
		if stopped == 1 {
			break
		}
	}
	if stopped != 1 {
		t.Fatalf("early stop yielded %d before break, want 1", stopped)
	}
}

func testEdgesWithUnresolvedTarget(t *testing.T, factory Factory) {
	t.Helper()
	s := factory(t)
	s.AddNode(mkNode("a", "A", "x.go", graph.KindFunction))
	s.AddNode(mkNode("b", "B", "x.go", graph.KindFunction))

	e1 := mkEdge("a", "b", graph.EdgeCalls)
	e1.Line = 1
	e2 := mkEdge("a", "unresolved::Foo", graph.EdgeCalls)
	e2.Line = 2
	e3 := mkEdge("a", "unresolved::Bar", graph.EdgeCalls)
	e3.Line = 3
	e4 := mkEdge("a", "resolved", graph.EdgeCalls)
	e4.Line = 4
	// Multi-repo COPY rewrite form: copyBulkLocked rewrites a bare
	// `unresolved::<name>` stub to `<repoPrefix>::unresolved::<name>`
	// so per-repo stubs can't collide on the COPY primary key. The
	// pending-edge scan MUST yield this form too, or the Go resolver
	// never gets a second pass at multi-repo stubs (the whole-repo
	// "every function looks dead" bug). graph.IsUnresolvedTarget is
	// the canonical matcher for both encodings.
	e5 := mkEdge("a", "gortex::unresolved::Baz", graph.EdgeCalls)
	e5.Line = 5
	// Gate-owned fn-value placeholders: a captured function value parks at
	// `unresolved::fnvalue::<name>` (bare and multi-repo forms) until the
	// callback gate binds it. is_unresolved is 1 for them (they live in the
	// unresolved space), but the master resolver can never bind them, so the
	// pending scan MUST exclude both forms — otherwise they are pure bloat on
	// the set the resolver reconciles every pass.
	e6 := mkEdge("a", "unresolved::fnvalue::handler", graph.EdgeReferences)
	e6.Line = 6
	e7 := mkEdge("a", "gortex::unresolved::fnvalue::handler", graph.EdgeReferences)
	e7.Line = 7
	s.AddEdge(e1)
	s.AddEdge(e2)
	s.AddEdge(e3)
	s.AddEdge(e4)
	s.AddEdge(e5)
	s.AddEdge(e6)
	s.AddEdge(e7)

	var unres []*graph.Edge
	for e := range s.EdgesWithUnresolvedTarget() {
		unres = append(unres, e)
	}
	if len(unres) != 3 {
		t.Fatalf("EdgesWithUnresolvedTarget yielded %d, want 3 (unresolved::Foo, unresolved::Bar, gortex::unresolved::Baz); fn-value placeholders must be excluded", len(unres))
	}
	gotPrefixed := false
	for _, e := range unres {
		if !graph.IsUnresolvedTarget(e.To) {
			t.Fatalf("yielded edge has non-unresolved To: %s", e.To)
		}
		if graph.IsFnValuePlaceholder(e.To) {
			t.Fatalf("EdgesWithUnresolvedTarget yielded a gate-owned fn-value placeholder %q: the resolver pending scan must exclude both the bare and multi-repo forms", e.To)
		}
		if e.To == "gortex::unresolved::Baz" {
			gotPrefixed = true
		}
	}
	if !gotPrefixed {
		t.Fatalf("EdgesWithUnresolvedTarget did not yield the multi-repo prefixed stub gortex::unresolved::Baz")
	}
}

// testFnValuePlaceholderEdges pins graph.FnValuePlaceholderScanner: the scan
// must return EXACTLY the fn-value gate placeholders (both the bare and the
// multi-repo COPY-rewrite forms) and nothing else — not real references, not
// non-fnvalue unresolved stubs, not resolved edges. It is the mirror image of
// testEdgesWithUnresolvedTarget's exclusion.
func testFnValuePlaceholderEdges(t *testing.T, factory Factory) {
	t.Helper()
	s := factory(t)
	sc, ok := s.(graph.FnValuePlaceholderScanner)
	if !ok {
		t.Skip("backend does not implement FnValuePlaceholderScanner")
	}
	s.AddNode(mkNode("a", "A", "x.go", graph.KindFunction))
	s.AddEdge(mkEdge("a", "b", graph.EdgeReferences))          // real reference — not a placeholder
	s.AddEdge(mkEdge("a", "unresolved::Foo", graph.EdgeCalls)) // unresolved, but not fn-value
	s.AddEdge(mkEdge("a", "resolved", graph.EdgeReferences))   // resolved
	ph1 := mkEdge("a", "unresolved::fnvalue::handler", graph.EdgeReferences)
	ph1.Line = 6
	ph2 := mkEdge("a", "gortex::unresolved::fnvalue::handler", graph.EdgeReferences)
	ph2.Line = 7
	s.AddEdge(ph1)
	s.AddEdge(ph2)

	seen := map[string]bool{}
	for e := range sc.FnValuePlaceholderEdges() {
		if !graph.IsFnValuePlaceholder(e.To) {
			t.Fatalf("FnValuePlaceholderEdges yielded a non-placeholder To: %s", e.To)
		}
		seen[e.To] = true
	}
	if len(seen) != 2 {
		t.Fatalf("FnValuePlaceholderEdges yielded %d distinct placeholders, want 2 (bare + multi-repo forms only); got %v", len(seen), seen)
	}
	if !seen["unresolved::fnvalue::handler"] {
		t.Fatalf("FnValuePlaceholderEdges missed the bare form; got %v", seen)
	}
	if !seen["gortex::unresolved::fnvalue::handler"] {
		t.Fatalf("FnValuePlaceholderEdges missed the multi-repo COPY-rewrite form; got %v", seen)
	}
}

// testValueRefPlaceholderEdges proves the optional value-reference projection
// narrows by the extractor-owned target namespace and EdgeReads kind while
// retaining metadata for the resolver's exact live-candidate revalidation.
func testValueRefPlaceholderEdges(t *testing.T, factory Factory) {
	t.Helper()
	s := factory(t)
	scanner, ok := s.(graph.ValueRefPlaceholderScanner)
	if !ok {
		t.Skip("backend does not implement ValueRefPlaceholderScanner")
	}
	s.AddNode(mkNode("a", "A", "x.go", graph.KindFunction))
	s.AddEdge(mkEdge("a", "resolved", graph.EdgeReads))

	candidate := mkEdge("a", "unresolved::valueref::MAX_RETRIES", graph.EdgeReads)
	candidate.Line = 6
	candidate.Meta = map[string]any{"via": "value_ref_candidate", "name": "MAX_RETRIES"}
	s.AddEdge(candidate)
	stale := mkEdge("a", "unresolved::valueref::STALE", graph.EdgeReads)
	stale.Line = 7
	stale.Meta = map[string]any{"via": "value_ref", "name": "STALE"}
	s.AddEdge(stale)
	wrongKind := mkEdge("a", "unresolved::valueref::NOT_A_READ", graph.EdgeCalls)
	wrongKind.Line = 8
	wrongKind.Meta = map[string]any{"via": "value_ref_candidate", "name": "NOT_A_READ"}
	s.AddEdge(wrongKind)

	seen := map[string]*graph.Edge{}
	for edge := range scanner.ValueRefPlaceholderEdges() {
		if edge.Kind != graph.EdgeReads {
			t.Fatalf("ValueRefPlaceholderEdges yielded kind %s, want reads", edge.Kind)
		}
		if edge.To < "unresolved::valueref::" || edge.To >= "unresolved::valueref:;" {
			t.Fatalf("ValueRefPlaceholderEdges yielded target outside namespace: %s", edge.To)
		}
		seen[edge.To] = edge
	}
	if len(seen) != 2 {
		t.Fatalf("ValueRefPlaceholderEdges yielded %d targets, want candidate + stale namespace row; got %v", len(seen), seen)
	}
	got := seen[candidate.To]
	if got == nil || got.Meta == nil || got.Meta["name"] != "MAX_RETRIES" {
		t.Fatalf("ValueRefPlaceholderEdges lost candidate metadata: %#v", got)
	}
	if seen[stale.To] == nil {
		t.Fatalf("ValueRefPlaceholderEdges must leave stale-via filtering to the resolver; got %v", seen)
	}
}

// testNodeLightScanner proves a whole-graph summary projection preserves the
// identity/location fields its analysis callers consume while omitting all
// Meta, including promoted docs and signatures.
func testNodeLightScanner(t *testing.T, factory Factory) {
	t.Helper()
	s := factory(t)
	sc, ok := s.(graph.NodeLightScanner)
	if !ok {
		t.Skip("backend does not implement NodeLightScanner")
	}

	a := mkNode("repo/pkg/a.go::A", "A", "pkg/a.go", graph.KindFunction)
	a.RepoPrefix = "repo"
	a.Language = "go"
	a.QualName = "repo.pkg.A"
	a.StartLine = 11
	a.EndLine = 19
	a.StartColumn = 2
	a.EndColumn = 8
	a.WorkspaceID = "workspace"
	a.ProjectID = "project"
	a.Meta = map[string]any{
		"signature": "func A()",
		"doc":       "large promoted documentation",
		"blob_only": "large opaque payload",
	}
	b := mkNode("repo/pkg/b.go::B", "B", "pkg/b.go", graph.KindType)
	b.RepoPrefix = "repo"
	b.Language = "go"
	for _, n := range []*graph.Node{a, b} {
		s.AddNode(n)
	}

	got := sc.AllNodesLight()
	if ids := sortNodeIDs(got); fmt.Sprint(ids) != fmt.Sprint([]string{a.ID, b.ID}) {
		t.Fatalf("AllNodesLight IDs = %v, want [%s %s]", ids, a.ID, b.ID)
	}
	var lightA *graph.Node
	for _, n := range got {
		if n.ID == a.ID {
			lightA = n
			break
		}
	}
	if lightA == nil {
		t.Fatal("AllNodesLight did not return A")
		return
	}
	if lightA.Kind != a.Kind || lightA.Name != a.Name || lightA.QualName != a.QualName ||
		lightA.FilePath != a.FilePath || lightA.StartLine != a.StartLine || lightA.EndLine != a.EndLine ||
		lightA.StartColumn != a.StartColumn || lightA.EndColumn != a.EndColumn ||
		lightA.Language != a.Language || lightA.RepoPrefix != a.RepoPrefix ||
		lightA.WorkspaceID != a.WorkspaceID || lightA.ProjectID != a.ProjectID {
		t.Fatalf("AllNodesLight altered identity/location fields: got %#v", lightA)
	}
	if lightA.Meta != nil {
		t.Fatalf("AllNodesLight materialized metadata: %#v", lightA.Meta)
	}
}

// testLightEdgeScanner pins graph.LightEdgeScanner: the kind filter must scope
// to the requested kinds (empty means all), and every promoted field must
// survive the meta-less scan intact and equal the values written. (The
// backend-specific Meta-is-nil guarantee is asserted where it holds — the disk
// backend's own test — since the in-memory backend legitimately keeps Meta.)
func testLightEdgeScanner(t *testing.T, factory Factory) {
	t.Helper()
	s := factory(t)
	sc, ok := s.(graph.LightEdgeScanner)
	if !ok {
		t.Skip("backend does not implement LightEdgeScanner")
	}
	s.AddNode(mkNode("a", "A", "x.go", graph.KindFunction))
	s.AddNode(mkNode("b", "B", "x.go", graph.KindFunction))
	call := mkEdge("a", "b", graph.EdgeCalls)
	call.Line, call.Confidence, call.Origin, call.Tier, call.CrossRepo = 11, 0.75, graph.OriginLSPResolved, "lsp", true
	call.Meta = map[string]any{"via": "direct", "blob_only": "x"}
	ref := mkEdge("a", "b", graph.EdgeReferences)
	ref.Line = 22
	imp := mkEdge("a", "b", graph.EdgeImports)
	imp.Line = 33
	s.AddEdge(call)
	s.AddEdge(ref)
	s.AddEdge(imp)

	// Kind filter: calls + references, imports excluded.
	got := sc.AllEdgesLight(graph.EdgeCalls, graph.EdgeReferences)
	kinds := map[graph.EdgeKind]int{}
	var lightCall *graph.Edge
	for _, e := range got {
		kinds[e.Kind]++
		if e.Kind == graph.EdgeCalls {
			lightCall = e
		}
	}
	if len(got) != 2 || kinds[graph.EdgeCalls] != 1 || kinds[graph.EdgeReferences] != 1 || kinds[graph.EdgeImports] != 0 {
		t.Fatalf("AllEdgesLight(calls,references) kind filter wrong: got %d edges %v, want {calls:1, references:1}", len(got), kinds)
	}
	// Empty kinds means every edge.
	if all := sc.AllEdgesLight(); len(all) != 3 {
		t.Fatalf("AllEdgesLight() with no kinds = %d, want 3 (all edges)", len(all))
	}
	// Promoted fields survive the meta-less scan.
	if lightCall == nil {
		t.Fatal("AllEdgesLight did not return the call edge")
		return
	}
	if lightCall.Line != 11 || lightCall.Confidence != 0.75 || lightCall.Origin != graph.OriginLSPResolved ||
		lightCall.Tier != "lsp" || !lightCall.CrossRepo {
		t.Fatalf("AllEdgesLight dropped/altered a promoted field: line=%d conf=%v origin=%q tier=%q crossRepo=%v",
			lightCall.Line, lightCall.Confidence, lightCall.Origin, lightCall.Tier, lightCall.CrossRepo)
	}
}

func testEmptyStore(t *testing.T, factory Factory) {
	t.Helper()
	s := factory(t)
	if s.NodeCount() != 0 {
		t.Fatalf("empty NodeCount = %d, want 0", s.NodeCount())
	}
	if s.EdgeCount() != 0 {
		t.Fatalf("empty EdgeCount = %d, want 0", s.EdgeCount())
	}
	if len(s.AllNodes()) != 0 {
		t.Fatalf("empty AllNodes nonzero")
	}
	if len(s.AllEdges()) != 0 {
		t.Fatalf("empty AllEdges nonzero")
	}
	if len(s.RepoPrefixes()) != 0 {
		t.Fatalf("empty RepoPrefixes nonzero")
	}
}

func testGetNodesByIDs(t *testing.T, factory Factory) {
	t.Helper()
	s := factory(t)
	s.AddNode(mkNode("a.go::Foo", "Foo", "a.go", graph.KindFunction))
	s.AddNode(mkNode("a.go::Bar", "Bar", "a.go", graph.KindFunction))
	s.AddNode(mkNode("b.go::Baz", "Baz", "b.go", graph.KindType))

	got := s.GetNodesByIDs([]string{"a.go::Foo", "b.go::Baz", "missing", "a.go::Bar", "a.go::Foo"})
	if len(got) != 3 {
		t.Fatalf("GetNodesByIDs len = %d, want 3 (3 present, 1 missing, 1 duplicate)", len(got))
	}
	if got["a.go::Foo"] == nil || got["a.go::Foo"].Name != "Foo" {
		t.Fatalf("missing or wrong Foo: %v", got["a.go::Foo"])
	}
	if got["b.go::Baz"] == nil || got["b.go::Baz"].Kind != graph.KindType {
		t.Fatalf("missing or wrong Baz: %v", got["b.go::Baz"])
	}
	if _, present := got["missing"]; present {
		t.Fatalf("missing ID should not be in map, got %v", got["missing"])
	}

	// Empty / nil input is a no-op.
	if got := s.GetNodesByIDs(nil); len(got) != 0 {
		t.Fatalf("nil input returned %d entries", len(got))
	}
	if got := s.GetNodesByIDs([]string{}); len(got) != 0 {
		t.Fatalf("empty input returned %d entries", len(got))
	}
	if got := s.GetNodesByIDs([]string{""}); len(got) != 0 {
		t.Fatalf("empty-string ID returned %d entries", len(got))
	}
}

func testFindNodesByNames(t *testing.T, factory Factory) {
	t.Helper()
	s := factory(t)
	s.AddNode(mkNode("a.go::Foo", "Foo", "a.go", graph.KindFunction))
	s.AddNode(mkNode("b.go::Foo", "Foo", "b.go", graph.KindFunction))
	s.AddNode(mkNode("c.go::Bar", "Bar", "c.go", graph.KindFunction))

	got := s.FindNodesByNames([]string{"Foo", "Missing", "Bar", "Foo"})
	if len(got) != 2 {
		t.Fatalf("FindNodesByNames len = %d, want 2 (2 present, 1 missing, 1 duplicate)", len(got))
	}
	foos := got["Foo"]
	if len(foos) != 2 {
		t.Fatalf("Foo matches = %d, want 2", len(foos))
	}
	for _, n := range foos {
		if n.Name != "Foo" {
			t.Fatalf("matched node has wrong Name: %s", n.Name)
		}
	}
	bars := got["Bar"]
	if len(bars) != 1 || bars[0].Name != "Bar" {
		t.Fatalf("Bar matches wrong: %v", bars)
	}
	if _, present := got["Missing"]; present {
		t.Fatalf("missing name should not be in map")
	}

	// Empty / nil input.
	if got := s.FindNodesByNames(nil); len(got) != 0 {
		t.Fatalf("nil input returned %d entries", len(got))
	}
	if got := s.FindNodesByNames([]string{}); len(got) != 0 {
		t.Fatalf("empty input returned %d entries", len(got))
	}
}

// testGetEdgesByNodeIDs covers the batched fan-in / fan-out edge
// lookups. Builds a small graph with mixed fan-in/out, calls both
// methods with a mix of present and missing ids (plus an empty
// string), and asserts the per-id slices match what GetInEdges /
// GetOutEdges would return individually.
func testGetEdgesByNodeIDs(t *testing.T, factory Factory) {
	t.Helper()
	s := factory(t)
	// Nodes
	s.AddNode(mkNode("a", "A", "x.go", graph.KindFunction))
	s.AddNode(mkNode("b", "B", "x.go", graph.KindFunction))
	s.AddNode(mkNode("c", "C", "y.go", graph.KindFunction))
	s.AddNode(mkNode("d", "D", "y.go", graph.KindFunction))
	// Edges: a→b, a→c, b→c, d→c (so c has 3 in-edges, a has 2 out-edges).
	s.AddEdge(mkEdge("a", "b", graph.EdgeCalls))
	s.AddEdge(mkEdge("a", "c", graph.EdgeCalls))
	s.AddEdge(mkEdge("b", "c", graph.EdgeCalls))
	s.AddEdge(mkEdge("d", "c", graph.EdgeReferences))

	// --- GetOutEdgesByNodeIDs ---
	outMap := s.GetOutEdgesByNodeIDs([]string{"a", "b", "d", "missing", "a"})
	// a has 2 out-edges (a→b, a→c).
	if got := sortEdgeKeys(outMap["a"]); len(got) != 2 {
		t.Fatalf("GetOutEdgesByNodeIDs[a] = %v, want 2 edges", got)
	}
	// b has 1 out-edge (b→c).
	if got := outMap["b"]; len(got) != 1 || got[0].To != "c" {
		t.Fatalf("GetOutEdgesByNodeIDs[b] = %v, want one edge to c", got)
	}
	// d has 1 out-edge (d→c).
	if got := outMap["d"]; len(got) != 1 || got[0].To != "c" {
		t.Fatalf("GetOutEdgesByNodeIDs[d] = %v, want one edge to c", got)
	}
	// missing key — range over nil is a no-op, so callers can index
	// without an ok-check.
	if got := outMap["missing"]; len(got) != 0 {
		t.Fatalf("GetOutEdgesByNodeIDs[missing] = %v, want empty", got)
	}

	// --- GetInEdgesByNodeIDs ---
	inMap := s.GetInEdgesByNodeIDs([]string{"a", "b", "c", "missing"})
	// a has 0 in-edges.
	if got := inMap["a"]; len(got) != 0 {
		t.Fatalf("GetInEdgesByNodeIDs[a] = %v, want empty", got)
	}
	// b has 1 in-edge (a→b).
	if got := inMap["b"]; len(got) != 1 || got[0].From != "a" {
		t.Fatalf("GetInEdgesByNodeIDs[b] = %v, want one edge from a", got)
	}
	// c has 3 in-edges (a→c, b→c, d→c).
	if got := inMap["c"]; len(got) != 3 {
		t.Fatalf("GetInEdgesByNodeIDs[c] = %v, want 3 edges", got)
	}
	froms := map[string]bool{}
	for _, e := range inMap["c"] {
		froms[e.From] = true
	}
	for _, want := range []string{"a", "b", "d"} {
		if !froms[want] {
			t.Fatalf("GetInEdgesByNodeIDs[c] missing edge from %q", want)
		}
	}
	if got := inMap["missing"]; len(got) != 0 {
		t.Fatalf("GetInEdgesByNodeIDs[missing] = %v, want empty", got)
	}

	// Empty / nil / empty-string inputs are no-ops.
	if got := s.GetOutEdgesByNodeIDs(nil); len(got) != 0 {
		t.Fatalf("GetOutEdgesByNodeIDs(nil) returned %d entries", len(got))
	}
	if got := s.GetInEdgesByNodeIDs(nil); len(got) != 0 {
		t.Fatalf("GetInEdgesByNodeIDs(nil) returned %d entries", len(got))
	}
	if got := s.GetOutEdgesByNodeIDs([]string{}); len(got) != 0 {
		t.Fatalf("GetOutEdgesByNodeIDs([]) returned %d entries", len(got))
	}
	if got := s.GetInEdgesByNodeIDs([]string{""}); len(got) != 0 {
		t.Fatalf("GetInEdgesByNodeIDs([\"\"]) returned %d entries", len(got))
	}
}

// testSymbolBundleSearcher exercises the optional
// graph.SymbolBundleSearcher capability. The interface is opt-in
// (today only the disk backend implements it; the in-memory
// *Graph deliberately leaves it unimplemented so the engine's
// fallback path stays exercised) — backends without the capability
// skip the subtest cleanly.
//
// Coverage:
//   - SymbolSearcher.BulkUpsertSymbolFTS + BuildSymbolIndex must be
//     called first so the FTS index is populated.
//   - SearchSymbolBundles returns a bundle per matched id with the
//     correct in/out edges attached.
//   - Empty / no-match query returns an empty bundle slice.
func testSymbolBundleSearcher(t *testing.T, factory Factory) {
	t.Helper()
	s := factory(t)
	bs, ok := s.(graph.SymbolBundleSearcher)
	if !ok {
		t.Skip("backend does not implement graph.SymbolBundleSearcher")
	}
	ss, ok := s.(graph.SymbolSearcher)
	if !ok {
		t.Skip("backend implements SymbolBundleSearcher but not SymbolSearcher — cannot populate FTS")
	}

	// Build a small graph: A → B → C, plus an unrelated isolated D.
	// FTS-searchable name tokens that should land on the same hit.
	s.AddNode(mkNode("a", "AlphaWidget", "x.go", graph.KindFunction))
	s.AddNode(mkNode("b", "BetaWidget", "x.go", graph.KindFunction))
	s.AddNode(mkNode("c", "GammaWidget", "y.go", graph.KindFunction))
	s.AddNode(mkNode("d", "Delta", "y.go", graph.KindFunction))
	s.AddEdge(mkEdge("a", "b", graph.EdgeCalls))
	s.AddEdge(mkEdge("b", "c", graph.EdgeCalls))
	s.AddEdge(mkEdge("a", "c", graph.EdgeCalls))

	// Populate the FTS sidecar — every searchable node carries its
	// tokenised name as the FTS text.
	items := []graph.SymbolFTSItem{
		{NodeID: "a", Tokens: "alpha widget"},
		{NodeID: "b", Tokens: "beta widget"},
		{NodeID: "c", Tokens: "gamma widget"},
		{NodeID: "d", Tokens: "delta"},
	}
	if err := ss.BulkUpsertSymbolFTS("", items); err != nil {
		t.Fatalf("BulkUpsertSymbolFTS: %v", err)
	}
	if err := ss.BuildSymbolIndex(); err != nil {
		t.Fatalf("BuildSymbolIndex: %v", err)
	}

	// Querying for "widget" should match a/b/c and not d. Each bundle
	// must carry the correct in/out edges off the graph.
	bundles, err := bs.SearchSymbolBundles("widget", 10)
	if err != nil {
		t.Fatalf("SearchSymbolBundles: %v", err)
	}
	if len(bundles) == 0 {
		t.Fatalf("SearchSymbolBundles returned no bundles — expected matches for a/b/c")
	}
	gotIDs := make(map[string]graph.SymbolBundle, len(bundles))
	for _, b := range bundles {
		if b.Node == nil {
			t.Fatalf("bundle has nil node: %+v", b)
			continue
		}
		gotIDs[b.Node.ID] = b
	}
	for _, want := range []string{"a", "b", "c"} {
		if _, ok := gotIDs[want]; !ok {
			t.Fatalf("missing bundle for id %q; got ids=%v", want, idsOf(bundles))
		}
	}
	if _, ok := gotIDs["d"]; ok {
		t.Fatalf("unexpected bundle for id %q (no 'widget' token in its FTS row)", "d")
	}

	// Edge verification: per-bundle in/out edges must match the
	// in-memory truth surfaced via the existing GetIn/Out edges.
	for id, b := range gotIDs {
		wantOut := s.GetOutEdges(id)
		if !edgeSlicesMatch(wantOut, b.OutEdges) {
			t.Fatalf("bundle[%s].OutEdges mismatch: want=%v got=%v", id, edgeKeys(wantOut), edgeKeys(b.OutEdges))
		}
		wantIn := s.GetInEdges(id)
		if !edgeSlicesMatch(wantIn, b.InEdges) {
			t.Fatalf("bundle[%s].InEdges mismatch: want=%v got=%v", id, edgeKeys(wantIn), edgeKeys(b.InEdges))
		}
	}

	// Empty query is a clean no-op.
	if empty, err := bs.SearchSymbolBundles("", 10); err != nil || len(empty) != 0 {
		t.Fatalf("SearchSymbolBundles(\"\"): err=%v len=%d, want empty", err, len(empty))
	}
	// No-match query — backend MAY return nil or empty slice; both
	// are valid.
	if no, err := bs.SearchSymbolBundles("nomatchforanything", 10); err != nil {
		t.Fatalf("SearchSymbolBundles(nomatch): err=%v", err)
	} else if len(no) != 0 {
		t.Fatalf("SearchSymbolBundles(nomatch) returned %d bundles, want 0", len(no))
	}
}

// idsOf is a small helper for the bundle assertions above.
func idsOf(bs []graph.SymbolBundle) []string {
	out := make([]string, 0, len(bs))
	for _, b := range bs {
		if b.Node != nil {
			out = append(out, b.Node.ID)
		}
	}
	sort.Strings(out)
	return out
}

// edgeSlicesMatch reports whether two edge slices contain the same
// (from, to, kind) tuples regardless of order. Used by the bundle
// assertions to ignore back-end-imposed ordering differences.
func edgeSlicesMatch(want, got []*graph.Edge) bool {
	if len(want) != len(got) {
		return false
	}
	wantKeys := edgeKeys(want)
	gotKeys := edgeKeys(got)
	sort.Strings(wantKeys)
	sort.Strings(gotKeys)
	for i := range wantKeys {
		if wantKeys[i] != gotKeys[i] {
			return false
		}
	}
	return true
}

// edgeKeys flattens a slice of edges into deterministic (from→to:kind)
// strings for ordered diffing.
func edgeKeys(es []*graph.Edge) []string {
	out := make([]string, 0, len(es))
	for _, e := range es {
		if e == nil {
			continue
		}
		out = append(out, fmt.Sprintf("%s->%s:%s", e.From, e.To, e.Kind))
	}
	return out
}

// testDeadCodeCandidator exercises the optional
// graph.DeadCodeCandidator capability. Builds a small graph with
// nodes that fall into each filter case the analyzer cares about:
//
//   - zero in-edges (dead).
//   - in-edges of disallowed kind only (dead).
//   - in-edges of allowed kind (alive).
//   - mixed kinds across the candidate set (per-row allowlist must apply).
//
// The in-memory *graph.Graph implements this; the disk backend overrides
// with a server-side query. Both must return the same candidate set.
func testDeadCodeCandidator(t *testing.T, factory Factory) {
	t.Helper()
	s := factory(t)
	dc, ok := s.(graph.DeadCodeCandidator)
	if !ok {
		t.Skip("backend does not implement graph.DeadCodeCandidator")
	}

	// Functions: AliveFunc (called), DeadFunc (no in-edges),
	// ReadOnlyFunc (only EdgeReads — disallowed for KindFunction).
	s.AddNode(mkNode("AliveFunc", "AliveFunc", "a.go", graph.KindFunction))
	s.AddNode(mkNode("DeadFunc", "DeadFunc", "a.go", graph.KindFunction))
	s.AddNode(mkNode("ReadOnlyFunc", "ReadOnlyFunc", "a.go", graph.KindFunction))
	s.AddNode(mkNode("Caller", "Caller", "a.go", graph.KindFunction))
	// Types: AliveType (referenced), DeadType (no in-edges).
	s.AddNode(mkNode("AliveType", "AliveType", "b.go", graph.KindType))
	s.AddNode(mkNode("DeadType", "DeadType", "b.go", graph.KindType))
	// Methods: AliveMethod (called), DeadMethod (no in-edges).
	s.AddNode(mkNode("AliveMethod", "AliveMethod", "c.go", graph.KindMethod))
	s.AddNode(mkNode("DeadMethod", "DeadMethod", "c.go", graph.KindMethod))

	// Edges that exercise the per-kind allowlist.
	e1 := mkEdge("Caller", "AliveFunc", graph.EdgeCalls)
	e1.Line = 1
	e2 := mkEdge("Caller", "ReadOnlyFunc", graph.EdgeReads)
	e2.Line = 2
	e3 := mkEdge("Caller", "AliveMethod", graph.EdgeCalls)
	e3.Line = 3
	e4 := mkEdge("Caller", "AliveType", graph.EdgeReferences)
	e4.Line = 4
	s.AddEdge(e1)
	s.AddEdge(e2)
	s.AddEdge(e3)
	s.AddEdge(e4)

	// Per-kind allowlist mirrors analysis.incomingUsageKinds for the
	// three kinds under test. Functions are alive on Calls/References;
	// methods on Calls/Implements; types on References/Instantiates.
	allowedKinds := []graph.NodeKind{
		graph.KindFunction,
		graph.KindMethod,
		graph.KindType,
	}
	allowedInEdges := map[graph.NodeKind][]graph.EdgeKind{
		graph.KindFunction: {graph.EdgeCalls, graph.EdgeReferences},
		graph.KindMethod:   {graph.EdgeCalls, graph.EdgeImplements},
		graph.KindType:     {graph.EdgeReferences, graph.EdgeInstantiates},
	}

	got := dc.DeadCodeCandidates(allowedKinds, allowedInEdges)
	gotIDs := sortNodeIDs(got)
	// Caller has zero in-edges of any kind, so it surfaces too — the
	// analyzer's per-kind allowlist would also flag it as a candidate
	// here. The backend's job is just the candidate set; post-filters
	// (exported / test / entry-point) run in Go.
	want := []string{"Caller", "DeadFunc", "DeadMethod", "DeadType", "ReadOnlyFunc"}
	if fmt.Sprint(gotIDs) != fmt.Sprint(want) {
		t.Fatalf("DeadCodeCandidates = %v\nwant %v", gotIDs, want)
	}

	// Empty kind list returns nothing — never the whole graph.
	if got := dc.DeadCodeCandidates(nil, allowedInEdges); len(got) != 0 {
		t.Fatalf("DeadCodeCandidates(nil) = %d, want 0", len(got))
	}

	// Empty per-kind allowlist means "any incoming edge counts as
	// usage" — AliveFunc and ReadOnlyFunc (both have *some* in-edge)
	// drop out; only DeadFunc + Caller remain among functions.
	anyKind := map[graph.NodeKind][]graph.EdgeKind{
		graph.KindFunction: nil,
	}
	gotAny := dc.DeadCodeCandidates([]graph.NodeKind{graph.KindFunction}, anyKind)
	gotAnyIDs := sortNodeIDs(gotAny)
	wantAny := []string{"Caller", "DeadFunc"}
	if fmt.Sprint(gotAnyIDs) != fmt.Sprint(wantAny) {
		t.Fatalf("DeadCodeCandidates(any-kind) = %v\nwant %v", gotAnyIDs, wantAny)
	}
}

// testIfaceImplementsScanner exercises the optional
// graph.IfaceImplementsScanner capability. Seeds two interfaces (one
// with methods Meta, one without) plus a type that implements each;
// the row set must include only the (type, iface) tuple whose target
// has a Meta["methods"] payload — the no-meta interface drops out.
func testIfaceImplementsScanner(t *testing.T, factory Factory) {
	t.Helper()
	s := factory(t)
	scanner, ok := s.(graph.IfaceImplementsScanner)
	if !ok {
		t.Skip("backend does not implement graph.IfaceImplementsScanner")
	}

	// Interface with required methods.
	ifaceA := mkNode("iface_A", "Reader", "a.go", graph.KindInterface)
	ifaceA.Meta = map[string]any{"methods": []string{"Read", "Close"}}
	s.AddNode(ifaceA)
	// Interface with no Meta — must not appear in the row set.
	ifaceB := mkNode("iface_B", "Empty", "a.go", graph.KindInterface)
	s.AddNode(ifaceB)
	// Implementing type for each.
	s.AddNode(mkNode("type_A", "ReaderImpl", "a.go", graph.KindType))
	s.AddNode(mkNode("type_B", "EmptyImpl", "a.go", graph.KindType))
	s.AddEdge(mkEdge("type_A", "iface_A", graph.EdgeImplements))
	s.AddEdge(mkEdge("type_B", "iface_B", graph.EdgeImplements))

	rows := scanner.IfaceImplementsRows()
	if len(rows) != 1 {
		t.Fatalf("IfaceImplementsRows len = %d, want 1 (iface_B has no Meta)", len(rows))
	}
	r := rows[0]
	if r.TypeID != "type_A" || r.IfaceID != "iface_A" {
		t.Fatalf("row = %+v, want type_A → iface_A", r)
	}
	if r.IfaceMeta == nil {
		t.Fatalf("IfaceMeta is nil")
	}
	raw, ok := r.IfaceMeta["methods"]
	if !ok {
		t.Fatalf("IfaceMeta missing methods key: %+v", r.IfaceMeta)
	}
	// Meta encoding round-trips lists differently between backends
	// (in-memory keeps []string; gob-encoded comes back as []any).
	// Accept either.
	var methods []string
	switch v := raw.(type) {
	case []string:
		methods = v
	case []any:
		for _, m := range v {
			if str, ok := m.(string); ok {
				methods = append(methods, str)
			}
		}
	default:
		t.Fatalf("unexpected methods type %T: %v", raw, raw)
	}
	sort.Strings(methods)
	if fmt.Sprint(methods) != fmt.Sprint([]string{"Close", "Read"}) {
		t.Fatalf("methods = %v, want [Close Read]", methods)
	}
}

// testNodeDegreeAggregator exercises the optional
// graph.NodeDegreeAggregator capability. Builds a small graph with
// nodes that cover every classification branch
// graph.GraphConnectivity / graph.ClassifyZeroEdge care about:
//
//   - isolated (zero edges).
//   - leaf (exactly one edge in either direction).
//   - usage-edge in-bound only (alive — at least one EdgeCalls in).
//   - non-usage-edge in-bound only (no EdgeCalls / EdgeReferences /
//     etc — counts as "likely unused").
//   - usage-edge mixed with non-usage in-edges (still alive).
//   - unknown id (must be elided).
func testNodeDegreeAggregator(t *testing.T, factory Factory) {
	t.Helper()
	s := factory(t)
	dc, ok := s.(graph.NodeDegreeAggregator)
	if !ok {
		t.Skip("backend does not implement graph.NodeDegreeAggregator")
	}

	s.AddNode(mkNode("Isolated", "Isolated", "a.go", graph.KindFunction))
	s.AddNode(mkNode("LeafSink", "LeafSink", "a.go", graph.KindFunction))
	s.AddNode(mkNode("LeafSource", "LeafSource", "a.go", graph.KindFunction))
	s.AddNode(mkNode("Alive", "Alive", "a.go", graph.KindFunction))
	s.AddNode(mkNode("StructuralOnly", "StructuralOnly", "a.go", graph.KindFunction))
	s.AddNode(mkNode("Mixed", "Mixed", "a.go", graph.KindFunction))
	s.AddNode(mkNode("Caller", "Caller", "a.go", graph.KindFunction))
	s.AddNode(mkNode("FileNode", "FileNode", "a.go", graph.KindFile))

	// One incoming call into LeafSink → leaf (in_count=1, out_count=0).
	e1 := mkEdge("Caller", "LeafSink", graph.EdgeCalls)
	e1.Line = 1
	s.AddEdge(e1)
	// One outgoing reference from LeafSource → leaf (in=0, out=1).
	e2 := mkEdge("LeafSource", "Caller", graph.EdgeReferences)
	e2.Line = 2
	s.AddEdge(e2)
	// Alive: incoming call → alive (in=1 usage).
	e3 := mkEdge("Caller", "Alive", graph.EdgeCalls)
	e3.Line = 3
	s.AddEdge(e3)
	// StructuralOnly: incoming EdgeDefines (NOT a usage kind) →
	// classified as "likely unused" but not isolated.
	e4 := mkEdge("FileNode", "StructuralOnly", graph.EdgeDefines)
	e4.Line = 4
	s.AddEdge(e4)
	// Mixed: incoming EdgeDefines (non-usage) + incoming EdgeCalls
	// (usage). UsageInCount must reflect ONLY the usage edge.
	e5 := mkEdge("FileNode", "Mixed", graph.EdgeDefines)
	e5.Line = 5
	s.AddEdge(e5)
	e6 := mkEdge("Caller", "Mixed", graph.EdgeCalls)
	e6.Line = 6
	s.AddEdge(e6)

	ids := []string{
		"Isolated",
		"LeafSink",
		"LeafSource",
		"Alive",
		"StructuralOnly",
		"Mixed",
		"unknown::id",
	}
	usage := []graph.EdgeKind{graph.EdgeCalls, graph.EdgeReferences}
	rows := dc.NodeDegreeCounts(ids, usage)

	byID := make(map[string]graph.NodeDegreeRow, len(rows))
	for _, r := range rows {
		byID[r.NodeID] = r
	}
	// Unknown id MUST be elided.
	if _, ok := byID["unknown::id"]; ok {
		t.Fatalf("NodeDegreeCounts must elide unknown ids, got row")
	}

	type want struct{ in, out, usageIn int }
	cases := map[string]want{
		"Isolated":       {0, 0, 0},
		"LeafSink":       {1, 0, 1},
		"LeafSource":     {0, 1, 0},
		"Alive":          {1, 0, 1},
		"StructuralOnly": {1, 0, 0},
		"Mixed":          {2, 0, 1},
	}
	for id, w := range cases {
		got, ok := byID[id]
		if !ok {
			t.Errorf("missing row for %s", id)
			continue
		}
		if got.InCount != w.in || got.OutCount != w.out || got.UsageInCount != w.usageIn {
			t.Errorf("row %s = in=%d out=%d usage=%d, want in=%d out=%d usage=%d",
				id, got.InCount, got.OutCount, got.UsageInCount,
				w.in, w.out, w.usageIn)
		}
	}

	// Empty ids returns nil — never the whole graph.
	if got := dc.NodeDegreeCounts(nil, usage); len(got) != 0 {
		t.Fatalf("NodeDegreeCounts(nil) = %d, want 0", len(got))
	}

	// Empty usage kinds means UsageInCount is always 0 (totals
	// still populated).
	noUsage := dc.NodeDegreeCounts([]string{"Mixed"}, nil)
	if len(noUsage) != 1 {
		t.Fatalf("NodeDegreeCounts(Mixed, nil) = %d rows, want 1", len(noUsage))
	}
	if noUsage[0].InCount != 2 || noUsage[0].UsageInCount != 0 {
		t.Fatalf("NodeDegreeCounts(Mixed, nil) = in=%d usage=%d, want in=2 usage=0",
			noUsage[0].InCount, noUsage[0].UsageInCount)
	}
}

// testNodeFanAggregator exercises the optional
// graph.NodeFanAggregator capability. Builds a small graph that
// exercises the per-direction kind filter independently:
//
//   - Hub: high fan-in (Calls + References) AND high fan-out (Calls).
//   - Leaf: zero fan in either direction.
//   - ReadHeavy: incoming Reads only — fan-in must be 0 when the
//     filter is Calls+References.
//   - CallerOnly: outgoing Calls only — fan-out non-zero, fan-in 0.
//   - Unknown id elided.
func testNodeFanAggregator(t *testing.T, factory Factory) {
	t.Helper()
	s := factory(t)
	fa, ok := s.(graph.NodeFanAggregator)
	if !ok {
		t.Skip("backend does not implement graph.NodeFanAggregator")
	}

	s.AddNode(mkNode("Hub", "Hub", "a.go", graph.KindFunction))
	s.AddNode(mkNode("Leaf", "Leaf", "a.go", graph.KindFunction))
	s.AddNode(mkNode("ReadHeavy", "ReadHeavy", "a.go", graph.KindFunction))
	s.AddNode(mkNode("CallerOnly", "CallerOnly", "a.go", graph.KindFunction))
	s.AddNode(mkNode("Target1", "Target1", "a.go", graph.KindFunction))
	s.AddNode(mkNode("Target2", "Target2", "a.go", graph.KindFunction))
	s.AddNode(mkNode("Src1", "Src1", "a.go", graph.KindFunction))
	s.AddNode(mkNode("Src2", "Src2", "a.go", graph.KindFunction))

	// Hub: 2 incoming Calls + 1 incoming Reference + 2 outgoing
	// Calls + 1 outgoing Reference. With fan-in=Calls+Refs and
	// fan-out=Calls: fan_in=3, fan_out=2.
	add := func(from, to string, kind graph.EdgeKind, line int) {
		e := mkEdge(from, to, kind)
		e.Line = line
		s.AddEdge(e)
	}
	add("Src1", "Hub", graph.EdgeCalls, 1)
	add("Src2", "Hub", graph.EdgeCalls, 2)
	add("Src1", "Hub", graph.EdgeReferences, 3)
	add("Hub", "Target1", graph.EdgeCalls, 4)
	add("Hub", "Target2", graph.EdgeCalls, 5)
	add("Hub", "Target1", graph.EdgeReferences, 6)

	// ReadHeavy: incoming Reads only.
	add("Src1", "ReadHeavy", graph.EdgeReads, 7)
	add("Src2", "ReadHeavy", graph.EdgeReads, 8)

	// CallerOnly: outgoing Calls only.
	add("CallerOnly", "Target1", graph.EdgeCalls, 9)

	ids := []string{"Hub", "Leaf", "ReadHeavy", "CallerOnly", "unknown::id"}
	rows := fa.NodeFanCounts(ids,
		[]graph.EdgeKind{graph.EdgeCalls, graph.EdgeReferences},
		[]graph.EdgeKind{graph.EdgeCalls},
	)

	byID := make(map[string]graph.NodeFanRow, len(rows))
	for _, r := range rows {
		byID[r.NodeID] = r
	}
	if _, ok := byID["unknown::id"]; ok {
		t.Fatalf("NodeFanCounts must elide unknown ids, got row")
	}

	type want struct{ in, out int }
	cases := map[string]want{
		"Hub":        {3, 2},
		"Leaf":       {0, 0},
		"ReadHeavy":  {0, 0},
		"CallerOnly": {0, 1},
	}
	for id, w := range cases {
		got, ok := byID[id]
		if !ok {
			t.Errorf("missing row for %s", id)
			continue
		}
		if got.FanIn != w.in || got.FanOut != w.out {
			t.Errorf("row %s = in=%d out=%d, want in=%d out=%d",
				id, got.FanIn, got.FanOut, w.in, w.out)
		}
	}

	// Empty ids returns nil.
	if got := fa.NodeFanCounts(nil, []graph.EdgeKind{graph.EdgeCalls}, nil); len(got) != 0 {
		t.Fatalf("NodeFanCounts(nil) = %d, want 0", len(got))
	}

	// Empty kind sets → all-zero rows for known ids only.
	zeros := fa.NodeFanCounts([]string{"Hub", "unknown::id"}, nil, nil)
	if len(zeros) != 1 {
		t.Fatalf("NodeFanCounts(empty kinds) = %d rows, want 1 (Hub only)", len(zeros))
	}
	if zeros[0].NodeID != "Hub" || zeros[0].FanIn != 0 || zeros[0].FanOut != 0 {
		t.Fatalf("NodeFanCounts(empty kinds) = %+v, want Hub/0/0", zeros[0])
	}
}

// testFileImporters exercises the optional graph.FileImporters
// capability. Seeds two importing files (one production, one test)
// plus an unrelated import edge that targets a different file. The
// returned rows must include exactly the importers of the target
// file — both via the file-node ID and via the FilePath-on-symbol
// shape — and must not surface the unrelated edge.
func testFileImporters(t *testing.T, factory Factory) {
	t.Helper()
	s := factory(t)
	fi, ok := s.(graph.FileImporters)
	if !ok {
		t.Skip("backend does not implement graph.FileImporters")
	}

	// target file node + a symbol inside it.
	s.AddNode(mkNode("pkg/target.go", "target.go", "pkg/target.go", graph.KindFile))
	s.AddNode(mkNode("TargetFunc", "TargetFunc", "pkg/target.go", graph.KindFunction))

	// Two importing files: one production, one test. Each has an
	// import edge — one targets the file node by id, the other
	// targets a symbol inside the file (FilePath match path).
	s.AddNode(mkNode("pkg/prod.go", "prod.go", "pkg/prod.go", graph.KindFile))
	s.AddNode(mkNode("pkg/test_test.go", "test_test.go", "pkg/test_test.go", graph.KindFile))

	// And an unrelated importer that points elsewhere — must NOT
	// surface in the results.
	s.AddNode(mkNode("pkg/other.go", "other.go", "pkg/other.go", graph.KindFile))
	s.AddNode(mkNode("pkg/elsewhere.go", "elsewhere.go", "pkg/elsewhere.go", graph.KindFile))

	s.AddEdge(mkEdge("pkg/prod.go", "pkg/target.go", graph.EdgeImports))
	s.AddEdge(mkEdge("pkg/test_test.go", "TargetFunc", graph.EdgeImports))
	s.AddEdge(mkEdge("pkg/other.go", "pkg/elsewhere.go", graph.EdgeImports))
	// A non-imports edge to the target file must also drop out.
	s.AddEdge(mkEdge("pkg/prod.go", "TargetFunc", graph.EdgeCalls))

	rows := fi.FileImporters("pkg/target.go")
	got := make([]string, 0, len(rows))
	for _, r := range rows {
		got = append(got, r.FromFile)
	}
	sort.Strings(got)
	want := []string{"pkg/prod.go", "pkg/test_test.go"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("FileImporters = %v, want %v", got, want)
	}

	if got := fi.FileImporters(""); len(got) != 0 {
		t.Fatalf("FileImporters(empty) = %d rows, want 0", len(got))
	}
	if got := fi.FileImporters("pkg/no_such.go"); len(got) != 0 {
		t.Fatalf("FileImporters(unknown) = %d rows, want 0", len(got))
	}
}

// testInEdgeCounter exercises the optional graph.InEdgeCounter
// capability. Seeds a small graph and asserts the per-To fan-in
// count matches what an AllEdges-bucketing loop would compute for
// the same edge-kind set.
func testInEdgeCounter(t *testing.T, factory Factory) {
	t.Helper()
	s := factory(t)
	ic, ok := s.(graph.InEdgeCounter)
	if !ok {
		t.Skip("backend does not implement graph.InEdgeCounter")
	}

	s.AddNode(mkNode("A", "A", "a.go", graph.KindFunction))
	s.AddNode(mkNode("B", "B", "a.go", graph.KindFunction))
	s.AddNode(mkNode("C", "C", "a.go", graph.KindFunction))
	s.AddNode(mkNode("T", "T", "a.go", graph.KindType))

	// B is called twice (from A and C), referenced once (from A).
	e1 := mkEdge("A", "B", graph.EdgeCalls)
	e1.Line = 1
	e2 := mkEdge("C", "B", graph.EdgeCalls)
	e2.Line = 2
	e3 := mkEdge("A", "B", graph.EdgeReferences)
	e3.Line = 3
	// T is referenced once and held by an import edge that should
	// not be counted under {calls,references}.
	e4 := mkEdge("A", "T", graph.EdgeReferences)
	e4.Line = 4
	e5 := mkEdge("A", "T", graph.EdgeImports)
	e5.Line = 5
	s.AddEdge(e1)
	s.AddEdge(e2)
	s.AddEdge(e3)
	s.AddEdge(e4)
	s.AddEdge(e5)

	got := ic.InEdgeCountsByKind([]graph.EdgeKind{graph.EdgeCalls, graph.EdgeReferences})
	if got["B"] != 3 {
		t.Fatalf("count[B] = %d, want 3", got["B"])
	}
	if got["T"] != 1 {
		t.Fatalf("count[T] = %d, want 1", got["T"])
	}
	if _, ok := got["A"]; ok {
		t.Fatalf("A should have zero matching incoming edges, got %d", got["A"])
	}

	// Empty kind list must return nil — never the whole graph.
	if got := ic.InEdgeCountsByKind(nil); got != nil {
		t.Fatalf("InEdgeCountsByKind(nil) = %v, want nil", got)
	}

	// Single-kind filter dedups when callers pass duplicates.
	got2 := ic.InEdgeCountsByKind([]graph.EdgeKind{graph.EdgeCalls, graph.EdgeCalls})
	if got2["B"] != 2 {
		t.Fatalf("count[B] (calls only, deduped) = %d, want 2", got2["B"])
	}
}

// testNodesInFilesByKindFinder exercises the optional
// graph.NodesInFilesByKindFinder capability. Seeds a graph spanning
// three files and three kinds; the result must include only the
// requested-kind nodes whose FilePath sits in the requested file
// set.
func testNodesInFilesByKindFinder(t *testing.T, factory Factory) {
	t.Helper()
	s := factory(t)
	fn, ok := s.(graph.NodesInFilesByKindFinder)
	if !ok {
		t.Skip("backend does not implement graph.NodesInFilesByKindFinder")
	}

	// f1.go: function + method + type.
	s.AddNode(mkNode("f1::F1", "F1", "f1.go", graph.KindFunction))
	s.AddNode(mkNode("f1::M1", "M1", "f1.go", graph.KindMethod))
	s.AddNode(mkNode("f1::T1", "T1", "f1.go", graph.KindType))
	// f2.go: function only.
	s.AddNode(mkNode("f2::F2", "F2", "f2.go", graph.KindFunction))
	// f3.go: drops out of every result — not in the requested files.
	s.AddNode(mkNode("f3::F3", "F3", "f3.go", graph.KindFunction))

	got := fn.NodesInFilesByKind(
		[]string{"f1.go", "f2.go"},
		[]graph.NodeKind{graph.KindFunction, graph.KindMethod},
	)
	gotIDs := sortNodeIDs(got)
	want := []string{"f1::F1", "f1::M1", "f2::F2"}
	if fmt.Sprint(gotIDs) != fmt.Sprint(want) {
		t.Fatalf("NodesInFilesByKind = %v, want %v", gotIDs, want)
	}

	// Empty files / kinds must return nil — never a whole-graph scan.
	if got := fn.NodesInFilesByKind(nil, []graph.NodeKind{graph.KindFunction}); got != nil {
		t.Fatalf("NodesInFilesByKind(nil files) = %v, want nil", got)
	}
	if got := fn.NodesInFilesByKind([]string{"f1.go"}, nil); got != nil {
		t.Fatalf("NodesInFilesByKind(nil kinds) = %v, want nil", got)
	}

	// Dedup: passing the same file / kind twice must not double-yield.
	gotDup := fn.NodesInFilesByKind(
		[]string{"f1.go", "f1.go"},
		[]graph.NodeKind{graph.KindType, graph.KindType},
	)
	if len(gotDup) != 1 || gotDup[0].ID != "f1::T1" {
		t.Fatalf("NodesInFilesByKind(dup) = %v, want [f1::T1]", sortNodeIDs(gotDup))
	}
}

// testEdgesByKindsScanner exercises the optional
// graph.EdgesByKindsScanner capability. Builds a small graph with a
// mix of edge kinds, then verifies the streaming filter returns
// exactly the union of the requested kinds in any order. Covers the
// edge cases that the edge-driven analyzers rely on: zero-match (no
// edge matches the requested kinds), empty filter (yields nothing —
// never a whole-table scan), and early stop honouring the iterator
// contract.
func testEdgesByKindsScanner(t *testing.T, factory Factory) {
	t.Helper()
	s := factory(t)

	s.AddNode(mkNode("a", "A", "x.go", graph.KindFunction))
	s.AddNode(mkNode("b", "B", "x.go", graph.KindFunction))
	s.AddNode(mkNode("c", "C", "y.go", graph.KindType))
	s.AddNode(mkNode("d", "D", "y.go", graph.KindField))

	calls1 := mkEdge("a", "b", graph.EdgeCalls)
	calls1.Line = 1
	calls2 := mkEdge("a", "b", graph.EdgeCalls)
	calls2.Line = 2
	refs := mkEdge("a", "c", graph.EdgeReferences)
	writes := mkEdge("a", "d", graph.EdgeWrites)
	throws := mkEdge("a", "c", graph.EdgeThrows)
	s.AddEdge(calls1)
	s.AddEdge(calls2)
	s.AddEdge(refs)
	s.AddEdge(writes)
	s.AddEdge(throws)

	es, ok := s.(graph.EdgesByKindsScanner)
	if !ok {
		t.Skip("backend does not implement graph.EdgesByKindsScanner")
	}

	// Multi-kind: union of Calls + References must surface all three
	// calls/refs edges; counts (not pointers) compared so the in-memory
	// and disk backends agree without relying on edge identity.
	counts := map[graph.EdgeKind]int{}
	for e := range es.EdgesByKinds([]graph.EdgeKind{graph.EdgeCalls, graph.EdgeReferences}) {
		counts[e.Kind]++
	}
	if counts[graph.EdgeCalls] != 2 || counts[graph.EdgeReferences] != 1 {
		t.Fatalf("EdgesByKinds(Calls,References) = %+v, want Calls:2 References:1", counts)
	}
	if got := len(counts); got != 2 {
		t.Fatalf("EdgesByKinds(Calls,References) yielded %d distinct kinds, want 2", got)
	}

	// Single-kind via the multi-kind path must match EdgesByKind.
	single := 0
	for e := range es.EdgesByKinds([]graph.EdgeKind{graph.EdgeWrites}) {
		if e.Kind != graph.EdgeWrites {
			t.Fatalf("EdgesByKinds(Writes) yielded kind=%s, want Writes", e.Kind)
		}
		single++
	}
	if single != 1 {
		t.Fatalf("EdgesByKinds(Writes) yielded %d, want 1", single)
	}

	// Dedupe: repeating a kind must not double-yield. The backend's
	// IN-list MUST collapse duplicates.
	dup := 0
	for range es.EdgesByKinds([]graph.EdgeKind{graph.EdgeCalls, graph.EdgeCalls}) {
		dup++
	}
	if dup != 2 {
		t.Fatalf("EdgesByKinds(Calls,Calls) yielded %d, want 2 (no double-yield)", dup)
	}

	// Empty kinds yields nothing — never a whole-table scan.
	empty := 0
	for range es.EdgesByKinds(nil) {
		empty++
	}
	if empty != 0 {
		t.Fatalf("EdgesByKinds(nil) yielded %d, want 0", empty)
	}
	emptySlice := 0
	for range es.EdgesByKinds([]graph.EdgeKind{}) {
		emptySlice++
	}
	if emptySlice != 0 {
		t.Fatalf("EdgesByKinds([]) yielded %d, want 0", emptySlice)
	}

	// Empty string kinds get elided (matches dedupeEdgeKinds contract).
	blank := 0
	for range es.EdgesByKinds([]graph.EdgeKind{"", "", ""}) {
		blank++
	}
	if blank != 0 {
		t.Fatalf("EdgesByKinds(blank) yielded %d, want 0", blank)
	}

	// Zero-match: a kind nothing in the graph uses yields nothing.
	zero := 0
	for range es.EdgesByKinds([]graph.EdgeKind{graph.EdgeKind("nonexistent")}) {
		zero++
	}
	if zero != 0 {
		t.Fatalf("EdgesByKinds(nonexistent) yielded %d, want 0", zero)
	}

	// Early stop honours the iterator contract.
	stopped := 0
	for range es.EdgesByKinds([]graph.EdgeKind{graph.EdgeCalls, graph.EdgeReferences}) {
		stopped++
		if stopped == 1 {
			break
		}
	}
	if stopped != 1 {
		t.Fatalf("early stop yielded %d before break, want 1", stopped)
	}
}

func testGetRepoNodesByLanguage(t *testing.T, factory Factory) {
	t.Helper()
	s := factory(t)

	nodes := []*graph.Node{
		{ID: "empty-go", Name: "empty-go", Kind: graph.KindFunction, FilePath: "a.go", Language: "go"},
		{ID: "empty-py", Name: "empty-py", Kind: graph.KindFunction, FilePath: "a.py", Language: "python"},
		{ID: "repo-go", Name: "repo-go", Kind: graph.KindFunction, FilePath: "repo/a.go", Language: "go", RepoPrefix: "repo"},
		{ID: "repo-py", Name: "repo-py", Kind: graph.KindFunction, FilePath: "repo/a.py", Language: "python", RepoPrefix: "repo"},
	}
	for _, n := range nodes {
		s.AddNode(n)
	}

	if got := s.GetRepoNodesByLanguage("", "go"); len(got) != 1 || got[0].ID != "empty-go" {
		t.Fatalf("GetRepoNodesByLanguage(empty, go) = %#v, want empty-go", got)
	}
	if got := s.GetRepoNodesByLanguage("repo", "go"); len(got) != 1 || got[0].ID != "repo-go" {
		t.Fatalf("GetRepoNodesByLanguage(repo, go) = %#v, want repo-go", got)
	}
	if got := s.GetRepoNodesByLanguage("repo", ""); len(got) != 0 {
		t.Fatalf("GetRepoNodesByLanguage(repo, empty) yielded %d nodes, want 0", len(got))
	}
}

func testDistinctExternalTargets(t *testing.T, factory Factory) {
	t.Helper()
	s := factory(t)
	add := func(to string, kind graph.EdgeKind, line int) {
		s.AddEdge(&graph.Edge{From: "caller", To: to, Kind: kind, FilePath: "a.go", Line: line})
	}

	add("dep::example.com/pkg::Call", graph.EdgeCalls, 1)
	add("dep::example.com/pkg::Call", graph.EdgeReferences, 2) // duplicate target
	add("stdlib::fmt::Sprintf", graph.EdgeReferences, 3)
	add("repo::stdlib::net/http::Get", graph.EdgeReads, 4)
	add("external::service/api", graph.EdgeCalls, 5)
	add("external-call::dep::example.com/old", graph.EdgeCalls, 6)
	add("local.go::resolved", graph.EdgeCalls, 7)
	add("dep::wrong-kind::Call", graph.EdgeTests, 8)

	got := s.DistinctExternalTargets([]graph.EdgeKind{
		graph.EdgeCalls, graph.EdgeReferences, graph.EdgeReads, graph.EdgeCalls,
	})
	want := []string{
		"dep::example.com/pkg::Call",
		"external-call::dep::example.com/old",
		"external::service/api",
		"repo::stdlib::net/http::Get",
		"stdlib::fmt::Sprintf",
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("DistinctExternalTargets = %v, want %v", got, want)
	}
	if got := s.DistinctExternalTargets(nil); len(got) != 0 {
		t.Fatalf("DistinctExternalTargets(nil) yielded %d targets, want 0", len(got))
	}
}

// testNodesByKindsScanner exercises the optional graph.NodesByKindsScanner
// capability. Seeds nodes of several kinds, including ones whose Meta
// holds the keys the metadata analyzers read, and asserts:
//   - the IN-list returns exactly the union of the requested kinds
//     (with nodes' Meta intact so post-filtering still works);
//   - kinds the caller did not request never surface;
//   - empty / nil kinds returns nil without scanning;
//   - duplicate kinds in the input never duplicate the output.
//
// The Meta-preservation assertion is the load-bearing one: every
// downstream handler still runs its meta gate in Go after the kind
// pushdown, so the capability is worthless if Meta doesn't round-trip
// through the backend.
func testNodesByKindsScanner(t *testing.T, factory Factory) {
	t.Helper()
	s := factory(t)
	scan, ok := s.(graph.NodesByKindsScanner)
	if !ok {
		t.Skip("backend does not implement graph.NodesByKindsScanner")
	}

	// Two functions (one with coverage meta), one method, one type,
	// one file (with cgo meta), one todo (with assignee meta), one
	// table. Mix of meta-bearing and meta-bare nodes so the
	// round-trip assertion covers both shapes. Meta values stay
	// scalar — testMetaPreserved already covers flat round-trip, and
	// the disk backend's gob encoder needs gob.Register for nested
	// map shapes (out of scope for a kind-pushdown capability test).
	fn1 := mkNode("pkg/a.go::Fn1", "Fn1", "pkg/a.go", graph.KindFunction)
	fn1.Meta = map[string]any{
		"coverage_pct": 42.5,
		"author_email": "alice@example.com",
	}
	fn2 := mkNode("pkg/a.go::Fn2", "Fn2", "pkg/a.go", graph.KindFunction)
	method := mkNode("pkg/a.go::T.M", "M", "pkg/a.go", graph.KindMethod)
	typ := mkNode("pkg/a.go::T", "T", "pkg/a.go", graph.KindType)
	file := mkNode("pkg/a.go", "a.go", "pkg/a.go", graph.KindFile)
	file.Meta = map[string]any{"uses_cgo": true}
	todo := mkNode("pkg/a.go::TODO:7", "TODO", "pkg/a.go", graph.KindTodo)
	todo.Meta = map[string]any{
		"tag":      "TODO",
		"assignee": "alice",
		"text":     "wire this up",
	}
	tbl := mkNode("table::users", "users", "schema/001.sql", graph.KindTable)
	tbl.Meta = map[string]any{"table": "users", "dialect": "postgres"}

	for _, n := range []*graph.Node{fn1, fn2, method, typ, file, todo, tbl} {
		s.AddNode(n)
	}

	// Function + method — the stale_code/ownership/coverage default.
	gotFnM := scan.NodesByKinds([]graph.NodeKind{graph.KindFunction, graph.KindMethod})
	wantFnM := []string{"pkg/a.go::Fn1", "pkg/a.go::Fn2", "pkg/a.go::T.M"}
	if got := sortNodeIDs(gotFnM); fmt.Sprint(got) != fmt.Sprint(wantFnM) {
		t.Fatalf("NodesByKinds(function,method) = %v, want %v", got, wantFnM)
	}

	// Meta round-trip: pick up Fn1 and assert flat scalar meta survived.
	var fn1Got *graph.Node
	for _, n := range gotFnM {
		if n.ID == "pkg/a.go::Fn1" {
			fn1Got = n
			break
		}
	}
	if fn1Got == nil {
		t.Fatalf("Fn1 missing from result")
		return
	}
	if pct, _ := fn1Got.Meta["coverage_pct"].(float64); pct != 42.5 {
		t.Fatalf("Fn1.Meta.coverage_pct = %v, want 42.5", fn1Got.Meta["coverage_pct"])
	}
	if email, _ := fn1Got.Meta["author_email"].(string); email != "alice@example.com" {
		t.Fatalf("Fn1.Meta.author_email = %q, want alice@example.com", email)
	}

	// Single kind on a kind with meta — todo/file.
	gotTodo := scan.NodesByKinds([]graph.NodeKind{graph.KindTodo})
	if len(gotTodo) != 1 || gotTodo[0].ID != "pkg/a.go::TODO:7" {
		t.Fatalf("NodesByKinds(todo) = %v, want [pkg/a.go::TODO:7]", sortNodeIDs(gotTodo))
	}
	if tag, _ := gotTodo[0].Meta["tag"].(string); tag != "TODO" {
		t.Fatalf("Todo.Meta.tag = %q, want TODO", tag)
	}

	gotFile := scan.NodesByKinds([]graph.NodeKind{graph.KindFile})
	if len(gotFile) != 1 || gotFile[0].ID != "pkg/a.go" {
		t.Fatalf("NodesByKinds(file) = %v, want [pkg/a.go]", sortNodeIDs(gotFile))
	}
	if cgo, _ := gotFile[0].Meta["uses_cgo"].(bool); !cgo {
		t.Fatalf("File.Meta.uses_cgo = false, want true")
	}

	// Table kind — for orphan/unreferenced analyzers.
	gotTbl := scan.NodesByKinds([]graph.NodeKind{graph.KindTable})
	if len(gotTbl) != 1 || gotTbl[0].ID != "table::users" {
		t.Fatalf("NodesByKinds(table) = %v, want [table::users]", sortNodeIDs(gotTbl))
	}

	// Empty / nil kinds — nil result, no scan.
	if got := scan.NodesByKinds(nil); got != nil {
		t.Fatalf("NodesByKinds(nil) = %v, want nil", got)
	}
	if got := scan.NodesByKinds([]graph.NodeKind{}); got != nil {
		t.Fatalf("NodesByKinds([]) = %v, want nil", got)
	}

	// Unknown kind — no rows, but still nil/empty, never the full table.
	if got := scan.NodesByKinds([]graph.NodeKind{graph.NodeKind("no_such_kind")}); len(got) != 0 {
		t.Fatalf("NodesByKinds(unknown) = %v, want 0 rows", got)
	}

	// Dedup: passing the same kind twice must not double-yield.
	gotDup := scan.NodesByKinds([]graph.NodeKind{graph.KindFunction, graph.KindFunction})
	wantDup := []string{"pkg/a.go::Fn1", "pkg/a.go::Fn2"}
	if got := sortNodeIDs(gotDup); fmt.Sprint(got) != fmt.Sprint(wantDup) {
		t.Fatalf("NodesByKinds(dup function) = %v, want %v", got, wantDup)
	}
}

// testEdgeKindCounter exercises the optional graph.EdgeKindCounter
// capability. Seeds a graph with several kinds in different
// frequencies and asserts the per-kind tally matches what an
// AllEdges()+map[kind]++ loop would compute.
func testEdgeKindCounter(t *testing.T, factory Factory) {
	t.Helper()
	s := factory(t)
	ek, ok := s.(graph.EdgeKindCounter)
	if !ok {
		t.Skip("backend does not implement graph.EdgeKindCounter")
	}

	// Empty graph returns nil or empty — both are valid per the
	// contract; callers must treat them the same.
	if got := ek.EdgeKindCounts(); len(got) != 0 {
		t.Fatalf("EdgeKindCounts(empty) = %v, want empty", got)
	}

	s.AddNode(mkNode("A", "A", "a.go", graph.KindFunction))
	s.AddNode(mkNode("B", "B", "a.go", graph.KindFunction))
	s.AddNode(mkNode("C", "C", "a.go", graph.KindFunction))
	s.AddNode(mkNode("f1", "a.go", "a.go", graph.KindFile))

	// 3 calls, 2 references, 1 imports.
	e1 := mkEdge("A", "B", graph.EdgeCalls)
	e2 := mkEdge("A", "C", graph.EdgeCalls)
	e2.Line = 2
	e3 := mkEdge("B", "C", graph.EdgeCalls)
	e3.Line = 3
	e4 := mkEdge("A", "C", graph.EdgeReferences)
	e4.Line = 4
	e5 := mkEdge("B", "C", graph.EdgeReferences)
	e5.Line = 5
	e6 := mkEdge("A", "f1", graph.EdgeImports)
	e6.Line = 6
	s.AddEdge(e1)
	s.AddEdge(e2)
	s.AddEdge(e3)
	s.AddEdge(e4)
	s.AddEdge(e5)
	s.AddEdge(e6)

	got := ek.EdgeKindCounts()
	if got[graph.EdgeCalls] != 3 {
		t.Fatalf("EdgeKindCounts[calls] = %d, want 3", got[graph.EdgeCalls])
	}
	if got[graph.EdgeReferences] != 2 {
		t.Fatalf("EdgeKindCounts[references] = %d, want 2", got[graph.EdgeReferences])
	}
	if got[graph.EdgeImports] != 1 {
		t.Fatalf("EdgeKindCounts[imports] = %d, want 1", got[graph.EdgeImports])
	}
	// No extends edge was added; absence must produce 0 via the
	// zero value (callers index with `m[k]`).
	if got[graph.EdgeExtends] != 0 {
		t.Fatalf("EdgeKindCounts[extends] = %d, want 0", got[graph.EdgeExtends])
	}
}

// testCrossRepoEdgeAggregator exercises the optional
// graph.CrossRepoEdgeAggregator capability. Seeds a two-repo graph
// with one cross_repo_calls + one cross_repo_implements and two
// same-repo edges of other kinds. Asserts the per-triple counts and
// that single-repo edges drop out.
func testCrossRepoEdgeAggregator(t *testing.T, factory Factory) {
	t.Helper()
	s := factory(t)
	ag, ok := s.(graph.CrossRepoEdgeAggregator)
	if !ok {
		t.Skip("backend does not implement graph.CrossRepoEdgeAggregator")
	}

	// Empty graph -> nil.
	if got := ag.CrossRepoEdgeCounts(); got != nil {
		t.Fatalf("CrossRepoEdgeCounts(empty) = %v, want nil", got)
	}

	s.AddNode(mkRepoNode("repoA::Caller", "Caller", "a/c.go", "repoA", graph.KindFunction))
	s.AddNode(mkRepoNode("repoA::Callee2", "Callee2", "a/d.go", "repoA", graph.KindFunction))
	s.AddNode(mkRepoNode("repoB::Callee", "Callee", "b/d.go", "repoB", graph.KindFunction))
	s.AddNode(mkRepoNode("repoB::Iface", "Iface", "b/i.go", "repoB", graph.KindType))
	s.AddNode(mkRepoNode("repoA::Impl", "Impl", "a/i.go", "repoA", graph.KindType))

	// Two cross-repo edges to the same (kind, fromRepo, toRepo) +
	// one cross-repo implements + one non-cross edge.
	e1 := mkEdge("repoA::Caller", "repoB::Callee", graph.EdgeCrossRepoCalls)
	e2 := mkEdge("repoA::Caller", "repoB::Callee", graph.EdgeCrossRepoCalls)
	e2.Line = 2
	e3 := mkEdge("repoA::Impl", "repoB::Iface", graph.EdgeCrossRepoImplements)
	e3.Line = 3
	e4 := mkEdge("repoA::Caller", "repoA::Callee2", graph.EdgeCalls)
	e4.Line = 4
	s.AddEdge(e1)
	s.AddEdge(e2)
	s.AddEdge(e3)
	s.AddEdge(e4)

	rows := ag.CrossRepoEdgeCounts()
	// Sort for stable assertions — capability output order is
	// unspecified.
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Kind != rows[j].Kind {
			return rows[i].Kind < rows[j].Kind
		}
		if rows[i].FromRepo != rows[j].FromRepo {
			return rows[i].FromRepo < rows[j].FromRepo
		}
		return rows[i].ToRepo < rows[j].ToRepo
	})
	if len(rows) != 2 {
		t.Fatalf("CrossRepoEdgeCounts: got %d rows, want 2 (rows=%v)", len(rows), rows)
	}
	if rows[0].Kind != graph.EdgeCrossRepoCalls || rows[0].FromRepo != "repoA" || rows[0].ToRepo != "repoB" || rows[0].Count != 2 {
		t.Fatalf("CrossRepoEdgeCounts[0] = %+v, want {cross_repo_calls,repoA,repoB,2}", rows[0])
	}
	if rows[1].Kind != graph.EdgeCrossRepoImplements || rows[1].FromRepo != "repoA" || rows[1].ToRepo != "repoB" || rows[1].Count != 1 {
		t.Fatalf("CrossRepoEdgeCounts[1] = %+v, want {cross_repo_implements,repoA,repoB,1}", rows[1])
	}
}

// testFileImportAggregator exercises the optional
// graph.FileImportAggregator capability. Seeds a graph with several
// import edges and asserts the per-target-file counts. Covers both
// the unscoped and the scope-bound paths plus the file-node-by-ID
// vs symbol-FilePath import shapes.
func testFileImportAggregator(t *testing.T, factory Factory) {
	t.Helper()
	s := factory(t)
	ag, ok := s.(graph.FileImportAggregator)
	if !ok {
		t.Skip("backend does not implement graph.FileImportAggregator")
	}

	if got := ag.FileImportCounts(nil); got != nil {
		t.Fatalf("FileImportCounts(empty graph) = %v, want nil", got)
	}

	// Two targets, three importing files, mixed shapes.
	s.AddNode(mkNode("pkg/popular.go", "popular.go", "pkg/popular.go", graph.KindFile))
	s.AddNode(mkNode("PopularFn", "PopularFn", "pkg/popular.go", graph.KindFunction))
	s.AddNode(mkNode("pkg/lonely.go", "lonely.go", "pkg/lonely.go", graph.KindFile))
	s.AddNode(mkNode("pkg/a.go", "a.go", "pkg/a.go", graph.KindFile))
	s.AddNode(mkNode("pkg/b.go", "b.go", "pkg/b.go", graph.KindFile))
	s.AddNode(mkNode("pkg/c.go", "c.go", "pkg/c.go", graph.KindFile))

	// pkg/popular.go imported by 3 files (two via file-id, one via symbol-FilePath).
	s.AddEdge(mkEdge("pkg/a.go", "pkg/popular.go", graph.EdgeImports))
	s.AddEdge(mkEdge("pkg/b.go", "pkg/popular.go", graph.EdgeImports))
	s.AddEdge(mkEdge("pkg/c.go", "PopularFn", graph.EdgeImports))
	// pkg/lonely.go imported once.
	s.AddEdge(mkEdge("pkg/a.go", "pkg/lonely.go", graph.EdgeImports))
	// A calls edge — must drop out of imports counts.
	s.AddEdge(mkEdge("pkg/a.go", "PopularFn", graph.EdgeCalls))

	rows := ag.FileImportCounts(nil)
	got := map[string]int{}
	for _, r := range rows {
		got[r.FilePath] = r.Count
	}
	if got["pkg/popular.go"] != 3 {
		t.Fatalf("FileImportCounts[popular.go] = %d, want 3", got["pkg/popular.go"])
	}
	if got["pkg/lonely.go"] != 1 {
		t.Fatalf("FileImportCounts[lonely.go] = %d, want 1", got["pkg/lonely.go"])
	}

	// Scope-bound: only count edges whose target is in the allow set.
	scoped := ag.FileImportCounts([]string{"pkg/lonely.go"})
	if len(scoped) != 1 || scoped[0].FilePath != "pkg/lonely.go" || scoped[0].Count != 1 {
		t.Fatalf("FileImportCounts(scope=lonely) = %v, want [lonely.go:1]", scoped)
	}

	// Scope-bound with file-id + symbol shape both targeting popular.
	scopedPop := ag.FileImportCounts([]string{"pkg/popular.go", "PopularFn"})
	gotPop := map[string]int{}
	for _, r := range scopedPop {
		gotPop[r.FilePath] = r.Count
	}
	if gotPop["pkg/popular.go"] != 3 {
		t.Fatalf("FileImportCounts(scope=popular+sym) = %v, want popular.go:3", scopedPop)
	}

	// Empty (non-nil) scope MUST return nil — never a whole-graph scan.
	if got := ag.FileImportCounts([]string{}); got != nil {
		t.Fatalf("FileImportCounts(empty scope) = %v, want nil", got)
	}
}

// testInDegreeForNodes exercises the optional graph.InDegreeForNodes
// capability. Seeds a tiny graph with three targets carrying 0 / 1 / 3
// incoming edges (of mixed kinds) and asserts the counter returns the
// per-target count restricted to the caller's id set.
func testInDegreeForNodes(t *testing.T, factory Factory) {
	t.Helper()
	s := factory(t)
	ic, ok := s.(graph.InDegreeForNodes)
	if !ok {
		t.Skip("backend does not implement graph.InDegreeForNodes")
	}

	s.AddNode(mkNode("Hub", "Hub", "a.go", graph.KindFunction))
	s.AddNode(mkNode("Lonely", "Lonely", "a.go", graph.KindFunction))
	s.AddNode(mkNode("Isolated", "Isolated", "a.go", graph.KindFunction))
	s.AddNode(mkNode("C1", "C1", "a.go", graph.KindFunction))
	s.AddNode(mkNode("C2", "C2", "a.go", graph.KindFunction))
	s.AddNode(mkNode("C3", "C3", "a.go", graph.KindFunction))
	s.AddNode(mkNode("Outside", "Outside", "a.go", graph.KindFunction))

	e1 := mkEdge("C1", "Hub", graph.EdgeCalls)
	e1.Line = 1
	e2 := mkEdge("C2", "Hub", graph.EdgeReferences)
	e2.Line = 2
	e3 := mkEdge("C3", "Hub", graph.EdgeReads)
	e3.Line = 3
	e4 := mkEdge("C1", "Lonely", graph.EdgeCalls)
	e4.Line = 4
	// One incoming edge that targets Outside — must NOT surface when
	// Outside is absent from the caller's id list.
	e5 := mkEdge("C2", "Outside", graph.EdgeCalls)
	e5.Line = 5
	s.AddEdge(e1)
	s.AddEdge(e2)
	s.AddEdge(e3)
	s.AddEdge(e4)
	s.AddEdge(e5)

	got := ic.InDegreeForNodes([]string{"Hub", "Lonely", "Isolated"})
	if got["Hub"] != 3 {
		t.Fatalf("InDegreeForNodes[Hub] = %d, want 3", got["Hub"])
	}
	if got["Lonely"] != 1 {
		t.Fatalf("InDegreeForNodes[Lonely] = %d, want 1", got["Lonely"])
	}
	// Isolated and Outside are absent — the contract drops zero-count
	// targets from the map.
	if _, ok := got["Isolated"]; ok {
		t.Fatalf("InDegreeForNodes[Isolated] surfaced with value %d, want absent", got["Isolated"])
	}
	if _, ok := got["Outside"]; ok {
		t.Fatalf("InDegreeForNodes[Outside] surfaced — caller didn't ask for it")
	}

	// Empty ids => nil (never a whole-table scan).
	if got := ic.InDegreeForNodes(nil); got != nil {
		t.Fatalf("InDegreeForNodes(nil) = %v, want nil", got)
	}
	if got := ic.InDegreeForNodes([]string{}); got != nil {
		t.Fatalf("InDegreeForNodes(empty) = %v, want nil", got)
	}
	// Duplicated ids dedup naturally.
	dup := ic.InDegreeForNodes([]string{"Hub", "Hub", "Hub"})
	if dup["Hub"] != 3 {
		t.Fatalf("InDegreeForNodes(dup Hub) = %d, want 3", dup["Hub"])
	}
}

// testReachableForwardByKinds exercises the optional
// graph.ReachableForwardByKinds capability. Seeds a small directed
// graph mixing allowed and disallowed edge kinds, then asserts the
// closure from the seed set is the transitive subset reachable
// through only the allowed kinds.
func testReachableForwardByKinds(t *testing.T, factory Factory) {
	t.Helper()
	s := factory(t)
	rf, ok := s.(graph.ReachableForwardByKinds)
	if !ok {
		t.Skip("backend does not implement graph.ReachableForwardByKinds")
	}

	// Layout:
	//   Test -> A (calls)
	//   A    -> B (calls)
	//   B    -> C (references)
	//   C    -> D (reads)  <-- disallowed kind: D unreachable
	//   X    -> Y (calls)  <-- disjoint subgraph: neither in closure
	for _, id := range []string{"Test", "A", "B", "C", "D", "X", "Y"} {
		s.AddNode(mkNode(id, id, "a.go", graph.KindFunction))
	}
	e1 := mkEdge("Test", "A", graph.EdgeCalls)
	e1.Line = 1
	e2 := mkEdge("A", "B", graph.EdgeCalls)
	e2.Line = 2
	e3 := mkEdge("B", "C", graph.EdgeReferences)
	e3.Line = 3
	e4 := mkEdge("C", "D", graph.EdgeReads)
	e4.Line = 4
	e5 := mkEdge("X", "Y", graph.EdgeCalls)
	e5.Line = 5
	s.AddEdge(e1)
	s.AddEdge(e2)
	s.AddEdge(e3)
	s.AddEdge(e4)
	s.AddEdge(e5)

	kinds := []graph.EdgeKind{graph.EdgeCalls, graph.EdgeReferences}
	got := rf.ReachableForwardByKinds([]string{"Test"}, kinds)
	want := map[string]bool{"Test": true, "A": true, "B": true, "C": true}
	for id := range want {
		if !got[id] {
			t.Fatalf("ReachableForwardByKinds: missing %q in closure %v", id, got)
		}
	}
	if got["D"] {
		t.Fatalf("ReachableForwardByKinds: D should not be reachable (reads is disallowed)")
	}
	if got["X"] || got["Y"] {
		t.Fatalf("ReachableForwardByKinds: disjoint subgraph leaked: %v", got)
	}

	// Empty seeds => nil.
	if got := rf.ReachableForwardByKinds(nil, kinds); got != nil {
		t.Fatalf("ReachableForwardByKinds(nil) = %v, want nil", got)
	}
	// Empty kinds => seed set only.
	zero := rf.ReachableForwardByKinds([]string{"Test"}, nil)
	if !zero["Test"] || zero["A"] {
		t.Fatalf("ReachableForwardByKinds(no kinds) = %v, want {Test:true}", zero)
	}
	// Duplicate seeds dedup naturally.
	dup := rf.ReachableForwardByKinds([]string{"Test", "Test"}, kinds)
	if !dup["Test"] || !dup["A"] || !dup["B"] || !dup["C"] {
		t.Fatalf("ReachableForwardByKinds(dup seeds) = %v, want full closure", dup)
	}
}

// testBFS exercises the optional graph.BFSCapable capability. The same
// fixture and assertions run against every backend the factory produces,
// so the in-memory reference walk and the SQLite recursive-CTE walk are
// shadow-tested for identical hop-sets — including a cycle, a node-backed
// gate (an unresolved target), a min-depth-via-two-parents tie-break, the
// depth bound, and deterministic limit truncation.
func testBFS(t *testing.T, factory Factory) {
	t.Helper()
	s := factory(t)
	bc, ok := s.(graph.BFSCapable)
	if !ok {
		t.Skip("backend does not implement graph.BFSCapable")
	}

	// Layout (functions):
	//   A -calls->      B, C
	//   B -calls->      D        B -references-> E
	//   C -calls->      D
	//   D -calls->      A (cycle)   D -calls-> unresolved::ghost (no node)
	//   E -references-> F
	for _, id := range []string{"A", "B", "C", "D", "E", "F"} {
		s.AddNode(mkNode(id, id, "a.go", graph.KindFunction))
	}
	addE := func(from, to string, kind graph.EdgeKind, line int) {
		e := mkEdge(from, to, kind)
		e.Line = line
		s.AddEdge(e)
	}
	addE("A", "B", graph.EdgeCalls, 1)
	addE("A", "C", graph.EdgeCalls, 2)
	addE("B", "D", graph.EdgeCalls, 3)
	addE("C", "D", graph.EdgeCalls, 4)
	addE("D", "A", graph.EdgeCalls, 5) // cycle back to the seed
	addE("D", "unresolved::ghost", graph.EdgeCalls, 6)
	addE("B", "E", graph.EdgeReferences, 7)
	addE("E", "F", graph.EdgeReferences, 8)

	calls := []graph.EdgeKind{graph.EdgeCalls}
	callsRefs := []graph.EdgeKind{graph.EdgeCalls, graph.EdgeReferences}

	must := func(hops []graph.BFSHop, err error) []graph.BFSHop {
		t.Helper()
		if err != nil {
			t.Fatalf("BFS error: %v", err)
		}
		return hops
	}
	assertHops := func(name string, got, want []graph.BFSHop) {
		t.Helper()
		if len(got) != len(want) {
			t.Fatalf("%s: got %d hops %+v, want %d %+v", name, len(got), got, len(want), want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("%s: hop[%d] = %+v, want %+v (full=%+v)", name, i, got[i], want[i], got)
			}
		}
	}

	// Forward, calls only: the cycle terminates; D settles at min depth 2
	// via the (parent,kind)-smallest edge (B < C); the unresolved target and
	// the references edges are excluded.
	assertHops("fwd/calls", must(bc.BFS([]string{"A"}, graph.DirectionForward, calls, 5, 0)), []graph.BFSHop{
		{NodeID: "A", Depth: 0},
		{NodeID: "B", Depth: 1, ParentID: "A", EdgeKind: graph.EdgeCalls},
		{NodeID: "C", Depth: 1, ParentID: "A", EdgeKind: graph.EdgeCalls},
		{NodeID: "D", Depth: 2, ParentID: "B", EdgeKind: graph.EdgeCalls},
	})

	// Forward, calls + references: E reached via B (references), F via E.
	assertHops("fwd/callsRefs", must(bc.BFS([]string{"A"}, graph.DirectionForward, callsRefs, 5, 0)), []graph.BFSHop{
		{NodeID: "A", Depth: 0},
		{NodeID: "B", Depth: 1, ParentID: "A", EdgeKind: graph.EdgeCalls},
		{NodeID: "C", Depth: 1, ParentID: "A", EdgeKind: graph.EdgeCalls},
		{NodeID: "D", Depth: 2, ParentID: "B", EdgeKind: graph.EdgeCalls},
		{NodeID: "E", Depth: 2, ParentID: "B", EdgeKind: graph.EdgeReferences},
		{NodeID: "F", Depth: 3, ParentID: "E", EdgeKind: graph.EdgeReferences},
	})

	// Backward (callers) from D, calls only: A is reached at depth 2 via the
	// (parent,kind)-smallest caller (B < C).
	assertHops("bwd/calls", must(bc.BFS([]string{"D"}, graph.DirectionBackward, calls, 5, 0)), []graph.BFSHop{
		{NodeID: "D", Depth: 0},
		{NodeID: "B", Depth: 1, ParentID: "D", EdgeKind: graph.EdgeCalls},
		{NodeID: "C", Depth: 1, ParentID: "D", EdgeKind: graph.EdgeCalls},
		{NodeID: "A", Depth: 2, ParentID: "B", EdgeKind: graph.EdgeCalls},
	})

	// Depth bound cuts the walk off before D.
	assertHops("fwd/depth1", must(bc.BFS([]string{"A"}, graph.DirectionForward, calls, 1, 0)), []graph.BFSHop{
		{NodeID: "A", Depth: 0},
		{NodeID: "B", Depth: 1, ParentID: "A", EdgeKind: graph.EdgeCalls},
		{NodeID: "C", Depth: 1, ParentID: "A", EdgeKind: graph.EdgeCalls},
	})

	// Limit truncates deterministically after (depth, node_id) ordering.
	assertHops("fwd/limit2", must(bc.BFS([]string{"A"}, graph.DirectionForward, calls, 5, 2)), []graph.BFSHop{
		{NodeID: "A", Depth: 0},
		{NodeID: "B", Depth: 1, ParentID: "A", EdgeKind: graph.EdgeCalls},
	})

	// Empty kinds and a zero depth bound both yield the seed only.
	assertHops("fwd/noKinds", must(bc.BFS([]string{"A"}, graph.DirectionForward, nil, 5, 0)), []graph.BFSHop{
		{NodeID: "A", Depth: 0},
	})
	assertHops("fwd/depth0", must(bc.BFS([]string{"A"}, graph.DirectionForward, calls, 0, 0)), []graph.BFSHop{
		{NodeID: "A", Depth: 0},
	})

	// Empty seeds returns nil; duplicate seeds collapse.
	if got := must(bc.BFS(nil, graph.DirectionForward, calls, 5, 0)); got != nil {
		t.Fatalf("BFS(nil seeds) = %+v, want nil", got)
	}
	assertHops("fwd/dupSeeds", must(bc.BFS([]string{"A", "A"}, graph.DirectionForward, calls, 1, 0)), []graph.BFSHop{
		{NodeID: "A", Depth: 0},
		{NodeID: "B", Depth: 1, ParentID: "A", EdgeKind: graph.EdgeCalls},
		{NodeID: "C", Depth: 1, ParentID: "A", EdgeKind: graph.EdgeCalls},
	})
}

// testThrowerErrorSurfacer exercises the optional
// graph.ThrowerErrorSurfacer capability. Seeds throwers with mixed
// error targets and EdgeEmits→KindString attachments, asserts the
// per-thrower row dedup + path-prefix filter both fire.
func testThrowerErrorSurfacer(t *testing.T, factory Factory) {
	t.Helper()
	s := factory(t)
	ts, ok := s.(graph.ThrowerErrorSurfacer)
	if !ok {
		t.Skip("backend does not implement graph.ThrowerErrorSurfacer")
	}

	// Throwers ThrowA (in pkg/keep/), ThrowB (in pkg/drop/). Targets
	// ErrIO + ErrTimeout. ThrowA also emits two literal error_msg
	// strings; one EdgeEmits goes to a non-error_msg context that
	// must NOT surface in ErrorMsgs.
	s.AddNode(mkNode("ThrowA", "ThrowA", "pkg/keep/a.go", graph.KindFunction))
	s.AddNode(mkNode("ThrowB", "ThrowB", "pkg/drop/b.go", graph.KindFunction))
	s.AddNode(mkNode("ErrIO", "ErrIO", "errors/io.go", graph.KindType))
	s.AddNode(mkNode("ErrTimeout", "ErrTimeout", "errors/io.go", graph.KindType))

	msgOK1 := mkNode("msg1", "open failed", "pkg/keep/a.go", graph.KindString)
	msgOK1.Meta = map[string]any{"context": "error_msg"}
	s.AddNode(msgOK1)
	msgOK2 := mkNode("msg2", "timeout", "pkg/keep/a.go", graph.KindString)
	msgOK2.Meta = map[string]any{"context": "error_msg"}
	s.AddNode(msgOK2)
	// Wrong context — must be filtered out.
	msgWrong := mkNode("msg3", "log line", "pkg/keep/a.go", graph.KindString)
	msgWrong.Meta = map[string]any{"context": "log_msg"}
	s.AddNode(msgWrong)

	// ThrowA throws ErrIO twice (dedup to one target) + ErrTimeout once.
	e1 := mkEdge("ThrowA", "ErrIO", graph.EdgeThrows)
	e1.FilePath = "pkg/keep/a.go"
	e1.Line = 10
	e2 := mkEdge("ThrowA", "ErrIO", graph.EdgeThrows)
	e2.FilePath = "pkg/keep/a.go"
	e2.Line = 12
	e3 := mkEdge("ThrowA", "ErrTimeout", graph.EdgeThrows)
	e3.FilePath = "pkg/keep/a.go"
	e3.Line = 14
	// ThrowB throws ErrIO once.
	e4 := mkEdge("ThrowB", "ErrIO", graph.EdgeThrows)
	e4.FilePath = "pkg/drop/b.go"
	e4.Line = 4
	// EdgeEmits attachments for ThrowA.
	e5 := mkEdge("ThrowA", "msg1", graph.EdgeEmits)
	e5.Line = 11
	e6 := mkEdge("ThrowA", "msg2", graph.EdgeEmits)
	e6.Line = 13
	e7 := mkEdge("ThrowA", "msg3", graph.EdgeEmits)
	e7.Line = 15
	for _, e := range []*graph.Edge{e1, e2, e3, e4, e5, e6, e7} {
		s.AddEdge(e)
	}

	rows := ts.ThrowerErrorSurface("")
	byID := map[string]graph.ThrowerErrorRow{}
	for _, r := range rows {
		byID[r.ThrowerID] = r
	}

	a, ok := byID["ThrowA"]
	if !ok {
		t.Fatalf("ThrowerErrorSurface: ThrowA missing from rows %v", rows)
	}
	if a.Throws != 3 {
		t.Fatalf("ThrowA.Throws = %d, want 3", a.Throws)
	}
	gotTargets := append([]string(nil), a.ErrorTargets...)
	sort.Strings(gotTargets)
	if fmt.Sprint(gotTargets) != fmt.Sprint([]string{"ErrIO", "ErrTimeout"}) {
		t.Fatalf("ThrowA.ErrorTargets = %v, want [ErrIO ErrTimeout]", gotTargets)
	}
	gotMsgs := append([]string(nil), a.ErrorMsgs...)
	sort.Strings(gotMsgs)
	if fmt.Sprint(gotMsgs) != fmt.Sprint([]string{"open failed", "timeout"}) {
		t.Fatalf("ThrowA.ErrorMsgs = %v, want [open failed timeout]", gotMsgs)
	}

	b, ok := byID["ThrowB"]
	if !ok || b.Throws != 1 || len(b.ErrorTargets) != 1 || b.ErrorTargets[0] != "ErrIO" {
		t.Fatalf("ThrowB row = %+v, want Throws=1 ErrorTargets=[ErrIO]", b)
	}
	if len(b.ErrorMsgs) != 0 {
		t.Fatalf("ThrowB.ErrorMsgs = %v, want empty", b.ErrorMsgs)
	}

	// Path-prefix filter drops ThrowB (under pkg/drop/) and keeps ThrowA.
	keep := ts.ThrowerErrorSurface("pkg/keep/")
	if len(keep) != 1 || keep[0].ThrowerID != "ThrowA" {
		t.Fatalf("ThrowerErrorSurface(pkg/keep/) = %v, want only ThrowA", keep)
	}
	drop := ts.ThrowerErrorSurface("pkg/missing/")
	if len(drop) != 0 {
		t.Fatalf("ThrowerErrorSurface(pkg/missing/) = %v, want empty", drop)
	}
}

// testEdgeAdjacencyForKinds exercises the optional
// graph.EdgeAdjacencyForKinds capability. Seeds a graph mixing
// function/method/type nodes joined by Calls / References / Writes
// edges and asserts the iterator yields only (from, to) pairs whose
// edge kind is in the allowed set AND whose endpoints both fall in
// the allowed node-kind set.
func testEdgeAdjacencyForKinds(t *testing.T, factory Factory) {
	t.Helper()
	s := factory(t)
	scan, ok := s.(graph.EdgeAdjacencyForKinds)
	if !ok {
		t.Skip("backend does not implement graph.EdgeAdjacencyForKinds")
	}

	s.AddNode(mkNode("F1", "F1", "x.go", graph.KindFunction))
	s.AddNode(mkNode("F2", "F2", "x.go", graph.KindFunction))
	s.AddNode(mkNode("M1", "M1", "x.go", graph.KindMethod))
	s.AddNode(mkNode("T1", "T1", "y.go", graph.KindType))
	s.AddNode(mkNode("V1", "V1", "y.go", graph.KindVariable))

	// F1 → F2 Calls (function→function, in-set)
	e1 := mkEdge("F1", "F2", graph.EdgeCalls)
	e1.Line = 1
	// F2 → M1 References (function→method, in-set)
	e2 := mkEdge("F2", "M1", graph.EdgeReferences)
	e2.Line = 2
	// F1 → T1 References (function→type, NOT in-set: T1 excluded)
	e3 := mkEdge("F1", "T1", graph.EdgeReferences)
	e3.Line = 3
	// T1 → F2 References (type→function, NOT in-set: T1 excluded)
	e4 := mkEdge("T1", "F2", graph.EdgeReferences)
	e4.Line = 4
	// M1 → F1 Writes (method→function, edge kind excluded)
	e5 := mkEdge("M1", "F1", graph.EdgeWrites)
	e5.Line = 5
	// F1 → V1 References (function→variable, NOT in-set: V1 excluded)
	e6 := mkEdge("F1", "V1", graph.EdgeReferences)
	e6.Line = 6
	for _, e := range []*graph.Edge{e1, e2, e3, e4, e5, e6} {
		s.AddEdge(e)
	}

	eKinds := []graph.EdgeKind{graph.EdgeCalls, graph.EdgeReferences}
	nKinds := []graph.NodeKind{graph.KindFunction, graph.KindMethod}

	got := make(map[[2]string]int)
	for pair := range scan.EdgeAdjacencyForKinds(eKinds, nKinds) {
		got[pair]++
	}
	want := map[[2]string]int{
		{"F1", "F2"}: 1,
		{"F2", "M1"}: 1,
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("EdgeAdjacencyForKinds = %v, want %v", got, want)
	}

	// Empty edge kinds yields nothing — never a whole-table scan.
	empty := 0
	for range scan.EdgeAdjacencyForKinds(nil, nKinds) {
		empty++
	}
	if empty != 0 {
		t.Fatalf("EdgeAdjacencyForKinds(nil edges) yielded %d, want 0", empty)
	}
	// Empty node kinds yields nothing.
	for range scan.EdgeAdjacencyForKinds(eKinds, nil) {
		empty++
	}
	if empty != 0 {
		t.Fatalf("EdgeAdjacencyForKinds(nil nodes) yielded %d, want 0", empty)
	}
	// Zero-match: edge kind absent from graph yields nothing.
	zero := 0
	for range scan.EdgeAdjacencyForKinds([]graph.EdgeKind{graph.EdgeKind("nonexistent")}, nKinds) {
		zero++
	}
	if zero != 0 {
		t.Fatalf("EdgeAdjacencyForKinds(nonexistent edge) yielded %d, want 0", zero)
	}
	// Node-kind filter actually narrows: asking only for {Type} drops every pair.
	narrowed := 0
	for range scan.EdgeAdjacencyForKinds(eKinds, []graph.NodeKind{graph.KindType}) {
		narrowed++
	}
	if narrowed != 0 {
		t.Fatalf("EdgeAdjacencyForKinds(Type only) yielded %d, want 0", narrowed)
	}
	// Early stop honours the iterator contract.
	stopped := 0
	for range scan.EdgeAdjacencyForKinds(eKinds, nKinds) {
		stopped++
		if stopped == 1 {
			break
		}
	}
	if stopped != 1 {
		t.Fatalf("early stop yielded %d before break, want 1", stopped)
	}
}

// testCommunityCrossingsByKind exercises the optional
// graph.CommunityCrossingsByKind capability. Seeds a small graph
// with a known community partition and asserts per-source crossing
// counts match for: no edges, all-same-community, all-cross, mixed.
func testCommunityCrossingsByKind(t *testing.T, factory Factory) {
	t.Helper()
	s := factory(t)
	scan, ok := s.(graph.CommunityCrossingsByKind)
	if !ok {
		t.Skip("backend does not implement graph.CommunityCrossingsByKind")
	}

	s.AddNode(mkNode("A1", "A1", "x.go", graph.KindFunction))
	s.AddNode(mkNode("A2", "A2", "x.go", graph.KindFunction))
	s.AddNode(mkNode("B1", "B1", "y.go", graph.KindFunction))
	s.AddNode(mkNode("B2", "B2", "y.go", graph.KindFunction))
	s.AddNode(mkNode("C1", "C1", "z.go", graph.KindFunction))

	// A1 → A2 Calls (same community A — NOT a crossing)
	e1 := mkEdge("A1", "A2", graph.EdgeCalls)
	e1.Line = 1
	// A1 → B1 Calls (A→B — crossing)
	e2 := mkEdge("A1", "B1", graph.EdgeCalls)
	e2.Line = 2
	// A1 → C1 References (A→C — crossing, second from A1)
	e3 := mkEdge("A1", "C1", graph.EdgeReferences)
	e3.Line = 3
	// B1 → B2 References (same community B — NOT a crossing)
	e4 := mkEdge("B1", "B2", graph.EdgeReferences)
	e4.Line = 4
	// B2 → C1 Calls (B→C — crossing)
	e5 := mkEdge("B2", "C1", graph.EdgeCalls)
	e5.Line = 5
	// A2 → B2 Writes (different community but edge kind excluded)
	e6 := mkEdge("A2", "B2", graph.EdgeWrites)
	e6.Line = 6
	for _, e := range []*graph.Edge{e1, e2, e3, e4, e5, e6} {
		s.AddEdge(e)
	}

	communities := map[string]string{
		"A1": "A", "A2": "A",
		"B1": "B", "B2": "B",
		"C1": "C",
	}
	kinds := []graph.EdgeKind{graph.EdgeCalls, graph.EdgeReferences}

	got := scan.CommunityCrossingsByKind(kinds, communities)
	want := map[string]int{
		"A1": 2, // → B1 + → C1
		"B2": 1, // → C1
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("CommunityCrossingsByKind(mixed) = %v, want %v", got, want)
	}

	// All-same-community partition: no crossings at all.
	same := map[string]string{
		"A1": "A", "A2": "A", "B1": "A", "B2": "A", "C1": "A",
	}
	if r := scan.CommunityCrossingsByKind(kinds, same); len(r) != 0 {
		t.Fatalf("CommunityCrossingsByKind(all-same) = %v, want empty", r)
	}

	// All-cross-community partition: every edge in scope is a crossing.
	allCross := map[string]string{
		"A1": "1", "A2": "2", "B1": "3", "B2": "4", "C1": "5",
	}
	allGot := scan.CommunityCrossingsByKind(kinds, allCross)
	allWant := map[string]int{
		"A1": 3, // A1 has 3 in-scope out-edges
		"B1": 1, // B1 → B2 (now also a crossing)
		"B2": 1, // B2 → C1
	}
	if fmt.Sprint(allGot) != fmt.Sprint(allWant) {
		t.Fatalf("CommunityCrossingsByKind(all-cross) = %v, want %v", allGot, allWant)
	}

	// Empty kinds returns nil — never a whole-table scan.
	if r := scan.CommunityCrossingsByKind(nil, communities); r != nil {
		t.Fatalf("CommunityCrossingsByKind(nil kinds) = %v, want nil", r)
	}
	// Empty community map returns nil.
	if r := scan.CommunityCrossingsByKind(kinds, nil); r != nil {
		t.Fatalf("CommunityCrossingsByKind(nil comm) = %v, want nil", r)
	}
	// Kind absent from graph yields nil.
	if r := scan.CommunityCrossingsByKind([]graph.EdgeKind{graph.EdgeKind("nonexistent")}, communities); r != nil {
		t.Fatalf("CommunityCrossingsByKind(nonexistent) = %v, want nil", r)
	}
}

// testNodeIDsByKinds exercises the optional graph.NodeIDsByKinds
// capability. Seeds nodes of several kinds and asserts the
// projection returns just the IDs of the requested kinds, with
// duplicates collapsed and empty input returning nil.
func testNodeIDsByKinds(t *testing.T, factory Factory) {
	t.Helper()
	s := factory(t)
	scan, ok := s.(graph.NodeIDsByKinds)
	if !ok {
		t.Skip("backend does not implement graph.NodeIDsByKinds")
	}

	s.AddNode(mkNode("F1", "F1", "x.go", graph.KindFunction))
	s.AddNode(mkNode("F2", "F2", "x.go", graph.KindFunction))
	s.AddNode(mkNode("M1", "M1", "x.go", graph.KindMethod))
	s.AddNode(mkNode("T1", "T1", "y.go", graph.KindType))
	s.AddNode(mkNode("V1", "V1", "y.go", graph.KindVariable))

	got := scan.NodeIDsByKinds([]graph.NodeKind{graph.KindFunction, graph.KindMethod})
	sort.Strings(got)
	want := []string{"F1", "F2", "M1"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("NodeIDsByKinds(Function,Method) = %v, want %v", got, want)
	}

	// Empty kinds returns nil.
	if r := scan.NodeIDsByKinds(nil); r != nil {
		t.Fatalf("NodeIDsByKinds(nil) = %v, want nil", r)
	}
	if r := scan.NodeIDsByKinds([]graph.NodeKind{}); r != nil {
		t.Fatalf("NodeIDsByKinds(empty) = %v, want nil", r)
	}

	// Blank kinds are elided.
	if r := scan.NodeIDsByKinds([]graph.NodeKind{"", ""}); r != nil {
		t.Fatalf("NodeIDsByKinds(blank) = %v, want nil", r)
	}

	// Duplicates collapse — the IN-list must dedupe.
	dup := scan.NodeIDsByKinds([]graph.NodeKind{graph.KindFunction, graph.KindFunction})
	sort.Strings(dup)
	wantDup := []string{"F1", "F2"}
	if fmt.Sprint(dup) != fmt.Sprint(wantDup) {
		t.Fatalf("NodeIDsByKinds(Function,Function) = %v, want %v", dup, wantDup)
	}

	// Kinds absent from the graph yield an empty slice (or nil).
	miss := scan.NodeIDsByKinds([]graph.NodeKind{graph.KindInterface})
	if len(miss) != 0 {
		t.Fatalf("NodeIDsByKinds(Interface) = %v, want empty", miss)
	}
}

// testMemberMethodsByType exercises the optional
// graph.MemberMethodsByType capability. Seeds a graph with multiple
// types, their methods, and a non-method EdgeMemberOf edge to verify
// the kind gate.
func testMemberMethodsByType(t *testing.T, factory Factory) {
	t.Helper()
	s := factory(t)
	scan, ok := s.(graph.MemberMethodsByType)
	if !ok {
		t.Skip("backend does not implement graph.MemberMethodsByType")
	}

	// Two types with method members + a noise field.
	s.AddNode(mkNode("T1", "T1", "a.go", graph.KindType))
	s.AddNode(mkNode("T2", "T2", "b.go", graph.KindType))
	s.AddNode(mkNode("M1", "Foo", "a.go", graph.KindMethod))
	s.AddNode(mkNode("M2", "Bar", "a.go", graph.KindMethod))
	s.AddNode(mkNode("M3", "Foo", "b.go", graph.KindMethod))
	s.AddNode(mkNode("F1", "Field1", "a.go", graph.KindField))

	s.AddEdge(mkEdge("M1", "T1", graph.EdgeMemberOf))
	s.AddEdge(mkEdge("M2", "T1", graph.EdgeMemberOf))
	s.AddEdge(mkEdge("M3", "T2", graph.EdgeMemberOf))
	// Non-method source — must NOT appear.
	s.AddEdge(mkEdge("F1", "T1", graph.EdgeMemberOf))

	got := scan.MemberMethodsByType()
	t1Names := map[string]bool{}
	for _, m := range got["T1"] {
		t1Names[m.Name] = true
	}
	if !t1Names["Foo"] || !t1Names["Bar"] {
		t.Fatalf("MemberMethodsByType T1 = %v, want {Foo, Bar}", got["T1"])
	}
	if len(got["T1"]) != 2 {
		t.Fatalf("MemberMethodsByType T1 size = %d, want 2", len(got["T1"]))
	}
	t2Names := map[string]bool{}
	for _, m := range got["T2"] {
		t2Names[m.Name] = true
	}
	if !t2Names["Foo"] || len(got["T2"]) != 1 {
		t.Fatalf("MemberMethodsByType T2 = %v, want {Foo}", got["T2"])
	}
	// Verify FilePath / StartLine columns are projected.
	for _, m := range got["T1"] {
		if m.MethodID == "" || m.FilePath == "" {
			t.Fatalf("MemberMethodsByType T1 row missing columns: %+v", m)
		}
	}

	// Empty store returns nil.
	empty := factory(t)
	if r := empty.(graph.MemberMethodsByType).MemberMethodsByType(); r != nil {
		t.Fatalf("MemberMethodsByType(empty) = %v, want nil", r)
	}
}

// testStructuralParentEdges exercises the optional
// graph.StructuralParentEdges capability. Seeds a mix of extends /
// implements / composes edges with varying endpoint kinds.
func testStructuralParentEdges(t *testing.T, factory Factory) {
	t.Helper()
	s := factory(t)
	scan, ok := s.(graph.StructuralParentEdges)
	if !ok {
		t.Skip("backend does not implement graph.StructuralParentEdges")
	}

	// Types / interfaces (in-set endpoints).
	s.AddNode(mkNode("C1", "Child", "a.go", graph.KindType))
	s.AddNode(mkNode("P1", "Parent", "a.go", graph.KindType))
	s.AddNode(mkNode("I1", "Iface", "a.go", graph.KindInterface))
	// A method (NOT in-set).
	s.AddNode(mkNode("M1", "Foo", "a.go", graph.KindMethod))

	// In-set: type → type extends.
	e1 := mkEdge("C1", "P1", graph.EdgeExtends)
	e1.Line = 1
	e1.Origin = graph.OriginASTResolved
	// In-set: type → interface implements.
	e2 := mkEdge("C1", "I1", graph.EdgeImplements)
	e2.Line = 2
	e2.Origin = graph.OriginASTInferred
	// In-set: type → type composes.
	e3 := mkEdge("C1", "P1", graph.EdgeComposes)
	e3.Line = 3
	// OUT: extends with a method on one side.
	e4 := mkEdge("M1", "P1", graph.EdgeExtends)
	e4.Line = 4
	// OUT: irrelevant kind.
	e5 := mkEdge("C1", "P1", graph.EdgeCalls)
	e5.Line = 5
	for _, e := range []*graph.Edge{e1, e2, e3, e4, e5} {
		s.AddEdge(e)
	}

	rows := scan.StructuralParentEdges()
	if len(rows) != 3 {
		t.Fatalf("StructuralParentEdges len = %d, want 3 (rows=%v)", len(rows), rows)
	}
	// Verify origin propagation on the ast_resolved row.
	var sawResolved, sawInferred bool
	for _, r := range rows {
		if r.FromID != "C1" {
			t.Fatalf("unexpected FromID %q in row %v", r.FromID, r)
		}
		if r.FromKind != graph.KindType {
			t.Fatalf("unexpected FromKind %q in row %v", r.FromKind, r)
		}
		if r.Origin == graph.OriginASTResolved {
			sawResolved = true
		}
		if r.Origin == graph.OriginASTInferred {
			sawInferred = true
		}
	}
	if !sawResolved || !sawInferred {
		t.Fatalf("origin not propagated: resolved=%v inferred=%v", sawResolved, sawInferred)
	}

	// Empty graph returns nil/empty.
	empty := factory(t)
	if r := empty.(graph.StructuralParentEdges).StructuralParentEdges(); len(r) != 0 {
		t.Fatalf("StructuralParentEdges(empty) = %v, want empty", r)
	}
}

// testCrossRepoCandidates exercises the optional
// graph.CrossRepoCandidates capability. Seeds same-repo and
// cross-repo edges and asserts only the distinct, non-empty
// repo-prefix pairs survive.
func testCrossRepoCandidates(t *testing.T, factory Factory) {
	t.Helper()
	s := factory(t)
	scan, ok := s.(graph.CrossRepoCandidates)
	if !ok {
		t.Skip("backend does not implement graph.CrossRepoCandidates")
	}

	// Repo A.
	s.AddNode(mkRepoNode("A1", "fnA1", "a.go", "repoA", graph.KindFunction))
	s.AddNode(mkRepoNode("A2", "fnA2", "a.go", "repoA", graph.KindFunction))
	// Repo B.
	s.AddNode(mkRepoNode("B1", "fnB1", "b.go", "repoB", graph.KindFunction))
	// No repo.
	s.AddNode(mkNode("X1", "fnX1", "x.go", graph.KindFunction))

	// Same-repo calls — must NOT appear.
	e1 := mkEdge("A1", "A2", graph.EdgeCalls)
	e1.Line = 1
	// Cross-repo call — in.
	e2 := mkEdge("A1", "B1", graph.EdgeCalls)
	e2.Line = 2
	// Cross-repo implements — in.
	e3 := mkEdge("A1", "B1", graph.EdgeImplements)
	e3.Line = 3
	// Cross-repo edge but kind not in baseKinds — out.
	e4 := mkEdge("A1", "B1", graph.EdgeReferences)
	e4.Line = 4
	// Either endpoint missing repo — out.
	e5 := mkEdge("A1", "X1", graph.EdgeCalls)
	e5.Line = 5
	for _, e := range []*graph.Edge{e1, e2, e3, e4, e5} {
		s.AddEdge(e)
	}

	kinds := []graph.EdgeKind{graph.EdgeCalls, graph.EdgeImplements, graph.EdgeExtends}
	rows := scan.CrossRepoCandidates(kinds)
	if len(rows) != 2 {
		t.Fatalf("CrossRepoCandidates len = %d, want 2 (rows=%v)", len(rows), rows)
	}
	for _, r := range rows {
		if r.FromRepo != "repoA" || r.ToRepo != "repoB" {
			t.Fatalf("unexpected repos in row %v", r)
		}
		if r.Edge == nil || r.Edge.From != "A1" || r.Edge.To != "B1" {
			t.Fatalf("unexpected edge in row %v", r)
		}
	}

	// Empty kinds returns nil — never a whole-table scan.
	if r := scan.CrossRepoCandidates(nil); r != nil {
		t.Fatalf("CrossRepoCandidates(nil) = %v, want nil", r)
	}
}

// testExtractCandidates exercises the optional
// graph.ExtractCandidatesScanner capability. Builds a graph with
// three functions:
//   - Long+Hot:   long body, 3 distinct callers, 6 distinct callees
//     (passes every threshold).
//   - Long+Cold:  long body, 1 caller, 6 callees (fails minCallers).
//   - Short+Hot:  short body, 3 callers, 6 callees (fails minLines).
func testExtractCandidates(t *testing.T, factory Factory) {
	t.Helper()
	s := factory(t)
	scan, ok := s.(graph.ExtractCandidatesScanner)
	if !ok {
		t.Skip("backend does not implement graph.ExtractCandidatesScanner")
	}

	mk := func(id string, kind graph.NodeKind, start, end int) *graph.Node {
		n := mkNode(id, id, "p/a.go", kind)
		n.StartLine = start
		n.EndLine = end
		return n
	}
	s.AddNode(mk("LongHot", graph.KindFunction, 1, 60))
	s.AddNode(mk("LongCold", graph.KindFunction, 100, 160))
	s.AddNode(mk("ShortHot", graph.KindFunction, 200, 205))
	// Callers + callees as plain function nodes.
	for i := 0; i < 6; i++ {
		c := mkNode(fmt.Sprintf("C%d", i), fmt.Sprintf("C%d", i), "p/c.go", graph.KindFunction)
		s.AddNode(c)
		t := mkNode(fmt.Sprintf("T%d", i), fmt.Sprintf("T%d", i), "p/t.go", graph.KindFunction)
		s.AddNode(t)
	}
	// LongHot: 3 distinct callers, 6 distinct callees.
	for i := 0; i < 3; i++ {
		e := mkEdge(fmt.Sprintf("C%d", i), "LongHot", graph.EdgeCalls)
		e.Line = i + 1
		s.AddEdge(e)
	}
	for i := 0; i < 6; i++ {
		e := mkEdge("LongHot", fmt.Sprintf("T%d", i), graph.EdgeCalls)
		e.Line = 100 + i
		s.AddEdge(e)
	}
	// LongCold: 1 caller, 6 callees.
	e := mkEdge("C0", "LongCold", graph.EdgeCalls)
	e.Line = 200
	s.AddEdge(e)
	for i := 0; i < 6; i++ {
		e := mkEdge("LongCold", fmt.Sprintf("T%d", i), graph.EdgeCalls)
		e.Line = 300 + i
		s.AddEdge(e)
	}
	// ShortHot: 3 callers, 6 callees but too short.
	for i := 0; i < 3; i++ {
		e := mkEdge(fmt.Sprintf("C%d", i), "ShortHot", graph.EdgeCalls)
		e.Line = 400 + i
		s.AddEdge(e)
	}
	for i := 0; i < 6; i++ {
		e := mkEdge("ShortHot", fmt.Sprintf("T%d", i), graph.EdgeCalls)
		e.Line = 500 + i
		s.AddEdge(e)
	}

	rows := scan.ExtractCandidates(
		[]graph.EdgeKind{graph.EdgeCalls},
		20, // minLines
		2,  // minCallers
		5,  // minFanOut
		"", // no prefix
	)
	byID := make(map[string]graph.ExtractCandidateRow)
	for _, r := range rows {
		byID[r.NodeID] = r
	}
	r, ok := byID["LongHot"]
	if !ok {
		t.Fatalf("expected LongHot in result, got %v", rows)
	}
	if r.CallerCount != 3 || r.FanOut != 6 || r.LineCount != 60 {
		t.Fatalf("LongHot row mismatch: %+v", r)
	}
	if _, present := byID["LongCold"]; present {
		t.Fatalf("LongCold should have been filtered (caller count < 2)")
	}
	if _, present := byID["ShortHot"]; present {
		t.Fatalf("ShortHot should have been filtered (lines < 20)")
	}

	// Path prefix narrows to only LongHot (it's the one in p/a.go;
	// LongCold and ShortHot also are in p/a.go so use a prefix that
	// doesn't match).
	none := scan.ExtractCandidates(
		[]graph.EdgeKind{graph.EdgeCalls}, 20, 2, 5, "no/such/",
	)
	if len(none) != 0 {
		t.Fatalf("ExtractCandidates with non-matching prefix = %d, want 0", len(none))
	}
	// Empty kinds returns nil.
	if r := scan.ExtractCandidates(nil, 0, 0, 0, ""); r != nil {
		t.Fatalf("ExtractCandidates(nil kinds) = %v, want nil", r)
	}
}

// testFileSymbolNamesByPaths exercises the optional
// graph.FileSymbolNamesByPaths capability.
func testFileSymbolNamesByPaths(t *testing.T, factory Factory) {
	t.Helper()
	s := factory(t)
	scan, ok := s.(graph.FileSymbolNamesByPaths)
	if !ok {
		t.Skip("backend does not implement graph.FileSymbolNamesByPaths")
	}

	s.AddNode(mkNode("Alpha", "Alpha", "a.go", graph.KindFunction))
	s.AddNode(mkNode("Beta", "Beta", "a.go", graph.KindType))
	s.AddNode(mkNode("Gamma", "Gamma", "a.go", graph.KindMethod))
	s.AddNode(mkNode("LowCardField", "LowCardField", "a.go", graph.KindField))
	s.AddNode(mkNode("Delta", "Delta", "b.go", graph.KindFunction))

	rows := scan.FileSymbolNamesByPaths(
		[]string{"a.go", "b.go"},
		[]graph.NodeKind{graph.KindFunction, graph.KindMethod, graph.KindType, graph.KindInterface},
	)
	byFile := make(map[string]map[string]struct{})
	for _, r := range rows {
		seen := byFile[r.FilePath]
		if seen == nil {
			seen = make(map[string]struct{})
			byFile[r.FilePath] = seen
		}
		seen[r.Name] = struct{}{}
	}
	want := map[string]map[string]struct{}{
		"a.go": {"Alpha": {}, "Beta": {}, "Gamma": {}},
		"b.go": {"Delta": {}},
	}
	for file, names := range want {
		got := byFile[file]
		if len(got) != len(names) {
			t.Fatalf("file %q: got %v, want %v", file, got, names)
		}
		for n := range names {
			if _, ok := got[n]; !ok {
				t.Errorf("file %q: missing name %q (got %v)", file, n, got)
			}
		}
	}
	// LowCardField (KindField) must not appear because it's not in
	// the requested kinds.
	if _, ok := byFile["a.go"]["LowCardField"]; ok {
		t.Fatalf("kind filter leaked KindField row")
	}

	// Empty paths returns nil.
	if r := scan.FileSymbolNamesByPaths(nil, nil); r != nil {
		t.Fatalf("FileSymbolNamesByPaths(nil) = %v, want nil", r)
	}
}

// testClassHierarchyTraverser exercises the optional
// graph.ClassHierarchyTraverser capability.
func testClassHierarchyTraverser(t *testing.T, factory Factory) {
	t.Helper()
	s := factory(t)
	scan, ok := s.(graph.ClassHierarchyTraverser)
	if !ok {
		t.Skip("backend does not implement graph.ClassHierarchyTraverser")
	}

	s.AddNode(mkNode("Animal", "Animal", "z.go", graph.KindInterface))
	s.AddNode(mkNode("Dog", "Dog", "z.go", graph.KindType))
	s.AddNode(mkNode("Puppy", "Puppy", "z.go", graph.KindType))
	// Dog implements Animal; Puppy extends Dog.
	e1 := mkEdge("Dog", "Animal", graph.EdgeImplements)
	e1.Line = 1
	s.AddEdge(e1)
	e2 := mkEdge("Puppy", "Dog", graph.EdgeExtends)
	e2.Line = 2
	s.AddEdge(e2)

	upRows := scan.ClassHierarchyTraverse(
		"Puppy", "up",
		[]graph.EdgeKind{graph.EdgeExtends, graph.EdgeImplements, graph.EdgeComposes},
		5,
	)
	if len(upRows) != 2 {
		t.Fatalf("Puppy up: %d rows, want 2 (Dog, Animal). rows=%v", len(upRows), upRows)
	}
	visited := map[string]bool{}
	for _, r := range upRows {
		for _, id := range r.Path {
			visited[id] = true
		}
	}
	if !visited["Dog"] || !visited["Animal"] {
		t.Fatalf("Puppy up: missing Dog or Animal in visited set: %v", visited)
	}
	downRows := scan.ClassHierarchyTraverse(
		"Animal", "down",
		[]graph.EdgeKind{graph.EdgeExtends, graph.EdgeImplements, graph.EdgeComposes},
		5,
	)
	visited = map[string]bool{}
	for _, r := range downRows {
		for _, id := range r.Path {
			visited[id] = true
		}
	}
	if !visited["Dog"] || !visited["Puppy"] {
		t.Fatalf("Animal down: missing Dog or Puppy in visited set: %v", visited)
	}

	// Empty kinds / depth=0 / unknown seed must return nil.
	if r := scan.ClassHierarchyTraverse("Puppy", "up", nil, 5); r != nil {
		t.Fatalf("nil kinds: got %v", r)
	}
	if r := scan.ClassHierarchyTraverse("Puppy", "up",
		[]graph.EdgeKind{graph.EdgeExtends}, 0); r != nil {
		t.Fatalf("depth=0: got %v", r)
	}
	if r := scan.ClassHierarchyTraverse("nope", "up",
		[]graph.EdgeKind{graph.EdgeExtends}, 5); r != nil {
		t.Fatalf("unknown seed: got %v", r)
	}
}

// testFileEditingContext exercises the optional
// graph.FileEditingContext capability.
func testFileEditingContext(t *testing.T, factory Factory) {
	t.Helper()
	s := factory(t)
	scan, ok := s.(graph.FileEditingContext)
	if !ok {
		t.Skip("backend does not implement graph.FileEditingContext")
	}
	// File node + two functions inside it; an importing file with one
	// function that calls into the file; a downstream file with a
	// function the file's function calls.
	s.AddNode(mkNode("a.go", "a.go", "a.go", graph.KindFile))
	s.AddNode(mkNode("a.go::Foo", "Foo", "a.go", graph.KindFunction))
	s.AddNode(mkNode("a.go::Bar", "Bar", "a.go", graph.KindMethod))
	s.AddNode(mkNode("b.go", "b.go", "b.go", graph.KindFile))
	s.AddNode(mkNode("b.go::Caller", "Caller", "b.go", graph.KindFunction))
	s.AddNode(mkNode("c.go::Callee", "Callee", "c.go", graph.KindFunction))

	// Import edge: a.go imports b.go.
	e := mkEdge("a.go", "b.go", graph.EdgeImports)
	e.Line = 1
	s.AddEdge(e)
	// Caller in b.go calls Foo in a.go.
	e = mkEdge("b.go::Caller", "a.go::Foo", graph.EdgeCalls)
	e.Line = 2
	s.AddEdge(e)
	// Foo in a.go calls Callee in c.go.
	e = mkEdge("a.go::Foo", "c.go::Callee", graph.EdgeCalls)
	e.Line = 3
	s.AddEdge(e)

	res := scan.FileEditingContext("a.go", []graph.NodeKind{graph.KindFunction, graph.KindMethod})
	if res == nil {
		t.Fatalf("FileEditingContext returned nil for a.go")
		return
	}
	if res.FileNode == nil || res.FileNode.ID != "a.go" {
		t.Fatalf("FileNode missing or wrong: %+v", res.FileNode)
	}
	defineIDs := map[string]bool{}
	for _, n := range res.Defines {
		defineIDs[n.ID] = true
	}
	if !defineIDs["a.go::Foo"] || !defineIDs["a.go::Bar"] {
		t.Fatalf("defines missing entries: got %v", defineIDs)
	}
	if len(res.Imports) != 1 || res.Imports[0].To != "b.go" {
		t.Fatalf("imports = %v, want one edge a.go→b.go", res.Imports)
	}
	calledBy := map[string]bool{}
	for _, n := range res.CalledBy {
		calledBy[n.ID] = true
	}
	if !calledBy["b.go::Caller"] {
		t.Fatalf("called_by missing Caller: %v", calledBy)
	}
	calls := map[string]bool{}
	for _, n := range res.Calls {
		calls[n.ID] = true
	}
	if !calls["c.go::Callee"] {
		t.Fatalf("calls missing Callee: %v", calls)
	}

	// Empty path returns nil.
	if r := scan.FileEditingContext("", nil); r != nil {
		t.Fatalf("empty path: got %v, want nil", r)
	}
}

// testNodeDegreeByKinds exercises the optional
// graph.NodeDegreeByKinds capability.
func testNodeDegreeByKinds(t *testing.T, factory Factory) {
	t.Helper()
	s := factory(t)
	scan, ok := s.(graph.NodeDegreeByKinds)
	if !ok {
		t.Skip("backend does not implement graph.NodeDegreeByKinds")
	}
	s.AddNode(mkNode("Iso", "Iso", "pkg/iso.go", graph.KindFunction))
	s.AddNode(mkNode("Hub", "Hub", "pkg/hub.go", graph.KindFunction))
	s.AddNode(mkNode("Leaf", "Leaf", "pkg/leaf.go", graph.KindMethod))
	s.AddNode(mkNode("Other", "Other", "pkg/other.go", graph.KindType))
	s.AddNode(mkNode("Caller", "Caller", "pkg/caller.go", graph.KindFunction))
	// 2 incoming + 1 outgoing on Hub.
	for i, from := range []string{"Caller", "Leaf"} {
		e := mkEdge(from, "Hub", graph.EdgeCalls)
		e.Line = i + 1
		s.AddEdge(e)
	}
	e := mkEdge("Hub", "Leaf", graph.EdgeCalls)
	e.Line = 3
	s.AddEdge(e)

	rows := scan.NodeDegreeByKinds(
		[]graph.NodeKind{graph.KindFunction, graph.KindMethod},
		"",
	)
	byID := make(map[string]graph.NodeDegreeRow)
	for _, r := range rows {
		byID[r.NodeID] = r
	}
	if got := byID["Hub"]; got.InCount != 2 || got.OutCount != 1 {
		t.Fatalf("Hub: %+v, want in=2 out=1", got)
	}
	if got, ok := byID["Iso"]; !ok || got.InCount != 0 || got.OutCount != 0 {
		t.Fatalf("Iso: ok=%v got=%+v, want in=0 out=0", ok, got)
	}
	if _, ok := byID["Other"]; ok {
		t.Fatalf("Other (KindType) leaked into kind-filtered result")
	}
	// Empty kinds returns nil.
	if r := scan.NodeDegreeByKinds(nil, ""); r != nil {
		t.Fatalf("NodeDegreeByKinds(nil) = %v, want nil", r)
	}
	// Path prefix narrows.
	rows = scan.NodeDegreeByKinds(
		[]graph.NodeKind{graph.KindFunction, graph.KindMethod},
		"pkg/leaf",
	)
	if len(rows) != 1 || rows[0].NodeID != "Leaf" {
		t.Fatalf("pathPrefix scope mismatch: got %v", rows)
	}
}

// testPathPrefixNativeSeparators pins path-prefix scoping against the
// store path shape (`repo/` + OS-native rest): a '/'-joined prefix must
// match rows whose stored path uses native separators past the repo
// prefix.
func testPathPrefixNativeSeparators(t *testing.T, factory Factory) {
	t.Helper()
	s := factory(t)
	scan, ok := s.(graph.NodeDegreeByKinds)
	if !ok {
		t.Skip("backend does not implement graph.NodeDegreeByKinds")
	}
	deep := mkNode("Deep", "Deep", "r/"+filepath.FromSlash("internal/resolver/deep.go"), graph.KindFunction)
	deep.StartLine, deep.EndLine = 1, 3
	shallow := mkNode("Shallow", "Shallow", "r/top.go", graph.KindFunction)
	shallow.StartLine, shallow.EndLine = 1, 3
	s.AddNode(deep)
	s.AddNode(shallow)

	rows := scan.NodeDegreeByKinds([]graph.NodeKind{graph.KindFunction}, "r/internal/resolver")
	if len(rows) != 1 || rows[0].NodeID != "Deep" {
		t.Fatalf("NodeDegreeByKinds deep prefix = %+v, want [Deep]", rows)
	}
	if cand, ok := s.(graph.ExtractCandidatesScanner); ok {
		got := cand.ExtractCandidates([]graph.EdgeKind{graph.EdgeCalls}, 0, 0, 0, "r/internal/resolver")
		if len(got) != 1 || got[0].NodeID != "Deep" {
			t.Fatalf("ExtractCandidates deep prefix = %+v, want [Deep]", got)
		}
	}
	if ts, ok := s.(graph.ThrowerErrorSurfacer); ok {
		te := mkEdge("Deep", "errs.ErrDeep", graph.EdgeThrows)
		te.FilePath = deep.FilePath
		te.Line = 2
		s.AddEdge(te)
		throwers := ts.ThrowerErrorSurface("r/internal/resolver")
		if len(throwers) != 1 || throwers[0].ThrowerID != "Deep" {
			t.Fatalf("ThrowerErrorSurface deep prefix = %+v, want [Deep]", throwers)
		}
	}
}

// eqShingles reports whether two []uint64 are element-for-element
// equal with order preserved — the exact contract LoadCloneShingles
// must round-trip.
func eqShingles(a, b []uint64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// testCloneShingleSidecar mirrors the FileMtime sidecar conformance:
// set shingle sets for a few node ids under a repo prefix, Load them
// back (asserting exact []uint64 equality with order preserved),
// Delete a subset and re-Load (asserting the gone rows are gone and
// the survivors untouched), verify repo-prefix scoping isolates rows,
// and that an empty/absent load returns an empty (non-nil) map, not an
// error. Backends that don't implement the capability skip — both the
// in-memory Graph and the SQLite Store do implement it.
func testCloneShingleSidecar(t *testing.T, factory Factory) {
	t.Helper()
	s := factory(t)
	w, ok := s.(graph.CloneShingleWriter)
	if !ok {
		t.Skip("backend does not implement graph.CloneShingleWriter")
	}
	r, ok := s.(graph.CloneShingleReader)
	if !ok {
		t.Skip("backend implements CloneShingleWriter but not CloneShingleReader")
	}

	// Empty / absent load returns an empty (non-nil) map, not an error.
	if got, err := r.LoadCloneShingles("repoA"); err != nil {
		t.Fatalf("LoadCloneShingles(empty store): %v", err)
	} else if got == nil {
		t.Fatalf("LoadCloneShingles(empty store) = nil, want empty non-nil map")
	} else if len(got) != 0 {
		t.Fatalf("LoadCloneShingles(empty store) = %v, want empty", got)
	}

	// Empty input is a no-op.
	if err := w.BulkSetCloneShingles("repoA", nil); err != nil {
		t.Fatalf("BulkSetCloneShingles(nil): %v", err)
	}

	// Write three shingle sets under repoA. Order within each set must
	// survive the round-trip, so use non-sorted, repeated-value slices.
	want := map[string][]uint64{
		"a.go::Foo": {9, 1, 9, 4, 2},
		"a.go::Bar": {7},
		"b.go::Baz": {0xFFFFFFFFFFFFFFFF, 0, 42},
	}
	if err := w.BulkSetCloneShingles("repoA", want); err != nil {
		t.Fatalf("BulkSetCloneShingles(repoA): %v", err)
	}

	got, err := r.LoadCloneShingles("repoA")
	if err != nil {
		t.Fatalf("LoadCloneShingles(repoA): %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("LoadCloneShingles(repoA) len = %d, want %d", len(got), len(want))
	}
	for id, ws := range want {
		if !eqShingles(got[id], ws) {
			t.Fatalf("LoadCloneShingles(repoA)[%q] = %v, want %v (order preserved)", id, got[id], ws)
		}
	}

	// Overwrite is idempotent in place: re-setting one id replaces it.
	if err := w.BulkSetCloneShingles("repoA", map[string][]uint64{"a.go::Bar": {7, 8, 9}}); err != nil {
		t.Fatalf("BulkSetCloneShingles(overwrite): %v", err)
	}
	if got, err := r.LoadCloneShingles("repoA"); err != nil {
		t.Fatalf("LoadCloneShingles after overwrite: %v", err)
	} else if !eqShingles(got["a.go::Bar"], []uint64{7, 8, 9}) {
		t.Fatalf("overwrite not in place: a.go::Bar = %v, want [7 8 9]", got["a.go::Bar"])
	}

	// Deep-copy isolation: mutating the input slice after the write must
	// not corrupt stored state, and mutating the returned slice must not
	// corrupt the next read.
	src := []uint64{1, 2, 3}
	if err := w.BulkSetCloneShingles("repoA", map[string][]uint64{"a.go::Foo": src}); err != nil {
		t.Fatalf("BulkSetCloneShingles(isolation): %v", err)
	}
	src[0] = 999
	got2, err := r.LoadCloneShingles("repoA")
	if err != nil {
		t.Fatalf("LoadCloneShingles(isolation): %v", err)
	}
	if !eqShingles(got2["a.go::Foo"], []uint64{1, 2, 3}) {
		t.Fatalf("input mutation leaked into store: a.go::Foo = %v, want [1 2 3]", got2["a.go::Foo"])
	}
	got2["a.go::Foo"][0] = 777
	if got3, _ := r.LoadCloneShingles("repoA"); !eqShingles(got3["a.go::Foo"], []uint64{1, 2, 3}) {
		t.Fatalf("returned-slice mutation leaked into store: a.go::Foo = %v, want [1 2 3]", got3["a.go::Foo"])
	}

	// Delete a subset and re-Load — the deleted rows must be gone; the
	// survivors untouched.
	if err := w.DeleteCloneShingles([]string{"a.go::Bar", "b.go::Baz", "missing::id", ""}); err != nil {
		t.Fatalf("DeleteCloneShingles: %v", err)
	}
	after, err := r.LoadCloneShingles("repoA")
	if err != nil {
		t.Fatalf("LoadCloneShingles after delete: %v", err)
	}
	if _, present := after["a.go::Bar"]; present {
		t.Fatalf("a.go::Bar still present after delete")
	}
	if _, present := after["b.go::Baz"]; present {
		t.Fatalf("b.go::Baz still present after delete")
	}
	if !eqShingles(after["a.go::Foo"], []uint64{1, 2, 3}) {
		t.Fatalf("survivor a.go::Foo corrupted after delete: %v", after["a.go::Foo"])
	}

	// Empty delete is a no-op.
	if err := w.DeleteCloneShingles(nil); err != nil {
		t.Fatalf("DeleteCloneShingles(nil): %v", err)
	}

	// Repo-prefix scoping: a write under repoB must not surface under
	// repoA, and vice versa.
	if err := w.BulkSetCloneShingles("repoB", map[string][]uint64{"c.go::Qux": {5, 6}}); err != nil {
		t.Fatalf("BulkSetCloneShingles(repoB): %v", err)
	}
	aRows, err := r.LoadCloneShingles("repoA")
	if err != nil {
		t.Fatalf("LoadCloneShingles(repoA) after repoB write: %v", err)
	}
	if _, leaked := aRows["c.go::Qux"]; leaked {
		t.Fatalf("repoB row c.go::Qux leaked into repoA scope")
	}
	bRows, err := r.LoadCloneShingles("repoB")
	if err != nil {
		t.Fatalf("LoadCloneShingles(repoB): %v", err)
	}
	if len(bRows) != 1 || !eqShingles(bRows["c.go::Qux"], []uint64{5, 6}) {
		t.Fatalf("LoadCloneShingles(repoB) = %v, want {c.go::Qux:[5 6]}", bRows)
	}
}

// testChurnEnrichmentSidecar mirrors the clone-shingle sidecar
// conformance for the churn enrichment capability (change A): write,
// read-all vs read-by-prefix, idempotent overwrite, per-repo isolation,
// and delete.
func testChurnEnrichmentSidecar(t *testing.T, factory Factory) {
	t.Helper()
	s := factory(t)
	w, ok := s.(graph.ChurnEnrichmentWriter)
	if !ok {
		t.Skip("backend does not implement graph.ChurnEnrichmentWriter")
	}
	r, ok := s.(graph.ChurnEnrichmentReader)
	if !ok {
		t.Skip("backend implements ChurnEnrichmentWriter but not ChurnEnrichmentReader")
	}

	// Empty store + empty input are no-ops.
	if got := r.ChurnRows("repoA"); len(got) != 0 {
		t.Fatalf("ChurnRows(empty store) = %v, want empty", got)
	}
	if err := w.BulkSetChurn("repoA", nil); err != nil {
		t.Fatalf("BulkSetChurn(nil): %v", err)
	}

	rowsA := []graph.ChurnEnrichment{
		{NodeID: "a.go", CommitCount: 5, AgeDays: 30, ChurnRate: 1.5, LastAuthor: "x@y", LastCommitAt: "2026-01-01T00:00:00Z", HeadSHA: "abc", Branch: "main", ComputedAt: "2026-06-01T00:00:00Z"},
		{NodeID: "a.go::Foo", CommitCount: 2, AgeDays: 10, ChurnRate: 0.2, LastAuthor: "z@y", LastCommitAt: "2026-02-01T00:00:00Z"},
	}
	rowsB := []graph.ChurnEnrichment{
		{NodeID: "b.go::Bar", CommitCount: 9, AgeDays: 90, ChurnRate: 0.1, LastAuthor: "q@y"},
	}
	if err := w.BulkSetChurn("repoA", rowsA); err != nil {
		t.Fatalf("BulkSetChurn(repoA): %v", err)
	}
	if err := w.BulkSetChurn("repoB", rowsB); err != nil {
		t.Fatalf("BulkSetChurn(repoB): %v", err)
	}

	// Per-repo read isolation.
	if got := r.ChurnRows("repoA"); len(got) != 2 {
		t.Fatalf("ChurnRows(repoA) len = %d, want 2", len(got))
	}
	if got := r.ChurnRows("repoB"); len(got) != 1 {
		t.Fatalf("ChurnRows(repoB) len = %d, want 1", len(got))
	}
	// Empty prefix returns ALL rows across repos.
	all := r.ChurnRows("")
	if len(all) != 3 {
		t.Fatalf("ChurnRows(\"\") len = %d, want 3 (all repos)", len(all))
	}

	// Field round-trip + repo_prefix stamping.
	byID := map[string]graph.ChurnEnrichment{}
	for _, e := range all {
		byID[e.NodeID] = e
	}
	foo := byID["a.go"]
	if foo.RepoPrefix != "repoA" || foo.CommitCount != 5 || foo.ChurnRate != 1.5 ||
		foo.LastAuthor != "x@y" || foo.LastCommitAt != "2026-01-01T00:00:00Z" ||
		foo.HeadSHA != "abc" || foo.Branch != "main" {
		t.Fatalf("round-trip mismatch for a.go: %+v", foo)
	}

	// Idempotent overwrite (INSERT OR REPLACE on node_id).
	rowsA[0].CommitCount = 99
	if err := w.BulkSetChurn("repoA", rowsA[:1]); err != nil {
		t.Fatalf("BulkSetChurn(overwrite): %v", err)
	}
	for _, e := range r.ChurnRows("repoA") {
		if e.NodeID == "a.go" && e.CommitCount != 99 {
			t.Fatalf("overwrite failed: a.go commit_count = %d, want 99", e.CommitCount)
		}
	}

	// Delete.
	if err := w.DeleteChurn([]string{"a.go", "a.go::Foo"}); err != nil {
		t.Fatalf("DeleteChurn: %v", err)
	}
	if got := r.ChurnRows("repoA"); len(got) != 0 {
		t.Fatalf("ChurnRows(repoA) after delete = %d, want 0", len(got))
	}
	if got := r.ChurnRows("repoB"); len(got) != 1 {
		t.Fatalf("DeleteChurn must not touch repoB: len = %d, want 1", len(got))
	}
}

// testCoverageEnrichmentSidecar mirrors the churn sidecar conformance.
func testCoverageEnrichmentSidecar(t *testing.T, factory Factory) {
	t.Helper()
	s := factory(t)
	w, ok := s.(graph.CoverageEnrichmentWriter)
	if !ok {
		t.Skip("backend does not implement graph.CoverageEnrichmentWriter")
	}
	r, ok := s.(graph.CoverageEnrichmentReader)
	if !ok {
		t.Skip("backend implements CoverageEnrichmentWriter but not Reader")
	}
	if got := r.CoverageRows("repoA"); len(got) != 0 {
		t.Fatalf("CoverageRows(empty) = %v, want empty", got)
	}
	if err := w.BulkSetCoverage("repoA", nil); err != nil {
		t.Fatalf("BulkSetCoverage(nil): %v", err)
	}
	rowsA := []graph.CoverageEnrichment{
		{NodeID: "a.go::Foo", CoveragePct: 87.5, NumStmt: 8, Hit: 7},
		{NodeID: "a.go::Bar", CoveragePct: 0, NumStmt: 3, Hit: 0},
	}
	rowsB := []graph.CoverageEnrichment{{NodeID: "b.go::Baz", CoveragePct: 100, NumStmt: 1, Hit: 1}}
	if err := w.BulkSetCoverage("repoA", rowsA); err != nil {
		t.Fatalf("BulkSetCoverage(repoA): %v", err)
	}
	if err := w.BulkSetCoverage("repoB", rowsB); err != nil {
		t.Fatalf("BulkSetCoverage(repoB): %v", err)
	}
	if got := r.CoverageRows("repoA"); len(got) != 2 {
		t.Fatalf("CoverageRows(repoA) = %d, want 2", len(got))
	}
	if got := r.CoverageRows(""); len(got) != 3 {
		t.Fatalf("CoverageRows(all) = %d, want 3", len(got))
	}
	byID := map[string]graph.CoverageEnrichment{}
	for _, e := range r.CoverageRows("") {
		byID[e.NodeID] = e
	}
	foo := byID["a.go::Foo"]
	if foo.RepoPrefix != "repoA" || foo.CoveragePct != 87.5 || foo.NumStmt != 8 || foo.Hit != 7 {
		t.Fatalf("round-trip mismatch: %+v", foo)
	}
	rowsA[0].CoveragePct = 12.0
	if err := w.BulkSetCoverage("repoA", rowsA[:1]); err != nil {
		t.Fatalf("overwrite: %v", err)
	}
	for _, e := range r.CoverageRows("repoA") {
		if e.NodeID == "a.go::Foo" && e.CoveragePct != 12.0 {
			t.Fatalf("overwrite failed: %v", e.CoveragePct)
		}
	}
	if err := w.DeleteCoverage([]string{"a.go::Foo", "a.go::Bar"}); err != nil {
		t.Fatalf("DeleteCoverage: %v", err)
	}
	if got := r.CoverageRows("repoA"); len(got) != 0 {
		t.Fatalf("after delete repoA = %d, want 0", len(got))
	}
	if got := r.CoverageRows("repoB"); len(got) != 1 {
		t.Fatalf("delete must not touch repoB: %d", len(got))
	}
}

// testReleaseEnrichmentSidecar mirrors the churn/coverage sidecar conformance.
func testReleaseEnrichmentSidecar(t *testing.T, factory Factory) {
	t.Helper()
	s := factory(t)
	w, ok := s.(graph.ReleaseEnrichmentWriter)
	if !ok {
		t.Skip("backend does not implement graph.ReleaseEnrichmentWriter")
	}
	r := s.(graph.ReleaseEnrichmentReader)
	if err := w.BulkSetReleases("repoA", nil); err != nil {
		t.Fatalf("BulkSetReleases(nil): %v", err)
	}
	if err := w.BulkSetReleases("repoA", []graph.ReleaseEnrichment{
		{NodeID: "a.go", AddedIn: "v1.0.0"},
		{NodeID: "b.go", AddedIn: "v1.2.0"},
	}); err != nil {
		t.Fatalf("BulkSetReleases(repoA): %v", err)
	}
	if err := w.BulkSetReleases("repoB", []graph.ReleaseEnrichment{{NodeID: "c.go", AddedIn: "v2.0.0"}}); err != nil {
		t.Fatalf("BulkSetReleases(repoB): %v", err)
	}
	if got := r.ReleaseRows("repoA"); len(got) != 2 {
		t.Fatalf("ReleaseRows(repoA) = %d, want 2", len(got))
	}
	if got := r.ReleaseRows(""); len(got) != 3 {
		t.Fatalf("ReleaseRows(all) = %d, want 3", len(got))
	}
	byID := map[string]graph.ReleaseEnrichment{}
	for _, e := range r.ReleaseRows("") {
		byID[e.NodeID] = e
	}
	if byID["a.go"].AddedIn != "v1.0.0" || byID["a.go"].RepoPrefix != "repoA" {
		t.Fatalf("round-trip mismatch: %+v", byID["a.go"])
	}
	if err := w.DeleteReleases([]string{"a.go", "b.go"}); err != nil {
		t.Fatalf("DeleteReleases: %v", err)
	}
	if got := r.ReleaseRows("repoA"); len(got) != 0 {
		t.Fatalf("after delete repoA = %d, want 0", len(got))
	}
	if got := r.ReleaseRows("repoB"); len(got) != 1 {
		t.Fatalf("delete must not touch repoB: %d", len(got))
	}
}

// testBlameEnrichmentSidecar mirrors the other enrichment sidecars.
func testBlameEnrichmentSidecar(t *testing.T, factory Factory) {
	t.Helper()
	s := factory(t)
	w, ok := s.(graph.BlameEnrichmentWriter)
	if !ok {
		t.Skip("backend does not implement graph.BlameEnrichmentWriter")
	}
	r := s.(graph.BlameEnrichmentReader)
	if err := w.BulkSetBlame("repoA", nil); err != nil {
		t.Fatalf("BulkSetBlame(nil): %v", err)
	}
	if err := w.BulkSetBlame("repoA", []graph.BlameEnrichment{
		{NodeID: "a.go::Foo", Commit: "abc", Email: "x@y", Timestamp: 1700000000},
		{NodeID: "a.go::Bar", Commit: "def", Email: "z@y", Timestamp: 1700001000},
	}); err != nil {
		t.Fatalf("BulkSetBlame(repoA): %v", err)
	}
	if err := w.BulkSetBlame("repoB", []graph.BlameEnrichment{{NodeID: "b.go::Baz", Commit: "ghi", Email: "q@y", Timestamp: 1700002000}}); err != nil {
		t.Fatalf("BulkSetBlame(repoB): %v", err)
	}
	if got := r.BlameRows("repoA"); len(got) != 2 {
		t.Fatalf("BlameRows(repoA) = %d, want 2", len(got))
	}
	if got := r.BlameRows(""); len(got) != 3 {
		t.Fatalf("BlameRows(all) = %d, want 3", len(got))
	}
	byID := map[string]graph.BlameEnrichment{}
	for _, e := range r.BlameRows("") {
		byID[e.NodeID] = e
	}
	foo := byID["a.go::Foo"]
	if foo.RepoPrefix != "repoA" || foo.Commit != "abc" || foo.Email != "x@y" || foo.Timestamp != 1700000000 {
		t.Fatalf("round-trip mismatch: %+v", foo)
	}
	if err := w.DeleteBlame([]string{"a.go::Foo", "a.go::Bar"}); err != nil {
		t.Fatalf("DeleteBlame: %v", err)
	}
	if got := r.BlameRows("repoA"); len(got) != 0 {
		t.Fatalf("after delete repoA = %d, want 0", len(got))
	}
	if got := r.BlameRows("repoB"); len(got) != 1 {
		t.Fatalf("delete must not touch repoB: %d", len(got))
	}
}

// testContractBridgeRoundTrip verifies the contract-bridge subgraph
// survives the backend: a KindContractBridge node with its grouped
// Meta (incl. a []string repo spread), EdgeBridges edges carrying
// Meta["side"], kind-filtered retrieval, and the EvictFile path the
// bridge re-materialisation pass uses to drop the prior generation.
func testContractBridgeRoundTrip(t *testing.T, factory Factory) {
	t.Helper()
	s := factory(t)

	const bridgeFile = "contracts://bridges"
	s.AddNode(&graph.Node{
		ID: "http::GET::/v1/users", Kind: graph.KindContract,
		Name: "http::GET::/v1/users", FilePath: "svc-a/routes.go",
		Language: "contract", RepoPrefix: "svc-a",
	})
	s.AddNode(&graph.Node{
		ID: "grpc::Users::GetUser", Kind: graph.KindContract,
		Name: "grpc::Users::GetUser", FilePath: "svc-b/client.go",
		Language: "contract", RepoPrefix: "svc-b",
	})
	s.AddNode(&graph.Node{
		ID: "bridge::http::GET::/v1/users", Kind: graph.KindContractBridge,
		Name: "GET /v1/users", FilePath: bridgeFile,
		Language: "contract", RepoPrefix: "svc-a",
		Meta: map[string]any{
			"contract_type":  "http",
			"canonical_key":  "GET /v1/users",
			"repos":          []string{"svc-a", "svc-b"},
			"provider_count": 1,
			"consumer_count": 2,
			"cross_repo":     true,
		},
	})
	s.AddEdge(&graph.Edge{
		From: "bridge::http::GET::/v1/users", To: "http::GET::/v1/users",
		Kind: graph.EdgeBridges, FilePath: bridgeFile,
		Meta: map[string]any{"side": "both"},
	})
	s.AddEdge(&graph.Edge{
		From: "bridge::http::GET::/v1/users", To: "grpc::Users::GetUser",
		Kind: graph.EdgeBridges, FilePath: bridgeFile,
		Meta: map[string]any{"side": "consumer"},
	})

	got := s.GetNode("bridge::http::GET::/v1/users")
	if got == nil {
		t.Fatalf("bridge node did not round-trip")
		return
	}
	if got.Kind != graph.KindContractBridge {
		t.Fatalf("bridge kind = %q, want %q", got.Kind, graph.KindContractBridge)
	}
	if got.Meta == nil {
		t.Fatalf("bridge Meta not preserved")
	}
	if got.Meta["canonical_key"] != "GET /v1/users" {
		t.Fatalf("Meta[canonical_key] = %v", got.Meta["canonical_key"])
	}
	switch repos := got.Meta["repos"].(type) {
	case []string:
		if len(repos) != 2 || repos[0] != "svc-a" || repos[1] != "svc-b" {
			t.Fatalf("Meta[repos] = %v", repos)
		}
	case []any:
		if len(repos) != 2 || repos[0] != "svc-a" || repos[1] != "svc-b" {
			t.Fatalf("Meta[repos] = %v", repos)
		}
	default:
		t.Fatalf("Meta[repos] has unexpected type %T (%v)", got.Meta["repos"], got.Meta["repos"])
	}

	var byKind []*graph.Node
	for n := range s.NodesByKind(graph.KindContractBridge) {
		byKind = append(byKind, n)
	}
	if len(byKind) != 1 || byKind[0].ID != "bridge::http::GET::/v1/users" {
		t.Fatalf("NodesByKind(contract_bridge) = %v", byKind)
	}

	out := s.GetOutEdges("bridge::http::GET::/v1/users")
	sides := map[string]string{}
	for _, e := range out {
		if e.Kind != graph.EdgeBridges {
			t.Fatalf("unexpected out-edge kind %q", e.Kind)
		}
		side, _ := e.Meta["side"].(string)
		sides[e.To] = side
	}
	if sides["http::GET::/v1/users"] != "both" || sides["grpc::Users::GetUser"] != "consumer" {
		t.Fatalf("EdgeBridges side meta did not round-trip: %v", sides)
	}

	// The reverse direction the impact query walks.
	var inKinds []graph.EdgeKind
	for _, e := range s.GetInEdges("http::GET::/v1/users") {
		inKinds = append(inKinds, e.Kind)
	}
	if len(inKinds) != 1 || inKinds[0] != graph.EdgeBridges {
		t.Fatalf("GetInEdges(contract) = %v, want one bridges edge", inKinds)
	}

	// Evicting the synthetic bridge file must drop the bridge node and
	// its edges while leaving the contract nodes untouched — this is
	// the idempotency mechanism the materialisation pass relies on.
	nodesRemoved, edgesRemoved := s.EvictFile(bridgeFile)
	if nodesRemoved != 1 {
		t.Fatalf("EvictFile removed %d nodes, want 1", nodesRemoved)
	}
	if edgesRemoved != 2 {
		t.Fatalf("EvictFile removed %d edges, want 2", edgesRemoved)
	}
	if s.GetNode("bridge::http::GET::/v1/users") != nil {
		t.Fatalf("bridge node survived EvictFile")
	}
	if s.GetNode("http::GET::/v1/users") == nil || s.GetNode("grpc::Users::GetUser") == nil {
		t.Fatalf("contract nodes must survive bridge eviction")
	}
	if got := s.GetInEdges("http::GET::/v1/users"); len(got) != 0 {
		t.Fatalf("stale bridge in-edges survived eviction: %v", got)
	}
}

// -- read semantics ----------------------------------------------------
//
// The two Store implementations answer a read differently, and until these
// two subtests existed nothing in the tree said so out loud. The staging
// graph is a bag of pointers, so a read hands back the store's own *Edge and
// an in-place field assignment is already durable. A disk backend decodes a
// row into a fresh struct, so the same assignment writes to garbage the
// moment the caller drops it.
//
// The reconciling mechanism is the optional persistence capability set
// (graph.EdgePersister / EdgeMetaBatchPersister / EdgeTerminalStampPersister).
// Code that must make an attribute change stick calls whichever of them the
// store advertises and is then correct under both semantics — which is why
// enrichment passes are written as `if w, ok := g.(graph.EdgePersister); ok`
// rather than mutating and hoping.

// testEdgePersistRoundTrip proves the escape hatch actually works: after a
// read → mutate → persist cycle, a *fresh* read observes the new attributes.
// Backends that alias their reads implement none of these capabilities (they
// have no need for them) and skip.
func testEdgePersistRoundTrip(t *testing.T, factory Factory) {
	t.Helper()
	s := factory(t)

	_, persistOne := s.(graph.EdgePersister)
	_, persistBatch := s.(graph.EdgeMetaBatchPersister)
	_, persistStamps := s.(graph.EdgeTerminalStampPersister)
	if !persistOne && !persistBatch && !persistStamps {
		t.Skip("backend implements no edge-persistence capability")
	}

	s.AddNode(mkNode("a", "A", "x.go", graph.KindFunction))
	s.AddNode(mkNode("b", "B", "x.go", graph.KindFunction))
	base := mkEdge("a", "b", graph.EdgeCalls)
	base.Confidence = 0.2
	base.ConfidenceLabel = "heuristic"
	base.Origin = graph.OriginTextMatched
	base.Tier = "syntax"
	base.Meta = map[string]any{"opaque": "before"}
	s.AddEdge(base)

	// readBack re-reads the single a→b edge through the public adjacency
	// API, so an assertion can never accidentally observe the same struct
	// the test just mutated.
	readBack := func() *graph.Edge {
		t.Helper()
		out := s.GetOutEdges("a")
		if len(out) != 1 {
			t.Fatalf("GetOutEdges(a) = %d edges, want exactly 1", len(out))
		}
		return out[0]
	}

	if w, ok := s.(graph.EdgePersister); ok {
		e := readBack()
		e.Confidence = 0.91
		e.ConfidenceLabel = "confirmed"
		e.Origin = graph.OriginLSPResolved
		e.Tier = "compiler"
		e.Meta = map[string]any{"opaque": "after-single"}
		w.PersistEdgeAttributes(e)

		got := readBack()
		if got.Confidence != 0.91 || got.ConfidenceLabel != "confirmed" {
			t.Fatalf("PersistEdgeAttributes lost confidence: %v/%q", got.Confidence, got.ConfidenceLabel)
		}
		if got.Origin != graph.OriginLSPResolved || got.Tier != "compiler" {
			t.Fatalf("PersistEdgeAttributes lost provenance: %q/%q", got.Origin, got.Tier)
		}
		if got.Meta["opaque"] != "after-single" {
			t.Fatalf("PersistEdgeAttributes lost meta: %v", got.Meta)
		}
	}

	if w, ok := s.(graph.EdgeMetaBatchPersister); ok {
		e := readBack()
		e.Confidence = 0.55
		e.Origin = graph.OriginASTInferred
		e.Meta = map[string]any{"opaque": "after-batch"}
		w.PersistEdgeAttributesBatch([]*graph.Edge{e})

		got := readBack()
		if got.Confidence != 0.55 || got.Origin != graph.OriginASTInferred {
			t.Fatalf("PersistEdgeAttributesBatch lost attributes: %v/%q", got.Confidence, got.Origin)
		}
		if got.Meta["opaque"] != "after-batch" {
			t.Fatalf("PersistEdgeAttributesBatch lost meta: %v", got.Meta)
		}
		// An empty batch must be a no-op rather than a panic or a wipe.
		w.PersistEdgeAttributesBatch(nil)
		if got := readBack(); got.Confidence != 0.55 {
			t.Fatalf("empty PersistEdgeAttributesBatch disturbed the edge: %+v", got)
		}
	}

	if w, ok := s.(graph.EdgeTerminalStampPersister); ok {
		// The narrow capability: only the two terminal keys may move. Set a
		// sentinel on every other attribute first so a wide write is caught.
		before := readBack()
		before.Confidence = 0.33
		before.Origin = "stamp-sentinel"
		before.Meta = map[string]any{"opaque": "stamp-sentinel"}
		if p, ok := s.(graph.EdgePersister); ok {
			p.PersistEdgeAttributes(before)
		}

		e := readBack()
		e.Meta = map[string]any{
			"opaque":                            "must-not-be-written",
			graph.EdgeMetaResolveTerminal:       true,
			graph.EdgeMetaResolveTerminalReason: "external",
		}
		e.Confidence = 0.01
		e.Origin = "must-not-be-written"
		w.PersistEdgeTerminalStamps([]*graph.Edge{e})

		got := readBack()
		if got.Meta[graph.EdgeMetaResolveTerminal] != true {
			t.Fatalf("terminal stamp did not land: %v", got.Meta)
		}
		if got.Meta[graph.EdgeMetaResolveTerminalReason] != "external" {
			t.Fatalf("terminal reason did not land: %v", got.Meta)
		}
		if got.Confidence != 0.33 || got.Origin != "stamp-sentinel" {
			t.Fatalf("terminal stamp widened into other attributes: %v/%q", got.Confidence, got.Origin)
		}
		if got.Meta["opaque"] != "stamp-sentinel" {
			t.Fatalf("terminal stamp overwrote unrelated meta: %v", got.Meta)
		}
		// Nil edges and unknown identities are no-ops.
		w.PersistEdgeTerminalStamps(nil)
		w.PersistEdgeTerminalStamps([]*graph.Edge{nil})
	}
}

// testReadAliasing pins the declared semantics on both the edge and the node
// axis. The assertion is deliberately symmetric: a backend that claims
// detached reads must DROP an unpersisted mutation, and one that claims
// aliasing must KEEP it. Either way the suite records a fact rather than a
// preference, so a backend cannot quietly change sides.
//
// There is no NodePersister — a node attribute change is written by handing
// the node back to AddNode — so nodes are covered on this axis only.
func testReadAliasing(t *testing.T, factory Factory, semantics Semantics) {
	t.Helper()
	aliases := semantics.Reads == ReadsAliasStore

	t.Run("Edge", func(t *testing.T) {
		s := factory(t)
		s.AddNode(mkNode("a", "A", "x.go", graph.KindFunction))
		s.AddNode(mkNode("b", "B", "x.go", graph.KindFunction))
		e := mkEdge("a", "b", graph.EdgeCalls)
		e.Confidence = 0.25
		s.AddEdge(e)

		out := s.GetOutEdges("a")
		if len(out) != 1 {
			t.Fatalf("GetOutEdges(a) = %d edges, want 1", len(out))
		}
		// No persistence call — this is exactly the mistake the split
		// punishes.
		out[0].Confidence = 0.99

		again := s.GetOutEdges("a")
		if len(again) != 1 {
			t.Fatalf("GetOutEdges(a) = %d edges, want 1", len(again))
		}
		if aliases && again[0].Confidence != 0.99 {
			t.Fatalf("backend declares aliasing reads but the mutation was dropped: %v", again[0].Confidence)
		}
		if !aliases && again[0].Confidence != 0.25 {
			t.Fatalf("backend declares detached reads but an unpersisted mutation landed: %v", again[0].Confidence)
		}
	})

	t.Run("Node", func(t *testing.T) {
		s := factory(t)
		n := mkNode("a.go::Foo", "Foo", "a.go", graph.KindFunction)
		n.EndLine = 10
		s.AddNode(n)

		got := s.GetNode("a.go::Foo")
		if got == nil {
			t.Fatal("GetNode returned nil for inserted node")
			return
		}
		// EndLine rather than Name/FilePath: the aliasing backend indexes
		// nodes by name and path, so mutating one of those behind its back
		// would corrupt the store instead of demonstrating anything.
		got.EndLine = 999

		again := s.GetNode("a.go::Foo")
		if again == nil {
			t.Fatal("GetNode returned nil on re-read")
			return
		}
		if aliases && again.EndLine != 999 {
			t.Fatalf("backend declares aliasing reads but the node mutation was dropped: %d", again.EndLine)
		}
		if !aliases && again.EndLine != 10 {
			t.Fatalf("backend declares detached reads but an unpersisted node mutation landed: %d", again.EndLine)
		}
	})
}

// -- reentrancy, ordering and payload semantics -------------------------

// testIteratorReentrancy proves a predicate iterator's yield body may call
// back into the store. This is not hypothetical: the resolver reads adjacency
// and node identities while walking EdgesWithUnresolvedTarget, so an
// implementation that held a connection (or a lock) across yield would
// deadlock the daemon rather than fail a test.
//
// A backend that streams rows must therefore release its read resource before
// calling yield, or materialise the page first. Note that a deadlock here
// HANGS the package run; a store built on a single connection (SQLite opened
// at ":memory:") is exactly that shape, which is why the disk factory stays
// file-backed.
func testIteratorReentrancy(t *testing.T, factory Factory) {
	t.Helper()
	s := factory(t)
	for i := range 4 {
		s.AddNode(mkNode(fmt.Sprintf("n%d", i), fmt.Sprintf("N%d", i), "x.go", graph.KindFunction))
	}
	for i := range 3 {
		e := mkEdge("n0", fmt.Sprintf("n%d", i+1), graph.EdgeCalls)
		e.Line = i + 1
		s.AddEdge(e)
	}
	unresolved := mkEdge("n0", "repo::"+graph.UnresolvedMarker+"Missing", graph.EdgeCalls)
	unresolved.Line = 90
	s.AddEdge(unresolved)

	// Point lookup, adjacency read and a nested iterator, all from inside a
	// yield body.
	seen := 0
	for e := range s.EdgesByKind(graph.EdgeCalls) {
		seen++
		if s.GetNode(e.From) == nil {
			t.Fatalf("re-entrant GetNode(%q) returned nil", e.From)
		}
		if len(s.GetOutEdges(e.From)) == 0 {
			t.Fatalf("re-entrant GetOutEdges(%q) returned nothing", e.From)
		}
		nested := 0
		for range s.NodesByKind(graph.KindFunction) {
			nested++
			if s.NodeCount() != 4 {
				t.Fatalf("doubly re-entrant NodeCount = %d, want 4", s.NodeCount())
			}
		}
		if nested != 4 {
			t.Fatalf("nested NodesByKind yielded %d nodes, want 4", nested)
		}
	}
	if seen != 4 {
		t.Fatalf("EdgesByKind(calls) yielded %d edges, want 4", seen)
	}

	for e := range s.EdgesWithUnresolvedTarget() {
		if got := s.GetOutEdges(e.From); len(got) != 4 {
			t.Fatalf("re-entrant read inside the unresolved scan saw %d out-edges, want 4", len(got))
		}
	}

	// Early stop must release whatever the iterator holds, or the very next
	// read blocks.
	for range s.EdgesByKind(graph.EdgeCalls) {
		break
	}
	if s.EdgeCount() != 4 {
		t.Fatalf("EdgeCount after an abandoned iterator = %d, want 4", s.EdgeCount())
	}
}

// testConcurrentReadsDuringWrite runs more concurrent readers than the SQLite
// backend keeps read connections for (its pool tops out at four) while a
// writer mutates, so readers must queue on the pool rather than fail, and the
// writer must not be starved behind them.
func testConcurrentReadsDuringWrite(t *testing.T, factory Factory) {
	t.Helper()
	s := factory(t)
	const (
		readers = 8
		writes  = 40
		reads   = 20
	)
	s.AddNode(mkNode("seed", "Seed", "x.go", graph.KindFunction))

	var wg sync.WaitGroup
	done := make(chan struct{})
	for r := range readers {
		wg.Add(1)
		go func(r int) {
			defer wg.Done()
			for range reads {
				select {
				case <-done:
					return
				default:
				}
				// A read concurrent with a write may legitimately observe
				// either side of it; only crashes, deadlocks and torn reads
				// are forbidden.
				if n := s.GetNode("seed"); n != nil && n.ID != "seed" {
					t.Errorf("torn node read: %+v", n)
					return
				}
				_ = s.GetOutEdges("seed")
				_ = s.NodeCount()
				for range s.NodesByKind(graph.KindFunction) {
					break
				}
			}
		}(r)
	}

	for i := range writes {
		id := fmt.Sprintf("w%d", i)
		s.AddNode(mkNode(id, fmt.Sprintf("W%d", i), "y.go", graph.KindFunction))
		e := mkEdge("seed", id, graph.EdgeCalls)
		e.Line = i + 1
		s.AddEdge(e)
	}
	close(done)
	wg.Wait()

	if got, want := s.NodeCount(), writes+1; got != want {
		t.Fatalf("NodeCount after concurrent load = %d, want %d", got, want)
	}
	if got := len(s.GetOutEdges("seed")); got != writes {
		t.Fatalf("GetOutEdges(seed) after concurrent load = %d, want %d", got, writes)
	}
}

// testReadOrdering pins how much of a read's ORDER callers may rely on. Three
// separate claims, and they are deliberately not the same claim:
//
//  1. GetOutEdges returns a node's out-edges in the order the backend declares
//     through Semantics.AdjacencyLineOrdered — ascending source line across
//     kinds when the backend sorts, the order the edges were added when it
//     does not. The distinction is load-bearing, which is why the edges here
//     are seeded OUT of line order: helpers that take the FIRST edge matching
//     a predicate — the semantic enricher's matching-edge lookup, for one —
//     pick a different edge on the two backends when a caller has several
//     edges to the same target from different lines, which is what any loop
//     body or repeated call produces. A caller that needs the earliest site
//     must sort rather than trust the read.
//
//  2. GetInEdges is stable per backend and, on a line-ordered backend,
//     grouped by edge kind. Ordering WITHIN a kind is insertion order, not
//     line order: the reverse-adjacency index carries the row id, not the
//     line, and buying a line order here would cost a sort on every in-edge
//     read. Callers that need a total order over in-edges must sort.
//
//  3. AllNodes / AllEdges are NOT ordered at all. The staging graph walks
//     hash-map shards, so it hands back a fresh permutation per call by
//     construction. Only set equality is portable here.
func testReadOrdering(t *testing.T, factory Factory, semantics Semantics) {
	t.Helper()
	s := factory(t)
	s.AddNode(mkNode("a", "A", "x.go", graph.KindFunction))
	s.AddNode(mkNode("b", "B", "x.go", graph.KindFunction))
	s.AddNode(mkNode("c", "C", "x.go", graph.KindFunction))

	// Several sites against the same target with an interleaved kind, added in
	// an order that is NOT the line order — the shape a re-extraction or a
	// deferred enrichment pass produces. Seeding in line order would let a
	// backend that simply replays its append slice pass the ordered assertion
	// without ordering anything. The kind interleave separates "ordered by
	// line" from "grouped by kind".
	seed := []struct {
		kind graph.EdgeKind
		line int
	}{
		{graph.EdgeCalls, 30},
		{graph.EdgeReferences, 20},
		{graph.EdgeCalls, 10},
	}
	for _, spec := range seed {
		e := mkEdge("a", "b", spec.kind)
		e.Line = spec.line
		s.AddEdge(e)
	}
	other := mkEdge("c", "b", graph.EdgeCalls)
	other.Line = 5
	s.AddEdge(other)

	// render describes an edge sequence exactly enough to catch a reordering.
	render := func(edges []*graph.Edge, keep func(*graph.Edge) bool) []string {
		out := make([]string, 0, len(edges))
		for _, e := range edges {
			if keep != nil && !keep(e) {
				continue
			}
			out = append(out, fmt.Sprintf("%s|%s|%d", e.To, e.Kind, e.Line))
		}
		return out
	}

	out := s.GetOutEdges("a")
	want := []string{"b|calls|30", "b|references|20", "b|calls|10"} // append order
	if semantics.AdjacencyLineOrdered {
		want = []string{"b|calls|10", "b|references|20", "b|calls|30"}
	}
	if got := render(out, nil); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("GetOutEdges(a) = %v, want %v (AdjacencyLineOrdered=%t)",
			got, want, semantics.AdjacencyLineOrdered)
	}
	if got := render(s.GetOutEdges("a"), nil); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("GetOutEdges(a) is not stable across calls: %v then %v", want, got)
	}
	// The consequence: on a line-ordered backend the first match is the first
	// site; on an append-ordered one it is whichever site was written first.
	wantFirstCall := 30
	if semantics.AdjacencyLineOrdered {
		wantFirstCall = 10
	}
	for _, e := range out {
		if e.To == "b" && e.Kind == graph.EdgeCalls {
			if e.Line != wantFirstCall {
				t.Fatalf("first (b, calls) out-edge is at line %d, want %d (AdjacencyLineOrdered=%t)",
					e.Line, wantFirstCall, semantics.AdjacencyLineOrdered)
			}
			break
		}
	}

	in := s.GetInEdges("b")
	if len(in) != 4 {
		t.Fatalf("GetInEdges(b) = %d edges, want 4", len(in))
	}
	if got := render(s.GetInEdges("b"), nil); fmt.Sprint(got) != fmt.Sprint(render(in, nil)) {
		t.Fatalf("GetInEdges(b) is not stable across calls: %v then %v", render(in, nil), got)
	}
	if semantics.AdjacencyLineOrdered {
		// Grouped by kind, insertion order within a kind — deliberately NOT
		// line order (line 10 stays behind line 30 because it was written
		// second).
		wantIn := []string{"b|calls|30", "b|calls|10", "b|calls|5", "b|references|20"}
		if got := render(in, nil); fmt.Sprint(got) != fmt.Sprint(wantIn) {
			t.Fatalf("GetInEdges(b) = %v, want %v (grouped by kind, insertion order within a kind)", got, wantIn)
		}
	} else {
		wantIn := []string{"b|calls|30", "b|references|20", "b|calls|10", "b|calls|5"}
		if got := render(in, nil); fmt.Sprint(got) != fmt.Sprint(wantIn) {
			t.Fatalf("GetInEdges(b) = %v, want %v (append order)", got, wantIn)
		}
	}

	// Whole-graph reads: same contents, order unspecified.
	if got, wantIDs := sortNodeIDs(s.AllNodes()), []string{"a", "b", "c"}; fmt.Sprint(got) != fmt.Sprint(wantIDs) {
		t.Fatalf("AllNodes = %v, want %v", got, wantIDs)
	}
	if got, second := sortEdgeKeys(s.AllEdges()), sortEdgeKeys(s.AllEdges()); fmt.Sprint(got) != fmt.Sprint(second) {
		t.Fatalf("AllEdges contents changed between calls: %v then %v", got, second)
	}
}

// testAttributeOnlyReindex pins that ReindexEdges persists an entry's payload
// even when its identity did not move.
//
// The resolver relies on it: when a deferred language-server lookup is still
// pending, the pass clears the edge's durable terminal stamp and hands the
// edge back with an unchanged target. Skipping such an entry as "nothing to
// do" leaves the stale stamp on disk, and the edge stays permanently excluded
// from later resolution.
func testAttributeOnlyReindex(t *testing.T, factory Factory) {
	t.Helper()
	s := factory(t)
	s.AddNode(mkNode("a", "A", "x.go", graph.KindFunction))
	s.AddNode(mkNode("b", "B", "x.go", graph.KindFunction))

	seed := mkEdge("a", "b", graph.EdgeCalls)
	seed.Line = 7
	seed.Confidence = 0.2
	seed.ConfidenceLabel = "heuristic"
	seed.Origin = graph.OriginTextMatched
	seed.Tier = "syntax"
	seed.Meta = map[string]any{
		"opaque":                            "before",
		graph.EdgeMetaResolveTerminal:       true,
		graph.EdgeMetaResolveTerminalReason: "no_candidate",
	}
	s.AddEdge(seed)

	read := func() *graph.Edge {
		t.Helper()
		out := s.GetOutEdges("a")
		if len(out) != 1 {
			t.Fatalf("GetOutEdges(a) = %d edges, want exactly 1", len(out))
		}
		return out[0]
	}

	e := read()
	e.Confidence = 0.87
	e.ConfidenceLabel = "confirmed"
	e.Origin = graph.OriginLSPResolved
	e.Tier = "compiler"
	e.Meta = map[string]any{"opaque": "after"}
	s.ReindexEdges([]graph.EdgeReindex{{Edge: e, OldTo: e.To}})

	got := read()
	if got.To != "b" || got.Line != 7 || got.Kind != graph.EdgeCalls {
		t.Fatalf("attribute-only reindex moved the identity: %+v", got)
	}
	if got.Confidence != 0.87 || got.ConfidenceLabel != "confirmed" {
		t.Fatalf("attribute-only reindex dropped confidence: %v/%q", got.Confidence, got.ConfidenceLabel)
	}
	if got.Origin != graph.OriginLSPResolved || got.Tier != "compiler" {
		t.Fatalf("attribute-only reindex dropped provenance: %q/%q", got.Origin, got.Tier)
	}
	if got.Meta["opaque"] != "after" {
		t.Fatalf("attribute-only reindex dropped meta: %v", got.Meta)
	}
	if _, still := got.Meta[graph.EdgeMetaResolveTerminal]; still {
		t.Fatalf("attribute-only reindex left the stale terminal stamp: %v", got.Meta)
	}

	// Replaying the same entry is a no-op, not a duplicate or a wipe.
	s.ReindexEdges([]graph.EdgeReindex{{Edge: got, OldTo: got.To}})
	replayed := read()
	if replayed.Confidence != 0.87 || replayed.Origin != graph.OriginLSPResolved {
		t.Fatalf("idempotent replay disturbed the edge: %+v", replayed)
	}
	if s.EdgeCount() != 1 {
		t.Fatalf("EdgeCount after replay = %d, want 1", s.EdgeCount())
	}
}

// testMutationReceipts pins the optional receipt capability both backends
// advertise. A receipt is how the daemon avoids a whole-graph resolve after a
// small edit, so an inflated one is merely slow but a receipt that misses a
// mutation leaves the graph wrong: the contract is that it either describes
// the window exactly or admits it is incomplete.
func testMutationReceipts(t *testing.T, factory Factory) {
	t.Helper()
	s := factory(t)
	rs, ok := s.(graph.MutationReceiptStore)
	if !ok {
		t.Skip("backend does not implement graph.MutationReceiptStore")
	}

	caller := mkRepoNode("repo/src/a.go::Caller", "Caller", "src/a.go", "repo", graph.KindFunction)
	callee := mkRepoNode("repo/src/b.go::Load", "Load", "src/b.go", "repo", graph.KindFunction)
	pending := &graph.Edge{
		From: caller.ID, To: "repo::" + graph.UnresolvedMarker + "Load",
		Kind: graph.EdgeCalls, FilePath: "src/a.go", Line: 3,
	}

	token := rs.BeginMutationReceipt()
	s.AddBatch([]*graph.Node{caller, callee}, []*graph.Edge{pending})
	receipt := rs.EndMutationReceipt(token)

	if !receipt.Complete {
		t.Fatalf("receipt incomplete for a plain batch insert: %+v", receipt)
	}
	if !receipt.ResolutionRelevant {
		t.Fatalf("receipt missed a new unresolved edge: %+v", receipt)
	}
	if got, want := receipt.ResolutionFiles(), []string{"src/a.go", "src/b.go"}; !slices.Equal(got, want) {
		t.Fatalf("resolution files = %v, want exactly %v", got, want)
	}
	if !slices.Contains(receipt.TargetIDs, pending.To) {
		t.Fatalf("receipt target ids %v omit the unresolved target %q", receipt.TargetIDs, pending.To)
	}
	if !slices.Contains(receipt.TargetNames, "Load") {
		t.Fatalf("receipt target names %v omit the added definition", receipt.TargetNames)
	}

	// Re-applying identical writes changes nothing, so the window must be
	// complete AND empty — this is what lets an idempotent reindex skip the
	// resolve entirely.
	token = rs.BeginMutationReceipt()
	s.AddBatch([]*graph.Node{caller, callee}, []*graph.Edge{pending})
	replay := rs.EndMutationReceipt(token)
	if !replay.Complete {
		t.Fatalf("idempotent replay produced an incomplete receipt: %+v", replay)
	}
	if replay.ResolutionRelevant || len(replay.ResolutionFiles()) != 0 {
		t.Fatalf("idempotent replay reported a delta: %+v", replay)
	}

	// A token that was never issued (or was already closed) must fail closed:
	// an empty delta and "nothing happened" have to be distinguishable.
	if stale := rs.EndMutationReceipt(token); stale.Complete {
		t.Fatalf("reusing a closed token reported a complete receipt: %+v", stale)
	}
}

// testEmptyRepoPrefix pins what the empty repository prefix means on each
// read. It is not one convention: standalone indexing leaves every node's
// prefix empty, so "" is a real namespace on some calls and a stand-in for
// "every repository" on others, and the two backends do not even agree on
// EvictRepo. Recording the current answers is what makes a future change to
// any of them a deliberate one.
func testEmptyRepoPrefix(t *testing.T, factory Factory, semantics Semantics) {
	t.Helper()

	t.Run("NonContentNodesAreGlobal", func(t *testing.T) {
		s := factory(t)
		s.AddNode(mkRepoNode("r1/a.go::Foo", "Foo", "r1/a.go", "r1", graph.KindFunction))
		s.AddNode(mkRepoNode("r2/x.go::Baz", "Baz", "r2/x.go", "r2", graph.KindFunction))
		s.AddNode(mkNode("standalone.go::Bare", "Bare", "standalone.go", graph.KindFunction))

		// "" is a WILDCARD here — the global code/search passes are built on
		// it. Tightening it to an exact prefix match would empty them
		// silently rather than fail.
		if got := len(s.GetRepoNonContentNodes("")); got != 3 {
			t.Fatalf("GetRepoNonContentNodes(\"\") = %d nodes, want 3 (every repository)", got)
		}
		if got := len(s.GetRepoNonContentNodes("r1")); got != 1 {
			t.Fatalf("GetRepoNonContentNodes(\"r1\") = %d nodes, want 1", got)
		}
	})

	t.Run("EvictRepo", func(t *testing.T) {
		s := factory(t)
		s.AddNode(mkNode("standalone.go::Bare", "Bare", "standalone.go", graph.KindFunction))
		s.AddNode(mkRepoNode("r1/a.go::Foo", "Foo", "r1/a.go", "r1", graph.KindFunction))

		nodesRemoved, _ := s.EvictRepo("")
		if semantics.EvictsEmptyRepoPrefix {
			if nodesRemoved != 1 {
				t.Fatalf("EvictRepo(\"\") removed %d nodes, want 1 — this backend treats "+
					"the empty prefix as an ordinary repository", nodesRemoved)
			}
			if s.GetNode("standalone.go::Bare") != nil {
				t.Fatalf("EvictRepo(\"\") left the unprefixed node behind")
			}
		} else {
			if nodesRemoved != 0 {
				t.Fatalf("EvictRepo(\"\") removed %d nodes, want 0 — this backend treats "+
					"the empty prefix as \"not a repository\"", nodesRemoved)
			}
			if s.GetNode("standalone.go::Bare") == nil {
				t.Fatalf("EvictRepo(\"\") removed the unprefixed node on a backend that declares it a no-op")
			}
		}
		if s.GetNode("r1/a.go::Foo") == nil {
			t.Fatalf("EvictRepo(\"\") must never touch a prefixed repository")
		}
	})
}
