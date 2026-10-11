package graphview

import (
	"path/filepath"
	"reflect"
	"testing"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

type countedIncomingLayer struct {
	*GenerationLayer
	pointCalls, batchCalls int
	batchIDs               []string
}

func (l *countedIncomingLayer) InEdges(id string) []*graph.Edge {
	l.pointCalls++
	return l.GenerationLayer.InEdges(id)
}

func (l *countedIncomingLayer) LayerInEdgesByNodeIDs(ids []string) map[string][]*graph.Edge {
	l.batchCalls++
	l.batchIDs = append([]string(nil), ids...)
	return l.GenerationLayer.LayerInEdgesByNodeIDs(ids)
}

func TestOverlaidIncomingBatchPreservesLayerMasksAndPointFallback(t *testing.T) {
	store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "incoming.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	base := graph.New()
	base.AddEdge(&graph.Edge{From: "base-keep", To: "target", FilePath: "keep.go", Kind: graph.EdgeCalls})
	base.AddEdge(&graph.Edge{From: "base-masked", To: "target", FilePath: "replace.go", Kind: graph.EdgeCalls})
	base.AddEdge(&graph.Edge{From: "removed", To: "target", FilePath: "keep.go", Kind: graph.EdgeCalls})
	lowerHandle, upperHandle := store.AtGeneration(41), store.AtGeneration(42)
	if err := lowerHandle.AddBatchChecked(nil, []*graph.Edge{
		{From: "lower-keep", To: "target", FilePath: "lower.go", Kind: graph.EdgeCalls},
		{From: "lower-context", To: "target", FilePath: "ctx.go", Kind: graph.EdgeCalls},
		{From: "lower-masked", To: "target", FilePath: "replace.go", Kind: graph.EdgeCalls},
	}); err != nil {
		t.Fatal(err)
	}
	if err := upperHandle.AddBatchChecked(nil, []*graph.Edge{
		{From: "upper", To: "target", FilePath: "replace.go", Kind: graph.EdgeCalls},
		{From: "other-target", To: "second", FilePath: "second.go", Kind: graph.EdgeCalls},
	}); err != nil {
		t.Fatal(err)
	}
	lower := &countedIncomingLayer{GenerationLayer: &GenerationLayer{handle: lowerHandle, contextPaths: map[string]struct{}{"ctx.go": {}}}}
	upper := &countedIncomingLayer{GenerationLayer: &GenerationLayer{handle: upperHandle, covered: map[string]store_sqlite.OwnershipMode{"replace.go": store_sqlite.OwnershipReplace}, removed: map[string]struct{}{"removed": {}}}}
	ids := []string{"target", "", "target", "second", "missing"}
	pointView := graph.NewOverlaidViewWithLayer(graph.NewOverlaidViewWithLayer(base, struct{ graph.OverlayLayerReader }{lower}), struct{ graph.OverlayLayerReader }{upper})
	want := pointView.GetInEdgesByNodeIDs(ids)
	if lower.pointCalls != 3 || upper.pointCalls != 3 {
		t.Fatalf("point counts lower=%d upper=%d", lower.pointCalls, upper.pointCalls)
	}
	lower.pointCalls, upper.pointCalls = 0, 0
	view := graph.NewOverlaidViewWithLayer(graph.NewOverlaidViewWithLayer(base, lower), upper)
	got := view.GetInEdgesByNodeIDs(ids)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("batch edges=%v point edges=%v", got, want)
	}
	for _, layer := range []*countedIncomingLayer{lower, upper} {
		if layer.batchCalls != 1 || layer.pointCalls != 0 || !reflect.DeepEqual(layer.batchIDs, []string{"target", "second", "missing"}) {
			t.Fatalf("batch=%d point=%d ids=%v", layer.batchCalls, layer.pointCalls, layer.batchIDs)
		}
	}
	if sources := []string{got["target"][0].From, got["target"][1].From, got["target"][2].From}; !reflect.DeepEqual(sources, []string{"base-keep", "lower-keep", "upper"}) {
		t.Fatalf("sources=%v", sources)
	}
	if len(got["second"]) != 1 || got["second"][0].From != "other-target" {
		t.Fatalf("second=%v", got["second"])
	}
	if nilBase := graph.NewOverlaidViewWithLayer(nil, upper).GetInEdgesByNodeIDs([]string{"missing"}); len(nilBase) != 0 {
		t.Fatalf("invented missing key: %v", nilBase)
	}
}
