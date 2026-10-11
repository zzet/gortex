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
