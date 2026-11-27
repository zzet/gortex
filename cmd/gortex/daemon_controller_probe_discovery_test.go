package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/daemon"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"github.com/zzet/gortex/internal/search"
	"go.uber.org/zap"
)

func TestProbeServesSymlinkedCheckoutPaths(t *testing.T) {
	c, _, catalog, dir := buildCatalogController(t)
	t.Cleanup(func() { _ = c.lifecycle.Close() })
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	dir, err := filepath.EvalSymlinks(dir)
	require.NoError(t, err)
	primary := filepath.Join(dir, "primary")
	wtGit(t, dir, "init", "-q", "-b", "main", "--", "primary")
	require.NoError(t, os.WriteFile(filepath.Join(primary, "lib.go"), []byte("package fixture\nfunc BaseOnly() {}\n"), 0o644))
	wtGit(t, primary, "add", ".")
	wtGit(t, primary, "commit", "-q", "-m", "base")
	gate := indexer.NewViewBuildGate()
	gate.Open()
	c.lifecycle.SetBuildGate(gate)
	registered, err := c.lifecycle.Register(ctx, config.RepoEntry{Path: primary, Name: "fixture"}, indexer.TrackSourceCLI)
	require.NoError(t, err)
	require.NoError(t, registered.CatalogErr)
	c.viewMaterializer = &graphview.Materializer{Store: c.graph.(*store_sqlite.Store), Catalog: catalog, Leases: c.lifecycle.ViewLeases()}
	alias := filepath.Join(dir, "alias")
	if err := os.Symlink(primary, alias); err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("symlink creation unavailable on this Windows host: %v", err)
		}
		t.Fatal(err)
	}
	canonical := c.selectProbeView(ctx, primary)
	defer canonical.release()
	require.True(t, canonical.answer.Exact)
	for _, path := range []string{alias, filepath.Join(alias, "lib.go")} {
		t.Run(filepath.Base(path), func(t *testing.T) {
			view := c.selectProbeView(ctx, path)
			defer view.release()
			require.True(t, view.servable)
			require.NotNil(t, view.answer)
			require.True(t, view.answer.Exact)
			require.Equal(t, canonical.answer.CheckoutID, view.answer.CheckoutID)
		})
	}
}

func TestProbeRefusesNestedCheckoutWhileUnrelatedIdentityIsPending(t *testing.T) {
	c, _, catalog, dir := buildCatalogController(t)
	t.Cleanup(func() { _ = c.lifecycle.Close() })
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	primary := filepath.Join(dir, "primary")
	wtGit(t, dir, "init", "-q", "-b", "main", "--", "primary")
	require.NoError(t, os.WriteFile(filepath.Join(primary, "lib.go"), []byte("package fixture\nfunc PrimaryOnly1497() {}\n"), 0o644))
	wtGit(t, primary, "add", ".")
	wtGit(t, primary, "commit", "-q", "-m", "base")
	gate := indexer.NewViewBuildGate()
	gate.Open()
	c.lifecycle.SetBuildGate(gate)
	registered, err := c.lifecycle.Register(ctx, config.RepoEntry{Path: primary, Name: "fixture"}, indexer.TrackSourceCLI)
	require.NoError(t, err)
	require.NoError(t, registered.CatalogErr)
	c.viewMaterializer = &graphview.Materializer{Store: c.graph.(*store_sqlite.Store), Catalog: catalog, Leases: c.lifecycle.ViewLeases()}
	control, err := c.SearchSymbols(ctx, daemon.SearchSymbolsParams{Path: primary, Query: "PrimaryOnly1497"})
	require.NoError(t, err)
	require.NotEmpty(t, control.Hits, "the enclosing primary must really contain the symbol")
	linked := filepath.Join(primary, "nested")
	wtGit(t, primary, "worktree", "add", "-q", "--detach", linked)
	require.NoError(t, os.WriteFile(filepath.Join(linked, "lib.go"), []byte("package fixture\nfunc Different() {}\n"), 0o644))
	reg := parser.NewRegistry()
	languages.RegisterAll(reg)
	c.multiIndexer = indexer.NewMultiIndexer(c.graph, reg, search.NewNull(), c.configManager, zap.NewNop())
	c.multiIndexer.DeclareRepos([]config.RepoEntry{{Path: filepath.Join(dir, "unrelated-pending")}})
	require.True(t, c.multiIndexer.RepoIdentityPending())
	result, err := c.SearchSymbols(ctx, daemon.SearchSymbolsParams{Path: linked, Query: "PrimaryOnly1497"})
	require.NoError(t, err)
	require.NotNil(t, result.View)
	require.False(t, result.View.Exact, "pending identity must not restore an invalid enclosing binding")
	require.Equal(t, daemon.ProbeViewUnrouted, result.View.Kind)
	require.Empty(t, result.Hits, "the queried working copy has no PrimaryOnly1497 symbol")
}

// No topology sweep runs in this fixture. The first public probe must discover
// only its selected checkout and must never cite the primary as exact for it.
func TestProbeDiscoversNewWorktreeWithoutTopologySweep(t *testing.T) {
	for _, name := range []string{"sibling", "nested", "sibling_pending"} {
		t.Run(name, func(t *testing.T) {
			c, _, catalog, dir := buildCatalogController(t)
			t.Cleanup(func() { _ = c.lifecycle.Close() })
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			primary, linked := filepath.Join(dir, "primary"), filepath.Join(dir, "linked")
			if name == "nested" {
				linked = filepath.Join(primary, "linked")
			}
			wtGit(t, dir, "init", "-q", "-b", "main", "--", "primary")
			require.NoError(t, os.WriteFile(filepath.Join(primary, "lib.go"), []byte("package fixture\nfunc BaseOnly() {}\n"), 0o644))
			wtGit(t, primary, "add", ".")
			wtGit(t, primary, "commit", "-q", "-m", "base")
			gate := indexer.NewViewBuildGate()
			gate.Open()
			c.lifecycle.SetBuildGate(gate)
			registered, err := c.lifecycle.Register(ctx, config.RepoEntry{Path: primary, Name: "fixture"}, indexer.TrackSourceCLI)
			require.NoError(t, err)
			require.NoError(t, registered.CatalogErr)
			store := c.graph.(*store_sqlite.Store)
			c.viewMaterializer = &graphview.Materializer{Store: store, Catalog: catalog, Leases: c.lifecycle.ViewLeases()}
			release, err := gate.Acquire(ctx, indexer.ViewBuildInteractive)
			require.NoError(t, err)
			release = sync.OnceFunc(release)
			t.Cleanup(release)
			wtGit(t, primary, "worktree", "add", "-q", "--detach", linked)
			before, err := c.lifecycle.ExplainView(ctx, linked)
			require.NoError(t, err)
			require.Equal(t, name == "nested", before.Matched, "nested checkout initially matches its parent")
			if name == "sibling_pending" {
				reg := parser.NewRegistry()
				languages.RegisterAll(reg)
				c.multiIndexer = indexer.NewMultiIndexer(c.graph, reg, search.NewNull(), c.configManager, zap.NewNop())
				c.multiIndexer.DeclareRepos([]config.RepoEntry{{Path: filepath.Join(dir, "unrelated-pending")}})
				require.True(t, c.multiIndexer.RepoIdentityPending())
			}
			configPath := c.configManager.Global().ConfigPath()
			configBefore, err := os.ReadFile(configPath)
			require.NoError(t, err)

			result, err := c.SearchSymbols(ctx, daemon.SearchSymbolsParams{Path: linked, Query: "BaseOnly"})
			require.NoError(t, err)
			require.NotNil(t, result.View)
			require.False(t, result.View.Exact, "a new linked checkout cannot borrow the primary's exactness")
			require.Equal(t, daemon.ProbeViewUnrouted, result.View.Kind)
			require.Empty(t, result.Hits)
			require.Eventually(t, func() bool {
				result, err = c.SearchSymbols(ctx, daemon.SearchSymbolsParams{Path: linked, Query: "BaseOnly"})
				return err == nil && result.View != nil && result.View.CheckoutID != ""
			}, 5*time.Second, 10*time.Millisecond)
			require.False(t, result.View.Exact)
			intents, err := catalog.ListTrackingIntents(ctx, result.View.CheckoutID)
			require.NoError(t, err)
			require.Empty(t, intents)
			graphs, err := catalog.ListDedicatedGraphs(ctx, registered.FamilyID)
			require.NoError(t, err)
			require.Len(t, graphs, 1)
			configAfter, err := os.ReadFile(configPath)
			require.NoError(t, err)
			require.Equal(t, configBefore, configAfter)
		})
	}
}

func TestProbeRefusesNestedCheckoutWithRejectedMarker(t *testing.T) {
	c, _, catalog, dir := buildCatalogController(t)
	t.Cleanup(func() { _ = c.lifecycle.Close() })
	primary := filepath.Join(dir, "primary")
	wtGit(t, dir, "init", "-q", "-b", "main", "--", "primary")
	require.NoError(t, os.WriteFile(filepath.Join(primary, "lib.go"), []byte("package fixture\nfunc PrimaryOnly1497() {}\n"), 0o644))
	wtGit(t, primary, "add", ".")
	wtGit(t, primary, "commit", "-q", "-m", "base")
	gate := indexer.NewViewBuildGate()
	gate.Open()
	c.lifecycle.SetBuildGate(gate)
	registered, err := c.lifecycle.Register(t.Context(), config.RepoEntry{Path: primary, Name: "fixture"}, indexer.TrackSourceCLI)
	require.NoError(t, err)
	require.NoError(t, registered.CatalogErr)
	c.viewMaterializer = &graphview.Materializer{Store: c.graph.(*store_sqlite.Store), Catalog: catalog, Leases: c.lifecycle.ViewLeases()}
	control, err := c.SearchSymbols(t.Context(), daemon.SearchSymbolsParams{Path: primary, Query: "PrimaryOnly1497"})
	require.NoError(t, err)
	require.NotEmpty(t, control.Hits)
	for _, marker := range []string{".git", "commondir"} {
		t.Run(marker, func(t *testing.T) {
			nested := filepath.Join(primary, "nested-"+strings.TrimPrefix(marker, "."))
			require.NoError(t, os.MkdirAll(nested, 0o755))
			path := filepath.Join(nested, ".git")
			if marker == "commondir" {
				require.NoError(t, os.Mkdir(path, 0o755))
				path = filepath.Join(path, marker)
			}
			require.NoError(t, os.WriteFile(path, []byte(strings.Repeat("x", 64*1024+1)), 0o644))
			result, err := c.SearchSymbols(t.Context(), daemon.SearchSymbolsParams{Path: nested, Query: "PrimaryOnly1497"})
			require.NoError(t, err)
			require.NotNil(t, result.View)
			require.False(t, result.View.Exact)
			require.Equal(t, daemon.ProbeViewUnrouted, result.View.Kind)
			require.Empty(t, result.Hits)
		})
	}
}

func TestProbeKnownDescendantsRemainServableDuringDiscoveryPressure(t *testing.T) {
	c, _, catalog, dir := buildCatalogController(t)
	t.Cleanup(func() { _ = c.lifecycle.Close() })
	primary := filepath.Join(dir, "primary")
	wtGit(t, dir, "init", "-q", "-b", "main", "--", "primary")
	require.NoError(t, os.WriteFile(filepath.Join(primary, "lib.go"), []byte("package fixture\nfunc KnownSymbol1497() {}\n"), 0o644))
	wtGit(t, primary, "add", ".")
	wtGit(t, primary, "commit", "-q", "-m", "base")
	gate := indexer.NewViewBuildGate()
	gate.Open()
	c.lifecycle.SetBuildGate(gate)
	registered, err := c.lifecycle.Register(t.Context(), config.RepoEntry{Path: primary, Name: "fixture"}, indexer.TrackSourceCLI)
	require.NoError(t, err)
	require.NoError(t, registered.CatalogErr)
	c.viewMaterializer = &graphview.Materializer{Store: c.graph.(*store_sqlite.Store), Catalog: catalog, Leases: c.lifecycle.ViewLeases()}
	binding, err := c.lifecycle.ExplainView(t.Context(), primary)
	require.NoError(t, err)
	started, unblock := make(chan struct{}, probeDiscoveryFamilyCapacity), make(chan struct{})
	c.probeGitRoot = func(string) (string, bool) { started <- struct{}{}; <-unblock; return primary, true }
	finish := sync.OnceFunc(func() { finishProbeMetadataWorkers(t, c, unblock) })
	defer finish()
	for i := range probeDiscoveryFamilyCapacity {
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan struct{})
		go func() {
			defer close(done)
			_, _, _ = c.probeCheckoutBinding(ctx, filepath.Join(primary, fmt.Sprintf("pending-%d.go", i)), binding)
		}()
		<-started
		cancel()
		<-done
	}
	// Both saturated admission and a stalled worker must answer before the
	// hook's 200ms deadline, with usable data explicitly marked non-exact.
	for _, path := range []string{filepath.Join(primary, "lib.go"), filepath.Join(primary, "pending-0.go")} {
		ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
		result, err := c.SearchSymbols(ctx, daemon.SearchSymbolsParams{Path: path, Query: "KnownSymbol1497"})
		cancel()
		require.NoError(t, err)
		require.NotEmpty(t, result.Hits)
		require.NotNil(t, result.View)
		require.False(t, result.View.Exact)
		require.Equal(t, daemon.FallbackViewBuilding, result.View.FallbackReason)
	}
	finish()
	c.probeGitRoot = nil
	result, err := c.SearchSymbols(t.Context(), daemon.SearchSymbolsParams{Path: filepath.Join(primary, "lib.go"), Query: "KnownSymbol1497"})
	require.NoError(t, err)
	require.NotEmpty(t, result.Hits)
	require.True(t, result.View.Exact)
}
