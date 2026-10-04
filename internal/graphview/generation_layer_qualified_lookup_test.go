package graphview

import (
	"iter"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

type qualifiedBatchBase struct {
	graph.Reader
	rows  map[string][]*graph.Node
	calls int
}

func (b *qualifiedBatchBase) GetNodesByQualNames(names []string) map[string][]*graph.Node {
	b.calls++
	out := make(map[string][]*graph.Node)
	for _, name := range names {
		out[name] = append([]*graph.Node(nil), b.rows[name]...)
	}
	return out
}

type qualifiedBatchLayer struct {
	*graph.OverlayLayer
	rows  map[string][]*graph.Node
	calls int
}

func (l *qualifiedBatchLayer) GetNodesByQualNames(names []string) map[string][]*graph.Node {
	l.calls++
	out := make(map[string][]*graph.Node)
	for _, name := range names {
		out[name] = append([]*graph.Node(nil), l.rows[name]...)
	}
	return out
}

func (*qualifiedBatchLayer) Nodes() iter.Seq[*graph.Node] {
	panic("qualified-name batch lookup enumerated the layer")
}

type qualifiedLegacyLayer struct {
	graph.OverlayLayerReader
	rows  []*graph.Node
	calls int
}

func (l *qualifiedLegacyLayer) Nodes() iter.Seq[*graph.Node] {
	l.calls++
	return func(yield func(*graph.Node) bool) {
		for _, node := range l.rows {
			if !yield(node) {
				return
			}
		}
	}
}

type qualifiedGenerationLayer struct {
	*GenerationLayer
}

func (*qualifiedGenerationLayer) Nodes() iter.Seq[*graph.Node] {
	panic("qualified-name lookup enumerated a persisted generation")
}

func qualifiedLookupIDs(nodes []*graph.Node) []string {
	var ids []string
	for _, node := range nodes {
		ids = append(ids, node.ID)
	}
	return ids
}

func TestOverlaidQualifiedNameBatchPreservesVisibilityAndOrder(t *testing.T) {
	a := &graph.Node{ID: "a", Name: "upper-a", QualName: "pkg.Type", FilePath: "upper.go"}
	b := &graph.Node{ID: "b", Name: "upper-b", QualName: "pkg.Type", FilePath: "upper.go"}
	layer := &qualifiedBatchLayer{
		OverlayLayer: graph.NewOverlayLayer(),
		rows: map[string][]*graph.Node{
			"pkg.Type": {b, nil, a, b},
		},
	}
	layer.MarkFile("upper.go", false)
	layer.AddNode(a.FilePath, a)
	layer.AddNode(b.FilePath, b)
	layer.MarkFile("deleted.go", true)
	layer.MarkRemoved("removed", "e")
	base := &qualifiedBatchBase{
		rows: map[string][]*graph.Node{
			"pkg.Type": {
				{ID: "e", Name: "removed", QualName: "pkg.Type", FilePath: "gone.go"},
				{ID: "d", Name: "survivor", QualName: "pkg.Type", FilePath: "visible.go"},
				{ID: "deleted.go::c", Name: "deleted", QualName: "pkg.Type", FilePath: "deleted.go"},
				{ID: "b", Name: "old-b", QualName: "pkg.Type", FilePath: "upper.go"},
			},
		},
	}
	view := graph.NewOverlaidViewWithLayer(base, layer)
	got := view.GetNodesByQualNames([]string{"pkg.Type", "missing", "pkg.Type", ""})
	if ids := qualifiedLookupIDs(got["pkg.Type"]); !reflect.DeepEqual(ids, []string{"a", "b", "d"}) {
		t.Fatalf("qualified candidates=%v", ids)
	}
	if got["pkg.Type"][1] != b {
		t.Fatal("base row displaced the overlay's identity")
	}
	if len(got) != 1 {
		t.Fatalf("unexpected qualified-name keys: %v", got)
	}
	if layer.calls != 1 || base.calls != 1 {
		t.Fatalf("batch calls layer=%d base=%d", layer.calls, base.calls)
	}
	if empty := view.GetNodesByQualNames([]string{"", ""}); len(empty) != 0 {
		t.Fatalf("empty request=%v", empty)
	}
	if layer.calls != 1 || base.calls != 1 {
		t.Fatal("empty request performed storage work")
	}
	legacy := &qualifiedLegacyLayer{
		OverlayLayerReader: graph.NewOverlayLayer(),
		rows: []*graph.Node{
			{ID: "legacy", QualName: "pkg.Type", FilePath: "legacy.go"},
			{ID: "other", QualName: "pkg.Other", FilePath: "legacy.go"},
		},
	}
	fallback := graph.NewOverlaidViewWithLayer(nil, legacy).GetNodesByQualNames([]string{"pkg.Type"})
	if ids := qualifiedLookupIDs(fallback["pkg.Type"]); !reflect.DeepEqual(ids, []string{"legacy"}) || legacy.calls != 1 {
		t.Fatalf("legacy fallback=%v scans=%d", ids, legacy.calls)
	}
}

func TestGenerationQualifiedNameBatchStaysBoundedAndIsolated(t *testing.T) {
	store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "qualified.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	current := store.AtGeneration(41)
	other := store.AtGeneration(42)
	if err := current.AddBatchChecked([]*graph.Node{
		{ID: "same", Name: "current-same", QualName: "pkg.Type", FilePath: "visible.go", Kind: graph.NodeKind("function")},
		{ID: "b", Name: "current-b", QualName: "pkg.Type", FilePath: "other-visible.go", Kind: graph.NodeKind("function")},
		{ID: "context", Name: "context", QualName: "pkg.Type", FilePath: "ctx.go", Kind: graph.NodeKind("function")},
		{ID: "context-only", Name: "context-only", QualName: "pkg.ContextOnly", FilePath: "ctx.go", Kind: graph.NodeKind("function")},
		{ID: "unrelated", Name: "unrelated", QualName: "pkg.Unrelated", FilePath: "unrelated.go", Kind: graph.NodeKind("function")},
	}, nil); err != nil {
		t.Fatal(err)
	}
	if err := other.AddBatchChecked([]*graph.Node{
		{ID: "same", Name: "foreign-same", QualName: "pkg.Type", FilePath: "foreign.go", Kind: graph.NodeKind("function")},
		{ID: "foreign", Name: "foreign", QualName: "pkg.Type", FilePath: "foreign.go", Kind: graph.NodeKind("function")},
	}, nil); err != nil {
		t.Fatal(err)
	}
	layer := &GenerationLayer{
		handle:       current,
		contextPaths: map[string]struct{}{"ctx.go": {}},
	}
	view := graph.NewOverlaidViewWithLayer(nil, &qualifiedGenerationLayer{GenerationLayer: layer})
	got := view.GetNodesByQualNames([]string{"pkg.Type", "pkg.ContextOnly", "missing", "", "pkg.Type"})
	if ids := qualifiedLookupIDs(got["pkg.Type"]); !reflect.DeepEqual(ids, []string{"b", "same"}) {
		t.Fatalf("generation candidates=%v", ids)
	}
	if got["pkg.Type"][1].Name != "current-same" {
		t.Fatalf("foreign generation won: %v", got["pkg.Type"][1])
	}
	if len(got) != 1 {
		t.Fatalf("context or missing names leaked: %v", got)
	}
	if empty := layer.GetNodesByQualNames(nil); len(empty) != 0 {
		t.Fatalf("nil request=%v", empty)
	}
	if empty := layer.GetNodesByQualNames([]string{"", "missing", "pkg.ContextOnly"}); len(empty) != 0 {
		t.Fatalf("empty or invisible request=%v", empty)
	}
	if layer.nodes != nil || layer.named != nil {
		t.Fatalf("whole generation materialized nodes=%v named=%v", layer.nodes != nil, layer.named != nil)
	}
}

func TestOverlaidQualifiedNameBatchPreservesRenamedIdentity(t *testing.T) {
	upper := &graph.Node{ID: "renamed", Name: "New", QualName: "pkg.New"}
	layer := &qualifiedBatchLayer{
		OverlayLayer: graph.NewOverlayLayer(),
		rows: map[string][]*graph.Node{
			"pkg.New": {upper},
		},
	}
	layer.AddNode(upper.FilePath, upper)
	base := &qualifiedBatchBase{
		rows: map[string][]*graph.Node{
			"pkg.Old": {{ID: "renamed", Name: "Old", QualName: "pkg.Old"}},
		},
	}
	view := graph.NewOverlaidViewWithLayer(base, layer)
	if got := view.GetNodesByQualNames([]string{"pkg.Old"}); len(got) != 0 {
		t.Fatalf("lower renamed identity was resurrected: %v", got)
	}
	got := view.GetNodesByQualNames([]string{"pkg.Old", "pkg.New"})
	if len(got) != 1 || len(got["pkg.New"]) != 1 || got["pkg.New"][0] != upper {
		t.Fatalf("renamed identity did not remain upper-owned: %v", got)
	}
}
