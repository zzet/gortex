package goanalysis

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/tools/go/packages"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/semantic"
)

// recordingDeclarationReader serves the layer below's declaration nodes for a
// checkout pass and records every file it was asked for.
type recordingDeclarationReader struct {
	mu    sync.Mutex
	nodes map[string][]*graph.Node
	asked []string
}

func (r *recordingDeclarationReader) GetFileNodesByPaths(paths []string) map[string][]*graph.Node {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.asked = append(r.asked, paths...)
	out := make(map[string][]*graph.Node, len(paths))
	for _, p := range paths {
		if nodes, ok := r.nodes[p]; ok {
			out[p] = nodes
		}
	}
	return out
}

// declarationFixture is scopeFixture whose handle carries api/api.go alone,
// with a sibling of api (api/sibling.go, not on the handle) that calls into
// other/other.go. The layer below serves impl/impl.go and other/other.go.
func declarationFixture(t *testing.T) (string, *graph.Graph, *recordingDeclarationReader) {
	t.Helper()
	root := scopeFixture(t)
	writeFile(t, root, "api/sibling.go", `package api

import "example.com/scope/other"

// Sibling is in a root package but not on the handle.
func Sibling() string {
	return other.Other()
}
`)
	g := graph.New()
	add := func(id string, kind graph.NodeKind, name, file string, start, end int) {
		g.AddNode(&graph.Node{ID: id, Kind: kind, Name: name, FilePath: file, StartLine: start, EndLine: end, Language: "go"})
	}
	add("api/api.go", graph.KindFile, "api.go", "api/api.go", 1, 21)
	add("api/api.go::Shape", graph.KindInterface, "Shape", "api/api.go", 10, 12)
	add("api/api.go::Shape.Area", graph.KindMethod, "Area", "api/api.go", 11, 11)
	add("api/api.go::Measure", graph.KindFunction, "Measure", "api/api.go", 15, 20)
	reader := &recordingDeclarationReader{nodes: map[string][]*graph.Node{
		"impl/impl.go": {
			{ID: "impl/impl.go", Kind: graph.KindFile, Name: "impl.go", FilePath: "impl/impl.go", StartLine: 1, EndLine: 19, Language: "go"},
			{ID: "impl/impl.go::Square", Kind: graph.KindType, Name: "Square", FilePath: "impl/impl.go", StartLine: 6, EndLine: 8, Language: "go"},
			{ID: "impl/impl.go::Square.Area", Kind: graph.KindMethod, Name: "Area", FilePath: "impl/impl.go", StartLine: 11, EndLine: 13, Language: "go"},
			{ID: "impl/impl.go::Double", Kind: graph.KindFunction, Name: "Double", FilePath: "impl/impl.go", StartLine: 16, EndLine: 19, Language: "go"},
		},
		"other/other.go": {
			{ID: "other/other.go", Kind: graph.KindFile, Name: "other.go", FilePath: "other/other.go", StartLine: 1, EndLine: 10, Language: "go"},
			{ID: "other/other.go::Other", Kind: graph.KindFunction, Name: "Other", FilePath: "other/other.go", StartLine: 6, EndLine: 10, Language: "go"},
		},
	}}
	return root, g, reader
}

// TestCheckoutContextDeclarationsReadOnlyWhatTheHandleUses: a checkout pass
// maps the handle's uses of declarations in files it does not carry through
// the layer below, and asks that layer only for the files the handle's own
// uses name. A use in a sibling file of a root package (type-checked for its
// declarations, never projected into an edge) must not make the layer below
// serve its target's file: in a package of hundreds of files those reads
// were the pass's dominant cost and changed no edge.
func TestCheckoutContextDeclarationsReadOnlyWhatTheHandleUses(t *testing.T) {
	root, g, reader := declarationFixture(t)
	p := newTestProvider(t)
	_, err := runCheckoutScope(t, p, g, root, semantic.CheckoutCompilerScope{HandleRoots: true, Declarations: reader})
	require.NoError(t, err)

	joined := strings.Join(scopeOutput(t, g, p), "\n")
	require.Contains(t, joined, "edge api/api.go::Measure -calls-> impl/impl.go::Double",
		"the handle's call into a file it does not carry keeps its type-resolved edge")
	require.NotContains(t, joined, "other/other.go::Other", "a sibling's use is never projected")

	reader.mu.Lock()
	asked := slices.Clone(reader.asked)
	reader.mu.Unlock()
	require.Contains(t, asked, "impl/impl.go")
	require.NotContains(t, asked, "other/other.go",
		"only the handle's uses may make the layer below serve a declaration's file")
}

// TestGoUseApplyKeepsTheEarliestUse: TypesInfo.Uses is a map, and the apply
// keeps the first use of a caller/target/kind as the edge. The edge's line
// must not depend on map order: every pass over the same source writes the
// earliest use's line.
func TestGoUseApplyKeepsTheEarliestUse(t *testing.T) {
	root := scopeFixture(t)
	// Three calls of impl.Double on three lines of one function (lines 17-19).
	writeFile(t, root, "api/api.go", `package api

import (
	"strings"

	"example.com/scope/impl"
)

// Shape is implemented by impl.Square without naming it.
type Shape interface {
	Area() int
}

// Measure calls impl.Double three times.
func Measure() int {
	var b strings.Builder
	x := impl.Double(1)
	y := impl.Double(2)
	z := impl.Double(3)
	return x + y + z + b.Len()
}
`)
	p := newTestProvider(t)
	for pass := 0; pass < 12; pass++ {
		g := scopeHandleGraph()
		_, err := runCheckoutScope(t, p, g, root, semantic.CheckoutCompilerScope{HandleRoots: true})
		require.NoError(t, err)
		var lines []int
		for _, e := range g.AllEdges() {
			if e.From == "api/api.go::Measure" && e.To == "impl/impl.go::Double" && e.Kind == graph.EdgeCalls {
				lines = append(lines, e.Line)
			}
		}
		require.Equal(t, []int{17}, lines, "pass %d: the call edge must carry the earliest use's line", pass)
	}
}

// TestGraphVisibleSyntaxKeepsProjectedFilesAndLineDirectives: the definition
// walk builds syntax contexts only for the files the projection holds nodes
// for, plus any file with a line directive (its identifiers may report
// positions in another file).
func TestGraphVisibleSyntaxKeepsProjectedFilesAndLineDirectives(t *testing.T) {
	root := t.TempDir()
	fset := token.NewFileSet()
	parse := func(name, src string) *ast.File {
		file, err := parser.ParseFile(fset, filepath.Join(root, name), src, parser.ParseComments)
		require.NoError(t, err)
		return file
	}
	onHandle := parse("p/a.go", "package p\n\nfunc A() {}\n")
	sibling := parse("p/b.go", "package p\n\nfunc B() {}\n")
	directive := parse("p/c.go", "package p\n\n//line a.go:40\nfunc C() {}\n")
	nodesByFile := map[string][]*graph.Node{"repo/p/a.go": {{ID: "repo/p/a.go::A"}}}
	got := graphVisibleSyntax([]*ast.File{onHandle, sibling, nil, directive}, fset, root, "repo", nodesByFile)
	require.Equal(t, []*ast.File{onHandle, directive}, got)
}

// TestExistingExternalIDsFollowTheProjectedUses: the externals prefetch asks
// the store only for externals a projected use can name.
func TestExistingExternalIDsFollowTheProjectedUses(t *testing.T) {
	root := scopeFixture(t)
	writeFile(t, root, "api/sibling.go", `package api

import "os"

// Sibling is in a root package but not on the handle.
func Sibling() string {
	return os.Getenv("HOME")
}
`)
	pkgs, err := packages.Load(&packages.Config{
		Mode: packages.NeedName | packages.NeedFiles | packages.NeedImports | packages.NeedDeps |
			packages.NeedTypes | packages.NeedTypesInfo | packages.NeedSyntax | packages.NeedModule,
		Dir: root,
	}, "./api")
	require.NoError(t, err)
	require.Len(t, pkgs, 1)
	fset := pkgs[0].Fset
	externals := newExternalsAttribution(graph.New(), pkgs, "go-types", "", nil)
	hasOS := func(ids []string) bool {
		return slices.ContainsFunc(ids, func(id string) bool { return strings.Contains(id, "go:os") })
	}
	require.True(t, hasOS(externals.existingNodeIDs(pkgs, nil, nil)), "the fixture's sibling names os")
	onlyAPI := func(ident *ast.Ident) bool {
		return filepath.Base(fset.Position(ident.Pos()).Filename) == "api.go"
	}
	ids := externals.existingNodeIDs(pkgs, nil, onlyAPI)
	require.False(t, hasOS(ids), "a use the pass cannot project must not be prefetched: %v", ids)
	require.True(t, slices.ContainsFunc(ids, func(id string) bool { return strings.Contains(id, "strings") }),
		"the projected file's externals are still prefetched: %v", ids)
}
