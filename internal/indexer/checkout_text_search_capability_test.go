package indexer

import (
	"context"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/indexer/source"
)

// The declared text-search capability, read back off generations the production
// builders published, and the route gate that says whether an answer off the
// checkout root describes the view a caller is reading. The unit table in
// builder_generation_test.go pins the classification; these tests pin that the
// production paths reach it.

// textSearchProducerRow reads what one published generation declared for
// literal and regex search, and whether it declared anything.
func textSearchProducerRow(
	t *testing.T, store *store_sqlite.Store, generationID int64,
) (store_sqlite.ProducerCompleteness, bool) {
	t.Helper()
	rows, err := store.AtGeneration(generationID).ProducerStates()
	if err != nil {
		t.Fatalf("read producer states of generation %d: %v", generationID, err)
	}
	for _, row := range rows {
		if row.Producer == string(graphview.CapSearchText) {
			return row, true
		}
	}
	return store_sqlite.ProducerCompleteness{}, false
}

// TestRoutedLayersDeclareTextSearchTruthfully drives the real coordinator cycle
// and reads the two routed generations back. The working-tree layer IS what is
// on disk, so it claims the capability; the commit layer under it answers for no
// working copy of its own and declares nothing, because anything it declared
// would be worst-cased over the whole stack and would narrow the live view.
func TestRoutedLayersDeclareTextSearchTruthfully(t *testing.T) {
	f := newCoordinatorFixture(t)
	f.commitTreeB()
	worktreeWrite(t, f.worktree, "island.go",
		"package fixture\n\nfunc Island() {\n\t// uncommitted-capability-marker\n}\n")

	c := f.inertCoordinator(t, CheckoutCoordinatorConfig{})
	cycle := coordinatorReconcile(t, c)
	if cycle.CommitGenerationID == 0 || cycle.DirtyGenerationID == 0 {
		t.Fatalf("the cycle did not route both layers: %+v", cycle)
	}
	route := f.route()
	if route.CommitGenerationID != cycle.CommitGenerationID ||
		route.DirtyGenerationID != cycle.DirtyGenerationID {
		t.Fatalf("the route names %+v, the cycle published %+v", route, cycle)
	}

	if row, declared := textSearchProducerRow(t, f.store, cycle.CommitGenerationID); declared {
		t.Errorf("the routed commit layer declared %+v for %s; a layer beneath the "+
			"working-tree layer must declare nothing", row, graphview.CapSearchText)
	}

	dirty, declared := textSearchProducerRow(t, f.store, cycle.DirtyGenerationID)
	if !declared {
		t.Fatalf("the routed working-tree layer declared nothing for %s", graphview.CapSearchText)
	}
	if dirty.State != store_sqlite.ProducerStateComplete || dirty.Reason != "" {
		t.Errorf("the routed working-tree layer declared %+v, want complete with no reason", dirty)
	}
}

// TestRoutedStackNeverNarrowsTextSearch is the in-package pin for the composed
// view a reader actually gets.
//
// graphview worst-cases the producer states of every generation in a checkout's
// stack — the routed commit and working-tree layers plus the whole
// BaseGenerationID ancestry beneath them — so one narrowed layer refuses text
// search for the live checkout even though the checkout can answer it exactly.
// This walks that same stack and requires that nothing in it is worse than
// complete.
func TestRoutedStackNeverNarrowsTextSearch(t *testing.T) {
	f := newCoordinatorFixture(t)
	worktreeWrite(t, f.worktree, "island.go",
		"package fixture\n\nfunc Island() {\n\t// stack-capability-marker\n}\n")

	c := f.inertCoordinator(t, CheckoutCoordinatorConfig{})
	cycle := coordinatorReconcile(t, c)
	if cycle.CommitGenerationID == 0 || cycle.DirtyGenerationID == 0 {
		t.Fatalf("the cycle did not route both layers: %+v", cycle)
	}

	// Walk the stack the way graphview assembles it: the routed generations
	// plus every generation reachable through BaseGenerationID.
	seen := map[int64]struct{}{}
	stack := []int64{}
	for _, routed := range []int64{cycle.CommitGenerationID, cycle.DirtyGenerationID} {
		for id := routed; id > 0; {
			if _, duplicate := seen[id]; duplicate {
				break
			}
			seen[id] = struct{}{}
			stack = append(stack, id)
			row, found := f.generation(id)
			if !found {
				t.Fatalf("generation %d is in the route but not in the catalog", id)
			}
			id = row.BaseGenerationID
		}
	}
	if len(stack) < 2 {
		t.Fatalf("the walked stack is %v, want at least the two routed layers", stack)
	}

	claimed := false
	for _, id := range stack {
		row, declared := textSearchProducerRow(t, f.store, id)
		if !declared {
			continue
		}
		if row.State != store_sqlite.ProducerStateComplete {
			t.Errorf("generation %d in the routed stack declares %s = %q (%s); the worst "+
				"state in the stack is what the view reports, so this refuses a search "+
				"the checkout answers exactly",
				id, graphview.CapSearchText, row.State, row.Reason)
		}
		claimed = true
	}
	if !claimed {
		t.Errorf("no generation in the routed stack %v claims %s at all",
			stack, graphview.CapSearchText)
	}
}

// TestDedicatedBaseDeclaresNoTextSearchClaim covers the identity the routed
// layers sit on. It is built from a git tree source, never from a working copy,
// so it is not the layer that answers a text search — and it sits beneath every
// routed layer of the graph, so a narrowing there would refuse every one of
// their views.
func TestDedicatedBaseDeclaresNoTextSearchClaim(t *testing.T) {
	f := newCoordinatorFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	tree := builderGit(t, f.primary, "rev-parse", "HEAD^{tree}")
	target, err := source.NewGitTreeSource(ctx, f.primary, tree)
	if err != nil {
		t.Fatalf("open the tree source: %v", err)
	}
	defer target.Close() //nolint:errcheck // read-only source

	plan, _, err := planDedicatedSnapshot(ctx, target)
	if err != nil {
		t.Fatalf("plan the dedicated snapshot: %v", err)
	}
	changes := make([]LayerPathChange, 0, len(plan.indexed))
	for _, path := range plan.indexed {
		changes = append(changes, LayerPathChange{Path: path, Kind: LayerPathAdded})
	}

	generationID, report, err := builderNewBuilder(f.store).Build(ctx, BuildRequest{
		Identity: GenerationIdentity{
			// The literal owner kind builder_dedicated_claimed.go mints, not the
			// coordinator's alias for the same string; see
			// TestBuilderIdentityLiteralsMatchTheBuilders.
			OwnerKind: dedicatedBaseOwnerKind, GraphID: f.graphID, CheckoutID: f.primaryID,
			GenerationKind: dedicatedBaseGenerationKind, TreeOID: tree,
			ConfigHash: "capability-config", ExtractorVersions: "capability-extractors",
			ResolverVersion: "capability-resolver",
		},
		Base: graph.New(), Target: target, Changes: changes, RootPath: f.primary,
		RepoPrefix: builderRepoPrefix, WorkspaceID: builderRepoPrefix, ProjectID: builderRepoPrefix,
	})
	if err != nil || generationID <= 0 || report.NodeCount == 0 {
		t.Fatalf("build the dedicated base: id=%d report=%+v err=%v", generationID, report, err)
	}

	if row, declared := textSearchProducerRow(t, f.store, generationID); declared {
		t.Fatalf("the dedicated base declared %+v for %s, want no claim at all",
			row, graphview.CapSearchText)
	}

	// The build's own report must agree with what the generation holds: a
	// reader asking the store and a caller reading the report cannot be told
	// two different things about the same capability.
	for _, candidate := range report.Producers {
		if candidate.Producer == string(graphview.CapSearchText) {
			t.Fatalf("the report declares %+v while the generation declares nothing", candidate)
		}
	}
}

// TestGrepAnswersARouteThatNamesAWorkingTreeLayer is the positive half of the
// route gate, and the pin that the claim change did not cost the grep path: a
// fully routed checkout is still answered out of its own working copy,
// uncommitted edits included.
func TestGrepAnswersARouteThatNamesAWorkingTreeLayer(t *testing.T) {
	f := newCoordinatorFixture(t)
	worktreeWrite(t, f.worktree, "island.go",
		"package fixture\n\nfunc Island() {\n\t// grep-path-marker\n}\n")

	c := f.inertCoordinator(t, CheckoutCoordinatorConfig{})
	cycle := coordinatorReconcile(t, c)
	if cycle.DirtyGenerationID == 0 {
		t.Fatalf("the cycle routed no working-tree layer: %+v", cycle)
	}

	l := &CheckoutLifecycle{coordinators: map[string]*CheckoutCoordinator{f.checkoutID: c}}
	matches, served, err := l.GrepCheckout(context.Background(), CheckoutTextQuery{
		CheckoutID: f.checkoutID, Query: "grep-path-marker", Limit: 10,
	})
	if err != nil || !served || len(matches) != 1 || matches[0].Path != "island.go" {
		t.Fatalf("the grep path stopped answering: served=%v err=%v matches=%v",
			served, err, grepPaths(matches))
	}
}

// TestGrepRefusesARouteWithNoWorkingTreeLayer is the negative half, driven
// through the coordinator's own withdrawal path rather than by hand-writing a
// route row.
//
// While the working-tree slot is empty the checkout's view is its committed tree
// alone, and the root holds an edit that tree does not contain. Answering would
// hand the caller a line their view does not have, so nothing serves it — and
// the same query answers again as soon as a cycle re-routes the slot, which is
// what makes this a gate rather than an outage.
func TestGrepRefusesARouteWithNoWorkingTreeLayer(t *testing.T) {
	f := newCoordinatorFixture(t)
	worktreeWrite(t, f.worktree, "island.go",
		"package fixture\n\nfunc Island() {\n\t// withdrawn-slot-marker\n}\n")

	ctx := context.Background()
	c := f.inertCoordinator(t, CheckoutCoordinatorConfig{})
	if cycle := coordinatorReconcile(t, c); cycle.DirtyGenerationID == 0 {
		t.Fatalf("the cycle routed no working-tree layer: %+v", cycle)
	}
	l := &CheckoutLifecycle{coordinators: map[string]*CheckoutCoordinator{f.checkoutID: c}}

	query := CheckoutTextQuery{CheckoutID: f.checkoutID, Query: "withdrawn-slot-marker", Limit: 10}
	if matches, served, err := l.GrepCheckout(ctx, query); err != nil || !served || len(matches) != 1 {
		t.Fatalf("the routed checkout did not answer first: served=%v err=%v matches=%v",
			served, err, grepPaths(matches))
	}

	route := f.route()
	if err := c.clearDirtySlot(ctx, &route); err != nil {
		t.Fatalf("withdraw the working-tree slot: %v", err)
	}
	if withdrawn := f.route(); withdrawn.DirtyGenerationID != 0 {
		t.Fatalf("the working-tree slot is still routed: %+v", withdrawn)
	}

	matches, served, err := l.GrepCheckout(ctx, query)
	if err != nil {
		t.Fatalf("the gate errored instead of refusing: %v", err)
	}
	if served || len(matches) != 0 {
		t.Fatalf("a checkout routed to a committed tree answered from its working copy: "+
			"served=%v matches=%v", served, grepPaths(matches))
	}

	// Re-routing the slot restores the answer: the refusal tracks the route,
	// not the coordinator.
	if cycle := coordinatorReconcile(t, c); cycle.DirtyGenerationID == 0 {
		t.Fatalf("the second cycle routed no working-tree layer: %+v", cycle)
	}
	if matches, served, err := l.GrepCheckout(ctx, query); err != nil || !served || len(matches) != 1 {
		t.Fatalf("the re-routed checkout did not answer again: served=%v err=%v matches=%v",
			served, err, grepPaths(matches))
	}
}

// TestGrepRefusesAnUnroutedCheckout pins the gate's third arm, which was a
// fail-OPEN default with no test behind it: a checkout the catalog holds no
// route for used to be answered straight off its working copy.
//
// Nothing has published a view of such a checkout, so no layer vouches for what
// is on the root, and the corpus a searcher would be built over is the base
// inventory alone — the state of some other tree. The only production caller
// reaches GrepCheckout through a materialized view, which cannot exist without
// a route, so closing the arm costs no live answer; what it buys is that a
// future caller arriving without one is refused rather than handed bytes no
// view describes.
func TestGrepRefusesAnUnroutedCheckout(t *testing.T) {
	f := newCoordinatorFixture(t)
	worktreeWrite(t, f.worktree, "island.go",
		"package fixture\n\nfunc Island() {\n\t// unrouted-marker\n}\n")

	ctx := context.Background()
	// No cycle is run, so the checkout has no route row at all.
	c := f.inertCoordinator(t, CheckoutCoordinatorConfig{})
	if _, found, err := f.catalog.GetCheckoutRoute(ctx, f.checkoutID); err != nil || found {
		t.Fatalf("the fixture checkout is already routed: found=%v err=%v", found, err)
	}
	if describes, err := c.routeDescribesTheWorkingCopy(ctx); err != nil || describes {
		t.Fatalf("an unrouted checkout claimed its working copy is described: %v %v", describes, err)
	}

	l := &CheckoutLifecycle{coordinators: map[string]*CheckoutCoordinator{f.checkoutID: c}}
	matches, served, err := l.GrepCheckout(ctx, CheckoutTextQuery{
		CheckoutID: f.checkoutID, Query: "unrouted-marker", Limit: 10,
	})
	if err != nil {
		t.Fatalf("the gate errored instead of refusing: %v", err)
	}
	if served || len(matches) != 0 {
		t.Fatalf("an unrouted checkout answered off its working copy: served=%v matches=%v",
			served, grepPaths(matches))
	}

	// The control: the marker really is on the root, so the refusal above is a
	// refusal to answer and not an empty tree. One cycle routes the checkout
	// and the same query answers.
	if cycle := coordinatorReconcile(t, c); cycle.DirtyGenerationID == 0 {
		t.Fatalf("the cycle routed no working-tree layer: %+v", cycle)
	}
	matches, served, err = l.GrepCheckout(ctx, CheckoutTextQuery{
		CheckoutID: f.checkoutID, Query: "unrouted-marker", Limit: 10,
	})
	if err != nil || !served || len(matches) != 1 || matches[0].Path != "island.go" {
		t.Fatalf("the routed checkout did not answer: served=%v err=%v matches=%v",
			served, err, grepPaths(matches))
	}
}
