package store_sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

// writeDeadCodeFixture writes, per generation, functions/methods/types where
// some are referenced (by a call or an import) and some are not.
func writeDeadCodeFixture(t *testing.T, s *Store, generations []int64, perGen int) {
	t.Helper()
	for _, generation := range generations {
		var nodes []*graph.Node
		var edges []*graph.Edge
		for i := 0; i < perGen; i++ {
			kind := []graph.NodeKind{graph.KindFunction, graph.KindMethod, graph.KindType}[i%3]
			file := fmt.Sprintf("repo/f%02d.go", i%9)
			id := fmt.Sprintf("%s::N%04d", file, i)
			nodes = append(nodes, &graph.Node{ID: id, Kind: kind, Name: fmt.Sprintf("N%d", i), FilePath: file, RepoPrefix: "repo", Language: "go"})
			switch i % 4 {
			case 0: // called
				edges = append(edges, &graph.Edge{From: fmt.Sprintf("repo/f%02d.go::N%04d", (i+1)%9, (i+1)%perGen), To: id, Kind: graph.EdgeCalls, FilePath: file, Line: i + 1})
			case 1: // only imported (counts only when any kind counts)
				edges = append(edges, &graph.Edge{From: "repo/main.go::main", To: id, Kind: graph.EdgeImports, FilePath: "repo/main.go", Line: i + 1})
			}
		}
		require.NoError(t, s.AtGeneration(generation).AddBatchChecked(nodes, edges))
	}
}

// legacyDeadCodeCandidates is the single-statement-per-kind reader the paged
// one replaced.
func legacyDeadCodeCandidates(s *Store, kinds []graph.NodeKind, allowedIn map[graph.NodeKind][]graph.EdgeKind) []*graph.Node {
	var out []*graph.Node
	for _, nk := range kinds {
		allowed := anaDedupeEdgeKinds(allowedIn[nk])
		var q string
		var args []any
		if len(allowed) == 0 {
			q = `SELECT ` + lookupNodeCols + ` FROM nodes n WHERE n.kind = ? AND NOT EXISTS (SELECT 1 FROM edges e WHERE e.to_id = n.id AND e.view_gen = n.view_gen) AND n.view_gen = ? ORDER BY n.id`
			args = []any{string(nk), s.viewGen}
		} else {
			q = `SELECT ` + lookupNodeCols + ` FROM nodes n WHERE n.kind = ? AND NOT EXISTS (SELECT 1 FROM edges e WHERE e.to_id = n.id AND e.kind IN (` + inPlaceholders(len(allowed)) + `) AND e.view_gen = n.view_gen) AND n.view_gen = ? ORDER BY n.id`
			args = append(args, string(nk))
			for _, ek := range allowed {
				args = append(args, string(ek))
			}
			args = append(args, s.viewGen)
		}
		out = append(out, s.queryNodesSQL(q, args...)...)
	}
	return out
}

// The paged DeadCodeCandidates returns exactly the single statements' rows
// in the same order: both verdict shapes, several pages, two generations.
func TestDeadCodeCandidatesPagesAndMatchesTheSingleStatements(t *testing.T) {
	prev := deadCodePageSize
	deadCodePageSize = 5
	t.Cleanup(func() { deadCodePageSize = prev })
	s, _ := openTempStore(t)
	generations := []int64{0, 4}
	writeDeadCodeFixture(t, s, generations, 60)
	kinds := []graph.NodeKind{graph.KindFunction, graph.KindMethod, graph.KindType}
	for _, generation := range generations {
		h := s.AtGeneration(generation)
		for name, allowed := range map[string]map[graph.NodeKind][]graph.EdgeKind{
			"any_edge_counts": nil,
			"calls_only":      {graph.KindFunction: {graph.EdgeCalls}, graph.KindMethod: {graph.EdgeCalls}, graph.KindType: {graph.EdgeCalls, graph.EdgeReferences}},
		} {
			got := h.DeadCodeCandidates(kinds, allowed)
			want := legacyDeadCodeCandidates(h, kinds, allowed)
			require.NotEmpty(t, want)
			require.True(t, reflect.DeepEqual(got, want), "generation %d %s: paged %d rows, single statements %d rows", generation, name, len(got), len(want))
		}
	}
}

// A handle bound to a request context stops a whole-store read within one
// page once the context ends and releases its WAL snapshot: the read returns
// promptly and a TRUNCATE (which no reader on the old snapshot may block)
// succeeds right after.
func TestAbandonedAnalysisReadReleasesItsSnapshot(t *testing.T) {
	prevNodes, prevDead := nodesByKindsPageSize, deadCodePageSize
	nodesByKindsPageSize, deadCodePageSize = 1, 1 // one row per page: the read runs long
	t.Cleanup(func() { nodesByKindsPageSize, deadCodePageSize = prevNodes, prevDead })
	s, path := openTempStore(t)
	writeDeadCodeFixture(t, s, []int64{0}, 3000)
	ckpt, err := openWALReclaimCheckpointDB(s.dbPath)
	require.NoError(t, err)
	defer ckpt.Close()

	for name, read := range map[string]func(g graph.Reader) error{
		"nodes_by_kinds": func(g graph.Reader) error {
			_, err := g.(*Store).NodesByKindsContext(g.(*Store).readContext(), []graph.NodeKind{graph.KindFunction, graph.KindMethod})
			return err
		},
		"dead_code_candidates": func(g graph.Reader) error {
			_, err := g.(*Store).DeadCodeCandidatesContext(g.(*Store).readContext(), []graph.NodeKind{graph.KindFunction, graph.KindMethod, graph.KindType}, nil)
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			growWALForReadTest(t, s)
			ctx, cancel := context.WithCancel(context.Background())
			bound := graph.BindReadContext(s, ctx)
			done := make(chan error, 1)
			go func() { done <- read(bound) }()
			time.Sleep(150 * time.Millisecond) // mid-read
			cancelled := time.Now()
			cancel()
			select {
			case err := <-done:
				require.True(t, errors.Is(err, context.Canceled), "err=%v", err)
			case <-time.After(5 * time.Second):
				t.Fatal("the bound read did not stop after its context ended")
			}
			stopped := time.Since(cancelled)
			_, terr := checkpointWALOnceOn(context.Background(), ckpt, "TRUNCATE")
			t.Logf("stopped %s after cancel; truncate err=%v wal=%d", stopped, terr, walFileSize(path+"-wal"))
			require.Less(t, stopped, time.Second, "the read held its snapshot past one page")
			require.NoError(t, terr, "a TRUNCATE must succeed once the abandoned read released its snapshot")
		})
	}
}

func growWALForReadTest(t *testing.T, s *Store) {
	t.Helper()
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_, err := s.writerDB.Exec(`UPDATE nodes SET updated_at = updated_at + 1 WHERE view_gen = 0`)
	require.NoError(t, err)
}

// The paged reads stay one keyset seek per page under every statistics
// regime (from the last id, on nodes_by_generation or nodes_by_kind), never a
// table scan or a temp sort of the generation.
func TestAnalysisPageReadsLockedAcrossStatisticsRegimes(t *testing.T) {
	s, _ := openTempStore(t)
	writeDeadCodeFixture(t, s, []int64{0, 4}, 90)
	assertLocked := func(t *testing.T, ctx context.Context, conn *sql.Conn) {
		t.Helper()
		plans := map[string][]string{
			"nodes_by_kinds": explainOnConn(t, ctx, conn, nodesByKindsPageSQL(2), int64(0), "", "function", "method", 10),
			"dead_verdict":   explainOnConn(t, ctx, conn, deadCodeVerdictPageSQL(0), int64(0), "", "function", 10),
			"dead_verdict_k": explainOnConn(t, ctx, conn, deadCodeVerdictPageSQL(1), "calls", int64(0), "", "function", 10),
		}
		for name, rows := range plans {
			plan := strings.Join(rows, "\n")
			// A keyset seek: nodes_by_generation (view_gen, id) or, with the
			// kind pinned, nodes_by_kind (kind, view_gen, id); either way the
			// page starts at the last id.
			if !strings.Contains(plan, "nodes_by_generation (view_gen=? AND id>?)") && !strings.Contains(plan, "nodes_by_kind (kind=? AND view_gen=? AND id>?)") {
				t.Errorf("%s: not a keyset seek:\n%s", name, plan)
			}
			for _, bad := range []string{"SCAN n", "SCAN nodes", "USE TEMP B-TREE"} {
				if strings.Contains(plan, bad) {
					t.Errorf("%s: plan uses %q:\n%s", name, bad, plan)
				}
			}
		}
	}
	t.Run("no_stats", func(t *testing.T) {
		withReceiverPlanLockWriter(t, s, func(ctx context.Context, conn *sql.Conn) { assertLocked(t, ctx, conn) })
	})
	t.Run("live_store_rows", func(t *testing.T) {
		withReceiverPlanLockWriter(t, s, func(ctx context.Context, conn *sql.Conn) {
			if _, err := conn.ExecContext(ctx, `ANALYZE`); err != nil {
				t.Fatalf("create sqlite_stat1: %v", err)
			}
			if _, err := conn.ExecContext(ctx, `DELETE FROM sqlite_stat1`); err != nil {
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
