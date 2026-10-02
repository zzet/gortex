package resolver

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

func TestPythonModuleFileMatches(t *testing.T) {
	cases := []struct {
		path, module string
		want         bool
	}{
		{"shop/pricing.py", "shop.pricing", true},
		{"shop/pricing.pyi", "shop.pricing", true},
		{"shop/pricing/__init__.py", "shop.pricing", true},
		{"src/shop/pricing.py", "shop.pricing", true},
		{"repo/src/shop/pricing.py", "shop.pricing", true},
		{"pricing.py", "pricing", true},
		{"shop/pricing.py", "pricing", true}, // module reachable from a nested source root
		{"shop/mypricing.py", "shop.pricing", false},
		{"shop/pricing_util.py", "shop.pricing", false},
		{"other/pricing.py", "shop.pricing", false},
		{"shop/pricing.go", "shop.pricing", false},
		{"", "shop.pricing", false},
		{"shop/pricing.py", "", false},
	}
	for _, c := range cases {
		assert.Equal(t, c.want, pythonModuleFileMatches(c.path, c.module), "%s vs %s", c.path, c.module)
	}
}

// seedPythonExternGraph builds a caller in tests/test_pricing.py plus the
// given definition nodes, and returns a resolver with the lookup cache warm.
func seedPythonExternGraph(t *testing.T, defs []*graph.Node, edges ...*graph.Edge) *Resolver {
	t.Helper()
	g := graph.New()
	g.AddNode(&graph.Node{ID: "tests/test_pricing.py::test_it", Kind: graph.KindFunction, Name: "test_it",
		FilePath: "tests/test_pricing.py", Language: "python"})
	for _, d := range defs {
		if d.Language == "" {
			d.Language = "python"
		}
		g.AddNode(d)
	}
	r := New(g)
	r.warmLookupCache(edges)
	return r
}

func pyCall(target string) *graph.Edge {
	return &graph.Edge{From: "tests/test_pricing.py::test_it", To: "unresolved::" + target,
		Kind: graph.EdgeCalls, FilePath: "tests/test_pricing.py", Line: 5}
}

func resolvePyExtern(r *Resolver, e *graph.Edge) *ResolveStats {
	stats := &ResolveStats{}
	r.resolveExtern(e, graph.UnresolvedName(e.To)[len("extern::"):], stats)
	return stats
}

var (
	applyDiscountFn = &graph.Node{ID: "src/shop/pricing.py::apply_discount", Kind: graph.KindFunction, Name: "apply_discount", FilePath: "src/shop/pricing.py"}
	cartType        = &graph.Node{ID: "src/shop/pricing.py::Cart", Kind: graph.KindType, Name: "Cart", FilePath: "src/shop/pricing.py"}
)

// `from shop.pricing import apply_discount; apply_discount()` lands on the
// function, including under a src/ layout.
func TestResolveExtern_PythonFromImportFunction(t *testing.T) {
	e := pyCall("extern::shop.pricing.apply_discount::apply_discount")
	r := seedPythonExternGraph(t, []*graph.Node{applyDiscountFn}, e)

	stats := resolvePyExtern(r, e)
	assert.Equal(t, "src/shop/pricing.py::apply_discount", e.To)
	assert.Equal(t, 1, stats.Resolved)
	assert.Zero(t, stats.External)
}

// `from shop.pricing import apply_discount as discount; discount()` — the
// local alias is not the definition's name, so only the import path can find
// it.
func TestResolveExtern_PythonFromImportAlias(t *testing.T) {
	e := pyCall("extern::shop.pricing.apply_discount::discount")
	r := seedPythonExternGraph(t, []*graph.Node{applyDiscountFn}, e)

	resolvePyExtern(r, e)
	assert.Equal(t, "src/shop/pricing.py::apply_discount", e.To)
}

// `from shop.pricing import Cart; Cart()` lands on the class.
func TestResolveExtern_PythonFromImportClass(t *testing.T) {
	e := pyCall("extern::shop.pricing.Cart::Cart")
	r := seedPythonExternGraph(t, []*graph.Node{cartType}, e)

	resolvePyExtern(r, e)
	assert.Equal(t, "src/shop/pricing.py::Cart", e.To)
}

// `import shop.pricing as p; p.apply_discount()` and `from shop import pricing;
// pricing.apply_discount()` both carry the module path and the attribute.
func TestResolveExtern_PythonModuleAttributeCall(t *testing.T) {
	e := pyCall("extern::shop.pricing::apply_discount")
	r := seedPythonExternGraph(t, []*graph.Node{applyDiscountFn}, e)

	resolvePyExtern(r, e)
	assert.Equal(t, "src/shop/pricing.py::apply_discount", e.To)
}

// A package's __init__.py is the module for `from pkg import helper`.
func TestResolveExtern_PythonPackageInit(t *testing.T) {
	helper := &graph.Node{ID: "pkg/__init__.py::helper", Kind: graph.KindFunction, Name: "helper", FilePath: "pkg/__init__.py"}
	e := pyCall("extern::pkg.helper::helper")
	r := seedPythonExternGraph(t, []*graph.Node{helper}, e)

	resolvePyExtern(r, e)
	assert.Equal(t, "pkg/__init__.py::helper", e.To)
}

// An import binds module attributes, never class members: a method with the
// imported name must not be picked.
func TestResolveExtern_PythonIgnoresMethods(t *testing.T) {
	method := &graph.Node{ID: "src/shop/pricing.py::Cart.apply_discount", Kind: graph.KindMethod, Name: "apply_discount", FilePath: "src/shop/pricing.py"}
	e := pyCall("extern::shop.pricing.apply_discount::apply_discount")
	r := seedPythonExternGraph(t, []*graph.Node{method}, e)

	resolvePyExtern(r, e)
	assert.Equal(t, "dep::shop.pricing.apply_discount::apply_discount", e.To)
}

// A same-named function in an unrelated module is not the import's target;
// the edge keeps its external classification.
func TestResolveExtern_PythonWrongModuleStaysExternal(t *testing.T) {
	other := &graph.Node{ID: "src/shop/other.py::apply_discount", Kind: graph.KindFunction, Name: "apply_discount", FilePath: "src/shop/other.py"}
	e := pyCall("extern::shop.pricing.apply_discount::apply_discount")
	r := seedPythonExternGraph(t, []*graph.Node{other}, e)

	stats := resolvePyExtern(r, e)
	assert.Equal(t, "dep::shop.pricing.apply_discount::apply_discount", e.To)
	assert.Equal(t, 1, stats.External)
}

// Third-party imports with no first-party module stay external.
func TestResolveExtern_PythonThirdPartyStaysExternal(t *testing.T) {
	e := pyCall("extern::numpy.array::array")
	r := seedPythonExternGraph(t, []*graph.Node{applyDiscountFn}, e)

	resolvePyExtern(r, e)
	assert.Equal(t, "dep::numpy.array::array", e.To)
}

// Two same-repo files that both match the module path (e.g. a src/ copy and a
// build/ copy) are ambiguous: leave the edge rather than guess.
func TestResolveExtern_PythonAmbiguousModuleNotGuessed(t *testing.T) {
	copyFn := &graph.Node{ID: "build/lib/shop/pricing.py::apply_discount", Kind: graph.KindFunction, Name: "apply_discount", FilePath: "build/lib/shop/pricing.py"}
	e := pyCall("extern::shop.pricing.apply_discount::apply_discount")
	r := seedPythonExternGraph(t, []*graph.Node{applyDiscountFn, copyFn}, e)

	resolvePyExtern(r, e)
	assert.Equal(t, "dep::shop.pricing.apply_discount::apply_discount", e.To)
}

// When the module exists in the caller's repo and in another tracked repo,
// the caller's own copy wins and the edge is not marked cross-repo.
func TestResolveExtern_PythonPrefersCallerRepo(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "app/tests/test_pricing.py::test_it", Kind: graph.KindFunction, Name: "test_it",
		FilePath: "app/tests/test_pricing.py", RepoPrefix: "app", Language: "python"})
	g.AddNode(&graph.Node{ID: "app/src/shop/pricing.py::apply_discount", Kind: graph.KindFunction, Name: "apply_discount",
		FilePath: "app/src/shop/pricing.py", RepoPrefix: "app", Language: "python"})
	g.AddNode(&graph.Node{ID: "fork/src/shop/pricing.py::apply_discount", Kind: graph.KindFunction, Name: "apply_discount",
		FilePath: "fork/src/shop/pricing.py", RepoPrefix: "fork", Language: "python"})
	e := &graph.Edge{From: "app/tests/test_pricing.py::test_it", To: "unresolved::extern::shop.pricing.apply_discount::apply_discount",
		Kind: graph.EdgeCalls, FilePath: "app/tests/test_pricing.py", Line: 5}
	r := New(g)
	r.warmLookupCache([]*graph.Edge{e})

	resolvePyExtern(r, e)
	assert.Equal(t, "app/src/shop/pricing.py::apply_discount", e.To)
	assert.False(t, e.CrossRepo)
}

// Non-Python callers keep the Go-shaped directory match untouched.
func TestResolveExtern_PythonStepSkipsOtherLanguages(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "cmd/main.go::main", Kind: graph.KindFunction, Name: "main", FilePath: "cmd/main.go", Language: "go"})
	g.AddNode(&graph.Node{ID: "internal/shop/pricing.go::ApplyDiscount", Kind: graph.KindFunction, Name: "ApplyDiscount", FilePath: "internal/shop/pricing.go", Language: "go"})
	e := &graph.Edge{From: "cmd/main.go::main", To: "unresolved::extern::example.com/app/shop::ApplyDiscount",
		Kind: graph.EdgeCalls, FilePath: "cmd/main.go", Line: 3}
	r := New(g)
	r.warmLookupCache([]*graph.Edge{e})

	resolvePyExtern(r, e)
	require.Equal(t, "internal/shop/pricing.go::ApplyDiscount", e.To)
}
