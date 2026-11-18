package graphview

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

func TestGenerationLayerExactNameVisitorFiltersContextPayloadAndStops(t *testing.T) {
	store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "generation.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	gen := store.AtGeneration(41)
	if err := gen.AddBatchChecked([]*graph.Node{
		{ID: "a-context", Name: "Shared", FilePath: "ctx.go", Kind: graph.KindFunction},
		{ID: "b-visible", Name: "Shared", FilePath: "visible.go", Kind: graph.KindFunction},
		{ID: "c-late", Name: "Shared", FilePath: "late.go", Kind: graph.KindFunction},
	}, nil); err != nil {
		t.Fatal(err)
	}
	layer := &GenerationLayer{handle: gen, contextPaths: map[string]struct{}{"ctx.go": {}}}
	var got []string
	err = layer.VisitNodesByNameContext(context.Background(), "Shared", func(node *graph.Node) bool {
		got = append(got, node.ID)
		return false
	})
	if err != nil || !reflect.DeepEqual(got, []string{"b-visible"}) {
		t.Fatalf("visible rows = %v, err %v", got, err)
	}
}

func TestGenerationLayerExactNamesVisitorPreservesContextBaseAndMasks(t *testing.T) {
	store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "generation.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	baseNodes := []*graph.Node{
		{ID: "ctx.go::fn", Name: "Shared", FilePath: "ctx.go", Kind: graph.KindFunction},
		{ID: "covered.go::old", Name: "Other", FilePath: "covered.go", Kind: graph.KindFunction},
		{ID: "deleted.go::old", Name: "Other", FilePath: "deleted.go", Kind: graph.KindFunction},
	}
	if err := store.AddBatchChecked(baseNodes, nil); err != nil {
		t.Fatal(err)
	}
	gen := store.AtGeneration(41)
	if err := gen.AddBatchChecked([]*graph.Node{
		{ID: "ctx.go::fn", Name: "Shared", FilePath: "ctx.go", Kind: graph.KindFunction},
		{ID: "covered.go::new", Name: "Shared", FilePath: "covered.go", Kind: graph.KindFunction},
	}, nil); err != nil {
		t.Fatal(err)
	}
	layer := &GenerationLayer{handle: gen, contextPaths: map[string]struct{}{"ctx.go": {}}, covered: map[string]store_sqlite.OwnershipMode{"covered.go": store_sqlite.OwnershipReplace, "deleted.go": store_sqlite.OwnershipDelete}}
	view := graph.NewOverlaidViewWithLayer(store, layer)
	var got []string
	err = graph.VisitNodesByNamesContext(context.Background(), view, []string{"Shared", "Other"}, func(node *graph.Node) bool { got = append(got, node.ID); return true })
	if err != nil || !reflect.DeepEqual(got, []string{"covered.go::new", "ctx.go::fn"}) {
		t.Fatalf("generation overlay batch = %v err=%v", got, err)
	}
}

func TestGenerationLayerExactDeclarationsPreserveContextBaseAndMasks(t *testing.T) {
	store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "generation.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	baseNodes := []*graph.Node{
		{ID: "ctx.go::fn", Name: "Shared", FilePath: "ctx.go", Kind: graph.KindFunction},
		{ID: "covered.go::old", Name: "Other", FilePath: "covered.go", Kind: graph.KindFunction},
		{ID: "deleted.go::old", Name: "Other", FilePath: "deleted.go", Kind: graph.KindFunction},
	}
	if err := store.AddBatchChecked(baseNodes, nil); err != nil {
		t.Fatal(err)
	}
	gen := store.AtGeneration(41)
	if err := gen.AddBatchChecked([]*graph.Node{
		{ID: "ctx.go::fn", Name: "Shared", FilePath: "ctx.go", Kind: graph.KindFunction},
		{ID: "covered.go::new", Name: "Shared", FilePath: "covered.go", Kind: graph.KindFunction},
	}, nil); err != nil {
		t.Fatal(err)
	}
	layer := &GenerationLayer{handle: gen, contextPaths: map[string]struct{}{"ctx.go": {}}, covered: map[string]store_sqlite.OwnershipMode{"covered.go": store_sqlite.OwnershipReplace, "deleted.go": store_sqlite.OwnershipDelete}}
	view := graph.NewOverlaidViewWithLayer(store, layer)
	var got []string
	err = graph.VisitNodeDeclarationsByNamesContext(context.Background(), view, []string{"Shared", "Other"}, func(node *graph.Node) bool { got = append(got, node.ID); return true })
	if err != nil || !reflect.DeepEqual(got, []string{"covered.go::new", "ctx.go::fn"}) {
		t.Fatalf("generation overlay batch = %v err=%v", got, err)
	}
}

func TestGenerationLayerRepoDeclarationsPreserveContextBaseAndMasks(t *testing.T) {
	store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "generation.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	base := []*graph.Node{
		{ID: "repo/context.go::fn", Name: "Shared", RepoPrefix: "repo", FilePath: "repo/context.go"},
		{ID: "repo/covered.go::old", Name: "Shared", RepoPrefix: "repo", FilePath: "repo/covered.go"},
		{ID: "repo/deleted.go::old", Name: "Shared", RepoPrefix: "repo", FilePath: "repo/deleted.go"},
		{ID: "foreign/file.go::other", Name: "Shared", RepoPrefix: "foreign", FilePath: "foreign/file.go"},
	}
	if err := store.AddBatchChecked(base, nil); err != nil {
		t.Fatal(err)
	}
	h := store.AtGeneration(41)
	if err := h.AddBatchChecked([]*graph.Node{base[0], {ID: "repo/covered.go::new", Name: "Shared", RepoPrefix: "repo", FilePath: "repo/covered.go"}, {ID: "foreign/new.go::other", Name: "Shared", RepoPrefix: "foreign", FilePath: "foreign/new.go"}}, nil); err != nil {
		t.Fatal(err)
	}
	layer := &GenerationLayer{handle: h, contextPaths: map[string]struct{}{"repo/context.go": {}}, covered: map[string]store_sqlite.OwnershipMode{"repo/covered.go": store_sqlite.OwnershipReplace, "repo/deleted.go": store_sqlite.OwnershipDelete}}
	view := graph.NewOverlaidViewWithLayer(store, layer)
	var got []string
	if err := graph.VisitNodeDeclarationsByNamesInRepoContext(t.Context(), view, []string{"Shared", "Missing"}, "repo", func(n *graph.Node) bool { got = append(got, n.ID); return true }); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, []string{"repo/covered.go::new", "repo/context.go::fn"}) {
		t.Fatalf("selected=%v", got)
	}
	seen := 0
	if err := graph.VisitNodeDeclarationsByNamesInRepoContext(t.Context(), view, []string{"Shared"}, "repo", func(*graph.Node) bool { seen++; return false }); err != nil || seen != 1 {
		t.Fatalf("stop=%d err=%v", seen, err)
	}
}
