package goanalysis

import (
	"os"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

// stdlibCallerFixture writes a one-package module that calls fmt.Println
// (a top-level stdlib func) and uses fmt.Stringer (a stdlib interface).
// Drives the externals attribution tests.
func stdlibCallerFixture(t *testing.T, root string) {
	t.Helper()
	writeGoMod(t, root, "example.com/stdlib")
	writeFile(t, root, "main.go", `package main

import "fmt"

type Banner struct{}

func (b Banner) String() string { return "hi" }

func Greet() {
	fmt.Println("hello")
	var _ fmt.Stringer = Banner{}
}
`)
}

func TestExternals_AttributesStdlibCallToModuleNode(t *testing.T) {
	root := resolvedTempDir(t)
	stdlibCallerFixture(t, root)

	g := graph.New()
	g.AddNode(&graph.Node{
		ID: "main.go::Greet", Kind: graph.KindFunction, Name: "Greet",
		FilePath: "main.go", StartLine: 9, EndLine: 12, Language: "go",
	})
	g.AddNode(&graph.Node{
		ID: "main.go::Banner", Kind: graph.KindType, Name: "Banner",
		FilePath: "main.go", StartLine: 5, EndLine: 5, Language: "go",
	})
	g.AddNode(&graph.Node{
		ID: "main.go::Banner.String", Kind: graph.KindMethod, Name: "String",
		FilePath: "main.go", StartLine: 7, EndLine: 7, Language: "go",
	})

	p := newTestProvider(t)
	_, err := p.Enrich(g, root)
	require.NoError(t, err)

	// fmt.Println landed as ext::go:fmt::Println.
	println := g.GetNode("ext::go:fmt::Println")
	require.NotNil(t, println, "expected synthetic node for fmt.Println")
	assert.Equal(t, graph.KindFunction, println.Kind)
	assert.Equal(t, "Println", println.Name)
	assert.Equal(t, "go", println.Language)
	require.NotNil(t, println.Meta)
	assert.True(t, println.Meta["external"].(bool))
	assert.Equal(t, "stdlib", println.Meta["module_kind"])
	assert.Equal(t, "fmt", println.Meta["import_path"])
	assert.Equal(t, "go-types", println.Meta["semantic_source"])
	sig, _ := println.Meta["signature"].(string)
	assert.Contains(t, sig, "Println")

	// Stdlib module node materialised exactly once.
	stdlib := g.GetNode("module::go:stdlib")
	require.NotNil(t, stdlib, "expected synthetic stdlib module node")
	assert.Equal(t, graph.KindModule, stdlib.Kind)
	assert.Equal(t, "stdlib", stdlib.Meta["module_kind"])

	// Greet → fmt.Println via EdgeCalls at lsp_resolved.
	var callEdge *graph.Edge
	for _, e := range g.GetOutEdges("main.go::Greet") {
		if e.To == "ext::go:fmt::Println" && e.Kind == graph.EdgeCalls {
			callEdge = e
			break
		}
	}
	require.NotNil(t, callEdge, "expected EdgeCalls Greet → ext::go:fmt::Println")
	assert.Equal(t, graph.OriginLSPResolved, callEdge.Origin)
	assert.Equal(t, "go-types", callEdge.Meta["semantic_source"])

	// External symbol → module via EdgeDependsOnModule.
	var modEdge *graph.Edge
	for _, e := range g.GetOutEdges("ext::go:fmt::Println") {
		if e.To == "module::go:stdlib" && e.Kind == graph.EdgeDependsOnModule {
			modEdge = e
			break
		}
	}
	require.NotNil(t, modEdge, "external symbol must link to its module via EdgeDependsOnModule")
	assert.Equal(t, graph.OriginLSPResolved, modEdge.Origin)

	// fmt.Stringer landed as a KindInterface external node.
	stringer := g.GetNode("ext::go:fmt::Stringer")
	require.NotNil(t, stringer, "expected synthetic node for fmt.Stringer")
	assert.Equal(t, graph.KindInterface, stringer.Kind)
}

// TestExternals_RecordsSourcePosition checks that external nodes carry
// the declaration position source reads need: an absolute file under
// GOROOT (or the module cache) and the line of the declaration.
func TestExternals_RecordsSourcePosition(t *testing.T) {
	root := resolvedTempDir(t)
	stdlibCallerFixture(t, root)

	g := graph.New()
	g.AddNode(&graph.Node{
		ID: "main.go::Greet", Kind: graph.KindFunction, Name: "Greet",
		FilePath: "main.go", StartLine: 9, EndLine: 12, Language: "go",
	})

	p := newTestProvider(t)
	_, err := p.Enrich(g, root)
	require.NoError(t, err)

	println := g.GetNode("ext::go:fmt::Println")
	require.NotNil(t, println, "expected synthetic node for fmt.Println")
	file, _ := println.Meta["source_file"].(string)
	require.NotEmpty(t, file, "expected source_file recorded for ext::go:fmt::Println")
	assert.True(t, strings.HasPrefix(file, runtime.GOROOT()),
		"source_file %q should live under GOROOT %q", file, runtime.GOROOT())
	require.FileExists(t, file)

	line := metaLineNumber(println.Meta["source_line"])
	require.Positive(t, line, "expected source_line recorded")
	raw, err := os.ReadFile(file)
	require.NoError(t, err)
	sourceLines := strings.Split(string(raw), "\n")
	require.GreaterOrEqual(t, len(sourceLines), line)
	assert.Contains(t, sourceLines[line-1], "func Println",
		"source_line should point at Println's declaration")
}

// metaLineNumber reads a Meta line number across the int / float64 forms
// a fresh enrich pass and a JSON round-trip produce.
func metaLineNumber(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	}
	return 0
}

func TestExternals_DepInModuleCacheGetsModuleNode(t *testing.T) {
	root := resolvedTempDir(t)
	writeGoMod(t, root, "example.com/depcaller")
	// We rely on x/tools being in the module cache for the host machine —
	// it's an indirect dep of the gortex module, so go/packages will find
	// it. The fixture imports a tiny well-known type to keep the test
	// resilient to API shifts.
	writeFile(t, root, "go.mod", `module example.com/depcaller

go 1.21

require golang.org/x/tools v0.20.0
`)
	writeFile(t, root, "main.go", `package main

import "golang.org/x/tools/go/packages"

func Use() *packages.Config {
	return &packages.Config{}
}
`)
	// Skip cleanly when module cache isn't populated for this dep.
	g := graph.New()
	g.AddNode(&graph.Node{
		ID: "main.go::Use", Kind: graph.KindFunction, Name: "Use",
		FilePath: "main.go", StartLine: 5, EndLine: 7, Language: "go",
	})

	p := newTestProvider(t)
	res, err := p.Enrich(g, root)
	if err != nil {
		t.Skipf("dep not in module cache (CI without GOFLAGS=-mod=mod cache): %v", err)
	}
	require.NotNil(t, res)

	var modulePathPrefix = "module::go:golang.org/x/tools"
	var foundModule *graph.Node
	for _, n := range g.AllNodes() {
		if n.Kind != graph.KindModule {
			continue
		}
		if strings.HasPrefix(n.ID, modulePathPrefix) {
			foundModule = n
			break
		}
	}
	if foundModule == nil {
		t.Skipf("x/tools not loaded with module info — host environment probably uses GOFLAGS=-mod=vendor")
	}
	assert.Equal(t, "module_cache", foundModule.Meta["module_kind"])
	assert.Equal(t, "go", foundModule.Language)

	// Use → some ext::go:golang.org/x/tools/go/packages::Config edge.
	var edge *graph.Edge
	for _, e := range g.GetOutEdges("main.go::Use") {
		if strings.HasPrefix(e.To, "ext::go:golang.org/x/tools/go/packages::") {
			edge = e
			break
		}
	}
	require.NotNil(t, edge, "expected an edge from Use to a packages.* external node")
	assert.Equal(t, graph.OriginLSPResolved, edge.Origin)
}

func TestExternals_ClaimsResolverStubEdge(t *testing.T) {
	root := resolvedTempDir(t)
	stdlibCallerFixture(t, root)

	g := graph.New()
	g.AddNode(&graph.Node{
		ID: "main.go::Greet", Kind: graph.KindFunction, Name: "Greet",
		FilePath: "main.go", StartLine: 9, EndLine: 12, Language: "go",
	})
	g.AddNode(&graph.Node{
		ID: "main.go::Banner", Kind: graph.KindType, Name: "Banner",
		FilePath: "main.go", StartLine: 5, EndLine: 5, Language: "go",
	})
	g.AddNode(&graph.Node{
		ID: "main.go::Banner.String", Kind: graph.KindMethod, Name: "String",
		FilePath: "main.go", StartLine: 7, EndLine: 7, Language: "go",
	})

	// Pre-seed the resolver-shaped stub edge that the parser+resolver
	// would have written before externals attribution landed.
	// Goanalysis must rewrite this edge to point at the real ext::
	// node rather than leaving a duplicate.
	stubEdge := &graph.Edge{
		From: "main.go::Greet", To: "stdlib::fmt::Println", Kind: graph.EdgeCalls,
		FilePath: "main.go", Line: 10,
		Confidence: 0.0, ConfidenceLabel: "INFERRED",
		Origin: graph.OriginTextMatched,
	}
	g.AddEdge(stubEdge)

	p := newTestProvider(t)
	_, err := p.Enrich(g, root)
	require.NoError(t, err)

	// The original stub bucket must drain; the new bucket must hold the edge.
	stubInBucket := false
	for _, e := range g.GetInEdges("stdlib::fmt::Println") {
		_ = e
		stubInBucket = true
	}
	assert.False(t, stubInBucket, "stub bucket stdlib::fmt::Println must be empty after externals upgrade")

	var realEdge *graph.Edge
	for _, e := range g.GetOutEdges("main.go::Greet") {
		if e.To == "ext::go:fmt::Println" && e.Kind == graph.EdgeCalls {
			realEdge = e
			break
		}
	}
	require.NotNil(t, realEdge)
	assert.Equal(t, graph.OriginLSPResolved, realEdge.Origin, "claimed stub edges must be promoted to lsp_resolved")
	assert.Equal(t, 1.0, realEdge.Confidence)
	require.NotNil(t, realEdge.Meta)
	assert.Equal(t, "go-types", realEdge.Meta["semantic_source"])

	// And the stub edge identity (same pointer) must now point at the real node.
	assert.Same(t, stubEdge, realEdge, "claim should mutate the existing edge struct, not duplicate it")
}

func TestExternals_ClaimsFuzzyStubByLineAndName(t *testing.T) {
	// The parser emits `unresolved::*.Method` for method calls on
	// external receivers it can't classify (e.g. `os.Stdout.Write`).
	// The fuzzy claim path matches by (line, trailing-name) so the
	// stub doesn't survive alongside the real ext::go: node.
	root := resolvedTempDir(t)
	writeGoMod(t, root, "example.com/methodcall")
	writeFile(t, root, "main.go", `package main

import "os"

func Write() {
	os.Stdout.Write([]byte("x"))
}
`)

	g := graph.New()
	g.AddNode(&graph.Node{
		ID: "main.go::Write", Kind: graph.KindFunction, Name: "Write",
		FilePath: "main.go", StartLine: 5, EndLine: 7, Language: "go",
	})
	stubEdge := &graph.Edge{
		From: "main.go::Write", To: "unresolved::*.Write", Kind: graph.EdgeCalls,
		FilePath: "main.go", Line: 6,
		Confidence: 0.0, ConfidenceLabel: "INFERRED",
		Origin: graph.OriginTextMatched,
	}
	g.AddEdge(stubEdge)

	p := newTestProvider(t)
	_, err := p.Enrich(g, root)
	require.NoError(t, err)

	// stub bucket drained — fuzzy claim should have rewritten the edge.
	stubInBucket := false
	for _, e := range g.GetInEdges("unresolved::*.Write") {
		_ = e
		stubInBucket = true
	}
	assert.False(t, stubInBucket, "fuzzy claim must drain the unresolved::*.Write bucket")

	// And the real ext::go:os::File.Write node holds the call.
	realNode := g.GetNode("ext::go:os::File.Write")
	require.NotNil(t, realNode, "expected ext::go:os::File.Write to be created")
	assert.Equal(t, graph.KindMethod, realNode.Kind)
	assert.Equal(t, "File", realNode.Meta["receiver"])

	var realEdge *graph.Edge
	for _, e := range g.GetOutEdges("main.go::Write") {
		if e.To == "ext::go:os::File.Write" && e.Kind == graph.EdgeCalls {
			realEdge = e
			break
		}
	}
	require.NotNil(t, realEdge, "expected EdgeCalls Write → ext::go:os::File.Write")
	assert.Equal(t, graph.OriginLSPResolved, realEdge.Origin)
	assert.Same(t, stubEdge, realEdge, "fuzzy claim should mutate the existing edge struct")
}

func TestExternals_FuzzyClaimDoesNotMatchAcrossLines(t *testing.T) {
	// Conservatism check: if a stub edge is at a different line, the
	// fuzzy pass must NOT claim it — that would clobber unrelated
	// outbound calls that happen to share a name.
	root := resolvedTempDir(t)
	stdlibCallerFixture(t, root)

	g := graph.New()
	g.AddNode(&graph.Node{
		ID: "main.go::Greet", Kind: graph.KindFunction, Name: "Greet",
		FilePath: "main.go", StartLine: 9, EndLine: 12, Language: "go",
	})
	g.AddNode(&graph.Node{
		ID: "main.go::Banner", Kind: graph.KindType, Name: "Banner",
		FilePath: "main.go", StartLine: 5, EndLine: 5, Language: "go",
	})
	g.AddNode(&graph.Node{
		ID: "main.go::Banner.String", Kind: graph.KindMethod, Name: "String",
		FilePath: "main.go", StartLine: 7, EndLine: 7, Language: "go",
	})
	// Stub on a wildly-wrong line (line 99) — must survive untouched.
	wrongLineStub := &graph.Edge{
		From: "main.go::Greet", To: "unresolved::*.Println", Kind: graph.EdgeCalls,
		FilePath: "main.go", Line: 99,
	}
	g.AddEdge(wrongLineStub)

	p := newTestProvider(t)
	_, err := p.Enrich(g, root)
	require.NoError(t, err)

	// The off-line stub must still be in its bucket — we didn't claim it.
	stillThere := false
	for _, e := range g.GetInEdges("unresolved::*.Println") {
		_ = e
		stillThere = true
	}
	assert.True(t, stillThere, "fuzzy claim must not match across lines")
}

func TestExternals_SkipsBuiltinsAndUniverseScope(t *testing.T) {
	root := resolvedTempDir(t)
	writeGoMod(t, root, "example.com/builtins")
	writeFile(t, root, "main.go", `package main

func Loop(xs []int) int {
	n := 0
	for _, x := range xs {
		n += x
	}
	return len(xs) + n
}
`)

	g := graph.New()
	g.AddNode(&graph.Node{
		ID: "main.go::Loop", Kind: graph.KindFunction, Name: "Loop",
		FilePath: "main.go", StartLine: 3, EndLine: 9, Language: "go",
	})

	p := newTestProvider(t)
	_, err := p.Enrich(g, root)
	require.NoError(t, err)

	// `int` and `len` are universe-scope objects (Pkg() == nil). They must
	// not become external nodes — that would be noise.
	for _, n := range g.AllNodes() {
		if n.Kind == graph.KindModule {
			continue
		}
		if !strings.HasPrefix(n.ID, "ext::") {
			continue
		}
		t.Fatalf("unexpected external node from universe-scope obj: %s", n.ID)
	}
}

func TestExternals_IdempotentOnReEnrich(t *testing.T) {
	root := resolvedTempDir(t)
	stdlibCallerFixture(t, root)

	g := graph.New()
	g.AddNode(&graph.Node{
		ID: "main.go::Greet", Kind: graph.KindFunction, Name: "Greet",
		FilePath: "main.go", StartLine: 9, EndLine: 12, Language: "go",
	})
	g.AddNode(&graph.Node{
		ID: "main.go::Banner", Kind: graph.KindType, Name: "Banner",
		FilePath: "main.go", StartLine: 5, EndLine: 5, Language: "go",
	})
	g.AddNode(&graph.Node{
		ID: "main.go::Banner.String", Kind: graph.KindMethod, Name: "String",
		FilePath: "main.go", StartLine: 7, EndLine: 7, Language: "go",
	})

	p := newTestProvider(t)
	_, err := p.Enrich(g, root)
	require.NoError(t, err)
	_, err = p.Enrich(g, root)
	require.NoError(t, err)

	// Count Greet → fmt.Println edges. Re-running Enrich must not
	// duplicate them; the second pass should confirm the existing edge
	// in place.
	matchingEdges := 0
	for _, e := range g.GetOutEdges("main.go::Greet") {
		if e.To == "ext::go:fmt::Println" && e.Kind == graph.EdgeCalls {
			matchingEdges++
		}
	}
	assert.Equal(t, 1, matchingEdges, "re-enrichment must not duplicate the external call edge")

	// One stdlib module node only.
	count := 0
	for _, n := range g.AllNodes() {
		if n.ID == "module::go:stdlib" {
			count++
		}
	}
	assert.Equal(t, 1, count)
}

func TestExternals_GoModuleNodeID(t *testing.T) {
	tests := []struct {
		path    string
		version string
		want    string
	}{
		{"github.com/foo/bar", "v1.2.3", "module::go:github.com/foo/bar@v1.2.3"},
		{"github.com/foo/bar", "", "module::go:github.com/foo/bar"},
		{"stdlib", "", "module::go:stdlib"},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.want, goModuleNodeID(tt.path, tt.version))
	}
}

func TestExternals_ShortModulePath(t *testing.T) {
	tests := map[string]string{
		"":                       "",
		"fmt":                    "fmt",
		"github.com/foo/bar":     "bar",
		"github.com/foo/bar/v2":  "bar",
		"github.com/foo/bar/v10": "bar",
		"github.com/foo/bar/sub": "sub",
		"single":                 "single",
		"vibrant":                "vibrant", // not a major-version segment
	}
	keys := make([]string, 0, len(tests))
	for k := range tests {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		assert.Equal(t, tests[k], shortModulePath(k), "input=%q", k)
	}
}

func TestExternals_IsMajorVersion(t *testing.T) {
	cases := map[string]bool{
		"":         false,
		"v":        false,
		"v0":       true,
		"v1":       true,
		"v999":     true,
		"v1.0":     false, // dots are not part of the suffix
		"v1a":      false,
		"vibrant":  false, // first letter v but rest non-digit
		"version1": false,
	}
	keys := make([]string, 0, len(cases))
	for k := range cases {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		assert.Equal(t, cases[k], isMajorVersion(k), "input=%q", k)
	}
}
