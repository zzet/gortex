package resolver

import (
	"slices"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// A scoped run over a file that carries no gRPC stub, registration or stub
// bound into it does not run the gRPC pass; a run over the files with the stub
// and its registration runs it, as before.
func TestScopedFrameworkRunSkipsPassesTheChangeCarriesNoCandidateFor(t *testing.T) {
	b := newGRPCTestGraph()
	b.addCaller("cli/main.go::run", "cli/main.go", "cli")
	call := b.addStubCall("cli/main.go::run", "UserService", "GetUser", "cli/main.go")
	methods := b.addServerImpl("userServer", "svc", "svc", "GetUser")
	_ = methods
	b.addRegistration("UserService", "userServer", "svc/main.go::main", "svc/main.go", "svc")
	b.g.AddNode(&graph.Node{ID: "cli/config.go::Load", Kind: graph.KindFunction, Name: "Load", FilePath: "cli/config.go", RepoPrefix: "cli", Language: "go"})
	b.g.AddNode(&graph.Node{ID: "cli/config.go", Kind: graph.KindFile, Name: "config.go", FilePath: "cli/config.go", RepoPrefix: "cli", Language: "go"})
	for _, n := range b.g.(*graph.Graph).AllNodes() {
		n.Language = "go"
	}

	unrelated := RunFrameworkSynthesizersScopedForFiles(b.g, map[string]bool{"cli": true}, []string{"cli/config.go"}, false)
	if !slices.Contains(unrelated.CandidateGated, SynthGRPCStub) {
		t.Fatalf("a change with no gRPC row ran the gRPC pass (gated: %v)", unrelated.CandidateGated)
	}
	if call.To == methods["GetUser"] {
		t.Fatal("the fixture's stub was bound before its own file changed")
	}

	withStub := RunFrameworkSynthesizersScopedForFiles(b.g, map[string]bool{"cli": true, "svc": true}, []string{"cli/main.go", "svc/main.go"}, false)
	if slices.Contains(withStub.CandidateGated, SynthGRPCStub) {
		t.Fatal("a change carrying a gRPC stub did not run the gRPC pass")
	}
	ran := false
	for _, row := range withStub.Per {
		if row.Name == SynthGRPCStub && row.ScopeRows > 0 {
			ran = true
		}
	}
	if !ran {
		t.Fatal("the gRPC pass did not read the change that carries its stub")
	}
}

// A composite literal's keyed fields are executor-field markers whether or not
// any Temporal dispatch reads them: a change whose only temporal rows are
// markers (a config struct literal) does not run the Temporal pass, while a
// change that carries the dispatch stub reading the field runs it and joins
// the marker's literal.
func TestScopedTemporalPassSkipsAChangeCarryingOnlyExecutorFieldMarkers(t *testing.T) {
	g := graph.New()
	addFunc := func(id, file string) {
		g.AddNode(&graph.Node{ID: id, Kind: graph.KindFunction, Name: lastSeg(id), FilePath: file, RepoPrefix: "app", Language: "go"})
	}
	marker := func(from, file, typ, field, value string, line int) {
		g.AddEdge(&graph.Edge{
			From: from, To: "unresolved::temporal-executor::" + typ + "::" + field,
			Kind: graph.EdgeCalls, FilePath: file, Line: line,
			Meta: map[string]any{"via": temporalExecutorFieldVia, "executor_type": typ, "executor_field": field, "executor_value": value},
		})
	}
	addFunc("app/config.go::Default", "app/config.go")
	marker("app/config.go::Default", "app/config.go", "MCPConfig", "Transport", "stdio", 10)

	onlyMarkers := RunFrameworkSynthesizersScopedForFiles(g, map[string]bool{"app": true}, []string{"app/config.go"}, false)
	if !slices.Contains(onlyMarkers.CandidateGated, SynthTemporalStub) {
		t.Fatalf("a change carrying only executor-field markers ran the Temporal pass (witness %v)", onlyMarkers.CandidateWitness)
	}

	addFunc("app/exec.go::Build", "app/exec.go")
	addFunc("app/exec.go::ActivityExecutor.Run", "app/exec.go")
	marker("app/exec.go::Build", "app/exec.go", "ActivityExecutor", "ActivityName", "ChargeCard", 5)
	stub := &graph.Edge{
		From: "app/exec.go::ActivityExecutor.Run", To: "unresolved::temporal::activity::ActivityName",
		Kind: graph.EdgeCalls, FilePath: "app/exec.go", Line: 20,
		Meta: map[string]any{
			"via": "temporal.stub", "temporal_kind": "activity", "temporal_name": "ActivityName",
			"temporal_name_field": "ActivityName", "temporal_recv_type": "ActivityExecutor",
		},
	}
	g.AddEdge(stub)
	withStub := RunFrameworkSynthesizersScopedForFiles(g, map[string]bool{"app": true}, []string{"app/exec.go"}, false)
	if slices.Contains(withStub.CandidateGated, SynthTemporalStub) {
		t.Fatal("a change carrying a Temporal dispatch stub did not run the Temporal pass")
	}
	if witness := withStub.CandidateWitness[SynthTemporalStub]; witness == "" {
		t.Fatal("the Temporal pass ran without naming the row that selected it")
	}
	joined := false
	for _, e := range g.GetOutEdges("app/exec.go::ActivityExecutor.Run") {
		if e.Meta != nil && e.Meta["temporal_name"] == "ChargeCard" {
			joined = true
		}
	}
	if !joined {
		t.Fatal("the Temporal pass did not join the marker's literal into the stub it dispatches")
	}
}
