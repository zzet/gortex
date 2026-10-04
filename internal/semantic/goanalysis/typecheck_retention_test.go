package goanalysis

import (
	"context"
	"go/types"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/tools/go/packages"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/semantic"
)

// The retained compiler state must survive what happens between two
// touches of one package: an edit in flight elsewhere in the checkout
// while a listing runs (a warm-up batch or another package's first pass),
// and the memory cap.

// retentionFixture is a module whose leaf package the tests break (an edit
// in flight: a declaration renamed, its use left behind) and restore; native
// is a cgo package importing leaf, top and side two roots importing native.
type retentionFixture struct {
	root   string
	leaf   string
	broken string
}

func newRetentionFixture(t *testing.T) retentionFixture {
	t.Helper()
	if os.Getenv("CGO_ENABLED") == "0" {
		t.Skip("cgo disabled")
	}
	root := resolvedTempDir(t)
	writeGoMod(t, root, "example.com/retain")
	leaf := "package leaf\n\n// A is New's result.\ntype A struct{ N int }\n\n// New builds an A.\nfunc New() A { return build() }\n\nfunc build() A { return A{N: 1} }\n"
	writeFile(t, root, "leaf/leaf.go", leaf)
	writeFile(t, root, "native/native.go", "package native\n\n// #include <stdlib.h>\nimport \"C\"\n\n// Abs calls into C.\nfunc Abs() int { return int(C.abs(-3)) }\n")
	writeFile(t, root, "native/value.go", "package native\n\nimport \"example.com/retain/leaf\"\n\n// Value's type is leaf.New's result.\nvar Value = leaf.New()\n")
	writeFile(t, root, "top/top.go", "package top\n\nimport \"example.com/retain/native\"\n\n// Use binds native.Value.\nfunc Use() int {\n\tv := native.Value\n\treturn v.N + native.Abs()\n}\n")
	writeFile(t, root, "side/side.go", "package side\n\nimport \"example.com/retain/native\"\n\n// Side is another root over native.\nfunc Side() int {\n\tw := native.Value\n\treturn w.N\n}\n")
	return retentionFixture{root: root, leaf: leaf, broken: strings.Replace(leaf, "func build() A", "func buildRen0() A", 1)}
}

func retentionHandle(rel, fn string, line, end int) func() *graph.Graph {
	return func() *graph.Graph {
		g := graph.New()
		g.AddNode(&graph.Node{ID: rel, Kind: graph.KindFile, Name: rel[strings.LastIndex(rel, "/")+1:], FilePath: rel, StartLine: 1, EndLine: end, Language: "go"})
		g.AddNode(&graph.Node{ID: rel + "::" + fn, Kind: graph.KindFunction, Name: fn, FilePath: rel, StartLine: line, EndLine: end, Language: "go"})
		return g
	}
}

var (
	topHandle    = retentionHandle("top/top.go", "Use", 6, 9)
	sideHandle   = retentionHandle("side/side.go", "Side", 6, 9)
	nativeHandle = retentionHandle("native/value.go", "Value", 1, 6)
)

// retentionPass runs one cached pass over the handle and, when parity is
// set, requires the output of a whole-module load of the same handle.
func retentionPass(t *testing.T, cached *Provider, root string, handle func() *graph.Graph, parity bool, step string) *semantic.CompilerCacheStats {
	t.Helper()
	cached.stateMu.Lock()
	cached.bindingTypes, cached.bindingOwners, cached.bindingKeysByRoot = nil, nil, nil
	cached.stateMu.Unlock()
	cachedGraph := handle()
	result, err := runCheckoutScope(t, cached, cachedGraph, root, cachedScope)
	require.NoError(t, err, step)
	if parity {
		fullGraph, fullProvider := handle(), newTestProvider(t)
		_, err := runCheckoutScope(t, fullProvider, fullGraph, root, semantic.CheckoutCompilerScope{})
		require.NoError(t, err, step)
		require.Equal(t, scopeOutput(t, fullGraph, fullProvider), scopeOutput(t, cachedGraph, cached),
			"%s: the cached pass must write exactly what the whole-module load writes", step)
	}
	require.NotNil(t, result.Compiler, step)
	require.NotNil(t, result.Compiler.Cache, step)
	return result.Compiler.Cache
}

func retainedState(t *testing.T, p *Provider, root string) *checkoutTypecheckState {
	t.Helper()
	p.tcMu.Lock()
	defer p.tcMu.Unlock()
	st := p.tcStates[root]
	require.NotNil(t, st)
	return st
}

// requireRepeatHit is the repeat-touch contract: no listing, no export read,
// no bypass, every sibling file reused.
func requireRepeatHit(t *testing.T, c *semantic.CompilerCacheStats, lists func() int, before int, step string) {
	t.Helper()
	require.Empty(t, c.Bypass, step)
	require.Equal(t, 1, c.ClosureHits, step)
	require.Equal(t, 0, c.ClosureMisses, "%s: miss %q (%s)", step, c.MissReason, c.MissPackage)
	require.Equal(t, before, lists(), "%s: no go list", step)
	require.Equal(t, 0, c.ExportReads, "%s: every import is served from the retained types", step)
}

// TestTypecheckRetentionWarmupMergeDuringEditKeepsTouchedState: a warm-up
// batch listed while leaf did not compile brings native back without export
// data and without its cgo-generated files. Merged over native's retained
// metadata it used to make the next pass over top list again (or bypass to
// the plain load); the retained metadata is kept, and the repeat touch of
// top re-checks only top.
func TestTypecheckRetentionWarmupMergeDuringEditKeepsTouchedState(t *testing.T) {
	fx := newRetentionFixture(t)
	loader := &countingLoader{}
	cached := newTestProvider(t)
	cached.packagesLoad = loader.load
	lists := func() int {
		loader.mu.Lock()
		defer loader.mu.Unlock()
		return loader.meta
	}

	c := retentionPass(t, cached, fx.root, topHandle, true, "cold")
	require.Equal(t, "cold", c.MissReason)
	c = retentionPass(t, cached, fx.root, topHandle, true, "repeat")
	requireRepeatHit(t, c, lists, lists(), "repeat")

	// An edit in flight elsewhere: leaf does not compile while the
	// warm-up's batch lists the module.
	writeFile(t, fx.root, "leaf/leaf.go", fx.broken)
	listing, err := cached.runListing(context.Background(), fx.root, []string{"./..."}, nil)
	require.NoError(t, err)
	listing.warmup = true
	var nativeListed *packages.Package
	for _, pkg := range listing.pkgs {
		if pkg.PkgPath == "example.com/retain/native" {
			nativeListed = pkg
		}
	}
	require.NotNil(t, nativeListed)
	require.Empty(t, nativeListed.ExportFile, "fixture: the listing's native entry is degraded")
	require.True(t, sameFiles(nativeListed.CompiledGoFiles, nativeListed.GoFiles), "fixture: native lost its cgo-generated files")
	st := retainedState(t, cached, fx.root)
	st.mu.Lock()
	st.mergeListing(listing)
	require.NotEmpty(t, st.meta["example.com/retain/native"].ExportFile, "the warm-up never replaces native's valid metadata")
	require.NotEmpty(t, st.meta["example.com/retain/leaf"].ExportFile, "nor leaf's")
	require.NotNil(t, st.view["example.com/retain/native"], "native's retained types survive the merge")
	st.mu.Unlock()

	// The edit is undone; top is touched again.
	writeFile(t, fx.root, "leaf/leaf.go", fx.leaf)
	before := lists()
	c = retentionPass(t, cached, fx.root, topHandle, true, "after warm-up merge")
	requireRepeatHit(t, c, lists, before, "after warm-up merge")
	require.Equal(t, 1, c.FilesParsed, "only the handle file is parsed")
	require.Greater(t, c.WorkingSetPackages, 2)
}

// TestTypecheckRetentionPassListingDuringEditKeepsRetainedMetadata: another
// root's first pass lists its closure while leaf does not compile. The
// listing's native and leaf entries are degraded; the state keeps their
// retained metadata, so the repeat touch of top (after the edit is undone)
// is a hit with retained imports.
func TestTypecheckRetentionPassListingDuringEditKeepsRetainedMetadata(t *testing.T) {
	fx := newRetentionFixture(t)
	loader := &countingLoader{}
	cached := newTestProvider(t)
	cached.packagesLoad = loader.load
	lists := func() int {
		loader.mu.Lock()
		defer loader.mu.Unlock()
		return loader.meta
	}
	retentionPass(t, cached, fx.root, topHandle, true, "cold")

	writeFile(t, fx.root, "leaf/leaf.go", fx.broken)
	c := retentionPass(t, cached, fx.root, sideHandle, false, "side while leaf is broken")
	require.Equal(t, "root_unlisted", c.MissReason)
	require.Greater(t, c.RetainedKept, 0, "the degraded entries left the retained metadata in place")

	writeFile(t, fx.root, "leaf/leaf.go", fx.leaf)
	before := lists()
	c = retentionPass(t, cached, fx.root, topHandle, true, "top again")
	requireRepeatHit(t, c, lists, before, "top again")
}

// TestTypecheckRetentionCgoRootListedDuringBrokenDependencyHeals: a cgo root
// first listed while its dependency did not compile has no generated files.
// While the dependency stays broken the pass uses the plain load; once it
// compiles again the pass lists the root once more and is served from the
// state from then on (it used to bypass to the plain load on every pass).
func TestTypecheckRetentionCgoRootListedDuringBrokenDependencyHeals(t *testing.T) {
	fx := newRetentionFixture(t)
	cached := newTestProvider(t)
	writeFile(t, fx.root, "leaf/leaf.go", fx.broken)
	c := retentionPass(t, cached, fx.root, nativeHandle, false, "native while leaf is broken")
	require.Equal(t, "cgo", c.Bypass, "the dependency is still the broken version: the plain load")

	writeFile(t, fx.root, "leaf/leaf.go", fx.leaf)
	c = retentionPass(t, cached, fx.root, nativeHandle, true, "leaf compiles again")
	require.Empty(t, c.Bypass)
	require.Equal(t, "cgo_unlisted", c.MissReason)
	require.Equal(t, 1, c.ClosureMisses)

	c = retentionPass(t, cached, fx.root, nativeHandle, true, "repeat")
	require.Empty(t, c.Bypass)
	require.Equal(t, 1, c.ClosureHits)
	require.Equal(t, 0, c.ClosureMisses)
	require.Equal(t, 0, c.ExportReads)
}

// retentionPolicyState is a synthetic state: r imports a imports b (the
// working set of a pass over r), c imports d, and e; every dependency holds
// 100 estimated bytes of retained types.
func retentionPolicyState() *checkoutTypecheckState {
	mod := &packages.Module{Path: "m", Main: true}
	st := newCheckoutTypecheckState("/m", "digest")
	st.touch = map[string]uint64{}
	add := func(path string, imports ...string) {
		pkg := &packages.Package{ID: path, PkgPath: path, ExportFile: path + ".a", Module: mod, Imports: map[string]*packages.Package{}}
		for _, imp := range imports {
			pkg.Imports[imp] = st.meta[imp]
		}
		st.meta[path] = pkg
	}
	add("m/b")
	add("m/a", "m/b")
	add("m/r", "m/a")
	add("m/d")
	add("m/c", "m/d")
	add("m/e")
	for _, path := range []string{"m/a", "m/b", "m/c", "m/d", "m/e"} {
		st.view[path] = types.NewPackage(path, path[2:])
		st.viewExport[path] = path + ".a"
		st.viewBytes[path] = 100 * typecheckExportDivisor
		st.exportBytes += 100 * typecheckExportDivisor
	}
	return st
}

func policyWorkingSet() workingSet {
	return workingSet{packages: map[string]bool{"m/r": true, "m/a": true, "m/b": true}, files: map[string]bool{}}
}

func retained(st *checkoutTypecheckState) []string {
	var out []string
	for _, path := range []string{"m/a", "m/b", "m/c", "m/d", "m/e"} {
		if st.view[path] != nil {
			out = append(out, path)
		}
	}
	return out
}

// TestTypecheckRetentionPolicyEvictsLeastRecentlyTouchedOutsideWorkingSet
// pins the memory cap policy inside one checkout: the working set is never
// evicted (even when its touch is older), the least recently touched package
// goes first, and a package never goes before a retained importer.
func TestTypecheckRetentionPolicyEvictsLeastRecentlyTouchedOutsideWorkingSet(t *testing.T) {
	p := newTestProvider(t)

	// Need 300 of 500 bytes: every package outside the working set goes,
	// c before d (d is c's import), although the working set's touch is
	// the oldest here.
	st := retentionPolicyState()
	st.touch = map[string]uint64{"m/a": 0, "m/b": 0, "m/c": 1, "m/d": 3, "m/e": 2}
	res := p.enforceTypecheckBudget(st, 250, policyWorkingSet())
	require.False(t, res.self)
	require.Equal(t, 3, res.packages)
	require.Equal(t, []string{"m/a", "m/b"}, retained(st), "the working set stays")
	require.Equal(t, int64(200), st.estimatedBytes())

	// Need 100: d is the least recently touched but c imports it, so c
	// goes and d stays.
	st = retentionPolicyState()
	st.touch = map[string]uint64{"m/a": 9, "m/b": 9, "m/c": 2, "m/d": 1, "m/e": 3}
	res = p.enforceTypecheckBudget(st, 450, policyWorkingSet())
	require.Equal(t, 1, res.packages)
	require.Equal(t, []string{"m/a", "m/b", "m/d", "m/e"}, retained(st))

	// Need 100 again: e (touched least recently, no importer) goes first.
	st = retentionPolicyState()
	st.touch = map[string]uint64{"m/a": 9, "m/b": 9, "m/c": 4, "m/d": 4, "m/e": 1}
	res = p.enforceTypecheckBudget(st, 450, policyWorkingSet())
	require.Equal(t, 1, res.packages)
	require.Equal(t, []string{"m/a", "m/b", "m/c", "m/d"}, retained(st))

	// The working set alone exceeds the cap: the state is dropped whole.
	st = retentionPolicyState()
	res = p.enforceTypecheckBudget(st, 150, policyWorkingSet())
	require.True(t, res.self)
	require.Empty(t, retained(st))
}

// TestTypecheckRetentionPolicyOrdersCheckoutsByPassTouch: across checkouts
// the cap drops the one a pass touched least recently, not the one used
// least recently (a warm-up uses a checkout without touching it).
func TestTypecheckRetentionPolicyOrdersCheckoutsByPassTouch(t *testing.T) {
	p := newTestProvider(t)
	now := time.Now()
	cur := newCheckoutTypecheckState("/cur", "d")
	x := retentionPolicyState() // touched longest ago, but a warm-up used it last
	x.loadDir, x.touchedAt, x.lastUsed = "/x", now.Add(-10*time.Second), now
	y := retentionPolicyState() // touched more recently, not used since
	y.loadDir, y.touchedAt, y.lastUsed = "/y", now.Add(-5*time.Second), now.Add(-20*time.Second)
	p.tcMu.Lock()
	p.tcStates = map[string]*checkoutTypecheckState{"/cur": cur, "/x": x, "/y": y}
	p.tcMu.Unlock()
	res := p.enforceTypecheckBudget(cur, 600, workingSet{packages: map[string]bool{}, files: map[string]bool{}})
	require.False(t, res.self)
	require.Empty(t, retained(x), "the checkout a pass touched least recently goes")
	require.Len(t, retained(y), 5, "the more recently touched checkout stays")
}

// TestTypecheckRetentionMergeDropsOnlyChangedTypes: a listing that changes
// one package's export data drops that package's retained types and its
// importers', never the rest of the checkout's.
func TestTypecheckRetentionMergeDropsOnlyChangedTypes(t *testing.T) {
	st := retentionPolicyState()
	mod := &packages.Module{Path: "m", Main: true}
	b := &packages.Package{ID: "m/b", PkgPath: "m/b", ExportFile: "m/b.new", Module: mod, Imports: map[string]*packages.Package{}}
	st.mergeListing(&tcListing{pkgs: []*packages.Package{b}, start: time.Now()})
	require.Equal(t, "m/b.new", st.meta["m/b"].ExportFile)
	require.True(t, st.exportStale["m/a"], "a was compiled against b's old export")
	require.Equal(t, []string{"m/c", "m/d", "m/e"}, retained(st), "b and its importer a go; c, d and e stay")
	require.Equal(t, int64(300), st.estimatedBytes())
}
