package store_sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/graph"
)

func TestIncomingSourceScopedProjectionStopsAfterDistinctSentinel(t *testing.T) {
	s, handle, id := newIncomingCandidateStore(t)
	const target = "repo/target.go::Target"
	edges := make([]*graph.Edge, graph.MaxIncomingSourceCandidateRows)
	for i := range edges {
		edges[i] = &graph.Edge{From: fmt.Sprintf("repo/source.go::S%05d", i), To: target, Kind: graph.EdgeKind("calls"), FilePath: "repo/source.go", Line: i + 1}
	}
	handle.AddBatch(nil, edges)
	s.AddBatch(nil, []*graph.Edge{{From: "gen0-poison", To: target, Kind: graph.EdgeKind("calls"), FilePath: "poison.go", Line: 1}})
	if err := s.PublishPayloadGeneration(t.Context(), id, 2); err != nil {
		t.Fatal(err)
	}
	s.db.SetMaxOpenConns(1)
	budget := &graph.IncomingSourceBudget{}
	page, err := handle.FindIncomingSourcesScoped(t.Context(), []string{target}, graph.EdgeKind("calls"), 1, graph.IncomingSourceScope{}, budget)
	if err != nil || !page.Truncated[target] || len(page.Sources[target]) != 0 {
		t.Fatalf("distinct sentinel: %+v %v", page, err)
	}
	if got := graph.MaxIncomingSourceCandidateRows - budget.Remaining(); got != 2 {
		t.Fatalf("small query read %d matching rows, want exactly2", got)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	var one int
	if err := s.db.QueryRowContext(ctx, "SELECT 1").Scan(&one); err != nil || one != 1 {
		t.Fatalf("early-stop cursor/tx leaked: %d %v", one, err)
	}
	if handle.EdgeCount() != len(edges) {
		t.Fatal("query changed physical input")
	}
}

func TestIncomingSourceScopedPageUsesIncomingRowIDSeek(t *testing.T) {
	s, _, id := newIncomingCandidateStore(t)
	rows, err := s.db.QueryContext(t.Context(), "EXPLAIN QUERY PLAN "+scopedIncomingSourcePageSQL, "target", "calls", int64(0), id, 2)
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
	if !strings.Contains(joined, "SEARCH") || !strings.Contains(joined, "edges_by_to") || !strings.Contains(joined, "rowid>?") || strings.Contains(joined, "TEMP B-TREE") {
		t.Fatalf("keyset is not an ordered incoming-index seek: %s", joined)
	}
	t.Log(joined)
}

func TestIncomingSourceScopedProjectionSharesBudgetAndReturnsNoPartial(t *testing.T) {
	s, handle, id := newIncomingCandidateStore(t)
	handle.AddBatch(nil, []*graph.Edge{
		{From: "a", To: "target", Kind: graph.EdgeKind("calls"), FilePath: "a.go", Line: 1},
		{From: "b", To: "target", Kind: graph.EdgeKind("calls"), FilePath: "b.go", Line: 2},
	})
	if err := s.PublishPayloadGeneration(t.Context(), id, 2); err != nil {
		t.Fatal(err)
	}
	budget := &graph.IncomingSourceBudget{}
	if err := budget.Charge(graph.MaxIncomingSourceCandidateRows - 1); err != nil {
		t.Fatal(err)
	}
	page, err := handle.FindIncomingSourcesScoped(t.Context(), []string{"target"}, graph.EdgeKind("calls"), 1, graph.IncomingSourceScope{}, budget)
	var limitErr *graph.BoundedLocalizationLimitError
	if !errors.As(err, &limitErr) || limitErr.Resource != "incoming-source candidate inspections" || len(page.Sources) != 0 || len(page.Truncated) != 0 {
		t.Fatalf("combined budget produced partial/truncated success: %+v %v", page, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	page, err = handle.FindIncomingSourcesScoped(ctx, []string{"target"}, graph.EdgeKind("calls"), 1, graph.IncomingSourceScope{}, nil)
	if !errors.Is(err, context.Canceled) || len(page.Sources) != 0 || len(page.Truncated) != 0 {
		t.Fatalf("cancel partial: %+v %v", page, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	page, err = handle.FindIncomingSourcesScoped(t.Context(), []string{"target"}, graph.EdgeKind("calls"), 1, graph.IncomingSourceScope{}, nil)
	if err == nil || len(page.Sources) != 0 || len(page.Truncated) != 0 {
		t.Fatalf("closed-store partial: %+v %v", page, err)
	}
}

func TestIncomingSourceNodeQueryUsesExactTransactionAndGeneration(t *testing.T) {
	s, handle, id := newIncomingCandidateStore(t)
	handle.AddNode(&graph.Node{ID: "present", Name: "Present"})
	s.AddNode(&graph.Node{ID: "gen0-only", Name: "Poison"})
	if err := s.PublishPayloadGeneration(t.Context(), id, 2); err != nil {
		t.Fatal(err)
	}
	s.db.SetMaxOpenConns(1)
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := readScopedIncomingSourcePage(ctx, tx, "target", graph.EdgeKind("calls"), id, 0, 2, nil); err != nil {
		t.Fatal(err)
	}
	query := incomingSourceTxQuery{tx: tx, db: s.db}
	for _, tc := range []struct {
		id   string
		want bool
	}{{"present", true}, {"gen0-only", false}, {"absent", false}} {
		exists, err := handle.IncomingSourceNodeExists(ctx, tc.id, query)
		if err != nil || exists != tc.want {
			t.Fatalf("same-tx %s=%v want%v: %v", tc.id, exists, tc.want, err)
		}
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if exists, err := handle.IncomingSourceNodeExists(ctx, "present", query); err == nil || exists {
		t.Fatalf("failed exact transaction fell back to nullable/global lookup: %v %v", exists, err)
	}
	if exists, err := handle.IncomingSourceNodeExists(ctx, "present", nil); err != nil || !exists {
		t.Fatalf("transaction did not drain: %v %v", exists, err)
	}
}

// Test-only external-package adapter: no production API or non-test overlay.
// It lets the public GenerationLayer composition test constrain the SAME pool.
func SetIncomingSourceReaderPoolSizeForTest(s *Store, size int) { s.db.SetMaxOpenConns(size) }
