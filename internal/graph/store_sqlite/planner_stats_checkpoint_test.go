package store_sqlite

import (
	"context"
	"database/sql"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/graph"
)

func TestRefreshPlannerStatsTargetsNamedGraphIndexes(t *testing.T) {
	store := openPlannerStatsTestStore(t)
	_, err := store.writerDB.Exec(`
WITH digits(d) AS (VALUES(0),(1),(2),(3),(4),(5),(6),(7),(8),(9)),
seq(x) AS (
    SELECT a.d*1000 + b.d*100 + c.d*10 + d.d + 1
    FROM digits AS a, digits AS b, digits AS c, digits AS d
    LIMIT 5000
)
INSERT INTO nodes(id, kind, name, file_path)
SELECT printf('node-%05d', x), 'function', printf('fn%d', x),
       CASE WHEN x = 1 THEN 'target.go' ELSE printf('file-%05d.go', x) END
FROM seq;
INSERT INTO nodes(id, kind, name, file_path, repo_prefix, language)
VALUES ('repo/types/model.go::Widget', 'type', 'Widget', 'types/model.go', 'repo', 'go')`)
	if err != nil {
		t.Fatalf("populate planner fixture: %v", err)
	}
	_, err = store.writerDB.Exec(`
WITH digits(d) AS (VALUES(0),(1),(2),(3),(4),(5),(6),(7),(8),(9)),
seq(x) AS (
    SELECT a.d*1000 + b.d*100 + c.d*10 + d.d + 1
    FROM digits AS a, digits AS b, digits AS c, digits AS d
    LIMIT 5000
)
INSERT INTO edges(from_id, to_id, kind, file_path, line)
SELECT 'hub', printf('node-%05d', x), 'calls', 'hub.go', x
FROM seq`)
	if err != nil {
		t.Fatalf("populate edge planner fixture: %v", err)
	}

	store.writeMu.Lock()
	err = store.refreshPlannerStatsLocked(t.Context())
	store.writeMu.Unlock()
	if err != nil {
		t.Fatalf("refresh planner stats: %v", err)
	}

	rows, err := store.db.Query(`SELECT idx FROM sqlite_stat1 WHERE tbl IN ('nodes', 'edges') ORDER BY idx`)
	if err != nil {
		t.Fatalf("query graph planner stats: %v", err)
	}
	var gotIndexes []string
	for rows.Next() {
		var index string
		if err := rows.Scan(&index); err != nil {
			_ = rows.Close()
			t.Fatalf("scan graph planner stat: %v", err)
		}
		gotIndexes = append(gotIndexes, index)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		t.Fatalf("iterate graph planner stats: %v", err)
	}
	if err := rows.Close(); err != nil {
		t.Fatalf("close graph planner stats: %v", err)
	}
	wantIndexes := []string{
		"edges_by_from_line",
		"edges_by_from_line_kind",
		"edges_by_generation",
		"edges_by_kind",
		"nodes_by_file",
		"nodes_by_generation",
		"nodes_by_kind",
		"nodes_by_name",
		"nodes_by_repo",
		"nodes_by_repo_kind",
		"nodes_by_repo_language_name",
		"nodes_go_receiver_type",
	}
	if !reflect.DeepEqual(gotIndexes, wantIndexes) {
		t.Fatalf("synchronous graph stats = %v, want %v", gotIndexes, wantIndexes)
	}

	plan := explainPlannerQueryPlan(t, store.db,
		`SELECT id FROM nodes WHERE file_path = ? AND kind = ?`,
		"target.go", "function")
	if !strings.Contains(plan, "nodes_by_file") {
		t.Fatalf("selective file query missed nodes_by_file after stats refresh:\n%s", plan)
	}

	// The table's UNIQUE key also starts with from_id. On the production
	// corpus a stats-blind planner chose that key and reread every edge owned by
	// a hub source; the line-bearing stat must keep exact-site probes selective.
	plan = explainPlannerQueryPlan(t, store.db,
		`SELECT to_id FROM edges WHERE from_id = ? AND line = ? AND kind = ? AND view_gen = ?`,
		"hub", 1, "calls", baseViewGeneration)
	if !strings.Contains(plan, "edges_by_from_line_kind") {
		t.Fatalf("exact-site query missed edges_by_from_line_kind after stats refresh:\n%s", plan)
	}

	// These indexes have no competing left-prefix path. They remain selected
	// without paying synchronous ANALYZE page counts for them.
	plan = explainPlannerQueryPlan(t, store.db,
		`SELECT from_id FROM edges WHERE to_id = ? AND kind = ? AND view_gen = ?`,
		"node-00001", "calls", baseViewGeneration)
	if !strings.Contains(plan, "edges_by_to") {
		t.Fatalf("in-edge query missed edges_by_to without a dedicated stat:\n%s", plan)
	}
	plan = explainPlannerQueryPlan(t, store.db,
		`SELECT from_id FROM edges WHERE file_path = ? AND kind = ? AND view_gen = ?`,
		"hub.go", "calls", baseViewGeneration)
	if !strings.Contains(plan, "edges_by_file") {
		t.Fatalf("file-edge query missed edges_by_file without a dedicated stat:\n%s", plan)
	}
}

func TestEndCoordinatedBulkLoadDoesNotWaitForReadSnapshot(t *testing.T) {
	store := openPlannerStatsTestStore(t)
	store.passiveCheckpointTimeout = 250 * time.Millisecond
	if !store.BeginCoordinatedBulkLoad() {
		t.Fatal("cold store did not enter coordinated bulk load")
	}
	store.AddNode(&graph.Node{ID: "before", Kind: graph.KindFunction, Name: "before", FilePath: "before.go"})

	// Hold an explicit old read snapshot across another committed write. Such a
	// snapshot prevents RESTART/TRUNCATE from resetting the WAL even though the
	// Store writer gate is exclusively held by finalization.
	readTx, err := store.db.BeginTx(context.Background(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatalf("begin read snapshot: %v", err)
	}
	defer func() { _ = readTx.Rollback() }()
	var count int
	if err := readTx.QueryRow(`SELECT COUNT(*) FROM nodes`).Scan(&count); err != nil {
		t.Fatalf("establish read snapshot: %v", err)
	}
	store.AddNode(&graph.Node{ID: "after", Kind: graph.KindFunction, Name: "after", FilePath: "after.go"})

	var events []bulkFinalizeEvent
	var checkpoint bulkFinalizeEvent
	store.bulkFinalizeObserver = func(event bulkFinalizeEvent) {
		events = append(events, event)
		if event.Stage == "checkpoint" {
			checkpoint = event
		}
	}
	started := time.Now()
	if err := store.EndCoordinatedBulkLoad(); err != nil {
		t.Fatalf("end coordinated bulk load: %v", err)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("reader snapshot delayed cold finalization by %s", elapsed)
	}
	edgeIndex, edgeCheckpoint, preStats, plannerStats := -1, -1, -1, -1
	for i, event := range events {
		switch {
		case event.Stage == "index" && event.Name == "edges_by_file":
			edgeIndex = i
		case event.Stage == "checkpoint_passive" && event.Name == "index_seal_edges":
			edgeCheckpoint = i
		case event.Stage == "checkpoint_passive" && event.Name == "planner_stats_before":
			preStats = i
		case event.Stage == "planner_stats":
			plannerStats = i
		}
	}
	if edgeIndex < 0 || preStats < 0 || plannerStats < 0 || edgeIndex >= preStats || preStats >= plannerStats {
		t.Fatalf("bulk finalization event order missing coalesced pre-stats checkpoint: %+v", events)
	}
	if edgeCheckpoint >= 0 {
		t.Fatalf("successful seal retained adjacent edge checkpoint at event %d: %+v", edgeCheckpoint, events)
	}
	if checkpoint.Name != "wal_passive" {
		t.Fatalf("final checkpoint = %q, want wal_passive", checkpoint.Name)
	}
	if checkpoint.Err != nil {
		t.Fatalf("passive checkpoint failed: %v", checkpoint.Err)
	}
}

func TestEndCoordinatedBulkLoadFinalCheckpointGetsDedicatedDrainWindow(t *testing.T) {
	store := openPlannerStatsTestStore(t)
	// Make every routine PASSIVE attempt expire immediately. The final handoff
	// must ignore this routine policy and use bulkFinalCheckpointTimeout.
	store.passiveCheckpointTimeout = time.Nanosecond
	if !store.BeginCoordinatedBulkLoad() {
		t.Fatal("cold store did not enter coordinated bulk load")
	}
	store.AddNode(&graph.Node{ID: "snapshot", Kind: graph.KindFunction, Name: "snapshot", FilePath: "snapshot.go"})

	readTx, err := store.db.BeginTx(context.Background(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatalf("begin read snapshot: %v", err)
	}
	readOpen := true
	defer func() {
		if readOpen {
			_ = readTx.Rollback()
		}
	}()
	var count int
	if err := readTx.QueryRow(`SELECT COUNT(*) FROM nodes`).Scan(&count); err != nil {
		t.Fatalf("establish read snapshot: %v", err)
	}

	nodes, edges := bulkFixture(4096, 32768)
	store.AddBatch(nodes, edges)

	var final bulkFinalizeEvent
	store.bulkFinalizeObserver = func(event bulkFinalizeEvent) {
		// Keep the snapshot through the pre-stats drain, then retire it before
		// the terminal handoff so the final PASSIVE can prove a complete copy.
		if event.Stage == "planner_stats" && readOpen {
			if err := readTx.Rollback(); err != nil {
				t.Errorf("release read snapshot: %v", err)
			}
			readOpen = false
		}
		if event.Stage == "checkpoint" && event.Name == "wal_passive" {
			final = event
		}
	}
	if err := store.EndCoordinatedBulkLoad(); err != nil {
		t.Fatalf("end coordinated bulk load: %v", err)
	}
	if readOpen {
		t.Fatal("planner-stats boundary did not release read snapshot")
	}
	if final.Name != "wal_passive" {
		t.Fatalf("missing final wal_passive event: %+v", final)
	}
	if final.Err != nil {
		t.Fatalf("final passive checkpoint failed: %v", final.Err)
	}
	if final.WALFrames == 0 {
		t.Fatalf("final passive checkpoint reported no WAL frames: %+v", final)
	}
	if final.Busy != 0 || final.CheckpointedFrames != final.WALFrames {
		t.Fatalf("final passive checkpoint incomplete: %+v", final)
	}
}

func TestBulkCheckpointTimeoutPolicy(t *testing.T) {
	if walPassiveCheckpointTimeout != time.Second {
		t.Fatalf("routine PASSIVE timeout = %s, want 1s", walPassiveCheckpointTimeout)
	}
	if bulkPlannerStatsCheckpointTimeout != 6*time.Second {
		t.Fatalf("coalesced pre-stats PASSIVE timeout = %s, want 6s", bulkPlannerStatsCheckpointTimeout)
	}
	if bulkFinalCheckpointTimeout != 30*time.Second {
		t.Fatalf("final PASSIVE timeout = %s, want 30s", bulkFinalCheckpointTimeout)
	}
}

func explainPlannerQueryPlan(t *testing.T, db *sql.DB, query string, args ...any) string {
	t.Helper()
	rows, err := db.QueryContext(t.Context(), `EXPLAIN QUERY PLAN `+query, args...)
	if err != nil {
		t.Fatalf("explain query plan: %v", err)
	}
	defer func() {
		if err := rows.Close(); err != nil {
			t.Errorf("close query plan: %v", err)
		}
	}()

	var details []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatalf("scan query plan: %v", err)
		}
		details = append(details, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate query plan: %v", err)
	}
	return strings.Join(details, "\n")
}

func openPlannerStatsTestStore(t *testing.T) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), "graph.db"))
	if err != nil {
		t.Fatalf("open sqlite store: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("close sqlite store: %v", err)
		}
	})
	return store
}
