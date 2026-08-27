package analysis

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/zzet/gortex/internal/graph"
)

func TestDeadCode_UnresolvedSameNameCandidatesAreCoverageGaps(t *testing.T) {
	g := graph.New()
	file := "pkg/prov.go"
	g.AddNode(&graph.Node{ID: file, Kind: graph.KindFile, Name: "prov.go", FilePath: file, Language: "go"})

	var ambiguousMethodIDs []string
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
		ambiguousMethodIDs = append(ambiguousMethodIDs, methodID)
	}

	callerID := file + "::scriptedSubmission.submit"
	g.AddNode(&graph.Node{
		ID: callerID, Kind: graph.KindMethod, Name: "submit",
		FilePath: file, StartLine: 30, EndLine: 34, Language: "go",
		Meta: map[string]any{"receiver": "scriptedSubmission"},
	})
	g.AddEdge(&graph.Edge{From: file, To: callerID, Kind: graph.EdgeDefines, FilePath: file})
	g.AddEdge(&graph.Edge{
		From: callerID, To: "unresolved::*.recordSubmit",
		Kind: graph.EdgeCalls, FilePath: file, Line: 32,
	})

	uniqueTypeID := file + "::UniqueProvider"
	uniqueMethodID := uniqueTypeID + ".uniqueUnused"
	g.AddNode(&graph.Node{ID: uniqueTypeID, Kind: graph.KindType, Name: "UniqueProvider", FilePath: file, Language: "go"})
	g.AddNode(&graph.Node{
		ID: uniqueMethodID, Kind: graph.KindMethod, Name: "uniqueUnused",
		FilePath: file, StartLine: 40, EndLine: 42, Language: "go",
		Meta: map[string]any{"receiver": "UniqueProvider"},
	})
	g.AddEdge(&graph.Edge{From: file, To: uniqueTypeID, Kind: graph.EdgeDefines, FilePath: file})
	g.AddEdge(&graph.Edge{From: file, To: uniqueMethodID, Kind: graph.EdgeDefines, FilePath: file})
	g.AddEdge(&graph.Edge{From: uniqueMethodID, To: uniqueTypeID, Kind: graph.EdgeMemberOf, FilePath: file})

	result := FindDeadCode(g, nil, nil)
	reported := make(map[string]bool, len(result))
	for _, entry := range result {
		reported[entry.ID] = true
	}

	for _, methodID := range ambiguousMethodIDs {
		assert.False(t, reported[methodID], "unresolved same-name evidence must suppress %s", methodID)
	}
	assert.True(t, reported[uniqueMethodID], "a uniquely named unused method must remain reportable")
}
