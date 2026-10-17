package graph

import (
	"context"
	"fmt"
	"testing"
)

func TestOverlaidViewExactNameManyDetachedMasksFilterBeforeLimit(t *testing.T) {
	base := New()
	layer := NewOverlayLayer()
	for index := 0; index < 1024; index++ {
		stale := &Node{ID: fmt.Sprintf("repo/old-%04d.go::handle", index), Name: "handle",
			Kind: KindFunction, FilePath: fmt.Sprintf("repo/old-%04d.go", index)}
		base.AddNode(stale)
		layer.MarkRemoved(stale.Name, stale.ID)
	}
	visible := &Node{ID: "repo/visible.go::handle", Name: "handle", Kind: KindFunction, FilePath: "repo/visible.go"}
	base.AddNode(visible)
	recording := &recordingBoundedExactNameReader{Reader: base, bounded: base}
	page, err := NewOverlaidView(recording, layer).FindNodesByNameBounded(context.Background(), "handle", LocalizationNodeScope{}, 8)
	if err != nil || page.Total != 1 || page.Truncated || len(page.Nodes) != 1 || page.Nodes[0].ID != visible.ID {
		t.Fatalf("matching masks lost the visible homonym: page=%#v err=%v", page, err)
	}
	if len(recording.limits) != 1 || recording.limits[0] != 8 {
		t.Fatalf("global mask count inflated the lower limit: %v", recording.limits)
	}
}

func TestOverlaidViewExactNameUnmatchedCoveredMasksDoNotConsumeInspectionBudget(t *testing.T) {
	base := New()
	visible := &Node{ID: "repo/visible.go::handle", Name: "handle", Kind: KindFunction, FilePath: "repo/visible.go"}
	base.AddNode(visible)
	layer := NewOverlayLayer()
	layer.MarkFile("repo/generated.go", true)
	for index := 0; index <= overlayExactNameInspectionLimit; index++ {
		layer.MarkRemoved("handle", fmt.Sprintf("repo/generated.go::handle:%04d", index))
	}
	page, err := NewOverlaidView(base, layer).FindNodesByNameBounded(context.Background(), "handle", LocalizationNodeScope{}, 8)
	if err != nil || page.Total != 1 || page.Truncated || len(page.Nodes) != 1 || page.Nodes[0].ID != visible.ID {
		t.Fatalf("unmatched covered masks consumed the query budget: page=%#v err=%v", page, err)
	}
}
