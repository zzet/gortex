package resolver

import (
	"slices"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// factoryChainPriorGraph is a builder chain across files: New() returns a
// Builder whose Build returns a Widget. main.go calls Widget.Missing, which no
// file declares, through the chain.
func factoryChainPriorGraph() (graph.Store, *graph.Edge) {
	g := graph.New()
	add := func(n *graph.Node) {
		n.RepoPrefix, n.Language = "app", "go"
		g.AddNode(n)
	}
	add(fn("a.go::New", "New", "a.go", "Builder"))
	add(&graph.Node{ID: "a.go::Builder", Kind: graph.KindType, Name: "Builder", FilePath: "a.go"})
	add(method("a.go::Builder.Build", "Build", "a.go", "Builder", "Widget"))
	add(&graph.Node{ID: "a.go::Widget", Kind: graph.KindType, Name: "Widget", FilePath: "a.go"})
	add(fn("main.go::main", "main", "main.go", ""))
	missing := &graph.Edge{
		From: "main.go::main", To: "unresolved::*.Missing", Kind: graph.EdgeCalls,
		FilePath: "main.go", Line: 7, Meta: map[string]any{"receiver_expr": "New().Build()"},
	}
	g.AddEdge(missing)
	return g, missing
}

func captureFactoryChainPrior(g graph.Store, file string) *FactoryChainPrior {
	prior := NewFactoryChainPrior()
	nodes := g.GetFileNodes(file)
	ids := make([]string, 0, len(nodes))
	for _, n := range nodes {
		ids = append(ids, n.ID)
	}
	prior.AddFile(file, nodes, g.GetOutEdgesByNodeIDs(ids))
	return prior
}

func runFactoryChainScoped(g graph.Store, prior *FactoryChainPrior) {
	RunFrameworkSynthesizersScopedForFilesWithSelection(
		g, map[string]bool{"app": true}, []string{"main.go"}, false,
		AllFrameworkSynthesizers().WithFactoryChainPrior(prior),
	)
}

// A save that keeps every declaration of the file does not walk a chain that
// failed before; a save that declares what the chain was missing walks it and
// resolves it.
func TestFactoryChainPriorSkipsOnlyChainsNothingCouldHaveMoved(t *testing.T) {
	g, missing := factoryChainPriorGraph()
	prior := captureFactoryChainPrior(g, "main.go")

	// A body-only save: the chain moves a line, the file declares the same.
	missing.Line = 8
	before := FactoryChainPriorSkipped()
	runFactoryChainScoped(g, prior)
	if FactoryChainPriorSkipped()-before != 1 {
		t.Fatalf("a declaration-preserving save walked the chain that failed before (skipped %d)", FactoryChainPriorSkipped()-before)
	}
	if !graph.IsUnresolvedTarget(missing.To) {
		t.Fatalf("the skipped chain moved to %s", missing.To)
	}

	// The save declares Widget.Missing in the changed file: the chain now
	// resolves and must be walked.
	m := method("main.go::Widget.Missing", "Missing", "main.go", "Widget", "")
	m.RepoPrefix, m.Language = "app", "go"
	g.AddNode(m)
	before = FactoryChainPriorSkipped()
	runFactoryChainScoped(g, prior)
	if FactoryChainPriorSkipped() != before {
		t.Fatal("a save that added a declaration skipped the chains of its file")
	}
	if missing.To != "main.go::Widget.Missing" {
		t.Fatalf("the chain resolved to %q, want main.go::Widget.Missing", missing.To)
	}
}

// A change carrying no unresolved chain with a receiver expression does not
// run the factory-chain pass; one carrying such a chain runs it.
func TestScopedFactoryChainPassIsCandidateGated(t *testing.T) {
	g, _ := factoryChainPriorGraph()
	noChain := RunFrameworkSynthesizersScopedForFiles(g, map[string]bool{"app": true}, []string{"a.go"}, false)
	if !slices.Contains(noChain.CandidateGated, SynthFactoryChain) {
		t.Fatalf("a change with no factory chain ran the factory-chain pass (gated: %v)", noChain.CandidateGated)
	}
	withChain := RunFrameworkSynthesizersScopedForFiles(g, map[string]bool{"app": true}, []string{"main.go"}, false)
	if slices.Contains(withChain.CandidateGated, SynthFactoryChain) {
		t.Fatal("a change carrying a factory chain did not run the factory-chain pass")
	}
}
