package graphview

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// Working-tree generation chains.
//
// A dirty generation may stand on the previously published dirty generation of
// the same checkout instead of directly on the routed commit generation. The
// route still names exactly [commit, top]; the materializer walks the top's
// BaseGenerationID down to the commit, composes every chain parent into the
// base reader, and keeps only the top as the working-tree layer.

// dirtyChainSpec describes one working-tree generation written over a parent.
type dirtyChainSpec struct {
	checkoutID string
	layerID    string
	configHash string
	nodes      []*graph.Node
	edges      []*graph.Edge
	metas      []graph.FileMetaRow
	masks      []store_sqlite.FileMask
}

func writeDirtyChainGeneration(t testing.TB, store *store_sqlite.Store, base int64, spec dirtyChainSpec) int64 {
	t.Helper()
	ctx := context.Background()
	if spec.checkoutID == "" {
		spec.checkoutID = testCheckoutID
	}
	if spec.layerID == "" {
		spec.layerID = stackDirtyLayerID
	}
	generationID, handle, err := store.BeginPayloadGeneration(ctx, store_sqlite.PayloadGenerationRequest{
		OwnerKind:        "dedicated_graph",
		GraphID:          testGraphID,
		LayerID:          spec.layerID,
		CheckoutID:       spec.checkoutID,
		GenerationKind:   dirtyGenerationKind,
		BaseGenerationID: base,
		TreeOID:          "tree-dirty",
		ConfigHash:       spec.configHash,
		CreatedAt:        5000,
	})
	if err != nil {
		t.Fatalf("BeginPayloadGeneration(dirty over %d): %v", base, err)
	}
	if len(spec.nodes) > 0 || len(spec.edges) > 0 {
		handle.AddBatch(spec.nodes, spec.edges)
	}
	if len(spec.metas) > 0 {
		if err := handle.SetFileMetas(stackRepo, spec.metas); err != nil {
			t.Fatalf("SetFileMetas(dirty over %d): %v", base, err)
		}
	}
	if len(spec.masks) > 0 {
		if err := handle.SetFileMasks(spec.masks); err != nil {
			t.Fatalf("SetFileMasks(dirty over %d): %v", base, err)
		}
	}
	if err := store.PublishPayloadGeneration(ctx, generationID, 6000); err != nil {
		t.Fatalf("PublishPayloadGeneration(dirty over %d): %v", base, err)
	}
	return generationID
}

// editChildSpec re-derives edit.go once more: New moves to line 40 and its
// call to Keeper to line 43. The parent's keep.go rewrite and added.go delete
// must still show through, since this child claims neither path.
func editChildSpec() dirtyChainSpec {
	return dirtyChainSpec{
		nodes: []*graph.Node{
			stackFileNode(stackEditFile, 45),
			stackSymbol(stackNewID, "New", graph.KindFunction, stackEditFile, 40),
		},
		edges: []*graph.Edge{
			stackEdge(stackEditFile, stackNewID, graph.EdgeContains, stackEditFile, 40),
			stackEdge(stackNewID, stackKeeperID, graph.EdgeCalls, stackEditFile, 43),
		},
		metas: []graph.FileMetaRow{stackFileMeta(stackEditFile, 2)},
		masks: []store_sqlite.FileMask{{RepoPrefix: stackRepo, FilePath: stackEditFile, Mode: store_sqlite.OwnershipReplace}},
	}
}

// seedDirtyChainFlatCorpus indexes the tree the commit, the first dirty
// generation and the edit child describe, as one plain corpus written by hand.
func seedDirtyChainFlatCorpus(t *testing.T, store *store_sqlite.Store) {
	t.Helper()
	store.AddBatch([]*graph.Node{
		stackFileNode(stackKeepFile, 12),
		stackFileNode(stackDepFile, 20),
		stackFileNode(stackEditFile, 45),
		stackSymbol(stackKeeperID, "Keeper", graph.KindFunction, stackKeepFile, 2),
		stackSymbol(stackCallerID, "Caller", graph.KindFunction, stackDepFile, 5),
		stackSymbol(stackNewID, "New", graph.KindFunction, stackEditFile, 40),
	}, []*graph.Edge{
		stackEdge(stackKeepFile, stackKeeperID, graph.EdgeContains, stackKeepFile, 2),
		stackEdge(stackDepFile, stackCallerID, graph.EdgeContains, stackDepFile, 5),
		stackEdge(stackEditFile, stackNewID, graph.EdgeContains, stackEditFile, 40),
		stackEdge(stackKeeperID, stackNewID, graph.EdgeCalls, stackKeepFile, 5),
		stackEdge(stackCallerID, stackNewID, graph.EdgeCalls, stackDepFile, 7),
		stackEdge(stackNewID, stackKeeperID, graph.EdgeCalls, stackEditFile, 43),
	})
	if err := store.SetFileMetas(stackRepo, []graph.FileMetaRow{
		stackFileMeta(stackKeepFile, 2),
		stackFileMeta(stackDepFile, 2),
		stackFileMeta(stackEditFile, 2),
	}); err != nil {
		t.Fatalf("SetFileMetas flat: %v", err)
	}
}

// seedDirtyChainStack writes corpus, commit C (legacy regime unless base > 0),
// D1 over C and D2 over D1, and routes the checkout to [C, D2].
func seedDirtyChainStack(t *testing.T, store *store_sqlite.Store, commitBase ...int64) (commit, d1, d2 int64) {
	t.Helper()
	commit = writeStackCommitGeneration(t, store, commitBase...)
	d1 = writeStackDirtyGeneration(t, store, commit)
	d2 = writeDirtyChainGeneration(t, store, d1, editChildSpec())
	routeStack(t, store, commit, d2, store_sqlite.RouteActive)
	return commit, d1, d2
}

func manualChainReader(t *testing.T, store *store_sqlite.Store, bottom graph.Reader, generations ...int64) graph.Reader {
	t.Helper()
	reader := bottom
	for _, generationID := range generations {
		layer, err := NewGenerationLayerContext(context.Background(), store.AtGeneration(generationID))
		if err != nil {
			t.Fatalf("NewGenerationLayerContext(%d): %v", generationID, err)
		}
		reader = graph.NewOverlaidViewWithLayer(reader, layer)
	}
	return reader
}

func assertNotLeased(t *testing.T, materializer *Materializer, generations ...int64) {
	t.Helper()
	for _, generationID := range generations {
		if materializer.Leases.InUse(generationID) {
			t.Fatalf("generation %d is still leased after a refused materialization", generationID)
		}
	}
}

// TestMaterializeCheckoutRefusesForeignDirtyChain: a chain parent from another
// checkout, built under another configuration, or a chain that ends at a
// commit generation other than the routed one, is refused as view_building
// and leaves nothing leased.
func TestMaterializeCheckoutRefusesForeignDirtyChain(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name   string
		parent func(t *testing.T, store *store_sqlite.Store, commit int64) int64
	}{
		{name: "other_checkout", parent: func(t *testing.T, store *store_sqlite.Store, commit int64) int64 {
			if err := store.Catalog().UpsertCheckout(ctx, store_sqlite.Checkout{
				CheckoutID: "wt-2", Incarnation: "inc-2", FamilyID: testFamilyID,
				RootPath: "/tmp/wt-2", GitDir: "/tmp/wt-2/.git", AdminName: "wt-2",
				State: store_sqlite.CheckoutStateReady, DesiredMode: store_sqlite.CheckoutModeDedicated,
				EffectiveMode: store_sqlite.CheckoutModeDedicated, HeadRef: "refs/heads/other",
				HeadCommit: "c0ffee", HeadTree: "7ee7", LastSeen: 101,
			}); err != nil {
				t.Fatalf("UpsertCheckout(wt-2): %v", err)
			}
			return writeDirtyChainGeneration(t, store, commit, dirtyChainSpec{checkoutID: "wt-2"})
		}},
		{name: "other_layer", parent: func(t *testing.T, store *store_sqlite.Store, commit int64) int64 {
			return writeDirtyChainGeneration(t, store, commit, dirtyChainSpec{layerID: "layer-dirty-other"})
		}},
		{name: "other_config", parent: func(t *testing.T, store *store_sqlite.Store, commit int64) int64 {
			return writeDirtyChainGeneration(t, store, commit, dirtyChainSpec{configHash: "config-other"})
		}},
		{name: "other_commit", parent: func(t *testing.T, store *store_sqlite.Store, _ int64) int64 {
			other := writeBenchmarkGeneration(t, store, "commit", stackCommitLayerID, 0)
			return writeDirtyChainGeneration(t, store, other, dirtyChainSpec{})
		}},
		{name: "commit_as_parent", parent: func(t *testing.T, store *store_sqlite.Store, _ int64) int64 {
			return writeBenchmarkGeneration(t, store, "commit", stackCommitLayerID, 0)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := openStackStore(t, "foreign-chain-"+tc.name)
			seedStackCorpus(t, store)
			seedStackControlPlane(t, store)
			commit := writeStackCommitGeneration(t, store)
			parent := tc.parent(t, store, commit)
			top := writeDirtyChainGeneration(t, store, parent, editChildSpec())
			routeStack(t, store, commit, top, store_sqlite.RouteActive)

			materializer := newTestMaterializer(store)
			view, err := materializer.MaterializeCheckout(ctx, testCheckoutID)
			if err == nil {
				view.Close()
				t.Fatalf("a chain through %s parent %d composed, want a refusal", tc.name, parent)
			}
			if CodeOf(err) != CodeViewBuilding {
				t.Fatalf("refusal code = %q (%v), want %q", CodeOf(err), err, CodeViewBuilding)
			}
			assertNotLeased(t, materializer, BaseCorpusGeneration, commit, parent, top)
		})
	}
}

// writeEmptyDirtyChain publishes count empty dirty generations, each over the
// previous one, starting over base. It returns them bottom first.
func writeEmptyDirtyChain(t *testing.T, store *store_sqlite.Store, base int64, count int) []int64 {
	t.Helper()
	chain := make([]int64, 0, count)
	parent := base
	for range count {
		parent = writeBenchmarkGeneration(t, store, dirtyGenerationKind, stackDirtyLayerID, parent)
		chain = append(chain, parent)
	}
	return chain
}

// TestMaterializeCheckoutRefusesDirtyChainPastBound pins both halves of the
// working-tree bound: exactly MaxDirtyChainDepth dirty generations over the
// commit compose, one more is refused with the labelled error rather than
// served short — through the checkout route and through a ref view alike.
func TestMaterializeCheckoutRefusesDirtyChainPastBound(t *testing.T) {
	ctx := context.Background()
	t.Run("at_bound", func(t *testing.T) {
		store := openStackStore(t, "dirty-chain-at-bound")
		seedStackControlPlane(t, store)
		commit := writeBenchmarkGeneration(t, store, "commit", stackCommitLayerID, 0)
		chain := writeEmptyDirtyChain(t, store, commit, MaxDirtyChainDepth)
		routeStack(t, store, commit, chain[len(chain)-1], store_sqlite.RouteActive)
		view, err := newTestMaterializer(store).MaterializeCheckout(ctx, testCheckoutID)
		if err != nil {
			t.Fatalf("a chain of exactly %d dirty generations: %v", MaxDirtyChainDepth, err)
		}
		defer view.Close()
		if got, want := view.Generations(), append([]int64{commit}, chain...); !slicesEqualInt64(got, want) {
			t.Fatalf("Generations() = %v, want %v", got, want)
		}
	})
	t.Run("past_bound", func(t *testing.T) {
		store := openStackStore(t, "dirty-chain-past-bound")
		seedStackControlPlane(t, store)
		commit := writeBenchmarkGeneration(t, store, "commit", stackCommitLayerID, 0)
		chain := writeEmptyDirtyChain(t, store, commit, MaxDirtyChainDepth+1)
		top := chain[len(chain)-1]
		routeStack(t, store, commit, top, store_sqlite.RouteActive)
		materializer := newTestMaterializer(store)

		for name, materialize := range map[string]func() (*RepoView, error){
			"checkout": func() (*RepoView, error) { return materializer.MaterializeCheckout(ctx, testCheckoutID) },
			"ref_view": func() (*RepoView, error) { return materializer.MaterializeRefView(ctx, testGraphID, top) },
		} {
			view, err := materialize()
			if err == nil {
				view.Close()
				t.Fatalf("%s: a chain of %d dirty generations composed, want a refusal", name, MaxDirtyChainDepth+1)
			}
			var tooDeep *AncestryTooDeepError
			if !errors.As(err, &tooDeep) {
				t.Fatalf("%s: refusal is not an *AncestryTooDeepError: %#v", name, err)
			}
			if !errors.Is(err, ErrViewBuilding) || CodeOf(err) != CodeViewBuilding {
				t.Fatalf("%s: refusal code = %q, want %q", name, CodeOf(err), CodeViewBuilding)
			}
			if tooDeep.Generation != top || tooDeep.Ancestor != chain[0] ||
				tooDeep.Depth != MaxDirtyChainDepth+1 || tooDeep.Limit != MaxDirtyChainDepth {
				t.Fatalf("%s: labels = generation %d ancestor %d depth %d limit %d; want %d %d %d %d", name,
					tooDeep.Generation, tooDeep.Ancestor, tooDeep.Depth, tooDeep.Limit,
					top, chain[0], MaxDirtyChainDepth+1, MaxDirtyChainDepth)
			}
			assertNotLeased(t, materializer, append([]int64{BaseCorpusGeneration, commit}, chain...)...)
		}
	})
}

// TestMaterializeRefViewOfDirtyParentComposesAncestry: the builder reads a
// dirty parent through MaterializeRefView, which must compose the parent's
// whole ancestry — commit side and working-tree chain — into one reader, and
// may walk the widened bound: a maximal dedicated chain, the commit over it,
// and a maximal dirty chain over that.
func TestMaterializeRefViewOfDirtyParentComposesAncestry(t *testing.T) {
	ctx := context.Background()
	t.Run("chain_parent", func(t *testing.T) {
		store := openStackStore(t, "ref-view-dirty-parent")
		seedStackCorpus(t, store)
		seedStackControlPlane(t, store)
		commit, d1, d2 := seedDirtyChainStack(t, store)
		materializer := newTestMaterializer(store)

		parent, err := materializer.MaterializeRefView(ctx, testGraphID, d1)
		if err != nil {
			t.Fatalf("MaterializeRefView(d1): %v", err)
		}
		defer parent.Close()
		if got := parent.Generations(); !slicesEqualInt64(got, []int64{commit, d1}) {
			t.Fatalf("parent Generations() = %v, want [%d %d]", got, commit, d1)
		}
		if parent.ID.BaseGeneration != d1 || len(parent.ID.Layers) != 0 {
			t.Fatalf("parent identity = %+v, want a one-generation ref view of %d", parent.ID, d1)
		}
		flatParent := openStackStore(t, "ref-view-dirty-parent-flat")
		seedStackFlatCorpus(t, flatParent)
		assertReadersAgree(t, parent.Reader, flatParent)

		top, err := materializer.MaterializeRefView(ctx, testGraphID, d2)
		if err != nil {
			t.Fatalf("MaterializeRefView(d2): %v", err)
		}
		defer top.Close()
		if got := top.Generations(); !slicesEqualInt64(got, []int64{commit, d1, d2}) {
			t.Fatalf("top Generations() = %v", got)
		}
		for _, generationID := range []int64{BaseCorpusGeneration, commit, d1, d2} {
			if !materializer.Leases.InUse(generationID) {
				t.Fatalf("generation %d is not leased by the ref view", generationID)
			}
		}
		flatTop := openStackStore(t, "ref-view-dirty-top-flat")
		seedDirtyChainFlatCorpus(t, flatTop)
		assertReadersAgree(t, top.Reader, flatTop)
	})
	t.Run("widened_bound", func(t *testing.T) {
		store := openStackStore(t, "ref-view-widened-bound")
		dedicated := writeDedicatedChain(t, store, MaxDedicatedBaseChainDepth)
		seedStackControlPlane(t, store, dedicated[0])
		commit := writeBenchmarkGeneration(t, store, "commit", stackCommitLayerID, dedicated[len(dedicated)-1])
		chain := writeEmptyDirtyChain(t, store, commit, MaxDirtyChainDepth)
		top := chain[len(chain)-1]

		view, err := newTestMaterializer(store).MaterializeRefView(ctx, testGraphID, top)
		if err != nil {
			t.Fatalf("MaterializeRefView over %d+%d generations: %v",
				MaxGenerationAncestryDepth, MaxDirtyChainDepth, err)
		}
		defer view.Close()
		want := append(append(slices.Clone(dedicated), commit), chain...)
		if got := view.Generations(); !slicesEqualInt64(got, want) {
			t.Fatalf("Generations() has %d entries, want %d", len(got), len(want))
		}
		if len(want) != MaxGenerationAncestryDepth+MaxDirtyChainDepth {
			t.Fatalf("fixture depth %d, want the widened bound %d", len(want), MaxGenerationAncestryDepth+MaxDirtyChainDepth)
		}
		if got := view.Reader.GetNode(chainSymbolID(0)); got == nil {
			t.Fatalf("the dedicated root's symbol %q did not survive the composition", chainSymbolID(0))
		}
		if view.PinsBaseCorpus() {
			t.Fatal("a ref view over a dedicated root leased generation zero")
		}
	})
}

// A cheap guard that the constant stays a small read-time bound and that the
// labelled refusal text names it.
func TestDirtyChainBoundIsDocumentedInTheRefusal(t *testing.T) {
	if MaxDirtyChainDepth < 2 || MaxDirtyChainDepth > MaxDedicatedBaseChainDepth {
		t.Fatalf("MaxDirtyChainDepth = %d, want a small bound in [2, %d]", MaxDirtyChainDepth, MaxDedicatedBaseChainDepth)
	}
	err := newDirtyChainTooDeep(10, 3, MaxDirtyChainDepth+1)
	if want := fmt.Sprintf("over the %d-generation chain bound", MaxDirtyChainDepth); !strings.Contains(err.Error(), want) {
		t.Fatalf("refusal %q does not name the bound (%q)", err.Error(), want)
	}
}
