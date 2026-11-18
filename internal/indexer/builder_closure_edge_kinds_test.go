package indexer

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"slices"
	"strconv"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// TestClosureCarriedEdgeKindsMatchThePredicate pins the endpoint projections'
// kind list to closureCarriesEdge: every EdgeKind constant the graph package
// declares is in the list exactly when the predicate carries it. The constants
// are read from the graph package's source, so a kind added there is covered
// without editing this test.
func TestClosureCarriedEdgeKindsMatchThePredicate(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, filepath.Join("..", "graph", "edge.go"), nil, 0)
	if err != nil {
		t.Fatalf("parse the graph package's edge kinds: %v", err)
	}
	var kinds []graph.EdgeKind
	ast.Inspect(file, func(n ast.Node) bool {
		spec, ok := n.(*ast.ValueSpec)
		if !ok {
			return true
		}
		if ident, ok := spec.Type.(*ast.Ident); !ok || ident.Name != "EdgeKind" {
			return true
		}
		for _, value := range spec.Values {
			if lit, ok := value.(*ast.BasicLit); ok && lit.Kind == token.STRING {
				if v, err := strconv.Unquote(lit.Value); err == nil {
					kinds = append(kinds, graph.EdgeKind(v))
				}
			}
		}
		return true
	})
	if len(kinds) < 20 {
		t.Fatalf("read %d edge kinds from edge.go; the scan is broken", len(kinds))
	}
	for _, kind := range closureCarriedEdgeKinds {
		if !slices.Contains(kinds, kind) {
			t.Errorf("closureCarriedEdgeKinds lists %q, which edge.go does not declare", kind)
		}
	}
	for _, kind := range kinds {
		if got, want := slices.Contains(closureCarriedEdgeKinds, kind), closureCarriesEdge(kind); got != want {
			t.Errorf("kind %q: in closureCarriedEdgeKinds = %v, closureCarriesEdge = %v", kind, got, want)
		}
	}
}
