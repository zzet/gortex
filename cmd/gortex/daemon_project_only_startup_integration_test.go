package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/daemon"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"github.com/zzet/gortex/internal/search"
)

func TestProjectOnlyColdStartupKeepsStatusUnreadyUntilExactViewPublishes(t *testing.T) {
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
	require.NoError(t, os.WriteFile(
		filepath.Join(repo, "one.go"),
		[]byte("package one\n\nfunc One() {}\n"),
		0o644,
	))
	runCheckoutWarmupGit(t, repo, "add", ".")
	runCheckoutWarmupGit(t, repo, "commit", "-q", "-m", "init")
	alias := filepath.Join(dir, "project-only-alias")
	if err := os.Symlink(repo, alias); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	canonicalRoot, err := filepath.EvalSymlinks(alias)
	require.NoError(t, err)

	configPath := filepath.Join(dir, "config.yaml")
	gc := &config.GlobalConfig{Projects: map[string]config.ProjectConfig{
		"only-project": {Repos: []config.RepoEntry{{Path: alias}}},
	}}
	gc.SetConfigPath(configPath)
	require.NoError(t, gc.Save())
	configManager, err := config.NewConfigManager(configPath)
	require.NoError(t, err)
	store, err := store_sqlite.Open(filepath.Join(dir, "store.sqlite"))
	require.NoError(t, err)
	registry := parser.NewRegistry()
	languages.RegisterAll(registry)
	searchBackend := search.NewSymbolSearcherBackend(store)
	multiIndexer := indexer.NewMultiIndexer(
		store, registry, searchBackend, configManager, zap.NewNop(),
	)
	lifecycle, err := indexer.NewCheckoutLifecycle(indexer.CheckoutLifecycleConfig{
		MultiIndexer:  multiIndexer,
		ConfigManager: configManager,
		Graph:         store,
		Logger:        zap.NewNop(),
	})
	require.NoError(t, err)
	state := &daemonState{
		graph:         store,
		multiIndexer:  multiIndexer,
		configManager: configManager,
		lifecycle:     lifecycle,
	}
	gate := indexer.NewViewBuildGate()
	lifecycle.SetBuildGate(gate)
	controller := &realController{
		graph:         store,
		multiIndexer:  multiIndexer,
		configManager: configManager,
		lifecycle:     lifecycle,
		viewMaterializer: &graphview.Materializer{
			Store:   store,
			Catalog: store.Catalog(),
			Leases:  lifecycle.ViewLeases(),
		},
		logger: zap.NewNop(),
	}
	state.readinessFilter = controller.filterReadinessPhase

	monitor := newStartupViewReadinessMonitor(nil)
	lifecycle.SetModeTransitionObserver(monitor.observe)
	lifecycle.SetCheckoutTopologyObserver(monitor.observeTopology)
	monitorCtx, cancelMonitor := context.WithCancel(ctx)
	var monitorWG sync.WaitGroup
	monitorWG.Add(1)
	go func() {
		defer monitorWG.Done()
		watchStartupViewReadiness(
			monitorCtx, state, controller, monitor, controller.TrackReadiness,
		)
	}()
	select {
	case <-monitor.watching:
	case <-time.After(5 * time.Second):
		t.Fatal("startup readiness monitor did not begin watching")
	}

	var releaseGateOnce sync.Once
	releaseGate := make(chan struct{})
	checkpoint := make(chan projectOnlyStartupCheckpoint, 1)
	startupDone := make(chan error, 1)
	var markReadyCalls atomic.Int32
	var finalizedBeforeFullOpen atomic.Bool
	go func() {
		if seedErr := lifecycle.Seed(ctx); seedErr != nil {
			checkpoint <- projectOnlyStartupCheckpoint{err: seedErr}
			startupDone <- seedErr
			return
		}
		ownership := daemonStartupOwnershipPlan(ctx, state, zap.NewNop())
		monitor.setPaths(ownership.managedPaths)
		initial := monitor.snapshot(monitorCtx, controller.TrackReadiness)
		controller.setStartupViewReadiness(initial)
		monitor.finishInitialSnapshot()
		started := time.Now()
		markReady := sync.OnceFunc(func() {
			markReadyCalls.Add(1)
			controller.MarkReady(time.Since(started))
		})
		watcher, timings := warmupDaemonStateWithOwnership(
			state, zap.NewNop(), markReady, &ownership,
		)
		attachCtx, cancelAttach := context.WithTimeout(ctx, 10*time.Second)
		attachErr := controller.AttachWatcherContext(attachCtx, watcher)
		cancelAttach()
		if attachErr != nil {
			checkpoint <- projectOnlyStartupCheckpoint{err: attachErr}
			startupDone <- attachErr
			return
		}
		checkpoint <- projectOnlyStartupCheckpoint{timings: timings}
		<-releaseGate
		monitor.onConfirmedComplete(func() {
			finalizedBeforeFullOpen.Store(!gate.IsOpen())
			defer gate.Open()
			controller.MarkEnriched(time.Since(started))
		})
		gate.OpenRequired()
		terminal, waitErr := monitor.waitTerminal(monitorCtx)
		if waitErr != nil {
			startupDone <- waitErr
			return
		}
		if !terminal.complete() {
			gate.Open()
			startupDone <- fmt.Errorf("startup view terminal failure: %+v", terminal)
			return
		}
		startupDone <- nil
	}()

	t.Cleanup(func() {
		releaseGateOnce.Do(func() { close(releaseGate) })
		lifecycle.SetModeTransitionObserver(nil)
		lifecycle.SetCheckoutTopologyObserver(nil)
		cancelMonitor()
		monitorWG.Wait()
		controller.StopWatcher()
		require.NoError(t, lifecycle.Close())
		require.NoError(t, multiIndexer.Close(ctx))
		require.NoError(t, store.Close())
	})

	preGate := <-checkpoint
	require.NoError(t, preGate.err)
	require.NotNil(t, preGate.timings)
	require.Zero(t, preGate.timings.reposChanged,
		"project-only Git work belongs exclusively to lifecycle promotion")
	require.Equal(t, int32(1), markReadyCalls.Load(),
		"the warmup queryable edge must be once-guarded")
	require.Eventually(t, func() bool {
		return controller.startupViewReadiness() == (startupViewReadiness{
			Expected: 1,
			Building: 1,
		})
	}, 5*time.Second, 10*time.Millisecond)
	require.True(t, controller.referenceReady.Load(),
		"legacy/reference warmup should have completed")
	require.False(t, controller.IsEnriched(),
		"legacy enrichment must not publish full enrichment before the exact view")
	require.False(t, controller.IsReady(),
		"reference warmup must not expose ready while the exact view is gated")

	entries, missing := controller.trackedRepoLiveness()
	require.Equal(t, []config.RepoEntry{{Path: canonicalRoot}}, entries,
		"status must use the same canonical physical project registration as warmup")
	require.False(t, missing[canonicalRoot])
	require.Empty(t, configManager.Global().Repos,
		"project membership must not manufacture a top-level repo entry")

	statusCtx, cancelStatus := context.WithTimeout(ctx, 5*time.Second)
	preStatus, err := controller.Status(statusCtx)
	cancelStatus()
	require.NoError(t, err)
	require.False(t, preStatus.Ready)
	require.Len(t, preStatus.TrackedRepos, 1,
		"project-only registration must remain visible during its cold build")
	require.Equal(t, canonicalRoot, preStatus.TrackedRepos[0].Path)
	require.Equal(t, daemon.RepoViewStateBuilding, preStatus.TrackedRepos[0].ViewState)
	require.NotNil(t, preStatus.TrackedRepos[0].CountsKnown)
	require.False(t, *preStatus.TrackedRepos[0].CountsKnown)
	require.False(t, preStatus.SearchBackend.DocCountKnown,
		"generation-zero docs=0 must not be presented as the routed corpus size")

	preOverview, err := lifecycle.FamiliesOverview(ctx, "")
	require.NoError(t, err)
	require.Len(t, preOverview.Families, 1)
	require.Len(t, preOverview.Families[0].Checkouts, 1)
	checkoutID := preOverview.Families[0].Checkouts[0].CheckoutID
	_, transitionPending, err := store.Catalog().GetIntentTransition(ctx, checkoutID)
	require.NoError(t, err)
	require.True(t, transitionPending,
		"closed startup gate should retain the durable promotion transition")

	releaseGateOnce.Do(func() { close(releaseGate) })
	select {
	case err := <-startupDone:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("startup goroutine did not open the view-build gate")
	}
	require.Eventually(t, controller.IsReady, 20*time.Second, 10*time.Millisecond,
		"exact-view transition did not publish daemon readiness")
	require.True(t, controller.IsEnriched(),
		"full enrichment must publish only after the exact route is ready")
	require.True(t, finalizedBeforeFullOpen.Load(),
		"normal build admission opened before exact-view finalization")
	gateStats := gate.Stats()
	require.True(t, gateStats.Open)
	require.True(t, gateStats.RequiredOpen)
	require.Positive(t, gateStats.AdmittedRequired)
	require.Zero(t, gateStats.AdmittedBackground,
		"cold required publication leaked into background admission")

	statusCtx, cancelStatus = context.WithTimeout(ctx, 5*time.Second)
	readyStatus, err := controller.Status(statusCtx)
	cancelStatus()
	require.NoError(t, err)
	require.True(t, readyStatus.Ready)
	require.Len(t, readyStatus.TrackedRepos, 1)
	readyRepo := readyStatus.TrackedRepos[0]
	require.Equal(t, canonicalRoot, readyRepo.Path)
	require.Equal(t, daemon.RepoViewStateReady, readyRepo.ViewState)
	require.NotNil(t, readyRepo.CountsKnown)
	require.True(t, *readyRepo.CountsKnown)
	require.Equal(t, 1, readyRepo.Files)
	require.Positive(t, readyRepo.Nodes,
		"ready must describe the published corpus, not an empty shell")
	require.False(t, readyStatus.SearchBackend.DocCountKnown,
		"generation-zero docs must remain suppressed for a routed view")

	finalOverview, err := lifecycle.FamiliesOverview(ctx, "")
	require.NoError(t, err)
	require.Len(t, finalOverview.Families, 1)
	require.Len(t, finalOverview.Families[0].Graphs, 1)
	graphID := finalOverview.Families[0].Graphs[0].GraphID
	generations, err := store.Catalog().ListViewGenerations(
		ctx,
		store_sqlite.ViewGenerationFilter{GraphID: graphID},
	)
	require.NoError(t, err)
	baseBuilds := 0
	for _, generation := range generations {
		if generation.OwnerKind == "dedicated_base" {
			baseBuilds++
		}
	}
	require.Equal(t, 1, baseBuilds,
		"cold startup must perform exactly one immutable physical build")
	_, transitionPending, err = store.Catalog().GetIntentTransition(ctx, checkoutID)
	require.NoError(t, err)
	require.False(t, transitionPending,
		"gate publication and watcher attachment must drain the durable transition")
	require.Empty(t, configManager.Global().Repos,
		"successful publication must preserve project-only provenance")
}

type projectOnlyStartupCheckpoint struct {
	timings *warmupTimings
	err     error
}
