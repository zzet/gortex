package mcp

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/zzet/gortex/internal/graph"
)

// buildBarrelGraph wires a canonical declaration plus a chain of barrels that
// forward it, the way the JS/TS extractor records a re-export (a file-level
// EdgeReExports edge, no node for the forwarded binding).
func buildBarrelGraph(t *testing.T) graph.Store {
	t.Helper()
	g := graph.New()
	addFile := func(id string) {
		g.AddNode(&graph.Node{ID: id, Kind: graph.KindFile, Name: id, FilePath: id, Language: "typescript"})
	}
	// Canonical declaration: src/middleware/persist.ts::persist.
	addFile("src/middleware/persist.ts")
	g.AddNode(&graph.Node{ID: "src/middleware/persist.ts::persist", Kind: graph.KindFunction,
		Name: "persist", FilePath: "src/middleware/persist.ts", Language: "typescript"})

	// Barrel: src/middleware.ts — `export { persist } from './middleware/persist'`.
	addFile("src/middleware.ts")
	g.AddEdge(&graph.Edge{From: "src/middleware.ts",
		To: "unresolved::import::./middleware/persist::persist", Kind: graph.EdgeReExports,
		FilePath: "src/middleware.ts", Line: 1})

	// Chained barrel: src/index.ts — `export { persist } from './middleware'`.
	addFile("src/index.ts")
	g.AddEdge(&graph.Edge{From: "src/index.ts",
		To: "unresolved::import::./middleware::persist", Kind: graph.EdgeReExports,
		FilePath: "src/index.ts", Line: 1})

	// Aliased re-export: src/ssr.ts — `export { ssrSafe as unstable_ssrSafe } from './impl'`.
	addFile("src/impl.ts")
	g.AddNode(&graph.Node{ID: "src/impl.ts::ssrSafe", Kind: graph.KindFunction,
		Name: "ssrSafe", FilePath: "src/impl.ts", Language: "typescript"})
	addFile("src/ssr.ts")
	g.AddEdge(&graph.Edge{From: "src/ssr.ts",
		To: "unresolved::import::./impl::ssrSafe", Kind: graph.EdgeReExports,
		Alias: "unstable_ssrSafe", FilePath: "src/ssr.ts", Line: 1})

	// Wildcard barrel: src/all.ts — `export * from './middleware/persist'`.
	addFile("src/all.ts")
	g.AddEdge(&graph.Edge{From: "src/all.ts",
		To: "unresolved::import::./middleware/persist", Kind: graph.EdgeReExports,
		FilePath: "src/all.ts", Line: 1})

	// Bare-package re-export (out of scope): src/vendor.ts — `export { x } from 'react'`.
	addFile("src/vendor.ts")
	g.AddEdge(&graph.Edge{From: "src/vendor.ts",
		To: "unresolved::import::react::x", Kind: graph.EdgeReExports,
		FilePath: "src/vendor.ts", Line: 1})

	return g
}

func TestReExportBindingCanonical(t *testing.T) {
	g := buildBarrelGraph(t)
	const canonical = "src/middleware/persist.ts::persist"

	assert.Equal(t, canonical, reExportBindingCanonical(g, "src/middleware.ts::persist", 0),
		"a named re-export maps to the canonical declaration")

	assert.Equal(t, canonical, reExportBindingCanonical(g, "src/index.ts::persist", 0),
		"a chained barrel resolves through both hops")

	assert.Equal(t, "src/impl.ts::ssrSafe", reExportBindingCanonical(g, "src/ssr.ts::unstable_ssrSafe", 0),
		"an aliased re-export resolves under the alias id to the original declaration")

	assert.Equal(t, canonical, reExportBindingCanonical(g, "src/all.ts::persist", 0),
		"`export * from` forwards a binding under its own name")

	assert.Empty(t, reExportBindingCanonical(g, "src/vendor.ts::x", 0),
		"a bare-package re-export is out of scope (needs alias resolution the query layer lacks)")

	assert.Empty(t, reExportBindingCanonical(g, "src/ssr.ts::ssrSafe", 0),
		"the original name is not the public binding when a re-export is aliased")

	assert.Empty(t, reExportBindingCanonical(g, "src/middleware/persist.ts::persist", 0),
		"the canonical declaration itself is not a barrel binding")
}

func TestReExportBindingCanonical_GroupedBindingsWildcardAndNamespace(t *testing.T) {
	g := graph.New()
	addFile := func(id string) {
		g.AddNode(&graph.Node{ID: id, Kind: graph.KindFile, Name: id, FilePath: id, Language: "javascript"})
	}
	addFile("src/impl.js")
	g.AddNode(&graph.Node{ID: "src/impl.js::foo", Kind: graph.KindFunction, Name: "foo", FilePath: "src/impl.js", Language: "javascript"})
	g.AddNode(&graph.Node{ID: "src/impl.js::bar", Kind: graph.KindFunction, Name: "bar", FilePath: "src/impl.js", Language: "javascript"})
	g.AddNode(&graph.Node{ID: "src/impl.js::baz", Kind: graph.KindFunction, Name: "baz", FilePath: "src/impl.js", Language: "javascript"})

	// Grouped reexport_bindings on unresolved target.
	addFile("src/grouped.js")
	g.AddEdge(&graph.Edge{
		From: "src/grouped.js", To: "unresolved::import::./impl", Kind: graph.EdgeReExports,
		FilePath: "src/grouped.js", Line: 1,
		Meta: map[string]any{
			"reexport_bindings": map[string]any{"pubFoo": "foo", "bar": "bar"},
		},
	})

	// Grouped reexport_bindings + reexport_wildcard on resolved KindFile target.
	addFile("src/mixed.js")
	g.AddEdge(&graph.Edge{
		From: "src/mixed.js", To: "src/impl.js", Kind: graph.EdgeReExports,
		FilePath: "src/mixed.js", Line: 1,
		Meta: map[string]any{
			"reexport_bindings": map[string]any{"renamed": "foo"},
			"reexport_wildcard": true,
		},
	})

	// Namespace re-export (`export * as ns from './impl'`) must never act as wildcard.
	addFile("src/ns.js")
	g.AddEdge(&graph.Edge{
		From: "src/ns.js", To: "unresolved::import::./impl", Kind: graph.EdgeReExports,
		FilePath: "src/ns.js", Line: 1, Alias: "ns",
		Meta: map[string]any{"reexport_namespace": true, "import_alias": "ns"},
	})

	// SQLite round-trip form: Edge.Alias cleared, Meta["import_alias"] + Meta["reexport_original"] carry the rename.
	addFile("src/meta_alias.js")
	g.AddEdge(&graph.Edge{
		From: "src/meta_alias.js", To: "src/impl.js", Kind: graph.EdgeReExports,
		FilePath: "src/meta_alias.js", Line: 1,
		Meta: map[string]any{"reexport_original": "foo", "import_alias": "aliasedFoo"},
	})

	// Grouped reexport_bindings resolved directly to a non-file symbol node
	// (`export { foo as a, foo as b } from './impl'`).
	addFile("src/dup_sym.js")
	g.AddEdge(&graph.Edge{
		From: "src/dup_sym.js", To: "src/impl.js::foo", Kind: graph.EdgeReExports,
		FilePath: "src/dup_sym.js", Line: 1,
		Meta: map[string]any{"reexport_bindings": map[string]any{"a": "foo", "b": "foo"}},
	})

	assert.Equal(t, "src/impl.js::foo", reExportBindingCanonical(g, "src/grouped.js::pubFoo", 0))
	assert.Equal(t, "src/impl.js::bar", reExportBindingCanonical(g, "src/grouped.js::bar", 0))
	assert.Empty(t, reExportBindingCanonical(g, "src/grouped.js::baz", 0))

	assert.Equal(t, "src/impl.js::foo", reExportBindingCanonical(g, "src/mixed.js::renamed", 0))
	assert.Equal(t, "src/impl.js::baz", reExportBindingCanonical(g, "src/mixed.js::baz", 0))

	assert.Empty(t, reExportBindingCanonical(g, "src/ns.js::foo", 0))
	assert.Empty(t, reExportBindingCanonical(g, "src/ns.js::ns", 0))

	assert.Equal(t, "src/impl.js::foo", reExportBindingCanonical(g, "src/meta_alias.js::aliasedFoo", 0))
	assert.Empty(t, reExportBindingCanonical(g, "src/meta_alias.js::foo", 0))

	assert.Equal(t, "src/impl.js::foo", reExportBindingCanonical(g, "src/dup_sym.js::a", 0))
	assert.Equal(t, "src/impl.js::foo", reExportBindingCanonical(g, "src/dup_sym.js::b", 0))
	assert.Empty(t, reExportBindingCanonical(g, "src/dup_sym.js::foo", 0))
}
