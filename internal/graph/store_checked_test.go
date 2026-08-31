package graph

import (
	"errors"
	"testing"
)

type checkedBatchTestStore struct {
	Store
	wantErr      error
	checkedCalls int
	legacyCalls  int
	nodes        []*Node
	edges        []*Edge
}

func (s *checkedBatchTestStore) AddBatch(nodes []*Node, edges []*Edge) {
	s.legacyCalls++
}

func (s *checkedBatchTestStore) AddBatchChecked(nodes []*Node, edges []*Edge) error {
	s.checkedCalls++
	s.nodes = nodes
	s.edges = edges
	return s.wantErr
}

type legacyBatchTestStore struct {
	Store
	legacyCalls int
}

func (s *legacyBatchTestStore) AddBatch(nodes []*Node, edges []*Edge) {
	s.legacyCalls++
	s.Store.AddBatch(nodes, edges)
}

func TestAddBatchCheckedCapabilityDispatch(t *testing.T) {
	wantErr := errors.New("checked write failed")
	nodes := []*Node{{ID: "n", Kind: KindFunction}}
	edges := []*Edge{{From: "n", To: "m", Kind: EdgeCalls}}
	checked := &checkedBatchTestStore{Store: New(), wantErr: wantErr}
	if err := AddBatchChecked(checked, nodes, edges); !errors.Is(err, wantErr) {
		t.Fatalf("AddBatchChecked error = %v, want %v", err, wantErr)
	}
	if checked.checkedCalls != 1 || checked.legacyCalls != 0 {
		t.Fatalf("checked/legacy calls = %d/%d, want 1/0", checked.checkedCalls, checked.legacyCalls)
	}
	if len(checked.nodes) != 1 || checked.nodes[0] != nodes[0] ||
		len(checked.edges) != 1 || checked.edges[0] != edges[0] {
		t.Fatal("checked capability did not receive the original batch")
	}

	legacy := &legacyBatchTestStore{Store: New()}
	if err := AddBatchChecked(legacy, nodes, nil); err != nil {
		t.Fatalf("legacy AddBatchChecked fallback: %v", err)
	}
	if legacy.legacyCalls != 1 || legacy.NodeCount() != 1 {
		t.Fatalf("legacy calls/nodes = %d/%d, want 1/1", legacy.legacyCalls, legacy.NodeCount())
	}
}

func BenchmarkAddBatchCheckedDispatch(b *testing.B) {
	nodes := []*Node{{ID: "n", Kind: KindFunction}}
	edges := []*Edge{{From: "n", To: "m", Kind: EdgeCalls}}
	b.Run("checked", func(b *testing.B) {
		store := &checkedBatchTestStore{Store: New()}
		b.ReportAllocs()
		b.ResetTimer()
		for range b.N {
			if err := AddBatchChecked(store, nodes, edges); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("legacy", func(b *testing.B) {
		store := &legacyBatchTestStore{Store: New()}
		b.ReportAllocs()
		b.ResetTimer()
		for range b.N {
			if err := AddBatchChecked(store, nil, nil); err != nil {
				b.Fatal(err)
			}
		}
	})
}
