package resolver

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/zzet/gortex/internal/graph"
)

func TestResolveMethodCall_AmbiguousCandidatesPreserveEvidence(t *testing.T) {
	g := graph.New()
	file := "pkg/prov.go"
	g.AddNode(&graph.Node{ID: file, Kind: graph.KindFile, Name: "prov.go", FilePath: file, Language: "go"})

	var methodIDs []string
	for _, typ := range []string{"ScriptedDomainProvider", "ScriptedEmailProvider", "ScriptedDNSProvider"} {
		typeID := file + "::" + typ
		g.AddNode(&graph.Node{ID: typeID, Kind: graph.KindType, Name: typ, FilePath: file, Language: "go"})
		g.AddEdge(&graph.Edge{From: file, To: typeID, Kind: graph.EdgeDefines, FilePath: file})

		methodID := file + "::" + typ + ".recordSubmit"
		g.AddNode(&graph.Node{
			ID: methodID, Kind: graph.KindMethod, Name: "recordSubmit",
			FilePath: file, StartLine: 10, EndLine: 12, Language: "go",
			Meta: map[string]any{"receiver": typ},
		})
		g.AddEdge(&graph.Edge{From: file, To: methodID, Kind: graph.EdgeDefines, FilePath: file})
		g.AddEdge(&graph.Edge{From: methodID, To: typeID, Kind: graph.EdgeMemberOf, FilePath: file})
		methodIDs = append(methodIDs, methodID)
	}

	callerID := file + "::scriptedSubmission.submit"
	g.AddNode(&graph.Node{
		ID: callerID, Kind: graph.KindMethod, Name: "submit",
		FilePath: file, StartLine: 30, EndLine: 34, Language: "go",
		Meta: map[string]any{"receiver": "scriptedSubmission"},
	})
	g.AddEdge(&graph.Edge{From: file, To: callerID, Kind: graph.EdgeDefines, FilePath: file})

	const placeholderID = "unresolved::*.recordSubmit"
	callEdge := &graph.Edge{
		From: callerID, To: placeholderID,
		Kind: graph.EdgeCalls, FilePath: file, Line: 32,
	}
	g.AddEdge(callEdge)

	stats := New(g).ResolveAll()

	assert.Equal(t, 0, stats.Resolved)
	assert.Equal(t, 1, stats.Unresolved)
	assert.Equal(t, placeholderID, callEdge.To)
	assert.Empty(t, callEdge.Origin)
	assert.Nil(t, callEdge.Meta["dispatch"])
	assert.Len(t, g.GetInEdges(placeholderID), 1)

	for _, methodID := range methodIDs {
		for _, edge := range g.GetInEdges(methodID) {
			assert.NotEqual(t, graph.EdgeCalls, edge.Kind, "ambiguous call must not pick %s", methodID)
		}
		assert.Equal(t, graph.ZeroEdgeCoverageIncomplete, graph.ClassifyZeroEdge(g, methodID))
	}
}

func TestResolveMethodCall_AmbiguousMethodsWithFunctionStayUnresolved(t *testing.T) {
	g := graph.New()
	file := "pkg/call.go"
	g.AddNode(&graph.Node{ID: file, Kind: graph.KindFile, Name: "call.go", FilePath: file, Language: "go"})
	g.AddNode(&graph.Node{ID: file + "::caller", Kind: graph.KindFunction, Name: "caller", FilePath: file, Language: "go"})

	for _, typ := range []string{"First", "Second"} {
		g.AddNode(&graph.Node{
			ID: file + "::" + typ + ".run", Kind: graph.KindMethod, Name: "run",
			FilePath: file, Language: "go", Meta: map[string]any{"receiver": typ},
		})
	}
	g.AddNode(&graph.Node{ID: file + "::run", Kind: graph.KindFunction, Name: "run", FilePath: file, Language: "go"})

	const placeholderID = "unresolved::*.run"
	callEdge := &graph.Edge{
		From: file + "::caller", To: placeholderID,
		Kind: graph.EdgeCalls, FilePath: file, Line: 10,
	}
	g.AddEdge(callEdge)

	stats := New(g).ResolveAll()

	assert.Equal(t, 0, stats.Resolved)
	assert.Equal(t, 1, stats.Unresolved)
	assert.Equal(t, placeholderID, callEdge.To,
		"a same-named function is not receiver evidence for choosing either method")
}
