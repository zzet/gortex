package indexer

import (
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// A declaration's rows are compared by the identity the save states: the
// view below is fully enriched while a delta is measured before its own
// enrichment, so metadata, confidence, origin and tier differ on rows the
// save did not touch; and the file's defines edge to a renamed function is
// the function's. A one-function rename of a file whose every row carries
// such metadata changes that function alone and qualifies.
func TestTheDeclarationDiffComparesRowIdentityNotEnrichment(t *testing.T) {
	const path = "repo/a.go"
	build := func(renamed string, enriched bool) ([]*graph.Node, []*graph.Edge) {
		nodes := []*graph.Node{{ID: path, Kind: graph.KindFile, Name: "a.go", FilePath: path, StartLine: 1, EndLine: 40}}
		var edges []*graph.Edge
		for i, name := range []string{"Alpha", "Beta", "Gamma", "Delta", "Eps", "Zeta", "Eta", "Theta", "Iota", "Kappa", "Lambda", "Mu"} {
			if i == 1 && renamed != "" {
				name = renamed
			}
			id := path + "::" + name
			start := 10 + 5*i
			n := &graph.Node{ID: id, Kind: graph.KindFunction, Name: name, FilePath: path, StartLine: start, EndLine: start + 3}
			call := &graph.Edge{From: id, To: "repo/b.go::Helper", Kind: graph.EdgeCalls, FilePath: path, Line: start + 1}
			if enriched {
				n.Meta = map[string]any{"type_confirmed": true, "source_derived_decl_fingerprint": "x"}
				call.Confidence, call.Origin = 1, "lsp_resolved"
			}
			nodes = append(nodes, n)
			edges = append(edges, &graph.Edge{From: path, To: id, Kind: graph.EdgeDefines, FilePath: path, Line: start}, call)
		}
		if enriched {
			nodes[0].Meta = map[string]any{"content_hash": "old"}
		}
		return nodes, edges
	}
	priorNodes, priorEdges := build("", true)
	nextNodes, nextEdges := build("BetaRen1", false)
	d := diffDeclarations(priorNodes, priorEdges, nextNodes, nextEdges)
	if !d.Qualifies || d.Changed != 0 || d.Added != 1 || d.Removed != 1 || len(d.Reasons) != 0 {
		t.Fatalf("a one-function rename over an enriched view: %+v", d)
	}
	// The renamed function's three rows written, its three prior rows
	// tombstoned, of the file's 37.
	if d.ChangedRows != 6 || d.TotalRows != 37 {
		t.Fatalf("rows written: changed %d of %d, want 6 of 37", d.ChangedRows, d.TotalRows)
	}
	// A body edit inside Gamma (its call moves a line, the span stays)
	// changes Gamma alone: its three rows are written once.
	bodyNodes, bodyEdges := build("", false)
	for _, e := range bodyEdges {
		if e.From == path+"::Gamma" && e.Kind == graph.EdgeCalls {
			e.Line++
		}
	}
	body := diffDeclarations(priorNodes, priorEdges, bodyNodes, bodyEdges)
	if !body.Qualifies || body.Changed != 1 || body.Added != 0 || body.Removed != 0 || body.ChangedRows != 3 {
		t.Fatalf("a one-function body edit over an enriched view: %+v", body)
	}
}

// A save that falls back for several reasons reports all of them, in a fixed
// order, and the same first reason every time.
func TestTheDeclarationDiffReasonIsDeterministic(t *testing.T) {
	const path = "repo/a.go"
	build := func(shift int) ([]*graph.Node, []*graph.Edge) {
		nodes := []*graph.Node{{ID: path, Kind: graph.KindFile, FilePath: path}}
		var edges []*graph.Edge
		for i, d := range []struct {
			name string
			kind graph.NodeKind
		}{{"T", graph.KindType}, {"V", graph.KindVariable}, {"C", graph.KindConstant}, {"U", graph.KindType}, {"W", graph.KindVariable}} {
			id := path + "::" + d.name
			start := 10 + 4*i
			nodes = append(nodes, &graph.Node{ID: id, Kind: d.kind, Name: d.name, FilePath: path, StartLine: start, EndLine: start + 2})
			edges = append(edges, &graph.Edge{From: id, To: "repo/b.go::X", Kind: graph.EdgeReferences, FilePath: path, Line: start + 1 + shift})
		}
		return nodes, edges
	}
	priorNodes, priorEdges := build(0)
	nextNodes, nextEdges := build(1)
	want := []string{"type_declaration", "package_level_declaration", "threshold"}
	for run := 0; run < 50; run++ {
		d := diffDeclarations(priorNodes, priorEdges, nextNodes, nextEdges)
		if d.Reason != "type_declaration" || len(d.Reasons) != len(want) {
			t.Fatalf("run %d: reason %q, reasons %v, want %v", run, d.Reason, d.Reasons, want)
		}
		for i := range want {
			if d.Reasons[i] != want[i] {
				t.Fatalf("run %d: reasons %v, want %v", run, d.Reasons, want)
			}
		}
	}
}
