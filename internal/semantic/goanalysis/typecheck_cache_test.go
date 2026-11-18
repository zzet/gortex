package goanalysis

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/semantic"
)

// The per-checkout closure and type-check state must never change what a
// pass writes: every pass through it is compared with a whole-module load of
// the same handle, over a sequence of edits that hit and miss the state.

var cachedScope = semantic.CheckoutCompilerScope{HandleRoots: true, StripSiblingBodies: true, TypecheckCache: true}

// typecheckCacheFixture is scopeFixture plus a sibling in impl (not in the
// handle) whose body uses the handle's Double.
func typecheckCacheFixture(t *testing.T) string {
	t.Helper()
	root := scopeFixture(t)
	writeFile(t, root, "impl/sibling.go", `package impl

// Sibling is never carried by the handle; its body uses Double.
func Sibling() int {
	x := Double(2)
	return x
}
`)
	return root
}

// cachedPassMatchesFull runs the persistent provider's cached pass and a
// fresh provider's whole-module pass over the same handle and requires the
// same output.
func cachedPassMatchesFull(t *testing.T, step string, cached *Provider, root string) *semantic.CompilerLoadStats {
	t.Helper()
	fullGraph, fullProvider := scopeHandleGraph(), newTestProvider(t)
	_, err := runCheckoutScope(t, fullProvider, fullGraph, root, semantic.CheckoutCompilerScope{})
	require.NoError(t, err, step)
	cachedGraph := scopeHandleGraph()
	// The binding map is the provider's; clear it so each step compares
	// only what this pass wrote.
	cached.stateMu.Lock()
	cached.bindingTypes, cached.bindingOwners, cached.bindingKeysByRoot = nil, nil, nil
	cached.stateMu.Unlock()
	result, err := runCheckoutScope(t, cached, cachedGraph, root, cachedScope)
	require.NoError(t, err, step)
	fullOut, cachedOut := scopeOutput(t, fullGraph, fullProvider), scopeOutput(t, cachedGraph, cached)
	require.Equal(t, fullOut, cachedOut, "%s: the cached pass must write exactly what the whole-module load writes", step)
	joined := strings.Join(cachedOut, "\n")
	for _, want := range []string{
		"edge api/api.go::Measure -calls-> impl/impl.go::Double",
		"edge impl/impl.go::Square -implements-> api/api.go::Shape",
		"ext::go:strings::Builder",
		"binding impl/impl.go:",
	} {
		require.Contains(t, joined, want, "%s: fixture must not be vacuous", step)
	}
	require.NotNil(t, result.Compiler, step)
	require.NotNil(t, result.Compiler.Cache, step)
	require.Equal(t, semantic.CompilerScopeHandleRoots, result.Compiler.Scope, step)
	require.Empty(t, result.Compiler.Cache.Bypass, step)
	return result.Compiler
}

// TestTypecheckCacheDifferentialAcrossEdits walks one checkout through the
// edits the state distinguishes and proves output identity at each step.
func TestTypecheckCacheDifferentialAcrossEdits(t *testing.T) {
	root := typecheckCacheFixture(t)
	loader := &countingLoader{}
	cached := newTestProvider(t)
	cached.packagesLoad = loader.load
	metaLoads := func() int {
		loader.mu.Lock()
		defer loader.mu.Unlock()
		return loader.meta
	}

	// Cold: the closure is listed once.
	c := cachedPassMatchesFull(t, "cold", cached, root)
	require.Equal(t, 1, c.Cache.ClosureMisses)
	require.Equal(t, "cold", c.Cache.MissReason)
	require.Equal(t, 0, loader.typed, "the cached path never runs a type-checking go/packages load")
	require.Greater(t, c.Cache.ExportReads, 0)
	require.Equal(t, 3, c.Files, "api.go, impl.go and the sibling")
	coldMeta := metaLoads()

	// Unchanged: no listing, no export read, every file reused except the
	// handle's (always re-read).
	c = cachedPassMatchesFull(t, "unchanged", cached, root)
	require.Equal(t, 1, c.Cache.ClosureHits)
	require.Equal(t, 0, c.Cache.ClosureMisses)
	require.Equal(t, coldMeta, metaLoads(), "a hit runs no go list (closure nor dependency index)")
	require.Equal(t, 0, c.Cache.ExportReads, "dependency types are served from the retained state")
	require.Greater(t, c.Cache.TypesReused, 0)
	require.Equal(t, 1, c.Cache.FilesReused, "the sibling's parse is reused")
	require.Equal(t, 2, c.Cache.FilesParsed, "handle files are always parsed")
	require.Greater(t, c.Cache.GoListSavedMs+1, int64(0))
	require.Equal(t, 0, c.Cache.DeclChangedFiles)

	// Body-only edit of a handle file: still a hit, no declaration change.
	writeFile(t, root, "impl/impl.go", strings.Replace(readFixture(t, root, "impl/impl.go"),
		"w := helper.Wrap(n)", "w := helper.Wrap(n + 1)", 1))
	c = cachedPassMatchesFull(t, "body edit", cached, root)
	require.Equal(t, 1, c.Cache.ClosureHits)
	require.Equal(t, coldMeta, metaLoads())
	require.Equal(t, 0, c.Cache.DeclChangedFiles, "a body edit changes no declaration")

	// Signature change of a handle file's function that the sibling's body
	// uses: still a hit (same files, same imports); the handle's output is
	// identical to the whole-module load's, and the change is reported so
	// the caller can widen the handle to the sibling.
	writeFile(t, root, "impl/impl.go", strings.Replace(readFixture(t, root, "impl/impl.go"),
		"func Double(n int) int {", "func Double(n int, extra ...int) int {", 1))
	c = cachedPassMatchesFull(t, "signature change", cached, root)
	require.Equal(t, 1, c.Cache.ClosureHits)
	require.Equal(t, 1, c.Cache.DeclChangedFiles, "the changed signature is reported")

	// Sibling edit (not in the handle): the root's file set is unchanged, so
	// still a hit; the sibling is re-parsed.
	writeFile(t, root, "impl/sibling.go", strings.Replace(readFixture(t, root, "impl/sibling.go"),
		"x := Double(2)", "x := Double(2, 3)", 1))
	c = cachedPassMatchesFull(t, "sibling edit", cached, root)
	require.Equal(t, 1, c.Cache.ClosureHits)
	require.Equal(t, 3, c.Cache.FilesParsed, "the edited sibling is parsed again")

	// A dependency outside the handle changes its API (Wrap now returns
	// Boxed, which the handle binds). Only its content changed, so the pass
	// lists nothing: it type-checks the dependency from source (retained
	// export data would bind Wrapped).
	writeFile(t, root, "helper/helper.go", `package helper

// Wrapped is a named type the handle's package binds.
type Wrapped struct {
	N int
}

// Boxed replaces Wrapped as Wrap's result.
type Boxed struct {
	N int
}

// Wrap is outside the handle.
func Wrap(n int) Boxed {
	var w Boxed
	w.N = n
	return w
}
`)
	c = cachedPassMatchesFull(t, "dependency change", cached, root)
	require.Equal(t, 1, c.Cache.ClosureHits)
	require.Equal(t, 0, c.Cache.ClosureMisses)
	require.Empty(t, c.Cache.MissReason)
	require.Equal(t, coldMeta, metaLoads(), "a content-only dependency change runs no go list")
	require.Equal(t, 1, c.Cache.ChangedDependencies)
	require.Equal(t, 1, c.Cache.SourceDependencies, "helper is checked from source")
	require.Equal(t, 1, c.Cache.SourceDependencyFiles)
	require.Empty(t, c.Cache.SourceDependencyFallback)

	// The same state serves the next pass the same way.
	c = cachedPassMatchesFull(t, "dependency still changed", cached, root)
	require.Equal(t, 1, c.Cache.ClosureHits)
	require.Equal(t, coldMeta, metaLoads())
	require.Equal(t, 1, c.Cache.SourceDependencies)

	// A dependency file's build header changes: whether it builds is the
	// go command's call, so the closure is listed again.
	writeFile(t, root, "helper/helper.go", "//go:build !never_set\n\n"+readFixture(t, root, "helper/helper.go"))
	c = cachedPassMatchesFull(t, "dependency header changed", cached, root)
	require.Equal(t, 1, c.Cache.ClosureMisses)
	require.Equal(t, "dependency_changed", c.Cache.MissReason)
	require.Equal(t, 0, c.Cache.SourceDependencies)

	// A dependency gains a file: its listed metadata no longer describes it,
	// so the closure is listed again.
	writeFile(t, root, "helper/extra.go", "package helper\n\n// Extra is new.\nfunc Extra() int { return 1 }\n")
	c = cachedPassMatchesFull(t, "dependency file added", cached, root)
	require.Equal(t, 1, c.Cache.ClosureMisses)
	require.Equal(t, "dependency_changed", c.Cache.MissReason)
	require.Greater(t, c.Cache.ExportReads, 0, "the changed dependency is read again")
	require.Equal(t, 0, c.Cache.SourceDependencies)

	// A handle file gains an import the closure does not hold.
	writeFile(t, root, "api/api.go", strings.Replace(readFixture(t, root, "api/api.go"),
		"import (\n\t\"strings\"\n", "import (\n\t\"strings\"\n\t_ \"encoding/csv\"\n", 1))
	c = cachedPassMatchesFull(t, "import added", cached, root)
	require.Equal(t, 1, c.Cache.ClosureMisses)
	require.Equal(t, "import_added", c.Cache.MissReason)

	// A new file in a root package changes the root's file set.
	writeFile(t, root, "impl/extra.go", "package impl\n\n// Extra is new.\nfunc Extra() int { return 1 }\n")
	c = cachedPassMatchesFull(t, "root file added", cached, root)
	require.Equal(t, 1, c.Cache.ClosureMisses)
	require.Equal(t, "root_files_changed", c.Cache.MissReason)
	require.Equal(t, 4, c.Files)

	// The retained export file disappears (build cache trimmed): the pass
	// relists once and still matches.
	st := cached.tcStates[root]
	require.NotNil(t, st)
	st.mu.Lock()
	st.resetTypes()
	st.meta["example.com/scope/helper"].ExportFile = root + "/missing-export"
	st.mu.Unlock()
	c = cachedPassMatchesFull(t, "export unavailable", cached, root)
	require.Equal(t, "export_unavailable", c.Cache.MissReason)

	status := cached.TypecheckCacheStatus()
	require.Len(t, status, 1)
	require.Greater(t, status[0].Packages, 0)
	require.Greater(t, status[0].EstimateBytes, int64(0))
	require.Greater(t, status[0].Hits, 0)
	require.Greater(t, status[0].Misses, 0)
}

// TestTypecheckCacheBudgetEvictsState: a state over the cap is dropped after
// the pass (the closure metadata stays), so the next pass reads export data
// again but still runs no go list.
func TestTypecheckCacheBudgetEvictsState(t *testing.T) {
	root := typecheckCacheFixture(t)
	cached := newTestProvider(t)
	scope := cachedScope
	scope.TypecheckCacheBytes = 1
	_, err := runCheckoutScope(t, cached, scopeHandleGraph(), root, scope)
	require.NoError(t, err)
	result, err := runCheckoutScope(t, cached, scopeHandleGraph(), root, scope)
	require.NoError(t, err)
	c := result.Compiler.Cache
	require.True(t, c.StateEvicted)
	require.Equal(t, int64(0), c.StateBytes)
	require.Equal(t, 1, c.ClosureHits, "the closure metadata survives eviction")
	require.Greater(t, c.ExportReads, 0, "evicted types are read again")
}

// TestTypecheckCacheBypassesDependencyBetweenRoots: a dependency of one root
// that imports another root must be type-checked from source (go/packages
// does); the cached path hands such a pass to the plain load, which writes
// the whole-module output.
func TestTypecheckCacheBypassesDependencyBetweenRoots(t *testing.T) {
	root := resolvedTempDir(t)
	writeGoMod(t, root, "example.com/between")
	writeFile(t, root, "c/c.go", "package c\n\n// C is a root.\nfunc C() int { return 1 }\n")
	writeFile(t, root, "b/b.go", "package b\n\nimport \"example.com/between/c\"\n\n// B is between the roots.\nfunc B() int { return c.C() }\n")
	writeFile(t, root, "a/a.go", "package a\n\nimport (\n\t\"example.com/between/b\"\n\t\"example.com/between/c\"\n)\n\n// A is a root.\nfunc A() int { return b.B() + c.C() }\n")
	handle := func() *graph.Graph {
		g := graph.New()
		g.AddNode(&graph.Node{ID: "a/a.go", Kind: graph.KindFile, Name: "a.go", FilePath: "a/a.go", StartLine: 1, EndLine: 9, Language: "go"})
		g.AddNode(&graph.Node{ID: "a/a.go::A", Kind: graph.KindFunction, Name: "A", FilePath: "a/a.go", StartLine: 9, EndLine: 9, Language: "go"})
		g.AddNode(&graph.Node{ID: "c/c.go", Kind: graph.KindFile, Name: "c.go", FilePath: "c/c.go", StartLine: 1, EndLine: 4, Language: "go"})
		g.AddNode(&graph.Node{ID: "c/c.go::C", Kind: graph.KindFunction, Name: "C", FilePath: "c/c.go", StartLine: 4, EndLine: 4, Language: "go"})
		return g
	}
	fullGraph, fullProvider := handle(), newTestProvider(t)
	_, err := runCheckoutScope(t, fullProvider, fullGraph, root, semantic.CheckoutCompilerScope{})
	require.NoError(t, err)
	cachedGraph, cached := handle(), newTestProvider(t)
	result, err := runCheckoutScope(t, cached, cachedGraph, root, cachedScope)
	require.NoError(t, err)
	require.Equal(t, "dependency_between_roots", result.Compiler.Cache.Bypass)
	fullOut := scopeOutput(t, fullGraph, fullProvider)
	require.Equal(t, fullOut, scopeOutput(t, cachedGraph, cached))
	require.Contains(t, strings.Join(fullOut, "\n"), "edge a/a.go::A -calls-> c/c.go::C")
}

func readFixture(t *testing.T, root, rel string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, rel))
	require.NoError(t, err)
	return string(data)
}

// TestTypecheckCacheCgoRoot: a root package with a cgo file is type-checked
// over the go command's generated files, which the state reuses while every
// cgo source is unchanged; editing the cgo source relists.
func TestTypecheckCacheCgoRoot(t *testing.T) {
	if os.Getenv("CGO_ENABLED") == "0" {
		t.Skip("cgo disabled")
	}
	root := resolvedTempDir(t)
	writeGoMod(t, root, "example.com/cgoroot")
	writeFile(t, root, "n/native.go", "package n\n\n// #include <stdlib.h>\nimport \"C\"\n\n// Native calls into C.\nfunc Native() int { return int(C.abs(-3)) }\n")
	writeFile(t, root, "n/plain.go", "package n\n\nimport \"strings\"\n\n// Plain is the handle.\nfunc Plain() int {\n\tvar b strings.Builder\n\tb.WriteString(\"x\")\n\treturn b.Len() + Native()\n}\n")
	handle := func() *graph.Graph {
		g := graph.New()
		g.AddNode(&graph.Node{ID: "n/plain.go", Kind: graph.KindFile, Name: "plain.go", FilePath: "n/plain.go", StartLine: 1, EndLine: 10, Language: "go"})
		g.AddNode(&graph.Node{ID: "n/plain.go::Plain", Kind: graph.KindFunction, Name: "Plain", FilePath: "n/plain.go", StartLine: 6, EndLine: 10, Language: "go"})
		return g
	}
	cached := newTestProvider(t)
	pass := func(step string) *semantic.CompilerCacheStats {
		fullGraph, fullProvider := handle(), newTestProvider(t)
		_, err := runCheckoutScope(t, fullProvider, fullGraph, root, semantic.CheckoutCompilerScope{})
		require.NoError(t, err, step)
		cachedGraph := handle()
		cached.stateMu.Lock()
		cached.bindingTypes, cached.bindingOwners, cached.bindingKeysByRoot = nil, nil, nil
		cached.stateMu.Unlock()
		result, err := runCheckoutScope(t, cached, cachedGraph, root, cachedScope)
		require.NoError(t, err, step)
		fullOut := scopeOutput(t, fullGraph, fullProvider)
		require.Equal(t, fullOut, scopeOutput(t, cachedGraph, cached), step)
		require.Contains(t, strings.Join(fullOut, "\n"), "ext::go:strings::Builder", step)
		require.Empty(t, result.Compiler.Cache.Bypass, step)
		return result.Compiler.Cache
	}
	c := pass("cold")
	require.Equal(t, "cold", c.MissReason)
	c = pass("unchanged")
	require.Equal(t, 1, c.ClosureHits)
	writeFile(t, root, "n/plain.go", strings.Replace(readFixture(t, root, "n/plain.go"), "WriteString(\"x\")", "WriteString(\"y\")", 1))
	c = pass("handle body edit")
	require.Equal(t, 1, c.ClosureHits)
	writeFile(t, root, "n/native.go", strings.Replace(readFixture(t, root, "n/native.go"), "-3", "-4", 1))
	c = pass("cgo source edit")
	require.Equal(t, 1, c.ClosureMisses)
	require.Equal(t, "cgo_changed", c.MissReason)
}

// TestTypecheckCacheChangedDependencyChecksImportersFromSource: a changed
// dependency's importers inside the closure are type-checked from source
// too. Their export data was compiled against the old dependency: mid's
// exported Value takes its type from leaf.New, so mid's retained export
// data would bind the handle's v to leaf.A after leaf.New starts returning
// a leaf.B. The pass must write what a whole-module load writes, without a
// listing.
func TestTypecheckCacheChangedDependencyChecksImportersFromSource(t *testing.T) {
	root := resolvedTempDir(t)
	writeGoMod(t, root, "example.com/chain")
	leafSrc := func(result string, extra string) string {
		return "package leaf\n\n" + extra + "// A is New's first result.\ntype A struct{ N int }\n\n// B is New's later result.\ntype B struct{ N int }\n\n// New is two packages below the handle.\nfunc New() " + result + " { return " + result + "{} }\n"
	}
	writeFile(t, root, "leaf/leaf.go", leafSrc("A", ""))
	writeFile(t, root, "mid/mid.go", "package mid\n\nimport \"example.com/chain/leaf\"\n\n// Value's type is leaf.New's result.\nvar Value = leaf.New()\n")
	writeFile(t, root, "other/other.go", "package other\n\n// Other is in the closure but imports nothing that changes.\nfunc Other() int { return 2 }\n")
	writeFile(t, root, "top/top.go", "package top\n\nimport (\n\t\"example.com/chain/mid\"\n\t\"example.com/chain/other\"\n)\n\n// Use binds mid.Value.\nfunc Use() int {\n\tv := mid.Value\n\treturn v.N + other.Other()\n}\n")
	handle := func() *graph.Graph {
		g := graph.New()
		g.AddNode(&graph.Node{ID: "top/top.go", Kind: graph.KindFile, Name: "top.go", FilePath: "top/top.go", StartLine: 1, EndLine: 12, Language: "go"})
		g.AddNode(&graph.Node{ID: "top/top.go::Use", Kind: graph.KindFunction, Name: "Use", FilePath: "top/top.go", StartLine: 9, EndLine: 12, Language: "go"})
		return g
	}
	loader := &countingLoader{}
	cached := newTestProvider(t)
	cached.packagesLoad = loader.load
	metaLoads := func() int {
		loader.mu.Lock()
		defer loader.mu.Unlock()
		return loader.meta
	}
	pass := func(step string) (*semantic.CompilerCacheStats, string) {
		t.Helper()
		fullGraph, fullProvider := handle(), newTestProvider(t)
		_, err := runCheckoutScope(t, fullProvider, fullGraph, root, semantic.CheckoutCompilerScope{})
		require.NoError(t, err, step)
		cachedGraph := handle()
		cached.stateMu.Lock()
		cached.bindingTypes, cached.bindingOwners, cached.bindingKeysByRoot = nil, nil, nil
		cached.stateMu.Unlock()
		result, err := runCheckoutScope(t, cached, cachedGraph, root, cachedScope)
		require.NoError(t, err, step)
		fullOut := scopeOutput(t, fullGraph, fullProvider)
		require.Equal(t, fullOut, scopeOutput(t, cachedGraph, cached), "%s: the cached pass must write exactly what the whole-module load writes", step)
		require.Empty(t, result.Compiler.Cache.Bypass, step)
		return result.Compiler.Cache, strings.Join(fullOut, "\n")
	}

	c, out := pass("cold")
	require.Equal(t, "cold", c.MissReason)
	require.Contains(t, out, "binding top/top.go:10:v=A")
	coldMeta := metaLoads()

	writeFile(t, root, "leaf/leaf.go", leafSrc("B", ""))
	c, out = pass("leaf changed")
	require.Contains(t, out, "binding top/top.go:10:v=B", "mid is checked against the new leaf")
	require.Equal(t, 1, c.ClosureHits)
	require.Equal(t, coldMeta, metaLoads(), "no listing")
	require.Equal(t, 1, c.ChangedDependencies)
	require.Equal(t, 2, c.SourceDependencies, "leaf and its importer mid; other is untouched")
	require.Equal(t, 2, c.SourceDependencyFiles)

	// mid itself changes too: still no listing.
	writeFile(t, root, "mid/mid.go", "package mid\n\nimport \"example.com/chain/leaf\"\n\n// Value's type is leaf.New's result.\nvar Value = leaf.New() // changed\n")
	c, out = pass("mid changed")
	require.Contains(t, out, "binding top/top.go:10:v=B")
	require.Equal(t, coldMeta, metaLoads(), "no listing")
	require.Equal(t, 2, c.ChangedDependencies)
	require.Equal(t, 2, c.SourceDependencies)

	// A changed dependency that now imports a package the closure does not
	// hold: the pass lists again.
	writeFile(t, root, "leaf/leaf.go", leafSrc("B", "import \"strconv\"\n\n// Unused keeps the import.\nvar Unused = strconv.Itoa(1)\n\n"))
	c, out = pass("leaf import added")
	require.Contains(t, out, "binding top/top.go:10:v=B")
	require.Equal(t, 1, c.ClosureMisses)
	require.Equal(t, "dependency_changed", c.MissReason)
	require.Equal(t, "import_added", c.SourceDependencyFallback)
	require.Equal(t, 0, c.SourceDependencies)
}

// TestTypecheckCacheCgoImporterOfChangedDependency: a cgo package that
// imports a changed dependency is checked from source over the go command's
// generated files while its cgo sources are unchanged; editing a cgo source
// makes the pass list again.
func TestTypecheckCacheCgoImporterOfChangedDependency(t *testing.T) {
	if os.Getenv("CGO_ENABLED") == "0" {
		t.Skip("cgo disabled")
	}
	root := resolvedTempDir(t)
	writeGoMod(t, root, "example.com/cgochain")
	leafSrc := func(result string) string {
		return "package leaf\n\n// A is New's first result.\ntype A struct{ N int }\n\n// B is New's later result.\ntype B struct{ N int }\n\n// New is below the cgo package.\nfunc New() " + result + " { return " + result + "{} }\n"
	}
	writeFile(t, root, "leaf/leaf.go", leafSrc("A"))
	writeFile(t, root, "native/native.go", "package native\n\n// #include <stdlib.h>\nimport \"C\"\n\n// Abs calls into C.\nfunc Abs() int { return int(C.abs(-3)) }\n")
	writeFile(t, root, "native/value.go", "package native\n\nimport \"example.com/cgochain/leaf\"\n\n// Value's type is leaf.New's result.\nvar Value = leaf.New()\n")
	writeFile(t, root, "top/top.go", "package top\n\nimport \"example.com/cgochain/native\"\n\n// Use binds native.Value.\nfunc Use() int {\n\tv := native.Value\n\treturn v.N + native.Abs()\n}\n")
	handle := func() *graph.Graph {
		g := graph.New()
		g.AddNode(&graph.Node{ID: "top/top.go", Kind: graph.KindFile, Name: "top.go", FilePath: "top/top.go", StartLine: 1, EndLine: 9, Language: "go"})
		g.AddNode(&graph.Node{ID: "top/top.go::Use", Kind: graph.KindFunction, Name: "Use", FilePath: "top/top.go", StartLine: 6, EndLine: 9, Language: "go"})
		return g
	}
	cached := newTestProvider(t)
	pass := func(step string) (*semantic.CompilerCacheStats, string) {
		t.Helper()
		fullGraph, fullProvider := handle(), newTestProvider(t)
		_, err := runCheckoutScope(t, fullProvider, fullGraph, root, semantic.CheckoutCompilerScope{})
		require.NoError(t, err, step)
		cachedGraph := handle()
		cached.stateMu.Lock()
		cached.bindingTypes, cached.bindingOwners, cached.bindingKeysByRoot = nil, nil, nil
		cached.stateMu.Unlock()
		result, err := runCheckoutScope(t, cached, cachedGraph, root, cachedScope)
		require.NoError(t, err, step)
		fullOut := scopeOutput(t, fullGraph, fullProvider)
		require.Equal(t, fullOut, scopeOutput(t, cachedGraph, cached), "%s: the cached pass must write exactly what the whole-module load writes", step)
		require.Empty(t, result.Compiler.Cache.Bypass, step)
		return result.Compiler.Cache, strings.Join(fullOut, "\n")
	}
	c, out := pass("cold")
	require.Equal(t, "cold", c.MissReason)
	require.Contains(t, out, "binding top/top.go:7:v=A")

	writeFile(t, root, "leaf/leaf.go", leafSrc("B"))
	c, out = pass("leaf changed")
	require.Contains(t, out, "binding top/top.go:7:v=B")
	require.Equal(t, 0, c.ClosureMisses, "the cgo importer is checked over its generated files")
	require.Equal(t, 2, c.SourceDependencies)
	require.Empty(t, c.SourceDependencyFallback)

	writeFile(t, root, "native/native.go", strings.Replace(readFixture(t, root, "native/native.go"), "-3", "-4", 1))
	c, out = pass("cgo source changed")
	require.Contains(t, out, "binding top/top.go:7:v=B")
	require.Equal(t, 1, c.ClosureMisses)
	require.Equal(t, "cgo_changed", c.SourceDependencyFallback)
}
