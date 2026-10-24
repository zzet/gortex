package resolver

import (
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

// seedFile adds a KindFile node with the given language to the
// graph; tests use it to drive the language-aware attribution pass.
func seedFile(g graph.Store, fileID, language string) {
	g.AddNode(&graph.Node{
		ID: fileID, Kind: graph.KindFile, Name: fileID,
		FilePath: fileID, Language: language,
	})
}

// seedExternalImport drops in an EdgeImports edge that's already
// landed at an `external::*` target — the post-pass inputs we want
// to exercise.
func seedExternalImport(g graph.Store, fileID, importPath string) *graph.Edge {
	e := &graph.Edge{
		From:     fileID,
		To:       "external::" + importPath,
		Kind:     graph.EdgeImports,
		FilePath: fileID,
		Line:     1,
	}
	g.AddEdge(e)
	return e
}

func TestAttributeNonGo_PythonPyPI(t *testing.T) {
	g := graph.New()
	seedFile(g, "app/main.py", "python")
	e := seedExternalImport(g, "app/main.py", "requests")

	r := New(g)
	r.attributeNonGoModuleImports()

	assert.Equal(t, "module::pypi:requests", e.To, "import edge should retarget onto KindModule")
	mod := g.GetNode("module::pypi:requests")
	require.NotNil(t, mod, "KindModule node should be materialised")
	assert.Equal(t, graph.KindModule, mod.Kind)
	assert.Equal(t, "pypi", mod.Meta["ecosystem"])

	depEdges := outEdgesOfKind(g, "app/main.py", graph.EdgeDependsOnModule)
	require.Len(t, depEdges, 1, "exactly one EdgeDependsOnModule per (file, module) pair")
	assert.Equal(t, "module::pypi:requests", depEdges[0].To)
}

func TestAttributeNonGo_PythonStdlibCollapsesToTopLevel(t *testing.T) {
	g := graph.New()
	seedFile(g, "app/main.py", "python")
	// `os.path` should attribute to `os` (the top-level module),
	// not to `os.path` — a `KindModule` is the package, not the
	// dotted path.
	e := seedExternalImport(g, "app/main.py", "os.path")

	r := New(g)
	r.attributeNonGoModuleImports()

	assert.Equal(t, "module::python:stdlib::os", e.To)
	mod := g.GetNode("module::python:stdlib::os")
	require.NotNil(t, mod)
	assert.Equal(t, "python:stdlib", mod.Meta["ecosystem"])
	assert.Equal(t, "stdlib", mod.Meta["role"])
	// import_path keeps the original dotted form so consumers can
	// recover the sub-module reference.
	assert.Equal(t, "os.path", mod.Meta["import_path"])
}

func TestAttributeNonGo_PythonRelativeImportsSkipped(t *testing.T) {
	g := graph.New()
	seedFile(g, "app/main.py", "python")
	// `from . import foo` lands as `external::.` — we can't
	// attribute relative imports without package-layout context,
	// so the pass leaves them alone.
	e := seedExternalImport(g, "app/main.py", ".foo.bar")

	r := New(g)
	r.attributeNonGoModuleImports()

	assert.Equal(t, "external::.foo.bar", e.To, "relative import must remain unchanged")
	depEdges := outEdgesOfKind(g, "app/main.py", graph.EdgeDependsOnModule)
	assert.Empty(t, depEdges)
}

func TestAttributeNonGo_DartPackageURI(t *testing.T) {
	g := graph.New()
	seedFile(g, "lib/main.dart", "dart")
	e := seedExternalImport(g, "lib/main.dart", "package:flutter/material.dart")

	r := New(g)
	r.attributeNonGoModuleImports()

	assert.Equal(t, "module::pub:flutter", e.To, "package: URI maps to its top-level pub package")
	mod := g.GetNode("module::pub:flutter")
	require.NotNil(t, mod)
	assert.Equal(t, "pub", mod.Meta["ecosystem"])
}

func TestAttributeNonGo_DartCoreLibraryStdlib(t *testing.T) {
	g := graph.New()
	seedFile(g, "lib/main.dart", "dart")
	e := seedExternalImport(g, "lib/main.dart", "dart:async")

	r := New(g)
	r.attributeNonGoModuleImports()

	assert.Equal(t, "module::dart:stdlib::async", e.To)
	mod := g.GetNode("module::dart:stdlib::async")
	require.NotNil(t, mod)
	assert.Equal(t, "stdlib", mod.Meta["role"])
}

func TestAttributeNonGo_DartRelativeImportsSkipped(t *testing.T) {
	g := graph.New()
	seedFile(g, "lib/main.dart", "dart")
	e := seedExternalImport(g, "lib/main.dart", "models/user.dart")

	r := New(g)
	r.attributeNonGoModuleImports()

	assert.Equal(t, "external::models/user.dart", e.To,
		"relative-path Dart imports stay external until layout-aware resolution lands")
}

func TestAttributeNonGo_OtherLanguagesUntouched(t *testing.T) {
	g := graph.New()
	// Go imports are handled by the dep-module bridge plus
	// goanalysis; this pass must not interfere with them.
	seedFile(g, "main.go", "go")
	e := seedExternalImport(g, "main.go", "github.com/foo/bar")

	r := New(g)
	r.attributeNonGoModuleImports()

	assert.Equal(t, "external::github.com/foo/bar", e.To)
}

func TestAttributeNonGo_DeduplicatesAcrossManyImports(t *testing.T) {
	g := graph.New()
	seedFile(g, "app/a.py", "python")
	// Three Python files all importing two distinct sub-modules
	// of `numpy`. The pass must yield exactly one
	// EdgeDependsOnModule per (file, module) pair, and a single
	// shared `module::pypi:numpy` node.
	for _, p := range []string{"numpy", "numpy.linalg", "numpy.random"} {
		seedExternalImport(g, "app/a.py", p)
	}

	r := New(g)
	r.attributeNonGoModuleImports()

	mod := g.GetNode("module::pypi:numpy")
	require.NotNil(t, mod)
	deps := outEdgesOfKind(g, "app/a.py", graph.EdgeDependsOnModule)
	require.Len(t, deps, 1, "three sub-imports should collapse to one EdgeDependsOnModule")
	assert.Equal(t, "module::pypi:numpy", deps[0].To)
}

func TestAttributeNonGo_IdempotentOnSecondPass(t *testing.T) {
	g := graph.New()
	seedFile(g, "app/main.py", "python")
	seedExternalImport(g, "app/main.py", "requests")

	r := New(g)
	r.attributeNonGoModuleImports()
	r.attributeNonGoModuleImports() // run again — must not duplicate.

	deps := outEdgesOfKind(g, "app/main.py", graph.EdgeDependsOnModule)
	require.Len(t, deps, 1, "second pass must not duplicate EdgeDependsOnModule")
}

// outEdgesOfKind is a small filter over Graph.GetOutEdges for the
// assertions above; declared here to keep the test file self-
// contained.
func outEdgesOfKind(g graph.Store, fileID string, kind graph.EdgeKind) []*graph.Edge {
	var out []*graph.Edge
	for _, e := range g.GetOutEdges(fileID) {
		if e.Kind == kind {
			out = append(out, e)
		}
	}
	return out
}

type moduleAttributionBatchCall struct {
	nodes int
	edges int
}

type moduleAttributionBatchRecordingStore struct {
	graph.Store
	addNodeCalls int
	addEdgeCalls int
	batchCalls   []moduleAttributionBatchCall
	reindexSizes []int
	events       []string
}

func (s *moduleAttributionBatchRecordingStore) AddNode(node *graph.Node) {
	s.addNodeCalls++
	s.Store.AddNode(node)
}

func (s *moduleAttributionBatchRecordingStore) AddEdge(edge *graph.Edge) {
	s.addEdgeCalls++
	s.Store.AddEdge(edge)
}

func (s *moduleAttributionBatchRecordingStore) AddBatch(nodes []*graph.Node, edges []*graph.Edge) {
	s.batchCalls = append(s.batchCalls, moduleAttributionBatchCall{nodes: len(nodes), edges: len(edges)})
	switch {
	case len(nodes) > 0 && len(edges) == 0:
		s.events = append(s.events, "nodes")
	case len(nodes) == 0 && len(edges) > 0:
		s.events = append(s.events, "edges")
	default:
		s.events = append(s.events, "mixed")
	}
	s.Store.AddBatch(nodes, edges)
}

func (s *moduleAttributionBatchRecordingStore) ReindexEdges(batch []graph.EdgeReindex) {
	s.reindexSizes = append(s.reindexSizes, len(batch))
	s.events = append(s.events, "reindex")
	s.Store.ReindexEdges(batch)
}

func TestAttributeNonGo_BatchedWritesPreserveDedupeAndOrdering(t *testing.T) {
	base := graph.New()
	for _, file := range []string{"app/a.py", "app/b.py", "app/c.py", "app/d.py"} {
		seedFile(base, file, "python")
	}
	imports := []*graph.Edge{
		seedExternalImport(base, "app/a.py", "numpy"),
		seedExternalImport(base, "app/a.py", "numpy.linalg"),
		seedExternalImport(base, "app/b.py", "numpy.random"),
		seedExternalImport(base, "app/c.py", "numpy.typing"),
		seedExternalImport(base, "app/d.py", "requests"),
	}
	base.AddNode(buildNonGoModuleNode(moduleSeed{
		id: "module::pypi:requests", language: "python", path: "requests",
	}))
	base.AddEdge(&graph.Edge{
		From: "app/c.py", To: "module::pypi:numpy", Kind: graph.EdgeDependsOnModule,
		FilePath: "app/c.py", Origin: graph.OriginASTResolved,
	})

	store := &moduleAttributionBatchRecordingStore{Store: base}
	New(store).attributeNonGoModuleImports()

	assert.Zero(t, store.addNodeCalls, "module materialization must not use single-row writes")
	assert.Zero(t, store.addEdgeCalls, "dependency materialization must not use single-row writes")
	assert.Equal(t, []moduleAttributionBatchCall{{nodes: 1}, {edges: 3}}, store.batchCalls)
	assert.Equal(t, []int{len(imports)}, store.reindexSizes)
	assert.Equal(t, []string{"nodes", "edges", "reindex"}, store.events,
		"modules and dependencies must be durable before import reindexing")

	require.NotNil(t, base.GetNode("module::pypi:numpy"))
	require.NotNil(t, base.GetNode("module::pypi:requests"))
	for i, edge := range imports {
		want := "module::pypi:numpy"
		if i == len(imports)-1 {
			want = "module::pypi:requests"
		}
		assert.Equal(t, want, edge.To)
		assert.Equal(t, graph.OriginASTResolved, edge.Origin)
	}
	assert.Len(t, outEdgesOfKind(base, "app/a.py", graph.EdgeDependsOnModule), 1,
		"dependsSeen must collapse sub-imports from one file")
	assert.Len(t, outEdgesOfKind(base, "app/b.py", graph.EdgeDependsOnModule), 1)
	assert.Len(t, outEdgesOfKind(base, "app/c.py", graph.EdgeDependsOnModule), 1,
		"existingDepends must suppress an already persisted dependency")
	assert.Len(t, outEdgesOfKind(base, "app/d.py", graph.EdgeDependsOnModule), 1)
	// Both app/a.py imports share line 1, so reindexing them to the same
	// module converges them onto one edge identity.
	assert.Equal(t, 8, base.EdgeCount(), "four import identities plus four dependency identities")
}

func TestAttributeNonGo_BoundsModuleAndDependencyWriteBatches(t *testing.T) {
	base := graph.New()
	seedFile(base, "app/main.py", "python")
	count := moduleAttributionWriteBatchSize + 1
	imports := make([]*graph.Edge, 0, count)
	for i := 0; i < count; i++ {
		name := "package_" + strconv.Itoa(i)
		imports = append(imports, &graph.Edge{
			From: "app/main.py", To: "external::" + name, Kind: graph.EdgeImports,
			FilePath: "app/main.py", Line: i + 1,
		})
	}
	base.AddBatch(nil, imports)

	store := &moduleAttributionBatchRecordingStore{Store: base}
	New(store).attributeNonGoModuleImports()

	assert.Zero(t, store.addNodeCalls)
	assert.Zero(t, store.addEdgeCalls)
	assert.Equal(t, []moduleAttributionBatchCall{
		{nodes: moduleAttributionWriteBatchSize}, {nodes: 1},
		{edges: moduleAttributionWriteBatchSize}, {edges: 1},
	}, store.batchCalls)
	assert.Equal(t, []int{count}, store.reindexSizes,
		"ReindexEdges must retain one ordered batch covering every rewritten import")
	assert.Equal(t, []string{"nodes", "nodes", "edges", "edges", "reindex"}, store.events)
	assert.Equal(t, count+1, base.NodeCount(), "one file plus one module per import")
	assert.Equal(t, count*2, base.EdgeCount(), "one import and one dependency per module")
	assert.Equal(t, "module::pypi:package_0", imports[0].To)
	assert.Equal(t, "module::pypi:package_"+strconv.Itoa(count-1), imports[count-1].To)
}
