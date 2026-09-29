package indexer

import (
	"slices"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// TestGroupedResultsOfOneTypeAreNotAShapeChange pins the target-side shape of a
// declaration whose results share a type on one line: the store keeps one
// `returns string` edge for `(a, b string, err error)` (one edge identity),
// the extraction emits two, and comparing the raw extraction with the stored
// shape reported a signature change on every edit of the file — so every
// caller of the function was re-derived as a dependent.
func TestGroupedResultsOfOneTypeAreNotAShapeChange(t *testing.T) {
	treeA := map[string]string{
		"split.go":  "package fixture\n\nfunc Split(s string) (a, b string, err error) { return s, s, nil }\n",
		"caller.go": "package fixture\n\nfunc Caller() string { x, _, _ := Split(\"v\"); return x }\n",
	}
	treeB := map[string]string{
		"split.go":  "package fixture\n\nfunc Split(s string) (a, b string, err error) {\n\t// body-only edit\n\treturn s, s, nil\n}\n",
		"caller.go": treeA["caller.go"],
	}
	c := buildClosureCase(t, treeA, treeB)
	if slices.Contains(c.report.ClosurePaths, "caller.go") {
		t.Errorf("a body edit of a grouped-results function re-derives its caller: closure %v", c.report.ClosurePaths)
	}
	builderAssertReadersAgree(t, c.composed, c.flat)
}

// TestStoredExtractionIdentitiesCollapseLikeTheStore pins the normalization
// itself: one node per id and one edge per identity, the later one winning.
func TestStoredExtractionIdentitiesCollapseLikeTheStore(t *testing.T) {
	first := &graph.Edge{From: "f", To: "string", Kind: graph.EdgeReturns, FilePath: "x.go", Line: 3, Meta: map[string]any{"position": 0}}
	second := &graph.Edge{From: "f", To: "string", Kind: graph.EdgeReturns, FilePath: "x.go", Line: 3, Meta: map[string]any{"position": 1}}
	other := &graph.Edge{From: "f", To: "error", Kind: graph.EdgeReturns, FilePath: "x.go", Line: 3, Meta: map[string]any{"position": 2}}
	n1 := &graph.Node{ID: "v", Name: "v", Kind: graph.KindVariable}
	n2 := &graph.Node{ID: "v", Name: "v", Kind: graph.KindVariable, Meta: map[string]any{"later": true}}
	nodes, edges := storedExtractionIdentities([]*graph.Node{n1, n2}, []*graph.Edge{first, other, second})
	if len(nodes) != 1 || nodes[0] != n2 {
		t.Fatalf("nodes = %v, want the later node with id v only", nodes)
	}
	if len(edges) != 2 || edges[0] != second || edges[1] != other {
		t.Fatalf("edges = %v, want [second other]", edges)
	}
}
