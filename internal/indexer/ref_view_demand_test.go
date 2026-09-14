package indexer

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/gitstate"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// The ref view's half of the committed-base consumer gate.
//
// A committed base is published only when something can read it, and a ref
// view is one of the two readers that gate counts
// (CheckoutLifecycle.dedicatedBaseConsumers): RefViewManager.base resolves its
// lower snapshot through the same graphBase a dependent checkout's commit layer
// does. The census alone is not enough, though — it is read INSIDE a
// publication attempt, so a family whose only reader is a ref view created on a
// RUNNING daemon has nothing that ever starts an attempt. A daemon start
// already declined it and a HEAD movement declines it too, and on an idle-HEAD
// repository that is forever.
//
// These tests pin the demand that closes it: the unpublished arm asks, once;
// the published arm asks nothing and names the base; the ask is raised late
// enough in the selection that the census it triggers COUNTS the asking view;
// a selector that resolves to nothing asks for nothing; the production door
// (CheckoutLifecycle.EnsureRefView) reaches the publisher with nothing stubbed;
// and concurrent selections ask once between them.

// demandRecorder counts what the manager asked the publisher for.
type demandRecorder struct {
	mu       sync.Mutex
	prefixes []string
}

func (d *demandRecorder) request(prefix string) {
	d.mu.Lock()
	d.prefixes = append(d.prefixes, prefix)
	d.mu.Unlock()
}

func (d *demandRecorder) asked() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.prefixes...)
}

// demandingManager builds a ref-view manager whose RequestBase is recorded
// rather than wired to a publisher, which is the manager shape ref_view_service
// builds in production minus the daemon behind it.
func demandingManager(t *testing.T, f *refViewFixture) (*RefViewManager, *demandRecorder) {
	t.Helper()
	demands := &demandRecorder{}
	manager := f.managerTuned(t, nil, func(cfg *RefViewManagerConfig) {
		cfg.RequestBase = demands.request
	})
	return manager, demands
}

// publishCommittedBase gives the fixture's dedicated graph a published base
// generation, the way InitialBasePublisher.publish would: a servable dedicated
// generation owned by the graph's own owner checkout, named by the graph's
// active pointer. It is written through the catalog's own API — graphBase
// validates ownership, kind, state and tree, so a row that does not satisfy all
// four would be refused rather than served.
func publishCommittedBase(t *testing.T, f *refViewFixture) int64 {
	t.Helper()
	ctx := context.Background()
	id, err := f.catalog.CreateViewGeneration(ctx, store_sqlite.ViewGeneration{
		OwnerKind:         checkoutLayerOwnerKind,
		GraphID:           f.graphID,
		CheckoutID:        f.checkoutID,
		GenerationKind:    "dedicated",
		TreeOID:           f.treeA,
		ConfigHash:        "demand-config",
		ExtractorVersions: "demand-extractors",
		ResolverVersion:   "demand-resolver",
		State:             store_sqlite.ViewGenerationBuilding,
		CreatedAt:         time.Now().Unix(),
	})
	require.NoError(t, err)
	require.Positive(t, id)
	require.NoError(t, f.catalog.PublishViewGeneration(ctx, id, time.Now().Unix()))

	graph, found, err := f.catalog.GetDedicatedGraph(ctx, f.graphID)
	require.NoError(t, err)
	require.True(t, found)
	graph.ActiveGenerationID = id
	require.NoError(t, f.catalog.UpsertDedicatedGraph(ctx, graph))
	return id
}

// TestARefViewOnAnUnpublishedFamilyAsksForTheCommittedBaseOnce is the demand
// itself.
//
// Two selections of one branch on a family whose primary has published nothing
// ask exactly once — the throttle, which exists because selection is the only
// thing that ever notices a ref moved and a client polling a building view
// would otherwise re-enter the publication protocol once per poll. And the view
// it serves meanwhile is TRUTHFULLY labelled: the generation it built names no
// committed ancestor, which is what composing over the mutable corpus is.
func TestARefViewOnAnUnpublishedFamilyAsksForTheCommittedBaseOnce(t *testing.T) {
	f := newRefViewFixture(t)
	commitB, treeB := f.commitTree(builderTreeB(), "B")
	f.setRef("refs/heads/feature", commitB)

	manager, demands := demandingManager(t, f)
	ctx := context.Background()

	first, err := manager.EnsureRefView(ctx, f.request("refs/heads/feature"))
	require.NoError(t, err)
	require.Equal(t, store_sqlite.RefViewReady, first.State)
	require.Positive(t, first.GenerationID)

	require.Equal(t, []string{builderRepoPrefix}, demands.asked(),
		"the first selection over an unpublished primary did not ask for a committed base")

	row := f.generation(first.GenerationID)
	require.Zero(t, row.BaseGenerationID,
		"the view was served as if it had a committed ancestor it does not have")
	require.Equal(t, treeB, row.TreeOID)

	second, err := manager.EnsureRefView(ctx, f.request("refs/heads/feature"))
	require.NoError(t, err)
	require.False(t, second.Built)
	require.Equal(t, first.GenerationID, second.GenerationID)

	require.Equal(t, []string{builderRepoPrefix}, demands.asked(),
		"a second selection inside the throttle window re-entered the publication protocol")
}

// TestARefViewOverAPublishedBaseComposesOverItAndAsksForNothing is the other
// side of the gate: once the family has a committed base there is nothing to
// demand, and the view keys on it.
//
// The fingerprint assertion is what makes the demand sufficient rather than
// merely polite. A view built while the base was generation 0 does not stay
// there: the base is part of the generation identity, so the fingerprint the
// next selection computes differs, activeIsCurrent says no, and the view
// rebuilds over the published base. Nothing has to notice the publication for
// itself.
func TestARefViewOverAPublishedBaseComposesOverItAndAsksForNothing(t *testing.T) {
	f := newRefViewFixture(t)
	commitB, treeB := f.commitTree(builderTreeB(), "B")
	f.setRef("refs/heads/feature", commitB)

	// Published BEFORE the manager exists, so the throttle cannot be what
	// explains a silent selection: this manager has never asked for anything.
	generationID := publishCommittedBase(t, f)
	manager, demands := demandingManager(t, f)
	ctx := context.Background()

	published, owed, err := manager.base(ctx, f.graphID)
	require.NoError(t, err)
	require.Equal(t, generationID, published.generationID,
		"the view does not compose over the committed base the family now has")
	require.Equal(t, f.treeA, published.treeOID)
	require.Equal(t, f.graphID, published.graphID)
	require.Empty(t, owed,
		"a family that already has a committed base was named as owing one")

	// And the whole selection stays silent, not merely the read: a family with
	// a base never reaches demandCommittedBase at all.
	_, err = manager.EnsureRefView(ctx, f.request("refs/heads/feature"))
	require.NoError(t, err)
	require.Empty(t, demands.asked(),
		"a family that already has a committed base was asked for another")

	unpublished := primaryBase{graphID: f.graphID, treeOID: f.treeA}
	req := f.request("refs/heads/feature")
	req.EnrichmentProfile = defaultEnrichmentProfile
	viewID := refViewID(req)
	over0 := refViewBuildFingerprint(manager.identity(ctx, req, viewID, unpublished, treeB), req.EnrichmentProfile)
	overBase := refViewBuildFingerprint(manager.identity(ctx, req, viewID, published, treeB), req.EnrichmentProfile)
	require.NotEqual(t, over0, overBase,
		"a view built over generation 0 would keep serving it after the base landed")
}

// TestARefViewSelectionAsksTheDaemonForTheDeferredCommittedBase is the wiring
// proof.
//
// The primitive works in isolation above, with RequestBase stubbed. What that
// cannot catch is the state this item would otherwise be in: a manager that
// asks and a ref_view_service that never wired RequestBase, which is a base
// that is never published at all — exactly the shape the F1 verifier found.
//
// So this drives the production chain with nothing stubbed:
//
//	InitialBasePublisher.publish declines the owner-only family
//	  -> a ref view of it is selected on the running daemon
//	  -> CheckoutLifecycle.EnsureRefView -> refViewManager wires RequestBase
//	  -> RefViewManager.base's generation-0 arm -> demandCommittedBase
//	  -> CheckoutLifecycle.requestDedicatedBase -> the advance registry
//	  -> InitialBasePublisher.RequestBase -> publish -> adoption
//
// It asserts on the outcome rather than on a count: the publication runs on the
// publisher's own worker, and what has to be true is that the base the daemon
// deferred gets published, that the publication says a CONSUMER asked for it,
// and that it is adopted.
func TestARefViewSelectionAsksTheDaemonForTheDeferredCommittedBase(t *testing.T) {
	f := newLifecycleFixture(t)
	defer f.close()
	installStartupPublisherRuntime(t, f)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	root := f.gitRepo("refview-demand")
	registered, err := f.lc.Register(ctx, config.RepoEntry{Path: root}, TrackSourceCLI)
	require.NoError(t, err)
	require.NotEmpty(t, registered.Prefix)

	publisher := startupPublisher(t, f)
	publisher.Schedule(registered.Prefix)
	publisher.BeginDraining()
	require.NoError(t, publisher.Wait(ctx))
	outcomes := publisher.Outcomes()
	require.Len(t, outcomes, 1)
	require.Equal(t, "no dependent checkout", outcomes[0].Skipped,
		"the premise: the daemon start deferred this family's committed base")
	require.Zero(t, f.familyOf(registered.Prefix).ActiveGenerationID)

	// The selection itself. It answers over generation 0 — ready or building,
	// both are legitimate — and what it must not do is answer without asking.
	_, err = f.lc.EnsureRefView(ctx, RefViewSelection{
		GraphID:       GraphIDFor(registered.Prefix),
		SelectorKind:  gitstate.ViewSelectorGitRef,
		SelectorValue: "refs/heads/main",
	})
	require.NoError(t, err)

	deadline := time.Now().Add(90 * time.Second)
	var demanded InitialBasePublication
	for {
		for _, outcome := range publisher.Outcomes() {
			if outcome.Demanded {
				demanded = outcome
			}
		}
		if demanded.RepoPrefix != "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("a ref view never asked for the deferred committed base; outcomes=%+v",
				publisher.Outcomes())
		}
		time.Sleep(20 * time.Millisecond)
	}
	require.NoError(t, demanded.Err)
	require.Empty(t, demanded.Skipped, "the demanded publication was declined")
	require.Positive(t, demanded.GenerationID)
	require.Equal(t, registered.Prefix, demanded.RepoPrefix)
	require.Equal(t, demanded.GenerationID, f.familyOf(registered.Prefix).ActiveGenerationID,
		"the on-demand publication was not adopted")
}

// TestConcurrentRefViewSelectionsAskForTheCommittedBaseOnce is the race leg.
//
// The ask happens on the selection's own goroutine, before anything else the
// selection does, and a manager serves every selection of its repository at
// once. So the throttle is shared mutable state on the hottest path the manager
// has, and "exactly one ask" has to hold when the selections are simultaneous
// and not merely sequential.
func TestConcurrentRefViewSelectionsAskForTheCommittedBaseOnce(t *testing.T) {
	f := newRefViewFixture(t)
	commitB, _ := f.commitTree(builderTreeB(), "B")
	f.setRef("refs/heads/feature", commitB)

	manager, demands := demandingManager(t, f)
	ctx := context.Background()

	const selections = 8
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < selections; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if _, err := manager.EnsureRefView(ctx, f.request("refs/heads/feature")); err != nil {
				t.Errorf("selection: %v", err)
			}
		}()
	}
	close(start)
	wg.Wait()

	require.Equal(t, []string{builderRepoPrefix}, demands.asked(),
		"%d concurrent selections of an unpublished family did not coalesce onto one ask", selections)
}

// TestARefViewManagerWithNoPublisherStaysInTheLegacyRegime states the nil
// contract: a manager built without RequestBase — a test manager, or any caller
// that is not the lifecycle — serves the same generation-0 base it always did
// and does not panic reaching for a publisher it was never given.
func TestARefViewManagerWithNoPublisherStaysInTheLegacyRegime(t *testing.T) {
	f := newRefViewFixture(t)
	manager := f.manager(t, nil)

	ctx := context.Background()
	base, owed, err := manager.base(ctx, f.graphID)
	require.NoError(t, err)
	require.Zero(t, base.generationID)
	require.Equal(t, f.treeA, base.treeOID)
	require.Equal(t, f.graphID, base.graphID)
	require.Equal(t, builderRepoPrefix, owed,
		"the read half stopped naming the repository that is owed a base")

	// And the selection that acts on that debt with no publisher behind it
	// serves the legacy regime rather than dereferencing one.
	result, err := manager.EnsureRefView(ctx, f.request("refs/heads/main"))
	require.NoError(t, err)
	require.Equal(t, store_sqlite.RefViewReady, result.State)
}

// TestRefViewDemandNamesTheGraphsOwnRepository guards the argument the demand
// carries. RequestBase is keyed on a REPOSITORY PREFIX and the publisher
// resolves the graph back from it (GraphIDFor), so a demand that named anything
// else — the graph id, the view id — would publish for the wrong repository or
// for none.
func TestRefViewDemandNamesTheGraphsOwnRepository(t *testing.T) {
	f := newRefViewFixture(t)
	commitB, _ := f.commitTree(builderTreeB(), "B")
	f.setRef("refs/heads/feature", commitB)
	manager, demands := demandingManager(t, f)

	_, err := manager.EnsureRefView(context.Background(), f.request("refs/heads/feature"))
	require.NoError(t, err)

	asked := demands.asked()
	require.Len(t, asked, 1)
	require.Equal(t, builderRepoPrefix, asked[0])
	require.NotEqual(t, f.graphID, asked[0])
	require.Equal(t, f.graphID, GraphIDFor(asked[0]),
		"the prefix the demand named does not resolve back to the graph that wanted the base")
}

// TestARefViewDemandIsVisibleToTheConsumerCensusItTriggers is the ORDER, and
// it is the whole reason the ask is not raised where the absence is noticed.
//
// The publication a ref view demands is itself consumer-gated: the publisher
// worker re-reads CheckoutLifecycle.dedicatedBaseConsumers (checkout_lifecycle.go),
// whose ref-view arm is ListRefViews(graph) len > 0, and skips with "no
// dependent checkout" when it counts none. So a demand raised BEFORE the
// view's catalog row is written can be declined by the very census it exists
// to satisfy — and the demand is then SPENT: nothing is queued on the
// publisher any more and the throttle refuses the next ask for
// dedicatedBaseDemandInterval, while nothing re-selects a ref view on its own.
// A one-shot client would end with no committed base at all.
//
// This runs the real predicate, not a proxy for it, at the exact instant the
// demand fires.
func TestARefViewDemandIsVisibleToTheConsumerCensusItTriggers(t *testing.T) {
	f := newRefViewFixture(t)
	commitB, _ := f.commitTree(builderTreeB(), "B")
	f.setRef("refs/heads/feature", commitB)
	ctx := context.Background()

	var (
		mu        sync.Mutex
		asks      int
		consumers bool
		censusErr error
		rows      []string
	)
	manager := f.managerTuned(t, nil, func(cfg *RefViewManagerConfig) {
		cfg.RequestBase = func(string) {
			// The publisher's own gate, evaluated from the catalog as the
			// worker would evaluate it. A lifecycle with nothing but the
			// catalog is all dedicatedBaseConsumers reads.
			graph, found, err := f.catalog.GetDedicatedGraph(ctx, f.graphID)
			mu.Lock()
			defer mu.Unlock()
			asks++
			if err != nil || !found {
				censusErr = err
				return
			}
			consumers, censusErr = (&CheckoutLifecycle{catalog: f.catalog}).
				dedicatedBaseConsumers(ctx, graph)
			views, listErr := f.catalog.ListRefViews(ctx, f.graphID)
			if listErr != nil && censusErr == nil {
				censusErr = listErr
			}
			for i := range views {
				rows = append(rows, views[i].RefViewID)
			}
		}
	})

	_, err := manager.EnsureRefView(ctx, f.request("refs/heads/feature"))
	require.NoError(t, err)

	mu.Lock()
	defer mu.Unlock()
	require.NoError(t, censusErr)
	require.Equal(t, 1, asks, "the selection did not ask for the committed base exactly once")
	require.True(t, consumers,
		"the publication this demand triggers would be declined as having no consumer; rows=%v", rows)
	require.Contains(t, rows, f.viewID("refs/heads/feature"),
		"the demanding view's own row was not in the catalog when it asked")
}

// TestARefViewSelectorThatResolvesToNothingAsksForNoBase keeps the daemon's
// single largest write off a selection that names a ref which does not exist.
//
// The view's row is created before the selector is resolved (it is where a
// failure is recorded), and that row alone makes the family a census consumer
// from then on — so the base is still OWED and the next publication attempt
// will publish it. What must not happen is paying for it immediately on a
// selection that cannot be served at all.
func TestARefViewSelectorThatResolvesToNothingAsksForNoBase(t *testing.T) {
	f := newRefViewFixture(t)
	manager, demands := demandingManager(t, f)
	ctx := context.Background()

	result, err := manager.EnsureRefView(ctx, f.request("refs/heads/no-such-branch"))
	require.Error(t, err)
	require.Equal(t, store_sqlite.RefViewFailed, result.State)
	require.Empty(t, demands.asked(),
		"a selection that resolved to nothing ordered a committed-base index")

	// The debt survives the failure: the row is there, so the census counts a
	// consumer whenever the next attempt reads it.
	views, err := f.catalog.ListRefViews(ctx, f.graphID)
	require.NoError(t, err)
	require.Len(t, views, 1)
	require.Equal(t, f.viewID("refs/heads/no-such-branch"), views[0].RefViewID)
}

// TestTheBaseDemandThrottleForgetsStampsItCanNoLongerRefuse states the bound on
// the throttle map. It is written on a client-driven path (one entry per
// repository prefix a manager is handed), and an entry older than the interval
// refuses nothing, so it is dropped rather than kept forever.
func TestTheBaseDemandThrottleForgetsStampsItCanNoLongerRefuse(t *testing.T) {
	f := newRefViewFixture(t)
	manager, demands := demandingManager(t, f)

	manager.baseDemandMu.Lock()
	manager.lastBaseDemand = map[string]time.Time{
		"stale-repo:": time.Now().Add(-2 * dedicatedBaseDemandInterval),
		"fresh-repo:": time.Now(),
	}
	manager.baseDemandMu.Unlock()

	manager.demandCommittedBase(builderRepoPrefix)

	manager.baseDemandMu.Lock()
	defer manager.baseDemandMu.Unlock()
	require.Equal(t, []string{builderRepoPrefix}, demands.asked())
	require.Contains(t, manager.lastBaseDemand, builderRepoPrefix)
	require.Contains(t, manager.lastBaseDemand, "fresh-repo:",
		"a stamp that still refuses asks was dropped")
	require.NotContains(t, manager.lastBaseDemand, "stale-repo:",
		"a stamp the throttle can no longer act on was kept")
}
