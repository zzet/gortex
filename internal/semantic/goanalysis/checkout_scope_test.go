package goanalysis

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/tools/go/packages"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/semantic"
)

// Handle-rooted checkout loads. A checkout pass enriches a generation handle
// whose facts are keyed to the files it carries; loading only the packages of
// those files must write exactly what a whole-module load writes.

// scopeFixture is a module with two handle packages (api, impl) and two
// packages the handle does not carry (helper, other). It exercises every
// output family: definitions, a cross-package call and reference, an
// implicit cross-package implements pair (both ends in the handle), an
// external (stdlib) use, return-type stamps and named-type bindings.
func scopeFixture(t *testing.T) string {
	t.Helper()
	root := resolvedTempDir(t)
	writeGoMod(t, root, "example.com/scope")
	writeFile(t, root, "api/api.go", `package api

import (
	"strings"

	"example.com/scope/impl"
)

// Shape is implemented by impl.Square without naming it.
type Shape interface {
	Area() int
}

// Measure uses the handle's other package and the standard library.
func Measure() int {
	var s Shape = impl.Square{Side: 2}
	var b strings.Builder
	b.WriteString("x")
	return s.Area() + impl.Double(3) + b.Len()
}
`)
	writeFile(t, root, "impl/impl.go", `package impl

import "example.com/scope/helper"

// Square is a concrete shape.
type Square struct {
	Side int
}

// Area is the implicit Shape method.
func (s Square) Area() int {
	return s.Side * s.Side
}

// Double calls a package the handle does not carry.
func Double(n int) int {
	w := helper.Wrap(n)
	return w.N * 2
}
`)
	writeFile(t, root, "helper/helper.go", `package helper

// Wrapped is a named type the handle's package binds.
type Wrapped struct {
	N int
}

// Wrap is outside the handle.
func Wrap(n int) Wrapped {
	var w Wrapped
	w.N = n
	return w
}
`)
	writeFile(t, root, "other/other.go", `package other

import "strings"

// Other is never in the handle; its bindings must not be written.
func Other() string {
	var b strings.Builder
	b.WriteString("other")
	return b.String()
}
`)
	return root
}

// scopeHandleGraph is the generation handle of the fixture: the Go nodes of
// api/api.go and impl/impl.go only.
func scopeHandleGraph() *graph.Graph {
	g := graph.New()
	add := func(id string, kind graph.NodeKind, name, file string, start, end int) {
		g.AddNode(&graph.Node{ID: id, Kind: kind, Name: name, FilePath: file, StartLine: start, EndLine: end, Language: "go"})
	}
	add("api/api.go", graph.KindFile, "api.go", "api/api.go", 1, 21)
	add("api/api.go::Shape", graph.KindInterface, "Shape", "api/api.go", 10, 12)
	add("api/api.go::Shape.Area", graph.KindMethod, "Area", "api/api.go", 11, 11)
	add("api/api.go::Measure", graph.KindFunction, "Measure", "api/api.go", 15, 20)
	add("impl/impl.go", graph.KindFile, "impl.go", "impl/impl.go", 1, 19)
	add("impl/impl.go::Square", graph.KindType, "Square", "impl/impl.go", 6, 8)
	add("impl/impl.go::Square.Area", graph.KindMethod, "Area", "impl/impl.go", 11, 13)
	add("impl/impl.go::Double", graph.KindFunction, "Double", "impl/impl.go", 16, 19)
	return g
}

// scopeOutput renders everything a pass wrote into the handle, plus the
// binding rows it kept.
func scopeOutput(t *testing.T, g *graph.Graph, p *Provider) []string {
	t.Helper()
	var out []string
	for _, n := range g.AllNodes() {
		keys := make([]string, 0, len(n.Meta))
		for k := range n.Meta {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var meta []string
		for _, k := range keys {
			meta = append(meta, fmt.Sprintf("%s=%v", k, n.Meta[k]))
		}
		out = append(out, fmt.Sprintf("node %s kind=%s file=%s meta={%s}", n.ID, n.Kind, n.FilePath, strings.Join(meta, ",")))
	}
	for _, e := range g.AllEdges() {
		out = append(out, fmt.Sprintf("edge %s -%s-> %s line=%d origin=%s conf=%v src=%v", e.From, e.Kind, e.To, e.Line, e.Origin, e.Confidence, e.Meta["semantic_source"]))
	}
	p.stateMu.RLock()
	for key, typeName := range p.bindingTypes {
		out = append(out, fmt.Sprintf("binding %s:%d:%s=%s", key.filePath, key.line, key.name, typeName))
	}
	p.stateMu.RUnlock()
	sort.Strings(out)
	return out
}

func runCheckoutScope(t *testing.T, p *Provider, g graph.Store, root string, scope semantic.CheckoutCompilerScope) (*semantic.EnrichResult, error) {
	t.Helper()
	ctx := semantic.WithCheckoutCompilerScope(context.Background(), scope)
	return p.EnrichRepoContext(ctx, g, "", root, nil)
}

// TestCheckoutScopeDifferentialFullVsHandleRoots is the parity proof: the same
// handle run through a whole-module load and a handle-rooted load writes the
// same definitions coverage, uses, externals, implements, stamps and bindings.
func TestCheckoutScopeDifferentialFullVsHandleRoots(t *testing.T) {
	root := scopeFixture(t)

	fullGraph, fullProvider := scopeHandleGraph(), newTestProvider(t)
	full, err := runCheckoutScope(t, fullProvider, fullGraph, root, semantic.CheckoutCompilerScope{})
	require.NoError(t, err)

	scopedGraph, scopedProvider := scopeHandleGraph(), newTestProvider(t)
	scoped, err := runCheckoutScope(t, scopedProvider, scopedGraph, root, semantic.CheckoutCompilerScope{HandleRoots: true})
	require.NoError(t, err)

	fullOut, scopedOut := scopeOutput(t, fullGraph, fullProvider), scopeOutput(t, scopedGraph, scopedProvider)
	require.Equal(t, fullOut, scopedOut, "a handle-rooted load must write exactly what the whole-module load writes")

	// The fixture is not vacuous: every output family is present.
	joined := strings.Join(scopedOut, "\n")
	for _, want := range []string{
		"edge api/api.go::Measure -calls-> impl/impl.go::Double",
		"edge impl/impl.go::Square -implements-> api/api.go::Shape",
		"ext::go:strings::Builder",
		"binding api/api.go:",
		"binding impl/impl.go:",
	} {
		require.Contains(t, joined, want)
	}
	require.NotContains(t, joined, "binding other/", "bindings outside the handle must not be written")
	require.NotContains(t, joined, "binding helper/", "bindings outside the handle must not be written")
	require.Equal(t, full.SymbolsCovered, scoped.SymbolsCovered)
	require.Equal(t, full.EdgesAdded+full.EdgesConfirmed, scoped.EdgesAdded+scoped.EdgesConfirmed)

	require.NotNil(t, full.Compiler)
	require.Equal(t, semantic.CompilerScopeFull, full.Compiler.Scope)
	require.Equal(t, semantic.CompilerScopeReasonDisabled, full.Compiler.ScopeReason)
	require.Equal(t, 4, full.Compiler.Packages)
	require.Equal(t, 4, full.Compiler.Files)
	require.Equal(t, 1, full.Compiler.Loads)

	require.NotNil(t, scoped.Compiler)
	require.Equal(t, semantic.CompilerScopeHandleRoots, scoped.Compiler.Scope)
	require.Empty(t, scoped.Compiler.ScopeReason)
	require.Equal(t, 2, scoped.Compiler.Packages, "packages(handle) = {api, impl}")
	require.Equal(t, 2, scoped.Compiler.Files)
	require.Equal(t, 1, scoped.Compiler.Loads)
	require.False(t, scoped.Compiler.IndexCached, "the first pass must build the dependency index")

	// A second pass over the same manifests reuses the dependency index.
	again, err := runCheckoutScope(t, scopedProvider, scopeHandleGraph(), root, semantic.CheckoutCompilerScope{HandleRoots: true})
	require.NoError(t, err)
	require.True(t, again.Compiler.IndexCached, "an unchanged go.mod/go.sum must not re-run the metadata go list")
}

// TestCheckoutScopeRootPatterns pins the root selection: directory patterns
// of the handle's Go files, minus what "./..." never loads.
func TestCheckoutScopeRootPatterns(t *testing.T) {
	root := resolvedTempDir(t)
	writeGoMod(t, root, "example.com/patterns")
	writeFile(t, root, "nested/go.mod", "module example.com/nested\n\ngo 1.21\n")
	files := map[string]struct{}{}
	for _, p := range []string{
		"r/main.go", "r/a/a.go", "r/a/a2.go", "r/b/c/c.go",
		"r/a/testdata/t.go", "r/_hidden/h.go", "r/.dot/d.go", "r/vendor/v/v.go", "r/nested/n/n.go", "r/a/_skip.go",
	} {
		files[p] = struct{}{}
	}
	plan := planHandleRoots(root, root, "r", files, semantic.CheckoutCompilerScope{HandleRoots: true})
	require.False(t, plan.full, plan.reason)
	require.Equal(t, []string{".", "./a", "./b/c"}, plan.patterns)

	require.True(t, planHandleRoots(root, root, "r", map[string]struct{}{"x/y.go": {}}, semantic.CheckoutCompilerScope{HandleRoots: true}).full)
	require.Equal(t, semantic.CompilerScopeReasonPathUnmappable,
		planHandleRoots(root, root, "r", map[string]struct{}{"x/y.go": {}}, semantic.CheckoutCompilerScope{HandleRoots: true}).reason)
	require.Equal(t, semantic.CompilerScopeReasonPathUnmappable,
		planHandleRoots(root, root, "", map[string]struct{}{"../y.go": {}}, semantic.CheckoutCompilerScope{HandleRoots: true}).reason)
	require.Equal(t, semantic.CompilerScopeReasonManifestChanged,
		planHandleRoots(root, root, "r", files, semantic.CheckoutCompilerScope{HandleRoots: true, ManifestChanged: true}).reason)
	require.Equal(t, semantic.CompilerScopeReasonMultiModule,
		planHandleRoots(root, filepath.Join(root, "nested"), "r", files, semantic.CheckoutCompilerScope{HandleRoots: true}).reason)
	empty := planHandleRoots(root, root, "r", map[string]struct{}{"r/a/testdata/t.go": {}}, semantic.CheckoutCompilerScope{HandleRoots: true})
	require.True(t, empty.empty())

	writeFile(t, root, "go.work", "go 1.21\n\nuse .\n")
	require.Equal(t, semantic.CompilerScopeReasonMultiModule,
		planHandleRoots(root, root, "r", files, semantic.CheckoutCompilerScope{HandleRoots: true}).reason)
}

// countingLoader wraps packages.Load, counting type-checking loads and
// letting a test fail or cut one.
type countingLoader struct {
	mu      sync.Mutex
	typed   int
	meta    int
	onTyped func(call int, cfg *packages.Config, patterns []string) ([]*packages.Package, error, bool)
}

func (l *countingLoader) load(cfg *packages.Config, patterns ...string) ([]*packages.Package, error) {
	l.mu.Lock()
	typed := cfg.Mode&packages.NeedTypes != 0
	if typed {
		l.typed++
	} else {
		l.meta++
	}
	call := l.typed
	hook := l.onTyped
	l.mu.Unlock()
	if typed && hook != nil {
		if pkgs, err, handled := hook(call, cfg, patterns); handled {
			return pkgs, err
		}
	}
	return packages.Load(cfg, patterns...)
}

// TestCheckoutScopeScopedThenFull: a scoped load that fails (or misses a
// root) while its context is live is retried exactly once with the whole
// module, before any graph or binding write, and the output is unchanged.
func TestCheckoutScopeScopedThenFull(t *testing.T) {
	root := scopeFixture(t)
	fullGraph, fullProvider := scopeHandleGraph(), newTestProvider(t)
	_, err := runCheckoutScope(t, fullProvider, fullGraph, root, semantic.CheckoutCompilerScope{})
	require.NoError(t, err)
	want := scopeOutput(t, fullGraph, fullProvider)

	for _, mode := range []string{"error", "missing_root"} {
		t.Run(mode, func(t *testing.T) {
			g := scopeHandleGraph()
			edgesBefore := len(g.AllEdges())
			var patternsSeen [][]string
			var mutatedBeforeRetry bool
			loader := &countingLoader{}
			loader.onTyped = func(call int, cfg *packages.Config, patterns []string) ([]*packages.Package, error, bool) {
				patternsSeen = append(patternsSeen, slices.Clone(patterns))
				if call == 2 {
					mutatedBeforeRetry = len(g.AllEdges()) != edgesBefore
					return nil, nil, false
				}
				if mode == "error" {
					return nil, errors.New("injected scoped load failure"), true
				}
				pkgs, err := packages.Load(cfg, patterns...)
				if err != nil {
					return nil, err, true
				}
				return pkgs[:len(pkgs)-1], nil, true
			}
			p := newTestProvider(t)
			p.packagesLoad = loader.load
			result, err := runCheckoutScope(t, p, g, root, semantic.CheckoutCompilerScope{HandleRoots: true})
			require.NoError(t, err)
			require.Equal(t, 2, loader.typed, "exactly one whole-module retry")
			require.Equal(t, []string{"./api", "./impl"}, patternsSeen[0])
			require.Equal(t, []string{"./..."}, patternsSeen[1])
			require.False(t, mutatedBeforeRetry, "nothing may be written before the retry")
			require.Equal(t, semantic.CompilerScopeFull, result.Compiler.Scope)
			require.Equal(t, semantic.CompilerScopeReasonScopedThenFull, result.Compiler.ScopeReason)
			require.Equal(t, 2, result.Compiler.Loads)
			require.Equal(t, 4, result.Compiler.Packages)
			require.Equal(t, want, scopeOutput(t, g, p))
		})
	}
}

// TestCheckoutScopeCancellationNeverRetries: a canceled scoped load returns
// the context error and never falls back to the whole module.
func TestCheckoutScopeCancellationNeverRetries(t *testing.T) {
	root := scopeFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	loader := &countingLoader{}
	loader.onTyped = func(int, *packages.Config, []string) ([]*packages.Package, error, bool) {
		cancel()
		return nil, context.Canceled, true
	}
	p := newTestProvider(t)
	p.packagesLoad = loader.load
	g := scopeHandleGraph()
	_, err := p.EnrichRepoContext(semantic.WithCheckoutCompilerScope(ctx, semantic.CheckoutCompilerScope{HandleRoots: true}), g, "", root, nil)
	require.Error(t, err)
	require.ErrorIs(t, err, context.Canceled)
	require.Equal(t, 1, loader.typed, "cancellation must not retry")
	require.Empty(t, g.AllEdges())
}

// TestCheckoutScopeEmptyLoadsNothing: a handle whose Go files a whole-module
// load would never type-check loads nothing and writes no bindings.
func TestCheckoutScopeEmptyLoadsNothing(t *testing.T) {
	root := scopeFixture(t)
	writeFile(t, root, "api/testdata/t.go", "package t\n\nfunc T() {}\n")
	g := graph.New()
	g.AddNode(&graph.Node{ID: "api/testdata/t.go::T", Kind: graph.KindFunction, Name: "T", FilePath: "api/testdata/t.go", StartLine: 3, EndLine: 3, Language: "go"})
	loader := &countingLoader{}
	p := newTestProvider(t)
	p.packagesLoad = loader.load
	result, err := runCheckoutScope(t, p, g, root, semantic.CheckoutCompilerScope{HandleRoots: true})
	require.NoError(t, err)
	require.Equal(t, 0, loader.typed+loader.meta, "nothing may be loaded")
	require.Equal(t, semantic.CompilerScopeHandleRoots, result.Compiler.Scope)
	require.Equal(t, semantic.CompilerScopeReasonEmpty, result.Compiler.ScopeReason)
	require.Equal(t, 0, result.Compiler.Loads)
}

// TestCheckoutScopeWholeModuleReasons: each shape that forces the whole
// module reports its reason on the pass.
func TestCheckoutScopeWholeModuleReasons(t *testing.T) {
	t.Run("go_manifest_changed", func(t *testing.T) {
		root := scopeFixture(t)
		result, err := runCheckoutScope(t, newTestProvider(t), scopeHandleGraph(), root,
			semantic.CheckoutCompilerScope{HandleRoots: true, ManifestChanged: true})
		require.NoError(t, err)
		require.Equal(t, semantic.CompilerScopeFull, result.Compiler.Scope)
		require.Equal(t, semantic.CompilerScopeReasonManifestChanged, result.Compiler.ScopeReason)
		require.Equal(t, 4, result.Compiler.Packages)
	})
	t.Run("scope_path_unmappable", func(t *testing.T) {
		root := scopeFixture(t)
		g := graph.New()
		g.AddNode(&graph.Node{ID: "r/api/api.go::Measure", Kind: graph.KindFunction, Name: "Measure", FilePath: "r/api/api.go", StartLine: 15, EndLine: 20, Language: "go", RepoPrefix: "r"})
		g.AddNode(&graph.Node{ID: "elsewhere/x.go::X", Kind: graph.KindFunction, Name: "X", FilePath: "elsewhere/x.go", StartLine: 1, EndLine: 1, Language: "go", RepoPrefix: "r"})
		ctx := semantic.WithCheckoutCompilerScope(context.Background(), semantic.CheckoutCompilerScope{HandleRoots: true})
		result, err := newTestProvider(t).EnrichRepoContext(ctx, g, "r", root, nil)
		require.NoError(t, err)
		require.Equal(t, semantic.CompilerScopeFull, result.Compiler.Scope)
		require.Equal(t, semantic.CompilerScopeReasonPathUnmappable, result.Compiler.ScopeReason)
	})
	t.Run("scope_multi_module", func(t *testing.T) {
		repo := resolvedTempDir(t)
		module := filepath.Join(repo, "mod")
		writeGoMod(t, module, "example.com/sub")
		writeFile(t, module, "p/p.go", "package p\n\nfunc P() int { return 1 }\n")
		g := graph.New()
		g.AddNode(&graph.Node{ID: "mod/p/p.go::P", Kind: graph.KindFunction, Name: "P", FilePath: "mod/p/p.go", StartLine: 3, EndLine: 3, Language: "go"})
		result, err := runCheckoutScope(t, newTestProvider(t), g, repo, semantic.CheckoutCompilerScope{HandleRoots: true})
		require.NoError(t, err)
		require.Equal(t, semantic.CompilerScopeFull, result.Compiler.Scope)
		require.Equal(t, semantic.CompilerScopeReasonMultiModule, result.Compiler.ScopeReason)
	})
}

// TestCheckoutScopeLineDirectiveAddsPackage: a package outside the handle's
// packages whose source carries a hand-written line directive joins the
// roots (the whole-module load scans it), without a whole-module load.
func TestCheckoutScopeLineDirectiveAddsPackage(t *testing.T) {
	root := scopeFixture(t)
	writeFile(t, root, "gen/gen.go", "package gen\n\n//line api/api.go:15\nfunc Gen() int { return 1 }\n")
	var patternsSeen []string
	loader := &countingLoader{}
	loader.onTyped = func(_ int, _ *packages.Config, patterns []string) ([]*packages.Package, error, bool) {
		patternsSeen = slices.Clone(patterns)
		return nil, nil, false
	}
	p := newTestProvider(t)
	p.packagesLoad = loader.load
	result, err := runCheckoutScope(t, p, scopeHandleGraph(), root, semantic.CheckoutCompilerScope{HandleRoots: true})
	require.NoError(t, err)
	require.Equal(t, []string{"./api", "./gen", "./impl"}, patternsSeen)
	require.Equal(t, semantic.CompilerScopeHandleRoots, result.Compiler.Scope)
	require.Equal(t, semantic.CompilerScopeReasonLineDirective, result.Compiler.ScopeReason)
	require.Equal(t, 3, result.Compiler.Packages)
}

// TestCheckoutScopeStripSiblingBodies: stripping the bodies of the root
// packages' files the handle does not carry changes no output.
func TestCheckoutScopeStripSiblingBodies(t *testing.T) {
	root := scopeFixture(t)
	// A sibling in a handle package, outside the handle, with a body that
	// would otherwise be checked.
	writeFile(t, root, "impl/sibling.go", `package impl

import "strings"

// Sibling is never carried by the handle.
func Sibling() string {
	var b strings.Builder
	b.WriteString("sibling")
	return b.String()
}
`)
	fullGraph, fullProvider := scopeHandleGraph(), newTestProvider(t)
	_, err := runCheckoutScope(t, fullProvider, fullGraph, root, semantic.CheckoutCompilerScope{})
	require.NoError(t, err)
	strippedGraph, strippedProvider := scopeHandleGraph(), newTestProvider(t)
	result, err := runCheckoutScope(t, strippedProvider, strippedGraph, root,
		semantic.CheckoutCompilerScope{HandleRoots: true, StripSiblingBodies: true})
	require.NoError(t, err)
	require.Equal(t, scopeOutput(t, fullGraph, fullProvider), scopeOutput(t, strippedGraph, strippedProvider))
	require.Equal(t, 2, result.Compiler.Packages)
	require.Equal(t, 3, result.Compiler.Files)
}

// TestHasLineDirectiveMatchesOnlyDirectives: only the forms the compiler
// honours count; code that merely mentions the prefix (as this provider's own
// scanner does) must not pull its package into every load.
func TestHasLineDirectiveMatchesOnlyDirectives(t *testing.T) {
	for src, want := range map[string]bool{
		"package p\n\n//line gen.go:10\nfunc F() {}\n":             true,
		"package p\n\nfunc F() { /*line gen.go:3:1*/ }\n":          true,
		"package p\n\nvar s = \"//line \"\n":                       false,
		"package p\n\n// mentions //line directives in prose: 1\n": false,
		"package p\n\nvar b = []byte(\"/*line \")\n":               false,
	} {
		require.Equal(t, want, hasLineDirective([]byte(src)), src)
	}
}
