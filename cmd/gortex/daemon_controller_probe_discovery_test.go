package main

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/daemon"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/indexer"
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

// No topology sweep runs in this fixture. The first public probe must discover
// only its selected checkout and must never cite the primary as exact for it.
func TestProbeDiscoversNewWorktreeWithoutTopologySweep(t *testing.T) {
	for _, name := range []string{"sibling", "nested"} {
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
