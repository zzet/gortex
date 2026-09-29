package store_sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/graph"
)

// writeFileHistoryFixture writes one file's edges into several generations
// (a file with a long history, as on the live store) plus other files.
func writeFileHistoryFixture(t *testing.T, s *Store) []int64 {
	t.Helper()
	generations := []int64{0, 3, 4, 5, 6}
	for _, generation := range generations {
		var edges []*graph.Edge
		for f := 0; f < 40; f++ {
			file := fmt.Sprintf("repo/pkg/f%02d.go", f)
			for e := 0; e < 8; e++ {
				edges = append(edges, &graph.Edge{
					From: fmt.Sprintf("%s::S%d", file, e%3), To: fmt.Sprintf("repo/pkg/f%02d.go::T%d", (f+1)%40, e),
					Kind: []graph.EdgeKind{graph.EdgeCalls, graph.EdgeReferences}[e%2], FilePath: file, Line: e + 1 + int(generation),
				})
			}
		}
		if err := s.AtGeneration(generation).AddBatchChecked(nil, edges); err != nil {
			t.Fatalf("write generation %d: %v", generation, err)
		}
	}
	return generations
}

func renderEndpointRows(rows []graph.EdgeEndpointRow) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, fmt.Sprintf("%s|%s|%s|%s", r.From, r.To, r.Kind, r.FilePath))
	}
	sort.Strings(out)
	return out
}

func renderEdgeRows(edges []*graph.Edge) []string {
	out := make([]string, 0, len(edges))
	for _, e := range edges {
		out = append(out, fmt.Sprintf("%s|%s|%s|%s|%d", e.From, e.To, e.Kind, e.FilePath, e.Line))
	}
	sort.Strings(out)
	return out
}

// The lazy builder creates edges_by_file_generation after Open (never inside
// it), the by-file readers return the same rows before and after it exists,
// and a reader whose index vanished under it (a cold bulk window dropped it)
// falls back to the legacy plan with the same rows.
func TestLazyFileGenerationIndexIsBuiltAfterOpenAndReadsAreIdentical(t *testing.T) {
	prevDelay, prevPoll := lazyIndexInitialDelay, lazyIndexPollInterval
	lazyIndexInitialDelay, lazyIndexPollInterval = 200*time.Millisecond, 100*time.Millisecond
	t.Cleanup(func() { lazyIndexInitialDelay, lazyIndexPollInterval = prevDelay, prevPoll })

	s, _ := openTempStore(t)
	if s.fileGenerationIndexPresent() && s.LazyIndexStats().Builds == 0 {
		// Open itself must not build it; only the builder may.
		var n int
		_ = s.db.QueryRow(`SELECT count(*) FROM edges`).Scan(&n)
		if n > 0 {
			t.Fatal("Open built edges_by_file_generation over existing rows")
		}
	}
	generations := writeFileHistoryFixture(t, s)
	paths := []string{"repo/pkg/f01.go", "repo/pkg/f07.go", "repo/pkg/missing.go"}

	// Force the legacy form for the reference answer.
	withIndexDropped := func(fn func()) {
		s.writeMu.Lock()
		s.forgetFileGenerationIndex()
		_, err := s.writerDB.Exec(`DROP INDEX IF EXISTS ` + edgesByFileGenerationIndexName)
		s.writeMu.Unlock()
		if err != nil {
			t.Fatalf("drop: %v", err)
		}
		fn()
	}
	legacyEndpoints := map[int64][]string{}
	legacyEdges := map[int64][]string{}
	withIndexDropped(func() {
		for _, g := range generations {
			h := s.AtGeneration(g)
			legacyEndpoints[g] = renderEndpointRows(h.EdgeEndpointsRecordedAt(paths))
			legacyEdges[g] = renderEdgeRows(h.RecordedEdgesAt(paths))
			if len(legacyEndpoints[g]) != 16 || len(legacyEdges[g]) != 16 {
				t.Fatalf("generation %d: legacy read %d/%d rows, want 16", g, len(legacyEndpoints[g]), len(legacyEdges[g]))
			}
		}
	})

	deadline := time.Now().Add(20 * time.Second)
	for !s.fileGenerationIndexPresent() && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if st := s.LazyIndexStats(); !st.Present || st.Builds < 1 {
		t.Fatalf("the lazy builder did not build the index: %+v", st)
	}
	for _, g := range generations {
		h := s.AtGeneration(g)
		if got := renderEndpointRows(h.EdgeEndpointsRecordedAt(paths)); !reflect.DeepEqual(got, legacyEndpoints[g]) {
			t.Fatalf("generation %d: pinned endpoints differ:\n got %v\nwant %v", g, got, legacyEndpoints[g])
		}
		if got := renderEdgeRows(h.RecordedEdgesAt(paths)); !reflect.DeepEqual(got, legacyEdges[g]) {
			t.Fatalf("generation %d: pinned full rows differ", g)
		}
	}

	// The index vanishes after a reader cached its presence: the reader falls
	// back instead of returning nothing.
	s.writeMu.Lock()
	_, err := s.writerDB.Exec(`DROP INDEX ` + edgesByFileGenerationIndexName)
	s.writeMu.Unlock()
	if err != nil {
		t.Fatalf("drop: %v", err)
	}
	if s.fileGenerationIndex.Load() != lazyIndexPresent {
		t.Fatal("presence was not cached")
	}
	if got := renderEdgeRows(s.AtGeneration(4).RecordedEdgesAt(paths)); !reflect.DeepEqual(got, legacyEdges[4]) {
		t.Fatalf("after the index vanished the reader returned %d rows, want %d", len(got), len(legacyEdges[4]))
	}
	if got := renderEndpointRows(s.AtGeneration(5).EdgeEndpointsRecordedAt(paths)); !reflect.DeepEqual(got, legacyEndpoints[5]) {
		t.Fatalf("after the index vanished the endpoint reader returned %d rows", len(got))
	}
}

// TestFileGenerationReadPlansLockedAcrossStatisticsRegimes pins both pinned
// by-file reads to one (file_path, view_gen) seek — never edges_by_file's
// file-only prefix, never a generation scan — under no statistics, the live
// store's rows, and a fresh ANALYZE.
func TestFileGenerationReadPlansLockedAcrossStatisticsRegimes(t *testing.T) {
	s, _ := openTempStore(t)
	writeFileHistoryFixture(t, s)
	s.writeMu.Lock()
	_, err := s.writerDB.Exec(edgesByFileGenerationIndexDDL)
	s.writeMu.Unlock()
	if err != nil {
		t.Fatalf("create index: %v", err)
	}
	assertLocked := func(t *testing.T, ctx context.Context, conn *sql.Conn) {
		t.Helper()
		for _, generation := range []int64{0, 4} {
			for name, prefix := range map[string]string{"endpoints": edgeEndpointsRecordedAtPinnedPrefix, "recorded": recordedEdgesAtPinnedPrefix} {
				q := prefix + inPlaceholders(2) + recordedAtPinnedSuffix
				plan := strings.Join(explainOnConn(t, ctx, conn, q, "repo/pkg/f01.go", "repo/pkg/f02.go", generation), "\n")
				if !strings.Contains(plan, "INDEX "+edgesByFileGenerationIndexName+" (file_path=? AND view_gen=?)") {
					t.Errorf("%s generation %d: not a (file, generation) seek:\n%s", name, generation, plan)
				}
				for _, bad := range []string{"SCAN edges", "edges_by_generation", "edges_by_file (", "edges_by_to"} {
					if strings.Contains(plan, bad) {
						t.Errorf("%s generation %d: plan uses %q:\n%s", name, generation, bad, plan)
					}
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

// A schema-v29 store opens and serves identical by-file rows with and without
// edges_by_file_generation, and the index never moves the schema version: an
// older binary can open the same file (the index is just one more index to
// it), so a binary rollback stays possible.
func TestFileGenerationIndexIsVersionNeutral(t *testing.T) {
	t.Setenv("GORTEX_SQLITE_LAZY_INDEXES", "off") // this case controls the index itself
	path := filepath.Join(t.TempDir(), "neutral.sqlite")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	generations := writeFileHistoryFixture(t, s)
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	paths := []string{"repo/pkg/f01.go", "repo/pkg/f07.go"}
	read := func(withIndex bool) (map[int64][]string, map[int64][]string) {
		t.Helper()
		s, err := Open(path)
		if err != nil {
			t.Fatalf("reopen (index=%v): %v", withIndex, err)
		}
		defer func() { _ = s.Close() }()
		if v, err := readUserVersion(s.writerDB); err != nil || v != currentSchemaVersion || currentSchemaVersion != 29 {
			t.Fatalf("user_version=%d (err %v), want 29", v, err)
		}
		if got := s.fileGenerationIndexPresent(); got != withIndex {
			t.Fatalf("index present=%v, want %v", got, withIndex)
		}
		endpoints, edges := map[int64][]string{}, map[int64][]string{}
		for _, g := range generations {
			h := s.AtGeneration(g)
			endpoints[g] = renderEndpointRows(h.EdgeEndpointsRecordedAt(paths))
			edges[g] = renderEdgeRows(h.RecordedEdgesAt(paths))
			if len(edges[g]) != 16 {
				t.Fatalf("generation %d: %d rows, want 16", g, len(edges[g]))
			}
		}
		return endpoints, edges
	}
	endpointsWithout, edgesWithout := read(false)

	s, err = Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	s.writeMu.Lock()
	_, err = s.writerDB.Exec(edgesByFileGenerationIndexDDL)
	s.writeMu.Unlock()
	if err != nil {
		t.Fatalf("create index: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	endpointsWith, edgesWith := read(true)
	if !reflect.DeepEqual(endpointsWith, endpointsWithout) || !reflect.DeepEqual(edgesWith, edgesWithout) {
		t.Fatal("the store serves different rows with the index than without it")
	}
}
