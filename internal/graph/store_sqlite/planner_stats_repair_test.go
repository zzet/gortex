package store_sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// Planner-statistics repair at Open.
//
// healPlannerStats is the only thing standing between a warm restart and a
// permanently misplanned store: cold loads refresh statistics at coordinated
// bulk finalize, and nothing else ever re-analyzes. Its historical predicate
// asked only "does a nodes_by_file row exist, and an edges_by_from_line_kind
// row when edges exist" — which is satisfied by a store whose
// nodes_go_receiver_type row says the partial index holds zero (or one) rows.
// That degenerate row is exactly what turns the Go receiver rebind into an
// O(types x member_of) scan, so a store carrying one must be repaired, not
// accepted.

// seedGoReceiverStatsFixture writes a small but genuinely Go-shaped graph:
// receiver types in their package's types.go, methods in their own files, and
// member_of edges connecting them. Only such a store has a non-empty
// nodes_go_receiver_type partial index.
func seedGoReceiverStatsFixture(s *Store, types int) {
	var nodes []*graph.Node
	var edges []*graph.Edge
	for i := 0; i < types; i++ {
		dir := fmt.Sprintf("repo/pkg/p%02d", i)
		name := fmt.Sprintf("T%03d", i)
		typeFile := dir + "/types.go"
		typeID := typeFile + "::" + name
		methodFile := dir + "/methods.go"
		methodID := methodFile + "::" + name + ".M"
		nodes = append(nodes,
			&graph.Node{ID: typeID, Name: name, Kind: graph.KindType, FilePath: typeFile, Language: "go", RepoPrefix: "repo"},
			&graph.Node{ID: methodID, Name: "M", Kind: graph.KindMethod, FilePath: methodFile, Language: "go", RepoPrefix: "repo"},
		)
		edges = append(edges, &graph.Edge{
			From: methodID, To: methodFile + "::" + name, Kind: graph.EdgeMemberOf,
			FilePath: methodFile, Line: i + 1,
		})
	}
	s.AddBatch(nodes, edges)
}

// statsRepairClosed records which handles openStatsRepairStore handed out have
// already been closed. Every one of them gets a t.Cleanup close, so a t.Fatalf
// anywhere in these tests cannot leak an open database (which on Windows also
// blocks the TempDir removal). reopenAfterStatMutation still has to close its
// outgoing handle in the middle of a test, and both paths go through
// closeStatsRepairStore so the cleanup's second close is a no-op rather than a
// final WAL checkpoint against already-closed pools.
var (
	statsRepairClosedMu sync.Mutex
	statsRepairClosed   = map[*Store]bool{}
)

func closeStatsRepairStore(s *Store) error {
	statsRepairClosedMu.Lock()
	already := statsRepairClosed[s]
	statsRepairClosed[s] = true
	statsRepairClosedMu.Unlock()
	if already {
		return nil
	}
	return s.Close()
}

func openStatsRepairStore(t *testing.T, path string) *Store {
	t.Helper()
	s, err := Open(path)
	if err != nil {
		t.Fatalf("open store %s: %v", path, err)
	}
	// Registered after the caller's t.TempDir() so LIFO cleanup closes the
	// database before the directory is removed.
	t.Cleanup(func() { _ = closeStatsRepairStore(s) })
	return s
}

func statRowFor(t *testing.T, s *Store, index string) (string, bool) {
	t.Helper()
	var stat sql.NullString
	err := s.db.QueryRow(`SELECT stat FROM sqlite_stat1 WHERE idx = ?`, index).Scan(&stat)
	if err == sql.ErrNoRows {
		return "", false
	}
	if err != nil {
		t.Fatalf("read %s planner stat: %v", index, err)
	}
	return stat.String, true
}

// statRowCount returns the leading token of a sqlite_stat1 row: SQLite's
// estimate of how many entries the index holds.
func statRowCount(t *testing.T, stat string) int {
	t.Helper()
	fields := strings.Fields(stat)
	if len(fields) == 0 {
		t.Fatalf("planner stat %q has no leading row count", stat)
	}
	var n int
	if _, err := fmt.Sscanf(fields[0], "%d", &n); err != nil {
		t.Fatalf("planner stat %q leading token is not a count: %v", stat, err)
	}
	return n
}

// indexDDLByName finds the CREATE INDEX statement the engine itself uses, so a
// test that drops an index rebuilds byte-identical DDL.
func indexDDLByName(t *testing.T, name string) string {
	t.Helper()
	for _, group := range [][]bulkDroppableIndex{bulkDroppableIndexes, bulkAlwaysLiveIndexes} {
		for _, idx := range group {
			if idx.name == name {
				return idx.ddl
			}
		}
	}
	t.Fatalf("no DDL registered for index %q", name)
	return ""
}

func reopenAfterStatMutation(t *testing.T, s *Store, path string) *Store {
	t.Helper()
	if err := closeStatsRepairStore(s); err != nil {
		t.Fatalf("close store before reopen: %v", err)
	}
	return openStatsRepairStore(t, path)
}

// A store poisoned with the degenerate all-zero receiver row must not be
// accepted as "already has statistics" — that row is precisely what misplans
// the receiver rebind.
func TestOpenRepairsZeroReceiverStat(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stats_zero_receiver.sqlite")
	s := openStatsRepairStore(t, path)
	seedGoReceiverStatsFixture(s, 100)
	s.writeMu.Lock()
	err := s.refreshPlannerStatsLocked(context.Background())
	s.writeMu.Unlock()
	if err != nil {
		t.Fatalf("seed planner stats: %v", err)
	}
	if _, err := s.writerDB.Exec(`UPDATE sqlite_stat1 SET stat = '0 0 0 0 0' WHERE idx = 'nodes_go_receiver_type'`); err != nil {
		t.Fatalf("poison receiver stat: %v", err)
	}

	reopened := reopenAfterStatMutation(t, s, path)
	stat, ok := statRowFor(t, reopened, "nodes_go_receiver_type")
	if !ok {
		t.Fatal("reopen dropped the receiver stat row instead of repairing it")
	}
	if got := statRowCount(t, stat); got < 50 {
		t.Fatalf("receiver stat after reopen = %q (count %d), want a repaired count >= 50", stat, got)
	}
}

// The same repair must fire for an honest-but-stale tiny row: ANALYZE captured
// the index while a single type existed and the planner has been driving the
// join from `c` ever since.
func TestOpenRepairsTinyReceiverStat(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stats_tiny_receiver.sqlite")
	s := openStatsRepairStore(t, path)
	seedGoReceiverStatsFixture(s, 100)
	s.writeMu.Lock()
	err := s.refreshPlannerStatsLocked(context.Background())
	s.writeMu.Unlock()
	if err != nil {
		t.Fatalf("seed planner stats: %v", err)
	}
	if _, err := s.writerDB.Exec(`UPDATE sqlite_stat1 SET stat = '1 1 1 1 1' WHERE idx = 'nodes_go_receiver_type'`); err != nil {
		t.Fatalf("shrink receiver stat: %v", err)
	}

	reopened := reopenAfterStatMutation(t, s, path)
	stat, ok := statRowFor(t, reopened, "nodes_go_receiver_type")
	if !ok {
		t.Fatal("reopen dropped the receiver stat row instead of repairing it")
	}
	if got := statRowCount(t, stat); got < 50 {
		t.Fatalf("receiver stat after reopen = %q (count %d), want a repaired count >= 50", stat, got)
	}
}

// An index recreated by a schema upgrade carries no statistics. The heal
// predicate must notice any missing critical stat row, not only the two it was
// originally written for.
func TestOpenRepairsMissingCriticalStatRow(t *testing.T) {
	for _, index := range []string{"edges_by_kind", "nodes_go_receiver_type"} {
		t.Run(index, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "stats_missing_"+index+".sqlite")
			s := openStatsRepairStore(t, path)
			seedGoReceiverStatsFixture(s, 100)
			s.writeMu.Lock()
			err := s.refreshPlannerStatsLocked(context.Background())
			s.writeMu.Unlock()
			if err != nil {
				t.Fatalf("seed planner stats: %v", err)
			}
			if _, ok := statRowFor(t, s, index); !ok {
				t.Fatalf("fixture never produced a %s stat row", index)
			}
			// DROP INDEX removes the index's sqlite_stat1 row; recreating it
			// leaves the store exactly as a schema upgrade would.
			if _, err := s.writerDB.Exec(`DROP INDEX ` + index); err != nil {
				t.Fatalf("drop %s: %v", index, err)
			}
			if _, err := s.writerDB.Exec(indexDDLByName(t, index)); err != nil {
				t.Fatalf("recreate %s: %v", index, err)
			}
			if _, ok := statRowFor(t, s, index); ok {
				t.Fatalf("fixture kept a %s stat row across DROP/CREATE", index)
			}

			reopened := reopenAfterStatMutation(t, s, path)
			if _, ok := statRowFor(t, reopened, index); !ok {
				t.Fatalf("open did not heal the missing %s planner stat", index)
			}
		})
	}
}

// The repair must stay narrow: a healthy refreshed store reopens without
// paying for another ANALYZE, so its statistics come back untouched.
func TestOpenKeepsFreshStats(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stats_fresh.sqlite")
	s := openStatsRepairStore(t, path)
	seedGoReceiverStatsFixture(s, 100)
	s.writeMu.Lock()
	err := s.refreshPlannerStatsLocked(context.Background())
	s.writeMu.Unlock()
	if err != nil {
		t.Fatalf("seed planner stats: %v", err)
	}
	before := allPlannerStats(t, s)
	if len(before) == 0 {
		t.Fatal("fixture produced no planner statistics")
	}

	reopened := reopenAfterStatMutation(t, s, path)
	after := allPlannerStats(t, reopened)
	if strings.Join(before, "\n") != strings.Join(after, "\n") {
		t.Fatalf("reopen rewrote healthy planner statistics:\nbefore:\n%s\nafter:\n%s",
			strings.Join(before, "\n"), strings.Join(after, "\n"))
	}
	// Byte-identical rows alone are weak evidence: ANALYZE is deterministic
	// for a fixed corpus, so a needless re-refresh would reproduce them. Ask
	// the predicate itself whether it wanted to spend that ANALYZE.
	if reason, needsRepair := plannerStatsRepairReason(context.Background(), reopened.writerDB); needsRepair {
		t.Fatalf("healthy refreshed store asked for repair reason=%q", reason)
	}
}

// sqlite_stat1 has no UNIQUE constraint. A store carrying both an honest row
// and a poisoned one for the same index must be read pessimistically, or the
// repair silently depends on which row the engine happens to return first.
func TestOpenRepairsDuplicateReceiverStatRows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stats_duplicate_receiver.sqlite")
	s := openStatsRepairStore(t, path)
	seedGoReceiverStatsFixture(s, 100)
	s.writeMu.Lock()
	err := s.refreshPlannerStatsLocked(context.Background())
	s.writeMu.Unlock()
	if err != nil {
		t.Fatalf("seed planner stats: %v", err)
	}
	honest, ok := statRowFor(t, s, "nodes_go_receiver_type")
	if !ok {
		t.Fatal("fixture never produced a receiver stat row")
	}
	if got := statRowCount(t, honest); got < 50 {
		t.Fatalf("fixture receiver stat = %q (count %d), want the honest count", honest, got)
	}
	// Append a second, poisoned row behind the honest one.
	if _, err := s.writerDB.Exec(`INSERT INTO sqlite_stat1(tbl, idx, stat) VALUES ('nodes', 'nodes_go_receiver_type', '0 0 0 0 0')`); err != nil {
		t.Fatalf("append duplicate receiver stat row: %v", err)
	}

	reopened := reopenAfterStatMutation(t, s, path)
	stats := 0
	for _, row := range allPlannerStats(t, reopened) {
		if strings.Contains(row, "|nodes_go_receiver_type|") {
			stats++
			if got := statRowCount(t, row[strings.LastIndex(row, "|")+1:]); got < 50 {
				t.Fatalf("receiver stat row after reopen = %q, want the repaired count", row)
			}
		}
	}
	if stats != 1 {
		t.Fatalf("reopen left %d receiver stat rows, want exactly one repaired row", stats)
	}
}

// A store with no Go types at all leaves nodes_go_receiver_type empty. ANALYZE
// on an empty partial index writes the degenerate all-zero row, which is worse
// than no row: SQLite falls back to sane defaults when a stat row is absent,
// but believes a zero row. Absence is the correct state; the row must appear
// only once the index has entries.
func TestRefreshLeavesEmptyPartialIndexWithoutRow(t *testing.T) {
	s := openStatsRepairStore(t, filepath.Join(t.TempDir(), "stats_empty_partial.sqlite"))

	var nodes []*graph.Node
	for i := 0; i < 100; i++ {
		file := fmt.Sprintf("repo/py/mod%02d.py", i)
		nodes = append(nodes, &graph.Node{
			ID:         file + "::" + fmt.Sprintf("fn%02d", i),
			Name:       fmt.Sprintf("fn%02d", i),
			Kind:       graph.KindFunction,
			FilePath:   file,
			Language:   "python",
			RepoPrefix: "repo",
		})
	}
	s.AddBatch(nodes, nil)
	s.writeMu.Lock()
	err := s.refreshPlannerStatsLocked(context.Background())
	s.writeMu.Unlock()
	if err != nil {
		t.Fatalf("refresh planner stats on a Go-free store: %v", err)
	}
	if _, ok := statRowFor(t, s, "nodes_by_file"); !ok {
		t.Fatal("refresh wrote no nodes_by_file stat row")
	}
	if stat, ok := statRowFor(t, s, "nodes_go_receiver_type"); ok {
		t.Fatalf("refresh wrote a stat row %q for an empty partial index; absence is the correct state", stat)
	}

	// Not writing the row is only half the hygiene: a row an older engine
	// already left behind must be cleared. Plant one and refresh again while
	// the index is still empty. This is the only path that exercises the
	// DELETE-matched-a-row branch and the `ANALYZE sqlite_schema` reload that
	// makes the deletion visible to the writer connection's planner.
	if _, err := s.writerDB.Exec(`INSERT INTO sqlite_stat1(tbl, idx, stat) VALUES ('nodes', 'nodes_go_receiver_type', '0 0 0 0 0')`); err != nil {
		t.Fatalf("plant a stale zero row on the empty partial index: %v", err)
	}
	s.writeMu.Lock()
	err = s.refreshPlannerStatsLocked(context.Background())
	s.writeMu.Unlock()
	if err != nil {
		t.Fatalf("refresh planner stats over a planted zero row: %v", err)
	}
	if stat, ok := statRowFor(t, s, "nodes_go_receiver_type"); ok {
		t.Fatalf("refresh kept a stale stat row %q for an empty partial index; it must be deleted", stat)
	}

	// Once Go receiver types exist the row must come back, describing them.
	seedGoReceiverStatsFixture(s, 100)
	s.writeMu.Lock()
	err = s.refreshPlannerStatsLocked(context.Background())
	s.writeMu.Unlock()
	if err != nil {
		t.Fatalf("refresh planner stats after Go types landed: %v", err)
	}
	stat, ok := statRowFor(t, s, "nodes_go_receiver_type")
	if !ok {
		t.Fatal("refresh skipped a now-populated nodes_go_receiver_type")
	}
	if got := statRowCount(t, stat); got <= 0 {
		t.Fatalf("populated receiver stat = %q (count %d), want a positive count", stat, got)
	}
}

// A zero row left behind by an older engine on an index that is still empty is
// invisible to the tiny-stat rule: the index really does hold what the row
// claims, so "believes a handful while holding materially more" is false and
// the row survives every reopen. It is still a poisoned row — SQLite plans an
// absent stat row correctly and a zero one catastrophically — so Open must
// notice the row/emptiness pairing itself and clear it.
func TestOpenRepairsStaleStatRowOnEmptyPartialIndex(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stats_stale_empty_partial.sqlite")
	s := openStatsRepairStore(t, path)
	// No Go nodes at all, so nodes_go_receiver_type stays empty while the rest
	// of the store is ordinary and populated.
	var nodes []*graph.Node
	for i := 0; i < 100; i++ {
		file := fmt.Sprintf("repo/py/mod%02d.py", i)
		nodes = append(nodes, &graph.Node{
			ID:         file + "::" + fmt.Sprintf("fn%02d", i),
			Name:       fmt.Sprintf("fn%02d", i),
			Kind:       graph.KindFunction,
			FilePath:   file,
			Language:   "python",
			RepoPrefix: "repo",
		})
	}
	s.AddBatch(nodes, nil)
	s.writeMu.Lock()
	err := s.refreshPlannerStatsLocked(context.Background())
	s.writeMu.Unlock()
	if err != nil {
		t.Fatalf("seed planner stats: %v", err)
	}
	// The row an older engine's unconditional ANALYZE would have written.
	if _, err := s.writerDB.Exec(`INSERT INTO sqlite_stat1(tbl, idx, stat) VALUES ('nodes', 'nodes_go_receiver_type', '0 0 0 0 0')`); err != nil {
		t.Fatalf("plant a stale zero row on the empty partial index: %v", err)
	}
	if reason, needsRepair := plannerStatsRepairReason(context.Background(), s.writerDB); !needsRepair {
		t.Fatal("a stat row over an empty partial index asked for no repair")
	} else if !strings.HasPrefix(reason, "stale_stat:nodes_go_receiver_type") {
		t.Fatalf("repair reason = %q, want stale_stat:nodes_go_receiver_type", reason)
	}

	reopened := reopenAfterStatMutation(t, s, path)
	if stat, ok := statRowFor(t, reopened, "nodes_go_receiver_type"); ok {
		t.Fatalf("reopen kept the stale stat row %q over an empty partial index; absence is the correct state", stat)
	}
	// And it converges: with the row gone and the index still empty there is
	// nothing left to repair, so the next Open must not ANALYZE again.
	if reason, needsRepair := plannerStatsRepairReason(context.Background(), reopened.writerDB); needsRepair {
		t.Fatalf("the cleared store still asks for repair reason=%q; the rule does not converge", reason)
	}
}

// The tiny-stat rule is generic over every critical index. nodes_by_repo is a
// dense two-column index in v26+, and a degenerate row can misplan repository
// projections just as badly as one on a partial index.
func TestOpenRepairsZeroRepoStat(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stats_zero_repo.sqlite")
	s := openStatsRepairStore(t, path)
	seedGoReceiverStatsFixture(s, 100)
	s.writeMu.Lock()
	err := s.refreshPlannerStatsLocked(context.Background())
	s.writeMu.Unlock()
	if err != nil {
		t.Fatalf("seed planner stats: %v", err)
	}
	var nodeCount int
	if err := s.db.QueryRow(`SELECT count(*) FROM nodes`).Scan(&nodeCount); err != nil {
		t.Fatalf("count fixture nodes: %v", err)
	}
	if nodeCount == 0 {
		t.Fatal("fixture wrote no nodes")
	}
	if _, ok := statRowFor(t, s, "nodes_by_repo"); !ok {
		t.Fatal("fixture never produced a nodes_by_repo stat row")
	}
	// sqlite_stat1 has no UNIQUE constraint, so an INSERT OR REPLACE would
	// append rather than replace. Delete, then insert the v26 two-key shape.
	if _, err := s.writerDB.Exec(`DELETE FROM sqlite_stat1 WHERE idx = 'nodes_by_repo'`); err != nil {
		t.Fatalf("clear repo stat row: %v", err)
	}
	if _, err := s.writerDB.Exec(`INSERT INTO sqlite_stat1(tbl, idx, stat) VALUES ('nodes', 'nodes_by_repo', '0 0 0')`); err != nil {
		t.Fatalf("poison repo stat row: %v", err)
	}

	reopened := reopenAfterStatMutation(t, s, path)
	stat, ok := statRowFor(t, reopened, "nodes_by_repo")
	if !ok {
		t.Fatal("reopen dropped the nodes_by_repo stat row instead of repairing it")
	}
	if got := statRowCount(t, stat); got < nodeCount/2 {
		t.Fatalf("nodes_by_repo stat after reopen = %q (count %d), want a repaired count >= %d",
			stat, got, nodeCount/2)
	}

	again := reopenAfterStatMutation(t, reopened, path)
	if reason, needsRepair := plannerStatsRepairReason(context.Background(), again.writerDB); needsRepair {
		t.Fatalf("second reopen asked to repair the dense repo stat again: reason=%q", reason)
	}
	if got, ok := statRowFor(t, again, "nodes_by_repo"); !ok || got != stat {
		t.Fatalf("second reopen changed the repaired dense repo stat: got %q present=%v, want %q", got, ok, stat)
	}
}

func TestDensePlannerStatsTinyBoundaries(t *testing.T) {
	t.Run("empty_zero_is_stable", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "stats_dense_empty.sqlite")
		s := openStatsRepairStore(t, path)
		s.writeMu.Lock()
		err := s.refreshPlannerStatsLocked(context.Background())
		s.writeMu.Unlock()
		if err != nil {
			t.Fatalf("seed empty planner stats: %v", err)
		}
		if _, err := s.writerDB.Exec(`INSERT INTO sqlite_stat1(tbl, idx, stat) VALUES ('nodes', 'nodes_by_repo', '0 0 0')`); err != nil {
			t.Fatalf("plant legitimate empty dense stat: %v", err)
		}
		if reason, needsRepair := plannerStatsRepairReason(context.Background(), s.writerDB); needsRepair {
			t.Fatalf("legitimate empty dense stat asked for repair reason=%q", reason)
		}
		reopened := reopenAfterStatMutation(t, s, path)
		if stat, ok := statRowFor(t, reopened, "nodes_by_repo"); !ok || stat != "0 0 0" {
			t.Fatalf("empty dense stat after reopen = %q present=%v, want stable 0 0 0", stat, ok)
		}
	})

	t.Run("small_is_stable", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "stats_dense_small.sqlite")
		s := openStatsRepairStore(t, path)
		seedGoReceiverStatsFixture(s, 1)
		s.writeMu.Lock()
		err := s.refreshPlannerStatsLocked(context.Background())
		s.writeMu.Unlock()
		if err != nil {
			t.Fatalf("seed small planner stats: %v", err)
		}
		stat, ok := statRowFor(t, s, "nodes_by_repo")
		if !ok {
			t.Fatal("small dense fixture produced no repo stat")
		}
		if got := statRowCount(t, stat); got <= 0 || got > plannerStatsSuspectRows {
			t.Fatalf("small dense fixture stat = %q (count %d), want 1..%d", stat, got, plannerStatsSuspectRows)
		}
		if reason, needsRepair := plannerStatsRepairReason(context.Background(), s.writerDB); needsRepair {
			t.Fatalf("legitimate small dense stat asked for repair reason=%q", reason)
		}
		reopened := reopenAfterStatMutation(t, s, path)
		if got, ok := statRowFor(t, reopened, "nodes_by_repo"); !ok || got != stat {
			t.Fatalf("small dense stat after reopen = %q present=%v, want %q", got, ok, stat)
		}
	})

	t.Run("negative_is_bounded_and_repaired", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "stats_dense_negative.sqlite")
		s := openStatsRepairStore(t, path)
		seedGoReceiverStatsFixture(s, 10)
		s.writeMu.Lock()
		err := s.refreshPlannerStatsLocked(context.Background())
		s.writeMu.Unlock()
		if err != nil {
			t.Fatalf("seed negative planner stats fixture: %v", err)
		}
		if _, err := s.writerDB.Exec(`UPDATE sqlite_stat1 SET stat = '-1 0 0' WHERE idx = 'nodes_by_repo'`); err != nil {
			t.Fatalf("plant negative dense stat: %v", err)
		}
		reason, needsRepair := plannerStatsRepairReason(context.Background(), s.writerDB)
		if !needsRepair || !strings.HasPrefix(reason, "tiny_stat:nodes_by_repo") {
			t.Fatalf("negative dense stat repair = (%q, %v), want bounded tiny_stat repair", reason, needsRepair)
		}
		reopened := reopenAfterStatMutation(t, s, path)
		stat, ok := statRowFor(t, reopened, "nodes_by_repo")
		if !ok || statRowCount(t, stat) <= 0 {
			t.Fatalf("negative dense stat after reopen = %q present=%v, want positive repaired stat", stat, ok)
		}
		if reason, needsRepair := plannerStatsRepairReason(context.Background(), reopened.writerDB); needsRepair {
			t.Fatalf("repaired negative dense stat still asks for repair reason=%q", reason)
		}
	})
}

// The counterweight to the repair rules: a store whose statistics are merely
// out of date must NOT buy a synchronous whole-index ANALYZE at every Open.
// Only a believed cardinality small enough to invert a join order qualifies,
// and this fixture's believed 100 is far above that gate even though the index
// has since quadrupled.
func TestOpenIgnoresOrdinaryStaleness(t *testing.T) {
	const seeded = 100
	const grownBy = 300
	if seeded <= plannerStatsSuspectRows {
		t.Fatalf("fixture believed count %d is inside the suspect gate %d; the test would not pin it",
			seeded, plannerStatsSuspectRows)
	}
	path := filepath.Join(t.TempDir(), "stats_stale.sqlite")
	s := openStatsRepairStore(t, path)
	seedGoReceiverStatsFixture(s, seeded)
	s.writeMu.Lock()
	err := s.refreshPlannerStatsLocked(context.Background())
	s.writeMu.Unlock()
	if err != nil {
		t.Fatalf("seed planner stats: %v", err)
	}
	stat, ok := statRowFor(t, s, "nodes_go_receiver_type")
	if !ok {
		t.Fatal("fixture never produced a receiver stat row")
	}
	if got := statRowCount(t, stat); got != seeded {
		t.Fatalf("fixture receiver stat = %q (count %d), want the honest %d", stat, got, seeded)
	}

	// The index quadruples behind the statistics. Ordinary staleness.
	var grown []*graph.Node
	for i := 0; i < grownBy; i++ {
		file := fmt.Sprintf("repo/pkg/late%03d/types.go", i)
		name := fmt.Sprintf("L%03d", i)
		grown = append(grown, &graph.Node{
			ID:         file + "::" + name,
			Name:       name,
			Kind:       graph.KindType,
			FilePath:   file,
			Language:   "go",
			RepoPrefix: "repo",
		})
	}
	s.AddBatch(grown, nil)

	reopened := reopenAfterStatMutation(t, s, path)
	if reason, needsRepair := plannerStatsRepairReason(context.Background(), reopened.writerDB); needsRepair {
		t.Fatalf("ordinary staleness asked for a repair reason=%q; only a suspect (<= %d) believed count may",
			reason, plannerStatsSuspectRows)
	}
	after, ok := statRowFor(t, reopened, "nodes_go_receiver_type")
	if !ok {
		t.Fatal("reopen dropped the receiver stat row")
	}
	if got := statRowCount(t, after); got != seeded {
		t.Fatalf("receiver stat after reopen = %q (count %d), want the untouched %d", after, got, seeded)
	}
}

// A repair that only the writer connection can see is half a repair. SQLite
// bumps the schema cookie when sqlite_stat1 is CREATED, never when its rows are
// rewritten, so a reader connection opened before the repair (openSQLiteReadPool
// pings one into existence during Open) keeps planning against the poisoned
// statistics for as long as it lives — on a daemon, forever. Open must
// therefore recycle the read pool's idle connections after a repair, and must
// not pay for that on a healthy store.
func TestOpenRecyclesReadPoolAfterPlannerStatsRepair(t *testing.T) {
	seedRefreshed := func(t *testing.T, path string) *Store {
		t.Helper()
		s := openStatsRepairStore(t, path)
		seedGoReceiverStatsFixture(s, 100)
		s.writeMu.Lock()
		err := s.refreshPlannerStatsLocked(context.Background())
		s.writeMu.Unlock()
		if err != nil {
			t.Fatalf("seed planner stats: %v", err)
		}
		return s
	}

	t.Run("repaired", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "stats_recycle_repaired.sqlite")
		s := seedRefreshed(t, path)
		if _, err := s.writerDB.Exec(`UPDATE sqlite_stat1 SET stat = '0 0 0 0 0' WHERE idx = 'nodes_go_receiver_type'`); err != nil {
			t.Fatalf("poison receiver stat: %v", err)
		}

		reopened := reopenAfterStatMutation(t, s, path)
		// Read the pool's counters before anything else touches it: the very
		// next reader query would check a fresh connection back in as idle.
		stats := reopened.db.Stats()
		if stats.MaxIdleClosed == 0 {
			t.Errorf("read pool kept its pre-repair connection (MaxIdleClosed=0, Idle=%d); "+
				"it would plan against the poisoned statistics for the life of the store",
				stats.Idle)
		}
		if stats.Idle != 0 {
			t.Errorf("read pool holds %d idle connection(s) after a repair, want 0", stats.Idle)
		}
		// The recycle must not have broken the pool it just emptied.
		if stat, ok := statRowFor(t, reopened, "nodes_go_receiver_type"); !ok {
			t.Error("reader cannot read sqlite_stat1 after the pool was recycled")
		} else if got := statRowCount(t, stat); got < 50 {
			t.Errorf("receiver stat after reopen = %q (count %d), want a repaired count >= 50", stat, got)
		}
	})

	t.Run("healthy", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "stats_recycle_healthy.sqlite")
		s := seedRefreshed(t, path)

		reopened := reopenAfterStatMutation(t, s, path)
		if stats := reopened.db.Stats(); stats.MaxIdleClosed != 0 {
			t.Errorf("healthy reopen recycled the read pool (MaxIdleClosed=%d); "+
				"the recycle must be paid for only by a store that was actually repaired",
				stats.MaxIdleClosed)
		}
	})
}

// plannerStatsCriticalIndexNames parses the VALUES list out of
// plannerStatsIndexQuery. Parsing the constant rather than querying a store
// makes the pairing check independent of which indexes a given schema happens
// to have materialised.
func plannerStatsCriticalIndexNames(t *testing.T) []string {
	t.Helper()
	names := regexp.MustCompile(`\('([a-z0-9_]+)'\)`).FindAllStringSubmatch(plannerStatsIndexQuery, -1)
	if len(names) == 0 {
		t.Fatal("plannerStatsIndexQuery exposes no critical index names; the VALUES list shape changed")
	}
	out := make([]string, 0, len(names))
	for _, m := range names {
		out = append(out, m[1])
	}
	return out
}

// The probe table and the index DDL are two hand-written copies of the same
// facts. A predicate that drifts from its DDL does not fail loudly — it
// silently reports the wrong population, and the wrong population is what
// decides whether a stat row is written at all. Pin them together.
func TestPlannerStatsProbesPairedWithIndexDDL(t *testing.T) {
	critical := plannerStatsCriticalIndexNames(t)
	criticalSet := make(map[string]bool, len(critical))
	for _, name := range critical {
		criticalSet[name] = true
	}

	for _, name := range critical {
		spec, known := plannerStatsIndexProbes[name]
		if !known {
			t.Errorf("critical index %q has no plannerStatsIndexProbes entry", name)
			continue
		}
		ddl := indexDDLByName(t, name)
		wantPartial := strings.Contains(ddl, " WHERE ")
		if spec.partial != wantPartial {
			t.Errorf("index %q: probe partial = %v, DDL says %v\nDDL: %s", name, spec.partial, wantPartial, ddl)
			continue
		}
		if !spec.partial {
			if spec.predicate != "" {
				t.Errorf("index %q: non-partial probe carries predicate %q", name, spec.predicate)
			}
			continue
		}
		if spec.predicate == "" {
			t.Errorf("index %q: partial probe carries no predicate", name)
			continue
		}
		if !strings.Contains(ddl, spec.predicate) {
			t.Errorf("index %q: probe predicate %q is not a substring of its DDL\nDDL: %s", name, spec.predicate, ddl)
		}
	}

	// An orphan entry is dead weight that outlives the index it describes.
	for name := range plannerStatsIndexProbes {
		if !criticalSet[name] {
			t.Errorf("plannerStatsIndexProbes entry %q names no critical index", name)
		}
	}

	// Substring agreement with the DDL is necessary but not sufficient. A
	// probe can quote its predicate correctly and still be unusable against a
	// live schema — a conjunct SQLite cannot match to the index's own WHERE
	// clause, a column the index no longer keys, an index that is not
	// materialised at all — and SQLite only rejects INDEXED BY at prepare
	// time. Production swallows that rejection: refreshPlannerStatsOnConn logs
	// the failed probe and falls through to the plain ANALYZE, which is
	// precisely the silent degradation (a zero row written for an empty
	// partial index) that this whole file exists to prevent. So run the
	// engine's own probes and require them to prepare.
	s := openStatsRepairStore(t, filepath.Join(t.TempDir(), "probe_pairing.sqlite"))
	seedGoReceiverStatsFixture(s, 8)
	for _, name := range critical {
		spec, known := plannerStatsIndexProbes[name]
		if !known {
			continue
		}
		existsSQL := spec.existsQuery(name)
		var exists bool
		if err := s.db.QueryRow(existsSQL).Scan(&exists); err != nil {
			t.Errorf("index %q: existence probe is unusable: %v\nSQL: %s", name, err, existsSQL)
		}
		if !spec.partial {
			// countQuery repeats the index's own WHERE clause, so it is only
			// ever built — and only ever executed — for a partial index.
			continue
		}
		countSQL := spec.countQuery(name)
		var actual int64
		if err := s.db.QueryRow(countSQL, plannerStatsSuspectRows).Scan(&actual); err != nil {
			t.Errorf("index %q: bounded count probe is unusable: %v\nSQL: %s", name, err, countSQL)
		}
	}
}

func allPlannerStats(t *testing.T, s *Store) []string {
	t.Helper()
	rows, err := s.db.Query(`SELECT tbl, COALESCE(idx, ''), COALESCE(stat, '') FROM sqlite_stat1`)
	if err != nil {
		t.Fatalf("read sqlite_stat1: %v", err)
	}
	var out []string
	for rows.Next() {
		var tbl, idx, stat string
		if err := rows.Scan(&tbl, &idx, &stat); err != nil {
			_ = rows.Close()
			t.Fatalf("scan sqlite_stat1 row: %v", err)
		}
		out = append(out, tbl+"|"+idx+"|"+stat)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		t.Fatalf("iterate sqlite_stat1: %v", err)
	}
	if err := rows.Close(); err != nil {
		t.Fatalf("close sqlite_stat1 rows: %v", err)
	}
	sort.Strings(out)
	return out
}
