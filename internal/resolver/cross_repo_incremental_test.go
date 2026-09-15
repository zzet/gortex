package resolver

import (
	"errors"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

func TestDetectCrossRepoEdgesForFilesUsesExactIncidentFrontier(t *testing.T) {
	g := graph.New()
	g.AddBatch([]*graph.Node{
		{ID: "a/a.go", Kind: graph.KindFile, Name: "a.go", FilePath: "a/a.go", RepoPrefix: "a"},
		{ID: "a/a.go::Call", Kind: graph.KindFunction, Name: "Call", FilePath: "a/a.go", RepoPrefix: "a"},
		{ID: "b/b.go", Kind: graph.KindFile, Name: "b.go", FilePath: "b/b.go", RepoPrefix: "b"},
		{ID: "b/b.go::Serve", Kind: graph.KindFunction, Name: "Serve", FilePath: "b/b.go", RepoPrefix: "b"},
		{ID: "c/c.go", Kind: graph.KindFile, Name: "c.go", FilePath: "c/c.go", RepoPrefix: "c"},
		{ID: "c/c.go::Call", Kind: graph.KindFunction, Name: "Call", FilePath: "c/c.go", RepoPrefix: "c"},
		{ID: "d/d.go", Kind: graph.KindFile, Name: "d.go", FilePath: "d/d.go", RepoPrefix: "d"},
		{ID: "d/d.go::Serve", Kind: graph.KindFunction, Name: "Serve", FilePath: "d/d.go", RepoPrefix: "d"},
	}, []*graph.Edge{
		{From: "a/a.go::Call", To: "b/b.go::Serve", Kind: graph.EdgeCalls, FilePath: "a/a.go", Line: 3},
		{From: "c/c.go::Call", To: "d/d.go::Serve", Kind: graph.EdgeCalls, FilePath: "c/c.go", Line: 4},
	})

	if got := DetectCrossRepoEdgesForFiles(g, []string{"b/b.go"}); got != 1 {
		t.Fatalf("emitted = %d, want incident edge only", got)
	}
	crossKind, ok := graph.CrossRepoKindFor(graph.EdgeCalls)
	if !ok {
		t.Fatal("calls has no cross-repo kind")
	}
	if !hasEdgeKindTo(g.GetOutEdges("a/a.go::Call"), crossKind, "b/b.go::Serve") {
		t.Fatal("incoming edge to changed target was not materialized")
	}
	if hasEdgeKindTo(g.GetOutEdges("c/c.go::Call"), crossKind, "d/d.go::Serve") {
		t.Fatal("unrelated cross-repo edge leaked outside exact frontier")
	}
}

func TestDetectCrossRepoEdgesForMutationFilesSeparatesRoles(t *testing.T) {
	newFixture := func() *graph.Graph {
		g := graph.New()
		nodes := []*graph.Node{
			{ID: "repoA/a.go::A", Kind: graph.KindFunction, FilePath: "repoA/a.go", RepoPrefix: "repoA"},
			{ID: "repoB/b.go::B", Kind: graph.KindFunction, FilePath: "repoB/b.go", RepoPrefix: "repoB"},
			{ID: "repoD/definition.go::D", Kind: graph.KindFunction, FilePath: "repoD/definition.go", RepoPrefix: "repoD"},
			{ID: "repoE/e.go::E", Kind: graph.KindFunction, FilePath: "repoE/e.go", RepoPrefix: "repoE"},
			{ID: "repoF/edge-source.go::F", Kind: graph.KindFunction, FilePath: "repoF/edge-source.go", RepoPrefix: "repoF"},
			{ID: "repoG/g.go::G", Kind: graph.KindFunction, FilePath: "repoG/g.go", RepoPrefix: "repoG"},
		}
		g.AddBatch(nodes, []*graph.Edge{
			{From: nodes[0].ID, To: nodes[1].ID, Kind: graph.EdgeCalls, FilePath: "repoF/edge-source.go", Line: 1},
			{From: nodes[4].ID, To: nodes[5].ID, Kind: graph.EdgeCalls, FilePath: "elsewhere.go", Line: 2},
			{From: nodes[2].ID, To: nodes[3].ID, Kind: graph.EdgeCalls, FilePath: "elsewhere.go", Line: 3},
			{From: nodes[0].ID, To: nodes[1].ID, Kind: graph.EdgeCalls, FilePath: "repoD/definition.go", Line: 4},
		})
		return g
	}
	crossKind, ok := graph.CrossRepoKindFor(graph.EdgeCalls)
	if !ok {
		t.Fatal("calls has no cross-repo kind")
	}

	t.Run("edge source", func(t *testing.T) {
		g := newFixture()
		if got := DetectCrossRepoEdgesForMutationFiles(g, []string{"repoF/edge-source.go"}, nil); got != 1 {
			t.Fatalf("emitted = %d, want edge-source edge only", got)
		}
		if !hasEdgeKindTo(g.GetOutEdges("repoA/a.go::A"), crossKind, "repoB/b.go::B") {
			t.Fatal("edge stamped with changed source file was not materialized")
		}
		if hasEdgeKindTo(g.GetOutEdges("repoF/edge-source.go::F"), crossKind, "repoG/g.go::G") {
			t.Fatal("edge-source role broadened into endpoint incidence")
		}
	})

	t.Run("definition", func(t *testing.T) {
		g := newFixture()
		if got := DetectCrossRepoEdgesForMutationFiles(g, nil, []string{"repoD/definition.go"}); got != 1 {
			t.Fatalf("emitted = %d, want definition-incident edge only", got)
		}
		if !hasEdgeKindTo(g.GetOutEdges("repoD/definition.go::D"), crossKind, "repoE/e.go::E") {
			t.Fatal("edge incident to changed definition was not materialized")
		}
		if hasEdgeKindTo(g.GetOutEdges("repoA/a.go::A"), crossKind, "repoB/b.go::B") {
			t.Fatal("definition role broadened into edge source stamps")
		}
	})
}

func TestResolveForFilePrefixesMultiRepoGraphPath(t *testing.T) {
	g := graph.New()
	g.AddBatch([]*graph.Node{
		{ID: "a/a.go", Kind: graph.KindFile, Name: "a.go", FilePath: "a/a.go", RepoPrefix: "a"},
		{ID: "a/a.go::Call", Kind: graph.KindFunction, Name: "Call", FilePath: "a/a.go", RepoPrefix: "a"},
		{ID: "b/b.go", Kind: graph.KindFile, Name: "b.go", FilePath: "b/b.go", RepoPrefix: "b"},
		{ID: "b/b.go::Serve", Kind: graph.KindFunction, Name: "Serve", FilePath: "b/b.go", RepoPrefix: "b"},
	}, []*graph.Edge{
		{From: "a/a.go::Call", To: "b/b.go::Serve", Kind: graph.EdgeCalls, FilePath: "a/a.go", Line: 3},
	})

	NewCrossRepo(g).ResolveForFile("a", "a.go")

	crossKind, ok := graph.CrossRepoKindFor(graph.EdgeCalls)
	if !ok {
		t.Fatal("calls has no cross-repo kind")
	}
	if !hasEdgeKindTo(g.GetOutEdges("a/a.go::Call"), crossKind, "b/b.go::Serve") {
		t.Fatal("repo-relative watcher path did not resolve to its prefixed graph file")
	}
}

func hasEdgeKindTo(edges []*graph.Edge, kind graph.EdgeKind, target string) bool {
	for _, edge := range edges {
		if edge != nil && edge.Kind == kind && edge.To == target {
			return true
		}
	}
	return false
}

// The cross-repository mutation pass builds the same bounded frontier the
// single-repository legs do. A refusal empties its incoming half, so the pass
// resolves only the changed files' own outgoing edges while its CrossRepoStats
// still count a normal-looking resolution. Without the admission fact riding
// out of the call and a Warn on the way, that reads as a complete pass.
func TestCrossRepoMutationFrontiersCarriesIncomingRefusal(t *testing.T) {
	fanIn := graph.MaxIncomingSourceCandidateRows + 1
	g, changed := incomingFanOutGraph(t, fanIn)
	stub := graph.UnresolvedMarker + "Close"

	logs, observed := observedResolverLogger()
	cr := NewCrossRepo(g)
	cr.SetLogger(logs)

	stats, admission := cr.ResolveMutationFrontiersBounded([]string{changed}, []string{changed}, []string{changed})
	if stats == nil {
		t.Fatal("cross-repo pass returned no stats")
	}
	var limit *graph.BoundedLocalizationLimitError
	if !errors.As(admission.Refusal, &limit) {
		t.Fatalf("cross-repo refusal = %v, want *graph.BoundedLocalizationLimitError", admission.Refusal)
	}
	if limit.Limit != graph.MaxIncomingSourceCandidateRows {
		t.Fatalf("refusal limit = %d, want the shared ceiling %d", limit.Limit, graph.MaxIncomingSourceCandidateRows)
	}
	if !admission.Refused {
		t.Fatalf("completeness fact = %+v, want Refused", admission.IncomingSourceAdmission)
	}
	if admission.Dropped != fanIn || admission.Inspected != fanIn {
		t.Fatalf("admission = %+v, want %d inspected and dropped", admission.IncomingSourceAdmission, fanIn)
	}
	if admission.Limit != graph.MaxIncomingSourceCandidateRows {
		t.Fatalf("admission ceiling = %d, want %d", admission.Limit, graph.MaxIncomingSourceCandidateRows)
	}
	// Fail-closed: nothing from the refused leg was rebound.
	if left := unresolvedInEdgeCount(g, stub); left != fanIn {
		t.Fatalf("%d of %d parked references were rebound by a refused cross-repo admission", fanIn-left, fanIn)
	}

	entries := observed.FilterMessage("resolver: incoming stub admission refused").All()
	if len(entries) != 1 {
		t.Fatalf("cross-repo refusal log records = %d, want exactly 1", len(entries))
	}
	fields := entries[0].ContextMap()
	if fields["leg"] != "cross_repo_preparation" {
		t.Fatalf("refusal logged for leg %v, want cross_repo_preparation", fields["leg"])
	}
	if fields["dropped"] != int64(fanIn) {
		t.Fatalf("logged dropped = %v, want %d", fields["dropped"], fanIn)
	}

	// The unchanged public entrypoint reaches the same fact-and-log path; only
	// the fact's return value is dropped there.
	logs, observed = observedResolverLogger()
	cr = NewCrossRepo(g)
	cr.SetLogger(logs)
	if got := cr.ResolveMutationFrontiers([]string{changed}, []string{changed}, []string{changed}); got == nil {
		t.Fatal("legacy entrypoint returned no stats")
	}
	if n := observed.FilterMessage("resolver: incoming stub admission refused").Len(); n != 1 {
		t.Fatalf("legacy entrypoint logged %d refusals, want 1", n)
	}
}

// An in-ceiling cross-repo pass stays a complete pass: no fact, no Warn, and
// the parked references admitted. Without this the test above is satisfiable by
// flagging every pass.
func TestCrossRepoMutationFrontiersKeepsAdmittedPassClean(t *testing.T) {
	g, changed := incomingFanOutGraph(t, 64)

	logs, observed := observedResolverLogger()
	cr := NewCrossRepo(g)
	cr.SetLogger(logs)

	_, admission := cr.ResolveMutationFrontiersBounded([]string{changed}, []string{changed}, []string{changed})
	if admission.Refusal != nil || admission.Refused || admission.Dropped != 0 {
		t.Fatalf("a 64-edge fan-out was reported as bounded: %+v %v", admission.IncomingSourceAdmission, admission.Refusal)
	}
	if admission.Inspected != 64 {
		t.Fatalf("inspected rows = %d, want 64", admission.Inspected)
	}
	if n := observed.FilterMessage("resolver: incoming stub admission refused").Len(); n != 0 {
		t.Fatalf("clean cross-repo pass logged %d refusals", n)
	}
}
