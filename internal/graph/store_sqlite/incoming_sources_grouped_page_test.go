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

func TestIncomingSourceGroupedPageKeepsRawBoundOrderAndProvenance(t *testing.T) {
	s, handle, generation := newIncomingCandidateStore(t)
	const target = "target"
	input := []graph.IncomingSourceCandidate{
		{From: "z", FilePath: "shared.go"},
		{From: "a", FilePath: "shared.go"},
		{From: "z", FilePath: "other.go"},
		{From: "z", FilePath: "shared.go"},
		{From: "z", FilePath: "shared.go"},
		{From: "a", FilePath: "shared.go"},
		{From: "after-bound", FilePath: "tail.go"},
	}
	for i, row := range input {
		handle.AddBatch(nil, []*graph.Edge{{From: row.From, To: target, Kind: graph.EdgeKind("calls"), FilePath: row.FilePath, Line: i + 1}})
	}
	s.AddBatch(nil, []*graph.Edge{{From: "gen0-poison", To: target, Kind: graph.EdgeKind("calls"), FilePath: "poison.go", Line: 1}})
	if err := s.PublishPayloadGeneration(t.Context(), generation, 2); err != nil {
		t.Fatal(err)
	}
	tx, err := s.db.BeginTx(t.Context(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	raw, err := readScopedIncomingSourcePage(t.Context(), tx, target, graph.EdgeKind("calls"), generation, 0, 6, nil)
	if err != nil || len(raw) != 6 {
		t.Fatalf("raw fixture: %+v %v", raw, err)
	}
	page, count, next, err := readScopedIncomingSourceGroupedPage(t.Context(), tx, target, graph.EdgeKind("calls"), generation, 0, 6, nil)
	if err != nil || count != 6 || len(page) != 3 || next != raw[len(raw)-1].id {
		t.Fatalf("grouped bound: %+v count=%d next=%d err=%v", page, count, next, err)
	}
	seen := make(map[graph.IncomingSourceCandidate]bool)
	var ordered []graph.IncomingSourceCandidate
	for _, row := range raw {
		if !seen[row.candidate] {
			seen[row.candidate] = true
			ordered = append(ordered, row.candidate)
		}
	}
	for i, row := range page {
		if row.candidate != ordered[i] {
			t.Fatalf("group %d changed first-occurrence/provenance order: %+v want %+v", i, row.candidate, ordered[i])
		}
	}
	if page[len(page)-1].id == next {
		t.Fatal("fixture must distinguish maximum page cursor from last group's maximum id")
	}
	tail, count, tailNext, err := readScopedIncomingSourceGroupedPage(t.Context(), tx, target, graph.EdgeKind("calls"), generation, next, 6, page)
	if err != nil || count != 1 || len(tail) != 1 || tail[0].candidate.From != "after-bound" || tailNext <= next {
		t.Fatalf("group continuation lost/skipped raw rows: %+v %d %d %v", tail, count, tailNext, err)
	}
}

func TestIncomingSourceGroupedPageDoesNotTreatOneGroupAsEOF(t *testing.T) {
	s, handle, generation := newIncomingCandidateStore(t)
	const target = "target"
	edges := make([]*graph.Edge, 300)
	for i := range edges {
		edges[i] = &graph.Edge{From: "source", To: target, Kind: graph.EdgeKind("calls"), FilePath: "source.go", Line: i + 1}
	}
	handle.AddBatch(nil, edges)
	if err := s.PublishPayloadGeneration(t.Context(), generation, 2); err != nil {
		t.Fatal(err)
	}
	s.db.SetMaxOpenConns(1)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	page, count, next, err := readScopedIncomingSourceGroupedPage(ctx, tx, target, graph.EdgeKind("calls"), generation, 0, 256, nil)
	if err != nil || count != 256 || len(page) != 1 {
		t.Fatalf("first physical page: %+v %d %v", page, count, err)
	}
	page, count, _, err = readScopedIncomingSourceGroupedPage(ctx, tx, target, graph.EdgeKind("calls"), generation, next, 256, page)
	if err != nil || count != 44 || len(page) != 1 {
		t.Fatalf("second physical page: %+v %d %v", page, count, err)
	}
	// This statement reuses the held transaction after grouped rows are closed.
	var one int
	if err := tx.QueryRowContext(ctx, "SELECT 1").Scan(&one); err != nil || one != 1 {
		t.Fatalf("group cursor was not released: %d %v", one, err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	budget := &graph.IncomingSourceBudget{}
	projection, err := handle.FindIncomingSourcesScoped(ctx, []string{target}, graph.EdgeKind("calls"), 1, graph.IncomingSourceScope{}, budget)
	if err != nil || projection.Truncated[target] || len(projection.Sources[target]) != 1 || projection.Sources[target][0] != "source" {
		t.Fatalf("hybrid projection: %+v %v", projection, err)
	}
	if got := graph.MaxIncomingSourceCandidateRows - budget.Remaining(); got != len(edges) {
		t.Fatalf("charged groups instead of all physical sites: %d want %d", got, len(edges))
	}
}

func TestIncomingSourceGroupedPagePreservesEagerInterleavedBudgetFailure(t *testing.T) {
	for _, capacity := range []int{4, 5} {
		t.Run(fmt.Sprint(capacity), func(t *testing.T) {
			s, handle, generation := newIncomingCandidateStore(t)
			for i, source := range []string{"A", "A", "A", "B", "A"} {
				handle.AddBatch(nil, []*graph.Edge{{From: source, To: "target", Kind: graph.EdgeKind("calls"), FilePath: "source.go", Line: i + 1}})
			}
			if err := s.PublishPayloadGeneration(t.Context(), generation, 2); err != nil {
				t.Fatal(err)
			}
			budget := &graph.IncomingSourceBudget{}
			if err := budget.Charge(graph.MaxIncomingSourceCandidateRows - capacity); err != nil {
				t.Fatal(err)
			}
			projection, err := handle.FindIncomingSourcesScoped(t.Context(), []string{"target"}, graph.EdgeKind("calls"), 1, graph.IncomingSourceScope{}, budget)
			if capacity == 4 {
				// First A,A raw page consumes2. Grouped A,B,A must charge all3
				// BEFORE B can establish a distinct sentinel, matching NkwcA3.
				var bounded *graph.BoundedLocalizationLimitError
				if !errors.As(err, &bounded) || bounded.Resource != "incoming-source candidate inspections" || len(projection.Sources) != 0 || len(projection.Truncated) != 0 {
					t.Fatalf("eager page became partial/prefix success: %+v %v", projection, err)
				}
			} else if err != nil || !projection.Truncated["target"] || len(projection.Sources["target"]) != 0 || budget.Remaining() != 0 {
				t.Fatalf("five-row charged sentinel control: %+v remaining=%d %v", projection, budget.Remaining(), err)
			}
		})
	}
}

func TestIncomingSourceGroupedPagePreservesMatchingWorkLimit(t *testing.T) {
	for _, count := range []int{graph.MaxIncomingSourceCandidateRows, graph.MaxIncomingSourceCandidateRows + 1} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			s, handle, generation := newIncomingCandidateStore(t)
			edges := make([]*graph.Edge, count)
			for i := range edges {
				edges[i] = &graph.Edge{From: "source", To: "target", Kind: graph.EdgeKind("calls"), FilePath: "source.go", Line: i + 1}
			}
			handle.AddBatch(nil, edges)
			if err := s.PublishPayloadGeneration(t.Context(), generation, 2); err != nil {
				t.Fatal(err)
			}
			budget := &graph.IncomingSourceBudget{}
			projection, err := handle.FindIncomingSourcesScoped(t.Context(), []string{"target"}, graph.EdgeKind("calls"), 1, graph.IncomingSourceScope{}, budget)
			if count > graph.MaxIncomingSourceCandidateRows {
				var bounded *graph.BoundedLocalizationLimitError
				if !errors.As(err, &bounded) || len(projection.Sources) != 0 || len(projection.Truncated) != 0 {
					t.Fatalf("group count bypassed raw work bound: %+v %v", projection, err)
				}
			} else if err != nil || projection.Truncated["target"] || len(projection.Sources["target"]) != 1 || budget.Remaining() != 0 {
				t.Fatalf("exact work bound control: %+v %d %v", projection, budget.Remaining(), err)
			}
		})
	}
}

func TestIncomingSourceGroupedPageErrorsReturnNoAccountingOrPartialRows(t *testing.T) {
	s, _, generation := newIncomingCandidateStore(t)
	tx, err := s.db.BeginTx(t.Context(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	page, count, next, err := readScopedIncomingSourceGroupedPage(ctx, tx, "target", graph.EdgeKind("calls"), generation, 7, 2, nil)
	if !errors.Is(err, context.Canceled) || page != nil || count != 0 || next != 7 {
		t.Fatalf("cancel returned partial group state: %+v %d %d %v", page, count, next, err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	page, count, next, err = readScopedIncomingSourceGroupedPage(t.Context(), tx, "target", graph.EdgeKind("calls"), generation, 7, 2, nil)
	if !errors.Is(err, sql.ErrTxDone) || page != nil || count != 0 || next != 7 {
		t.Fatalf("failed tx returned partial group state: %+v %d %d %v", page, count, next, err)
	}
}

func TestIncomingSourceGroupedPageRecordsBoundedIndexPlan(t *testing.T) {
	s, _, generation := newIncomingCandidateStore(t)
	rows, err := s.db.QueryContext(t.Context(), "EXPLAIN QUERY PLAN "+scopedIncomingSourceGroupedPageSQL, "target", "calls", int64(0), generation, 256)
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
	if !strings.Contains(joined, "SEARCH") || !strings.Contains(joined, "edges_by_to") || !strings.Contains(joined, "rowid>?") {
		t.Fatalf("grouped page lost inner incoming keyset seek: %s", joined)
	}
	// A bounded outer GROUP/ORDER temporary B-tree is expected; this test does
	// not mislabel it as a full-query no-sort guarantee or count VM visits.
	t.Log(joined)
}
