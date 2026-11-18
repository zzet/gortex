package graph

import (
	"iter"
	"sort"
	"testing"
)

// detachedChainLayer is a published chain layer that carries a node at a
// path it does not cover (a restated identity), as a generation layer's
// mask-backed rows at an unclaimed path do.
type detachedChainLayer struct {
	*OverlayLayer
	generation int64
	detached   *Node
}

func (l *detachedChainLayer) GenerationID() int64 { return l.generation }

func (l *detachedChainLayer) OwnsNodeIdentity(id string) bool {
	return id == l.detached.ID || l.OverlayLayer.OwnsNodeIdentity(id)
}

func (l *detachedChainLayer) NodeByID(id string) *Node {
	if id == l.detached.ID {
		return l.detached
	}
	return l.OverlayLayer.NodeByID(id)
}

func (l *detachedChainLayer) Nodes() iter.Seq[*Node] {
	return func(yield func(*Node) bool) {
		if !yield(l.detached) {
			return
		}
		for n := range l.OverlayLayer.Nodes() {
			if !yield(n) {
				return
			}
		}
	}
}

func (l *detachedChainLayer) DetachedFileNodes(filePath string) []*Node {
	if filePath == l.detached.FilePath {
		return []*Node{l.detached}
	}
	return nil
}

func (l *detachedChainLayer) DetachedNodeSummaries() iter.Seq[*Node] {
	return func(yield func(*Node) bool) { yield(l.detached) }
}

func (l *detachedChainLayer) DetachedRepoNodes(repoPrefix string) []*Node {
	if l.detached.RepoPrefix == repoPrefix {
		return []*Node{l.detached}
	}
	return nil
}

// A chain layer's node at a path it does not cover is served from its kept
// rows exactly as the layer itself serves it.
func TestChainLayerRowsServeADetachedNode(t *testing.T) {
	base := New()
	base.AddNode(&Node{ID: "r/a.go", Kind: KindFile, Name: "a.go", FilePath: "r/a.go", RepoPrefix: "r"})
	base.AddNode(&Node{ID: "r/a.go::A", Kind: KindFunction, Name: "A", FilePath: "r/a.go", RepoPrefix: "r"})
	base.AddNode(&Node{ID: "r/b.go", Kind: KindFile, Name: "b.go", FilePath: "r/b.go", RepoPrefix: "r"})
	chain := &detachedChainLayer{OverlayLayer: NewOverlayLayer(), generation: 7,
		detached: &Node{ID: "r/a.go::Restated", Kind: KindFunction, Name: "Restated", FilePath: "r/a.go", RepoPrefix: "r"}}
	chain.MarkFile("r/b.go", false)
	chain.AddNode("r/b.go", &Node{ID: "r/b.go", Kind: KindFile, Name: "b.go", FilePath: "r/b.go", RepoPrefix: "r"})
	below := NewOverlaidViewWithLayer(base, chain)

	render := func(m map[string][]*Node) []string {
		var out []string
		for p, nodes := range m {
			for _, n := range nodes {
				out = append(out, p+"="+n.ID)
			}
		}
		sort.Strings(out)
		return out
	}
	plain := NewDeltaWriter(below, nil)
	want := render(plain.GetFileNodesByPaths([]string{"r/a.go", "r/b.go"}))
	keeper := NewChainLayerRows(0, 0)
	for i := 0; i < 2; i++ {
		cached := NewDeltaWriter(below, nil)
		cached.SetBaseProjectionCache(NewBaseProjectionCache())
		cached.SetChainGenerations([]int64{7}, []uint64{0})
		cached.SetChainLayerRows(keeper)
		got := render(cached.GetFileNodesByPaths([]string{"r/a.go", "r/b.go"}))
		if len(got) != len(want) {
			t.Fatalf("read %d: %v, want %v", i, got, want)
		}
		for j := range got {
			if got[j] != want[j] {
				t.Fatalf("read %d: %v, want %v", i, got, want)
			}
		}
		kinds := []NodeKind{KindFunction}
		gotKinds := cached.NodesInFilesByKind([]string{"r/a.go"}, kinds)
		wantKinds := plain.NodesInFilesByKind([]string{"r/a.go"}, kinds)
		if len(gotKinds) != len(wantKinds) {
			t.Fatalf("read %d: %d functions at a.go, want %d", i, len(gotKinds), len(wantKinds))
		}
	}
	if !keeper.Holds(7, 0) {
		t.Fatal("the chain layer was not kept")
	}
	found := false
	for _, line := range want {
		if line == "r/a.go=r/a.go::Restated" {
			found = true
		}
	}
	if !found {
		t.Fatalf("fixture precondition: the uncached read does not serve the detached node: %v", want)
	}
}
