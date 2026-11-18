package store_sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

func recordedEdgeTokens(edges []*graph.Edge) []string {
	out := make([]string, 0, len(edges))
	for _, e := range edges {
		if e == nil {
			continue
		}
		out = append(out, fmt.Sprintf("%+v", *e))
	}
	slices.Sort(out)
	return out
}

// TestRecordedEdgesAtMatchesTheFullRowReaders: the by-file reader returns, on
// each generation handle, exactly the full rows AllEdges returns for those
// recording files — every column, the structural read filter included — and
// nothing from another generation.
func TestRecordedEdgesAtMatchesTheFullRowReaders(t *testing.T) {
	s, _ := openTempStore(t)
	writeEndpointFixture(t, s)
	for _, generation := range endpointFixtureGenerations {
		h := s.AtGeneration(generation)
		for _, paths := range [][]string{{"pkg/f0.go"}, {"pkg/f0.go", "pkg/other.go"}, {"pkg/f1.go", "pkg/f2.go", "", "pkg/f1.go"}, {"pkg/absent.go"}} {
			want := map[string]struct{}{}
			for _, p := range paths {
				want[p] = struct{}{}
			}
			var full []*graph.Edge
			for _, e := range h.AllEdges() {
				if _, ok := want[e.FilePath]; ok {
					full = append(full, e)
				}
			}
			got, wantRows := recordedEdgeTokens(h.RecordedEdgesAt(paths)), recordedEdgeTokens(full)
			if !slices.Equal(got, wantRows) {
				t.Errorf("generation %d paths %v:\n got  %v\n want %v", generation, paths, got, wantRows)
			}
		}
	}
	if got := s.AtGeneration(0).RecordedEdgesAt(nil); got != nil {
		t.Errorf("no paths returned %d rows", len(got))
	}
}

// TestRecordedEdgesAtPlanLock: the by-file read seeks edges_by_file and never
// scans the table or the generation index, in the same three statistics
// regimes as the endpoint projections' plan lock.
func TestRecordedEdgesAtPlanLock(t *testing.T) {
	s, _ := openTempStore(t)
	writeEndpointFixture(t, s)
	for _, generation := range endpointFixtureGenerations {
		var edges []*graph.Edge
		for f := 0; f < 400; f++ {
			for e := 0; e < 6; e++ {
				edges = append(edges, &graph.Edge{
					From: fmt.Sprintf("spread/f%03d.go::S%d", f, e%3), To: fmt.Sprintf("spread/f%03d.go::T%d", (f+1)%400, e),
					Kind: graph.EdgeCalls, FilePath: fmt.Sprintf("spread/f%03d.go", f), Line: e + 1,
				})
			}
		}
		if err := s.AtGeneration(generation).AddBatchChecked(nil, edges); err != nil {
			t.Fatalf("write spread generation %d: %v", generation, err)
		}
	}
	assertLocked := func(t *testing.T, ctx context.Context, conn *sql.Conn) {
		t.Helper()
		for _, generation := range endpointFixtureGenerations {
			paths := []any{"pkg/f0.go", "pkg/f2.go"}
			q := recordedEdgesAtPrefix + inPlaceholders(len(paths)) + recordedEdgesAtSuffix
			plan := strings.Join(explainOnConn(t, ctx, conn, q, append(paths, generation)...), "\n")
			if !strings.Contains(plan, "USING INDEX edges_by_file (file_path=?)") {
				t.Errorf("generation %d: plan does not seek edges_by_file:\n%s", generation, plan)
			}
			for _, bad := range []string{"SCAN edges", "edges_by_generation"} {
				if strings.Contains(plan, bad) {
					t.Errorf("generation %d: plan uses %q:\n%s", generation, bad, plan)
				}
			}
		}
	}
	t.Run("no_stats", func(t *testing.T) {
		withReceiverPlanLockWriter(t, s, func(ctx context.Context, conn *sql.Conn) {
			assertLocked(t, ctx, conn)
		})
	})
	t.Run("production_rows", func(t *testing.T) {
		withReceiverPlanLockWriter(t, s, func(ctx context.Context, conn *sql.Conn) {
			if _, err := conn.ExecContext(ctx, `ANALYZE`); err != nil {
				t.Fatalf("create sqlite_stat1: %v", err)
			}
			if _, err := conn.ExecContext(ctx, `DELETE FROM sqlite_stat1 WHERE tbl IN ('edges', 'nodes')`); err != nil {
				t.Fatalf("clear stat rows: %v", err)
			}
			for _, row := range receiverMutationCallStatRows {
				if _, err := conn.ExecContext(ctx, `INSERT INTO sqlite_stat1(tbl, idx, stat) VALUES (?, ?, ?)`, row[0], row[1], row[2]); err != nil {
					t.Fatalf("insert stat row %v: %v", row, err)
				}
			}
			if _, err := conn.ExecContext(ctx, `ANALYZE sqlite_schema`); err != nil {
				t.Fatalf("reload statistics: %v", err)
			}
			assertLocked(t, ctx, conn)
		})
	})
	t.Run("refreshed", func(t *testing.T) {
		withReceiverPlanLockWriter(t, s, func(ctx context.Context, conn *sql.Conn) {
			if _, err := conn.ExecContext(ctx, `DELETE FROM sqlite_stat1`); err != nil {
				t.Fatalf("clear stat rows: %v", err)
			}
			if _, err := conn.ExecContext(ctx, `ANALYZE`); err != nil {
				t.Fatalf("refresh statistics: %v", err)
			}
			assertLocked(t, ctx, conn)
		})
	})
}
