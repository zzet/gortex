package graph

import (
	"context"
	"testing"
)

func TestFilteredNameSearchLegacyOverlayAndDeltaScopeBeforeCap(t *testing.T) {
	base := New()
	for _, n := range []*Node{
		{ID: "other/out.go::needle", Name: "needle", Kind: KindFunction, FilePath: "other/out.go", RepoPrefix: "other"},
		{ID: "repo/hidden.go::needle", Name: "needle", Kind: KindFunction, FilePath: "repo/hidden.go", RepoPrefix: "repo"},
		{ID: "repo/keep.go::needle", Name: "needle", Kind: KindFunction, FilePath: "repo/keep.go", RepoPrefix: "repo"},
	} {
		base.AddNode(n)
	}
	filter := NameSearchFilter{RepoAllow: map[string]bool{"repo": true}, Accept: func(n *Node) bool { return n.FilePath == "repo/keep.go" }}
	for _, r := range []Reader{base, NewDeltaWriter(base, nil)} {
		got, err := FindNodesByNameContainingFilteredContext(context.Background(), r, "needle", 1, filter)
		if err != nil || len(got) != 1 || got[0].ID != "repo/keep.go::needle" {
			t.Fatalf("legacy/delta scoped cap=%v,%v", got, err)
		}
	}
	layer := NewOverlayLayer()
	layer.MarkFile("repo/hidden.go", true)
	layer.AddNode("other/upper.go", &Node{ID: "other/upper.go::needle", Name: "needle", Kind: KindFunction, FilePath: "other/upper.go", RepoPrefix: "other"})
	view := NewOverlaidView(base, layer)
	got, err := view.FindNodesByNameContainingFilteredContext(context.Background(), "needle", 1, NameSearchFilter{RepoAllow: map[string]bool{"repo": true}})
	if err != nil || len(got) != 1 || got[0].ID != "repo/keep.go::needle" {
		t.Fatalf("hidden/outside consumed overlay budget=%v,%v", got, err)
	}
	layer.AddNode("repo/upper.go", &Node{ID: "repo/upper.go::ÉCOLE", Name: "ÉCOLE", Kind: KindFunction, FilePath: "repo/upper.go", RepoPrefix: "repo"})
	got, err = view.FindNodesByNameContainingFilteredContext(context.Background(), "école", 1, NameSearchFilter{RepoAllow: map[string]bool{"repo": true}})
	if err != nil || len(got) != 1 || got[0].Name != "ÉCOLE" {
		t.Fatalf("upper Unicode folding changed=%v,%v", got, err)
	}
	// Delta results stay detached from its mutable working graph.
	dw := NewDeltaWriter(base, nil)
	got, err = dw.FindNodesByNameContainingFilteredContext(context.Background(), "needle", 1, filter)
	if err != nil || len(got) != 1 {
		t.Fatalf("delta filtered lookup=%v,%v", got, err)
	}
	got[0].Name = "mutated caller copy"
	if n := dw.GetNode("repo/keep.go::needle"); n.Name != "needle" {
		t.Fatal("filtered delta lookup leaked mutable pointer")
	}
}
