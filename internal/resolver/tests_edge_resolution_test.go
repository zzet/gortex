package resolver

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

// A tests edge is DERIVED: the test-linkage pass clones a test caller's
// calls edges, meta-free. Routing such a clone through call resolution
// re-runs the bind WITHOUT the original's receiver evidence, bypassing
// every receiver-gated guard. Field shape: a `List<int>` receiver whose
// calls edge the extension shape guard correctly refuses (`this
// List<string>` conflicts) - the naked tests clone of the same site was
// bound by the untyped pool-unique fallback at 0.75. The resolver must
// never bind a tests edge; the tests layer follows its calls edge.
func TestResolveAll_NeverBindsATestsEdge(t *testing.T) {
	g := buildCSharpResolverGraph(t, map[string]string{
		"SpecGenArgs.cs": `using System.Collections.Generic;

namespace Probe.Spec.GenArgs {
    public static class ListStretch {
        public static int Total(this List<string> xs, int pad) { return pad; }
    }

    public class GenRunner {
        public int Run() {
            var xs = new List<int>();
            return xs.Total(3);
        }
    }
}`,
	})

	callerID := "SpecGenArgs.cs::GenRunner.Run"
	// The clone the test-linkage pass would have minted while the call
	// was unresolved: same site, no receiver meta.
	g.AddEdge(&graph.Edge{
		From: callerID, To: "unresolved::*.Total", Kind: graph.EdgeTests,
		FilePath: "SpecGenArgs.cs", Line: 11, Origin: graph.OriginASTInferred,
	})

	New(g).ResolveAll()

	var testsEdge *graph.Edge
	for _, e := range g.GetOutEdges(callerID) {
		if e != nil && e.Kind == graph.EdgeTests {
			testsEdge = e
		}
	}
	require.NotNil(t, testsEdge, "fixture: the tests clone must survive resolution")
	assert.True(t, graph.IsUnresolvedTarget(testsEdge.To),
		"the resolver bound a derived tests edge (to %s) - the naked clone bypasses the receiver-gated guards", testsEdge.To)

	// Control: the CALLS edge at the same site keeps its own verdict -
	// the shape guard refuses the List<int> vs `this List<string>`
	// conflict, so it stays honestly unresolved too, WITH its receiver
	// evidence intact.
	for _, e := range g.GetOutEdges(callerID) {
		if e != nil && e.Kind == graph.EdgeCalls && e.Line == 11 {
			assert.True(t, graph.IsUnresolvedTarget(e.To),
				"control drifted: the guarded calls edge bound to %s", e.To)
		}
	}
}

// The refusal must hold on EVERY resolver path, not just the heuristic
// cascade: the inline LSP hot-path answers by (file, line, name) - exactly
// the receiver-evidence-free lookup the tests clone must never take.
func TestResolveFile_LSPNeverBindsATestsEdge(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "src/spec.ts", Kind: graph.KindFile, Name: "spec.ts", FilePath: "src/spec.ts", Language: "typescript"})
	g.AddNode(&graph.Node{
		ID: "src/spec.ts::specRun", Kind: graph.KindFunction, Name: "specRun",
		FilePath: "src/spec.ts", StartLine: 3, EndLine: 5, Language: "typescript",
	})
	g.AddNode(&graph.Node{
		ID: "src/real.ts::doWork", Kind: graph.KindFunction, Name: "doWork",
		FilePath: "src/real.ts", StartLine: 7, EndLine: 9, Language: "typescript",
	})
	testsEdge := &graph.Edge{
		From: "src/spec.ts::specRun", To: "unresolved::doWork",
		Kind: graph.EdgeTests, FilePath: "src/spec.ts", Line: 4,
	}
	g.AddEdge(testsEdge)

	helper := &fakeLSPHelper{
		exts: []string{".ts"},
		defs: map[lspKey]lspAnswer{
			{path: "src/spec.ts", line: 4, name: "doWork"}: {defPath: "src/real.ts", defLine: 7},
		},
	}
	r := New(g)
	r.SetLSPHelper(helper)
	r.ResolveFile("src/spec.ts")

	assert.True(t, graph.IsUnresolvedTarget(testsEdge.To),
		"the inline LSP path bound a derived tests edge (to %s)", testsEdge.To)
}

// Bulk-mode ResolveAll defers LSP lookups to a post-loop batch collected
// BEFORE resolveEdge sees the edge - the batch must refuse tests clones too.
func TestResolveAll_DeferredLSPNeverBindsATestsEdge(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "src/spec.ts", Kind: graph.KindFile, Name: "spec.ts", FilePath: "src/spec.ts", Language: "typescript"})
	g.AddNode(&graph.Node{
		ID: "src/spec.ts::specRun", Kind: graph.KindFunction, Name: "specRun",
		FilePath: "src/spec.ts", StartLine: 3, EndLine: 5, Language: "typescript",
	})
	g.AddNode(&graph.Node{
		ID: "src/real.ts::doWork", Kind: graph.KindFunction, Name: "doWork",
		FilePath: "src/real.ts", StartLine: 7, EndLine: 9, Language: "typescript",
	})
	testsEdge := &graph.Edge{
		From: "src/spec.ts::specRun", To: "unresolved::doWork",
		Kind: graph.EdgeTests, FilePath: "src/spec.ts", Line: 4,
	}
	g.AddEdge(testsEdge)

	helper := &fakeLSPHelper{
		exts: []string{".ts"},
		defs: map[lspKey]lspAnswer{
			{path: "src/spec.ts", line: 4, name: "doWork"}: {defPath: "src/real.ts", defLine: 7},
		},
	}
	r := New(g)
	r.SetLSPHelper(helper)
	r.ResolveAll()

	assert.True(t, graph.IsUnresolvedTarget(testsEdge.To),
		"the deferred LSP batch bound a derived tests edge (to %s)", testsEdge.To)
}

// CrossRepoResolver has its own independent resolveEdge; the same-repo
// name tier would happily bind the naked clone by bare name.
func TestCrossRepoResolveAll_NeverBindsATestsEdge(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "repoA/pkg/a_test.go::TestCaller", Kind: graph.KindFunction, Name: "TestCaller", FilePath: "repoA/pkg/a_test.go", Language: "go", RepoPrefix: "repoA"})
	g.AddNode(&graph.Node{ID: "repoA/pkg/b.go::Helper", Kind: graph.KindFunction, Name: "Helper", FilePath: "repoA/pkg/b.go", Language: "go", RepoPrefix: "repoA"})

	testsEdge := &graph.Edge{From: "repoA/pkg/a_test.go::TestCaller", To: "unresolved::Helper", Kind: graph.EdgeTests, FilePath: "repoA/pkg/a_test.go", Line: 5}
	g.AddEdge(testsEdge)

	cr := NewCrossRepo(g)
	cr.ResolveAll()

	assert.True(t, graph.IsUnresolvedTarget(testsEdge.To),
		"the cross-repository pass bound a derived tests edge (to %s)", testsEdge.To)
}
