package store_sqlite

import (
	"fmt"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

func viewRevisionNode(tag string) *graph.Node {
	return &graph.Node{ID: "repo/" + tag + ".go::" + tag, Kind: graph.KindFunction, Name: tag, FilePath: "repo/" + tag + ".go", RepoPrefix: "repo"}
}

func activeAnalysisPointers(t *testing.T, s *Store, viewGen int64) int {
	t.Helper()
	return scalarInt(t, s.db, `SELECT COUNT(*) FROM analysis_active_generation WHERE view_gen = ?`, viewGen)
}

// A worktree's edit writes only its derived generations. On a view-scoped
// handle the base view's analysis revision does not move for it, while a write
// to the base does; the whole-store clock (the unscoped API) still moves for
// every write.
func TestViewScopedAnalysisRevisionMovesOnlyForItsViewAndTheBase(t *testing.T) {
	store := openAnalysisViewGenStore(t)
	store.AddNode(viewRevisionNode("seed"))
	base := store.ViewScopedAnalysis()
	derived := store.AtGeneration(5).ViewScopedAnalysis()
	baseRev, derivedRev, storeRev := base.AnalysisMutationRevision(), derived.AnalysisMutationRevision(), store.AnalysisMutationRevision()

	store.AtGeneration(5).AddNode(viewRevisionNode("edit"))
	if got := base.AnalysisMutationRevision(); got != baseRev {
		t.Fatalf("a write to generation 5 moved the base view's revision %d -> %d", baseRev, got)
	}
	if got := derived.AnalysisMutationRevision(); got == derivedRev {
		t.Fatal("a write to generation 5 did not move generation 5's revision")
	}
	if got := store.AnalysisMutationRevision(); got == storeRev {
		t.Fatal("the whole-store clock must move for every write")
	}
	store.AtGeneration(9).AddNode(viewRevisionNode("other"))
	if got := derived.AnalysisMutationRevision(); got != derivedRev+1 {
		t.Fatalf("a write to generation 9 moved generation 5's revision to %d", got)
	}

	derivedRev = derived.AnalysisMutationRevision()
	store.AddNode(viewRevisionNode("base-write"))
	if got := base.AnalysisMutationRevision(); got == baseRev {
		t.Fatal("a write to the base did not move the base view's revision")
	}
	if got := derived.AnalysisMutationRevision(); got == derivedRev {
		t.Fatal("a write to the base must move every view's revision (every view composes over it)")
	}
}

// The base view's analysis — a pass in flight and the installed, activated
// one — survives a write to another generation, durably and in memory; a write
// to the base invalidates it.
func TestBaseAnalysisSurvivesAWriteToAnotherGeneration(t *testing.T) {
	store := openAnalysisViewGenStore(t)
	store.AddNode(viewRevisionNode("seed"))
	base := store.ViewScopedAnalysis()
	active := buildMinimalAnalysisGeneration(t, base, "base", 2, true)
	if activeAnalysisPointers(t, store, 0) != 1 {
		t.Fatal("precondition: the base analysis is active")
	}

	// A pass begins, a worktree edit lands in generation 5, the pass goes on.
	revision := base.AnalysisMutationRevision()
	header := graph.AnalysisGenerationHeader{FormatVersion: 77, NodeCount: 1, CommunityCount: 1, PageRankMax: 1, AuthorityMax: 1, HubMax: 1, Modularity: 0.5}
	inflight, accepted, err := base.BeginAnalysisGeneration(revision, header)
	requireAnalysisAccepted(t)(accepted, err)
	store.AtGeneration(5).AddNode(viewRevisionNode("edit"))
	requireAnalysisAccepted(t)(base.AppendAnalysisCommunities(revision, inflight, []graph.AnalysisCommunitySummary{{ID: "c", Label: "c", Size: 1}}))
	if !base.CommitAnalysisSnapshot(revision, func() {}) {
		t.Fatal("the base snapshot was refused after a write to another generation")
	}
	if activeAnalysisPointers(t, store, 0) != 1 {
		t.Fatal("a write to generation 5 cleared the base analysis pointer")
	}
	if got, found, err := base.LoadActiveAnalysisHeader(77); err != nil || !found || got.GenerationID != active {
		t.Fatalf("base analysis after a write to generation 5 = %+v, %v, %v", got, found, err)
	}
	if _, err := base.AnalysisNodeMetrics(active, []string{"base-node"}); err != nil {
		t.Fatalf("base analysis unreadable after a write to generation 5: %v", err)
	}
	// The unscoped protocol keeps its whole-store behaviour.
	if store.CommitAnalysisSnapshot(store.AnalysisMutationRevision()-1, func() {}) {
		t.Fatal("the unscoped protocol accepted a revision a write superseded")
	}

	store.AddNode(viewRevisionNode("base-write"))
	if base.CommitAnalysisSnapshot(revision, func() {}) {
		t.Fatal("the base snapshot was accepted after a write to the base")
	}
	if n := activeAnalysisPointers(t, store, 0); n != 0 {
		t.Fatalf("a write to the base left %d base analysis pointers", n)
	}
}

// A derived generation's write stales only its own view's analysis, and the
// mutation latch stays armed while another view still holds one, so that
// view's next write is still invalidated.
func TestDerivedWriteStalesOnlyItsViewAndKeepsTheLatch(t *testing.T) {
	store := openAnalysisViewGenStore(t)
	store.AddNode(viewRevisionNode("seed"))
	_, five := buildAnalysisAt(t, store, 5, "five")
	_, seven := buildAnalysisAt(t, store, 7, "seven")
	store.AtGeneration(7).AddNode(viewRevisionNode("edit-seven"))
	if n := activeAnalysisPointers(t, store, 7); n != 0 {
		t.Fatalf("generation 7's write left %d of its own pointers", n)
	}
	if got := scalarInt(t, store.db, `SELECT state FROM analysis_generations WHERE generation_id = ?`, seven); got != analysisGenerationStale {
		t.Fatalf("generation 7's analysis state = %d, want stale", got)
	}
	if n := activeAnalysisPointers(t, store, 5); n != 1 {
		t.Fatalf("generation 7's write cleared generation 5's pointer (%d left)", n)
	}
	if !store.analysisGenerationPresent {
		t.Fatal("the latch was cleared while generation 5 still holds an analysis")
	}
	store.AtGeneration(5).AddNode(viewRevisionNode(fmt.Sprintf("edit-five-%d", five)))
	if n := activeAnalysisPointers(t, store, 5); n != 0 {
		t.Fatalf("generation 5's own write left %d pointers", n)
	}
}

// Edge-identity revisions follow the same scope: a provenance change in a
// worktree's generation leaves the base view's count (the incremental Leiden
// cache's key) unchanged; a change at the base moves every view's.
func TestEdgeIdentityRevisionsAreScopedToTheView(t *testing.T) {
	store := openAnalysisViewGenStore(t)
	edge := func() *graph.Edge {
		return &graph.Edge{From: "repo/a.go::A", To: "repo/a.go::B", Kind: graph.EdgeCalls, FilePath: "repo/a.go", Line: 3, Origin: "ast"}
	}
	nodes := []*graph.Node{viewRevisionNode("a"), {ID: "repo/a.go::A", Kind: graph.KindFunction, Name: "A", FilePath: "repo/a.go"}, {ID: "repo/a.go::B", Kind: graph.KindFunction, Name: "B", FilePath: "repo/a.go"}}
	store.AddBatch(nodes, []*graph.Edge{edge()})
	derived := store.AtGeneration(5)
	derived.AddBatch(nodes, []*graph.Edge{edge()})
	base0, derived0, all0 := store.EdgeIdentityRevisions(), derived.EdgeIdentityRevisions(), store.EdgeIdentityRevisionsAll()

	if !derived.SetEdgeProvenance(edge(), "lsp") {
		t.Fatal("precondition: the derived edge's provenance changed")
	}
	if got := store.EdgeIdentityRevisions(); got != base0 {
		t.Fatalf("a provenance change at generation 5 moved the base view's count %d -> %d", base0, got)
	}
	if derived.EdgeIdentityRevisions() == derived0 || store.EdgeIdentityRevisionsAll() == all0 {
		t.Fatal("the change must move generation 5's count and the whole store's")
	}
	derived1 := derived.EdgeIdentityRevisions()
	if !store.SetEdgeProvenance(edge(), "lsp") {
		t.Fatal("precondition: the base edge's provenance changed")
	}
	if store.EdgeIdentityRevisions() == base0 || derived.EdgeIdentityRevisions() == derived1 {
		t.Fatal("a base change must move every view's count")
	}
}
