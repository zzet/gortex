package graphview

import (
	"context"
	"path"
	"path/filepath"
	"testing"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

func dedicatedRootFileNode(file string) *graph.Node {
	return &graph.Node{ID: file, Name: path.Base(file), Kind: graph.KindFile, FilePath: file, RepoPrefix: stackRepo, Language: "go"}
}

func dedicatedRootSymbol(file, name string, line int) *graph.Node {
	return stackSymbol(file+"::"+name, name, graph.KindFunction, file, line)
}

func writeDedicatedRootGeneration(t testing.TB, store *store_sqlite.Store, layer string, parent int64, nodes []*graph.Node, masks []store_sqlite.FileMask) int64 {
	t.Helper()
	return writeDedicatedRootRequest(t, store, store_sqlite.PayloadGenerationRequest{
		OwnerKind:        "dedicated_graph",
		GraphID:          testGraphID,
		LayerID:          layer,
		CheckoutID:       testCheckoutID,
		GenerationKind:   "dedicated",
		BaseGenerationID: parent,
		TreeOID:          "tree-" + layer,
		CreatedAt:        500,
	}, nodes, masks)
}

func writeDedicatedRootRequest(t testing.TB, store *store_sqlite.Store, req store_sqlite.PayloadGenerationRequest, nodes []*graph.Node, masks []store_sqlite.FileMask) int64 {
	t.Helper()
	ctx := context.Background()
	id, handle, err := store.BeginPayloadGeneration(ctx, req)
	if err != nil {
		t.Fatalf("BeginPayloadGeneration(%s): %v", req.LayerID, err)
	}
	handle.AddBatch(nodes, nil)
	if err := handle.SetFileMasks(masks); err != nil {
		t.Fatalf("SetFileMasks(%s): %v", req.LayerID, err)
	}
	if err := store.PublishPayloadGeneration(ctx, id, 750); err != nil {
		t.Fatalf("PublishPayloadGeneration(%s): %v", req.LayerID, err)
	}
	return id
}

func TestDedicatedRootAssemblyGuards(t *testing.T) {
	testDedicatedRootAssemblyGuards(t, false)
}

func TestInheritedDedicatedRootAssemblyGuards(t *testing.T) {
	testDedicatedRootAssemblyGuards(t, true)
}

func testDedicatedRootAssemblyGuards(t *testing.T, inherited bool) {
	t.Helper()
	for _, tc := range []struct {
		name          string
		change        func(*store_sqlite.PayloadGenerationRequest)
		omitBinding   bool
		requestedRepo string
	}{
		{name: "wrong_owner_kind", change: func(r *store_sqlite.PayloadGenerationRequest) { r.OwnerKind = "ref_view" }},
		{name: "wrong_graph_owner", change: func(r *store_sqlite.PayloadGenerationRequest) { r.GraphID = "graph-other" }},
		{name: "wrong_checkout_owner", change: func(r *store_sqlite.PayloadGenerationRequest) { r.CheckoutID = "checkout-other" }},
		{name: "empty_committed_tree", change: func(r *store_sqlite.PayloadGenerationRequest) { r.TreeOID = "" }},
		{name: "missing_graph_binding", omitBinding: true},
		{name: "wrong_repo_binding", requestedRepo: "other-repo"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := openStackStore(t, "private-root-guard-"+tc.name)
			file := stackRepo + "/committed.go"
			req := store_sqlite.PayloadGenerationRequest{
				OwnerKind: "dedicated_graph", GraphID: testGraphID, LayerID: "guard-" + tc.name,
				CheckoutID: testCheckoutID, GenerationKind: "dedicated", TreeOID: "committed-tree", CreatedAt: 500,
			}
			if tc.change != nil {
				tc.change(&req)
			}
			root := writeDedicatedRootRequest(t, store, req,
				[]*graph.Node{dedicatedRootFileNode(file), dedicatedRootSymbol(file, "Committed", 7)},
				[]store_sqlite.FileMask{{RepoPrefix: stackRepo, FilePath: file, Mode: store_sqlite.OwnershipReplace}})
			selected := root
			if inherited {
				// A valid committed child must not launder a malformed inherited
				// dedicated root. Request only the child so routedStart is positive.
				childFile := stackRepo + "/child.go"
				selected = writeDedicatedRootRequest(t, store, store_sqlite.PayloadGenerationRequest{
					OwnerKind: "dedicated_graph", GraphID: testGraphID, LayerID: "guard-child-" + tc.name,
					CheckoutID: testCheckoutID, GenerationKind: "commit", BaseGenerationID: root,
					TreeOID: "child-committed-tree", CreatedAt: 600,
				}, []*graph.Node{dedicatedRootFileNode(childFile), dedicatedRootSymbol(childFile, "Child", 9)},
					[]store_sqlite.FileMask{{RepoPrefix: stackRepo, FilePath: childFile, Mode: store_sqlite.OwnershipReplace}})
			}
			if !tc.omitBinding {
				seedStackControlPlane(t, store, selected)
			}
			prefix := stackRepo
			if tc.requestedRepo != "" {
				prefix = tc.requestedRepo
			}
			view, err := newTestMaterializer(store).assemble(context.Background(), testGraphID, prefix, []int64{selected}, nil)
			if view != nil {
				defer view.Close()
			}
			if err == nil {
				t.Fatalf("assembly accepted malformed dedicated root (inherited=%v): %s", inherited, tc.name)
			}
		})
	}
}

// These tests exercise the exact assembly seam directly. They do not establish
// that MCP kind:base routes here, nor validate external lease/GC acquisition.
func TestDedicatedGenerationAssembly(t *testing.T) {
	t.Run("sole_dedicated_root_excludes_mutable_generation_zero", func(t *testing.T) {
		store := openStackStore(t, "private-dedicated-root")
		dirtyFile := stackRepo + "/dirty_only.go"
		committedFile := stackRepo + "/committed.go"
		dirty := dedicatedRootSymbol(dirtyFile, "DirtyOnly", 99)
		committed := dedicatedRootSymbol(committedFile, "Committed", 7)
		store.AddBatch([]*graph.Node{dedicatedRootFileNode(dirtyFile), dirty}, nil)
		root := writeDedicatedRootGeneration(t, store, "sole-root", 0,
			[]*graph.Node{dedicatedRootFileNode(committedFile), committed},
			[]store_sqlite.FileMask{{RepoPrefix: stackRepo, FilePath: committedFile, Mode: store_sqlite.OwnershipReplace}})
		seedStackControlPlane(t, store, root)

		view, err := newTestMaterializer(store).assemble(context.Background(), testGraphID, stackRepo, []int64{root}, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer view.Close()
		if got := view.Reader.GetNode(committed.ID); got == nil || got.StartLine != 7 {
			t.Errorf("dedicated committed symbol missing or wrong: %+v", got)
		}
		if got := view.Reader.GetNode(dirty.ID); got != nil {
			t.Errorf("sole dedicated root inherited mutable generation-zero symbol: %+v", got)
		}
		if got := view.Reader.GetNode(dirtyFile); got != nil {
			t.Errorf("sole dedicated root inherited mutable generation-zero file: %+v", got)
		}
		if got := view.Generations(); !slicesEqualInt64(got, []int64{root}) {
			t.Errorf("generation identity=%v, want only root %d", got, root)
		}
	})

	t.Run("dedicated_ancestors_preserve_unchanged_and_apply_latest_masks", func(t *testing.T) {
		store := openStackStore(t, "private-dedicated-chain")
		keepFile := stackRepo + "/keep.go"
		editFile := stackRepo + "/edit.go"
		goneFile := stackRepo + "/gone.go"
		dirtyFile := stackRepo + "/dirty_only.go"
		keep := dedicatedRootSymbol(keepFile, "Keep", 5)
		old := dedicatedRootSymbol(editFile, "Old", 10)
		gone := dedicatedRootSymbol(goneFile, "Gone", 10)
		sharedOld := dedicatedRootSymbol(editFile, "Shared", 10)
		dirty := dedicatedRootSymbol(dirtyFile, "DirtyOnly", 99)
		store.AddBatch([]*graph.Node{dedicatedRootFileNode(dirtyFile), dirty}, nil)
		root := writeDedicatedRootGeneration(t, store, "chain-root", 0,
			[]*graph.Node{dedicatedRootFileNode(keepFile), keep, dedicatedRootFileNode(editFile), old, sharedOld, dedicatedRootFileNode(goneFile), gone},
			[]store_sqlite.FileMask{
				{RepoPrefix: stackRepo, FilePath: keepFile, Mode: store_sqlite.OwnershipReplace},
				{RepoPrefix: stackRepo, FilePath: editFile, Mode: store_sqlite.OwnershipReplace},
				{RepoPrefix: stackRepo, FilePath: goneFile, Mode: store_sqlite.OwnershipReplace},
			})
		middle := dedicatedRootSymbol(editFile, "Middle", 20)
		sharedMiddle := dedicatedRootSymbol(editFile, "Shared", 20)
		delta := writeDedicatedRootGeneration(t, store, "chain-middle", root,
			[]*graph.Node{dedicatedRootFileNode(editFile), middle, sharedMiddle},
			[]store_sqlite.FileMask{
				{RepoPrefix: stackRepo, FilePath: editFile, Mode: store_sqlite.OwnershipReplace},
				{RepoPrefix: stackRepo, FilePath: goneFile, Mode: store_sqlite.OwnershipDelete},
			})
		latest := dedicatedRootSymbol(editFile, "Latest", 30)
		sharedLatest := dedicatedRootSymbol(editFile, "Shared", 30)
		tip := writeDedicatedRootGeneration(t, store, "chain-tip", delta,
			[]*graph.Node{dedicatedRootFileNode(editFile), latest, sharedLatest},
			[]store_sqlite.FileMask{{RepoPrefix: stackRepo, FilePath: editFile, Mode: store_sqlite.OwnershipReplace}})
		seedStackControlPlane(t, store, tip)

		view, err := newTestMaterializer(store).assemble(context.Background(), testGraphID, stackRepo, []int64{tip}, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer view.Close()
		for _, want := range []*graph.Node{keep, latest, sharedLatest} {
			if got := view.Reader.GetNode(want.ID); got == nil || got.StartLine != want.StartLine {
				t.Errorf("ancestor/delta symbol %q=%+v, want line %d", want.ID, got, want.StartLine)
			}
		}
		for _, absent := range []string{old.ID, middle.ID, gone.ID, goneFile, dirty.ID, dirtyFile} {
			if got := view.Reader.GetNode(absent); got != nil {
				t.Errorf("masked/deleted/unrelated generation-zero node %q leaked: %+v", absent, got)
			}
		}
		if got := view.Generations(); !slicesEqualInt64(got, []int64{root, delta, tip}) {
			t.Errorf("generation ancestry=%v, want [%d %d %d]", got, root, delta, tip)
		}
		if got := view.GenerationSources(); len(got) != 3 {
			t.Errorf("generation sources=%d, want every ancestor (3)", len(got))
		}
	})

	t.Run("legacy_automatic_commit_still_inherits_generation_zero", func(t *testing.T) {
		store := openStackStore(t, "private-legacy-root")
		seedStackCorpus(t, store)
		commit := writeStackCommitGeneration(t, store)
		seedStackControlPlane(t, store)
		view, err := newTestMaterializer(store).assemble(context.Background(), testGraphID, stackRepo, []int64{commit}, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer view.Close()
		if got := view.Reader.GetNode(stackCallerID); got == nil || got.StartLine != 5 {
			t.Errorf("legacy commit lost unchanged generation-zero node: %+v", got)
		}
		if got := view.Reader.GetNode(stackStaleID); got != nil {
			t.Errorf("legacy commit tombstone stopped masking node: %+v", got)
		}
	})
}

func TestDedicatedFullRootValidation(t *testing.T) {
	validRow := store_sqlite.ViewGeneration{
		GenerationID: 1, BaseGenerationID: 0, OwnerKind: "dedicated_graph", GenerationKind: "dedicated",
		GraphID: "graph-1", CheckoutID: "checkout-1", TreeOID: "committed-tree", State: store_sqlite.ViewGenerationReady,
	}
	validBinding := store_sqlite.DedicatedGraph{GraphID: "graph-1", OwnerCheckoutID: "checkout-1", RepoPrefix: "repo"}
	for _, tc := range []struct {
		name   string
		change func(*store_sqlite.ViewGeneration, *store_sqlite.DedicatedGraph)
		valid  bool
	}{
		{name: "ready_full_root", valid: true},
		{name: "superseded_full_root", valid: true, change: func(r *store_sqlite.ViewGeneration, _ *store_sqlite.DedicatedGraph) {
			r.State = store_sqlite.ViewGenerationSuperseded
		}},
		{name: "generation_zero", change: func(r *store_sqlite.ViewGeneration, _ *store_sqlite.DedicatedGraph) { r.GenerationID = 0 }},
		{name: "negative_generation", change: func(r *store_sqlite.ViewGeneration, _ *store_sqlite.DedicatedGraph) { r.GenerationID = -1 }},
		{name: "positive_parent_not_full_root", change: func(r *store_sqlite.ViewGeneration, _ *store_sqlite.DedicatedGraph) { r.BaseGenerationID = 2 }},
		{name: "wrong_owner_kind", change: func(r *store_sqlite.ViewGeneration, _ *store_sqlite.DedicatedGraph) { r.OwnerKind = "ref_view" }},
		{name: "ordinary_commit_is_not_dedicated_root", change: func(r *store_sqlite.ViewGeneration, _ *store_sqlite.DedicatedGraph) { r.GenerationKind = "commit" }},
		{name: "wrong_generation_graph", change: func(r *store_sqlite.ViewGeneration, _ *store_sqlite.DedicatedGraph) { r.GraphID = "graph-other" }},
		{name: "wrong_binding_graph", change: func(_ *store_sqlite.ViewGeneration, g *store_sqlite.DedicatedGraph) { g.GraphID = "graph-other" }},
		{name: "wrong_checkout_owner", change: func(r *store_sqlite.ViewGeneration, _ *store_sqlite.DedicatedGraph) { r.CheckoutID = "checkout-other" }},
		{name: "empty_checkout_owner", change: func(r *store_sqlite.ViewGeneration, _ *store_sqlite.DedicatedGraph) { r.CheckoutID = "" }},
		{name: "wrong_prefix_binding", change: func(_ *store_sqlite.ViewGeneration, g *store_sqlite.DedicatedGraph) { g.RepoPrefix = "other-repo" }},
		{name: "empty_tree", change: func(r *store_sqlite.ViewGeneration, _ *store_sqlite.DedicatedGraph) { r.TreeOID = "" }},
		{name: "building_root", change: func(r *store_sqlite.ViewGeneration, _ *store_sqlite.DedicatedGraph) {
			r.State = store_sqlite.ViewGenerationBuilding
		}},
		{name: "failed_root", change: func(r *store_sqlite.ViewGeneration, _ *store_sqlite.DedicatedGraph) {
			r.State = store_sqlite.ViewGenerationFailed
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			row, binding := validRow, validBinding
			if tc.change != nil {
				tc.change(&row, &binding)
			}
			err := validateDedicatedFullRoot(row, binding, "graph-1", "repo")
			if (err == nil) != tc.valid {
				t.Fatalf("validation error=%v, want valid=%v", err, tc.valid)
			}
		})
	}
}

func TestDedicatedRootRefFailureReleasesLeases(t *testing.T) {
	for _, inherited := range []bool{false, true} {
		name := "standalone"
		if inherited {
			name = "inherited"
		}
		t.Run(name, func(t *testing.T) {
			store := openStackStore(t, "dedicated-ref-leases-"+name)
			file := stackRepo + "/committed.go"
			root := writeDedicatedRootGeneration(t, store, "lease-valid-root", 0,
				[]*graph.Node{dedicatedRootFileNode(file), dedicatedRootSymbol(file, "Committed", 7)}, nil)
			seedStackControlPlane(t, store, root)
			materializer := newTestMaterializer(store)
			pins := func() map[int64]int {
				materializer.Leases.mu.Lock()
				defer materializer.Leases.mu.Unlock()
				out := make(map[int64]int, len(materializer.Leases.counts))
				for generation, count := range materializer.Leases.counts {
					if count != 0 {
						out[generation] = count
					}
				}
				return out
			}
			valid, err := materializer.MaterializeRefView(t.Context(), testGraphID, root)
			if err != nil {
				t.Fatalf("valid public ref setup: %v", err)
			}
			if got := pins(); got[root] != 1 || len(got) != 1 {
				valid.Close()
				t.Fatalf("valid public ref did not pin its root: %v", got)
			}
			valid.Close()
			if got := pins(); len(got) != 0 {
				t.Fatalf("valid ref close left pins: %v", got)
			}
			badRoot := writeDedicatedRootRequest(t, store, store_sqlite.PayloadGenerationRequest{
				OwnerKind: "dedicated_graph", GraphID: testGraphID, LayerID: "lease-bad-root",
				CheckoutID: "wrong-checkout", GenerationKind: "dedicated", TreeOID: "committed-tree", CreatedAt: 500,
			}, []*graph.Node{dedicatedRootFileNode(file)}, nil)
			selected := badRoot
			if inherited {
				selected = writeDedicatedRootRequest(t, store, store_sqlite.PayloadGenerationRequest{
					OwnerKind: "dedicated_graph", GraphID: testGraphID, LayerID: "lease-valid-child",
					CheckoutID: testCheckoutID, GenerationKind: "commit", BaseGenerationID: badRoot,
					TreeOID: "child-tree", CreatedAt: 600,
				}, nil, nil)
			}
			view, err := materializer.MaterializeRefView(t.Context(), testGraphID, selected)
			if view != nil {
				view.Close()
			}
			if err == nil {
				t.Error("public ref accepted a dedicated root belonging to another checkout")
			}
			if got := pins(); len(got) != 0 {
				t.Fatalf("failed public ref left generation pins: %v", got)
			}
		})
	}
}

func BenchmarkDedicatedRootMaterialization(b *testing.B) {
	for _, kind := range []string{"legacy_commit", "standalone_dedicated", "inherited_dedicated"} {
		b.Run(kind, func(b *testing.B) {
			store, err := store_sqlite.Open(filepath.Join(b.TempDir(), "materialize.sqlite"))
			if err != nil {
				b.Fatal(err)
			}
			b.Cleanup(func() {
				if err := store.Close(); err != nil {
					b.Error(err)
				}
			})
			ctx := context.Background()
			catalog := store.Catalog()
			if err := catalog.UpsertRepositoryFamily(ctx, store_sqlite.RepositoryFamily{
				FamilyID: "bench-family", CommonDirIdentity: "bench-common", State: "family_ready",
			}); err != nil {
				b.Fatal(err)
			}
			if err := catalog.UpsertCheckout(ctx, store_sqlite.Checkout{
				CheckoutID: testCheckoutID, Incarnation: "bench-incarnation", FamilyID: "bench-family",
				State: store_sqlite.CheckoutStateReady, DesiredMode: store_sqlite.CheckoutModeDedicated,
				EffectiveMode: store_sqlite.CheckoutModeDedicated,
			}); err != nil {
				b.Fatal(err)
			}
			if err := catalog.UpsertDedicatedGraph(ctx, store_sqlite.DedicatedGraph{
				GraphID: testGraphID, RepoPrefix: stackRepo, FamilyID: "bench-family",
				OwnerCheckoutID: testCheckoutID, State: "graph_ready",
			}); err != nil {
				b.Fatal(err)
			}
			file := stackRepo + "/bench.go"
			node := dedicatedRootSymbol(file, "Bench", 7)
			kindName := "dedicated"
			if kind == "legacy_commit" {
				kindName = "commit"
			}
			selected := writeDedicatedRootRequest(b, store, store_sqlite.PayloadGenerationRequest{
				OwnerKind: "dedicated_graph", GraphID: testGraphID, LayerID: "bench-root",
				CheckoutID: testCheckoutID, GenerationKind: kindName, TreeOID: "bench-root-tree", CreatedAt: 500,
			}, []*graph.Node{dedicatedRootFileNode(file), node},
				[]store_sqlite.FileMask{{RepoPrefix: stackRepo, FilePath: file, Mode: store_sqlite.OwnershipReplace}})
			if kind == "inherited_dedicated" {
				selected = writeDedicatedRootRequest(b, store, store_sqlite.PayloadGenerationRequest{
					OwnerKind: "dedicated_graph", GraphID: testGraphID, LayerID: "bench-child",
					CheckoutID: testCheckoutID, GenerationKind: "commit", BaseGenerationID: selected,
					TreeOID: "bench-child-tree", CreatedAt: 600,
				}, nil, nil)
			}
			materializer := newTestMaterializer(store)
			b.ReportAllocs()
			for b.Loop() {
				view, err := materializer.assemble(ctx, testGraphID, stackRepo, []int64{selected}, nil)
				if err != nil {
					b.Fatal(err)
				}
				if got := view.Reader.GetNode(node.ID); got == nil || got.StartLine != 7 {
					view.Close()
					b.Fatalf("valid %s view lost committed symbol: %+v", kind, got)
				}
				view.Close()
			}
		})
	}
}
