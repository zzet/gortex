package graph

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

type maskedBFSReader struct{ Reader }

func (r maskedBFSReader) GetOutEdges(id string) []*Edge {
	var out []*Edge
	for _, edge := range r.Reader.GetOutEdges(id) {
		if edge.Meta["hidden"] != true {
			out = append(out, edge)
		}
	}
	return out
}

func TestBFSOfHonorsSelectedAdjacencyAndCancellation(t *testing.T) {
	g := New()
	const root, visible, hidden = "a/f.go::Root", "a/f.go::Visible", "a/f.go::Hidden"
	for _, id := range []string{root, visible, hidden} {
		g.AddNode(&Node{ID: id, Kind: KindFunction, FilePath: "a/f.go"})
	}
	g.AddEdge(&Edge{From: root, To: visible, Kind: EdgeCalls})
	g.AddEdge(&Edge{From: visible, To: root, Kind: EdgeCalls})
	g.AddEdge(&Edge{From: root, To: hidden, Kind: EdgeCalls, Meta: map[string]any{"hidden": true}})
	got, err := BFSOf(t.Context(), maskedBFSReader{g}, []string{root}, DirectionForward, []EdgeKind{EdgeCalls}, 3, 10)
	want := []BFSHop{{NodeID: root, Depth: 0}, {NodeID: visible, Depth: 1, ParentID: root, EdgeKind: EdgeCalls}}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("selected BFS=%+v error=%v want=%+v", got, err, want)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := BFSOf(ctx, g, []string{root}, DirectionForward, []EdgeKind{EdgeCalls}, 3, 10); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled BFS lost cause: %v", err)
	}
}

func TestBFSAdmissionReaderRetainsPayloadAndDetachedOrder(t *testing.T) {
	seed := &Node{ID: "a/f.go::Root", Meta: map[string]any{"signature": "func Root()"}}
	child := &Node{ID: "a/g.go::Child", Meta: map[string]any{"signature": "func Child()"}}
	edge := &Edge{From: seed.ID, To: child.ID, Kind: EdgeCalls, Line: 9, Meta: map[string]any{"payload": "retained"}}
	snapshot := BFSAdmissionSnapshot{Nodes: map[string]*Node{child.ID: child}, Adjacency: map[string]KindAdjacency{seed.ID: {Edges: []*Edge{edge}, Total: 7}}}
	reader := NewBFSAdmissionReader(snapshot, seed, DirectionForward)
	if reader.GetNode(seed.ID) != seed || reader.GetNode(child.ID) != child || reader.GetNode("outside") != nil {
		t.Fatal("snapshot lost payload or consulted an unrelated identity")
	}
	rows, err := ReadKindAdjacency(t.Context(), reader, seed.ID, DirectionForward, []EdgeKind{EdgeCalls})
	if err != nil || rows.Total != 7 || len(rows.Edges) != 1 || rows.Edges[0] != edge {
		t.Fatalf("snapshot adjacency=%+v error=%v", rows, err)
	}
	rows.Edges[0] = nil
	if snapshot.Adjacency[seed.ID].Edges[0] != edge {
		t.Fatal("admission mutated the stored adjacency slice")
	}
	for _, request := range []BFSAdmissionRequest{{Seed: seed.ID, Depth: 17, Limit: 10, Kinds: []EdgeKind{EdgeCalls}}, {Seed: seed.ID, Depth: 2, Limit: 257, Kinds: []EdgeKind{EdgeCalls}}} {
		if _, _, supported := BFSAdmissionBudgets(request); supported {
			t.Fatal("unsupported broad request was accepted")
		}
	}
}
