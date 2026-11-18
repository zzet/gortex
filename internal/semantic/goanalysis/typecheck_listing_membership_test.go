package goanalysis

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/tools/go/packages"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/semantic"
)

func TestTypecheckCacheRejectsListingWithNewerFileManifest(t *testing.T) {
	root := typecheckCacheFixture(t)
	cached := newTestProvider(t)
	cachedPassMatchesFull(t, "cold", cached, root)
	writeFile(t, root, "impl/excluded.go", "//go:build gortex_never_enabled\n\npackage impl\nfunc Excluded() missingType { return missingValue }\n")

	// Reproduce the warm-up race exactly: package metadata has already
	// returned when a new source arrives, before runListing scans its manifest.
	added := "impl/added.go"
	cached.packagesLoad = func(cfg *packages.Config, patterns ...string) ([]*packages.Package, error) {
		pkgs, err := packages.Load(cfg, patterns...)
		if err == nil {
			writeFile(t, root, added, "package impl\n\nfunc Added() int { return Double(3) }\n")
		}
		return pkgs, err
	}
	listing, err := cached.runListing(context.Background(), root, []string{"./..."}, nil)
	require.NoError(t, err)
	cached.packagesLoad = nil
	listing.warmup = true
	st := cached.tcStates[root]
	require.NotNil(t, st)
	st.mu.Lock()
	st.mergeListing(listing)
	meta := st.byDir[filepath.Join(root, "impl")]
	require.NotContains(t, meta.GoFiles, filepath.Join(root, added), "metadata predates the added source")
	require.Contains(t, st.manifests[meta.PkgPath].files, "added.go", "manifest observed the later source")
	require.Contains(t, meta.IgnoredFiles, filepath.Join(root, "impl/excluded.go"), "intentional compiler exclusions remain accounted for")
	st.mu.Unlock()

	pass := func(step, file string) *semantic.CompilerLoadStats {
		t.Helper()
		handle := func() *graph.Graph {
			g := scopeHandleGraph()
			if file != "" {
				g.AddNode(&graph.Node{ID: file, Kind: graph.KindFile, Name: filepath.Base(file), FilePath: file, StartLine: 1, EndLine: 3, Language: "go"})
				g.AddNode(&graph.Node{ID: file + "::Added", Kind: graph.KindFunction, Name: "Added", FilePath: file, StartLine: 3, EndLine: 3, Language: "go"})
			}
			return g
		}
		fullGraph, full := handle(), newTestProvider(t)
		_, err := runCheckoutScope(t, full, fullGraph, root, semantic.CheckoutCompilerScope{})
		require.NoError(t, err, step)
		cached.stateMu.Lock()
		cached.bindingTypes, cached.bindingOwners, cached.bindingKeysByRoot = nil, nil, nil
		cached.stateMu.Unlock()
		cachedGraph := handle()
		result, err := runCheckoutScope(t, cached, cachedGraph, root, cachedScope)
		require.NoError(t, err, step)
		require.Equal(t, scopeOutput(t, fullGraph, full), scopeOutput(t, cachedGraph, cached), step)
		if file != "" {
			require.Contains(t, strings.Join(scopeOutput(t, cachedGraph, cached), "\n"), "edge "+file+"::Added -calls-> impl/impl.go::Double", step)
		}
		require.Empty(t, result.Compiler.Cache.Bypass, step)
		return result.Compiler
	}
	c := pass("new file behind listing", added)
	require.Equal(t, "root_files_changed", c.Cache.MissReason)
	require.Equal(t, 1, c.Cache.ClosureMisses)
	require.Equal(t, 4, c.Files)
	c = pass("unchanged including excluded source", added)
	require.Equal(t, 1, c.Cache.ClosureHits)
	require.Zero(t, c.Cache.ClosureMisses)
	writeFile(t, root, added, "package impl\n\nfunc Added() int { return Double(4) }\n")
	c = pass("body edit", added)
	require.Equal(t, 1, c.Cache.ClosureHits)
	require.Zero(t, c.Cache.ClosureMisses)
	renamed := "impl/renamed.go"
	require.NoError(t, os.Rename(filepath.Join(root, added), filepath.Join(root, renamed)))
	c = pass("rename", renamed)
	require.Equal(t, "root_files_changed", c.Cache.MissReason)
	require.Equal(t, 1, c.Cache.ClosureMisses)
	require.NoError(t, os.Remove(filepath.Join(root, renamed)))
	c = pass("delete", "")
	require.Equal(t, "root_files_changed", c.Cache.MissReason)
	require.Equal(t, 1, c.Cache.ClosureMisses)
	require.Equal(t, 3, c.Files)
}
