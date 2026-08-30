package indexer

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/reconcile"
)

type primaryBaseTestFixture struct {
	tb      testing.TB
	catalog *store_sqlite.Catalog
	ctx     context.Context

	familyID string
	graphID  string
	ownerID  string
	treeOID  string

	graph      store_sqlite.DedicatedGraph
	generation int64
	sequence   int64
	graphs     []store_sqlite.DedicatedGraph
}

func newPrimaryBaseTestFixture(tb testing.TB, graphCount int) *primaryBaseTestFixture {
	tb.Helper()
	if graphCount < 1 {
		graphCount = 1
	}
	store, err := store_sqlite.Open(":memory:")
	if err != nil {
		tb.Fatalf("open store: %v", err)
	}
	tb.Cleanup(func() { _ = store.Close() })

	f := &primaryBaseTestFixture{
		tb:       tb,
		catalog:  store.Catalog(),
		ctx:      context.Background(),
		familyID: "family-primary-base",
		graphID:  "graph-primary-base",
		ownerID:  "checkout-primary-base",
		treeOID:  "tree-primary-base",
	}
	now := time.Now().Unix()
	if err := f.catalog.UpsertRepositoryFamily(f.ctx, store_sqlite.RepositoryFamily{
		FamilyID:          f.familyID,
		CommonDirIdentity: "/primary-base/common",
		State:             reconcile.FamilyStateReady,
		CreatedAt:         now,
		LastSeen:          now,
	}); err != nil {
		tb.Fatalf("upsert family: %v", err)
	}

	f.addCheckout(f.ownerID, "@main", f.treeOID)
	f.graph = store_sqlite.DedicatedGraph{
		GraphID:         f.graphID,
		OwnerCheckoutID: f.ownerID,
		RepoPrefix:      "primary-base",
		FamilyID:        f.familyID,
		IsPrimaryBase:   true,
		State:           reconcile.GraphStateReady,
	}
	f.upsertGraph(f.graph)
	f.generation = f.createGeneration(
		f.graphID, f.ownerID, store_sqlite.ViewGenerationReady, f.treeOID,
	)
	f.graph.ActiveGenerationID = f.generation
	f.upsertGraph(f.graph)
	f.graphs = append(f.graphs, f.graph)

	for i := 1; i < graphCount; i++ {
		ownerID := fmt.Sprintf("checkout-secondary-%03d", i)
		graphID := fmt.Sprintf("graph-secondary-%03d", i)
		f.addCheckout(ownerID, fmt.Sprintf("secondary-%03d", i), f.treeOID)
		graph := store_sqlite.DedicatedGraph{
			GraphID:         graphID,
			OwnerCheckoutID: ownerID,
			RepoPrefix:      fmt.Sprintf("secondary-%03d", i),
			FamilyID:        f.familyID,
			State:           reconcile.GraphStateReady,
		}
		f.upsertGraph(graph)
		f.graphs = append(f.graphs, graph)
	}
	return f
}

func (f *primaryBaseTestFixture) addCheckout(checkoutID, adminName, headTree string) {
	f.tb.Helper()
	now := time.Now().Unix()
	if err := f.catalog.AllocateCheckout(f.ctx, store_sqlite.Checkout{
		CheckoutID:     checkoutID,
		Incarnation:    "incarnation-" + checkoutID,
		FamilyID:       f.familyID,
		RootPath:       "/primary-base/" + checkoutID,
		GitDir:         "/primary-base/git/" + checkoutID,
		AdminName:      adminName,
		State:          store_sqlite.CheckoutStateReady,
		DesiredMode:    store_sqlite.CheckoutModeDedicated,
		EffectiveMode:  store_sqlite.CheckoutModeDedicated,
		HeadRef:        "refs/heads/main",
		HeadTree:       headTree,
		LastAccessible: now,
		LastSeen:       now,
	}); err != nil {
		f.tb.Fatalf("allocate checkout %s: %v", checkoutID, err)
	}
}

func (f *primaryBaseTestFixture) upsertGraph(graph store_sqlite.DedicatedGraph) {
	f.tb.Helper()
	if err := f.catalog.UpsertDedicatedGraph(f.ctx, graph); err != nil {
		f.tb.Fatalf("upsert graph %s: %v", graph.GraphID, err)
	}
	if graph.GraphID == f.graphID {
		f.graph = graph
	}
}

func (f *primaryBaseTestFixture) createGeneration(
	graphID, checkoutID string,
	state store_sqlite.ViewGenerationState,
	treeOID string,
) int64 {
	return f.createGenerationWith(graphID, checkoutID, state, treeOID, nil)
}

func (f *primaryBaseTestFixture) createGenerationWith(
	graphID, checkoutID string,
	state store_sqlite.ViewGenerationState,
	treeOID string,
	mutate func(*store_sqlite.ViewGeneration),
) int64 {
	f.tb.Helper()
	f.sequence++
	now := time.Now().Unix()
	generation := store_sqlite.ViewGeneration{
		OwnerKind:         dedicatedBaseGenerationKind,
		GraphID:           graphID,
		LayerID:           graphID + ":base",
		CheckoutID:        checkoutID,
		GenerationKind:    dedicatedBaseGenerationKind,
		TreeOID:           treeOID,
		ConfigHash:        "config-primary-base",
		ExtractorVersions: "extractors-primary-base",
		ResolverVersion:   checkoutResolverVersion,
		State:             state,
		CreatedAt:         now,
		PublishedAt:       now,
	}
	if mutate != nil {
		mutate(&generation)
	}
	generationID, err := f.catalog.CreateViewGeneration(f.ctx, generation)
	if err != nil {
		f.tb.Fatalf("create %s generation: %v", state, err)
	}
	return generationID
}

func TestGraphBaseRejectsStalePipelineIdentity(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*store_sqlite.ViewGeneration)
	}{
		{name: "config", mutate: func(row *store_sqlite.ViewGeneration) { row.ConfigHash = "old-config" }},
		{name: "extractors", mutate: func(row *store_sqlite.ViewGeneration) { row.ExtractorVersions = "old-extractors" }},
		{name: "resolver", mutate: func(row *store_sqlite.ViewGeneration) { row.ResolverVersion = "old-resolver" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newPrimaryBaseTestFixture(t, 1)
			generationID := f.createGenerationWith(
				f.graphID, f.ownerID, store_sqlite.ViewGenerationReady, f.treeOID, tt.mutate,
			)
			graph := f.graph
			graph.ActiveGenerationID = generationID
			f.upsertGraph(graph)

			_, err := f.coordinator(f.graphID).primaryBase(f.ctx)
			_ = requirePrimaryBaseUnavailable(t, err)
		})
	}
}

func TestGraphBaseRejectsRefreshingGraphBeforeSparseComposition(t *testing.T) {
	f := newPrimaryBaseTestFixture(t, 1)
	f.graph.State = store_sqlite.DedicatedGraphStateRefreshing
	f.upsertGraph(f.graph)
	_, err := graphBase(f.ctx, f.catalog, f.graph, f.desiredIdentity())
	_ = requirePrimaryBaseUnavailable(t, err)
}

func (f *primaryBaseTestFixture) coordinator(graphID string) *CheckoutCoordinator {
	return &CheckoutCoordinator{
		catalog: f.catalog, familyID: f.familyID, graphID: graphID,
		configHash: "config-primary-base", extractors: "extractors-primary-base",
	}
}

func (f *primaryBaseTestFixture) desiredIdentity() dedicatedBaseIdentity {
	return f.coordinator(f.graphID).dedicatedBaseIdentity()
}

func requirePrimaryBaseUnavailable(tb testing.TB, err error) *primaryBaseUnavailableError {
	tb.Helper()
	if err == nil {
		tb.Fatal("expected primary-base-unavailable error")
	}
	var unavailable *primaryBaseUnavailableError
	if !errors.As(err, &unavailable) {
		tb.Fatalf("error type = %T (%v), want *primaryBaseUnavailableError", err, err)
	}
	return unavailable
}

func TestPrimaryBaseUnavailableClassifiesStructuralLoss(t *testing.T) {
	f := newPrimaryBaseTestFixture(t, 1)

	missing := requirePrimaryBaseUnavailable(t,
		func() error {
			_, err := f.coordinator("graph-does-not-exist").primaryBase(f.ctx)
			return err
		}())
	if missing.Temporary() || !missing.Terminal() {
		t.Fatalf("missing graph classification = temporary:%v terminal:%v, want terminal",
			missing.Temporary(), missing.Terminal())
	}

	graph := f.graph
	graph.ActiveGenerationID = 0
	f.upsertGraph(graph)
	unpublished := requirePrimaryBaseUnavailable(t,
		func() error {
			_, err := f.coordinator(f.graphID).primaryBase(f.ctx)
			return err
		}())
	if !unpublished.Temporary() || unpublished.Terminal() {
		t.Fatalf("unpublished graph classification = temporary:%v terminal:%v, want temporary",
			unpublished.Temporary(), unpublished.Terminal())
	}
}

func BenchmarkPrimaryBaseUnavailableClassification(b *testing.B) {
	b.Run("missing_graph_terminal", func(b *testing.B) {
		f := newPrimaryBaseTestFixture(b, 1)
		coordinator := f.coordinator("graph-does-not-exist")
		b.ReportAllocs()
		b.ResetTimer()
		for b.Loop() {
			_, err := coordinator.primaryBase(f.ctx)
			var unavailable *primaryBaseUnavailableError
			if !errors.As(err, &unavailable) || !unavailable.Terminal() {
				b.Fatalf("classification = %v", err)
			}
		}
	})
	b.Run("zero_generation_temporary", func(b *testing.B) {
		f := newPrimaryBaseTestFixture(b, 1)
		graph := f.graph
		graph.ActiveGenerationID = 0
		f.upsertGraph(graph)
		coordinator := f.coordinator(f.graphID)
		b.ReportAllocs()
		b.ResetTimer()
		for b.Loop() {
			_, err := coordinator.primaryBase(f.ctx)
			var unavailable *primaryBaseUnavailableError
			if !errors.As(err, &unavailable) || !unavailable.Temporary() {
				b.Fatalf("classification = %v", err)
			}
		}
	})
}

func TestPrimaryBaseUsesDesignatedOwnerReadyGeneration(t *testing.T) {
	f := newPrimaryBaseTestFixture(t, 10)
	base, err := f.coordinator(f.graphID).primaryBase(f.ctx)
	if err != nil {
		t.Fatalf("primaryBase: %v", err)
	}
	if base.graphID != f.graphID || base.generationID != f.generation || base.treeOID != f.treeOID {
		t.Fatalf("primary base = %+v, want graph=%s generation=%d tree=%s",
			base, f.graphID, f.generation, f.treeOID)
	}
}

func TestPrimaryBaseRejectsAdversarialSameFamilyGraphBeforeBuild(t *testing.T) {
	f := newPrimaryBaseTestFixture(t, 2)
	coordinator := f.coordinator(f.graphs[1].GraphID)
	coordinator.builder = nil
	out := coordinator.reconcile(f.ctx)
	_ = requirePrimaryBaseUnavailable(t, out.Err)
}

func TestPrimaryBaseAcceptsTheDedicatedOwnersOwnGraph(t *testing.T) {
	f := newPrimaryBaseTestFixture(t, 2)
	graph := f.graphs[1]
	generationID := f.createGeneration(graph.GraphID, graph.OwnerCheckoutID,
		store_sqlite.ViewGenerationReady, "tree-dedicated-owner")
	graph.ActiveGenerationID = generationID
	f.upsertGraph(graph)

	coordinator := f.coordinator(graph.GraphID)
	coordinator.checkoutID = graph.OwnerCheckoutID
	base, err := coordinator.primaryBase(f.ctx)
	if err != nil {
		t.Fatalf("primaryBase for dedicated owner: %v", err)
	}
	if base.graphID != graph.GraphID || base.generationID != generationID || base.treeOID != "tree-dedicated-owner" {
		t.Fatalf("dedicated base = %+v, want graph=%s generation=%d tree=tree-dedicated-owner",
			base, graph.GraphID, generationID)
	}
}

func TestPrimaryBaseRequiresDesignatedGraph(t *testing.T) {
	f := newPrimaryBaseTestFixture(t, 1)
	for _, graphID := range []string{"", "graph-missing"} {
		t.Run(fmt.Sprintf("graph=%q", graphID), func(t *testing.T) {
			_, err := f.coordinator(graphID).primaryBase(f.ctx)
			_ = requirePrimaryBaseUnavailable(t, err)
		})
	}
}

func TestGraphBaseAcceptsCanonicalServableStates(t *testing.T) {
	for _, state := range []store_sqlite.ViewGenerationState{
		store_sqlite.ViewGenerationReady,
		store_sqlite.ViewGenerationSuperseded,
	} {
		t.Run(string(state), func(t *testing.T) {
			f := newPrimaryBaseTestFixture(t, 1)
			wantTree := "tree-" + string(state)
			generationID := f.createGeneration(f.graphID, f.ownerID, state, wantTree)
			graph := f.graph
			graph.ActiveGenerationID = generationID
			base, err := graphBase(f.ctx, f.catalog, graph, f.desiredIdentity())
			if err != nil {
				t.Fatalf("graphBase: %v", err)
			}
			if base.generationID != generationID || base.treeOID != wantTree {
				t.Fatalf("base = %+v, want generation=%d tree=%s", base, generationID, wantTree)
			}
		})
	}
}

func TestGraphBaseRejectsNonServableAndInvalidActiveGenerations(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*primaryBaseTestFixture) store_sqlite.DedicatedGraph
	}{
		{
			name: "zero active pointer despite owner head",
			setup: func(f *primaryBaseTestFixture) store_sqlite.DedicatedGraph {
				graph := f.graph
				graph.ActiveGenerationID = 0
				return graph
			},
		},
		{
			name: "dangling active pointer despite owner head",
			setup: func(f *primaryBaseTestFixture) store_sqlite.DedicatedGraph {
				graph := f.graph
				graph.ActiveGenerationID = 1 << 60
				return graph
			},
		},
		{
			name: "active row belongs to another graph",
			setup: func(f *primaryBaseTestFixture) store_sqlite.DedicatedGraph {
				other := f.graphs[1]
				generationID := f.createGeneration(other.GraphID, other.OwnerCheckoutID,
					store_sqlite.ViewGenerationReady, "tree-other")
				graph := f.graph
				graph.ActiveGenerationID = generationID
				return graph
			},
		},
		{
			name: "ready row has empty tree",
			setup: func(f *primaryBaseTestFixture) store_sqlite.DedicatedGraph {
				generationID := f.createGeneration(f.graphID, f.ownerID,
					store_sqlite.ViewGenerationReady, "")
				graph := f.graph
				graph.ActiveGenerationID = generationID
				return graph
			},
		},
	}
	for _, state := range []store_sqlite.ViewGenerationState{
		store_sqlite.ViewGenerationBuilding,
		store_sqlite.ViewGenerationRetiring,
		store_sqlite.ViewGenerationFailed,
	} {
		state := state
		tests = append(tests, struct {
			name  string
			setup func(*primaryBaseTestFixture) store_sqlite.DedicatedGraph
		}{
			name: "state " + string(state),
			setup: func(f *primaryBaseTestFixture) store_sqlite.DedicatedGraph {
				generationID := f.createGeneration(f.graphID, f.ownerID, state, "tree-"+string(state))
				graph := f.graph
				graph.ActiveGenerationID = generationID
				return graph
			},
		})
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			graphCount := 1
			if tt.name == "active row belongs to another graph" {
				graphCount = 2
			}
			f := newPrimaryBaseTestFixture(t, graphCount)
			owner, found, err := f.catalog.GetCheckout(f.ctx, f.ownerID)
			if err != nil || !found || owner.HeadTree == "" {
				t.Fatalf("owner should retain a mutable head: found=%v row=%+v err=%v", found, owner, err)
			}
			_, err = graphBase(f.ctx, f.catalog, tt.setup(f), f.desiredIdentity())
			_ = requirePrimaryBaseUnavailable(t, err)
		})
	}
}

func TestPrimaryBaseIgnoresOwnerHeadDrift(t *testing.T) {
	f := newPrimaryBaseTestFixture(t, 1)
	owner, found, err := f.catalog.GetCheckout(f.ctx, f.ownerID)
	if err != nil || !found {
		t.Fatalf("get owner: found=%v err=%v", found, err)
	}
	owner.HeadTree = "tree-owner-drifted"
	if err := f.catalog.UpsertCheckout(f.ctx, owner); err != nil {
		t.Fatalf("drift owner head: %v", err)
	}
	base, err := f.coordinator(f.graphID).primaryBase(f.ctx)
	if err != nil {
		t.Fatalf("primaryBase: %v", err)
	}
	if base.treeOID != f.treeOID || base.generationID != f.generation {
		t.Fatalf("base followed mutable owner head: %+v", base)
	}
}

func TestPrimaryBaseKeepsReadyActiveWhileNewGenerationBuilds(t *testing.T) {
	f := newPrimaryBaseTestFixture(t, 1)
	buildingID := f.createGeneration(f.graphID, f.ownerID,
		store_sqlite.ViewGenerationBuilding, "tree-building")
	base, err := f.coordinator(f.graphID).primaryBase(f.ctx)
	if err != nil {
		t.Fatalf("primaryBase with newer building generation %d: %v", buildingID, err)
	}
	if base.generationID != f.generation || base.treeOID != f.treeOID {
		t.Fatalf("base = %+v, want active ready generation %d tree %s",
			base, f.generation, f.treeOID)
	}
}

func TestDiffTreeChangesKeepsOneFileDeltaSparseInTwoThousandFileRepo(t *testing.T) {
	repo, baseTree, targetTree, changedPath := largeOneFileDelta(t)
	changes, err := diffTreeChanges(context.Background(), repo, baseTree, targetTree)
	if err != nil {
		t.Fatalf("diffTreeChanges: %v", err)
	}
	if len(changes) != 1 {
		t.Fatalf("changes = %d, want 1 from a 2,000-file corpus: %+v", len(changes), changes)
	}
	if changes[0].Path != changedPath || changes[0].Kind != LayerPathModified {
		t.Fatalf("change = %+v, want modified %s", changes[0], changedPath)
	}
}

func largeOneFileDelta(tb testing.TB) (repo, baseTree, targetTree, changedPath string) {
	tb.Helper()
	builderIsolateGit(tb)
	repo = builderTempDir(tb, "primary-base-diff")
	tb.Cleanup(func() {
		var lastErr error
		for attempt := 0; attempt < 10; attempt++ {
			if lastErr = os.RemoveAll(repo); lastErr == nil {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		tb.Errorf("remove Git fixture: %v", lastErr)
	})
	builderGit(tb, repo, "init", "--initial-branch=main")

	treeA := make(map[string]string, 2000)
	for i := 0; i < 2000; i++ {
		path := fmt.Sprintf("pkg/file_%04d.go", i)
		treeA[path] = fmt.Sprintf("package pkg\n\nconst Value%04d = %d\n", i, i)
	}
	builderWriteTree(tb, repo, treeA)
	builderGit(tb, repo, "add", "-A")
	builderGit(tb, repo, "commit", "-m", "base 2000 files")
	baseTree = builderGit(tb, repo, "rev-parse", "HEAD^{tree}")

	treeB := make(map[string]string, len(treeA))
	for path, body := range treeA {
		treeB[path] = body
	}
	changedPath = "pkg/file_1234.go"
	treeB[changedPath] = "package pkg\n\nconst Value1234 = 9999\n"
	builderWriteTree(tb, repo, treeB)
	builderGit(tb, repo, "add", "-A")
	builderGit(tb, repo, "commit", "-m", "change one file")
	targetTree = builderGit(tb, repo, "rev-parse", "HEAD^{tree}")
	return repo, baseTree, targetTree, changedPath
}

func BenchmarkDiffTreeChangesOneOfTwoThousand(b *testing.B) {
	repo, baseTree, targetTree, _ := largeOneFileDelta(b)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		changes, err := diffTreeChanges(context.Background(), repo, baseTree, targetTree)
		if err != nil {
			b.Fatal(err)
		}
		if len(changes) != 1 {
			b.Fatalf("changes = %d, want 1", len(changes))
		}
	}
	b.ReportMetric(1, "changes/op")
}

func BenchmarkPrimaryBaseDesignatedOwner(b *testing.B) {
	for _, graphCount := range []int{1, 10, 100} {
		b.Run(fmt.Sprintf("graphs=%d", graphCount), func(b *testing.B) {
			f := newPrimaryBaseTestFixture(b, graphCount)
			coordinator := f.coordinator(f.graphID)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := coordinator.primaryBase(f.ctx); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkGraphBaseReadyHit(b *testing.B) {
	f := newPrimaryBaseTestFixture(b, 1)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := graphBase(f.ctx, f.catalog, f.graph, f.desiredIdentity()); err != nil {
			b.Fatal(err)
		}
	}
}
