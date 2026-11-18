package store_sqlite

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

func TestGetNodeContextHonorsCancellation(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "graph.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := s.GetNodeContext(ctx, "missing"); !errors.Is(err, context.Canceled) {
		t.Fatalf("GetNodeContext error = %v, want context.Canceled", err)
	}
}

func TestGetNodesByIDsContextHonorsCancellation(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "graph.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := s.GetNodesByIDsContext(ctx, []string{"missing"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("GetNodesByIDsContext error = %v, want context.Canceled", err)
	}
}

func TestGetOutEdgesByNodeIDsContextHonorsCancellation(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "graph.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, _, err := s.GetOutEdgesByNodeIDsContext(ctx, []string{"missing"}, 1); !errors.Is(err, context.Canceled) {
		t.Fatalf("GetOutEdgesByNodeIDsContext error = %v, want context.Canceled", err)
	}
}

// The metadata-carrying bounded read returns the rows and truncation the plain
// bounded read does, with each edge's metadata decoded.
func TestGetInEdgesByNodeIDsWithMetaContextKeepsBoundAndMetadata(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "graph.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	s.AddBatch([]*graph.Node{
		{ID: "seed", Name: "seed", Kind: graph.KindFunction},
		{ID: "a", Name: "a", Kind: graph.KindFunction},
		{ID: "b", Name: "b", Kind: graph.KindFunction},
		{ID: "c", Name: "c", Kind: graph.KindFunction},
	}, []*graph.Edge{
		{From: "a", To: "seed", Kind: graph.EdgeCalls, Line: 1, Meta: map[string]any{"via": "spring.Bean"}},
		{From: "b", To: "seed", Kind: graph.EdgeCalls, Line: 2},
		{From: "c", To: "seed", Kind: graph.EdgeCalls, Line: 3},
	})

	for _, limit := range []int{2, 3} {
		light, lightLimited, err := s.GetInEdgesByNodeIDsContext(context.Background(), []string{"seed"}, limit)
		if err != nil {
			t.Fatal(err)
		}
		full, fullLimited, err := s.GetInEdgesByNodeIDsWithMetaContext(context.Background(), []string{"seed"}, limit)
		if err != nil {
			t.Fatal(err)
		}
		if fullLimited != lightLimited || len(full["seed"]) != len(light["seed"]) {
			t.Fatalf("limit %d: with metadata %d rows limited=%v, plain %d rows limited=%v",
				limit, len(full["seed"]), fullLimited, len(light["seed"]), lightLimited)
		}
		if wantLimited := limit < 3; fullLimited != wantLimited {
			t.Fatalf("limit %d: limited=%v, want %v", limit, fullLimited, wantLimited)
		}
	}
	full, _, err := s.GetInEdgesByNodeIDsWithMetaContext(context.Background(), []string{"seed"}, 3)
	if err != nil {
		t.Fatal(err)
	}
	var via []string
	for _, edge := range full["seed"] {
		if v, ok := edge.Meta["via"].(string); ok {
			via = append(via, edge.From+"="+v)
		}
	}
	if len(via) != 1 || via[0] != "a=spring.Bean" {
		t.Fatalf("decoded metadata = %v, want [a=spring.Bean]", via)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := s.GetInEdgesByNodeIDsWithMetaContext(ctx, []string{"seed"}, 1); !errors.Is(err, context.Canceled) {
		t.Fatalf("GetInEdgesByNodeIDsWithMetaContext error = %v, want context.Canceled", err)
	}
}
