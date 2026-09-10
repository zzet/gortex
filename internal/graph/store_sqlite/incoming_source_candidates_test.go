package store_sqlite

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/graph"
)

func newIncomingCandidateStore(t testing.TB) (*Store, *Store, int64) {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "incoming-candidates.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	id, err := s.Catalog().CreateViewGeneration(t.Context(), ViewGeneration{OwnerKind: "dedicated_graph", GraphID: "incoming-storage-fixture", GenerationKind: "dedicated", TreeOID: "source", ConfigHash: "policy", State: ViewGenerationBuilding, CreatedAt: 1})
	if err != nil {
		t.Fatal(err)
	}
	return s, s.AtGeneration(id), id
}

func TestIncomingSourceCandidateProjectionKeepsGenerationAndRawMultiplicity(t *testing.T) {
	s, handle, id := newIncomingCandidateStore(t)
	const target = "repo/target.go::Target"
	edges := make([]*graph.Edge, 300)
	for i := range edges {
		edges[i] = &graph.Edge{From: "repo/source.go::Call", To: target, Kind: graph.EdgeKind("calls"), FilePath: "repo/calls.go", Line: i + 1}
	}
	handle.AddBatch(nil, edges)
	s.AddBatch(nil, []*graph.Edge{{From: "gen0-poison", To: target, Kind: graph.EdgeKind("calls"), FilePath: "poison.go", Line: 1}})
	if err := s.PublishPayloadGeneration(t.Context(), id, 2); err != nil {
		t.Fatal(err)
	}
	// A leaked cursor or read transaction would block the post-query read.
	s.db.SetMaxOpenConns(1)
	rows, err := handle.ReadIncomingSourceCandidates(t.Context(), []string{target, target}, graph.EdgeKind("calls"))
	if err != nil || len(rows[target]) != 300 {
		t.Fatalf("candidate multiplicity: count=%d err=%v", len(rows[target]), err)
	}
	for _, row := range rows[target] {
		if row.From != "repo/source.go::Call" || row.FilePath != "repo/calls.go" {
			t.Fatalf("wrong generation/provenance: %+v", row)
		}
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	var one int
	if err := s.db.QueryRowContext(ctx, "SELECT 1").Scan(&one); err != nil || one != 1 {
		t.Fatalf("candidate cursor/transaction did not drain: %d %v", one, err)
	}
}

func TestIncomingSourceCandidateProjectionErrorsNeverReturnPartialRows(t *testing.T) {
	s, handle, _ := newIncomingCandidateStore(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	rows, err := handle.ReadIncomingSourceCandidates(ctx, []string{"target"}, graph.EdgeKind("calls"))
	if !errors.Is(err, context.Canceled) || rows != nil {
		t.Fatalf("cancel partial: %+v %v", rows, err)
	}
	keys := make([]string, graph.MaxBoundedAdjacencyKeys+1)
	for i := range keys {
		keys[i] = strings.Repeat("x", i+1)
	}
	rows, err = handle.ReadIncomingSourceCandidates(t.Context(), keys, graph.EdgeKind("calls"))
	var limitErr *graph.BoundedLocalizationLimitError
	if !errors.As(err, &limitErr) || rows != nil {
		t.Fatalf("key-bound partial: %+v %v", rows, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	rows, err = handle.ReadIncomingSourceCandidates(t.Context(), []string{"target"}, graph.EdgeKind("calls"))
	if err == nil || rows != nil {
		t.Fatalf("closed-store partial: %+v %v", rows, err)
	}
}

func TestIncomingSourceCandidateProjectionUsesActualIncomingIndex(t *testing.T) {
	s, _, id := newIncomingCandidateStore(t)
	rows, err := s.db.QueryContext(t.Context(), "EXPLAIN QUERY PLAN "+readIncomingSourceCandidatesSQL, "target", "calls", id, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var plan []string
	for rows.Next() {
		var node, parent, unused int
		var detail string
		if err := rows.Scan(&node, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(plan, "\n")
	if !strings.Contains(joined, "SEARCH") || !strings.Contains(joined, "edges_by_to") {
		t.Fatalf("unexpected incoming plan: %s", joined)
	}
}
