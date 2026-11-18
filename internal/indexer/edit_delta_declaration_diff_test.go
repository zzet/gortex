package indexer

import (
	"fmt"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// declFile builds a file's rows: a file node, an import, and n functions of
// three lines each (each with a call edge), starting at line 10+shift; names
// renames function i when set, and typed makes function 0 a type.
func declFile(n, shift int, rename map[int]string, typed bool, extraImport bool) ([]*graph.Node, []*graph.Edge) {
	const path = "repo/a.go"
	nodes := []*graph.Node{{ID: path, Kind: graph.KindFile, FilePath: path}}
	edges := []*graph.Edge{{From: path, To: path + "::import::fmt", Kind: graph.EdgeImports, FilePath: path, Line: 3}}
	if extraImport {
		edges = append(edges, &graph.Edge{From: path, To: path + "::import::os", Kind: graph.EdgeImports, FilePath: path, Line: 4})
	}
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("F%d", i)
		if r, ok := rename[i]; ok {
			name = r
		}
		kind := graph.KindFunction
		if typed && i == 0 {
			kind = graph.KindType
		}
		start := 10 + shift + 4*i
		id := path + "::" + name
		nodes = append(nodes, &graph.Node{ID: id, Name: name, Kind: kind, FilePath: path, StartLine: start, EndLine: start + 2})
		edges = append(edges, &graph.Edge{From: id, To: "repo/b.go::Helper", Kind: graph.EdgeCalls, FilePath: path, Line: start + 1})
	}
	return nodes, edges
}

// The declaration diff of a save counts what a declaration-granular form
// would write and says whether the save qualifies for it: a one-function
// rename does; an import change, a type change, a line shift across the file
// and a row no declaration owns each fall back, naming why.
func TestTheDeclarationDiffSaysWhichSavesQualify(t *testing.T) {
	priorNodes, priorEdges := declFile(10, 0, nil, false, false)

	bigNodes, bigEdges := declFile(30, 0, nil, false, false)
	rn, re := declFile(30, 0, map[int]string{4: "Renamed"}, false, false)
	// A placeholder-sourced row inside F3's span that the save changes is
	// F3's: F3 changes, and the save still qualifies.
	site := func(to string) *graph.Edge {
		return &graph.Edge{From: "unresolved::v", To: to, Kind: graph.EdgeValueFlow, FilePath: "repo/a.go", Line: 23}
	}
	rename := diffDeclarations(bigNodes, append(bigEdges, site("unresolved::w")),
		rn, append(re, site("unresolved::z")))
	if !rename.Qualifies || rename.Added != 1 || rename.Removed != 1 || rename.Changed != 1 {
		t.Fatalf("a one-function rename: %+v", rename)
	}

	in, ie := declFile(10, 0, nil, false, true)
	if d := diffDeclarations(priorNodes, priorEdges, in, ie); d.Qualifies || d.Reason != "file_rows" {
		t.Fatalf("an import change: %+v", d)
	}
	tpNodes, tpEdges := declFile(10, 0, nil, true, false)
	tn, te := declFile(10, 0, nil, true, false)
	// The type's body changes under the same identity.
	for _, e := range te {
		if e.From == "repo/a.go::F0" {
			e.To = "repo/b.go::Other"
		}
	}
	if d := diffDeclarations(tpNodes, tpEdges, tn, te); d.Qualifies || d.Reason != "type_declaration" {
		t.Fatalf("a type change: %+v", d)
	}
	sn, se := declFile(10, 1, nil, false, false)
	if d := diffDeclarations(priorNodes, priorEdges, sn, se); d.Qualifies || d.Reason != "threshold" {
		t.Fatalf("a line shift across the file: %+v", d)
	}
	un, ue := declFile(10, 0, nil, false, false)
	ue = append(ue, &graph.Edge{From: "unresolved::x", To: "unresolved::y", Kind: graph.EdgeValueFlow, FilePath: "repo/a.go", Line: 200})
	if d := diffDeclarations(priorNodes, priorEdges, un, ue); d.Qualifies || d.Reason != "unowned_rows" {
		t.Fatalf("a row no declaration owns: %+v", d)
	}
}
