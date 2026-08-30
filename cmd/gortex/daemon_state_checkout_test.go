package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"github.com/zzet/gortex/internal/search"
)

func TestWarmupDefersConfiguredGitCorpusToLifecyclePromotion(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git binary not available")
	}
	ctx := context.Background()
	dir := t.TempDir()
	repo := filepath.Join(dir, "repo")
	require.NoError(t, os.MkdirAll(repo, 0o755))
	runCheckoutWarmupGit(t, repo, "init", "-q", "-b", "main")
	runCheckoutWarmupGit(t, repo, "config", "user.email", "test@example.com")
	runCheckoutWarmupGit(t, repo, "config", "user.name", "Test")
	require.NoError(t, os.WriteFile(filepath.Join(repo, "one.go"), []byte("package one\n\nfunc One() {}\n"), 0o644))
	runCheckoutWarmupGit(t, repo, "add", ".")
	runCheckoutWarmupGit(t, repo, "commit", "-q", "-m", "init")
	alias := filepath.Join(dir, "project-only-alias")
	if err := os.Symlink(repo, alias); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	expectedPrefix := filepath.Base(repo)

	nonGit := filepath.Join(dir, "plain-source")
	require.NoError(t, os.MkdirAll(nonGit, 0o755))
	legacy, managed := legacyWarmupRepos(ctx, &indexer.CheckoutLifecycle{}, []config.RepoEntry{{Path: repo}, {Path: nonGit}})
	require.Equal(t, 1, managed)
	require.Equal(t, []config.RepoEntry{{Path: nonGit}}, legacy, "non-Git roots retain the historical warmup path")

	cfgPath := filepath.Join(dir, "config.yaml")
	gc := &config.GlobalConfig{Projects: map[string]config.ProjectConfig{
		"only-project": {Repos: []config.RepoEntry{{Path: alias}}},
	}}
	gc.SetConfigPath(cfgPath)
	require.NoError(t, gc.Save())
	dbPath := filepath.Join(dir, "store.sqlite")

	open := func() (*store_sqlite.Store, *indexer.MultiIndexer, *indexer.CheckoutLifecycle, *daemonState) {
		store, err := store_sqlite.Open(dbPath)
		require.NoError(t, err)
		cm, err := config.NewConfigManager(cfgPath)
		require.NoError(t, err)
		reg := parser.NewRegistry()
		languages.RegisterAll(reg)
		mi := indexer.NewMultiIndexer(store, reg, search.NewNull(), cm, zap.NewNop())
		lc, err := indexer.NewCheckoutLifecycle(indexer.CheckoutLifecycleConfig{MultiIndexer: mi, ConfigManager: cm, Graph: store, Logger: zap.NewNop()})
		require.NoError(t, err)
		return store, mi, lc, &daemonState{graph: store, multiIndexer: mi, configManager: cm, lifecycle: lc}
	}
	closeState := func(store *store_sqlite.Store, mi *indexer.MultiIndexer, lc *indexer.CheckoutLifecycle) {
		require.NoError(t, lc.Close())
		require.NoError(t, mi.Close(ctx))
		require.NoError(t, store.Close())
	}

	store, mi, lc, state := open()
	gate := indexer.NewViewBuildGate()
	lc.SetBuildGate(gate)
	require.NoError(t, lc.Seed(ctx))
	before, err := lc.FamiliesOverview(ctx, "")
	require.NoError(t, err)
	require.Len(t, before.Families, 1)
	require.Len(t, before.Families[0].Graphs, 1)
	require.Zero(t, before.Families[0].Graphs[0].ActiveGenerationID)
	require.False(t, before.Families[0].Checkouts[0].CoordinatorLive, "closed startup gate must not admit a dependent coordinator")

	var coldMemBefore, coldMemAfter runtime.MemStats
	runtime.ReadMemStats(&coldMemBefore)
	coldStarted := time.Now()
	mw, cold := warmupDaemonState(state, zap.NewNop(), nil)
	coldWarmupElapsed := time.Since(coldStarted)
	runtime.ReadMemStats(&coldMemAfter)
	if mw != nil {
		require.NoError(t, mw.Stop())
	}
	require.Zero(t, cold.reposChanged, "configured Git checkout must produce no legacy generation-0 warmup job")
	require.Empty(t, store.AllNodes(), "managed Git checkout must leave generation-0 nodes empty")
	require.Empty(t, store.AllEdges(), "managed Git checkout must leave generation-0 edges empty")
	require.Empty(t, store.LoadFileMtimes(expectedPrefix), "managed Git checkout must leave generation-0 mtimes empty")
	promotionStarted := time.Now()
	gate.Open()
	require.Eventually(t, func() bool {
		overview, overviewErr := lc.FamiliesOverview(ctx, "")
		return overviewErr == nil && len(overview.Families) == 1 && len(overview.Families[0].Graphs) == 1 && overview.Families[0].Graphs[0].ActiveGenerationID > 0
	}, 10*time.Second, 10*time.Millisecond)
	coldPromotionElapsed := time.Since(promotionStarted)
	var coldPromotionMemAfter runtime.MemStats
	runtime.ReadMemStats(&coldPromotionMemAfter)
	coldOverview, err := lc.FamiliesOverview(ctx, "")
	require.NoError(t, err)
	require.Equal(t, expectedPrefix, coldOverview.Families[0].Graphs[0].RepoPrefix,
		"cold startup derives the graph prefix from the canonical physical root")
	coldGeneration := coldOverview.Families[0].Graphs[0].ActiveGenerationID
	require.Positive(t, coldGeneration)
	coldGenerations, err := store.Catalog().ListViewGenerations(ctx, store_sqlite.ViewGenerationFilter{GraphID: coldOverview.Families[0].Graphs[0].GraphID})
	require.NoError(t, err)
	require.Len(t, coldGenerations, 3, "one promoted view consists of base, commit, and dirty generations")
	baseBuilds := 0
	for _, generation := range coldGenerations {
		if generation.OwnerKind == "dedicated_base" {
			baseBuilds++
		}
	}
	require.Equal(t, 1, baseBuilds, "cold startup performs exactly one physical immutable corpus build")
	require.Empty(t, store.LoadFileMtimes(coldOverview.Families[0].Graphs[0].RepoPrefix),
		"managed Git checkout must leave no generation-0 mtime payload")
	require.Empty(t, state.configManager.Global().Repos,
		"publishing a project-only corpus must not manufacture a top-level config source")
	require.NoError(t, store.SetFileMtime(expectedPrefix, "orphan-protection-sentinel", 1))
	closeState(store, mi, lc)

	store, mi, lc, state = open()
	gate = indexer.NewViewBuildGate()
	lc.SetBuildGate(gate)
	require.NoError(t, lc.Seed(ctx))
	var warmMemBefore, warmMemAfter runtime.MemStats
	runtime.ReadMemStats(&warmMemBefore)
	warmStarted := time.Now()
	mw, warm := warmupDaemonState(state, zap.NewNop(), nil)
	warmElapsed := time.Since(warmStarted)
	runtime.ReadMemStats(&warmMemAfter)
	if mw != nil {
		require.NoError(t, mw.Stop())
	}
	require.Zero(t, warm.reposChanged, "unchanged warm startup must perform zero physical corpus builds")
	warmOverview, err := lc.FamiliesOverview(ctx, "")
	require.NoError(t, err)
	require.Equal(t, expectedPrefix, warmOverview.Families[0].Graphs[0].RepoPrefix,
		"warm startup retains the canonical physical prefix across the symlink alias")
	require.Equal(t, coldGeneration, warmOverview.Families[0].Graphs[0].ActiveGenerationID)
	require.True(t, warmOverview.Families[0].Graphs[0].Served, "warm Seed restores the published route-owned shell")
	warmGenerations, err := store.Catalog().ListViewGenerations(ctx, store_sqlite.ViewGenerationFilter{GraphID: warmOverview.Families[0].Graphs[0].GraphID})
	require.NoError(t, err)
	require.Equal(t, coldGenerations, warmGenerations, "unchanged warm startup reuses the exact base/commit/dirty generations without another build")
	require.Equal(t, int64(1), store.LoadFileMtimes(expectedPrefix)["orphan-protection-sentinel"],
		"orphan protection must recognize the canonical prefix behind a blank-name alias")
	require.Empty(t, state.configManager.Global().Repos,
		"warm route restoration must preserve project-only provenance")
	t.Logf("bounded warmup benchmark: cold_legacy=%s cold_promotion=%s cold_total_allocs=%d cold_total_alloc_bytes=%d legacy_allocs=%d legacy_alloc_bytes=%d warm=%s warm_allocs=%d warm_alloc_bytes=%d physical_base_builds=%d cold_repos_changed=%d warm_repos_changed=%d",
		coldWarmupElapsed, coldPromotionElapsed,
		coldPromotionMemAfter.Mallocs-coldMemBefore.Mallocs, coldPromotionMemAfter.TotalAlloc-coldMemBefore.TotalAlloc,
		coldMemAfter.Mallocs-coldMemBefore.Mallocs, coldMemAfter.TotalAlloc-coldMemBefore.TotalAlloc,
		warmElapsed, warmMemAfter.Mallocs-warmMemBefore.Mallocs, warmMemAfter.TotalAlloc-warmMemBefore.TotalAlloc,
		baseBuilds, cold.reposChanged, warm.reposChanged)
	closeState(store, mi, lc)
}

func runCheckoutWarmupGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "%s", out)
}
