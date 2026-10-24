package languages

import (
	"fmt"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
)

// importEdge finds an edge of the given kind whose To target matches and
// returns it (or nil). target is matched exactly.
func importEdge(res *parser.ExtractionResult, kind graph.EdgeKind, to string) *graph.Edge {
	for _, e := range res.Edges {
		if e.Kind == kind && e.To == to {
			return e
		}
	}
	return nil
}

func assertPerBindingImports(t *testing.T, res *parser.ExtractionResult) {
	t.Helper()

	// Module-level dependency edge is preserved.
	if importEdge(res, graph.EdgeImports, "unresolved::import::mod") == nil {
		t.Error("module-level import edge to mod was dropped")
	}

	// Per-binding import edges: foo (no rename) and bar→baz (renamed).
	foo := importEdge(res, graph.EdgeImports, "unresolved::import::mod::foo")
	if foo == nil {
		t.Fatal("missing per-binding import edge for foo")
	}
	if foo.Alias != "" {
		t.Errorf("foo import should have no alias, got %q", foo.Alias)
	}
	bar := importEdge(res, graph.EdgeImports, "unresolved::import::mod::bar")
	if bar == nil {
		t.Fatal("missing per-binding import edge for bar")
	}
	if bar.Alias != "baz" {
		t.Errorf("bar import alias = %q, want baz", bar.Alias)
	}
	if bar.Meta["import_alias"] != "baz" {
		t.Errorf("persisted bar import alias = %v, want baz", bar.Meta["import_alias"])
	}
	if _, present := foo.Meta["import_alias"]; present {
		t.Error("an unrenamed import must not carry import_alias")
	}

	// Named re-export edges: a (no rename) and b→c (renamed exported name).
	a := importEdge(res, graph.EdgeReExports, "unresolved::import::up::a")
	if a == nil {
		t.Fatal("missing named re-export edge for a")
	}
	if a.Alias != "" {
		t.Errorf("re-export a should have no alias, got %q", a.Alias)
	}
	b := importEdge(res, graph.EdgeReExports, "unresolved::import::up::b")
	if b == nil {
		t.Fatal("missing named re-export edge for b")
	}
	if b.Alias != "c" {
		t.Errorf("re-export b alias = %q, want c", b.Alias)
	}

	// Namespace re-export: `export * as ns from "nsmod"`.
	ns := importEdge(res, graph.EdgeReExports, "unresolved::import::nsmod")
	if ns == nil {
		t.Fatal("missing namespace re-export edge for nsmod")
	}
	if ns.Alias != "ns" {
		t.Errorf("namespace re-export alias = %q, want ns", ns.Alias)
	}

	// Wildcard re-export: `export * from "all"` — module-level, no alias.
	all := importEdge(res, graph.EdgeReExports, "unresolved::import::all")
	if all == nil {
		t.Fatal("missing wildcard re-export edge for all")
	}
	if all.Alias != "" {
		t.Errorf("wildcard re-export should have no alias, got %q", all.Alias)
	}
}

const jstsImportFixture = `import { foo, bar as baz } from "mod";
export { a, b as c } from "up";
export * as ns from "nsmod";
export * from "all";
`

func TestTSExtractor_PerBindingImports(t *testing.T) {
	res, err := NewTypeScriptExtractor().Extract("a.ts", []byte(jstsImportFixture))
	if err != nil {
		t.Fatal(err)
	}
	assertPerBindingImports(t, res)
}

func TestJSExtractor_PerBindingImports(t *testing.T) {
	res, err := NewJavaScriptExtractor().Extract("a.js", []byte(jstsImportFixture))
	if err != nil {
		t.Fatal(err)
	}
	assertPerBindingImports(t, res)
}

// TestTSExtractor_PerBindingImportsVolumeGuard verifies the per-binding edges
// collapse to the module edge once a statement exceeds the binding cap, so a
// barrel file cannot explode the edge set.
func TestTSExtractor_PerBindingImportsVolumeGuard(t *testing.T) {
	var names []string
	for i := 0; i < jsImportBindingCap+5; i++ {
		names = append(names, fmt.Sprintf("n%d", i))
	}
	src := "import { " + strings.Join(names, ", ") + " } from \"big\";\n"
	res, err := NewTypeScriptExtractor().Extract("big.ts", []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	if importEdge(res, graph.EdgeImports, "unresolved::import::big") == nil {
		t.Error("module-level import edge to big was dropped under the volume guard")
	}
	for _, e := range res.Edges {
		if e.Kind == graph.EdgeImports && strings.HasPrefix(e.To, "unresolved::import::big::") {
			t.Fatalf("per-binding edge %q emitted past the volume guard", e.To)
		}
	}
}
