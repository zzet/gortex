package goanalysis

import (
	"context"
	"go/token"
	"go/types"
	"iter"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"golang.org/x/tools/go/packages"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

type queryCountingStore struct {
	graph.Store
	getFileNodesCalls     int
	getNodesByIDsCalls    int
	existingNodeIDCalls   int
	repoSummaryCalls      int
	semanticStampCalls    int
	getOutEdgesCalls      int
	getEdgeCandidateCalls int
	addNodeCalls          int
	addEdgeCalls          int
	addBatchCalls         int
	reindexEdgeCalls      int
	reindexEdgesCalls     int
	reindexEntries        int
}

func (s *queryCountingStore) AddNode(node *graph.Node) {
	s.addNodeCalls++
	s.Store.AddNode(node)
}

func (s *queryCountingStore) AddEdge(edge *graph.Edge) {
	s.addEdgeCalls++
	s.Store.AddEdge(edge)
}

func (s *queryCountingStore) AddBatch(nodes []*graph.Node, edges []*graph.Edge) {
	s.addBatchCalls++
	s.Store.AddBatch(nodes, edges)
}

func (s *queryCountingStore) ReindexEdge(edge *graph.Edge, oldTo string) {
	s.reindexEdgeCalls++
	s.Store.ReindexEdge(edge, oldTo)
}

func (s *queryCountingStore) ReindexEdges(batch []graph.EdgeReindex) {
	s.reindexEdgesCalls++
	s.reindexEntries += len(batch)
	s.Store.ReindexEdges(batch)
}

func (s *queryCountingStore) GetFileNodes(filePath string) []*graph.Node {
	s.getFileNodesCalls++
	return s.Store.GetFileNodes(filePath)
}

func (s *queryCountingStore) GetNodesByIDs(ids []string) map[string]*graph.Node {
	s.getNodesByIDsCalls++
	return s.Store.GetNodesByIDs(ids)
}

func (s *queryCountingStore) ExistingNodeIDs(ids []string) map[string]struct{} {
	s.existingNodeIDCalls++
	return graph.LookupExistingNodeIDs(s.Store, ids)
}

func (s *queryCountingStore) GetRepoNodeSummariesByLanguage(repoPrefix, language string) []*graph.Node {
	s.repoSummaryCalls++
	if reader, ok := s.Store.(graph.RepoLanguageNodeSummaryReader); ok {
		return reader.GetRepoNodeSummariesByLanguage(repoPrefix, language)
	}
	return s.GetRepoNodesByLanguage(repoPrefix, language)
}

func (s *queryCountingStore) PersistSemanticNodeStamps(updates []graph.SemanticNodeStamp) int {
	s.semanticStampCalls++
	if writer, ok := s.Store.(graph.SemanticNodeStampWriter); ok {
		return writer.PersistSemanticNodeStamps(updates)
	}
	return 0
}

func (s *queryCountingStore) GetOutEdges(nodeID string) []*graph.Edge {
	s.getOutEdgesCalls++
	return s.Store.GetOutEdges(nodeID)
}

func (s *queryCountingStore) GetEdgeCandidates(endpoints []graph.EdgeEndpoint, sites []graph.EdgeSite) graph.EdgeCandidateSet {
	s.getEdgeCandidateCalls++
	return graph.LookupEdgeCandidates(s.Store, endpoints, sites)
}

func (s *queryCountingStore) PersistEdgeAttributesBatch(edges []*graph.Edge) {
	if batch, ok := s.Store.(graph.EdgeMetaBatchPersister); ok {
		batch.PersistEdgeAttributesBatch(edges)
	}
}

func (s *queryCountingStore) ReplaceSemanticBindingTypes(repoPrefix string, rows []graph.SemanticBindingType) error {
	return s.Store.(graph.SemanticBindingTypeStore).ReplaceSemanticBindingTypes(repoPrefix, rows)
}

func (s *queryCountingStore) ReplaceSemanticBindingTypesForFiles(repoPrefix string, files []string, rows []graph.SemanticBindingType) error {
	return s.Store.(graph.SemanticBindingTypeStore).ReplaceSemanticBindingTypesForFiles(repoPrefix, files, rows)
}

func (s *queryCountingStore) DeleteSemanticBindingTypesByFiles(repoPrefix string, files []string) error {
	return s.Store.(graph.SemanticBindingTypeStore).DeleteSemanticBindingTypesByFiles(repoPrefix, files)
}

func (s *queryCountingStore) SemanticBindingTypes(sites []graph.SemanticBindingSite) (map[graph.SemanticBindingSite]string, error) {
	return s.Store.(graph.SemanticBindingTypeStore).SemanticBindingTypes(sites)
}

// resolvedTempDir wraps t.TempDir() with EvalSymlinks because on macOS
// t.TempDir() returns /var/folders/... while go/packages reports paths
// as /private/var/folders/... — the relativePath() prefix check would
// otherwise drop every file.
func resolvedTempDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	return dir
}

func writeFile(t *testing.T, dir, rel, content string) {
	t.Helper()
	full := filepath.Join(dir, rel)
	require.NoError(t, os.MkdirAll(filepath.Dir(full), 0o755))
	require.NoError(t, os.WriteFile(full, []byte(content), 0o644))
}

func writeGoMod(t *testing.T, dir, module string) {
	t.Helper()
	writeFile(t, dir, "go.mod", "module "+module+"\n\ngo 1.21\n")
}

// twoPackageCallerFixture writes a 2-package module where pkg/b/b.go calls
// pkg/a/a.go's Hello function. Used by the call-edge tests.
func twoPackageCallerFixture(t *testing.T, root string) {
	t.Helper()
	writeGoMod(t, root, "example.com/test")
	writeFile(t, root, "pkg/a/a.go", `package a

func Hello() string {
	return "hi"
}
`)
	writeFile(t, root, "pkg/b/b.go", `package b

import "example.com/test/pkg/a"

func Caller() string {
	return a.Hello()
}
`)
}

func newTestProvider(t *testing.T) *Provider {
	t.Helper()
	return NewProvider(ModeTypeCheck, false, zap.NewNop())
}

func TestGoAnalysis_Available(t *testing.T) {
	p := newTestProvider(t)
	assert.True(t, p.Available(), "go toolchain should be on PATH in tests")
	assert.Equal(t, "go-types", p.Name())
	assert.Equal(t, []string{"go"}, p.Languages())
}

func TestGoAnalysis_RelativePath(t *testing.T) {
	tests := []struct {
		name    string
		absPath string
		root    string
		want    string
	}{
		{"inside root", "/repo/pkg/a/a.go", "/repo", "pkg/a/a.go"},
		{"at root", "/repo/main.go", "/repo", "main.go"},
		{"outside root returns empty", "/elsewhere/foo.go", "/repo", ""},
		{"sibling sharing root prefix returns empty", "/repo-other/main.go", "/repo", ""},
		{"parent traversal returns empty", "/repo/../outside/main.go", "/repo", ""},
		{"empty repo", "/repo/main.go", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := relativePath(tt.absPath, tt.root)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestGoAnalysis_FindContainingFunc_PicksSmallest(t *testing.T) {
	g := graph.New()
	// Outer function spans lines 10-30.
	g.AddNode(&graph.Node{
		ID: "main.go::Outer", Kind: graph.KindFunction, Name: "Outer",
		FilePath: "main.go", StartLine: 10, EndLine: 30, Language: "go",
	})
	// Inner method spans lines 15-20 (smaller, should win for line 17).
	g.AddNode(&graph.Node{
		ID: "main.go::Inner", Kind: graph.KindMethod, Name: "Inner",
		FilePath: "main.go", StartLine: 15, EndLine: 20, Language: "go",
	})

	fileNodes := g.GetFileNodes("main.go")
	pos := token.Position{Filename: "/repo/main.go", Line: 17}
	got := findContainingFuncInNodes(fileNodes, pos.Line)
	require.NotNil(t, got)
	assert.Equal(t, "main.go::Inner", got.ID)

	// Line 25 is inside Outer only.
	pos.Line = 25
	got = findContainingFuncInNodes(fileNodes, pos.Line)
	require.NotNil(t, got)
	assert.Equal(t, "main.go::Outer", got.ID)

	// Line 5 is in neither.
	pos.Line = 5
	got = findContainingFuncInNodes(fileNodes, pos.Line)
	assert.Nil(t, got)
}

func TestGoAnalysis_InferEdgeKindFromObj(t *testing.T) {
	// Construct minimal go/types objects via a real package load so we get
	// real *types.Func / *types.TypeName / *types.Var / *types.Const values.
	root := resolvedTempDir(t)
	writeGoMod(t, root, "example.com/kindtest")
	writeFile(t, root, "main.go", `package main

const Pi = 3.14

var Counter int

type Widget struct{}

func Make() *Widget {
	return &Widget{}
}
`)

	p := newTestProvider(t)
	pkgs, _, err := p.loadPackages(root)
	require.NoError(t, err)
	require.NotEmpty(t, pkgs)

	var pi, counter, widget, make_ types.Object
	for _, pkg := range pkgs {
		if pkg.TypesInfo == nil {
			continue
		}
		for _, obj := range pkg.TypesInfo.Defs {
			if obj == nil {
				continue
			}
			switch obj.Name() {
			case "Pi":
				pi = obj
			case "Counter":
				counter = obj
			case "Widget":
				widget = obj
			case "Make":
				make_ = obj
			}
		}
	}
	require.NotNil(t, pi)
	require.NotNil(t, counter)
	require.NotNil(t, widget)
	require.NotNil(t, make_)

	assert.Equal(t, graph.EdgeCalls, inferEdgeKindFromObj(make_))
	assert.Equal(t, graph.EdgeReferences, inferEdgeKindFromObj(widget))
	assert.Equal(t, graph.EdgeReferences, inferEdgeKindFromObj(counter))
	assert.Equal(t, graph.EdgeReferences, inferEdgeKindFromObj(pi))
}

func TestGoAnalysis_LoadPackages_Smoke(t *testing.T) {
	root := resolvedTempDir(t)
	twoPackageCallerFixture(t, root)

	p := newTestProvider(t)
	pkgs, fset, err := p.loadPackages(root)
	require.NoError(t, err)
	require.NotEmpty(t, pkgs)
	require.NotNil(t, fset)

	var sawA, sawB bool
	for _, pkg := range pkgs {
		require.NotNil(t, pkg.TypesInfo, "every returned package must have TypesInfo")
		switch pkg.PkgPath {
		case "example.com/test/pkg/a":
			sawA = true
		case "example.com/test/pkg/b":
			sawB = true
		}
	}
	assert.True(t, sawA, "expected pkg a to be loaded")
	assert.True(t, sawB, "expected pkg b to be loaded")
}

func TestGoAnalysis_ConfirmsCallEdge(t *testing.T) {
	root := resolvedTempDir(t)
	twoPackageCallerFixture(t, root)

	g := graph.New()
	g.AddNode(&graph.Node{
		ID: "pkg/a/a.go::Hello", Kind: graph.KindFunction, Name: "Hello",
		FilePath: "pkg/a/a.go", StartLine: 3, EndLine: 5, Language: "go",
	})
	g.AddNode(&graph.Node{
		ID: "pkg/b/b.go::Caller", Kind: graph.KindFunction, Name: "Caller",
		FilePath: "pkg/b/b.go", StartLine: 5, EndLine: 7, Language: "go",
	})

	// Pre-seed an INFERRED call edge that go-types should confirm.
	g.AddEdge(&graph.Edge{
		From: "pkg/b/b.go::Caller", To: "pkg/a/a.go::Hello", Kind: graph.EdgeCalls,
		Confidence: 0.7, ConfidenceLabel: "INFERRED",
		Origin: graph.OriginASTInferred,
	})
	// The same endpoint may legitimately have another relationship. Candidate
	// confirmation must select by endpoint and inferred edge kind.
	g.AddEdge(&graph.Edge{
		From: "pkg/b/b.go::Caller", To: "pkg/a/a.go::Hello", Kind: graph.EdgeReferences,
		Confidence: 0.4, ConfidenceLabel: "HEURISTIC",
		Origin: graph.OriginTextMatched,
	})

	p := newTestProvider(t)
	result, err := p.Enrich(g, root)
	require.NoError(t, err)
	require.NotNil(t, result)

	assert.Greater(t, result.SymbolsCovered, 0, "should map at least Hello and Caller")
	assert.GreaterOrEqual(t, result.EdgesConfirmed, 1, "INFERRED call edge should be confirmed")

	// Verify the edge was upgraded in place.
	edges := g.GetOutEdges("pkg/b/b.go::Caller")
	require.NotEmpty(t, edges)
	var confirmed *graph.Edge
	for _, e := range edges {
		if e.To == "pkg/a/a.go::Hello" && e.Kind == graph.EdgeCalls {
			confirmed = e
			break
		}
	}
	require.NotNil(t, confirmed, "expected the call edge to still exist")
	assert.Equal(t, 1.0, confirmed.Confidence)
	assert.Equal(t, "EXTRACTED", confirmed.ConfidenceLabel)
	assert.Equal(t, graph.OriginLSPResolved, confirmed.Origin,
		"call edges resolved by go/types should land at lsp_resolved")
	require.NotNil(t, confirmed.Meta)
	assert.Equal(t, "go-types", confirmed.Meta["semantic_source"])

	var untouchedReference *graph.Edge
	for _, edge := range edges {
		if edge.To == "pkg/a/a.go::Hello" && edge.Kind == graph.EdgeReferences {
			untouchedReference = edge
			break
		}
	}
	require.NotNil(t, untouchedReference)
	assert.Equal(t, 0.4, untouchedReference.Confidence)
	assert.Equal(t, "HEURISTIC", untouchedReference.ConfidenceLabel)
	assert.Equal(t, graph.OriginTextMatched, untouchedReference.Origin)
}

func TestGoAnalysis_AddsMissingCallEdge(t *testing.T) {
	root := resolvedTempDir(t)
	twoPackageCallerFixture(t, root)

	g := graph.New()
	g.AddNode(&graph.Node{
		ID: "pkg/a/a.go::Hello", Kind: graph.KindFunction, Name: "Hello",
		FilePath: "pkg/a/a.go", StartLine: 3, EndLine: 5, Language: "go",
	})
	g.AddNode(&graph.Node{
		ID: "pkg/b/b.go::Caller", Kind: graph.KindFunction, Name: "Caller",
		FilePath: "pkg/b/b.go", StartLine: 5, EndLine: 7, Language: "go",
	})
	// No pre-seeded edge — the provider must discover the call.

	p := newTestProvider(t)
	result, err := p.Enrich(g, root)
	require.NoError(t, err)
	require.GreaterOrEqual(t, result.EdgesAdded, 1, "missing call edge should be added")

	edges := g.GetOutEdges("pkg/b/b.go::Caller")
	var added *graph.Edge
	for _, e := range edges {
		if e.To == "pkg/a/a.go::Hello" && e.Kind == graph.EdgeCalls {
			added = e
			break
		}
	}
	require.NotNil(t, added, "expected a new call edge to be added")
	assert.Equal(t, 1.0, added.Confidence)
	assert.Equal(t, graph.OriginLSPResolved, added.Origin)
	require.NotNil(t, added.Meta)
	assert.Equal(t, "go-types", added.Meta["semantic_source"])
}

func TestGoAnalysis_DetectsInterfaceImplementation(t *testing.T) {
	root := resolvedTempDir(t)
	writeGoMod(t, root, "example.com/iface")
	writeFile(t, root, "main.go", `package main

type Greeter interface {
	Greet() string
}

type EnglishGreeter struct{}

func (e EnglishGreeter) Greet() string {
	return "hello"
}
`)

	g := graph.New()
	g.AddNode(&graph.Node{
		ID: "main.go::Greeter", Kind: graph.KindInterface, Name: "Greeter",
		FilePath: "main.go", StartLine: 3, EndLine: 5, Language: "go",
	})
	g.AddNode(&graph.Node{
		ID: "main.go::EnglishGreeter", Kind: graph.KindType, Name: "EnglishGreeter",
		FilePath: "main.go", StartLine: 7, EndLine: 7, Language: "go",
	})
	g.AddNode(&graph.Node{
		ID: "main.go::EnglishGreeter.Greet", Kind: graph.KindMethod, Name: "Greet",
		FilePath: "main.go", StartLine: 9, EndLine: 11, Language: "go",
	})

	p := newTestProvider(t)
	result, err := p.Enrich(g, root)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, result.EdgesAdded, 1, "implements edge should be added")

	edges := g.GetOutEdges("main.go::EnglishGreeter")
	var impl *graph.Edge
	for _, e := range edges {
		if e.To == "main.go::Greeter" && e.Kind == graph.EdgeImplements {
			impl = e
			break
		}
	}
	require.NotNil(t, impl, "expected EdgeImplements from EnglishGreeter to Greeter")
	assert.Equal(t, 1.0, impl.Confidence)
	assert.Equal(t, graph.OriginLSPDispatch, impl.Origin,
		"implements edges should land at lsp_dispatch (one step from literal target)")
}

func TestGoAnalysis_NoFalseImplementsForUnrelatedTypes(t *testing.T) {
	root := resolvedTempDir(t)
	writeGoMod(t, root, "example.com/unrelated")
	writeFile(t, root, "main.go", `package main

type Reader interface {
	Read() string
}

type Counter struct{}

func (c Counter) Increment() int {
	return 1
}
`)

	g := graph.New()
	g.AddNode(&graph.Node{
		ID: "main.go::Reader", Kind: graph.KindInterface, Name: "Reader",
		FilePath: "main.go", StartLine: 3, EndLine: 5, Language: "go",
	})
	g.AddNode(&graph.Node{
		ID: "main.go::Counter", Kind: graph.KindType, Name: "Counter",
		FilePath: "main.go", StartLine: 7, EndLine: 7, Language: "go",
	})
	g.AddNode(&graph.Node{
		ID: "main.go::Counter.Increment", Kind: graph.KindMethod, Name: "Increment",
		FilePath: "main.go", StartLine: 9, EndLine: 11, Language: "go",
	})

	p := newTestProvider(t)
	_, err := p.Enrich(g, root)
	require.NoError(t, err)

	for _, e := range g.GetOutEdges("main.go::Counter") {
		if e.Kind == graph.EdgeImplements && e.To == "main.go::Reader" {
			t.Fatalf("Counter does not implement Reader; provider must not synthesize this edge")
		}
	}
}

func TestGoAnalysis_EnrichesNodeMeta(t *testing.T) {
	root := resolvedTempDir(t)
	writeGoMod(t, root, "example.com/meta")
	writeFile(t, root, "main.go", `package main

func F() (int, error) {
	return 0, nil
}
`)

	g := graph.New()
	g.AddNode(&graph.Node{
		ID: "main.go::F", Kind: graph.KindFunction, Name: "F",
		FilePath: "main.go", StartLine: 3, EndLine: 5, Language: "go",
	})

	p := newTestProvider(t)
	result, err := p.Enrich(g, root)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, result.NodesEnriched, 1)

	node := g.GetNode("main.go::F")
	require.NotNil(t, node)
	require.NotNil(t, node.Meta)
	semType, ok := node.Meta["semantic_type"].(string)
	require.True(t, ok, "semantic_type should be populated as a string")
	assert.Contains(t, semType, "func", "semantic_type should describe a function type")

	retType, ok := node.Meta["return_type"].(string)
	require.True(t, ok, "return_type should be populated for funcs")
	assert.Contains(t, retType, "int")
	assert.Contains(t, retType, "error")

	assert.Equal(t, "go-types", node.Meta["semantic_source"])
}

func TestProviderEnrichRepoScopesGraphPathsForMultiRepoStore(t *testing.T) {
	root := resolvedTempDir(t)
	writeGoMod(t, root, "example.com/sample")
	writeFile(t, root, "sample.go", `package sample

func BuildWidget() int { return 1 }

func UseWidget() int { return BuildWidget() }
`)

	store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, store.Close())
	})

	const (
		repoPrefix = "sample-repo"
		graphPath  = repoPrefix + "/sample.go"
		buildID    = graphPath + "::BuildWidget"
		useID      = graphPath + "::UseWidget"
	)
	store.AddBatch([]*graph.Node{
		{ID: buildID, Kind: graph.KindFunction, Name: "BuildWidget", FilePath: graphPath, StartLine: 3, EndLine: 3, Language: "go", RepoPrefix: repoPrefix},
		{ID: useID, Kind: graph.KindFunction, Name: "UseWidget", FilePath: graphPath, StartLine: 5, EndLine: 5, Language: "go", RepoPrefix: repoPrefix},
		// Decoys reproduce the ambiguity that hid the bug: go/types yields
		// repo-relative paths while a multi-repo graph stores prefixed paths.
		{ID: "sample.go::BuildWidget", Kind: graph.KindFunction, Name: "BuildWidget", FilePath: "sample.go", StartLine: 3, EndLine: 3, Language: "go"},
		{ID: "sample.go::UseWidget", Kind: graph.KindFunction, Name: "UseWidget", FilePath: "sample.go", StartLine: 5, EndLine: 5, Language: "go"},
	}, []*graph.Edge{{
		From:            useID,
		To:              buildID,
		Kind:            graph.EdgeCalls,
		FilePath:        graphPath,
		Line:            5,
		Confidence:      0.25,
		ConfidenceLabel: "HEURISTIC",
		Meta:            map[string]any{"preserve": "yes"},
	}})

	provider := newTestProvider(t)
	t.Cleanup(func() {
		require.NoError(t, provider.Close())
	})

	countingStore := &queryCountingStore{Store: store}
	result, err := provider.EnrichRepo(countingStore, repoPrefix, root)
	require.NoError(t, err)
	require.Zero(t, countingStore.getFileNodesCalls,
		"repo enrichment must not query SQLite once per go/types definition")
	require.Zero(t, countingStore.getNodesByIDsCalls,
		"SQLite summary/stamp paths must not materialize full node rows")
	require.Equal(t, 1, countingStore.repoSummaryCalls,
		"repo matching should use one metadata-free summary query")
	require.Equal(t, 1, countingStore.semanticStampCalls,
		"semantic stamps should use one set-oriented writer call")
	require.Zero(t, countingStore.getOutEdgesCalls,
		"repo enrichment must not query SQLite once per go/types use")
	require.Equal(t, 1, countingStore.getEdgeCandidateCalls,
		"the single package should use one predicate-shaped candidate batch")
	require.Equal(t, 1, result.EdgesConfirmed)
	require.Zero(t, result.EdgesAdded)
	require.Equal(t, 2, result.SymbolsTotal)
	require.Equal(t, result.SymbolsTotal, result.SymbolsCovered,
		"all repo-scoped symbols should match their prefixed graph nodes")

	build := store.GetNode(buildID)
	require.NotNil(t, build)
	require.NotEmpty(t, build.Meta["semantic_type"])
	decoy := store.GetNode("sample.go::BuildWidget")
	require.NotNil(t, decoy)
	assert.Empty(t, decoy.Meta["semantic_type"],
		"an unprefixed node outside this repo must not be enriched")

	var call *graph.Edge
	for _, edge := range store.GetOutEdges(useID) {
		if edge.To == buildID && edge.Kind == graph.EdgeCalls {
			call = edge
			break
		}
	}
	require.NotNil(t, call, "semantic call edge should use prefixed node IDs")
	assert.Equal(t, graphPath, call.FilePath)
	assert.Equal(t, 1.0, call.Confidence)
	assert.Equal(t, "EXTRACTED", call.ConfidenceLabel)
	assert.Equal(t, "go-types", call.Meta["semantic_source"])
	assert.Equal(t, "yes", call.Meta["preserve"],
		"persisting semantic confirmation must retain unrelated metadata")
}

func TestEnrichFilesUsesOneLoadForSameAndDifferentPackages(t *testing.T) {
	for _, tc := range []struct {
		name  string
		files []string
		want  []string
	}{
		{
			name:  "same package",
			files: []string{"repo/pkg/b.go", "repo/pkg/a.go", "repo/pkg/a.go"},
			want:  []string{"pkg/a.go", "pkg/b.go"},
		},
		{
			name:  "different packages",
			files: []string{"repo/other/b.go", "repo/pkg/a.go"},
			want:  []string{"other/b.go", "pkg/a.go"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := resolvedTempDir(t)
			writeGoMod(t, root, "example.com/batch")
			for _, relPath := range tc.want {
				packageName := filepath.Base(filepath.Dir(relPath))
				writeFile(t, root, relPath, "package "+packageName+"\n")
			}
			provider := newTestProvider(t)
			t.Cleanup(func() { require.NoError(t, provider.Close()) })

			loadCalls := 0
			var gotPatterns []string
			provider.packagesLoad = func(cfg *packages.Config, patterns ...string) ([]*packages.Package, error) {
				loadCalls++
				assert.Equal(t, root, cfg.Dir)
				gotPatterns = append([]string(nil), patterns...)
				return packages.Load(cfg, patterns...)
			}

			_, err := provider.EnrichFiles(graph.New(), "repo", root, tc.files)
			require.NoError(t, err)
			require.Equal(t, 1, loadCalls)
			wantPatterns := make([]string, 0, len(tc.want))
			for _, relPath := range tc.want {
				wantPatterns = append(wantPatterns, "file="+filepath.Join(root, filepath.FromSlash(relPath)))
			}
			assert.Equal(t, wantPatterns, gotPatterns)
			assert.NotContains(t, gotPatterns, "./...")
		})
	}
}

func TestEnrichFilesContextPropagatesCancellationToPackagesLoad(t *testing.T) {
	provider := newTestProvider(t)
	t.Cleanup(func() { require.NoError(t, provider.Close()) })

	ctx, cancel := context.WithCancel(context.Background())
	sawManagerContext := false
	provider.packagesLoad = func(cfg *packages.Config, _ ...string) ([]*packages.Package, error) {
		sawManagerContext = cfg.Context == ctx
		cancel()
		return nil, cfg.Context.Err()
	}

	_, err := provider.EnrichFilesContext(ctx, graph.New(), "repo", t.TempDir(), []string{"repo/a.go"})
	require.ErrorIs(t, err, context.Canceled)
	require.True(t, sawManagerContext, "packages.Load must receive the manager-derived context unchanged")
}

func TestProviderPersistsBindingsWithoutMirroringSQLite(t *testing.T) {
	root := resolvedTempDir(t)
	writeGoMod(t, root, "example.com/bindings")
	writeFile(t, root, "sample.go", `package sample

type Widget struct{}

func Use() Widget {
	value := Widget{}
	return value
}
`)

	store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	provider := newTestProvider(t)
	t.Cleanup(func() { require.NoError(t, provider.Close()) })

	const repoPrefix = "sample-repo"
	_, err = provider.EnrichRepo(store, repoPrefix, root)
	require.NoError(t, err)

	site := graph.SemanticBindingSite{
		RepoPrefix: repoPrefix,
		FilePath:   repoPrefix + "/sample.go",
		Line:       6,
		Name:       "value",
	}
	storedBindings, err := store.SemanticBindingTypes([]graph.SemanticBindingSite{site})
	require.NoError(t, err)
	assert.Equal(t, "Widget", storedBindings[site])
	memoryBindings, err := provider.SemanticBindingTypes([]graph.SemanticBindingSite{site})
	require.NoError(t, err)
	assert.NotContains(t, memoryBindings, site,
		"a persistent binding store must not be mirrored in provider memory")
}

func TestProviderEnrichRepoBatchesExternalSQLiteMutations(t *testing.T) {
	root := resolvedTempDir(t)
	stdlibCallerFixture(t, root)

	store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	store.AddBatch([]*graph.Node{
		{ID: "main.go::Greet", Kind: graph.KindFunction, Name: "Greet", FilePath: "main.go", StartLine: 9, EndLine: 12, Language: "go"},
		{ID: "main.go::Banner", Kind: graph.KindType, Name: "Banner", FilePath: "main.go", StartLine: 5, EndLine: 5, Language: "go"},
		{ID: "main.go::Banner.String", Kind: graph.KindMethod, Name: "String", FilePath: "main.go", StartLine: 7, EndLine: 7, Language: "go"},
	}, []*graph.Edge{{
		From: "main.go::Greet", To: "stdlib::fmt::Println", Kind: graph.EdgeCalls,
		FilePath: "main.go", Line: 10, ConfidenceLabel: "INFERRED", Origin: graph.OriginTextMatched,
	}})

	provider := newTestProvider(t)
	t.Cleanup(func() { require.NoError(t, provider.Close()) })
	counting := &queryCountingStore{Store: store}
	_, err = provider.Enrich(counting, root)
	require.NoError(t, err)

	require.Equal(t, 2, counting.existingNodeIDCalls,
		"external prefetch and final apply recheck must each use one lightweight ID-existence batch")
	require.Zero(t, counting.getNodesByIDsCalls,
		"external prefetch and SQLite stamps must not materialize full nodes")
	require.Equal(t, 1, counting.repoSummaryCalls)
	require.Equal(t, 1, counting.semanticStampCalls)
	require.Zero(t, counting.addNodeCalls, "external symbols must not commit one node at a time")
	require.Zero(t, counting.addEdgeCalls, "go/types Uses must not commit one edge at a time")
	require.Zero(t, counting.reindexEdgeCalls, "stub claims must not commit one rebind at a time")
	require.Positive(t, counting.addBatchCalls)
	require.Equal(t, 1, counting.reindexEdgesCalls)
	require.Equal(t, 1, counting.reindexEntries)

	var upgraded *graph.Edge
	for _, edge := range store.GetOutEdges("main.go::Greet") {
		if edge.To == "ext::go:fmt::Println" && edge.Kind == graph.EdgeCalls {
			upgraded = edge
			break
		}
	}
	require.NotNil(t, upgraded)
	assert.Equal(t, 1.0, upgraded.Confidence)
	assert.Equal(t, graph.OriginLSPResolved, upgraded.Origin)
	assert.Equal(t, "go-types", upgraded.Meta["semantic_source"])
}

func TestReleaseRepoStateDropsOnlyCompletedRepository(t *testing.T) {
	provider := newTestProvider(t)
	t.Cleanup(func() {
		require.NoError(t, provider.Close())
	})

	rootA := resolvedTempDir(t)
	rootB := resolvedTempDir(t)
	siteA := graph.SemanticBindingSite{RepoPrefix: "a", FilePath: "a/main.go", Line: 4, Name: "value"}
	siteB := graph.SemanticBindingSite{RepoPrefix: "b", FilePath: "b/main.go", Line: 4, Name: "value"}
	provider.replaceBindingIndex(rootA, nil, []graph.SemanticBindingType{{Site: siteA, TypeName: "TypeA"}})
	provider.replaceBindingIndex(rootB, nil, []graph.SemanticBindingType{{Site: siteB, TypeName: "TypeB"}})

	require.True(t, provider.ReleaseRepoState(rootA))
	got, err := provider.SemanticBindingTypes([]graph.SemanticBindingSite{siteA, siteB})
	require.NoError(t, err)
	assert.NotContains(t, got, siteA)
	assert.Equal(t, "TypeB", got[siteB])
	assert.False(t, provider.ReleaseRepoState(rootA), "release should be idempotent")
}

func TestBindingIndexIsolatesSameFileAndLineAcrossRepositories(t *testing.T) {
	provider := newTestProvider(t)
	t.Cleanup(func() { require.NoError(t, provider.Close()) })

	rootA := resolvedTempDir(t)
	rootB := resolvedTempDir(t)
	siteA := graph.SemanticBindingSite{RepoPrefix: "repo-a", FilePath: "shared/main.go", Line: 4, Name: "value"}
	siteB := graph.SemanticBindingSite{RepoPrefix: "repo-b", FilePath: "shared/main.go", Line: 4, Name: "value"}
	provider.replaceBindingIndex(rootA, nil, []graph.SemanticBindingType{{Site: siteA, TypeName: "TypeA"}})
	provider.replaceBindingIndex(rootB, nil, []graph.SemanticBindingType{{Site: siteB, TypeName: "TypeB"}})

	got, err := provider.SemanticBindingTypes([]graph.SemanticBindingSite{siteA, siteB})
	require.NoError(t, err)
	assert.Equal(t, "TypeA", got[siteA])
	assert.Equal(t, "TypeB", got[siteB])
	_, ok := provider.LookupTypeAtLine("shared/main.go", 4)
	assert.False(t, ok, "legacy lookup without a repo prefix must reject an ambiguous match")
}

func TestRetainedRepoStateSurvivesUntilFinalRelease(t *testing.T) {
	provider := newTestProvider(t)
	t.Cleanup(func() {
		require.NoError(t, provider.Close())
	})

	root := resolvedTempDir(t)
	site := graph.SemanticBindingSite{FilePath: "main.go", Line: 4, Name: "value"}
	provider.replaceBindingIndex(root, nil, []graph.SemanticBindingType{{Site: site, TypeName: "Widget"}})
	require.True(t, provider.RetainRepoState(root))
	require.True(t, provider.RetainRepoState(root))

	assert.False(t, provider.ReleaseRepoState(root), "the first release leaves one active lease")
	got, err := provider.SemanticBindingTypes([]graph.SemanticBindingSite{site})
	require.NoError(t, err)
	assert.Equal(t, "Widget", got[site])

	assert.True(t, provider.ReleaseRepoState(root))
	got, err = provider.SemanticBindingTypes([]graph.SemanticBindingSite{site})
	require.NoError(t, err)
	assert.NotContains(t, got, site)
}

func TestBuildSemanticBindingTypesPreservesLegacyLineParityAndAddsNamePrecision(t *testing.T) {
	root := resolvedTempDir(t)
	writeGoMod(t, root, "example.com/bindings")
	writeFile(t, root, "sample.go", `package sample

type Widget struct{}

func NewWidget() *Widget { return &Widget{} }

func Use() {
	first := NewWidget()
	var second Widget
	left, right := NewWidget(), NewWidget()
	var (
		grouped Widget
	)
	_, _, _, _ = first, second, left, right
}
`)

	provider := newTestProvider(t)
	pkgs, fset, err := provider.loadPackages(root)
	require.NoError(t, err)
	rows := buildSemanticBindingTypes(pkgs, fset, root, "repo")
	bySite := make(map[graph.SemanticBindingSite]string, len(rows))
	for _, row := range rows {
		bySite[row.Site] = row.TypeName
	}

	var fileInfo *types.Info
	var syntaxFileIndex int
	var found bool
	for _, pkg := range pkgs {
		for i, syntax := range pkg.Syntax {
			if filepath.Base(fset.Position(syntax.Pos()).Filename) == "sample.go" {
				fileInfo = pkg.TypesInfo
				syntaxFileIndex = i
				found = true
				break
			}
		}
		if found {
			legacyFile := pkg.Syntax[syntaxFileIndex]
			for _, line := range []int{8, 9, 10, 11} {
				legacy, ok := lookupTypeAtLineInFile(legacyFile, fileInfo, fset, line)
				require.True(t, ok, "legacy lookup should resolve line %d", line)
				site := graph.SemanticBindingSite{RepoPrefix: "repo", FilePath: "repo/sample.go", Line: line}
				assert.Equal(t, legacy, bySite[site], "compact line entry must preserve legacy result")
			}
			break
		}
	}
	require.True(t, found)

	for _, tc := range []struct {
		line int
		name string
	}{
		{8, "first"},
		{9, "second"},
		{10, "left"},
		{10, "right"},
		{12, "grouped"},
	} {
		site := graph.SemanticBindingSite{RepoPrefix: "repo", FilePath: "repo/sample.go", Line: tc.line, Name: tc.name}
		assert.Equal(t, "Widget", bySite[site], "named binding %s", tc.name)
	}

	provider.replaceBindingIndex(root, nil, rows)
	got, ok := provider.LookupTypeAtLine("repo/sample.go", 8)
	require.True(t, ok)
	assert.Equal(t, "Widget", got)
}

func TestClaimByExactStubSelectsCandidateKind(t *testing.T) {
	const (
		callerID   = "main.go::Caller"
		importPath = "example.com/ext"
		newTarget  = "ext::go:example.com/ext::Do"
	)
	pkg := types.NewPackage(importPath, "ext")
	obj := types.NewFunc(token.NoPos, pkg, "Do", types.NewSignatureType(nil, nil, nil, nil, nil, false))
	targets := stubEdgeTargets("", importPath, obj)
	require.NotEmpty(t, targets)

	call := &graph.Edge{From: callerID, To: targets[0], Kind: graph.EdgeCalls, Confidence: 0.4}
	reference := &graph.Edge{From: callerID, To: targets[0], Kind: graph.EdgeReferences, Confidence: 0.2}
	candidates := graph.NewEdgeCandidateSet()
	candidates.Add(reference)
	candidates.Add(call)

	attribution := &externalsAttribution{edgeCandidates: &candidates, provider: "go-types"}
	claimed := attribution.claimByExactStub(callerID, importPath, obj, newTarget)
	require.Same(t, call, claimed)
	assert.Equal(t, newTarget, call.To)
	assert.Equal(t, 1.0, call.Confidence)
	assert.Equal(t, targets[0], reference.To)
	assert.Equal(t, 0.2, reference.Confidence)
	require.Len(t, attribution.pendingReindexes, 1)
	assert.Same(t, call, attribution.pendingReindexes[0].Edge)
	assert.Equal(t, targets[0], attribution.pendingReindexes[0].OldTo)
}

func TestGoTypesHeavyGateDefaultsToOneAndHonorsCancellation(t *testing.T) {
	t.Setenv("GORTEX_GOTYPES_CONCURRENCY", "1")
	provider := newTestProvider(t)
	require.Equal(t, defaultGoTypesConcurrency, cap(provider.heavyGate))

	release, err := provider.acquireHeavy(context.Background(), false)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = provider.acquireHeavy(ctx, false)
	require.ErrorIs(t, err, context.Canceled)
	release()

	release, err = provider.acquireHeavy(context.Background(), false)
	require.NoError(t, err, "the cancelled waiter must not consume the slot")
	release()
}

type implementsBatchTestStore struct {
	graph.Store
	inboundBatchCalls int
	fullNodeBatches   int
	edgeKindScans     int
}

func (s *implementsBatchTestStore) GetInEdgesByNodeIDs(ids []string) map[string][]*graph.Edge {
	s.inboundBatchCalls++
	return s.Store.GetInEdgesByNodeIDs(ids)
}

func (s *implementsBatchTestStore) GetNodesByIDs(ids []string) map[string]*graph.Node {
	s.fullNodeBatches++
	return s.Store.GetNodesByIDs(ids)
}

func (s *implementsBatchTestStore) EdgesByKind(kind graph.EdgeKind) iter.Seq[*graph.Edge] {
	s.edgeKindScans++
	return s.Store.EdgesByKind(kind)
}

func TestEnrichImplementsUsesOneInboundBatchWithoutKindScan(t *testing.T) {
	base := graph.New()
	const (
		interfaceID = "repo/api.go::Service"
		concreteID  = "other/impl.go::Implementation"
	)
	base.AddBatch([]*graph.Node{
		{ID: interfaceID, Name: "Service", Kind: graph.KindInterface, FilePath: "repo/api.go", Language: "go", RepoPrefix: "repo"},
		{ID: concreteID, Name: "Implementation", Kind: graph.KindType, FilePath: "other/impl.go", Language: "go", RepoPrefix: "other"},
	}, []*graph.Edge{
		{From: concreteID, To: interfaceID, Kind: graph.EdgeImplements, Confidence: 0.25},
		{From: concreteID, To: interfaceID, Kind: graph.EdgeCalls, Confidence: 0.25},
		{From: concreteID, To: "other/api.go::Other", Kind: graph.EdgeImplements, Confidence: 0.25},
	})
	store := &implementsBatchTestStore{Store: base}

	pkg := types.NewPackage("example.com/repo", "repo")
	interfaceObj := types.NewTypeName(token.NoPos, pkg, "Service", nil)
	types.NewNamed(interfaceObj, types.NewInterfaceType(nil, nil).Complete(), nil)
	provider := newTestProvider(t)

	confirmed := provider.enrichImplements(store, map[types.Object]string{interfaceObj: interfaceID},
		map[string]*graph.Node{interfaceID: {ID: interfaceID, Kind: graph.KindInterface}})
	assert.Equal(t, 1, confirmed)
	assert.Equal(t, 1, store.inboundBatchCalls)
	assert.Equal(t, 1, store.fullNodeBatches)
	assert.Zero(t, store.edgeKindScans, "repo enrichment must not scan graph-wide implements edges")

	var implementation *graph.Edge
	for _, edge := range base.GetInEdges(interfaceID) {
		if edge.Kind == graph.EdgeImplements {
			implementation = edge
			break
		}
	}
	require.NotNil(t, implementation)
	assert.Equal(t, 1.0, implementation.Confidence)
	assert.Equal(t, "go-types", implementation.Meta["semantic_source"])
}

type lightProjectionTestStore struct {
	graph.Store
	light           []*graph.Node
	summaryCalls    int
	lightCalls      int
	fullReadBatches [][]string
}

func (s *lightProjectionTestStore) GetRepoNodeSummariesByLanguage(repoPrefix, language string) []*graph.Node {
	s.summaryCalls++
	var out []*graph.Node
	for _, node := range s.light {
		if node != nil && node.RepoPrefix == repoPrefix && node.Language == language {
			out = append(out, node)
		}
	}
	return out
}

func (s *lightProjectionTestStore) GetRepoNodesLight(string) []*graph.Node {
	s.lightCalls++
	return s.light
}

func (s *lightProjectionTestStore) GetNodesByIDs(ids []string) map[string]*graph.Node {
	s.fullReadBatches = append(s.fullReadBatches, append([]string(nil), ids...))
	return s.Store.GetNodesByIDs(ids)
}

func TestLightProjectionStampsRefetchFullNodesAndPreserveMeta(t *testing.T) {
	base := graph.New()
	full := &graph.Node{
		ID:         "repo/model.go::Thing",
		Name:       "Thing",
		Kind:       graph.KindType,
		FilePath:   "repo/model.go",
		Language:   "go",
		RepoPrefix: "repo",
		StartLine:  3,
		EndLine:    3,
		Meta:       map[string]any{"custom": "keep"},
	}
	base.AddNode(full)
	store := &lightProjectionTestStore{
		Store: base,
		light: []*graph.Node{
			{ID: full.ID, Name: full.Name, Kind: full.Kind, FilePath: full.FilePath, Language: "go", RepoPrefix: "repo", StartLine: 3, EndLine: 3},
			{ID: "repo/other.py::Other", Name: "Other", Kind: graph.KindType, FilePath: "repo/other.py", Language: "python", RepoPrefix: "repo"},
		},
	}

	projected := repoGoNodes(store, "repo")
	require.Len(t, projected, 1)
	assert.Equal(t, 1, store.summaryCalls)
	assert.Zero(t, store.lightCalls, "summary-capable stores must not load promoted metadata")
	assert.Nil(t, projected[0].Meta, "matching must use the metadata-free summary projection")

	count, err := persistGoNodeStamps(context.Background(), store, map[string]goNodeStamp{
		full.ID: {semanticType: "example.Thing", returnType: "(string)"},
	}, "go-types")
	require.NoError(t, err)
	assert.Equal(t, 1, count)
	require.Equal(t, [][]string{{full.ID}}, store.fullReadBatches)

	persisted := base.GetNode(full.ID)
	require.NotNil(t, persisted)
	assert.Equal(t, "keep", persisted.Meta["custom"])
	assert.Equal(t, "example.Thing", persisted.Meta["semantic_type"])
	assert.Equal(t, "(string)", persisted.Meta["return_type"])
}

type semanticStampWriterTestStore struct {
	*lightProjectionTestStore
	stampCalls int
	updates    []graph.SemanticNodeStamp
}

func (s *semanticStampWriterTestStore) PersistSemanticNodeStamps(updates []graph.SemanticNodeStamp) int {
	s.stampCalls++
	s.updates = append(s.updates, updates...)
	return len(updates)
}

func TestPersistGoNodeStampsPrefersSetOrientedWriter(t *testing.T) {
	base := graph.New()
	fallback := &lightProjectionTestStore{Store: base}
	store := &semanticStampWriterTestStore{lightProjectionTestStore: fallback}

	count, err := persistGoNodeStamps(context.Background(), store, map[string]goNodeStamp{
		"repo/model.go::Thing": {semanticType: "example.Thing", returnType: "(string)"},
	}, "go-types")
	require.NoError(t, err)
	assert.Equal(t, 1, count)
	assert.Equal(t, 1, store.stampCalls)
	assert.Empty(t, fallback.fullReadBatches, "set-oriented writer must avoid full node refetch")
	require.Equal(t, []graph.SemanticNodeStamp{{
		NodeID:         "repo/model.go::Thing",
		SemanticType:   "example.Thing",
		ReturnType:     "(string)",
		SemanticSource: "go-types",
	}}, store.updates)
}
