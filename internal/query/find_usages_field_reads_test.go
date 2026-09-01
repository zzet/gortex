package query

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

// The C# field-identifier emitter (parser: read/write edges for C# field
// identifiers) lands resolved EdgeReads/EdgeWrites on field nodes — but
// find_usages filtered both kinds out, so a field provably read inside
// its own class still answered empty: the store told the authored story
// (one read, one write) while the usages view served zero rows. The
// production repro was a repository field read as a call receiver inside
// a ctor-initializer lambda, resolved `reads` edge present, usages 0.
// reads/writes ARE the value-side usage story for variables and fields
// (edge.go's own EdgeReads doc); EdgeAccessesField stays excluded — it
// is the synthesized union of the two riding the same sites and would
// double-count every one of them.
func TestFindUsages_IncludesFieldReadsAndWrites(t *testing.T) {
	g := graph.New()
	field := "Svc.cs::Svc._lookup"
	ctor := "Svc.cs::Svc.<init>"
	reader := "Svc.cs::Svc.Report"

	g.AddNode(&graph.Node{ID: "Svc.cs", Kind: graph.KindFile, Name: "Svc.cs", Language: "csharp"})
	g.AddNode(&graph.Node{ID: field, Kind: graph.KindField, Name: "_lookup", FilePath: "Svc.cs", Language: "csharp"})
	g.AddNode(&graph.Node{ID: ctor, Kind: graph.KindMethod, Name: "<init>", FilePath: "Svc.cs", Language: "csharp"})
	g.AddNode(&graph.Node{ID: reader, Kind: graph.KindMethod, Name: "Report", FilePath: "Svc.cs", Language: "csharp"})

	g.AddEdge(&graph.Edge{From: ctor, To: field, Kind: graph.EdgeWrites, FilePath: "Svc.cs", Line: 10})
	g.AddEdge(&graph.Edge{From: reader, To: field, Kind: graph.EdgeReads, FilePath: "Svc.cs", Line: 40})
	// The synthesized union rides alongside the split edges at the SAME
	// sites — it must not add duplicate usage rows.
	g.AddEdge(&graph.Edge{From: ctor, To: field, Kind: graph.EdgeAccessesField, FilePath: "Svc.cs", Line: 10})
	g.AddEdge(&graph.Edge{From: reader, To: field, Kind: graph.EdgeAccessesField, FilePath: "Svc.cs", Line: 40})

	e := NewEngine(g)
	usages := e.FindUsages(field)

	require.Len(t, usages.Edges, 2, "one write + one read, no accesses_field duplicates")
	kinds := map[graph.EdgeKind]int{}
	for _, ed := range usages.Edges {
		kinds[ed.Kind]++
	}
	assert.Equal(t, 1, kinds[graph.EdgeWrites], "the ctor assignment is a usage")
	assert.Equal(t, 1, kinds[graph.EdgeReads], "the lambda-body read is a usage")
}
