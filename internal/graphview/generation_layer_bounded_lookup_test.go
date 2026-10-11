package graphview

import (
	"iter"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

type boundedLookupBase struct {
	graph.Reader
	names  []*graph.Node
	limits []int
	edges  map[string][]*graph.Edge
}

func (b *boundedLookupBase) FindNodesByNameContaining(_ string, limit int) []*graph.Node {
	b.limits = append(b.limits, limit)
	n := len(b.names)
	if limit > 0 && limit < n {
		n = limit
	}
	return append([]*graph.Node(nil), b.names[:n]...)
}

func (b *boundedLookupBase) GetOutEdgesByNodeIDs(ids []string) map[string][]*graph.Edge {
	out := make(map[string][]*graph.Edge, len(ids))
	for _, id := range ids {
		out[id] = append([]*graph.Edge(nil), b.edges[id]...)
	}
	return out
}

type boundedLookupLayer struct {
	*graph.OverlayLayer
	names                  []*graph.Node
	nameCalls, namedCalls  int
	batchCalls, pointCalls int
	batchIDs               []string
	edges                  map[string][]*graph.Edge
}

func (l *boundedLookupLayer) VisitNodesByNameContainingFolded(_ string, yield func(*graph.Node) bool) {
	l.nameCalls++
	for _, node := range l.names {
		if !yield(node) {
			return
		}
	}
}

func (l *boundedLookupLayer) NamedNodes() iter.Seq2[string, []*graph.Node] {
	l.namedCalls++
	return func(func(string, []*graph.Node) bool) {}
}

func (l *boundedLookupLayer) GetOutEdgesByNodeIDs(ids []string) map[string][]*graph.Edge {
	l.batchCalls++
	l.batchIDs = append([]string(nil), ids...)
	out := make(map[string][]*graph.Edge, len(ids))
	for _, id := range ids {
		out[id] = append([]*graph.Edge(nil), l.edges[id]...)
	}
	return out
}

func (l *boundedLookupLayer) OutEdges(string) []*graph.Edge {
	l.pointCalls++
	return nil
}

func TestOverlaidViewUsesBoundedNameAndEdgeLayerCapabilities(t *testing.T) {
	layer := &boundedLookupLayer{
		OverlayLayer: graph.NewOverlayLayer(),
		names: []*graph.Node{{
			ID:       "overlay.go::hit",
			Name:     "NeedleOverlay",
			FilePath: "overlay.go",
		}},
		edges: map[string][]*graph.Edge{
			"a": {{From: "a", To: "layer-a", FilePath: "overlay.go", Kind: graph.EdgeKind("calls")}},
			"b": {{From: "b", To: "layer-b", FilePath: "overlay.go", Kind: graph.EdgeKind("calls")}},
		},
	}
	layer.MarkFile("masked.go", false)
	layer.MarkRemoved("gone", "gone.go::gone")
	base := &boundedLookupBase{
		names: []*graph.Node{
			{ID: "masked.go::1", Name: "needle1", FilePath: "masked.go"},
			{ID: "masked.go::2", Name: "needle2", FilePath: "masked.go"},
			{ID: "gone.go::gone", Name: "needle3", FilePath: "gone.go"},
			{ID: "masked.go::4", Name: "needle4", FilePath: "masked.go"},
			{ID: "visible.go::1", Name: "needle5", FilePath: "visible.go"},
			{ID: "visible.go::2", Name: "needle6", FilePath: "visible.go"},
		},
		edges: map[string][]*graph.Edge{
			"a": {
				{From: "a", To: "visible", FilePath: "other.go", Kind: graph.EdgeKind("calls")},
				{From: "a", To: "masked", FilePath: "masked.go", Kind: graph.EdgeKind("calls")},
				{From: "a", To: "gone.go::gone", FilePath: "other.go", Kind: graph.EdgeKind("calls")},
			},
		},
	}
	view := graph.NewOverlaidViewWithLayer(base, layer)

	got := view.FindNodesByNameContaining("needle", 3)
	gotIDs := []string{got[0].ID, got[1].ID, got[2].ID}
	wantIDs := []string{"overlay.go::hit", "visible.go::1", "visible.go::2"}
	if !reflect.DeepEqual(gotIDs, wantIDs) {
		t.Fatalf("names got %v want %v", gotIDs, wantIDs)
	}
	if layer.nameCalls != 1 || layer.namedCalls != 0 {
		t.Fatalf("name calls visitor=%d NamedNodes=%d", layer.nameCalls, layer.namedCalls)
	}
	if !reflect.DeepEqual(base.limits, []int{4, 8}) {
		t.Fatalf("limits=%v", base.limits)
	}

	edges := view.GetOutEdgesByNodeIDs([]string{"a", "", "a", "b", "c"})
	if layer.batchCalls != 1 || layer.pointCalls != 0 {
		t.Fatalf("edge calls batch=%d point=%d", layer.batchCalls, layer.pointCalls)
	}
	if !reflect.DeepEqual(layer.batchIDs, []string{"a", "b", "c"}) {
		t.Fatalf("batch IDs=%v", layer.batchIDs)
	}
	if got := edges["a"]; len(got) != 2 || got[0].To != "visible" || got[1].To != "layer-a" {
		t.Fatalf("a edges=%v", got)
	}
	if got := edges["b"]; len(got) != 1 || got[0].To != "layer-b" {
		t.Fatalf("b edges=%v", got)
	}
	if got, ok := edges["c"]; !ok || len(got) != 0 {
		t.Fatalf("c edges present=%v value=%v", ok, got)
	}

	plain := graph.NewOverlayLayer()
	plain.AddEdge(&graph.Edge{From: "a", To: "fallback", FilePath: "plain.go", Kind: graph.EdgeKind("calls")})
	fallback := graph.NewOverlaidViewWithLayer(nil, plain).GetOutEdgesByNodeIDs([]string{"a"})
	if len(fallback["a"]) != 1 || fallback["a"][0].To != "fallback" {
		t.Fatalf("fallback=%v", fallback)
	}
}

func TestGenerationLayerBoundedNameLookupFiltersContextAndGeneration(t *testing.T) {
	store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "generation.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})

	gen := store.AtGeneration(41)
	other := store.AtGeneration(42)
	rows := []*graph.Node{
		{ID: "a-context", Name: "needle context", FilePath: "ctx.go", Kind: graph.NodeKind("function")},
		{ID: "b-visible", Name: "Needle visible", FilePath: "visible.go", Kind: graph.NodeKind("function")},
		{ID: "c-kelvin", Name: "Kite", FilePath: "kelvin.go", Kind: graph.NodeKind("function")},
		{ID: "d-greek", Name: "Δelta", FilePath: "greek.go", Kind: graph.NodeKind("function")},
		{ID: "e-literal", Name: `100%_\done`, FilePath: "literal.go", Kind: graph.NodeKind("function")},
		{ID: "source", Name: "source", FilePath: "visible.go", Kind: graph.NodeKind("function")},
	}
	genEdges := []*graph.Edge{
		{From: "source", To: "b-visible", FilePath: "ctx.go", Kind: graph.EdgeKind("calls")},
		{From: "source", To: "c-kelvin", FilePath: "visible.go", Kind: graph.EdgeKind("calls")},
	}
	if err := gen.AddBatchChecked(rows, genEdges); err != nil {
		t.Fatal(err)
	}
	if err := other.AddBatchChecked(
		[]*graph.Node{{ID: "other", Name: "Needle other generation", FilePath: "other.go", Kind: graph.NodeKind("function")}},
		[]*graph.Edge{{From: "source", To: "other", FilePath: "other.go", Kind: graph.EdgeKind("calls")}},
	); err != nil {
		t.Fatal(err)
	}

	layer := &GenerationLayer{
		handle:       gen,
		contextPaths: map[string]struct{}{"ctx.go": {}},
	}
	view := graph.NewOverlaidViewWithLayer(nil, layer)
	if got := view.FindNodesByNameContaining("needle", 1); len(got) != 1 || got[0].ID != "b-visible" {
		t.Fatalf("context/limit=%v", got)
	}
	cases := map[string]string{
		"k":   "c-kelvin",
		"δE":  "d-greek",
		`%_\`: "e-literal",
	}
	for query, want := range cases {
		got := view.FindNodesByNameContaining(query, 1)
		if len(got) != 1 || got[0].ID != want {
			t.Fatalf("%q=%v want %s", query, got, want)
		}
	}
	if got := view.FindNodesByNameContaining("other generation", 0); len(got) != 0 {
		t.Fatalf("generation leak=%v", got)
	}
	if got := view.FindNodesByNameContaining("absent", 1); len(got) != 0 {
		t.Fatalf("no result=%v", got)
	}

	batch := layer.GetOutEdgesByNodeIDs([]string{"source", "source", "missing"})
	if got := batch["source"]; len(got) != 1 || got[0].To != "c-kelvin" || got[0].FilePath != "visible.go" {
		t.Fatalf("generation batch=%v", got)
	}
	if got, ok := batch["missing"]; !ok || len(got) != 0 {
		t.Fatalf("missing batch present=%v value=%v", ok, got)
	}

	lower := &boundedLookupBase{
		names: []*graph.Node{{ID: "lower.go::hit", Name: "lower target", FilePath: "lower.go"}},
	}
	lowerView := graph.NewOverlaidViewWithLayer(lower, layer)
	if got := lowerView.FindNodesByNameContaining("lower target", 1); len(got) != 1 || got[0].ID != "lower.go::hit" {
		t.Fatalf("lower result=%v", got)
	}
	if layer.nodes != nil || layer.named != nil {
		t.Fatalf("whole generation materialized nodes=%v named=%v", layer.nodes != nil, layer.named != nil)
	}
}
