package graph

import (
	"context"
	"errors"
	"reflect"
	"sort"
	"testing"
)

type exactNamesVisitorStub struct {
	Reader
	nodes []*Node
	err   error
	calls int
}

func (r *exactNamesVisitorStub) VisitNodesByNamesContext(ctx context.Context, _ []string, yield func(*Node) bool) error {
	r.calls++
	for _, node := range r.nodes {
		if !yield(node) {
			return ctx.Err()
		}
	}
	return r.err
}

func TestExactNamesVisitorOverlayMasksAndStops(t *testing.T) {
	layer := NewOverlayLayer()
	layer.MarkFile("covered.go", false)
	layer.MarkFile("deleted.go", true)
	layer.MarkRemoved("Old", "removed")
	layer.AddNode("covered.go", &Node{ID: "covered.go::new", Name: "New", FilePath: "covered.go"})
	layer.AddNode("", &Node{ID: "identity", Name: "New", FilePath: "outside.go"})
	base := &exactNamesVisitorStub{nodes: []*Node{
		{ID: "covered.go::old", Name: "Old"},
		{ID: "deleted.go::old", Name: "Old"},
		{ID: "identity", Name: "Old"},
		{ID: "removed", Name: "Old"},
		{ID: "visible.go::old", Name: "Old"},
	}}
	view := NewOverlaidViewWithLayer(base, layer)
	var got []string
	err := VisitNodesByNamesContext(context.Background(), view, []string{"New", "Old"}, func(n *Node) bool { got = append(got, n.ID); return true })
	sort.Strings(got)
	if err != nil || !reflect.DeepEqual(got, []string{"covered.go::new", "identity", "visible.go::old"}) || base.calls != 1 {
		t.Fatalf("batch = %v calls=%d err=%v", got, base.calls, err)
	}
	base.calls = 0
	if err := VisitNodesByNamesContext(context.Background(), view, []string{"New", "Old"}, func(*Node) bool { return false }); err != nil || base.calls != 0 {
		t.Fatalf("overlay stop calls=%d err=%v", base.calls, err)
	}
}

func TestExactNamesVisitorFallbackAndErrorAuthority(t *testing.T) {
	base := &exactVisitorReaderStub{nodes: []*Node{{ID: "match"}}}
	if err := VisitNodesByNamesContext(context.Background(), base, []string{"One", "Two"}, func(*Node) bool { return false }); err != nil || base.visits != 1 {
		t.Fatalf("fallback stop visits=%d err=%v", base.visits, err)
	}
	boom := errors.New("partial batch failure")
	batch := &exactNamesVisitorStub{nodes: []*Node{{ID: "partial"}}, err: boom}
	if err := VisitNodesByNamesContext(context.Background(), batch, []string{"One", "Two"}, func(*Node) bool { return true }); !errors.Is(err, boom) {
		t.Fatalf("batch error = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	batch.err = nil
	if err := VisitNodesByNamesContext(ctx, batch, []string{"One", "Two"}, func(*Node) bool { cancel(); return true }); !errors.Is(err, context.Canceled) {
		t.Fatalf("callback cancellation = %v", err)
	}
}

type contextualBatchFallbackLayer struct {
	*OverlayLayer
	err         error
	legacyCalls int
}

func (l *contextualBatchFallbackLayer) NodesByName(string) []*Node { l.legacyCalls++; return nil }
func (l *contextualBatchFallbackLayer) NodesByNameContext(context.Context, string) ([]*Node, error) {
	return []*Node{{ID: "partial"}}, l.err
}

func TestExactNamesVisitorContextualLayerFallback(t *testing.T) {
	boom := errors.New("contextual layer error")
	layer := &contextualBatchFallbackLayer{OverlayLayer: NewOverlayLayer(), err: boom}
	base := &exactNamesVisitorStub{}
	view := NewOverlaidViewWithLayer(base, layer)
	seen := 0
	err := VisitNodesByNamesContext(context.Background(), view, []string{"One", "Two"}, func(*Node) bool { seen++; return true })
	if !errors.Is(err, boom) || seen != 0 || layer.legacyCalls != 0 || base.calls != 0 {
		t.Fatalf("layer fallback seen=%d legacy=%d base=%d err=%v", seen, layer.legacyCalls, base.calls, err)
	}
}
