package graph

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

type exactVisitorReaderStub struct {
	Reader
	nodes  []*Node
	err    error
	visits int
}

func (r *exactVisitorReaderStub) VisitNodesByNameContext(ctx context.Context, _ string, yield func(*Node) bool) error {
	for _, node := range r.nodes {
		r.visits++
		if !yield(node) {
			return ctx.Err()
		}
	}
	return r.err
}

func TestExactNameVisitorOverlaidMasksAndEarlyStops(t *testing.T) {
	layer := NewOverlayLayer()
	layer.MarkFile("covered.go", false)
	layer.AddNode("covered.go", &Node{ID: "overlay", Name: "Name", FilePath: "covered.go"})
	base := &exactVisitorReaderStub{nodes: []*Node{
		{ID: "covered.go::masked", Name: "Name", FilePath: "covered.go"},
		{ID: "visible", Name: "Name", FilePath: "visible.go"},
	}}
	view := NewOverlaidView(base, layer)
	var got []string
	err := VisitNodesByNameContext(context.Background(), view, "Name", func(node *Node) bool {
		got = append(got, node.ID)
		return node.ID != "visible"
	})
	if err != nil || !reflect.DeepEqual(got, []string{"overlay", "visible"}) || base.visits != 2 {
		t.Fatalf("masked visitor = %v, visits %d, err %v", got, base.visits, err)
	}
}

func TestExactNameVisitorOverlayStopAvoidsBaseAndErrorsAreAuthoritative(t *testing.T) {
	layer := NewOverlayLayer()
	layer.AddNode("overlay.go", &Node{ID: "overlay", Name: "Name", FilePath: "overlay.go"})
	base := &exactVisitorReaderStub{nodes: []*Node{{ID: "base", Name: "Name", FilePath: "base.go"}}}
	view := NewOverlaidView(base, layer)
	if err := VisitNodesByNameContext(context.Background(), view, "Name", func(*Node) bool { return false }); err != nil {
		t.Fatal(err)
	}
	if base.visits != 0 {
		t.Fatalf("base visits = %d, want 0", base.visits)
	}

	boom := errors.New("late visitor failure")
	failing := &exactVisitorReaderStub{nodes: []*Node{{ID: "partial"}}, err: boom}
	seen := 0
	err := VisitNodesByNameContext(context.Background(), failing, "Name", func(*Node) bool { seen++; return true })
	if !errors.Is(err, boom) || seen != 1 {
		t.Fatalf("partial error = seen %d, err %v", seen, err)
	}
}
