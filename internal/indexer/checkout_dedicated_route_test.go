package indexer

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/config"
)

func TestDedicatedBaseStaysCapturedAcrossBranchSwitchAndRestart(t *testing.T) {
	f := newLifecycleFixture(t)
	defer f.close()
	ctx := context.Background()

	main := f.gitRepo("immutable-main")
	tracked, err := f.lc.Register(ctx, config.RepoEntry{
		Path: main, Name: "immutable-main",
	}, TrackSourceCLI)
	require.NoError(t, err)
	require.NoError(t, tracked.CatalogErr)
	require.True(t, f.lc.SignalCheckout(tracked.CheckoutID, "initial"),
		"a dedicated checkout owns a coordinator")

	graphBefore, found, err := f.catalog.GetDedicatedGraph(ctx, tracked.GraphID)
	require.NoError(t, err)
	require.True(t, found)
	require.NotZero(t, graphBefore.ActiveGenerationID)
	capturedBase := contentIdentities(f.store.AtGeneration(graphBefore.ActiveGenerationID), tracked.Prefix)
	require.Contains(t, capturedBase, "immutable-main.go::A")

	runGit(t, main, "checkout", "-q", "-b", "alternate")
	writeFile(t, filepath.Join(main, "immutable-main.go"),
		"package a\n\nfunc Alternate() {}\n")
	runGit(t, main, "add", "immutable-main.go")
	runGit(t, main, "commit", "-q", "-m", "alternate")
	f.runCoordinator(tracked.CheckoutID)

	alternate := f.materialize(tracked.CheckoutID)
	alternateContent := contentIdentities(alternate.Reader, tracked.Prefix)
	alternate.Close()
	assert.Contains(t, alternateContent, "immutable-main.go::Alternate")
	assert.NotContains(t, alternateContent, "immutable-main.go::A")
	assert.Equal(t, capturedBase,
		contentIdentities(f.store.AtGeneration(graphBefore.ActiveGenerationID), tracked.Prefix),
		"switching branches changes only layers over the captured base")
	graphAway, found, err := f.catalog.GetDedicatedGraph(ctx, tracked.GraphID)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, graphBefore.ActiveGenerationID, graphAway.ActiveGenerationID)

	// Recreate the process around the switch back. The fresh coordinator must
	// recompose the main branch over the same captured base.
	runGit(t, main, "checkout", "-q", "main")
	f.restart()
	_, err = f.mi.TrackRepoCtx(ctx, config.RepoEntry{Path: main, Name: "immutable-main"})
	require.NoError(t, err)
	require.NoError(t, f.lc.Seed(ctx))
	_, err = f.lc.Sweep(ctx)
	require.NoError(t, err)
	assert.NotNil(t, f.mi.GetMetadata(tracked.Prefix), "the route-owned corpus shell is restored")
	require.True(t, f.lc.SignalCheckout(tracked.CheckoutID, "after-restart"),
		"the dedicated coordinator is restored")
	f.runCoordinator(tracked.CheckoutID)
	back := f.materialize(tracked.CheckoutID)
	backContent := contentIdentities(back.Reader, tracked.Prefix)
	back.Close()
	assert.Contains(t, backContent, "immutable-main.go::A")
	assert.NotContains(t, backContent, "immutable-main.go::Alternate")
	assert.Equal(t, capturedBase,
		contentIdentities(f.store.AtGeneration(graphBefore.ActiveGenerationID), tracked.Prefix))

	// Restart again while the live checkout names a different branch. Merely
	// restoring the shell must not walk that branch into the immutable base.
	runGit(t, main, "checkout", "-q", "alternate")
	f.restart()
	_, err = f.mi.TrackRepoCtx(ctx, config.RepoEntry{Path: main, Name: "immutable-main"})
	require.NoError(t, err)
	require.NoError(t, f.lc.Seed(ctx))
	_, err = f.lc.Sweep(ctx)
	require.NoError(t, err)
	graphAfter, found, err := f.catalog.GetDedicatedGraph(ctx, tracked.GraphID)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, graphBefore.ActiveGenerationID, graphAfter.ActiveGenerationID)
	assert.Equal(t, capturedBase,
		contentIdentities(f.store.AtGeneration(graphAfter.ActiveGenerationID), tracked.Prefix),
		"restart restoration does not walk the live checkout into the base")
	assert.NotNil(t, f.mi.GetMetadata(tracked.Prefix))
	assert.True(t, f.lc.SignalCheckout(tracked.CheckoutID, "after-alt-restart"))
	route, routed := f.routeOf(tracked.CheckoutID)
	require.True(t, routed)
	assert.Equal(t, tracked.GraphID, route.GraphID)
}

func TestDedicatedPromotionComposesDirtyAndUntrackedFilesOverCapturedHEAD(t *testing.T) {
	f := newFamilyFixture(t, "layers")
	defer f.close()
	ctx := context.Background()

	writeFile(t, filepath.Join(f.worktree, "layers-main.go"),
		"package a\n\nfunc DirtyTracked() {}\n")
	writeFile(t, filepath.Join(f.worktree, "untracked.go"),
		"package a\n\nfunc Untracked() {}\n")
	result, err := f.lc.PromoteCheckout(ctx, f.automatic.CheckoutID, TrackSourceCLI)
	require.NoError(t, err)

	baseGraph, found, err := f.catalog.GetDedicatedGraph(ctx, result.GraphID)
	require.NoError(t, err)
	require.True(t, found)
	baseContent := contentIdentities(f.store.AtGeneration(baseGraph.ActiveGenerationID), result.Prefix)
	assert.Contains(t, baseContent, "layers-main.go::A")
	assert.NotContains(t, baseContent, "layers-main.go::DirtyTracked")
	assert.NotContains(t, baseContent, "untracked.go::Untracked")

	view := f.materialize(f.automatic.CheckoutID)
	viewContent := contentIdentities(view.Reader, result.Prefix)
	view.Close()
	assert.Contains(t, viewContent, "layers-main.go::DirtyTracked")
	assert.Contains(t, viewContent, "layers-wt.go::B")
	assert.Contains(t, viewContent, "untracked.go::Untracked")
	assert.NotContains(t, viewContent, "layers-main.go::A")
	route, routed := f.routeOf(f.automatic.CheckoutID)
	require.True(t, routed)
	assert.Equal(t, result.GraphID, route.GraphID)
	assert.NotZero(t, route.CommitGenerationID)
	assert.NotZero(t, route.DirtyGenerationID)
}

func TestTwoDedicatedWorktreesKeepRoutesAndLayersIsolated(t *testing.T) {
	f := newLifecycleFixture(t)
	defer f.close()
	ctx := context.Background()

	main := f.gitRepo("isolation-main")
	worktreeA := f.worktreeOf(main, "isolation-a")
	worktreeB := f.worktreeOf(main, "isolation-b")
	mainTracked, err := f.lc.Register(ctx, config.RepoEntry{
		Path: main, Name: "isolation-main",
	}, TrackSourceCLI)
	require.NoError(t, err)
	require.NoError(t, mainTracked.CatalogErr)
	trackedA, err := f.lc.Register(ctx, config.RepoEntry{Path: worktreeA}, TrackSourceCLI)
	require.NoError(t, err)
	require.NoError(t, trackedA.CatalogErr)
	trackedB, err := f.lc.Register(ctx, config.RepoEntry{Path: worktreeB}, TrackSourceCLI)
	require.NoError(t, err)
	require.NoError(t, trackedB.CatalogErr)

	assert.NotEqual(t, mainTracked.GraphID, trackedA.GraphID)
	assert.NotEqual(t, mainTracked.GraphID, trackedB.GraphID)
	assert.NotEqual(t, trackedA.GraphID, trackedB.GraphID)
	routeA, routed := f.routeOf(trackedA.CheckoutID)
	require.True(t, routed)
	routeB, routed := f.routeOf(trackedB.CheckoutID)
	require.True(t, routed)
	assert.Equal(t, trackedA.GraphID, routeA.GraphID)
	assert.Equal(t, trackedB.GraphID, routeB.GraphID)

	viewA := f.materialize(trackedA.CheckoutID)
	contentA := contentIdentities(viewA.Reader, trackedA.Prefix)
	viewA.Close()
	viewB := f.materialize(trackedB.CheckoutID)
	contentB := contentIdentities(viewB.Reader, trackedB.Prefix)
	viewB.Close()
	assert.Contains(t, contentA, "isolation-a.go::B")
	assert.NotContains(t, contentA, "isolation-b.go::B")
	assert.Contains(t, contentB, "isolation-b.go::B")
	assert.NotContains(t, contentB, "isolation-a.go::B")

	writeFile(t, filepath.Join(worktreeA, "only-a.go"),
		"package a\n\nfunc OnlyA() {}\n")
	f.runCoordinator(trackedA.CheckoutID)
	updatedA := f.materialize(trackedA.CheckoutID)
	updatedAContent := contentIdentities(updatedA.Reader, trackedA.Prefix)
	updatedA.Close()
	unchangedB := f.materialize(trackedB.CheckoutID)
	unchangedBContent := contentIdentities(unchangedB.Reader, trackedB.Prefix)
	unchangedB.Close()
	assert.Contains(t, updatedAContent, "only-a.go::OnlyA")
	assert.NotContains(t, unchangedBContent, "only-a.go::OnlyA")
	stillB, routed := f.routeOf(trackedB.CheckoutID)
	require.True(t, routed)
	assert.Equal(t, routeB, stillB, "cycling one dedicated checkout does not republish its sibling")
	assert.True(t, f.lc.SignalCheckout(trackedA.CheckoutID, "test-a"))
	assert.True(t, f.lc.SignalCheckout(trackedB.CheckoutID, "test-b"))
}
