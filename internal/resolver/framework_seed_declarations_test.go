package resolver

import (
	"fmt"
	"sort"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// Every synthesizer that runs through the generic scoped store states what it
// needs from the change; the table names no synthesizer that does not exist.
func TestEveryGenericFrameworkSynthesizerDeclaresItsSeed(t *testing.T) {
	registered := make(map[string]bool)
	for _, s := range defaultFrameworkSynthesizers() {
		registered[s.Name()] = true
		sf, ok := s.(synthFunc)
		if !ok || sf.scopedFn != nil {
			continue
		}
		if _, declared := frameworkSynthSeedDeclarations[sf.name]; !declared {
			t.Errorf("framework synthesizer %s runs through the generic scoped store without a seed declaration", sf.name)
		}
	}
	for name := range frameworkSynthSeedDeclarations {
		if !registered[name] {
			t.Errorf("seed declaration for %s names no registered synthesizer", name)
		}
	}
}

// declaredSeedFixture is a changed file that the rest of the repository calls
// into far past the old seed's row cap, with a Gin registrar in it.
func declaredSeedFixture(callers int) *graph.Graph {
	g := graph.New()
	const file = "repo/config.go"
	g.AddBatch([]*graph.Node{
		{ID: file, Kind: graph.KindFile, Name: "config.go", FilePath: file, RepoPrefix: "repo", Language: "go"},
		{ID: file + "::Load", Kind: graph.KindFunction, Name: "Load", FilePath: file, RepoPrefix: "repo", Language: "go",
			Meta: map[string]any{"gin_handlers": []string{"listUsers"}, "signature": "func Load() Config"}},
		{ID: "repo/handlers.go::listUsers", Kind: graph.KindFunction, Name: "listUsers", FilePath: "repo/handlers.go", RepoPrefix: "repo", Language: "go"},
	}, []*graph.Edge{
		// The changed file's own receiver-typed call to an external method.
		{From: file + "::Load", To: "unresolved::*.WriteString", Kind: graph.EdgeCalls, FilePath: file, Line: 9,
			Meta: map[string]any{"receiver_type": "strings.Builder"}},
	})
	var nodes []*graph.Node
	var edges []*graph.Edge
	for i := 0; i < callers; i++ {
		path := fmt.Sprintf("repo/c%05d.go", i)
		caller := fmt.Sprintf("%s::Use%d", path, i)
		nodes = append(nodes, &graph.Node{ID: caller, Kind: graph.KindFunction, Name: fmt.Sprintf("Use%d", i),
			FilePath: path, RepoPrefix: "repo", Language: "go"})
		edges = append(edges, &graph.Edge{From: caller, To: file + "::Load", Kind: graph.EdgeCalls, FilePath: path, Line: 3,
			Meta: map[string]any{"callee": fmt.Sprintf("tok%d", i)}})
	}
	g.AddBatch(nodes, edges)
	return g
}

func countFrameworkEdges(seq func(func(*graph.Edge) bool)) int {
	n := 0
	for e := range seq {
		if e != nil {
			n++
		}
	}
	return n
}

// TestFrameworkDeclaredSeedReadsOnlyWhatAPassDeclares pins the per-pass
// scope: a pass that declares nothing reads no incoming row and no name, a
// name-declaring pass reads exactly its names once, an incoming-declaring
// pass reads the incident rows once per run, uncapped, and a second pass
// declaring the same kind reuses that read.
func TestFrameworkDeclaredSeedReadsOnlyWhatAPassDeclares(t *testing.T) {
	const callers = frameworkScopeRetainedRowCap + 500
	trap := &frameworkScopeTrapStore{Store: declaredSeedFixture(callers)}
	seed := newFrameworkDeclaredSeed(trap, map[string]bool{"repo": true}, []string{"repo/config.go"})

	none := seed.passStore(SynthFactoryChain)
	for range none.NodesByKind(graph.KindFunction) {
	}
	if got := countFrameworkEdges(none.EdgesByKind(graph.EdgeCalls)); got != 1 {
		t.Fatalf("a pass declaring nothing enumerated %d calls; want the changed file's own one", got)
	}
	if trap.batchInEdges != 0 || trap.batchNames != 0 {
		t.Fatalf("a pass declaring nothing read incoming rows %d times and names %d times", trap.batchInEdges, trap.batchNames)
	}

	gin := seed.passStore(SynthGinMiddleware)
	var sawHandler bool
	for n := range gin.NodesByKind(graph.KindFunction) {
		if n != nil && n.ID == "repo/handlers.go::listUsers" {
			sawHandler = true
		}
	}
	if !sawHandler {
		t.Fatal("the Gin pass did not see the handler its registrar names")
	}
	if trap.batchNames != 1 || trap.batchInEdges != 0 {
		t.Fatalf("the Gin pass read names %d times and incoming rows %d times; want 1 and 0", trap.batchNames, trap.batchInEdges)
	}
	names := seed.declaredNames(frameworkSynthSeedDeclarationFor(SynthGinMiddleware))
	sort.Strings(names)
	if fmt.Sprint(names) != "[listUsers]" {
		t.Fatalf("the Gin pass prefetched %v; want only its handler names", names)
	}

	// A Godot pass over a Go change reads nothing: its candidates and targets
	// are GDScript and project rows.
	godot := seed.passStore(SynthGodotAutoload)
	for range godot.NodesByKind(graph.KindType) {
	}
	if trap.batchNames != 1 {
		t.Fatalf("the Godot pass read names over a Go change (%d name reads in all)", trap.batchNames)
	}

	grpc := seed.passStore(SynthGRPCStub)
	if got := countFrameworkEdges(grpc.EdgesByKind(graph.EdgeCalls)); got != callers+1 {
		t.Fatalf("the gRPC pass enumerated %d calls; want all %d incoming (no row cap) and the file's own", got, callers)
	}
	mediatr := seed.passStore(SynthMediatR)
	if got := countFrameworkEdges(mediatr.EdgesByKind(graph.EdgeCalls)); got != callers+1 {
		t.Fatalf("the MediatR pass enumerated %d calls; want %d incoming and the file's own", got, callers)
	}
	if trap.batchInEdges != 1 {
		t.Fatalf("incoming rows were read %d times; want one read per run", trap.batchInEdges)
	}
	if got := countFrameworkEdges(gin.EdgesByKind(graph.EdgeCalls)); got != 1 {
		t.Fatalf("the Gin pass, which declares no incoming rows, enumerated %d calls; want the file's own one", got)
	}
	requireNoFrameworkGlobalScans(t, trap)
}

// TestFrameworkRunScopesTheSeedToTheRunningPasses: the scoped run over a
// widely-called Go file serves its passes from their declared parts — the
// incident read returns every caller, past the old row cap — and builds no
// bounded legacy seed, because no pass that runs on it is undeclared.
func TestFrameworkRunScopesTheSeedToTheRunningPasses(t *testing.T) {
	const callers = 3 * frameworkScopeRetainedRowCap
	trap := &frameworkScopeTrapStore{Store: declaredSeedFixture(callers)}
	rep := RunFrameworkSynthesizersScopedForFiles(trap, map[string]bool{"repo": true}, []string{"repo/config.go"}, false)
	requireNoFrameworkGlobalScans(t, trap)
	if rep.ScopeRows < callers {
		t.Fatalf("scope rows = %d; the incident read is uncapped and returns all %d callers", rep.ScopeRows, callers)
	}
	if rep.ScopeBytes != 0 {
		t.Fatalf("the run built the bounded legacy seed (%d bytes) though no undeclared pass runs on a Go file", rep.ScopeBytes)
	}
}

// TestFrameworkReceiverGateIsGatedByTheFrontierLanguage: the receiver-type
// tail of a changed-file run reads nothing for a frontier with no C# node,
// runs for a C# frontier, and always runs when the C# hierarchy changed.
func TestFrameworkReceiverGateIsGatedByTheFrontierLanguage(t *testing.T) {
	g := graph.New()
	g.AddBatch([]*graph.Node{
		{ID: "repo/a.go::F", Kind: graph.KindMethod, Name: "F", FilePath: "repo/a.go", RepoPrefix: "repo", Language: "go"},
		{ID: "repo/B.cs::B.M", Kind: graph.KindMethod, Name: "M", FilePath: "repo/B.cs", RepoPrefix: "repo", Language: "csharp"},
	}, nil)
	scope := map[string]bool{"repo": true}
	goFrontier := summarizeFrameworkCandidatesCensus(g, scope, []string{"repo/a.go"}, false)
	if frameworkReceiverGateReachesFrontier([]string{"repo/a.go"}, false, goFrontier) {
		t.Fatal("the receiver-type tail runs over a Go-only frontier")
	}
	if !frameworkReceiverGateReachesFrontier([]string{"repo/a.go"}, true, goFrontier) {
		t.Fatal("a changed C# hierarchy must keep the receiver-type tail")
	}
	csFrontier := summarizeFrameworkCandidatesCensus(g, scope, []string{"repo/B.cs"}, false)
	if !frameworkReceiverGateReachesFrontier([]string{"repo/B.cs"}, false, csFrontier) {
		t.Fatal("a C# frontier must run the receiver-type tail")
	}
	if !frameworkReceiverGateReachesFrontier(nil, false, goFrontier) {
		t.Fatal("a repository-scoped run has no frontier to gate on")
	}
}
