package graph

import (
	"context"
	"testing"
)

func TestFoldedNameMemorySnapshotCopiesRowsAndRefusesContention(t *testing.T) {
	g := New()
	g.AddNode(&Node{ID: "target", Name: "Key", Kind: KindFunction, Language: "go", Meta: map[string]any{"is_test": false}})
	nodes, more, err := g.FindFoldedNameCandidatesContext(t.Context(), "kEY", 1, NameSearchFilter{})
	if err != nil || more || len(nodes) != 1 || nodes[0].Name != "Key" {
		t.Fatalf("coherent memory result = %v/%v/%v", nodes, more, err)
	}
	nodes[0].Name = "caller copy"
	nodes[0].Meta["is_test"] = true
	if original := g.GetNode("target"); original.Name != "Key" || original.Meta["is_test"] != false {
		t.Fatal("returned row aliases the stored node or metadata map")
	}
	g.shards[0].mu.Lock()
	busy, busyMore, busyErr := g.FindFoldedNameCandidatesContext(t.Context(), "kEY", 1, NameSearchFilter{})
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	_, _, cancelErr := g.FindFoldedNameCandidatesContext(canceled, "kEY", 1, NameSearchFilter{})
	g.shards[0].mu.Unlock()
	if busyErr == nil || len(busy) != 0 || busyMore || cancelErr != context.Canceled {
		t.Fatalf("busy/cancel = %v/%v/%v/%v", busy, busyMore, busyErr, cancelErr)
	}
}

func TestFoldedNameOverlayMasksBeforeHorizonAndMergesActualMore(t *testing.T) {
	base := New()
	base.AddNode(&Node{ID: "old.go::Needle", Name: "Needle", Kind: KindFunction, FilePath: "old.go"})
	base.AddNode(&Node{ID: "keep.go::NeedleLong", Name: "NeedleLong", Kind: KindFunction, FilePath: "keep.go"})
	layer := NewOverlayLayer()
	layer.MarkFile("old.go", false)
	layer.AddNode("new.go", &Node{ID: "new.go::Needle", Name: "Needle", Kind: KindFunction, FilePath: "new.go"})
	view := NewOverlaidViewWithLayer(base, layer)
	nodes, more, err := view.FindFoldedNameCandidatesContext(t.Context(), "Needle", 1, NameSearchFilter{})
	if err != nil || !more || len(nodes) != 1 || nodes[0].ID != "new.go::Needle" {
		t.Fatalf("masked ranked head = %v/%v/%v", nodes, more, err)
	}
	nodes, more, err = view.FindFoldedNameCandidatesContext(t.Context(), "Needle", 2, NameSearchFilter{})
	if err != nil || more || len(nodes) != 2 || nodes[0].ID != "new.go::Needle" || nodes[1].ID != "keep.go::NeedleLong" {
		t.Fatalf("complete selected head = %v/%v/%v", nodes, more, err)
	}
}
