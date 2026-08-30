package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/daemon"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/reconcile"
	"github.com/zzet/gortex/internal/testenv"
)

// The probe fixture: one family with a dedicated primary and one automatic
// worktree beside it, over a base corpus the worktree's routed generations
// stack on.
const (
	probeFamily     = "fam-probe"
	probePrimaryID  = "co-probe-primary"
	probeWorktreeID = "co-probe-worktree"
	probeGraphID    = "graph-probe"
	probePrefix     = "repo"
	probeFile       = "internal/live.go"
)

// probeFileKey is the key the store holds probeFile under: the repo prefix,
// one '/', then the repo-relative remainder in the indexing machine's native
// separators (see internal/graphpath).
//
// The two spellings coincide on POSIX and diverge on Windows, and the probe is
// exactly the seam between them: fileGraphKey renders `prefix + "/" +
// filepath.Rel(root, path)`, whose remainder carries the host separator.
// Keying the corpus with the '/'-joined form describes a file a Windows daemon
// never indexes, so the key the probe computes misses it and every coverage
// assertion below reads as uncovered.
var probeFileKey = probePrefix + "/" + filepath.FromSlash(probeFile)

// probeFixture is the seeded catalog a probe resolves against, plus the paths
// a caller probes with.
type probeFixture struct {
	controller   *realController
	multi        *indexer.MultiIndexer
	store        *store_sqlite.Store
	catalog      *store_sqlite.Catalog
	primaryRoot  string
	worktreeRoot string
}

// newProbeFixture builds a controller over a real sqlite store, seeds the base
// corpus, and registers a family whose primary is dedicated and whose worktree
// is automatic. The worktree is left unrouted; the routing helpers below add
// the generations a test needs.
func newProbeFixture(t *testing.T) *probeFixture {
	t.Helper()
	c, mi, catalog, dir := buildCatalogController(t)
	store, ok := c.graph.(*store_sqlite.Store)
	require.True(t, ok, "the fixture opened a %T, not the sqlite store", c.graph)

	primaryRoot := filepath.Join(dir, "primary")
	worktreeRoot := filepath.Join(dir, "worktree")

	// The base corpus: what the primary's own checkout reads, and what an
	// automatic worktree's generations compose over.
	store.AddBatch([]*graph.Node{
		{
			ID:         probeFileKey + "::BaseOnly",
			Kind:       graph.KindFunction,
			Name:       "BaseOnly",
			FilePath:   probeFileKey,
			RepoPrefix: probePrefix,
			Language:   "go",
			StartLine:  3,
			EndLine:    5,
		},
		{
			ID:         probeFileKey + "::AlsoBase",
			Kind:       graph.KindFunction,
			Name:       "AlsoBase",
			FilePath:   probeFileKey,
			RepoPrefix: probePrefix,
			Language:   "go",
			StartLine:  7,
			EndLine:    9,
		},
	}, nil)

	ctx := context.Background()
	require.NoError(t, catalog.UpsertRepositoryFamily(ctx, store_sqlite.RepositoryFamily{
		FamilyID:          probeFamily,
		CommonDirIdentity: filepath.Join(primaryRoot, ".git"),
		State:             reconcile.FamilyStateReady,
		CreatedAt:         100,
		LastSeen:          100,
	}))
	require.NoError(t, catalog.UpsertCheckout(ctx, store_sqlite.Checkout{
		CheckoutID:    probePrimaryID,
		Incarnation:   "inc-primary",
		FamilyID:      probeFamily,
		RootPath:      primaryRoot,
		GitDir:        filepath.Join(primaryRoot, ".git"),
		AdminName:     "primary",
		State:         store_sqlite.CheckoutStateReady,
		DesiredMode:   store_sqlite.CheckoutModeDedicated,
		EffectiveMode: store_sqlite.CheckoutModeDedicated,
		LastSeen:      101,
	}))
	require.NoError(t, catalog.UpsertCheckout(ctx, store_sqlite.Checkout{
		CheckoutID:    probeWorktreeID,
		Incarnation:   "inc-worktree",
		FamilyID:      probeFamily,
		RootPath:      worktreeRoot,
		GitDir:        filepath.Join(primaryRoot, ".git", "worktrees", "worktree"),
		AdminName:     "worktree",
		State:         store_sqlite.CheckoutStateReady,
		DesiredMode:   store_sqlite.CheckoutModeAutomatic,
		EffectiveMode: store_sqlite.CheckoutModeAutomatic,
		LastSeen:      101,
	}))
	require.NoError(t, catalog.UpsertDedicatedGraph(ctx, store_sqlite.DedicatedGraph{
		GraphID:         probeGraphID,
		OwnerCheckoutID: probePrimaryID,
		RepoPrefix:      probePrefix,
		FamilyID:        probeFamily,
		IsPrimaryBase:   true,
		State:           reconcile.GraphStateReady,
	}))

	c.viewMaterializer = &graphview.Materializer{
		Store:   store,
		Catalog: catalog,
		Leases:  c.lifecycle.ViewLeases(),
	}
	return &probeFixture{
		controller:   c,
		multi:        mi,
		store:        store,
		catalog:      catalog,
		primaryRoot:  primaryRoot,
		worktreeRoot: worktreeRoot,
	}
}

// routeWorktree publishes the two generations the worktree's route names and
// activates the route. The dirty generation claims the probed file and emits
// one symbol of its own, so a composed answer is distinguishable from a base
// one by content rather than by shape.
func (f *probeFixture) routeWorktree(t *testing.T) {
	t.Helper()
	ctx := context.Background()

	commitID, commitHandle, err := f.store.BeginPayloadGeneration(ctx, store_sqlite.PayloadGenerationRequest{
		OwnerKind:      "dedicated_graph",
		GraphID:        probeGraphID,
		LayerID:        "layer-probe-commit",
		CheckoutID:     probeWorktreeID,
		GenerationKind: "commit",
		TreeOID:        "tree-probe-commit",
		CreatedAt:      1000,
	})
	require.NoError(t, err)
	_ = commitHandle
	require.NoError(t, f.store.PublishPayloadGeneration(ctx, commitID, 2000))
	graphRow, found, err := f.catalog.GetDedicatedGraph(ctx, probeGraphID)
	require.NoError(t, err)
	require.True(t, found)
	graphRow.ActiveGenerationID = commitID
	require.NoError(t, f.catalog.UpsertDedicatedGraph(ctx, graphRow))
	checkout, found, err := f.catalog.GetCheckout(ctx, probeWorktreeID)
	require.NoError(t, err)
	require.True(t, found)
	checkout.HeadTree = "tree-probe-commit"
	require.NoError(t, f.catalog.UpsertCheckout(ctx, checkout))

	dirtyID, dirtyHandle, err := f.store.BeginPayloadGeneration(ctx, store_sqlite.PayloadGenerationRequest{
		OwnerKind:        "dedicated_graph",
		GraphID:          probeGraphID,
		LayerID:          "layer-probe-dirty",
		CheckoutID:       probeWorktreeID,
		GenerationKind:   "dirty",
		BaseGenerationID: commitID,
		TreeOID:          "tree-probe-dirty",
		CreatedAt:        1001,
	})
	require.NoError(t, err)
	dirtyHandle.AddBatch([]*graph.Node{{
		ID:         probeFileKey + "::GenerationOnly",
		Kind:       graph.KindFunction,
		Name:       "GenerationOnly",
		FilePath:   probeFileKey,
		RepoPrefix: probePrefix,
		Language:   "go",
		StartLine:  3,
		EndLine:    6,
	}}, nil)
	require.NoError(t, dirtyHandle.SetFileMasks([]store_sqlite.FileMask{{
		RepoPrefix: probePrefix,
		FilePath:   probeFileKey,
		Mode:       store_sqlite.OwnershipReplace,
	}}))
	require.NoError(t, f.store.PublishPayloadGeneration(ctx, dirtyID, 2001))

	require.NoError(t, f.catalog.UpsertCheckoutRoute(ctx, store_sqlite.CheckoutRoute{
		CheckoutID:         probeWorktreeID,
		GraphID:            probeGraphID,
		CommitGenerationID: commitID,
		DirtyGenerationID:  dirtyID,
		State:              store_sqlite.RouteActive,
	}))
}

// upsertWorktree rewrites the automatic checkout's row with the given root and
// state, which is how a test moves it into a grace window or re-homes it.
func (f *probeFixture) upsertWorktree(t *testing.T, root string, state store_sqlite.CheckoutState) {
	t.Helper()
	require.NoError(t, f.catalog.UpsertCheckout(context.Background(), store_sqlite.Checkout{
		CheckoutID:    probeWorktreeID,
		Incarnation:   "inc-worktree",
		FamilyID:      probeFamily,
		RootPath:      root,
		GitDir:        filepath.Join(f.primaryRoot, ".git", "worktrees", "worktree"),
		AdminName:     "worktree",
		State:         state,
		DesiredMode:   store_sqlite.CheckoutModeAutomatic,
		EffectiveMode: store_sqlite.CheckoutModeAutomatic,
		LastSeen:      102,
	}))
	f.worktreeRoot = root
}

// symbolNames flattens a probe answer to the names it cited.
func symbolNames(hits []daemon.SymbolHit) []string {
	out := make([]string, 0, len(hits))
	for _, h := range hits {
		out = append(out, h.Name)
	}
	return out
}

// TestProbeOfRoutedWorktreeReadsTheComposedView pins the whole point of the
// path parameter: a file inside a routed automatic worktree is answered from
// that working copy's composed stack, so the generation's own symbol is cited
// and the base symbol the generation replaced is not.
func TestProbeOfRoutedWorktreeReadsTheComposedView(t *testing.T) {
	f := newProbeFixture(t)
	f.routeWorktree(t)
	ctx := context.Background()
	probed := filepath.Join(f.worktreeRoot, probeFile)

	coverage, err := f.controller.FileCoverage(ctx, daemon.FileCoverageParams{Path: probed})
	require.NoError(t, err)
	assert.True(t, coverage.Covered, "the composed view holds this file")
	assert.Equal(t, 1, coverage.Symbols,
		"the generation replaced the file, so only its own symbol is in it")
	require.NotNil(t, coverage.View)
	assert.Equal(t, daemon.ProbeViewWorktree, coverage.View.Kind)
	assert.Equal(t, probeWorktreeID, coverage.View.CheckoutID)
	assert.True(t, coverage.View.Exact, "the working copy's own view answered")

	found, err := f.controller.SearchSymbols(ctx, daemon.SearchSymbolsParams{
		Query: "GenerationOnly", Path: probed,
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"GenerationOnly"}, symbolNames(found.Hits))

	hidden, err := f.controller.SearchSymbols(ctx, daemon.SearchSymbolsParams{
		Query: "BaseOnly", Path: probed,
	})
	require.NoError(t, err)
	assert.Empty(t, hidden.Hits,
		"the generation owns the file, so the base symbol it dropped must not surface")

	// The same query without a path still reads the base corpus, which is
	// what proves the two answers are different graphs rather than one.
	base, err := f.controller.SearchSymbols(ctx, daemon.SearchSymbolsParams{Query: "BaseOnly"})
	require.NoError(t, err)
	assert.Equal(t, []string{"BaseOnly"}, symbolNames(base.Hits))
}

func TestTrackReadinessWaitsForExactRoutedView(t *testing.T) {
	testenv.Sandbox(t)
	f := newProbeFixture(t)
	ctx := context.Background()
	probed := filepath.Join(f.worktreeRoot, probeFile)

	building, err := f.controller.TrackReadiness(ctx, probed)
	require.NoError(t, err)
	assert.Equal(t, daemon.TrackReadinessBuilding, building.State)
	require.NotNil(t, building.View)
	assert.False(t, building.View.Exact)
	assert.Equal(t, daemon.FallbackViewBuilding, building.View.FallbackReason)

	f.routeWorktree(t)
	ready, err := f.controller.TrackReadiness(ctx, probed)
	require.NoError(t, err)
	assert.Equal(t, daemon.TrackReadinessReady, ready.State)
	require.NotNil(t, ready.View)
	assert.True(t, ready.View.Exact)
	assert.Equal(t, daemon.ProbeViewWorktree, ready.View.Kind)

	// The same exact gate a following path-scoped query uses now succeeds.
	coverage, err := f.controller.FileCoverage(ctx, daemon.FileCoverageParams{Path: probed})
	require.NoError(t, err)
	assert.True(t, coverage.Covered)
	assert.Equal(t, 1, coverage.Symbols)
	require.NotNil(t, coverage.View)
	assert.True(t, coverage.View.Exact)

	// A warm routed checkout is ready on the first metadata/materialization
	// pass; no coordinator or corpus build is started by this read.
	warm, err := f.controller.TrackReadiness(ctx, probed)
	require.NoError(t, err)
	assert.Equal(t, daemon.TrackReadinessReady, warm.State)
	binding, err := f.controller.lifecycle.ExplainView(ctx, probed)
	require.NoError(t, err)
	assert.False(t, binding.CoordinatorLive, "readiness polling must not start a build coordinator")
}

func TestTrackReadinessHeldPromotionDoesNotTrustStableEmptyShell(t *testing.T) {
	f := newProbeFixture(t)
	ctx := context.Background()
	const transitionID = "track-wait-held-promotion"
	require.NoError(t, f.catalog.BeginIntentTransition(ctx, store_sqlite.IntentTransition{
		TransitionID:       transitionID,
		CheckoutID:         probeWorktreeID,
		Cause:              "promote_checkout",
		PriorDesiredMode:   store_sqlite.CheckoutModeAutomatic,
		PriorEffectiveMode: store_sqlite.CheckoutModeAutomatic,
		RequestedMode:      store_sqlite.CheckoutModeDedicated,
		PriorCheckoutState: store_sqlite.CheckoutStateReady,
		State:              store_sqlite.IntentTransitionPending,
		CreatedAt:          100,
		LastProgress:       100,
	}))

	// Publish a complete route underneath the held transition, just as the
	// empty process-local shell can look stable while promotion is gated.
	f.routeWorktree(t)
	checkout, found, err := f.catalog.GetCheckout(ctx, probeWorktreeID)
	require.NoError(t, err)
	require.True(t, found)
	checkout.DesiredMode = store_sqlite.CheckoutModeDedicated
	checkout.EffectiveMode = store_sqlite.CheckoutModeDedicated
	require.NoError(t, f.catalog.UpsertCheckout(ctx, checkout))
	graphRow, found, err := f.catalog.GetDedicatedGraph(ctx, probeGraphID)
	require.NoError(t, err)
	require.True(t, found)
	graphRow.OwnerCheckoutID = probeWorktreeID
	require.NoError(t, f.catalog.UpsertDedicatedGraph(ctx, graphRow))
	f.controller.ready.Store(true)

	// The old heuristic would return success here: globally ready, zero nodes,
	// and the same zero on the previous poll. The routed gate must still block.
	settled, _ := indexSettled(statusWithRepo(f.worktreeRoot, 0, true), f.worktreeRoot, 0)
	assert.True(t, settled, "fixture must reproduce the old false-ready signal")
	building, err := f.controller.TrackReadiness(ctx, f.worktreeRoot)
	require.NoError(t, err)
	assert.Equal(t, daemon.TrackReadinessBuilding, building.State)

	require.NoError(t, f.catalog.CompleteIntentTransition(ctx, probeWorktreeID, transitionID))
	ready, err := f.controller.TrackReadiness(ctx, f.worktreeRoot)
	require.NoError(t, err)
	assert.Equal(t, daemon.TrackReadinessReady, ready.State)
	require.NotNil(t, ready.View)
	assert.True(t, ready.View.Exact)
	assert.Equal(t, daemon.ProbeViewBase, ready.View.Kind)
}

func TestTrackReadinessSurfacesPromotionFailure(t *testing.T) {
	f := newProbeFixture(t)
	ctx := context.Background()
	require.NoError(t, f.catalog.BeginIntentTransition(ctx, store_sqlite.IntentTransition{
		TransitionID:       "track-wait-failed-promotion",
		CheckoutID:         probeWorktreeID,
		Cause:              "promote_checkout",
		PriorDesiredMode:   store_sqlite.CheckoutModeAutomatic,
		PriorEffectiveMode: store_sqlite.CheckoutModeAutomatic,
		RequestedMode:      store_sqlite.CheckoutModeDedicated,
		PriorCheckoutState: store_sqlite.CheckoutStateReady,
		State:              store_sqlite.IntentTransitionFailed,
		CreatedAt:          100,
		LastProgress:       101,
		LastError:          "synthetic promotion failure",
	}))

	readiness, err := f.controller.TrackReadiness(ctx, f.worktreeRoot)
	require.NoError(t, err)
	assert.Equal(t, daemon.TrackReadinessFailed, readiness.State)
	assert.Equal(t, "synthetic promotion failure", readiness.Error)
}

func TestTrackReadinessRejectsGraceFallback(t *testing.T) {
	f := newProbeFixture(t)
	f.routeWorktree(t)
	f.upsertWorktree(t, f.worktreeRoot, store_sqlite.CheckoutStateAvailabilityGrace)

	readiness, err := f.controller.TrackReadiness(context.Background(), f.worktreeRoot)
	require.NoError(t, err)
	assert.Equal(t, daemon.TrackReadinessBuilding, readiness.State)
	require.NotNil(t, readiness.View)
	assert.False(t, readiness.View.Exact)
}

// TestProbeOfRoutedWorktreeReleasesItsLease proves the lease a probe takes is
// dropped when the answer is built. A lease that outlived the call would make
// the generation permanently unretirable, which is worse than the stale answer
// it protects against.
func TestProbeOfRoutedWorktreeReleasesItsLease(t *testing.T) {
	f := newProbeFixture(t)
	f.routeWorktree(t)
	ctx := context.Background()

	route, found, err := f.catalog.GetCheckoutRoute(ctx, probeWorktreeID)
	require.NoError(t, err)
	require.True(t, found)

	_, err = f.controller.FileCoverage(ctx, daemon.FileCoverageParams{
		Path: filepath.Join(f.worktreeRoot, probeFile),
	})
	require.NoError(t, err)

	inUse := f.controller.lifecycle.ViewLeases().InUse
	assert.False(t, inUse(route.DirtyGenerationID),
		"the probe still holds the working-tree generation after answering")
	assert.False(t, inUse(route.CommitGenerationID),
		"the probe still holds the commit generation after answering")
}

// TestProbeOfUnroutedWorktreeAnswersUncovered pins the reconcile-then-enforce
// posture: a registered working copy with no composed view answers uncovered,
// so the hook lets the native tool through instead of denying it on the
// primary's content.
func TestProbeOfUnroutedWorktreeAnswersUncovered(t *testing.T) {
	f := newProbeFixture(t)
	f.controller.probeReconcile = func(string) {}
	ctx := context.Background()
	probed := filepath.Join(f.worktreeRoot, probeFile)

	coverage, err := f.controller.FileCoverage(ctx, daemon.FileCoverageParams{Path: probed})
	require.NoError(t, err)
	assert.False(t, coverage.Covered, "nothing describes this working copy yet")
	assert.Zero(t, coverage.Symbols)
	require.NotNil(t, coverage.View)
	assert.Equal(t, daemon.ProbeViewUnrouted, coverage.View.Kind)
	assert.False(t, coverage.View.Exact)
	assert.Equal(t, daemon.FallbackViewBuilding, coverage.View.FallbackReason)

	found, err := f.controller.SearchSymbols(ctx, daemon.SearchSymbolsParams{
		Query: "BaseOnly", Path: probed,
	})
	require.NoError(t, err)
	assert.Empty(t, found.Hits,
		"the primary's symbols are another working copy's content, not evidence about this one")
	require.NotNil(t, found.View)
	assert.Equal(t, daemon.ProbeViewUnrouted, found.View.Kind)
}

// TestUnroutedProbeBurstReconcilesOncePerWindow pins the debounce. A hook
// probes once per tool call, so an agent working in an unrouted worktree
// raises the same request continuously; the family must be reconciled once per
// window however many probes ask for it.
func TestUnroutedProbeBurstReconcilesOncePerWindow(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newProbeFixture(t)
		var families []string
		f.controller.probeReconcile = func(familyID string) {
			families = append(families, familyID)
		}
		ctx := context.Background()
		probed := filepath.Join(f.worktreeRoot, probeFile)

		for range 5 {
			_, err := f.controller.FileCoverage(ctx, daemon.FileCoverageParams{Path: probed})
			require.NoError(t, err)
		}
		synctest.Wait()
		assert.Equal(t, []string{probeFamily}, families,
			"a burst of probes must not raise a reconciliation per probe")

		// Inside the window nothing more is asked for, however long the burst
		// runs.
		time.Sleep(probeReconcileDebounce - time.Second)
		_, err := f.controller.FileCoverage(ctx, daemon.FileCoverageParams{Path: probed})
		require.NoError(t, err)
		synctest.Wait()
		assert.Len(t, families, 1, "the window had not elapsed")

		// Past it, the next probe asks again: the working copy is still
		// unrouted and the janitor's own tick is an hour away.
		time.Sleep(2 * time.Second)
		_, err = f.controller.FileCoverage(ctx, daemon.FileCoverageParams{Path: probed})
		require.NoError(t, err)
		synctest.Wait()
		assert.Equal(t, []string{probeFamily, probeFamily}, families)
	})
}

// TestProbeOfWorktreeInGraceFallsBackToTheFamilyPrimary pins the grace rule: a
// working copy that stopped answering is served by the family primary, by the
// same fallback a read-only query takes — and the answer says so rather than
// passing for the working copy's own.
func TestProbeOfWorktreeInGraceFallsBackToTheFamilyPrimary(t *testing.T) {
	f := newProbeFixture(t)
	f.controller.probeReconcile = func(string) {}
	f.upsertWorktree(t, f.worktreeRoot, store_sqlite.CheckoutStateAvailabilityGrace)
	ctx := context.Background()
	probed := filepath.Join(f.worktreeRoot, probeFile)

	coverage, err := f.controller.FileCoverage(ctx, daemon.FileCoverageParams{Path: probed})
	require.NoError(t, err)
	assert.True(t, coverage.Covered, "the family primary still describes this file")
	assert.Equal(t, 2, coverage.Symbols)
	require.NotNil(t, coverage.View)
	assert.Equal(t, daemon.ProbeViewBase, coverage.View.Kind)
	assert.False(t, coverage.View.Exact, "a fallback answer must never read as exact")
	assert.Equal(t, string(store_sqlite.CheckoutStateAvailabilityGrace), coverage.View.FallbackReason)
}

// TestProbeOfDedicatedCheckoutReadsTheBaseCorpus pins that a checkout served
// from its own corpus is answered exactly as it was before routed views: from
// the base, unscoped, and marked exact.
func TestProbeOfDedicatedCheckoutReadsTheBaseCorpus(t *testing.T) {
	f := newProbeFixture(t)
	ctx := context.Background()
	probed := filepath.Join(f.primaryRoot, probeFile)

	coverage, err := f.controller.FileCoverage(ctx, daemon.FileCoverageParams{Path: probed})
	require.NoError(t, err)
	assert.True(t, coverage.Covered)
	assert.Equal(t, 2, coverage.Symbols)
	require.NotNil(t, coverage.View)
	assert.Equal(t, daemon.ProbeViewBase, coverage.View.Kind)
	assert.True(t, coverage.View.Exact)

	found, err := f.controller.SearchSymbols(ctx, daemon.SearchSymbolsParams{
		Query: "BaseOnly", Path: probed,
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"BaseOnly"}, symbolNames(found.Hits))
}

func TestProbeOfDedicatedCheckoutInRemovalGraceIsLabeledFallback(t *testing.T) {
	f := newProbeFixture(t)
	ctx := context.Background()
	checkout, found, err := f.catalog.GetCheckout(ctx, probePrimaryID)
	require.NoError(t, err)
	require.True(t, found)
	checkout.State = store_sqlite.CheckoutStateRemovalGrace
	require.NoError(t, f.catalog.UpsertCheckout(ctx, checkout))

	result, err := f.controller.SearchSymbols(ctx, daemon.SearchSymbolsParams{
		Query: "BaseOnly", Path: filepath.Join(f.primaryRoot, probeFile),
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"BaseOnly"}, symbolNames(result.Hits))
	require.NotNil(t, result.View)
	assert.Equal(t, daemon.ProbeViewBase, result.View.Kind)
	assert.False(t, result.View.Exact)
	assert.Equal(t, string(store_sqlite.CheckoutStateRemovalGrace), result.View.FallbackReason)
}

// TestProbeOfUntrackedPathIsUnchanged pins that a path no checkout owns still
// reads the base corpus. It is the ordinary case for every directory nobody
// has tracked, and nothing about routed views may change it.
func TestProbeOfUntrackedPathIsUnchanged(t *testing.T) {
	f := newProbeFixture(t)
	ctx := context.Background()

	found, err := f.controller.SearchSymbols(ctx, daemon.SearchSymbolsParams{
		Query: "BaseOnly", Path: filepath.Join(t.TempDir(), "elsewhere.go"),
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"BaseOnly"}, symbolNames(found.Hits))
	require.NotNil(t, found.View)
	assert.Equal(t, daemon.ProbeViewBase, found.View.Kind)
	assert.True(t, found.View.Exact)
}

// TestSearchSymbolsWithoutAPathKeepsTheLegacyWireShape is the compatibility
// gate. A client that predates routed views sends no path, and its answer must
// come back byte for byte what it has always been — in particular with no view
// block at all, which an older decoder would reject or silently ignore.
func TestSearchSymbolsWithoutAPathKeepsTheLegacyWireShape(t *testing.T) {
	f := newProbeFixture(t)
	f.routeWorktree(t)

	result, err := f.controller.SearchSymbols(context.Background(),
		daemon.SearchSymbolsParams{Query: "BaseOnly"})
	require.NoError(t, err)
	require.Nil(t, result.View, "an answer to a path-less request names no view")

	encoded, err := json.Marshal(result)
	require.NoError(t, err)
	// The hit carries the node's own file path, so the expectation is the key
	// the corpus was seeded under rather than a '/'-spelled literal that only
	// happens to be that key on POSIX.
	wantPath, err := json.Marshal(probeFileKey)
	require.NoError(t, err)
	assert.JSONEq(t,
		`{"hits":[{"name":"BaseOnly","kind":"function","file_path":`+string(wantPath)+`,"line":3}]}`,
		string(encoded))

	// A miss keeps its shape too: the empty hit list, and nothing else.
	miss, err := f.controller.SearchSymbols(context.Background(),
		daemon.SearchSymbolsParams{Query: "NothingNamedThis"})
	require.NoError(t, err)
	encodedMiss, err := json.Marshal(miss)
	require.NoError(t, err)
	assert.JSONEq(t, `{"hits":[]}`, string(encodedMiss))
}

// TestProbeMatchesAPathSpelledThroughASymlink pins the spelling rule the
// catalog already holds to: git records a worktree root with its symlinks
// resolved while a shell hands over the path it was given, and measuring one
// against the other reports a file inside the checkout as outside it.
func TestProbeMatchesAPathSpelledThroughASymlink(t *testing.T) {
	f := newProbeFixture(t)
	dir := t.TempDir()
	realRoot := filepath.Join(dir, "real-wt")
	linkRoot := filepath.Join(dir, "link-wt")
	require.NoError(t, os.MkdirAll(filepath.Join(realRoot, "internal"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(realRoot, probeFile), []byte("package live\n"), 0o644))
	// Creating a symlink on Windows needs a privilege the host may withhold,
	// which is a limitation of the host rather than a defect in the fold.
	// Everywhere else the failure is real.
	if err := os.Symlink(realRoot, linkRoot); err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("symlink creation unavailable on this Windows host: %v", err)
		}
		t.Fatal(err)
	}

	// The catalog holds the spelling git records — symlinks resolved — while
	// the probe arrives spelled the way the caller's shell handed it over.
	resolvedRoot, err := filepath.EvalSymlinks(realRoot)
	require.NoError(t, err)
	f.upsertWorktree(t, resolvedRoot, store_sqlite.CheckoutStateReady)
	f.routeWorktree(t)

	coverage, coverageErr := f.controller.FileCoverage(context.Background(), daemon.FileCoverageParams{
		Path: filepath.Join(linkRoot, probeFile),
	})
	require.NoError(t, coverageErr)
	assert.True(t, coverage.Covered, "the path names a file the composed view holds")
	assert.Equal(t, 1, coverage.Symbols)
}

// TestControllerAnswersTheFileCoverageVerb pins that the production controller
// is what the daemon dispatches the coverage kind to. Without it the verb
// exists on the wire and nothing on this side implements it.
func TestControllerAnswersTheFileCoverageVerb(t *testing.T) {
	var _ daemon.FileCoverageController = newProbeFixture(t).controller
}

func TestTopologyNudgeRunsImmediatelyAndKeepsATrailingEvent(t *testing.T) {
	testenv.Sandbox(t)
	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	calls := make(chan string, 3)
	releases := make(chan string, 4)
	callOrdinal := 0
	controller := &realController{
		// A recent probe nudge must not suppress a filesystem event.
		probeNudgedAt: map[string]time.Time{"family": time.Now()},
	}
	controller.probeReconcile = func(familyID string) {
		callOrdinal++
		calls <- familyID
		if callOrdinal == 1 {
			close(firstStarted)
			<-releaseFirst
		}
	}

	controller.nudgeFamilyTopologyRequest(context.Background(), "family", func() {
		releases <- "running"
	})
	select {
	case <-firstStarted:
	case <-time.After(time.Second):
		t.Fatal("topology reconciliation did not start promptly")
	}

	// A burst during the active pass is coalesced to exactly one trailing
	// reconciliation rather than discarded by the probe debounce. The latest
	// event's values survive cancellation so teardown reached by that trailing
	// pass can still identify its exact MultiWatcher dispatch.
	type topologyContextKey struct{}
	firstPending, cancel := context.WithCancel(context.WithValue(
		context.Background(), topologyContextKey{}, "first-pending"))
	controller.nudgeFamilyTopologyRequest(firstPending, "family", func() {
		releases <- "superseded"
	})
	cancel()
	controller.nudgeFamilyTopologyRequest(context.WithValue(
		context.Background(), topologyContextKey{}, "latest-pending"), "family", func() {
		releases <- "trailing"
	})
	select {
	case released := <-releases:
		assert.Equal(t, "superseded", released)
	case <-time.After(time.Second):
		t.Fatal("superseded pending lease was not released promptly")
	}
	controller.topologyNudgeMu.Lock()
	pendingRequest := controller.topologyNudges["family"].pending
	controller.topologyNudgeMu.Unlock()
	require.NotNil(t, pendingRequest)
	pendingContext := pendingRequest.ctx
	require.NotNil(t, pendingContext)
	assert.NoError(t, pendingContext.Err())
	assert.Equal(t, "latest-pending", pendingContext.Value(topologyContextKey{}))
	close(releaseFirst)

	for want := 0; want < 2; want++ {
		select {
		case familyID := <-calls:
			if familyID != "family" {
				t.Fatalf("reconciled family %q, want family", familyID)
			}
		case <-time.After(time.Second):
			t.Fatalf("received %d reconciliation calls, want two", want)
		}
	}
	seen := map[string]bool{}
	for len(seen) < 2 {
		select {
		case released := <-releases:
			seen[released] = true
		case <-time.After(time.Second):
			t.Fatalf("completed releases = %v, want running and trailing", seen)
		}
	}
	assert.True(t, seen["running"])
	assert.True(t, seen["trailing"])
	select {
	case familyID := <-calls:
		t.Fatalf("unexpected third reconciliation for %q", familyID)
	case released := <-releases:
		t.Fatalf("lease %q released more than once", released)
	case <-time.After(25 * time.Millisecond):
	}
}

func TestTopologyNudgeReleasesRejectedRequest(t *testing.T) {
	controller := &realController{}
	released := make(chan struct{}, 1)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	controller.nudgeFamilyTopologyRequest(ctx, "family", func() { released <- struct{}{} })
	select {
	case <-released:
	case <-time.After(time.Second):
		t.Fatal("rejected topology request retained its lease")
	}
}

func TestTopologyNudgePanicReleasesCurrentAndPending(t *testing.T) {
	controller := &realController{topologyNudges: make(map[string]*topologyNudgeState)}
	released := make(chan string, 2)
	current := newTopologyNudgeRequest(context.Background(), func() { released <- "current" })
	pending := newTopologyNudgeRequest(context.Background(), func() { released <- "pending" })
	controller.topologyNudges["family"] = &topologyNudgeState{pending: &pending}

	assert.Panics(t, func() {
		controller.runTopologyNudgeLoop(func(context.Context, string) {
			panic("topology reconcile panic")
		}, "family", current)
	})
	seen := map[string]bool{}
	for range 2 {
		select {
		case name := <-released:
			seen[name] = true
		case <-time.After(time.Second):
			t.Fatalf("panic releases = %v, want current and pending", seen)
		}
	}
	assert.True(t, seen["current"])
	assert.True(t, seen["pending"])
	assert.Empty(t, controller.topologyNudges)
}

func TestAttachWatcherSchedulesPendingPromotionWithoutWaitingForBuildGate(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		name := "context"
		if legacy {
			name = "legacy"
		}
		t.Run(name, func(t *testing.T) {
			testenv.Sandbox(t)
			fixture := newProbeFixture(t)
			gate := indexer.NewViewBuildGate()
			fixture.controller.lifecycle.SetBuildGate(gate)
			ctx := context.Background()
			transitionID := "attach-promotion-" + name
			require.NoError(t, fixture.catalog.BeginIntentTransition(ctx, store_sqlite.IntentTransition{
				TransitionID:       transitionID,
				CheckoutID:         probeWorktreeID,
				Cause:              "promote_checkout",
				PriorDesiredMode:   store_sqlite.CheckoutModeAutomatic,
				PriorEffectiveMode: store_sqlite.CheckoutModeAutomatic,
				RequestedMode:      store_sqlite.CheckoutModeDedicated,
				PriorCheckoutState: store_sqlite.CheckoutStateReady,
				State:              store_sqlite.IntentTransitionPending,
				CreatedAt:          100,
				LastProgress:       100,
			}))

			watcher, err := indexer.NewMultiWatcher(fixture.multi, map[string]config.WatchConfig{}, zap.NewNop())
			require.NoError(t, err)
			require.NoError(t, watcher.Start())
			t.Cleanup(func() {
				_ = fixture.controller.AttachWatcherContext(context.Background(), nil)
				require.NoError(t, watcher.Stop())
			})

			attached := make(chan error, 1)
			go func() {
				if legacy {
					fixture.controller.AttachWatcher(watcher)
					attached <- nil
					return
				}
				attached <- fixture.controller.AttachWatcherContext(ctx, watcher)
			}()
			select {
			case err := <-attached:
				require.NoError(t, err)
			case <-time.After(2 * time.Second):
				t.Fatal("AttachWatcher waited for the closed view-build gate")
			}
			require.False(t, gate.IsOpen(), "attachment must not open the daemon warmup gate")
			transitions, err := fixture.catalog.ListIntentTransitions(ctx)
			require.NoError(t, err)
			var pending *store_sqlite.IntentTransition
			for i := range transitions {
				if transitions[i].TransitionID == transitionID {
					pending = &transitions[i]
					break
				}
			}
			require.NotNil(t, pending, "durable transition disappeared before the gate opened")
			assert.Equal(t, store_sqlite.IntentTransitionPending, pending.State)
			assert.EqualValues(t, 100, pending.LastProgress)

			gate.Open()
			require.Eventually(t, func() bool {
				rows, listErr := fixture.catalog.ListIntentTransitions(ctx)
				if listErr != nil {
					return false
				}
				for _, row := range rows {
					if row.TransitionID == transitionID {
						return row.State != store_sqlite.IntentTransitionPending || row.LastProgress > 100
					}
				}
				return true
			}, 3*time.Second, 10*time.Millisecond,
				"opening the gate must let the transition admitted by AttachWatcher progress")
		})
	}
}

// TestAttachedWatcherDiscoversAndForgetsLinkedWorktree exercises the complete
// topology-event path with a real, disposable Git family. Nothing calls a
// reconciliation method directly: GitWatcher observes the administration
// changes, AttachWatcher resolves the family, and the lifecycle owns both the
// automatic route and its removal-grace cleanup.
func TestAttachedWatcherDiscoversAndForgetsLinkedWorktree(t *testing.T) {
	testenv.Sandbox(t)
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git is unavailable: %v", err)
	}

	controller, multi, catalog, dir := buildCatalogController(t)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		require.NoError(t, multi.Close(ctx))
	})
	store, ok := controller.graph.(*store_sqlite.Store)
	require.True(t, ok, "the fixture opened a %T, not the sqlite store", controller.graph)

	logger := zap.NewNop()
	controller.logger = logger
	lifecycle, err := indexer.NewCheckoutLifecycle(indexer.CheckoutLifecycleConfig{
		MultiIndexer:  multi,
		ConfigManager: controller.configManager,
		Graph:         store,
		Logger:        logger,
		Reconcile: reconcile.Config{
			AvailabilityGrace: time.Second,
			RemovalGrace:      5 * time.Second,
		},
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, lifecycle.Close()) })
	controller.lifecycle = lifecycle
	controller.viewMaterializer = &graphview.Materializer{
		Store:   store,
		Catalog: catalog,
		Leases:  lifecycle.ViewLeases(),
	}

	seedRoot := filepath.Join(dir, "topology-seed")
	commonDir := filepath.Join(dir, "topology.git")
	primaryRoot := filepath.Join(dir, "topology-primary")
	worktreeRoot := filepath.Join(dir, "topology-linked")

	// Keep the physical Git common directory outside the tracked worktree.
	// Removing the tracked primary can then erase its root while leaving a
	// durable family probe that authoritatively reports the worktree omission.
	require.NoError(t, os.MkdirAll(seedRoot, 0o755))
	runTopologyGitCommand(t, seedRoot, "init", "-b", "main")
	runTopologyGitCommand(t, seedRoot, "config", "user.email", "topology@example.invalid")
	runTopologyGitCommand(t, seedRoot, "config", "user.name", "Topology Test")
	require.NoError(t, os.WriteFile(
		filepath.Join(seedRoot, "main.go"),
		[]byte("package topology\n\nfunc TopologyBase() {}\n"),
		0o644,
	))
	runTopologyGitCommand(t, seedRoot, "add", "main.go")
	runTopologyGitCommand(t, seedRoot, "commit", "-m", "seed topology fixture")
	require.NoError(t, os.MkdirAll(commonDir, 0o755))
	runTopologyGitCommand(t, commonDir, "init", "--bare")
	runTopologyGitCommand(t, seedRoot, "remote", "add", "origin", commonDir)
	runTopologyGitCommand(t, seedRoot, "push", "origin", "main")
	runTopologyGitCommand(t, commonDir, "symbolic-ref", "HEAD", "refs/heads/main")
	runTopologyGitCommand(t, commonDir, "worktree", "add", primaryRoot, "main")

	// Start from the stale startup snapshot: the watcher saw no configured
	// repositories when it was built. A Track racing this warmup must persist
	// its durable state, report the process-local gap, and let attachment repair
	// that exact prefix synchronously.
	watcher, err := indexer.NewMultiWatcher(multi, map[string]config.WatchConfig{}, logger)
	require.NoError(t, err)
	require.NoError(t, watcher.Start())
	t.Cleanup(func() {
		controller.AttachWatcher(nil)
		require.NoError(t, watcher.Stop())
	})

	ctx := context.Background()
	payload, err := controller.Track(ctx, daemon.TrackParams{
		Path: primaryRoot,
		Name: "topology-event",
	})
	require.ErrorIs(t, err, indexer.ErrWatcherUnavailable)
	assert.Empty(t, payload, "Track must not report success before its watcher is live")

	metadata := multi.AllMetadata()
	require.Len(t, metadata, 1, "failed watcher attachment must retain the indexed repository")
	var trackedPrefix string
	for prefix := range metadata {
		trackedPrefix = prefix
	}
	require.NotEmpty(t, trackedPrefix)
	global := controller.configManager.Global()
	require.NotNil(t, global)
	require.Len(t, global.Repos, 1, "failed watcher attachment must retain durable config")
	var configuredPath string
	for _, repo := range global.Repos {
		configuredPath = repo.Path
	}
	assert.Equal(t, primaryRoot, configuredPath)

	graphRow, found, err := catalog.GetDedicatedGraph(ctx, indexer.GraphIDFor(trackedPrefix))
	require.NoError(t, err)
	require.True(t, found, "failed watcher attachment must retain the dedicated graph")
	registration := indexer.RegisterResult{
		Prefix:     trackedPrefix,
		GraphID:    graphRow.GraphID,
		CheckoutID: graphRow.OwnerCheckoutID,
		FamilyID:   graphRow.FamilyID,
	}
	liveWatchers, _ := watcher.WatchedRepos()
	require.Zero(t, liveWatchers, "the stale startup snapshot must remain empty before AttachWatcher")

	require.NoError(t, controller.AttachWatcherContext(ctx, watcher))
	require.Eventually(t, func() bool {
		count, _ := watcher.WatchedRepos()
		return count == 1
	}, 5*time.Second, 10*time.Millisecond,
		"AttachWatcher must repair the durable configured prefix")
	require.NoError(t, controller.AttachWatcherContext(ctx, watcher))
	require.Eventually(t, func() bool {
		count, _ := watcher.WatchedRepos()
		return count == 1
	}, 5*time.Second, 10*time.Millisecond,
		"attaching the same watcher twice must converge idempotently")

	alreadyPayload, err := controller.Track(ctx, daemon.TrackParams{
		Path: primaryRoot,
		Name: "topology-event",
	})
	require.NoError(t, err)
	var already struct {
		Status string `json:"status"`
	}
	require.NoError(t, json.Unmarshal(alreadyPayload, &already))
	assert.Equal(t, "already_tracked", already.Status)

	// AlreadyTracked is also a repair path: a lost process-local membership must
	// be restored without rebuilding or requiring another filesystem event.
	require.NoError(t, watcher.RemoveRepo(trackedPrefix))
	liveWatchers, _ = watcher.WatchedRepos()
	require.Zero(t, liveWatchers)
	repairedPayload, err := controller.Track(ctx, daemon.TrackParams{
		Path: primaryRoot,
		Name: "topology-event",
	})
	require.NoError(t, err)
	var repaired struct {
		Status string `json:"status"`
	}
	require.NoError(t, json.Unmarshal(repairedPayload, &repaired))
	assert.Equal(t, "already_tracked", repaired.Status)
	liveWatchers, _ = watcher.WatchedRepos()
	require.Equal(t, 1, liveWatchers, "AlreadyTracked must repair missing watcher membership")

	// AttachWatcher intentionally raises one startup reconciliation. Let it
	// drain so the assertion below proves a later filesystem event, rather
	// than the startup safety nudge, discovered the linked worktree.
	require.Eventually(t, func() bool {
		controller.topologyNudgeMu.Lock()
		defer controller.topologyNudgeMu.Unlock()
		return len(controller.topologyNudges) == 0
	}, 5*time.Second, 10*time.Millisecond, "startup topology nudge did not drain")

	runTopologyGitCommand(t, primaryRoot,
		"worktree", "add", "-b", "topology-linked", worktreeRoot)
	canonicalWorktreeRoot, err := filepath.EvalSymlinks(worktreeRoot)
	require.NoError(t, err)
	worktreeFile := filepath.Join(canonicalWorktreeRoot, "main.go")

	var automaticCheckoutID string
	var automaticCommitGenerationID, automaticDirtyGenerationID int64
	require.Eventually(t, func() bool {
		binding, explainErr := lifecycle.ExplainView(ctx, worktreeFile)
		if explainErr != nil || !binding.Matched || !binding.Composed ||
			binding.EffectiveMode != string(store_sqlite.CheckoutModeAutomatic) {
			return false
		}
		route, found, routeErr := catalog.GetCheckoutRoute(ctx, binding.CheckoutID)
		if routeErr != nil || !found || route.State != store_sqlite.RouteActive {
			return false
		}
		automaticCheckoutID = binding.CheckoutID
		automaticCommitGenerationID = route.CommitGenerationID
		automaticDirtyGenerationID = route.DirtyGenerationID
		return true
	}, 20*time.Second, 20*time.Millisecond,
		"the post-attachment worktree event did not publish an automatic route")
	require.NotEmpty(t, automaticCheckoutID)
	require.Greater(t, automaticCommitGenerationID, int64(0))
	require.Greater(t, automaticDirtyGenerationID, int64(0))
	require.NotEqual(t, automaticCommitGenerationID, automaticDirtyGenerationID)

	checkout, found, err := catalog.GetCheckout(ctx, automaticCheckoutID)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, store_sqlite.CheckoutModeAutomatic, checkout.DesiredMode)
	assert.Equal(t, store_sqlite.CheckoutModeAutomatic, checkout.EffectiveMode)

	routed, err := controller.SearchSymbols(ctx, daemon.SearchSymbolsParams{
		Query: "TopologyBase",
		Path:  worktreeFile,
	})
	require.NoError(t, err)
	require.NotNil(t, routed.View)
	assert.Equal(t, daemon.ProbeViewWorktree, routed.View.Kind)
	assert.Equal(t, automaticCheckoutID, routed.View.CheckoutID)
	assert.True(t, routed.View.Exact)
	assert.Equal(t, []string{"TopologyBase"}, symbolNames(routed.Hits))

	runTopologyGitCommand(t, primaryRoot,
		"worktree", "remove", "--force", worktreeRoot)

	// The event-driven pass withdraws the exact route immediately and leaves
	// only the explicitly labelled, read-only family-base fallback while the
	// deliberately short grace is running.
	require.Eventually(t, func() bool {
		checkout, checkoutFound, checkoutErr := catalog.GetCheckout(ctx, automaticCheckoutID)
		if checkoutErr != nil || !checkoutFound ||
			checkout.State != store_sqlite.CheckoutStateRemovalGrace {
			return false
		}
		route, routeFound, routeErr := catalog.GetCheckoutRoute(ctx, automaticCheckoutID)
		return routeErr == nil && (!routeFound || route.State != store_sqlite.RouteActive)
	}, 10*time.Second, 20*time.Millisecond,
		"worktree removal did not withdraw its exact route into removal grace")

	graceCheckout, graceFound, err := catalog.GetCheckout(ctx, automaticCheckoutID)
	require.NoError(t, err)
	require.True(t, graceFound)
	graceBinding, err := lifecycle.ExplainView(ctx, worktreeFile)
	require.NoError(t, err)
	require.True(t, graceBinding.Matched,
		"removal-grace checkout stopped owning its path; checkout=%+v binding=%+v",
		graceCheckout, graceBinding)
	assert.Equal(t, automaticCheckoutID, graceBinding.CheckoutID)
	assert.Equal(t, string(store_sqlite.CheckoutStateRemovalGrace), graceBinding.CheckoutState)

	fallback, err := controller.SearchSymbols(ctx, daemon.SearchSymbolsParams{
		Query: "TopologyBase",
		Path:  worktreeFile,
	})
	require.NoError(t, err)
	require.NotNil(t, fallback.View)
	assert.Equal(t, daemon.ProbeViewBase, fallback.View.Kind)
	assert.False(t, fallback.View.Exact)
	assert.Equal(t, string(store_sqlite.CheckoutStateRemovalGrace), fallback.View.FallbackReason)

	// ReconcileFamily scheduled its own deadline retry. Once it fires, both
	// logical objects are gone without a janitor tick or a manual reconcile.
	require.Eventually(t, func() bool {
		_, checkoutFound, checkoutErr := catalog.GetCheckout(ctx, automaticCheckoutID)
		_, routeFound, routeErr := catalog.GetCheckoutRoute(ctx, automaticCheckoutID)
		return checkoutErr == nil && routeErr == nil && !checkoutFound && !routeFound
	}, 10*time.Second, 20*time.Millisecond,
		"removal-grace retry did not forget the checkout and route")

	// Route removal is not enough: the checkout-unique dirty generation and
	// every payload row it owns must be retired too. The canonical commit
	// generation is family-shared cache state, so forgetting this one worktree
	// must leave it available for a later checkout of the same tree.
	require.Eventually(t, func() bool {
		_, dirtyFound, generationErr := catalog.GetViewGeneration(ctx, automaticDirtyGenerationID)
		return generationErr == nil && !dirtyFound
	}, 5*time.Second, 20*time.Millisecond,
		"forgotten worktree retained its dirty generation payload")
	_, commitFound, err := catalog.GetViewGeneration(ctx, automaticCommitGenerationID)
	require.NoError(t, err)
	assert.True(t, commitFound, "forgetting one worktree removed the shared commit generation")

	_, primaryFound, err := catalog.GetCheckout(ctx, registration.CheckoutID)
	require.NoError(t, err)
	assert.True(t, primaryFound, "forgetting an automatic worktree removed its dedicated primary")
	primaryGraph, primaryGraphFound, err := catalog.GetDedicatedGraph(ctx, registration.GraphID)
	require.NoError(t, err)
	require.True(t, primaryGraphFound, "forgetting an automatic worktree removed its primary graph")
	assert.Equal(t, registration.CheckoutID, primaryGraph.OwnerCheckoutID)

	// The linked watcher is gone, so the primary is again this family's sole
	// topology owner. Remove that source too, and let its last event drain while
	// the root is still healthy. The subsequent attachment therefore has an
	// empty watcher snapshot and can discover the family only from durable
	// catalog ownership.
	require.NoError(t, watcher.RemoveRepo(trackedPrefix))
	liveWatchers, _ = watcher.WatchedRepos()
	require.Zero(t, liveWatchers)
	require.Eventually(t, func() bool {
		controller.topologyNudgeMu.Lock()
		defer controller.topologyNudgeMu.Unlock()
		return len(controller.topologyNudges) == 0
	}, 5*time.Second, 10*time.Millisecond,
		"source-removal topology reconciliation did not drain while the primary was healthy")

	// Remove the primary through Git's own administration, then re-register the
	// real controller callback. Reconciliation must seed from the catalog even
	// though registration can no longer snapshot a filesystem source. The
	// durable common directory authoritatively omits the worktree; the ordinary
	// removal grace and its scheduled retry then remove the current owner through
	// the retained async lease.
	runTopologyGitCommand(t, commonDir, "worktree", "remove", "--force", primaryRoot)
	type topologyResult struct {
		report reconcile.FamilyReport
		err    error
	}
	topologyResults := make(chan topologyResult, 4)
	controller.topologyReconcile = func(runCtx context.Context, familyID string) {
		report, reconcileErr := lifecycle.ReconcileFamily(runCtx, familyID)
		select {
		case topologyResults <- topologyResult{report: report, err: reconcileErr}:
		default:
		}
	}
	controller.AttachWatcher(watcher)

	select {
	case result := <-topologyResults:
		require.NoError(t, result.err)
		assert.Equal(t, registration.FamilyID, result.report.FamilyID)
		assert.True(t, result.report.InventoryUsable,
			"catalog-seeded reconciliation did not use the durable Git common directory")
	case <-time.After(5 * time.Second):
		t.Fatal("catalog-owned family was not submitted to topology reconciliation")
	}

	require.Eventually(t, func() bool {
		_, checkoutFound, checkoutErr := catalog.GetCheckout(ctx, registration.CheckoutID)
		_, graphFound, graphErr := catalog.GetDedicatedGraph(ctx, registration.GraphID)
		return checkoutErr == nil && graphErr == nil && !checkoutFound && !graphFound
	}, 15*time.Second, 20*time.Millisecond,
		"current-owner topology reconciliation did not forget the vanished primary")
	require.Eventually(t, func() bool {
		controller.topologyNudgeMu.Lock()
		defer controller.topologyNudgeMu.Unlock()
		return len(controller.topologyNudges) == 0
	}, 5*time.Second, 10*time.Millisecond,
		"current-owner topology reconciliation did not release its retained lease")

	// Quiesce in production teardown order before testing.TempDir starts
	// removing config and store paths. Registered cleanups repeat these
	// idempotent stops, then detach the already-stopped watcher pointer.
	controller.StopWatcher()
	require.NoError(t, lifecycle.Close())
	controller.AttachWatcher(nil)
}

// TestRetainedTopologyReconcileRemovesWholeWatcherFamilyWithoutCycle pins the
// controller/indexer ownership seam. The controller retains owner A's dispatch
// while its reconcile removes A and then B. Promoting B before A releases would
// enqueue a B request behind this same family single-flight; removing B would
// then wait for a lease that only this blocked reconcile can release.
func TestRetainedTopologyReconcileRemovesWholeWatcherFamilyWithoutCycle(t *testing.T) {
	testenv.Sandbox(t)
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git is unavailable: %v", err)
	}

	controller, multi, catalog, dir := buildCatalogController(t)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		require.NoError(t, multi.Close(ctx))
	})
	store, ok := controller.graph.(*store_sqlite.Store)
	require.True(t, ok, "the fixture opened a %T, not the sqlite store", controller.graph)

	logger := zap.NewNop()
	controller.logger = logger
	lifecycle, err := indexer.NewCheckoutLifecycle(indexer.CheckoutLifecycleConfig{
		MultiIndexer:  multi,
		ConfigManager: controller.configManager,
		Graph:         store,
		Logger:        logger,
		Reconcile: reconcile.Config{
			AvailabilityGrace: time.Second,
			RemovalGrace:      5 * time.Second,
		},
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, lifecycle.Close()) })
	controller.lifecycle = lifecycle
	controller.viewMaterializer = &graphview.Materializer{
		Store:   store,
		Catalog: catalog,
		Leases:  lifecycle.ViewLeases(),
	}

	primaryRoot := filepath.Join(dir, "topology-cycle-primary")
	worktreeRoot := filepath.Join(dir, "topology-cycle-linked")
	require.NoError(t, os.MkdirAll(primaryRoot, 0o755))
	runTopologyGitCommand(t, primaryRoot, "init", "-b", "main")
	runTopologyGitCommand(t, primaryRoot, "config", "user.email", "topology@example.invalid")
	runTopologyGitCommand(t, primaryRoot, "config", "user.name", "Topology Test")
	require.NoError(t, os.WriteFile(
		filepath.Join(primaryRoot, "main.go"),
		[]byte("package topology\n\nfunc TopologyCycle() {}\n"),
		0o644,
	))
	runTopologyGitCommand(t, primaryRoot, "add", "main.go")
	runTopologyGitCommand(t, primaryRoot, "commit", "-m", "seed topology cycle fixture")

	// Runtime tracking now requires the already-published watcher registry.
	// Start it from the same stale-empty warmup snapshot as daemon startup;
	// each Register call below must dynamically ensure its own live member.
	watcher, err := indexer.NewMultiWatcher(multi, map[string]config.WatchConfig{}, logger)
	require.NoError(t, err)
	require.NoError(t, watcher.Start())
	t.Cleanup(func() {
		controller.AttachWatcher(nil)
		require.NoError(t, watcher.Stop())
	})
	// Register through the real watcher source before enabling topology
	// callbacks. Creating B emits Git admin events; reconciling them while B's
	// explicit promotion is publishing would make the fixture race itself.
	lifecycle.SetWatcherSource(func() indexer.RepoWatcher { return watcher })

	ctx := context.Background()
	registration, err := lifecycle.Register(ctx, config.RepoEntry{
		Path: primaryRoot,
		Name: "topology-cycle",
	}, indexer.TrackSourceCLI)
	require.NoError(t, err)
	require.False(t, registration.Pending)
	require.NotEmpty(t, registration.FamilyID)

	// Explicitly register B through the attached dynamic registry. Automatic
	// worktrees use the primary corpus prefix; this regression needs two
	// physical watcher members in one family so ownership can transfer A to B.
	runTopologyGitCommand(t, primaryRoot,
		"worktree", "add", "-b", "topology-cycle-linked", worktreeRoot)
	linkedRegistration, err := lifecycle.Register(ctx, config.RepoEntry{
		Path: worktreeRoot,
		Name: "topology-cycle-linked",
	}, indexer.TrackSourceCLI)
	require.NoError(t, err)
	require.False(t, linkedRegistration.Pending)
	require.Equal(t, registration.FamilyID, linkedRegistration.FamilyID)
	require.NotEqual(t, registration.Prefix, linkedRegistration.Prefix)

	ownerPrefix, survivorPrefix := registration.Prefix, linkedRegistration.Prefix
	if survivorPrefix < ownerPrefix {
		ownerPrefix, survivorPrefix = survivorPrefix, ownerPrefix
	}

	firstRemoval := make(chan error, 1)
	removeSurvivor := make(chan struct{})
	type transferResult struct {
		familyID string
		err      error
	}
	results := make(chan transferResult, 1)
	var reconcileCalls atomic.Int32
	controller.topologyReconcile = func(runCtx context.Context, familyID string) {
		if reconcileCalls.Add(1) != 1 {
			return
		}
		firstErr := watcher.RemoveRepoContext(runCtx, ownerPrefix)
		firstRemoval <- firstErr
		if firstErr != nil {
			results <- transferResult{familyID: familyID, err: firstErr}
			return
		}
		<-removeSurvivor
		results <- transferResult{
			familyID: familyID,
			err:      watcher.RemoveRepoContext(runCtx, survivorPrefix),
		}
	}
	controller.AttachWatcher(watcher)

	select {
	case firstErr := <-firstRemoval:
		require.NoError(t, firstErr, "retained reconcile could not remove its owner")
	case <-time.After(10 * time.Second):
		t.Fatal("retained reconcile did not remove its owner")
	}

	// GitWatcher debounces a promoted-owner nudge for 300ms. Hold A's request
	// for five windows. A legacy immediate transfer deterministically installs
	// B as this family's pending request; take and finish that request before
	// failing so the regression cannot wedge the rest of the test process.
	var legacyPending *topologyNudgeRequest
	deadline := time.NewTimer(1500 * time.Millisecond)
	ticker := time.NewTicker(5 * time.Millisecond)
observePending:
	for {
		select {
		case <-ticker.C:
			controller.topologyNudgeMu.Lock()
			if state := controller.topologyNudges[registration.FamilyID]; state != nil && state.pending != nil {
				legacyPending = state.pending
				state.pending = nil
			}
			controller.topologyNudgeMu.Unlock()
			if legacyPending != nil {
				break observePending
			}
		case <-deadline.C:
			break observePending
		}
	}
	if !deadline.Stop() {
		select {
		case <-deadline.C:
		default:
		}
	}
	ticker.Stop()
	if legacyPending != nil {
		legacyPending.finish()
	}
	close(removeSurvivor)

	select {
	case result := <-results:
		require.Equal(t, registration.FamilyID, result.familyID)
		require.NoError(t, result.err, "same retained reconcile could not remove the survivor")
	case <-time.After(10 * time.Second):
		t.Fatal("removing the survivor deadlocked behind its own pending controller request")
	}
	if legacyPending != nil {
		require.Nil(t, legacyPending.lease,
			"owner transfer retained a watcher lease behind the active family reconcile")
	}
	require.Equal(t, int32(1), reconcileCalls.Load(),
		"removing the whole family should not start a descendant reconcile")
	require.Eventually(t, func() bool {
		controller.topologyNudgeMu.Lock()
		defer controller.topologyNudgeMu.Unlock()
		return len(controller.topologyNudges) == 0
	}, 5*time.Second, 10*time.Millisecond, "controller retained stale topology nudge state")

	stopDone := make(chan struct{})
	go func() {
		controller.StopWatcher()
		close(stopDone)
	}()
	select {
	case <-stopDone:
	case <-time.After(10 * time.Second):
		t.Fatal("controller watcher stop blocked after whole-family topology removal")
	}
	require.NoError(t, lifecycle.Close())
	controller.AttachWatcher(nil)
}

func runTopologyGitCommand(t *testing.T, dir string, args ...string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	output, err := cmd.CombinedOutput()
	require.NoErrorf(t, err, "git %v in %s failed:\n%s", args, dir, output)
}
