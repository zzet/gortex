package goanalysis

import (
	"context"
	"go/types"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/tools/go/packages"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/semantic"
)

// The background whole-module listing must only ever turn a first pass's
// cold listing into a hit: every pass below is compared with a fresh
// whole-module load of the same handle.

// otherHandleGraph is a handle carrying only other/other.go, a package no
// pass over the api/impl handle lists.
func otherHandleGraph() *graph.Graph {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "other/other.go", Kind: graph.KindFile, Name: "other.go", FilePath: "other/other.go", StartLine: 1, EndLine: 10, Language: "go"})
	g.AddNode(&graph.Node{ID: "other/other.go::Other", Kind: graph.KindFunction, Name: "Other", FilePath: "other/other.go", StartLine: 6, EndLine: 10, Language: "go"})
	return g
}

// handlePassMatchesFull runs the cached provider's pass and a fresh
// provider's whole-module pass over the same handle and requires the same
// output (and some of it).
func handlePassMatchesFull(t *testing.T, step string, cached *Provider, root string, handle func() *graph.Graph, want ...string) *semantic.CompilerCacheStats {
	t.Helper()
	fullGraph, fullProvider := handle(), newTestProvider(t)
	_, err := runCheckoutScope(t, fullProvider, fullGraph, root, semantic.CheckoutCompilerScope{})
	require.NoError(t, err, step)
	cached.stateMu.Lock()
	cached.bindingTypes, cached.bindingOwners, cached.bindingKeysByRoot = nil, nil, nil
	cached.stateMu.Unlock()
	cachedGraph := handle()
	result, err := runCheckoutScope(t, cached, cachedGraph, root, cachedScope)
	require.NoError(t, err, step)
	fullOut := scopeOutput(t, fullGraph, fullProvider)
	require.Equal(t, fullOut, scopeOutput(t, cachedGraph, cached), "%s: the cached pass must write exactly what the whole-module load writes", step)
	joined := strings.Join(fullOut, "\n")
	for _, w := range want {
		require.Contains(t, joined, w, "%s: fixture must not be vacuous", step)
	}
	require.NotNil(t, result.Compiler, step)
	require.NotNil(t, result.Compiler.Cache, step)
	require.Empty(t, result.Compiler.Cache.Bypass, step)
	return result.Compiler.Cache
}

func waitWarm(t *testing.T, p *Provider, root string) CheckoutWarmupStatus {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	status := p.waitCheckoutWarmup(ctx, root)
	require.Equal(t, warmupWarm, status.State, "warm-up status %+v", status)
	return status
}

// TestTypecheckCacheWarmupServesFirstEdit: after the background listing,
// the first pass over packages no pass listed is a closure hit with no go
// command at all, and its output is the whole-module load's.
func TestTypecheckCacheWarmupServesFirstEdit(t *testing.T) {
	root := typecheckCacheFixture(t)
	loader := &countingLoader{}
	cached := newTestProvider(t)
	cached.packagesLoad = loader.load
	cached.warm.quiet = 10 * time.Millisecond
	t.Cleanup(func() { _ = cached.Close() })

	require.Equal(t, warmupOutcomeDisabled, cached.WarmCheckoutCompiler(root, semantic.CheckoutCompilerScope{HandleRoots: true}))
	require.Equal(t, warmupOutcomeStarted, cached.WarmCheckoutCompiler(root, cachedScope))
	status := waitWarm(t, cached, root)
	require.Equal(t, 1, status.Attempts)
	require.Greater(t, status.Packages, 4, "the whole module and its standard-library closure")
	require.Equal(t, warmupOutcomeWarm, cached.WarmCheckoutCompiler(root, cachedScope), "a warm checkout is not listed again")
	loader.mu.Lock()
	metaAfterWarmup := loader.meta
	loader.mu.Unlock()
	require.Equal(t, 2, metaAfterWarmup, "one package enumeration (names and files) and one batch listing")
	require.Equal(t, 1, status.Batches)
	require.Equal(t, 0, status.Remaining)

	for _, step := range []struct {
		name   string
		handle func() *graph.Graph
		want   []string
	}{
		{"first pass over api and impl", scopeHandleGraph, []string{
			"edge api/api.go::Measure -calls-> impl/impl.go::Double",
			"edge impl/impl.go::Square -implements-> api/api.go::Shape",
			"binding impl/impl.go:",
		}},
		{"first pass over other", otherHandleGraph, []string{"ext::go:strings::Builder"}},
	} {
		c := handlePassMatchesFull(t, step.name, cached, root, step.handle, step.want...)
		require.Equal(t, 1, c.ClosureHits, step.name)
		require.Equal(t, 0, c.ClosureMisses, step.name)
		require.Equal(t, int64(0), c.GoListMs, step.name)
		require.True(t, c.WarmServed, step.name)
		require.Equal(t, warmupWarm, c.WarmupState, step.name)
		require.Greater(t, c.WarmupPackages, 0, step.name)
	}
	loader.mu.Lock()
	require.Equal(t, metaAfterWarmup, loader.meta, "no pass ran a go command of its own")
	loader.mu.Unlock()

	rows := cached.TypecheckCacheStatus()
	require.Len(t, rows, 1)
	require.Equal(t, 2, rows[0].WarmHits)
	require.Equal(t, warmupWarm, rows[0].Warmup.State)
	require.Greater(t, rows[0].Warmup.Ms+1, int64(0))
}

// TestTypecheckCacheWarmupYieldsToCompilerLoads: a compiler load that starts
// while the warm-up lists cancels the listing; the pass proceeds as without
// a warm-up (it lists its own closure), and the warm-up lists again once
// the provider is quiet, after which a never-listed package hits.
func TestTypecheckCacheWarmupYieldsToCompilerLoads(t *testing.T) {
	root := typecheckCacheFixture(t)
	cached := newTestProvider(t)
	cached.warm.quiet = 50 * time.Millisecond
	t.Cleanup(func() { _ = cached.Close() })
	var (
		mu       sync.Mutex
		warmups  int
		started  = make(chan struct{})
		canceled = make(chan struct{})
	)
	cached.packagesLoad = func(cfg *packages.Config, patterns ...string) ([]*packages.Package, error) {
		if warmupListingFlags(cfg) {
			mu.Lock()
			warmups++
			first := warmups == 1
			mu.Unlock()
			if first {
				// The first warm-up listing blocks until it is preempted.
				close(started)
				<-cfg.Context.Done()
				close(canceled)
				return nil, cfg.Context.Err()
			}
		}
		return packages.Load(cfg, patterns...)
	}

	require.Equal(t, warmupOutcomeStarted, cached.WarmCheckoutCompiler(root, cachedScope))
	<-started
	require.Equal(t, warmupOutcomeRunning, cached.WarmCheckoutCompiler(root, cachedScope))

	c := handlePassMatchesFull(t, "pass during the warm-up", cached, root, scopeHandleGraph,
		"edge api/api.go::Measure -calls-> impl/impl.go::Double")
	select {
	case <-canceled:
	case <-time.After(time.Minute):
		t.Fatal("the pass's listing did not preempt the warm-up")
	}
	require.Equal(t, 1, c.ClosureMisses, "the pass lists its own closure, as without a warm-up")
	require.Equal(t, "cold", c.MissReason)
	require.False(t, c.WarmServed)

	status := waitWarm(t, cached, root)
	require.Equal(t, 2, status.Attempts, "the preempted listing ran again")
	require.Equal(t, 1, status.Preemptions)
	mu.Lock()
	require.Equal(t, 2, warmups)
	mu.Unlock()

	c = handlePassMatchesFull(t, "first pass over other", cached, root, otherHandleGraph, "ext::go:strings::Builder")
	require.Equal(t, 1, c.ClosureHits)
	require.Equal(t, int64(0), c.GoListMs)
	require.True(t, c.WarmServed)
	require.Equal(t, 1, c.WarmupPreemptions)
}

// TestTypecheckCacheWarmupRewarmsOnModuleChange: a go.mod change starts the
// checkout's state over; asking again lists the module under the new
// manifests, and the first pass after it hits.
func TestTypecheckCacheWarmupRewarmsOnModuleChange(t *testing.T) {
	root := typecheckCacheFixture(t)
	cached := newTestProvider(t)
	cached.warm.quiet = 10 * time.Millisecond
	t.Cleanup(func() { _ = cached.Close() })

	require.Equal(t, warmupOutcomeStarted, cached.WarmCheckoutCompiler(root, cachedScope))
	first := waitWarm(t, cached, root)
	stateBefore := cached.tcStates[root]

	writeFile(t, root, "go.mod", readFixture(t, root, "go.mod")+"\n// a manifest edit\n")
	require.Equal(t, warmupOutcomeStarted, cached.WarmCheckoutCompiler(root, cachedScope), "new manifests are warmed again")
	second := waitWarm(t, cached, root)
	require.NotEqual(t, first.Digest, second.Digest)
	require.NotSame(t, stateBefore, cached.tcStates[root], "the manifest change started the state over")
	require.Equal(t, warmupOutcomeWarm, cached.WarmCheckoutCompiler(root, cachedScope))

	c := handlePassMatchesFull(t, "first pass after the manifest change", cached, root, otherHandleGraph, "ext::go:strings::Builder")
	require.Equal(t, 1, c.ClosureHits)
	require.True(t, c.WarmServed)
	require.Equal(t, int64(0), c.GoListMs)
}

// TestTypecheckCacheWarmupNeverServesAStaleDependent: the warm-up lists mid
// (which re-exports helper.Wrap) against helper's old API. After helper
// changes, mid's old export data must never be served: a pass over top
// (which uses mid.Make's result) would otherwise bind helper's old result
// type. helper's change is content-only, so both passes check it (and, for
// top, its importer mid) from source instead of listing.
func TestTypecheckCacheWarmupNeverServesAStaleDependent(t *testing.T) {
	root := resolvedTempDir(t)
	writeGoMod(t, root, "example.com/stale")
	writeFile(t, root, "helper/helper.go", `package helper

// Wrapped is Wrap's first result type.
type Wrapped struct {
	N int
}

// Boxed is Wrap's later result type.
type Boxed struct {
	N int
}

// Wrap is re-exported by mid.
func Wrap(n int) Wrapped {
	var w Wrapped
	w.N = n
	return w
}
`)
	writeFile(t, root, "mid/mid.go", "package mid\n\nimport \"example.com/stale/helper\"\n\n// Make is whatever helper.Wrap is.\nvar Make = helper.Wrap\n")
	writeFile(t, root, "top/top.go", "package top\n\nimport \"example.com/stale/mid\"\n\n// Top binds Make's result.\nfunc Top() int {\n\tw := mid.Make(1)\n\treturn w.N\n}\n")
	writeFile(t, root, "direct/direct.go", "package direct\n\nimport \"example.com/stale/helper\"\n\n// Direct uses helper without mid.\nfunc Direct() int {\n\tw := helper.Wrap(2)\n\treturn w.N\n}\n")
	topHandle := func() *graph.Graph {
		g := graph.New()
		g.AddNode(&graph.Node{ID: "top/top.go", Kind: graph.KindFile, Name: "top.go", FilePath: "top/top.go", StartLine: 1, EndLine: 9, Language: "go"})
		g.AddNode(&graph.Node{ID: "top/top.go::Top", Kind: graph.KindFunction, Name: "Top", FilePath: "top/top.go", StartLine: 6, EndLine: 9, Language: "go"})
		return g
	}
	directHandle := func() *graph.Graph {
		g := graph.New()
		g.AddNode(&graph.Node{ID: "direct/direct.go", Kind: graph.KindFile, Name: "direct.go", FilePath: "direct/direct.go", StartLine: 1, EndLine: 9, Language: "go"})
		g.AddNode(&graph.Node{ID: "direct/direct.go::Direct", Kind: graph.KindFunction, Name: "Direct", FilePath: "direct/direct.go", StartLine: 6, EndLine: 9, Language: "go"})
		return g
	}

	cached := newTestProvider(t)
	cached.warm.quiet = 10 * time.Millisecond
	t.Cleanup(func() { _ = cached.Close() })
	require.Equal(t, warmupOutcomeStarted, cached.WarmCheckoutCompiler(root, cachedScope))
	waitWarm(t, cached, root)

	c := handlePassMatchesFull(t, "top before the change", cached, root, topHandle, "binding top/top.go:7:w=Wrapped")
	require.Equal(t, 1, c.ClosureHits)

	// helper.Wrap now returns Boxed; mid's source is unchanged.
	writeFile(t, root, "helper/helper.go", strings.Replace(readFixture(t, root, "helper/helper.go"),
		"func Wrap(n int) Wrapped {\n\tvar w Wrapped", "func Wrap(n int) Boxed {\n\tvar w Boxed", 1))
	c = handlePassMatchesFull(t, "direct after the change", cached, root, directHandle, "binding direct/direct.go:7:w=Boxed")
	require.Empty(t, c.MissReason, "the direct pass lists nothing")
	require.Equal(t, 1, c.SourceDependencies, "helper is checked from source")

	c = handlePassMatchesFull(t, "top after the change", cached, root, topHandle, "binding top/top.go:7:w=Boxed")
	require.Equal(t, 0, c.ClosureMisses)
	require.Equal(t, 2, c.SourceDependencies, "mid was compiled against the old helper: checked from source with it")
}

// TestTypecheckCacheMergeKeepsNewerMetadata: a listing that started earlier
// never replaces metadata a later listing merged, and a package listed
// against an export file the state no longer holds is marked export-stale,
// whichever order the listings merge in.
func TestTypecheckCacheMergeKeepsNewerMetadata(t *testing.T) {
	mod := &packages.Module{Path: "example.com/m", Main: true}
	pkg := func(path, export string, imports ...*packages.Package) *packages.Package {
		p := &packages.Package{ID: path, PkgPath: path, ExportFile: export, Module: mod, Imports: map[string]*packages.Package{}}
		for _, imp := range imports {
			p.Imports[imp.PkgPath] = imp
		}
		return p
	}
	t0 := time.Now()
	older := func() *tcListing {
		b := pkg("example.com/m/b", "b-old")
		return &tcListing{pkgs: []*packages.Package{pkg("example.com/m/a", "a-old", b), b}, start: t0, warmup: true}
	}
	newer := func() *tcListing {
		return &tcListing{pkgs: []*packages.Package{pkg("example.com/m/b", "b-new")}, start: t0.Add(time.Second)}
	}
	for _, order := range []string{"older first", "newer first"} {
		st := newCheckoutTypecheckState("/m", "digest")
		if order == "older first" {
			st.mergeListing(older())
			st.viewExport["example.com/m/b"] = "b-old"
			st.view["example.com/m/b"] = types.NewPackage("example.com/m/b", "b")
			st.mergeListing(newer())
			require.Empty(t, st.view, "%s: types read from b's old export are dropped", order)
		} else {
			st.mergeListing(newer())
			st.mergeListing(older())
		}
		require.Equal(t, "b-new", st.meta["example.com/m/b"].ExportFile, order)
		require.True(t, st.exportStale["example.com/m/a"], "%s: a was compiled against b's old export: kept, never read from export", order)
		require.False(t, st.exportStale["example.com/m/b"], order)
		require.False(t, st.fromWarmup["example.com/m/b"], order)
	}
}
