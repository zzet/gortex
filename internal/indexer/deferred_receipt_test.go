package indexer

import (
	"iter"
	"sync/atomic"
	"testing"

	"github.com/zzet/gortex/internal/graph"
	"go.uber.org/zap"
)

type deferredScanCountingStore struct {
	*graph.Graph
	unresolvedScans atomic.Int64
}

func (s *deferredScanCountingStore) EdgesWithUnresolvedTarget() iter.Seq[*graph.Edge] {
	s.unresolvedScans.Add(1)
	return s.Graph.EdgesWithUnresolvedTarget()
}

func TestResolveDeferredMutationsSkipsCompleteEmptyReceiptWithoutScanning(t *testing.T) {
	store, edge := deferredReceiptFixture()
	mi := &MultiIndexer{graph: store, logger: zap.NewNop()}

	mode, _ := mi.resolveDeferredMutations(&graph.MutationReceipt{Complete: true}, true, nil, false)
	if mode != deferredResolveSkipped {
		t.Fatalf("mode = %q, want %q", mode, deferredResolveSkipped)
	}
	if got := store.unresolvedScans.Load(); got != 0 {
		t.Fatalf("unresolved full scans = %d, want 0", got)
	}
	if edge.To != graph.UnresolvedMarker+"Target" {
		t.Fatalf("skip unexpectedly changed target to %q", edge.To)
	}
}

func TestResolveDeferredMutationsUsesExactDefinitionFrontier(t *testing.T) {
	store, edge := deferredReceiptFixture()
	mi := &MultiIndexer{graph: store, logger: zap.NewNop()}
	receipt := &graph.MutationReceipt{
		Complete:           true,
		ResolutionRelevant: true,
		DefinitionFiles:    []string{"b.go"},
		TargetNames:        []string{"Target"},
		TargetIDs:          []string{"b.go::Target"},
	}

	mode, _ := mi.resolveDeferredMutations(receipt, true, nil, false)
	if mode != deferredResolveExact {
		t.Fatalf("mode = %q, want %q", mode, deferredResolveExact)
	}
	if got := store.unresolvedScans.Load(); got != 0 {
		t.Fatalf("exact resolution performed %d whole unresolved scans", got)
	}
	if edge.To != "b.go::Target" {
		t.Fatalf("edge target = %q, want b.go::Target", edge.To)
	}
}

func TestResolveDeferredMutationsUsesExactChangedFileFrontier(t *testing.T) {
	store, edge := deferredReceiptFixture()
	mi := &MultiIndexer{graph: store, logger: zap.NewNop()}
	receipt := &graph.MutationReceipt{
		Complete:           true,
		ResolutionRelevant: true,
		ChangedFiles:       []string{"a.go"},
		UnresolvedFiles:    []string{"a.go"},
		TargetNames:        []string{"Target"},
	}

	mode, _ := mi.resolveDeferredMutations(receipt, false, nil, false)
	if mode != deferredResolveExact {
		t.Fatalf("mode = %q, want %q", mode, deferredResolveExact)
	}
	if got := store.unresolvedScans.Load(); got != 0 {
		t.Fatalf("exact resolution performed %d whole unresolved scans", got)
	}
	if edge.To != "b.go::Target" {
		t.Fatalf("edge target = %q, want b.go::Target", edge.To)
	}
}

func TestResolveDeferredMutationsIncompleteReceiptFallsBackToFullScan(t *testing.T) {
	store, edge := deferredReceiptFixture()
	mi := &MultiIndexer{graph: store, logger: zap.NewNop()}

	mode, _ := mi.resolveDeferredMutations(&graph.MutationReceipt{Complete: false}, false, map[string]struct{}{"repo": {}}, false)
	if mode != deferredResolveFallback {
		t.Fatalf("mode = %q, want %q", mode, deferredResolveFallback)
	}
	if got := store.unresolvedScans.Load(); got == 0 {
		t.Fatal("incomplete receipt did not use whole-graph unresolved scan")
	}
	if edge.To != "b.go::Target" {
		t.Fatalf("fallback edge target = %q, want b.go::Target", edge.To)
	}
}

func TestResolveDeferredMutationsMissingExactPathFailsClosed(t *testing.T) {
	store, _ := deferredReceiptFixture()
	mi := &MultiIndexer{graph: store, logger: zap.NewNop()}
	receipt := &graph.MutationReceipt{Complete: true, ResolutionRelevant: true, TargetNames: []string{"Target"}}

	mode, _ := mi.resolveDeferredMutations(receipt, false, nil, false)
	if mode != deferredResolveFallback {
		t.Fatalf("mode = %q, want %q", mode, deferredResolveFallback)
	}
	if got := store.unresolvedScans.Load(); got == 0 {
		t.Fatal("missing exact path did not fail closed to whole scan")
	}
}

func TestResolveDeferredMutationsExactReceiptCompletesCrossRepoFrontier(t *testing.T) {
	store := deferredCrossRepoReceiptFixture()
	mi := &MultiIndexer{graph: store, logger: zap.NewNop()}
	receipt := &graph.MutationReceipt{
		Complete:           true,
		ResolutionRelevant: true,
		DefinitionFiles:    []string{"b/b.go"},
		TargetIDs:          []string{"b/b.go::Serve"},
	}

	mode, _ := mi.resolveDeferredMutations(receipt, true, nil, false)
	if mode != deferredResolveExact {
		t.Fatalf("mode = %q, want %q", mode, deferredResolveExact)
	}
	if got := store.unresolvedScans.Load(); got != 0 {
		t.Fatalf("exact cross-repo catch-up performed %d whole unresolved scans", got)
	}
	crossKind, ok := graph.CrossRepoKindFor(graph.EdgeCalls)
	if !ok {
		t.Fatal("calls has no cross-repo kind")
	}
	if !deferredHasEdgeKindTo(store.GetOutEdges("a/a.go::Call"), crossKind, "b/b.go::Serve") {
		t.Fatal("exact receipt did not materialize the incoming cross-repo edge")
	}
	if deferredHasEdgeKindTo(store.GetOutEdges("c/c.go::Call"), crossKind, "d/d.go::Serve") {
		t.Fatal("exact receipt leaked cross-repo materialization outside its file frontier")
	}
}

func TestResolveDeferredMutationsResolvedEdgeSkipsMasterButMaterializesCrossRepo(t *testing.T) {
	store := deferredCrossRepoReceiptFixture()
	mi := &MultiIndexer{graph: store, logger: zap.NewNop()}
	receipt := &graph.MutationReceipt{
		Complete:     true,
		ChangedFiles: []string{"a/a.go"},
	}

	mode, _ := mi.resolveDeferredMutations(receipt, true, nil, false)
	if mode != deferredResolveExact {
		t.Fatalf("mode = %q, want %q", mode, deferredResolveExact)
	}
	if got := store.unresolvedScans.Load(); got != 0 {
		t.Fatalf("resolved-only receipt performed %d unresolved scans", got)
	}
	crossKind, ok := graph.CrossRepoKindFor(graph.EdgeCalls)
	if !ok {
		t.Fatal("calls has no cross-repo kind")
	}
	if !deferredHasEdgeKindTo(store.GetOutEdges("a/a.go::Call"), crossKind, "b/b.go::Serve") {
		t.Fatal("resolved-only receipt did not materialize its cross-repo edge")
	}
	if deferredHasEdgeKindTo(store.GetOutEdges("c/c.go::Call"), crossKind, "d/d.go::Serve") {
		t.Fatal("resolved-only receipt leaked outside its edge-source frontier")
	}
}

func TestResolveDeferredMutationsIncompleteReceiptCompletesFullCrossRepoFallback(t *testing.T) {
	store := deferredCrossRepoReceiptFixture()
	mi := &MultiIndexer{graph: store, logger: zap.NewNop()}

	mode, complete := mi.resolveDeferredMutations(&graph.MutationReceipt{
		Complete:         false,
		IncompleteReason: "overlapping_resolver_write",
	}, false, map[string]struct{}{"b": {}}, false)
	if mode != deferredResolveFallback {
		t.Fatalf("mode = %q, want %q", mode, deferredResolveFallback)
	}
	if !complete {
		t.Fatal("successful full fallback did not report cross-repository completion")
	}
	if got := store.unresolvedScans.Load(); got == 0 {
		t.Fatal("incomplete receipt did not use the full unresolved fallback")
	}
	crossKind, ok := graph.CrossRepoKindFor(graph.EdgeCalls)
	if !ok {
		t.Fatal("calls has no cross-repo kind")
	}
	for _, want := range []struct{ from, to string }{
		{from: "a/a.go::Call", to: "b/b.go::Serve"},
		{from: "c/c.go::Call", to: "d/d.go::Serve"},
	} {
		if !deferredHasEdgeKindTo(store.GetOutEdges(want.from), crossKind, want.to) {
			t.Fatalf("full fallback did not materialize cross-repo edge %s -> %s", want.from, want.to)
		}
	}
}

func TestFinishTailResultPublishesFullFallbackCompletionRevision(t *testing.T) {
	store := deferredCrossRepoReceiptFixture()
	done := make(chan struct{})
	close(done)
	run := &DeferredPassesRun{
		mi:            &MultiIndexer{graph: store, logger: zap.NewNop()},
		catchupNeeded: true,
		poolDone:      done,
	}

	result := run.FinishTailResult()
	if !result.CrossRepoComplete {
		t.Fatal("successful full fallback did not publish a completion proof")
	}
	if result.ExactCrossRepoComplete {
		t.Fatal("full fallback was incorrectly reported as an exact receipt frontier")
	}
	if !result.CrossRepoMutationRevisionKnown {
		t.Fatal("revision-capable graph did not publish a completion revision")
	}
	if got := store.MutationRevision(); result.CrossRepoMutationRevision != got {
		t.Fatalf("completion revision = %d, current graph revision = %d", result.CrossRepoMutationRevision, got)
	}
}

func deferredCrossRepoReceiptFixture() *deferredScanCountingStore {
	g := graph.New()
	g.AddBatch([]*graph.Node{
		{ID: "a/a.go", Kind: graph.KindFile, Name: "a.go", FilePath: "a/a.go", RepoPrefix: "a", WorkspaceID: "ws"},
		{ID: "a/a.go::Call", Kind: graph.KindFunction, Name: "Call", FilePath: "a/a.go", RepoPrefix: "a", WorkspaceID: "ws"},
		{ID: "b/b.go", Kind: graph.KindFile, Name: "b.go", FilePath: "b/b.go", RepoPrefix: "b", WorkspaceID: "ws"},
		{ID: "b/b.go::Serve", Kind: graph.KindFunction, Name: "Serve", FilePath: "b/b.go", RepoPrefix: "b", WorkspaceID: "ws"},
		{ID: "c/c.go", Kind: graph.KindFile, Name: "c.go", FilePath: "c/c.go", RepoPrefix: "c", WorkspaceID: "ws"},
		{ID: "c/c.go::Call", Kind: graph.KindFunction, Name: "Call", FilePath: "c/c.go", RepoPrefix: "c", WorkspaceID: "ws"},
		{ID: "d/d.go", Kind: graph.KindFile, Name: "d.go", FilePath: "d/d.go", RepoPrefix: "d", WorkspaceID: "ws"},
		{ID: "d/d.go::Serve", Kind: graph.KindFunction, Name: "Serve", FilePath: "d/d.go", RepoPrefix: "d", WorkspaceID: "ws"},
	}, []*graph.Edge{
		{From: "a/a.go::Call", To: "b/b.go::Serve", Kind: graph.EdgeCalls, FilePath: "a/a.go", Line: 3},
		{From: "c/c.go::Call", To: "d/d.go::Serve", Kind: graph.EdgeCalls, FilePath: "c/c.go", Line: 4},
	})
	return &deferredScanCountingStore{Graph: g}
}

func deferredHasEdgeKindTo(edges []*graph.Edge, kind graph.EdgeKind, target string) bool {
	for _, edge := range edges {
		if edge != nil && edge.Kind == kind && edge.To == target {
			return true
		}
	}
	return false
}

func deferredReceiptFixture() (*deferredScanCountingStore, *graph.Edge) {
	g := graph.New()
	edge := &graph.Edge{From: "a.go::Caller", To: graph.UnresolvedMarker + "Target", Kind: graph.EdgeCalls, FilePath: "a.go", Line: 3}
	g.AddBatch([]*graph.Node{
		{ID: "a.go", Kind: graph.KindFile, Name: "a.go", FilePath: "a.go", Language: "go"},
		{ID: "a.go::Caller", Kind: graph.KindFunction, Name: "Caller", FilePath: "a.go", Language: "go"},
		{ID: "b.go", Kind: graph.KindFile, Name: "b.go", FilePath: "b.go", Language: "go"},
		{ID: "b.go::Target", Kind: graph.KindFunction, Name: "Target", FilePath: "b.go", Language: "go"},
	}, []*graph.Edge{edge})
	return &deferredScanCountingStore{Graph: g}, edge
}

// A flat unresolved-insertion counter does not make an incomplete receipt
// exact: enrichment may add a definition that binds an old incoming stub, or
// an already-resolved cross-repo base edge that still needs materialising.
func TestResolveDeferredMutationsIncompleteReceiptFallsBackWhenNoUnresolvedWrites(t *testing.T) {
	store, edge := deferredReceiptFixture()
	mi := &MultiIndexer{graph: store, logger: zap.NewNop()}

	mode, _ := mi.resolveDeferredMutations(&graph.MutationReceipt{Complete: false}, false, nil, true)
	if mode != deferredResolveFallback {
		t.Fatalf("mode = %q, want %q", mode, deferredResolveFallback)
	}
	if got := store.unresolvedScans.Load(); got == 0 {
		t.Fatal("incomplete receipt skipped the fail-closed unresolved scan")
	}
	if edge.To != "b.go::Target" {
		t.Fatalf("fallback edge target = %q, want b.go::Target", edge.To)
	}
}

// The reviewer scenario for receipt-exact evictions: a pending reference in a
// file OUTSIDE every receipt frontier names an evicted definition. The evicted
// file is empty post-eviction, so the file-scoped pass cannot reach the
// pending edge; only the receipt's EvictedNames can. Before the names pass the
// whole-graph fallback healed this shape on every wave.
func TestResolveDeferredMutationsExactReceiptRebindsEvictedNamePendings(t *testing.T) {
	store, edge := deferredReceiptFixture()
	mi := &MultiIndexer{graph: store, logger: zap.NewNop()}
	receipt := &graph.MutationReceipt{
		Complete:           true,
		ResolutionRelevant: true,
		DefinitionFiles:    []string{"gone.go"},
		TargetNames:        []string{"Target"},
		EvictedNames:       []string{"Target"},
	}

	mode, complete := mi.resolveDeferredMutations(receipt, false, nil, false)
	if mode != deferredResolveExact || !complete {
		t.Fatalf("mode = %q complete=%v, want %q exact", mode, complete, deferredResolveExact)
	}
	if got := store.unresolvedScans.Load(); got != 0 {
		t.Fatalf("exact path performed %d whole unresolved scans", got)
	}
	if edge.To != "b.go::Target" {
		t.Fatalf("name-parked pending edge target = %q, want b.go::Target (rebound via receipt EvictedNames)", edge.To)
	}
}

// The narrowing itself: the name pass is driven by EvictedNames, not by the
// whole target set. TargetNames carries every added node's Name and QualName,
// and each entry costs four stub forms per repository prefix in the pass's
// probe, so feeding it the added names is what made the pass scale with batch
// size instead of with the evictions that motivated it. An added definition
// needs no name pass: its file is in DefinitionFiles, and the file frontier
// enumerates the stub forms of every name that file declares.
func TestResolveDeferredMutationsExactReceiptNamePassIgnoresAddedNames(t *testing.T) {
	store, edge := deferredReceiptFixture()
	mi := &MultiIndexer{graph: store, logger: zap.NewNop()}
	receipt := &graph.MutationReceipt{
		Complete:           true,
		ResolutionRelevant: true,
		DefinitionFiles:    []string{"gone.go"},
		TargetNames:        []string{"Target"},
	}

	mode, complete := mi.resolveDeferredMutations(receipt, false, nil, false)
	if mode != deferredResolveExact || !complete {
		t.Fatalf("mode = %q complete=%v, want %q exact", mode, complete, deferredResolveExact)
	}
	if edge.To != graph.UnresolvedMarker+"Target" {
		t.Fatalf("name-parked pending edge target = %q, want it untouched by the name pass", edge.To)
	}
}
