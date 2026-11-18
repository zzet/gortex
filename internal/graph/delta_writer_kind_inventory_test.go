package graph

import (
	"iter"
	"reflect"
	"testing"
)

type kindInventoryBase struct {
	*Graph
	full, summary int
}

func (s *kindInventoryBase) NodesInScopeSeq(repos, paths []string, kinds ...NodeKind) iter.Seq[*Node] {
	s.full++
	return NodesInScopeSeq(s.Graph, repos, paths, kinds...)
}
func (s *kindInventoryBase) NodesLightInScopeSeq(repos, paths []string) iter.Seq[*Node] {
	return NodesInScopeSeq(s.Graph, repos, paths, KindFile, KindFunction)
}
func (s *kindInventoryBase) EdgesInScopeSeq([]string, []string, ...EdgeKind) iter.Seq[ScopedEdgeRow] {
	return func(func(ScopedEdgeRow) bool) {}
}
func (s *kindInventoryBase) NodesLightByKindsInScopeSeq(repos, paths []string, kinds ...NodeKind) iter.Seq[*Node] {
	s.summary++
	return func(yield func(*Node) bool) {
		for n := range NodesInScopeSeq(s.Graph, repos, paths, kinds...) {
			copyNode := *n
			copyNode.Meta = nil
			if !yield(&copyNode) {
				return
			}
		}
	}
}
func TestDeltaWriterKindInventoryPreservesCompositionAndFallback(t *testing.T) {
	base := &kindInventoryBase{Graph: New()}
	for _, path := range []string{"repo/a.go", "repo/removed.go", "other/c.go"} {
		base.AddBatch([]*Node{{ID: path, Kind: KindFile, FilePath: path, RepoPrefix: RepoPrefixOfID(path), Language: "go", Meta: map[string]any{"large": "payload"}}, {ID: path + "::fn", Kind: KindFunction, FilePath: path, RepoPrefix: RepoPrefixOfID(path)}}, nil)
	}
	dw := NewDeltaWriter(base, nil)
	dw.EvictFiles([]string{"repo/removed.go"})
	dw.AddNode(&Node{ID: "repo/b.go", Kind: KindFile, FilePath: "repo/b.go", RepoPrefix: "repo", Language: "python", WorkspaceID: "workspace", ProjectID: "project"})
	project := func(seq iter.Seq[*Node]) []Node {
		var out []Node
		for n := range seq {
			out = append(out, Node{ID: n.ID, Kind: n.Kind, Language: n.Language, FilePath: n.FilePath, RepoPrefix: n.RepoPrefix, WorkspaceID: n.WorkspaceID, ProjectID: n.ProjectID})
		}
		return out
	}
	want := project(dw.NodesInScopeSeq([]string{"repo"}, nil, KindFile))
	base.full = 0
	got := project(dw.NodesLightByKindsInScopeSeq([]string{"repo"}, nil, KindFile))
	if !reflect.DeepEqual(want, got) || len(got) != 2 || base.summary != 1 || base.full != 0 {
		t.Fatalf("want=%+v got=%+v full=%d summary=%d", want, got, base.full, base.summary)
	}
	if got[1].Language != "python" {
		t.Fatal("language inferred from .go suffix")
	}
	count := 0
	dw.NodesLightByKindsInScopeSeq([]string{"repo"}, nil, KindFile)(func(*Node) bool { count++; return false })
	if count != 1 {
		t.Fatal("yield stop ignored")
	}
	if rows := project(dw.NodesLightByKindsInScopeSeq(nil, nil, KindFile)); len(rows) != 0 {
		t.Fatal("unbounded frontier")
	}
	if rows := project(dw.NodesLightByKindsInScopeSeq([]string{"repo"}, nil, NodeKind(""))); len(rows) != 0 {
		t.Fatal("empty kind widened inventory")
	}
	// An adapter without compact capability retains exact full-row fallback.
	legacy := &scopedCountingStore{Graph: base.Graph}
	// This older test seam's full node stream is intentionally empty; the
	// compact request must use it rather than silently enumerate another view.
	if rows := project(NewDeltaWriter(legacy, nil).NodesLightByKindsInScopeSeq([]string{"repo"}, nil, KindFile)); len(rows) != 0 {
		t.Fatal("fallback bypassed selected full scope")
	}
}
