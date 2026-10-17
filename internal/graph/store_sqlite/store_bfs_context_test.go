package store_sqlite

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

func bfsContextFixture(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "store.sqlite"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	// A chain long enough that the walk has real work to do.
	for i := range 40 {
		s.AddNode(&graph.Node{ID: fmt.Sprintf("n%02d", i), Kind: graph.KindFunction, Name: fmt.Sprintf("n%02d", i), FilePath: "a.go", Language: "go"})
	}
	for i := range 39 {
		s.AddEdge(&graph.Edge{From: fmt.Sprintf("n%02d", i), To: fmt.Sprintf("n%02d", i+1), Kind: graph.EdgeCalls, Confidence: 1, Origin: graph.OriginASTResolved})
	}
	return s
}

// TestBFSContextAnswersLikeBFSWhileTheRequestLives pins that binding the
// walk to a live request changes nothing about its answer.
func TestBFSContextAnswersLikeBFSWhileTheRequestLives(t *testing.T) {
	s := bfsContextFixture(t)
	for _, dir := range []graph.Direction{graph.DirectionForward, graph.DirectionBackward} {
		seed := "n00"
		if dir == graph.DirectionBackward {
			seed = "n39"
		}
		want, err := s.BFS([]string{seed}, dir, []graph.EdgeKind{graph.EdgeCalls}, 10, 25)
		if err != nil {
			t.Fatal(err)
		}
		got, err := s.BFSContext(context.Background(), []string{seed}, dir, []graph.EdgeKind{graph.EdgeCalls}, 10, 25)
		if err != nil {
			t.Fatal(err)
		}
		if len(want) == 0 || !reflect.DeepEqual(got, want) {
			t.Fatalf("direction %v: BFSContext = %v, BFS = %v", dir, got, want)
		}
	}
}

// TestBFSContextStopsWhenTheRequestEnds pins the abandonment contract: a walk
// whose request already ended runs no query and reports the request's end,
// never a partial hop set a caller could mistake for the answer.
func TestBFSContextStopsWhenTheRequestEnds(t *testing.T) {
	s := bfsContextFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	hops, err := s.BFSContext(ctx, []string{"n00"}, graph.DirectionForward, []graph.EdgeKind{graph.EdgeCalls}, 10, 25)
	if !errors.Is(err, context.Canceled) || hops != nil {
		t.Fatalf("cancelled BFSContext = %v, %v; want nil, context.Canceled", hops, err)
	}
}
