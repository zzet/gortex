package store_sqlite

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/graph"
)

// Runtime planner-statistics freshness.
//
// PR 1 made a store repair misleading statistics at Open and stop writing the
// poisoned zero row. Neither helps a daemon that stays up: sqlite_stat1 is
// written at cold-load finalize and at Open, and nothing else re-analyzes
// while a second repository drains in, watchers reindex, and view generations
// publish payload. These tests pin the growth rule, the paths that must NOT
// pay for it, and — most importantly — that it converges.
//
// None of them is OS-specific, and none is named to match the Windows CI leg's
// -run 'PlanLock|PlansLocked|PlansNeverScan' filter for this package, so the
// whole file runs on Linux and macOS only. That is deliberate: nothing here
// touches a path separator or a file lock, and the Windows leg exists to guard
// the plan locks, not this mechanism.

// seedNamedGoReceiverFixture is seedGoReceiverStatsFixture with a package
// namespace, so a test can grow a store by adding a second disjoint batch.
func seedNamedGoReceiverFixture(s *Store, namespace string, types int) {
	var nodes []*graph.Node
	var edges []*graph.Edge
	for i := 0; i < types; i++ {
		dir := fmt.Sprintf("repo/%s/p%03d", namespace, i)
		name := fmt.Sprintf("%sT%03d", namespace, i)
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

// writeIndexStateCounters writes one repo_index_state row at an explicit view
// generation. Tests use it rather than SetRepoIndexState when the point is
// which generation the row belongs to.
func writeIndexStateCounters(t *testing.T, s *Store, viewGen int64, repo string, nodes, edges int) {
	t.Helper()
	_, err := s.writerDB.Exec(`
INSERT OR REPLACE INTO repo_index_state
  (view_gen, repo_prefix, indexed_sha, dirty, indexed_at, workspace_fp, node_count, edge_count, extractor_versions)
VALUES (?, ?, '', 0, 0, '', ?, ?, '')`, viewGen, repo, nodes, edges)
	if err != nil {
		t.Fatalf("write repo_index_state(view_gen=%d, repo=%q): %v", viewGen, repo, err)
	}
}

func refreshStatsNow(t *testing.T, s *Store) {
	t.Helper()
	s.writeMu.Lock()
	err := s.refreshPlannerStatsLocked(context.Background())
	s.writeMu.Unlock()
	if err != nil {
		t.Fatalf("seed planner stats: %v", err)
	}
}

// freshStatsStore returns an on-disk store holding `types` Go receiver types
// (so 2*types nodes and `types` edges), counters that agree with the corpus,
// and statistics computed over exactly that corpus.
func freshStatsStore(t *testing.T, name string, types int) *Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	s := openStatsRepairStore(t, path)
	seedNamedGoReceiverFixture(s, "a", types)
	writeIndexStateCounters(t, s, 0, "repo", 2*types, types)
	refreshStatsNow(t, s)
	return s
}

func mustHealth(t *testing.T, s *Store) graph.PlannerStatsFreshness {
	t.Helper()
	h, err := s.PlannerStatsHealth(context.Background())
	if err != nil {
		t.Fatalf("planner stats health: %v", err)
	}
	return h
}

func mustEnsure(t *testing.T, s *Store) graph.PlannerStatsFreshness {
	t.Helper()
	h, err := s.EnsurePlannerStatsFresh(context.Background())
	if err != nil {
		t.Fatalf("ensure planner stats fresh: %v", err)
	}
	return h
}

// A store whose statistics describe the store it actually is must not pay for
// an ANALYZE. This is the steady state at every one of the call sites, so
// "cheap when fresh" is the property the whole mechanism rests on.
func TestEnsurePlannerStatsFresh_NoOpWhenFresh(t *testing.T) {
	s := freshStatsStore(t, "stats_fresh_noop.sqlite", 100)

	before := map[string]string{}
	for _, idx := range []string{"nodes_by_kind", "edges_by_kind", "nodes_go_receiver_type"} {
		stat, ok := statRowFor(t, s, idx)
		if !ok {
			t.Fatalf("fixture left no stat row for %s", idx)
		}
		before[idx] = stat
	}
	if h := mustHealth(t, s); !h.LastRefreshAt.IsZero() {
		t.Fatalf("ledger stamped before any runtime refresh: %v", h.LastRefreshAt)
	}

	h := mustEnsure(t, s)
	if h.Stale || h.Refreshed {
		t.Fatalf("fresh store reported stale=%v refreshed=%v reason=%q", h.Stale, h.Refreshed, h.Reason)
	}
	if h.Refreshes != 0 {
		t.Fatalf("fresh store recorded %d refresh(es)", h.Refreshes)
	}
	if !h.LastRefreshAt.IsZero() {
		t.Fatalf("fresh store stamped the refresh ledger at %v (reason %q)", h.LastRefreshAt, h.LastRefreshReason)
	}
	for idx, want := range before {
		got, ok := statRowFor(t, s, idx)
		if !ok || got != want {
			t.Errorf("stat row for %s changed on a fresh store: %q -> %q (present=%v)", idx, want, got, ok)
		}
	}

	// "Cheap when fresh" means no lock, not just no ANALYZE. Five pipeline
	// boundaries call this on every index pass, every save-driven incremental
	// reconcile, every whole-graph resolve and every generation publish;
	// queuing behind whatever else holds the write gate would put a wait on
	// somebody else's transaction into all of them.
	t.Run("takes no write gate", func(t *testing.T) {
		s.writeMu.Lock()
		defer s.writeMu.Unlock()

		// Generous on purpose: the assertion is "did not QUEUE on the gate",
		// and the fresh path still issues a dozen-odd read-pool queries. Under
		// -race on a loaded CI runner those alone can outlast a one-second
		// budget, so the deadline is set where only a real queue can reach it.
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		done := make(chan error, 1)
		go func() {
			_, err := s.EnsurePlannerStatsFresh(ctx)
			done <- err
		}()
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("fresh-path check under a held write gate: %v", err)
			}
		case <-ctx.Done():
			t.Fatal("EnsurePlannerStatsFresh blocked on the write gate for a store that is already fresh")
		}
	})
}

// The defect this PR exists for: the store doubles at runtime and nothing
// re-analyzes, so the planner keeps costing joins against the size it saw at
// boot. The fixture grows to EXACTLY the factor so the comparison's boundary
// is pinned too.
func TestEnsurePlannerStatsFresh_RefreshesAfterDoubling(t *testing.T) {
	s := freshStatsStore(t, "stats_growth.sqlite", 100)
	if believed := plannerStatsBelievedRows(context.Background(), s.db, "nodes_by_kind"); believed != 200 {
		t.Fatalf("fixture believed %d nodes, want the 200 it holds", believed)
	}

	// Exactly 2x: 200 more nodes and a counter row that says so.
	seedNamedGoReceiverFixture(s, "b", 100)
	writeIndexStateCounters(t, s, 0, "repo", 400, 200)

	h := mustEnsure(t, s)
	if !h.Refreshed {
		t.Fatalf("doubled store did not refresh: stale=%v reason=%q", h.Stale, h.Reason)
	}
	if !strings.HasPrefix(h.Reason, "growth:nodes_by_kind") {
		t.Errorf("refresh reason = %q, want a growth:nodes_by_kind verdict", h.Reason)
	}
	if h.Nodes.Actual != 400 || h.Nodes.Believed != 200 {
		t.Errorf("verdict carried believed=%d actual=%d, want 200/400", h.Nodes.Believed, h.Nodes.Actual)
	}
	stat, ok := statRowFor(t, s, "nodes_by_kind")
	if !ok {
		t.Fatal("refresh left no nodes_by_kind stat row")
	}
	if got := statRowCount(t, stat); got < 300 {
		t.Errorf("nodes_by_kind believes %d after the refresh, want the grown corpus (>=300)", got)
	}
	if h.LastRefreshReason != h.Reason || h.LastRefreshAt.IsZero() {
		t.Errorf("ledger = %q at %v, want the verdict that triggered the refresh", h.LastRefreshReason, h.LastRefreshAt)
	}
}

// sqlite_stat1 describes the PHYSICAL index, which holds every view
// generation's rows. Summing the counters through the base handle's view_gen
// filter is what made the lead's store believe 592k nodes while holding 1.69M
// across 16 generations, so the freshness sum must carry no view filter.
func TestPlannerStatsHealth_CountsGenerationRows(t *testing.T) {
	s := freshStatsStore(t, "stats_generations.sqlite", 20)
	writeIndexStateCounters(t, s, 0, "repo", 40, 20)
	writeIndexStateCounters(t, s, 7, "repo", 400, 300)
	writeIndexStateCounters(t, s, 9, "other", 60, 30)

	h := mustHealth(t, s)
	if !h.Nodes.Known || h.Nodes.Actual != 500 {
		t.Errorf("nodes actual = %d (known=%v), want 500 summed across every generation", h.Nodes.Actual, h.Nodes.Known)
	}
	if !h.Edges.Known || h.Edges.Actual != 350 {
		t.Errorf("edges actual = %d (known=%v), want 350 summed across every generation", h.Edges.Actual, h.Edges.Known)
	}
}

// A populated relation with no stat row at all is the R1 state PR 1 repairs at
// Open. At runtime it is reached by a FRESH single-repo bulk load, whose
// counters are not written until after the drain — so the rule must take its
// evidence from the corpus, not from the counters that do not exist yet.
func TestEnsurePlannerStatsFresh_MissingRowWithRows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stats_missing_row.sqlite")
	s := openStatsRepairStore(t, path)
	seedNamedGoReceiverFixture(s, "a", 50)
	refreshStatsNow(t, s)
	// No repo_index_state rows at all: this is the fresh-cold-load shape.
	if _, err := s.writerDB.Exec(`DELETE FROM sqlite_stat1 WHERE idx IN ('nodes_by_kind','nodes_by_file')`); err != nil {
		t.Fatalf("delete node stat rows: %v", err)
	}

	h := mustEnsure(t, s)
	if !h.Refreshed {
		t.Fatalf("store with no node statistics did not refresh: stale=%v reason=%q", h.Stale, h.Reason)
	}
	if !strings.HasPrefix(h.Reason, "missing:") {
		t.Errorf("reason = %q, want a missing: verdict", h.Reason)
	}
	if h.Nodes.Known {
		t.Errorf("counters reported known with no repo_index_state rows (actual=%d)", h.Nodes.Actual)
	}
	if _, ok := statRowFor(t, s, "nodes_by_kind"); !ok {
		t.Error("refresh did not restore the nodes_by_kind stat row")
	}
}

// A bulk window owns the writer AND has dropped the droppable critical
// indexes. Refreshing inside it would fight the pinned connection for a result
// the cold path recomputes at its own finalize anyway.
func TestEnsurePlannerStatsFresh_SkipsInsideBulkWindow(t *testing.T) {
	// t.TempDir + a real file: beginBulkLoadLocked is a no-op for in-memory
	// paths, so an in-memory store would assert nothing at all here.
	path := filepath.Join(t.TempDir(), "stats_bulk_window.sqlite")
	s := openStatsRepairStore(t, path)
	if !s.BeginCoordinatedBulkLoad() {
		t.Fatal("coordinated bulk load did not engage on an empty on-disk store")
	}
	seedNamedGoReceiverFixture(s, "a", 60)

	h := mustEnsure(t, s)
	if h.Refreshed || h.Stale {
		t.Fatalf("refreshed inside a bulk window: refreshed=%v stale=%v reason=%q", h.Refreshed, h.Stale, h.Reason)
	}
	if h.Reason != plannerStatsBulkWindowReason {
		t.Errorf("reason = %q, want %q", h.Reason, plannerStatsBulkWindowReason)
	}

	// The read-only probe has to say the same thing. index_health rebuilds it
	// on a schedule, and a cold load in progress — every droppable critical
	// index gone, statistics not yet computed — would otherwise be reported as
	// a defect for the whole duration of a perfectly healthy load.
	probe := mustHealth(t, s)
	if probe.Stale || probe.Reason != plannerStatsBulkWindowReason {
		t.Errorf("health probe inside a bulk window: stale=%v reason=%q, want a %q skip",
			probe.Stale, probe.Reason, plannerStatsBulkWindowReason)
	}

	// The window can also open between the lock-free probe and the write gate.
	// Clearing only the mirrored flag reproduces that race exactly, and the
	// check under the gate is what has to catch it.
	s.bulkWindowOpen.Store(false)
	raced := mustEnsure(t, s)
	if raced.Refreshed || raced.Reason != plannerStatsBulkWindowReason {
		t.Errorf("racing check: refreshed=%v reason=%q, want a %q skip",
			raced.Refreshed, raced.Reason, plannerStatsBulkWindowReason)
	}
	s.bulkWindowOpen.Store(true)

	if err := s.EndCoordinatedBulkLoad(); err != nil {
		t.Fatalf("end coordinated bulk load: %v", err)
	}
	if _, ok := statRowFor(t, s, "nodes_by_kind"); !ok {
		t.Error("cold finalize left no nodes_by_kind statistics")
	}
	after := mustHealth(t, s)
	if after.LastRefreshReason != "cold_load_finalize" || after.LastRefreshAt.IsZero() {
		t.Errorf("cold finalize did not stamp the ledger: reason=%q at=%v", after.LastRefreshReason, after.LastRefreshAt)
	}
}

// SQLite bumps the schema cookie only when sqlite_stat1 is CREATED, so a
// refresh performed on the writer is invisible to every reader connection
// already open. A runtime refresh has exactly the same problem the Open-time
// repair does, and must use the same remedy — and only when it refreshed.
func TestEnsurePlannerStatsFresh_RecyclesReadPool(t *testing.T) {
	t.Run("refreshed", func(t *testing.T) {
		s := freshStatsStore(t, "stats_recycle_runtime.sqlite", 100)
		seedNamedGoReceiverFixture(s, "b", 150)
		writeIndexStateCounters(t, s, 0, "repo", 500, 250)

		h := mustEnsure(t, s)
		if !h.Refreshed {
			t.Fatalf("grown store did not refresh: reason=%q", h.Reason)
		}
		stats := s.db.Stats()
		if stats.MaxIdleClosed == 0 {
			t.Errorf("read pool kept its pre-refresh connections (MaxIdleClosed=0, Idle=%d); "+
				"they would plan against the pre-growth statistics for the life of the store", stats.Idle)
		}
		if stats.Idle != 0 {
			t.Errorf("read pool holds %d idle connection(s) after a refresh, want 0", stats.Idle)
		}
	})

	t.Run("fresh", func(t *testing.T) {
		s := freshStatsStore(t, "stats_recycle_fresh.sqlite", 100)
		if h := mustEnsure(t, s); h.Refreshed {
			t.Fatalf("fresh store refreshed: reason=%q", h.Reason)
		}
		if stats := s.db.Stats(); stats.MaxIdleClosed != 0 {
			t.Errorf("fresh store recycled the read pool (MaxIdleClosed=%d); the recycle is only "+
				"owed by a store that actually rewrote its statistics", stats.MaxIdleClosed)
		}
	})
}

// The payload index_health publishes is read straight off this struct, so what
// each field means has to be pinned: believed comes from the named index's
// stat row (with the documented nodes_by_file fallback), actual from the
// counters, and the receiver figure is a bounded probe rather than a count.
func TestPlannerStatsHealth_ReportsCardinalities(t *testing.T) {
	s := freshStatsStore(t, "stats_cardinalities.sqlite", 40)

	h := mustHealth(t, s)
	for _, tc := range []struct {
		name  string
		index string
		got   int64
	}{
		{"nodes", "nodes_by_kind", h.Nodes.Believed},
		{"edges", "edges_by_kind", h.Edges.Believed},
		{"receivers", "nodes_go_receiver_type", h.Receivers.Believed},
	} {
		stat, ok := statRowFor(t, s, tc.index)
		if !ok {
			t.Fatalf("fixture left no stat row for %s", tc.index)
		}
		if want := int64(statRowCount(t, stat)); tc.got != want {
			t.Errorf("%s believed = %d, want %s's %d", tc.name, tc.got, tc.index, want)
		}
	}
	if h.Nodes.Actual != 80 || h.Edges.Actual != 40 {
		t.Errorf("counters reported %d nodes / %d edges, want 80/40", h.Nodes.Actual, h.Edges.Actual)
	}
	if h.Nodes.Bounded || h.Edges.Bounded {
		t.Error("counter-derived cardinalities must not claim to be bounded probes")
	}
	if !h.Receivers.Bounded {
		t.Error("the receiver figure is a LIMIT-bounded probe and must say so")
	}
	if h.Stale {
		t.Errorf("fixture reported stale: %q", h.Reason)
	}

	t.Run("nodes_by_file fallback", func(t *testing.T) {
		if _, err := s.writerDB.Exec(`DELETE FROM sqlite_stat1 WHERE idx = 'nodes_by_kind'`); err != nil {
			t.Fatalf("delete nodes_by_kind stat row: %v", err)
		}
		fallback := mustHealth(t, s)
		if fallback.Nodes.Believed == 0 {
			t.Fatalf("nodes believed fell to 0 with nodes_by_kind absent; the nodes_by_file fallback did not answer")
		}
		// The cardinality is answered by the fallback; the missing row itself
		// is owed (any planner index that holds rows and has no statistics
		// row is), where it once read fresh.
		if !fallback.Stale || fallback.Reason != "missing:nodes_by_kind" {
			t.Errorf("fallback path: stale=%v reason=%q, want missing:nodes_by_kind", fallback.Stale, fallback.Reason)
		}
	})
}

// An index a bulk window has physically DROPPED is not a store with missing
// statistics: no ANALYZE can write a row for an index that is not there, so
// reporting it would alarm on every cold load and ask for a refresh that can
// never converge. The verdict has to be driven off the schema.
func TestPlannerStatsHealth_IgnoresIndexesAbsentFromTheSchema(t *testing.T) {
	s := freshStatsStore(t, "stats_absent_index.sqlite", 40)
	for _, idx := range []string{"nodes_by_kind", "nodes_by_file"} {
		if _, err := s.writerDB.Exec(`DROP INDEX ` + quoteSQLiteIdentifier(idx)); err != nil {
			t.Fatalf("drop %s: %v", idx, err)
		}
	}

	h := mustHealth(t, s)
	if h.Stale {
		t.Errorf("reported stale for indexes the schema does not hold: %q", h.Reason)
	}
	if h.Nodes.Believed != 0 {
		t.Errorf("nodes believed = %d with both node indexes dropped, want 0", h.Nodes.Believed)
	}

	// And the verdict comes back once the indexes do — dropping an index also
	// drops its statistics row, so this is a genuine "missing" state.
	for _, idx := range []string{"nodes_by_kind", "nodes_by_file"} {
		if _, err := s.writerDB.Exec(indexDDLByName(t, idx)); err != nil {
			t.Fatalf("recreate %s: %v", idx, err)
		}
	}
	if restored := mustHealth(t, s); !restored.Stale || !strings.HasPrefix(restored.Reason, "missing:") {
		t.Errorf("after the indexes came back: stale=%v reason=%q, want a missing: verdict",
			restored.Stale, restored.Reason)
	}
}

// The receiver probe is NOT index-only: satisfying the partial predicate reads
// language, kind and file_path, so it costs one WITHOUT ROWID table probe per
// index entry. PR 1 could bind believed*2+1 because it only ran on a believed
// count already known to be tiny; the health probe runs at every call site and
// on every index_health rebuild, so the bound has to be absolute.
func TestPlannerStatsHealth_CapsTheReceiverProbe(t *testing.T) {
	s := freshStatsStore(t, "stats_probe_cap.sqlite", 200)

	h := mustHealth(t, s)
	if h.Receivers.Believed < 100 {
		t.Fatalf("fixture believes %d receiver entries; it cannot exercise the cap", h.Receivers.Believed)
	}
	if h.Receivers.Actual > plannerStatsHealthProbeCap {
		t.Errorf("receiver probe read %d entries, want at most the %d cap", h.Receivers.Actual, plannerStatsHealthProbeCap)
	}
	if h.Receivers.Known {
		t.Error("a capped probe answered a question it could not ask in full; it must report Known=false " +
			"and leave general growth detection to the counters")
	}
	if h.Stale {
		t.Errorf("capped receiver probe produced a verdict: %q", h.Reason)
	}
}

// The anti-loop guard. A verdict a refresh does not clear must not be paid for
// again at every call site until the store grows: the growth baseline cannot
// help here, because a "missing" verdict is not measured against it.
func TestEnsurePlannerStatsFresh_DoesNotRepeatASettledVerdict(t *testing.T) {
	s := freshStatsStore(t, "stats_settled.sqlite", 100)
	dropNodeStats := func() {
		t.Helper()
		if _, err := s.writerDB.Exec(`DELETE FROM sqlite_stat1 WHERE idx IN ('nodes_by_kind','nodes_by_file')`); err != nil {
			t.Fatalf("delete node stat rows: %v", err)
		}
	}

	dropNodeStats()
	if first := mustEnsure(t, s); !first.Refreshed {
		t.Fatalf("first missing verdict did not refresh: %q", first.Reason)
	}

	// The same verdict over a store that has not grown since.
	dropNodeStats()
	second := mustEnsure(t, s)
	if second.Refreshed {
		t.Fatalf("re-paid for a verdict this store already acted on; every call site would pay an "+
			"ANALYZE forever (reason=%q)", second.Reason)
	}
	if !strings.HasPrefix(second.Reason, "settled:") {
		t.Errorf("reason = %q, want a settled: skip", second.Reason)
	}

	// Real growth re-arms it: the guard is about repetition, not suppression.
	seedNamedGoReceiverFixture(s, "b", 100)
	writeIndexStateCounters(t, s, 0, "repo", 400, 200)
	if third := mustEnsure(t, s); !third.Refreshed {
		t.Fatalf("the guard suppressed a refresh a grown store needed: %q", third.Reason)
	}
}

// index_health rebuilds this payload in the background on a daemon that may be
// mid-write. A health probe that queued behind the write gate would turn "is
// the index healthy" into a wait on somebody else's transaction.
func TestPlannerStatsHealth_TakesNoWriteGate(t *testing.T) {
	s := freshStatsStore(t, "stats_no_write_gate.sqlite", 20)

	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	// Generous on purpose, for the same reason as the fresh-path check above:
	// the probe issues real read-pool queries, and only a genuine wait on the
	// write gate can reach a ten-second deadline.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := s.PlannerStatsHealth(ctx)
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("health probe under a held write gate: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("PlannerStatsHealth blocked on the write gate; it must read only through the read pool")
	}
}

// An in-memory store's reader and writer are one pool, and it holds the whole
// database: dropping its last connection destroys it.
func TestRecycleStatsReadPool_SharedHandleIsNoop(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatalf("open in-memory store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if s.db != s.writerDB {
		t.Fatal("in-memory store no longer shares one pool; this test's premise is gone")
	}
	seedNamedGoReceiverFixture(s, "a", 5)

	recycleStatsReadPool(s.db, s.writerDB)

	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM nodes`).Scan(&count); err != nil {
		t.Fatalf("in-memory database lost after recycling a shared pool: %v", err)
	}
	if count != 10 {
		t.Fatalf("in-memory store holds %d nodes after the recycle, want 10", count)
	}
	if stats := s.db.Stats(); stats.MaxIdleClosed != 0 {
		t.Errorf("recycle closed %d idle connection(s) on a shared pool", stats.MaxIdleClosed)
	}
}

// The convergence property. The counter sum is not the index's cardinality and
// never will be, and it errs in BOTH directions: no counter row describes the
// external-call bucket or nodes carrying no repo prefix, while a leftover
// empty-prefix row on a store that later became multi-repo IS summed on top of
// the per-repo rows. So the sum can sit on either side of the true cardinality.
// Measure growth against the BELIEVED count and a store on the high side is
// stale the instant the refresh finishes — forever, at every call site.
//
// The assertion is TERMINATION, not one-pass quiescence. Each family's anchor
// moves on its own family's completed refresh, so a store whose nodes and edges
// both outgrew their statistics legitimately spends one boundary per family
// (see plannerStatsBaseline). What must not happen is a store that keeps
// refreshing without ever reaching a quiet probe, so the loop is bounded and
// exceeding the bound is the failure.
func TestEnsurePlannerStatsFresh_ConvergesWhenCountersExceedCardinality(t *testing.T) {
	s := freshStatsStore(t, "stats_convergence.sqlite", 100)
	// 200 real nodes; counters that legitimately sum to twice that.
	writeIndexStateCounters(t, s, 0, "repo", 200, 100)
	writeIndexStateCounters(t, s, 0, "", 200, 100)

	// One boundary per stale family (nodes, then edges) plus the quiet probe
	// that proves it settled: anything past that is not converging.
	const maxPasses = 4
	refreshes := 0
	for pass := 1; ; pass++ {
		h := mustEnsure(t, s)
		if !h.Refreshed {
			if h.Stale {
				t.Fatalf("pass %d left the store stale without refreshing it: reason=%q", pass, h.Reason)
			}
			break
		}
		refreshes++
		if pass >= maxPasses {
			t.Fatalf("still refreshing on pass %d (reason=%q): the store is permanently stale and every "+
				"call site pays an ANALYZE forever", pass, h.Reason)
		}
	}
	if refreshes == 0 {
		t.Fatal("no pass refreshed: the fixture is not stale and the convergence rule is untested")
	}

	// Grow the counters slightly — far below another doubling, but enough that
	// the "already attempted this exact verdict" guard cannot be what stops
	// the next call. Only the growth baseline can.
	writeIndexStateCounters(t, s, 0, "repo", 201, 101)

	settled := mustEnsure(t, s)
	if settled.Refreshed {
		t.Fatalf("a converged store refreshed again (reason=%q): the store is permanently stale and "+
			"every call site pays an ANALYZE forever", settled.Reason)
	}
	if settled.Stale {
		t.Errorf("store still reported stale after it converged: %q", settled.Reason)
	}
}

// Once the store has been seen fresh, growth is measured from the size it was
// then — not from the believed count. The two are not comparable in either
// direction: no counter row describes the external-call bucket or nodes
// carrying no repo prefix, while a leftover empty-prefix row IS summed on top
// of the per-repo rows, so the sum can sit on either side of the true
// cardinality — and ANALYZE's leading token is itself an extrapolation under
// analysis_limit. Anchoring on first sight is what keeps the rule a statement
// about the store GROWING rather than about the two numbers disagreeing.
func TestEnsurePlannerStatsFresh_MeasuresGrowthFromTheFirstProbe(t *testing.T) {
	s := freshStatsStore(t, "stats_baseline_anchor.sqlite", 100)
	// Counters that already exceed the believed 200 without reaching the
	// factor. Nothing to repair — but the anchor now sits at 300, not 200.
	writeIndexStateCounters(t, s, 0, "repo", 300, 150)
	if first := mustEnsure(t, s); first.Refreshed || first.Stale {
		t.Fatalf("fixture was not fresh: refreshed=%v reason=%q", first.Refreshed, first.Reason)
	}

	// Past twice the believed count, short of twice the anchor.
	writeIndexStateCounters(t, s, 0, "repo", 500, 250)
	second := mustEnsure(t, s)
	if second.Refreshed {
		t.Fatalf("refreshed at %d nodes against an anchor of 300: growth is being measured from the "+
			"believed count (%d), which the counter sum is not comparable to (reason=%q)",
			second.Nodes.Actual, second.Nodes.Believed, second.Reason)
	}

	// Past twice the anchor it must fire.
	writeIndexStateCounters(t, s, 0, "repo", 600, 300)
	if third := mustEnsure(t, s); !third.Refreshed {
		t.Fatalf("did not refresh at twice the anchor: stale=%v reason=%q", third.Stale, third.Reason)
	}
}

// baselineSnapshot reads the growth high-water mark a verdict is measured
// against. It is package-private state on purpose — the mechanism converges
// only because nothing outside it can move the anchor — so a test reads it
// directly rather than through a published field.
func baselineSnapshot(s *Store) plannerStatsBaseline {
	s.plannerStatsMu.Lock()
	defer s.plannerStatsMu.Unlock()
	return s.plannerStatsBaseline
}

// A refresh that FAILS is not a refresh. The anti-loop guard still has to arm,
// or a store whose ANALYZE cannot succeed pays for the attempt at every call
// site forever — but everything else the ledger holds is a claim that
// sqlite_stat1 now describes a store of this size, and a failed ANALYZE earns
// none of it. Stamp it anyway and index_health publishes a last_refresh_at for
// a rebuild that never happened, while the growth baseline jumps to the totals
// that triggered the failure and swallows the next verdict over a store whose
// statistics are still wrong.
func TestEnsurePlannerStatsFresh_FailedRefreshRecordsOnlyTheAttempt(t *testing.T) {
	s := freshStatsStore(t, "stats_failed_refresh.sqlite", 100)
	if first := mustEnsure(t, s); first.Refreshed || first.Stale {
		t.Fatalf("fixture was not fresh: refreshed=%v reason=%q", first.Refreshed, first.Reason)
	}
	anchor := baselineSnapshot(s)
	if !anchor.seeded || anchor.nodes != 200 {
		t.Fatalf("baseline = %+v, want the fixture's 200 nodes anchored", anchor)
	}

	// Grow past the factor, then make the ANALYZE itself fail. The writer pool
	// is a single connection with no lifetime limit, so query_only sticks to
	// the one connection refreshPlannerStatsLocked runs on — and it fails the
	// ANALYZE without corrupting anything the rest of the test reads.
	seedNamedGoReceiverFixture(s, "b", 100)
	writeIndexStateCounters(t, s, 0, "repo", 400, 200)
	if _, err := s.writerDB.Exec(`PRAGMA query_only = ON`); err != nil {
		t.Fatalf("arm the failing writer: %v", err)
	}

	failed, err := s.EnsurePlannerStatsFresh(context.Background())
	if err == nil {
		t.Fatalf("refresh succeeded against a read-only writer: refreshed=%v reason=%q",
			failed.Refreshed, failed.Reason)
	}
	if failed.Refreshed {
		t.Error("a refresh that returned an error still reported Refreshed")
	}
	if !failed.LastRefreshAt.IsZero() {
		t.Errorf("failed refresh stamped the ledger at %v (reason %q); index_health would report "+
			"statistics rebuilt at a moment they were not", failed.LastRefreshAt, failed.LastRefreshReason)
	}

	after := mustHealth(t, s)
	if !after.LastRefreshAt.IsZero() || after.LastRefreshReason != "" {
		t.Errorf("ledger = %q at %v after a failed refresh, want it untouched",
			after.LastRefreshReason, after.LastRefreshAt)
	}
	if after.Refreshes != 0 {
		t.Errorf("a failed refresh was counted as %d refresh(es)", after.Refreshes)
	}
	if got := baselineSnapshot(s); got != anchor {
		t.Errorf("growth baseline moved to %+v on a failed refresh, want %+v; the next verdict over a "+
			"store whose statistics are still wrong would be measured from a size it never had", got, anchor)
	}
	if !after.Stale {
		t.Errorf("the verdict was cleared by a refresh that failed: reason=%q", after.Reason)
	}

	// The guard did arm: the same verdict over a store that has not grown
	// since is not retried at the next call site.
	settled := mustEnsure(t, s)
	if settled.Refreshed || !strings.HasPrefix(settled.Reason, "settled:") {
		t.Errorf("second call: refreshed=%v reason=%q, want a settled: skip", settled.Refreshed, settled.Reason)
	}

	// And it re-arms once the guard key's totals DOUBLE: a store that grew
	// materially retries, and a retry that works stamps everything the failure
	// did not. The guard was armed at 400/200, so the retry is owed at 800/400
	// and not before — see plannerStatsAttemptSettled.
	if _, err := s.writerDB.Exec(`PRAGMA query_only = OFF`); err != nil {
		t.Fatalf("disarm the failing writer: %v", err)
	}
	seedNamedGoReceiverFixture(s, "c", 200)
	writeIndexStateCounters(t, s, 0, "repo", 800, 400)
	retried := mustEnsure(t, s)
	if !retried.Refreshed {
		t.Fatalf("a grown store never retried after an earlier failure: stale=%v reason=%q",
			retried.Stale, retried.Reason)
	}
	if retried.LastRefreshAt.IsZero() || retried.LastRefreshReason != retried.Reason {
		t.Errorf("successful retry left the ledger at %q/%v, want the verdict it acted on",
			retried.LastRefreshReason, retried.LastRefreshAt)
	}
	if got := baselineSnapshot(s); got.nodes != 800 {
		t.Errorf("baseline = %+v after a successful refresh, want the 800 totals that triggered it", got)
	}
}

// Present is what lets a reporter tell "this index holds nothing" apart from
// "this index is not here". index_health omits the receiver block entirely on
// the second, because a believed=0 / actual=0 receiver figure reads as exactly
// the poisoned near-zero state issue #651 is about.
func TestPlannerStatsHealth_ReportsSchemaPresence(t *testing.T) {
	s := freshStatsStore(t, "stats_presence.sqlite", 40)

	h := mustHealth(t, s)
	if !h.Nodes.Present || !h.Edges.Present || !h.Receivers.Present {
		t.Fatalf("fixture reported an absent index: nodes=%v edges=%v receivers=%v",
			h.Nodes.Present, h.Edges.Present, h.Receivers.Present)
	}

	if _, err := s.writerDB.Exec(`DROP INDEX ` + quoteSQLiteIdentifier(plannerStatsReceiverIndex)); err != nil {
		t.Fatalf("drop %s: %v", plannerStatsReceiverIndex, err)
	}
	dropped := mustHealth(t, s)
	if dropped.Receivers.Present {
		t.Error("the receiver index reported present after it was dropped from the schema")
	}
	if !dropped.Nodes.Present || !dropped.Edges.Present {
		t.Errorf("dropping the receiver index marked unrelated relations absent: nodes=%v edges=%v",
			dropped.Nodes.Present, dropped.Edges.Present)
	}
	if dropped.Stale {
		t.Errorf("an index the schema does not hold produced a verdict: %q", dropped.Reason)
	}
}

// The bulk window returns before any presence can be established, and the
// honest answer there is that nothing is known: the droppable critical indexes
// physically are not in the schema for its duration.
func TestPlannerStatsHealth_MarksEverythingAbsentInsideABulkWindow(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stats_bulk_presence.sqlite")
	s := openStatsRepairStore(t, path)
	if !s.BeginCoordinatedBulkLoad() {
		t.Fatal("coordinated bulk load did not engage on an empty on-disk store")
	}
	t.Cleanup(func() { _ = s.EndCoordinatedBulkLoad() })

	h := mustHealth(t, s)
	if h.Reason != plannerStatsBulkWindowReason {
		t.Fatalf("reason = %q, want %q", h.Reason, plannerStatsBulkWindowReason)
	}
	if h.Nodes.Present || h.Edges.Present || h.Receivers.Present {
		t.Errorf("a bulk window reported an index present: nodes=%v edges=%v receivers=%v",
			h.Nodes.Present, h.Edges.Present, h.Receivers.Present)
	}
}

// deleteIndexStateCounters removes one generation's counter row, which is what
// retiring a view generation does: repo_index_state is a view_gen sidecar and
// the row is swept with the payload.
func deleteIndexStateCounters(t *testing.T, s *Store, viewGen int64) {
	t.Helper()
	if _, err := s.writerDB.Exec(`DELETE FROM repo_index_state WHERE view_gen = ?`, viewGen); err != nil {
		t.Fatalf("delete repo_index_state(view_gen=%d): %v", viewGen, err)
	}
}

// growthWorkList is the index set a nodes-growth verdict is scoped to on this
// store. Tests assert against it rather than a hard-coded count so adding a
// critical index to plannerStatsIndexQuery cannot silently break them.
func growthWorkList(t *testing.T, s *Store, reason string) []string {
	t.Helper()
	present, err := s.plannerStatsPresentIndexList(context.Background())
	if err != nil {
		t.Fatalf("list present critical indexes: %v", err)
	}
	work := plannerStatsWorkList(reason, present, s.plannerStatsIndexesWithStats(context.Background()))
	if len(work) == 0 {
		t.Fatalf("verdict %q scoped to no indexes; present=%v", reason, present)
	}
	return work
}

// growStoreToDouble adds a second disjoint batch and the counters that describe
// it, producing a growth:nodes_by_kind verdict on a store seeded with 100 types.
// BOTH families double, so the store owes two consecutive refreshes: see
// growNodesOnlyToDouble for the fixture a test wanting exactly one uses.
func growStoreToDouble(t *testing.T, s *Store) {
	t.Helper()
	seedNamedGoReceiverFixture(s, "b", 100)
	writeIndexStateCounters(t, s, 0, "repo", 400, 200)
}

// growNodesOnlyToDouble doubles the node corpus and leaves the edges exactly
// where they were, so a store seeded with 100 types owes exactly ONE refresh.
//
// Tests that count gate holds or refreshes across a boundary need that. Each
// family's growth anchor moves only on its own family's completed pass
// (plannerStatsBaseline), so a store whose nodes AND edges both doubled
// correctly refreshes twice — nodes at one boundary, edges at the next — and a
// test asserting "exactly one caller refreshed" would then be asserting the
// scheduling of two verdicts rather than the property it is about.
func growNodesOnlyToDouble(t *testing.T, s *Store) {
	t.Helper()
	var nodes []*graph.Node
	for i := 0; i < 200; i++ {
		file := fmt.Sprintf("repo/nodesonly/p%03d/types.go", i)
		name := fmt.Sprintf("nT%03d", i)
		nodes = append(nodes, &graph.Node{
			ID: file + "::" + name, Name: name, Kind: graph.KindType,
			FilePath: file, Language: "go", RepoPrefix: "repo",
		})
	}
	s.AddBatch(nodes, nil)
	writeIndexStateCounters(t, s, 0, "repo", 400, 100)
}

// T1. Every call site reaches this holding a wider lock — the process-global
// reach topology writer gate at the indexer sites, the shared batch mutation
// gate plus a repository lane on the watcher path, the resolver's ResolveMutex
// on the resolve arm. Waiting on the store's write gate here holds all of them
// for the wait: reach readers give up rather than queue, so MCP answers go
// empty, other repositories' lanes stall, and the bounded-gate writers that
// give up after 15 s drop their batches outright. So a busy gate must end the
// pass immediately, and must leave the verdict standing for the next boundary.
func TestEnsurePlannerStatsFresh_DefersWhenWriterBusy(t *testing.T) {
	s := freshStatsStore(t, "stats_writer_busy.sqlite", 100)
	if first := mustEnsure(t, s); first.Refreshed || first.Stale {
		t.Fatalf("fixture was not fresh: refreshed=%v reason=%q", first.Refreshed, first.Reason)
	}
	growStoreToDouble(t, s)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	s.writeMu.Lock()
	started := time.Now()
	busy, err := s.EnsurePlannerStatsFresh(ctx)
	elapsed := time.Since(started)
	s.writeMu.Unlock()

	if err != nil {
		t.Fatalf("a busy writer is not a failure: %v", err)
	}
	// The bound is loose because the property is "did not QUEUE", not "was
	// fast". A deferred pass still pays the two health probes, the
	// present-index list and the stat-row set — sixteen-odd read-pool queries
	// — and under -race on a loaded runner those can exceed a second on their
	// own. Queuing on a gate this test holds for the whole call would never
	// return at all, so five seconds separates the two states cleanly while
	// the reason assertions below carry the real weight.
	if elapsed > 5*time.Second {
		t.Fatalf("waited %s for the write gate; the refresh must defer, not queue — every call site "+
			"holds a wider gate while it waits", elapsed)
	}
	if busy.Refreshed {
		t.Error("reported a refresh it could not have performed under a held gate")
	}
	if !strings.HasPrefix(busy.Reason, plannerStatsWriterBusyReason) {
		t.Errorf("reason = %q, want a %q verdict", busy.Reason, plannerStatsWriterBusyReason)
	}
	if !busy.Stale {
		t.Error("a deferred pass cleared the verdict; the store is still stale and the next boundary " +
			"has to act on it")
	}

	after := mustHealth(t, s)
	if !after.LastRefreshAt.IsZero() || after.Refreshes != 0 {
		t.Errorf("a deferred pass stamped the ledger: at=%v reason=%q refreshes=%d",
			after.LastRefreshAt, after.LastRefreshReason, after.Refreshes)
	}

	// The anti-loop guard must NOT have armed: the pass did nothing wrong and
	// the very next boundary has to be free to retry it.
	retry := mustEnsure(t, s)
	if !retry.Refreshed {
		t.Fatalf("the boundary after a deferred pass did not refresh: reason=%q; a deferred pass armed "+
			"the settled guard and suppressed the retry", retry.Reason)
	}
}

// T2. One index per gate hold. Holding the gate across the whole work list is
// indistinguishable from the old shape at every scale that matters — the wider
// gates above are held for the sum, not the maximum — and nothing externally
// observable tells the two apart, so the count of acquisitions is the assertion.
func TestEnsurePlannerStatsFresh_ReleasesGateBetweenIndexes(t *testing.T) {
	s := freshStatsStore(t, "stats_gate_per_index.sqlite", 100)
	if first := mustEnsure(t, s); first.Refreshed || first.Stale {
		t.Fatalf("fixture was not fresh: refreshed=%v reason=%q", first.Refreshed, first.Reason)
	}
	growStoreToDouble(t, s)

	work := growthWorkList(t, s, "growth:"+plannerStatsNodesIndex)
	if len(work) < 2 {
		t.Fatalf("the nodes family holds %d critical index(es); the per-index property is untestable", len(work))
	}
	before := s.plannerStatsGateHolds.Load()

	h := mustEnsure(t, s)
	if !h.Refreshed {
		t.Fatalf("grown store did not refresh: reason=%q", h.Reason)
	}
	if got := s.plannerStatsGateHolds.Load() - before; got != int64(len(work)) {
		t.Errorf("took the write gate %d time(s) for %d index(es) (%v); one hold means the gate — and "+
			"every wider gate above it — was held across the whole ANALYZE", got, len(work), work)
	}
}

// T3. A "growth:" verdict is a statement about a TABLE's cardinality: nodes
// grew, so every statistics row describing an index on nodes now understates
// it, and no row describing an index on edges does. Re-analyzing the edge
// indexes anyway doubles a gate-holding cost that is paid against live traffic.
func TestEnsurePlannerStatsFresh_RefreshesOnlyTheNamedFamily(t *testing.T) {
	s := freshStatsStore(t, "stats_family_scope.sqlite", 100)
	if first := mustEnsure(t, s); first.Refreshed || first.Stale {
		t.Fatalf("fixture was not fresh: refreshed=%v reason=%q", first.Refreshed, first.Reason)
	}

	edgeIndexes := []string{"edges_by_kind", "edges_by_from_line", "edges_by_from_line_kind"}
	before := map[string]string{}
	for _, idx := range edgeIndexes {
		stat, ok := statRowFor(t, s, idx)
		if !ok {
			t.Fatalf("fixture left no stat row for %s", idx)
		}
		before[idx] = stat
	}

	growStoreToDouble(t, s)
	h := mustEnsure(t, s)
	if !h.Refreshed {
		t.Fatalf("grown store did not refresh: reason=%q", h.Reason)
	}
	if !strings.HasPrefix(h.Reason, "growth:nodes") {
		t.Fatalf("verdict = %q, want a nodes-family growth verdict", h.Reason)
	}

	for _, idx := range edgeIndexes {
		got, ok := statRowFor(t, s, idx)
		if !ok || got != before[idx] {
			t.Errorf("a nodes verdict rewrote %s: %q -> %q (present=%v)", idx, before[idx], got, ok)
		}
	}
	nodesStat, ok := statRowFor(t, s, plannerStatsNodesIndex)
	if !ok {
		t.Fatal("refresh left no nodes_by_kind stat row")
	}
	if got := statRowCount(t, nodesStat); got < 300 {
		t.Errorf("nodes_by_kind believes %d after the refresh, want the grown corpus (>=300)", got)
	}
}

// T4. Two pipeline boundaries can reach this at once — a watcher reconcile and
// a generation publish, say. The old shape serialised them on the write gate,
// which the cooperative shape deliberately no longer does, so a single
// in-flight claim is what stops both from analyzing the same family. The loser
// must NOT wait: it holds a wider gate of its own, and the verdict it would
// act on is the one already being acted on.
//
// The fixture grows the NODES only. A store that outgrew both families owes a
// refresh per family, so a loser that happened to run after the winner finished
// would legitimately refresh the edges — and "exactly one of two callers
// refreshed" would then be measuring goroutine scheduling rather than the claim.
func TestEnsurePlannerStatsFresh_SingleInFlight(t *testing.T) {
	s := freshStatsStore(t, "stats_single_inflight.sqlite", 100)
	if first := mustEnsure(t, s); first.Refreshed || first.Stale {
		t.Fatalf("fixture was not fresh: refreshed=%v reason=%q", first.Refreshed, first.Reason)
	}
	growNodesOnlyToDouble(t, s)

	work := growthWorkList(t, s, "growth:"+plannerStatsNodesIndex)

	// The claim, deterministically: a refresh is in flight, so this caller
	// must decline immediately rather than analyze the same family a second
	// time. Racing two goroutines can only ever observe this by luck; setting
	// the flag observes it every run.
	s.plannerStatsRefreshInFlight.Store(true)
	holdsAtClaim := s.plannerStatsGateHolds.Load()
	declined := mustEnsure(t, s)
	s.plannerStatsRefreshInFlight.Store(false)
	if declined.Refreshed {
		t.Error("refreshed while another refresh was in flight; both callers analyze the same family " +
			"and the second pays a whole extra round of gate holds")
	}
	if declined.Reason != plannerStatsRacedReason {
		t.Errorf("reason = %q, want %q", declined.Reason, plannerStatsRacedReason)
	}
	if got := s.plannerStatsGateHolds.Load() - holdsAtClaim; got != 0 {
		t.Errorf("a declined caller took the write gate %d time(s), want 0", got)
	}

	beforeHolds := s.plannerStatsGateHolds.Load()
	beforeRefreshes := mustHealth(t, s).Refreshes

	start := make(chan struct{})
	results := make(chan graph.PlannerStatsFreshness, 2)
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() {
			<-start
			h, err := s.EnsurePlannerStatsFresh(context.Background())
			errs <- err
			results <- h
		}()
	}
	close(start)
	refreshed := 0
	for i := 0; i < 2; i++ {
		if err := <-errs; err != nil {
			t.Errorf("concurrent ensure: %v", err)
		}
		if h := <-results; h.Refreshed {
			refreshed++
		}
	}

	if refreshed != 1 {
		t.Errorf("%d of 2 concurrent callers refreshed, want exactly 1", refreshed)
	}
	if got := mustHealth(t, s).Refreshes - beforeRefreshes; got != 1 {
		t.Errorf("the store recorded %d refresh(es) for one growth verdict, want 1", got)
	}
	if got := s.plannerStatsGateHolds.Load() - beforeHolds; got != int64(len(work)) {
		t.Errorf("took the write gate %d time(s) for a %d-index work list; two callers each analyzed "+
			"the whole family", got, len(work))
	}
}

// T5. The anti-loop guard re-arms on a DOUBLING, not on any movement. It exists
// for a verdict a refresh cannot clear, and such a verdict re-fires at all five
// call sites; re-arming on the next indexed file would restore the loop it was
// written to prevent. Once per doubling matches the growth baseline's own
// convergence argument.
func TestEnsurePlannerStatsFresh_SettledGuardReArmsOnlyOnADoubling(t *testing.T) {
	s := freshStatsStore(t, "stats_settled_factor.sqlite", 100)
	if first := mustEnsure(t, s); first.Refreshed || first.Stale {
		t.Fatalf("fixture was not fresh: refreshed=%v reason=%q", first.Refreshed, first.Reason)
	}

	growStoreToDouble(t, s)
	if _, err := s.writerDB.Exec(`PRAGMA query_only = ON`); err != nil {
		t.Fatalf("arm the failing writer: %v", err)
	}
	if _, err := s.EnsurePlannerStatsFresh(context.Background()); err == nil {
		t.Fatal("refresh succeeded against a read-only writer")
	}
	if _, err := s.writerDB.Exec(`PRAGMA query_only = OFF`); err != nil {
		t.Fatalf("disarm the failing writer: %v", err)
	}

	// Ten percent past the totals the guard armed at. A daemon indexing one
	// changed file moves the counters this much.
	writeIndexStateCounters(t, s, 0, "repo", 440, 220)
	nudged := mustEnsure(t, s)
	if nudged.Refreshed || !strings.HasPrefix(nudged.Reason, "settled:") {
		t.Errorf("10%% counter growth re-armed the guard: refreshed=%v reason=%q; a verdict no refresh "+
			"can clear would be retried at every call site on every indexed file",
			nudged.Refreshed, nudged.Reason)
	}

	// A doubling is a materially different store, and worth paying once more.
	writeIndexStateCounters(t, s, 0, "repo", 800, 400)
	doubled := mustEnsure(t, s)
	if !doubled.Refreshed {
		t.Fatalf("a doubled store never retried after an earlier failure: stale=%v reason=%q",
			doubled.Stale, doubled.Reason)
	}
}

// T6. The counter totals fall as well as rise: a retired view generation's
// counter row is swept with its payload, and an untracked repository's goes
// with the repository. A high-water baseline frozen at the largest size the
// store ever reported would then demand it climb back AND double again before
// any growth registered — on a workspace that churns generations, a store that
// never re-analyzes.
func TestEnsurePlannerStatsFresh_BaselineDecaysWithTheCounters(t *testing.T) {
	s := freshStatsStore(t, "stats_baseline_decay.sqlite", 300)
	// Two generations summing to the corpus the statistics describe.
	writeIndexStateCounters(t, s, 0, "repo", 200, 100)
	writeIndexStateCounters(t, s, 7, "repo", 400, 200)
	if first := mustEnsure(t, s); first.Refreshed || first.Stale {
		t.Fatalf("fixture was not fresh: refreshed=%v reason=%q", first.Refreshed, first.Reason)
	}
	if anchor := baselineSnapshot(s); !anchor.seeded || anchor.nodes != 600 {
		t.Fatalf("baseline = %+v, want the fixture's 600 nodes anchored", anchor)
	}

	// Retirement: the generation's counter row goes with its payload.
	deleteIndexStateCounters(t, s, 7)
	retired := mustHealth(t, s)
	if retired.Nodes.Actual != 200 {
		t.Fatalf("counters read %d nodes after the retirement, want 200", retired.Nodes.Actual)
	}
	if retired.Stale {
		t.Fatalf("a shrinking store reported stale: %q", retired.Reason)
	}
	if got := baselineSnapshot(s); got.nodes != 200 || got.edges != 100 {
		t.Errorf("baseline = %+v after the totals fell to 200/100, want it lowered to the new floor", got)
	}

	// Growth measured from the new floor, well below the retired high-water
	// mark of 600.
	writeIndexStateCounters(t, s, 0, "repo", 400, 200)
	grown := mustEnsure(t, s)
	if !grown.Refreshed {
		t.Fatalf("a store that doubled from its post-retirement floor did not refresh: stale=%v reason=%q; "+
			"growth is still being measured from a size the store no longer has",
			grown.Stale, grown.Reason)
	}
}

// T7. Both cold-path callers of stampPlannerStatsRefresh hold writeMu with a
// writer connection pinned, and the stamp reads the counters through the READ
// pool. On an in-memory store the two pools are one handle capped at a single
// connection, so that read would block on the connection its own caller holds.
// The guard is the handle comparison, not the coincidence that bulk loads do
// not engage in memory.
func TestStampPlannerStatsRefresh_SkipsTheCounterReadOnASharedHandle(t *testing.T) {
	s, err := Open(":memory:")
	if err != nil {
		t.Fatalf("open in-memory store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if s.db != s.writerDB {
		t.Fatal("in-memory store no longer shares one pool; this test's premise is gone")
	}
	seedNamedGoReceiverFixture(s, "a", 5)

	ctx := context.Background()
	// Pin the only connection, exactly as a cold finalize does.
	conn, err := s.writerDB.Conn(ctx)
	if err != nil {
		t.Fatalf("pin the writer connection: %v", err)
	}
	defer func() { _ = conn.Close() }()

	done := make(chan struct{})
	go func() {
		defer close(done)
		s.stampPlannerStatsRefresh(ctx, "cold_load_finalize")
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("stampPlannerStatsRefresh blocked on the shared one-connection pool its caller already " +
			"holds; the counter read must be skipped when s.db == s.writerDB")
	}

	if got := baselineSnapshot(s); got.seeded {
		t.Errorf("baseline = %+v, want it left unseeded: no counters were read", got)
	}
	if s.plannerStatsRefreshes.Load() != 1 {
		t.Errorf("the stamp recorded %d refresh(es), want 1", s.plannerStatsRefreshes.Load())
	}
	s.plannerStatsMu.Lock()
	reason := s.plannerStatsLastReason
	s.plannerStatsMu.Unlock()
	if reason != "cold_load_finalize" {
		t.Errorf("ledger reason = %q, want the stamp's own", reason)
	}
}

// The work-list rule on its own, so the family mapping is pinned independently
// of any store fixture.
func TestPlannerStatsWorkList(t *testing.T) {
	present := []string{
		"edges_by_from_line", "edges_by_from_line_kind", "edges_by_kind",
		"nodes_by_file", "nodes_by_kind", "nodes_go_receiver_type",
	}
	for _, tc := range []struct {
		name    string
		reason  string
		hasStat map[string]bool
		want    []string
	}{
		{
			// The verdict's own index leads, then the rest of its family in
			// schema order. Leading with it is what lets a pass that defers
			// after ONE index still clear the verdict it was called about —
			// and edges_by_kind is ~44 MiB of pages against
			// edges_by_from_line_kind's 273-534 MiB, so it is also the most
			// planning value per second of held gate.
			name:   "nodes growth takes the named index first, then its family",
			reason: "growth:nodes_by_kind believed=200 actual=400 base=200",
			want:   []string{"nodes_by_kind", "nodes_by_file", "nodes_go_receiver_type"},
		},
		{
			name:   "the nodes fallback index names the same family and still leads",
			reason: "growth:nodes_by_file believed=200 actual=400 base=200",
			want:   []string{"nodes_by_file", "nodes_by_kind", "nodes_go_receiver_type"},
		},
		{
			name:   "edges growth takes the named index first, then its family",
			reason: "growth:edges_by_kind believed=100 actual=300 base=100",
			want:   []string{"edges_by_kind", "edges_by_from_line", "edges_by_from_line_kind"},
		},
		{
			name:    "a missing row backfills every index without statistics, named one first",
			reason:  "missing:nodes_go_receiver_type",
			hasStat: map[string]bool{"edges_by_kind": true, "nodes_by_kind": true},
			want:    []string{"nodes_go_receiver_type", "edges_by_from_line", "edges_by_from_line_kind", "nodes_by_file"},
		},
		{
			name:   "a store with no statistics at all backfills everything present",
			reason: "missing:nodes_by_kind",
			want: []string{
				"nodes_by_kind",
				"edges_by_from_line", "edges_by_from_line_kind", "edges_by_kind",
				"nodes_by_file", "nodes_go_receiver_type",
			},
		},
		{
			// A "missing:" verdict is a believed cardinality of ZERO, which a
			// row that exists and reads zero reports just as an absent row
			// does — the '0 0 0 0 0' row an older engine wrote for an empty
			// partial index, or the smallest of two rows on a store whose
			// sqlite_stat1 has no UNIQUE constraint. Scoped on hasStat alone
			// the work list drops the one index the verdict is about, the
			// pass stamps a success, and the verdict outlives it.
			name:   "a missing verdict always re-analyzes the index it named",
			reason: "missing:nodes_by_kind",
			hasStat: map[string]bool{
				"edges_by_from_line": true, "edges_by_from_line_kind": true, "edges_by_kind": true,
				"nodes_by_file": true, "nodes_by_kind": true, "nodes_go_receiver_type": true,
			},
			want: []string{"nodes_by_kind"},
		},
		{
			// The verdict's own index is prepended only when the SCHEMA holds
			// it: a bulk window that dropped it must not put it back on a work
			// list, because no ANALYZE can write a row for an index that is
			// not there and the pass would fail forever.
			name:   "an index absent from the schema is never scheduled",
			reason: "missing:nodes_by_repo",
			hasStat: map[string]bool{
				"edges_by_from_line": true, "edges_by_from_line_kind": true, "edges_by_kind": true,
				"nodes_by_file": true, "nodes_by_kind": true, "nodes_go_receiver_type": true,
			},
			want: nil,
		},
		{
			// plannerStatsRepairReason's verdicts belong to the Open-time
			// repair, which rebuilds every critical index through
			// refreshPlannerStatsOnConn and never consults a work list. They
			// cannot reach this function — plannerStatsStaleReason produces
			// only "missing:" and "growth:" — so they get the safe fallback
			// rather than a dead special case of their own.
			name:    "an Open-time repair verdict falls back to everything present",
			reason:  "stale_stat:nodes_go_receiver_type",
			hasStat: map[string]bool{"nodes_go_receiver_type": true},
			want:    present,
		},
		{
			name:   "an unrecognised verdict falls back to everything present",
			reason: "no_stats",
			want:   present,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := plannerStatsWorkList(tc.reason, present, tc.hasStat)
			if len(got) != len(tc.want) {
				t.Fatalf("work list = %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("work list = %v, want %v", got, tc.want)
				}
			}
		})
	}
}

// waitForGateHolds blocks until the cooperative refresh loop has taken the
// store's write gate at least `want` times since `before`, or fails.
//
// The counter is incremented at the ACQUISITION site, so crossing it proves the
// pass is inside a hold — which is the only window in which a test can act on a
// pass that is mid-index.
func waitForGateHolds(t *testing.T, s *Store, before, want int64) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if s.plannerStatsGateHolds.Load()-before >= want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("the refresh never took the write gate %d time(s) (holds=%d)",
		want, s.plannerStatsGateHolds.Load()-before)
}

// statRowsFor snapshots the statistics rows of a whole work list, so a test can
// say WHICH indexes a partial pass rewrote.
func statRowsFor(t *testing.T, s *Store, indexes []string) map[string]string {
	t.Helper()
	rows := make(map[string]string, len(indexes))
	for _, idx := range indexes {
		stat, ok := statRowFor(t, s, idx)
		if !ok {
			// A partial index that holds nothing legitimately has no row
			// (ANALYZE writes none for it, and absence beats a zero row).
			if spec := plannerStatsIndexProbes[idx]; spec.partial {
				var hasRows bool
				if err := s.db.QueryRow(spec.existsQuery(idx)).Scan(&hasRows); err == nil && !hasRows {
					continue
				}
			}
			t.Fatalf("fixture left no stat row for %s", idx)
		}
		rows[idx] = stat
	}
	return rows
}

// T8. A pass that stops mid work list must leave a RESUMABLE position behind.
//
// Every stop this file admits — a busy gate, a spent budget, an expired
// context, a per-index timeout — leaves indexes it never reached. Without a
// cursor the next boundary rebuilds the same work list and starts at the HEAD,
// so a store whose gate is contended at the same point every time re-analyzes
// index 1 forever and never reaches index 3: a mechanism that cannot converge,
// which is the property the whole file claims.
//
// MUTANT: delete the cursor (make plannerStatsPending return the full list).
// Phase B's reason then names the FIRST index rather than the second, and phase
// C takes the gate len(work) times instead of len(work)-1.
//
// The mid-list stop is produced with the pass budget rather than by racing a
// goroutine against the gate: the loop releases the gate between indexes, so a
// helper waiting for holds==1 and then locking is only PROBABLY parked before
// the pass re-acquires. Phase B still exercises the writer-busy stop itself,
// deterministically, on the resumed pass.
func TestEnsurePlannerStatsFresh_ResumesAfterAMidListDeferral(t *testing.T) {
	s := freshStatsStore(t, "stats_resume_cursor.sqlite", 100)
	if first := mustEnsure(t, s); first.Refreshed || first.Stale {
		t.Fatalf("fixture was not fresh: refreshed=%v reason=%q", first.Refreshed, first.Reason)
	}
	growStoreToDouble(t, s)

	work := growthWorkList(t, s, "growth:"+plannerStatsNodesIndex)
	if len(work) < 2 {
		t.Fatalf("the nodes family holds %d critical index(es); resumption is untestable", len(work))
	}
	if work[0] != plannerStatsNodesIndex {
		t.Fatalf("work list = %v, want the verdict's own index first", work)
	}
	before := statRowsFor(t, s, work)

	// Phase A: a pass that stops after exactly one index.
	restore := plannerStatsPassBudget
	plannerStatsPassBudget = 0
	t.Cleanup(func() { plannerStatsPassBudget = restore })

	holdsA := s.plannerStatsGateHolds.Load()
	partial := mustEnsure(t, s)
	if partial.Refreshed {
		t.Fatal("a pass that spent its budget after one index reported a completed refresh")
	}
	if want := plannerStatsBudgetReason + work[1]; partial.Reason != want {
		t.Errorf("reason = %q, want %q", partial.Reason, want)
	}
	if got := s.plannerStatsGateHolds.Load() - holdsA; got != 1 {
		t.Errorf("took the write gate %d time(s) before the budget stopped it, want 1", got)
	}
	if after, _ := statRowFor(t, s, work[0]); after == before[work[0]] {
		t.Errorf("the pass stamped nothing over %s: row still %q; it analyzed no index at all",
			work[0], before[work[0]])
	}
	for _, idx := range work[1:] {
		if got, _ := statRowFor(t, s, idx); got != before[idx] {
			t.Errorf("a budget-stopped pass rewrote %s: %q -> %q", idx, before[idx], got)
		}
	}
	if ledger := mustHealth(t, s); !ledger.LastRefreshAt.IsZero() || ledger.Refreshes != 0 {
		t.Errorf("a deferred pass stamped the ledger: at=%v reason=%q refreshes=%d",
			ledger.LastRefreshAt, ledger.LastRefreshReason, ledger.Refreshes)
	}
	plannerStatsPassBudget = restore

	// Phase B: the next boundary resumes AFTER the completed index. A held
	// write gate makes that observable without ambiguity — the reason names the
	// index the pass stopped at, and a pass that restarted at the head would
	// name work[0].
	holdsB := s.plannerStatsGateHolds.Load()
	s.writeMu.Lock()
	busy, err := s.EnsurePlannerStatsFresh(context.Background())
	s.writeMu.Unlock()
	if err != nil {
		t.Fatalf("a busy writer is not a failure: %v", err)
	}
	if want := plannerStatsWriterBusyReason + work[1]; busy.Reason != want {
		t.Errorf("reason = %q, want %q: the pass restarted at the head instead of resuming", busy.Reason, want)
	}
	if got := s.plannerStatsGateHolds.Load() - holdsB; got != 0 {
		t.Errorf("a pass that could not take the gate took it %d time(s)", got)
	}

	// Phase C: with the gate free the resumed pass finishes the REMAINDER and
	// completes — one hold per index still owed, not one per index in the list.
	holdsC := s.plannerStatsGateHolds.Load()
	done := mustEnsure(t, s)
	if !done.Refreshed {
		t.Fatalf("the resumed pass did not complete: stale=%v reason=%q", done.Stale, done.Reason)
	}
	if got, want := s.plannerStatsGateHolds.Load()-holdsC, int64(len(work)-1); got != want {
		t.Errorf("the resumed pass took the write gate %d time(s) for %d remaining index(es) (%v); "+
			"it re-analyzed indexes the earlier pass had already finished", got, want, work)
	}
	if done.LastRefreshAt.IsZero() || done.LastRefreshReason != done.Reason {
		t.Errorf("a completed pass left the ledger at %q/%v", done.LastRefreshReason, done.LastRefreshAt)
	}
	for _, idx := range work[1:] {
		if got, _ := statRowFor(t, s, idx); got == before[idx] {
			t.Errorf("%s was never analyzed by the resumed pass: row still %q", idx, before[idx])
		}
	}

	// And a completed pass retires the cursor, so an identical verdict later —
	// the same store grown the same way again — starts from the head.
	s.plannerStatsMu.Lock()
	cursor := s.plannerStatsCursor
	s.plannerStatsMu.Unlock()
	if cursor.key != "" || len(cursor.done) != 0 {
		t.Errorf("cursor = %+v after a completed pass, want it cleared", cursor)
	}
}

// T9. A caller that walks away mid-ANALYZE is a DEFERRAL, not a failure. Only a
// real ANALYZE error under a live context may arm the anti-loop guard: arming
// it on a cancellation would suppress the next boundary's refresh over a store
// whose statistics really are stale, and cancellations are routine (a daemon
// shutting a repository lane down mid-pass).
//
// The cancellation is landed INSIDE a gate hold deterministically, without any
// racing: the writer pool is a ONE-connection pool, so pinning that connection
// parks the pass in activeWriteConnLocked — after the gate was taken and the
// hold counted — until the context dies under it. That is the same branch a
// mid-statement interrupt reaches. The production check is on ctx.Err() rather
// than errors.Is(err, context.Canceled) not because the driver hides the
// cancellation — modernc does surface the context error on this shape — but
// because asking about the CALLER is what this branch is actually about, and it
// stays right whatever the driver's error wrapping does next.
//
// MUTANT: treat any error from the hold as a failure. notePlannerStatsAttempt
// then arms the settled guard and the follow-up call returns "settled:" instead
// of refreshing.
func TestEnsurePlannerStatsFresh_CancellationDefersAndDoesNotArmTheGuard(t *testing.T) {
	s := freshStatsStore(t, "stats_cancel_midflight.sqlite", 100)
	if first := mustEnsure(t, s); first.Refreshed || first.Stale {
		t.Fatalf("fixture was not fresh: refreshed=%v reason=%q", first.Refreshed, first.Reason)
	}
	growStoreToDouble(t, s)

	// Pin the sole writer connection. Nothing else in this test may touch
	// s.writerDB until it is released.
	pinned, err := s.writerDB.Conn(context.Background())
	if err != nil {
		t.Fatalf("pin the writer connection: %v", err)
	}
	released := false
	defer func() {
		if !released {
			_ = pinned.Close()
		}
	}()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	holdsBefore := s.plannerStatsGateHolds.Load()
	type ensureResult struct {
		health graph.PlannerStatsFreshness
		err    error
	}
	done := make(chan ensureResult, 1)
	go func() {
		health, err := s.EnsurePlannerStatsFresh(ctx)
		done <- ensureResult{health: health, err: err}
	}()

	waitForGateHolds(t, s, holdsBefore, 1)
	cancel()

	var got ensureResult
	select {
	case got = <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("the pass never returned after its context was canceled underneath a gate hold")
	}
	_ = pinned.Close()
	released = true

	if got.err != nil {
		t.Errorf("a canceled pass returned an error (%v); a caller that walked away is not a store defect", got.err)
	}
	if got.health.Refreshed {
		t.Error("a canceled pass reported a completed refresh")
	}
	if !strings.HasPrefix(got.health.Reason, plannerStatsCanceledReason) &&
		!strings.HasPrefix(got.health.Reason, plannerStatsTimeoutReason) {
		t.Errorf("reason = %q, want a %q (or %q) deferral",
			got.health.Reason, plannerStatsCanceledReason, plannerStatsTimeoutReason)
	}
	if ledger := mustHealth(t, s); !ledger.LastRefreshAt.IsZero() || ledger.Refreshes != 0 {
		t.Errorf("a canceled pass stamped the ledger: at=%v reason=%q refreshes=%d",
			ledger.LastRefreshAt, ledger.LastRefreshReason, ledger.Refreshes)
	}

	// The guard did NOT arm: the very next boundary is free to act on the
	// verdict the canceled pass left standing.
	retry := mustEnsure(t, s)
	if !retry.Refreshed {
		t.Fatalf("the boundary after a canceled pass did not refresh: reason=%q; the cancellation was "+
			"recorded as a failed attempt and armed the settled guard", retry.Reason)
	}
}

// The per-index ANALYZE timeout exists to keep ONE hold strictly inside the
// window the bounded-gate writers (reindex_set.go,
// unresolved_edge_identity_batches.go) give this gate before they DROP their
// batches. A timeout at or above that window would let a statistics refresh
// discard concurrent rebind work — a correctness loss paid for a planning
// refinement — so the relationship is asserted rather than left to a comment.
func TestPlannerStatsIndexTimeoutFitsInsideTheBusyRetryWindow(t *testing.T) {
	if plannerStatsIndexTimeout >= defaultSQLiteBusyRetryTimeout {
		t.Fatalf("plannerStatsIndexTimeout = %s, want strictly below the %s bounded-gate window",
			plannerStatsIndexTimeout, defaultSQLiteBusyRetryTimeout)
	}
	if plannerStatsPassBudget <= 0 {
		t.Fatalf("plannerStatsPassBudget = %s; a non-positive budget stops every pass after one index",
			plannerStatsPassBudget)
	}
	// The other end of the same range, and not idle: the timeout test drives
	// this var negative on purpose so the per-index context is dead the moment
	// it is created. A leaked override — or a negative default — would make
	// every ANALYZE time out before it started and no index would ever be
	// rebuilt.
	if plannerStatsIndexTimeout <= 0 {
		t.Fatalf("plannerStatsIndexTimeout = %s; a non-positive timeout expires every per-index "+
			"context at creation, so no ANALYZE ever runs", plannerStatsIndexTimeout)
	}

	// The constant is not what reindex_set.go and
	// unresolved_edge_identity_batches.go actually give the gate: both call
	// s.sqliteBusyRetryWindow(), which prefers a store's configured
	// busyRetryTimeout and only falls back to the constant. Asserting against
	// the constant alone would pass over a store whose window was configured
	// shorter than one ANALYZE, which is the store where a refresh hold starts
	// discarding concurrent rebind work.
	s := freshStatsStore(t, "stats_timeout_window.sqlite", 5)
	if window := s.sqliteBusyRetryWindow(); plannerStatsIndexTimeout >= window {
		t.Fatalf("plannerStatsIndexTimeout = %s, want strictly below this store's %s bounded-gate window",
			plannerStatsIndexTimeout, window)
	}
}

// T10. The per-index timeout is a DEFERRAL — until the same index has burned it
// plannerStatsIndexTimeoutRetries times in a row, at which point the pass takes
// the failure arm so a pathological index cannot cost a full timeout at every
// boundary for the life of the daemon.
//
// An ALREADY-EXPIRED timeout reaches the same branch a real one does: the
// per-index context is dead before the hold acquires its write connection, so
// the hold fails while the CALLER's context is still live — which is exactly
// what separates "this index ran out of time" from "the caller walked away".
func TestEnsurePlannerStatsFresh_PerIndexTimeoutDefersThenSettles(t *testing.T) {
	s := freshStatsStore(t, "stats_index_timeout.sqlite", 100)
	if first := mustEnsure(t, s); first.Refreshed || first.Stale {
		t.Fatalf("fixture was not fresh: refreshed=%v reason=%q", first.Refreshed, first.Reason)
	}
	growStoreToDouble(t, s)
	work := growthWorkList(t, s, "growth:"+plannerStatsNodesIndex)

	// NEGATIVE, not a tiny positive duration. Production feeds this straight
	// into context.WithTimeout, and context.WithDeadlineCause cancels
	// synchronously with DeadlineExceeded only when time.Until(deadline) <= 0
	// at CREATION; otherwise it arms a timer. A positive nanosecond relies on
	// the wall clock having advanced past now+1ns by the time WithDeadline
	// re-reads it — true on a nanosecond-resolution clock (macOS, Linux),
	// false on Windows, where the clock ticks in ~0.5-15 ms steps: the timer
	// gets armed instead and fires only after this fixture's tiny ANALYZE has
	// already succeeded, so no index ever times out and the pass reports
	// "growth:" rather than "timeout:". A deadline already in the past takes
	// the dur <= 0 branch on every clock, on every OS.
	restore := plannerStatsIndexTimeout
	plannerStatsIndexTimeout = -time.Second
	t.Cleanup(func() { plannerStatsIndexTimeout = restore })

	// The allowance: every timeout but the last is a plain deferral that leaves
	// the ledger and the anti-loop guard alone, so the next boundary retries.
	for attempt := 1; attempt < plannerStatsIndexTimeoutRetries; attempt++ {
		health, err := s.EnsurePlannerStatsFresh(context.Background())
		if err != nil {
			t.Fatalf("attempt %d: a timed-out index is a deferral, not a failure: %v", attempt, err)
		}
		if want := plannerStatsTimeoutReason + work[0]; health.Reason != want {
			t.Fatalf("attempt %d: reason = %q, want %q", attempt, health.Reason, want)
		}
		if ledger := mustHealth(t, s); !ledger.LastRefreshAt.IsZero() || ledger.Refreshes != 0 {
			t.Fatalf("attempt %d stamped the ledger: at=%v refreshes=%d",
				attempt, ledger.LastRefreshAt, ledger.Refreshes)
		}
	}

	// The last one arms the settled guard, exactly as a failed ANALYZE does.
	last, err := s.EnsurePlannerStatsFresh(context.Background())
	if err == nil {
		t.Fatalf("the %dth consecutive timeout on %s was still reported as a deferral (reason=%q); a "+
			"pathological index would cost a full timeout at every boundary forever",
			plannerStatsIndexTimeoutRetries, work[0], last.Reason)
	}
	if last.Refreshed {
		t.Error("a pass that never analyzed anything reported a refresh")
	}
	if ledger := mustHealth(t, s); !ledger.LastRefreshAt.IsZero() || ledger.Refreshes != 0 {
		t.Errorf("a failed pass stamped the ledger: at=%v refreshes=%d", ledger.LastRefreshAt, ledger.Refreshes)
	}
	settled := mustEnsure(t, s)
	if !strings.HasPrefix(settled.Reason, "settled:") {
		t.Errorf("reason = %q after the guard armed, want a settled: skip", settled.Reason)
	}
}

// T11. The stale path probes health twice, and the second probe skips the
// capped receiver count — it only has to answer "is the verdict I just read
// still standing", and the receiver probe is the one part of the read that
// costs table probes rather than seeks.
//
// The skip has exactly one precondition: the verdict under re-check must not be
// the RECEIVER index's own. Without the probe the receiver rules cannot fire at
// all, so a receiver verdict would re-read as a clean store, come back "raced",
// and never be repaired — at every boundary, forever. This is the case that
// pins the precondition.
//
// MUTANT: skip the receiver probe unconditionally on the re-probe. The reason
// becomes "raced" and nothing is rebuilt.
func TestEnsurePlannerStatsFresh_RepairsAReceiverOnlyVerdict(t *testing.T) {
	s := freshStatsStore(t, "stats_receiver_verdict.sqlite", 100)
	if first := mustEnsure(t, s); first.Refreshed || first.Stale {
		t.Fatalf("fixture was not fresh: refreshed=%v reason=%q", first.Refreshed, first.Reason)
	}
	// Only the receiver row goes: nodes and edges stay honest, so this verdict
	// is reachable through the receiver rule alone.
	if _, err := s.writerDB.Exec(`DELETE FROM sqlite_stat1 WHERE idx = ?`, plannerStatsReceiverIndex); err != nil {
		t.Fatalf("delete the receiver stat row: %v", err)
	}

	verdict := mustHealth(t, s)
	if verdict.Reason != "missing:"+plannerStatsReceiverIndex {
		t.Fatalf("verdict = %q, want missing:%s — the receiver-only case is not under test",
			verdict.Reason, plannerStatsReceiverIndex)
	}

	repaired := mustEnsure(t, s)
	if !repaired.Refreshed {
		t.Fatalf("a receiver-only verdict was not repaired: reason=%q; the re-probe skipped the receiver "+
			"count, read a clean store, and handed the verdict back unrefreshed", repaired.Reason)
	}
	if _, ok := statRowFor(t, s, plannerStatsReceiverIndex); !ok {
		t.Errorf("the refresh left no %s stat row", plannerStatsReceiverIndex)
	}
}

// T12. A completed pass may move the growth anchor of the family it REBUILT and
// of no other.
//
// Ordinary indexing grows nodes and edges together. The rules read nodes first
// and stop at the first hit, so the nodes verdict always wins and its pass
// rebuilds only the indexes on nodes. Move the edges component of the anchor as
// well and it lands on exactly the total that was about to produce the edges
// verdict — so edges_by_kind, edges_by_from_line and edges_by_from_line_kind
// keep the figures the cold load gave them for the life of a proportionally
// growing store, and nothing at Open repairs a row that believes 100 over 400.
// Those are the rows the receiver/edge joins issue #651 is about are costed
// from.
//
// MUTANT: move both components on completion. The second call finds nothing
// stale, and the edge stat rows are never rewritten.
func TestEnsurePlannerStatsFresh_RefreshesEachFamilyInTurn(t *testing.T) {
	s := freshStatsStore(t, "stats_family_baseline.sqlite", 100)
	if first := mustEnsure(t, s); first.Refreshed || first.Stale {
		t.Fatalf("fixture was not fresh: refreshed=%v reason=%q", first.Refreshed, first.Reason)
	}

	nodeWork := growthWorkList(t, s, "growth:"+plannerStatsNodesIndex)
	edgeWork := growthWorkList(t, s, "growth:"+plannerStatsEdgesIndex)
	before := statRowsFor(t, s, append(append([]string{}, nodeWork...), edgeWork...))

	// Both families double, which is what indexing a second repository does.
	growStoreToDouble(t, s)

	nodesPass := mustEnsure(t, s)
	if !nodesPass.Refreshed {
		t.Fatalf("a doubled store did not refresh: stale=%v reason=%q", nodesPass.Stale, nodesPass.Reason)
	}
	if !strings.HasPrefix(nodesPass.Reason, "growth:nodes") {
		t.Fatalf("first verdict = %q, want the nodes family: the rules read nodes before edges",
			nodesPass.Reason)
	}
	if got, _ := statRowFor(t, s, plannerStatsNodesIndex); got == before[plannerStatsNodesIndex] {
		t.Errorf("the nodes pass left %s at %q; it analyzed nothing",
			plannerStatsNodesIndex, before[plannerStatsNodesIndex])
	}
	for _, idx := range edgeWork {
		if got, _ := statRowFor(t, s, idx); got != before[idx] {
			t.Errorf("a nodes verdict rewrote %s: %q -> %q", idx, before[idx], got)
		}
	}

	edgesPass := mustEnsure(t, s)
	if !edgesPass.Refreshed {
		t.Fatalf("the boundary after the nodes pass did not refresh the edges (stale=%v reason=%q); the "+
			"completed nodes pass moved the EDGES anchor to the total that was about to fire the edges "+
			"verdict, so the edge statistics stay frozen at their cold-load figures forever",
			edgesPass.Stale, edgesPass.Reason)
	}
	if !strings.HasPrefix(edgesPass.Reason, "growth:edges") {
		t.Fatalf("second verdict = %q, want the edges family", edgesPass.Reason)
	}
	for _, idx := range edgeWork {
		if got, _ := statRowFor(t, s, idx); got == before[idx] {
			t.Errorf("the edges pass never rewrote %s: row still %q", idx, before[idx])
		}
	}
	edgeStat, ok := statRowFor(t, s, plannerStatsEdgesIndex)
	if !ok {
		t.Fatalf("the edges pass left no %s stat row", plannerStatsEdgesIndex)
	}
	if got := statRowCount(t, edgeStat); got < 150 {
		t.Errorf("%s believes %d after the refresh, want the grown corpus (>=150)",
			plannerStatsEdgesIndex, got)
	}

	quiet := mustEnsure(t, s)
	if quiet.Refreshed || quiet.Stale {
		t.Errorf("the third boundary was not quiescent: refreshed=%v stale=%v reason=%q; both anchors "+
			"have been moved by their own family's pass and nothing is owed",
			quiet.Refreshed, quiet.Stale, quiet.Reason)
	}
}

// T13. A resume position means something only while the verdict it belongs to
// is still standing.
//
// A verdict can clear with no pass of OURS completing it: another caller
// refreshed under the write gate, or the counters fell and the baseline decayed
// with them. A position kept through that would be handed, arbitrarily later,
// to the first pass whose key happened to match — resuming past indexes
// analyzed at a size the cursor's "trails by at most one doubling" bound no
// longer covers, and skipping the very index the verdict was read off.
//
// MUTANT: drop resetPlannerStatsCursor from the not-stale probe. The re-fired
// verdict resumes after the index the deferred pass finished, so the pass takes
// the gate len(work)-1 times instead of len(work).
func TestEnsurePlannerStatsFresh_CursorClearsWhenTheVerdictClears(t *testing.T) {
	s := freshStatsStore(t, "stats_cursor_clear.sqlite", 100)
	if first := mustEnsure(t, s); first.Refreshed || first.Stale {
		t.Fatalf("fixture was not fresh: refreshed=%v reason=%q", first.Refreshed, first.Reason)
	}
	growStoreToDouble(t, s)

	work := growthWorkList(t, s, "growth:"+plannerStatsNodesIndex)
	if len(work) < 2 {
		t.Fatalf("the nodes family holds %d critical index(es); resumption is untestable", len(work))
	}
	if work[0] != plannerStatsNodesIndex {
		t.Fatalf("work list = %v, want the verdict's own index first", work)
	}

	// Phase A: a pass that stops after exactly one index, leaving a position.
	restore := plannerStatsPassBudget
	plannerStatsPassBudget = 0
	t.Cleanup(func() { plannerStatsPassBudget = restore })
	partial := mustEnsure(t, s)
	plannerStatsPassBudget = restore
	if partial.Refreshed {
		t.Fatal("a pass that spent its budget after one index reported a completed refresh")
	}
	if want := plannerStatsBudgetReason + work[1]; partial.Reason != want {
		t.Fatalf("reason = %q, want %q", partial.Reason, want)
	}
	s.plannerStatsMu.Lock()
	deferredDone := len(s.plannerStatsCursor.done)
	s.plannerStatsMu.Unlock()
	if deferredDone == 0 {
		t.Fatal("the deferred pass left no resume position; there is nothing for the clear to retire")
	}

	// Phase B: the verdict clears without any pass of ours completing it. An
	// out-of-band refresh under the write gate is the "somebody else did it"
	// half; counters back below the growth factor are the decay half.
	refreshStatsNow(t, s)
	writeIndexStateCounters(t, s, 0, "repo", 300, 150)
	if cleared := mustEnsure(t, s); cleared.Stale || cleared.Refreshed {
		t.Fatalf("the store still owed a refresh after the verdict was cleared out of band: "+
			"stale=%v refreshed=%v reason=%q", cleared.Stale, cleared.Refreshed, cleared.Reason)
	}

	// Phase C: the SAME verdict key fires again over a store that grew past the
	// anchor once more. It must start at the index it names.
	writeIndexStateCounters(t, s, 0, "repo", 400, 150)
	holds := s.plannerStatsGateHolds.Load()
	again := mustEnsure(t, s)
	if !again.Refreshed {
		t.Fatalf("the re-fired verdict did not refresh: stale=%v reason=%q", again.Stale, again.Reason)
	}
	if key := plannerStatsReasonKey(again.Reason); key != "growth:"+plannerStatsNodesIndex {
		t.Fatalf("re-fired verdict key = %q, want the one phase A deferred on", key)
	}
	if got, want := s.plannerStatsGateHolds.Load()-holds, int64(len(work)); got != want {
		t.Errorf("the re-fired verdict took the write gate %d time(s) for a %d-index work list (%v); the "+
			"cursor survived a verdict that cleared without a completed pass, so the pass resumed past "+
			"%s — the index the verdict was read off — instead of starting at it", got, want, work, work[0])
	}
}

// assertConvergesInThreeBoundaries pins the shape a store that outgrew BOTH
// relations must take: a nodes pass, then an edges pass that actually rewrites
// the edge statistics rows, then a quiet probe.
//
// It takes no probe of its own before the first Ensure — growthWorkList and
// statRowsFor read the catalog and sqlite_stat1 directly — so a caller can use
// it on a store whose growth anchor has never been seeded.
func assertConvergesInThreeBoundaries(t *testing.T, s *Store) {
	t.Helper()
	edgeWork := growthWorkList(t, s, "growth:"+plannerStatsEdgesIndex)
	beforeEdges := statRowsFor(t, s, edgeWork)

	nodesPass := mustEnsure(t, s)
	if !nodesPass.Refreshed || !strings.HasPrefix(nodesPass.Reason, "growth:nodes") {
		t.Fatalf("first boundary: refreshed=%v reason=%q, want a nodes growth pass — the rules read "+
			"nodes before edges and stop at the first hit", nodesPass.Refreshed, nodesPass.Reason)
	}

	edgesPass := mustEnsure(t, s)
	if !edgesPass.Refreshed || !strings.HasPrefix(edgesPass.Reason, "growth:edges") {
		t.Fatalf("second boundary: refreshed=%v stale=%v reason=%q, want an edges growth pass. The "+
			"completed nodes pass seeded the EDGES component of the anchor as well, at the very total "+
			"that was about to fire the edges verdict, so edges_by_kind keeps the figure it booted "+
			"with for the life of the daemon", edgesPass.Refreshed, edgesPass.Stale, edgesPass.Reason)
	}
	for _, idx := range edgeWork {
		if got, _ := statRowFor(t, s, idx); got == beforeEdges[idx] {
			t.Errorf("the edges pass never rewrote %s: row still %q", idx, beforeEdges[idx])
		}
	}

	quiet := mustEnsure(t, s)
	if quiet.Refreshed || quiet.Stale {
		t.Errorf("the third boundary was not quiescent: refreshed=%v stale=%v reason=%q; both anchors "+
			"have been moved by their own family's pass and nothing is owed",
			quiet.Refreshed, quiet.Stale, quiet.Reason)
	}
}

// T14. A store whose FIRST EnsurePlannerStatsFresh is already stale never
// reaches the not-stale probe that seeds the growth anchor — so the first thing
// that seeds it is a COMPLETED PASS, and a pass may seed only what it rebuilt.
//
// That store is not hypothetical; it is the one this PR was written for. The
// daemon opens an existing on-disk database whose nodes_by_kind and
// edges_by_kind rows believe 592k / 2.77M over a corpus of 1.69M / 8.58M. Both
// rows are non-partial and far above plannerStatsSuspectRows, so the Open-time
// repair does not touch them and the very first verdict is a nodes growth one.
// Seed BOTH components on that pass and the edges anchor lands at 8.58M without
// a single ANALYZE of edges_by_kind: the edges verdict then needs the store to
// reach 17.16M before it fires, and the 2.77M row — the one the receiver/edge
// joins issue #651 is about are costed from — survives forever.
//
// A component left at zero is the correct alternative because zero is not a
// claim: plannerStatsStaleReason falls back to the BELIEVED row for it, which
// is the pre-seed rule that fired correctly on this very store, and it
// self-terminates as soon as that family's own pass completes.
//
// MUTANT: seed both components in notePlannerStatsRefresh's unseeded branch.
// The second call finds nothing stale and the edge stat rows are never
// rewritten.
func TestEnsurePlannerStatsFresh_ConvergesFromAnUnseededStaleStore(t *testing.T) {
	// The store grows with NO probe in between, which is what leaves the
	// anchor unseeded: freshStatsStore analyzes through refreshPlannerStatsLocked
	// rather than through the freshness path, and growStoreToDouble only writes
	// rows and counters. Neither calls mustEnsure or mustHealth.
	t.Run("never probed before the growth", func(t *testing.T) {
		s := freshStatsStore(t, "stats_unseeded_stale.sqlite", 100)
		if base := baselineSnapshot(s); base.seeded {
			t.Fatalf("the fixture seeded the anchor before any probe (%+v); this test's premise is gone", base)
		}
		growStoreToDouble(t, s)
		assertConvergesInThreeBoundaries(t, s)
	})

	// The lead's own shape: the growth happened in a previous process, and the
	// handle that reads the verdict was opened over a database that is already
	// bigger than its statistics.
	t.Run("reopened over a store that already outgrew its statistics", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "stats_unseeded_reopen.sqlite")
		s := openStatsRepairStore(t, path)
		seedNamedGoReceiverFixture(s, "a", 100)
		writeIndexStateCounters(t, s, 0, "repo", 200, 100)
		refreshStatsNow(t, s)
		growStoreToDouble(t, s)
		if err := closeStatsRepairStore(s); err != nil {
			t.Fatalf("close the store before reopening: %v", err)
		}

		reopened := openStatsRepairStore(t, path)
		if base := baselineSnapshot(reopened); base.seeded {
			t.Fatalf("the reopened handle carried a seeded anchor (%+v): the Open-time repair fired and "+
				"stamped it, so this case no longer reproduces a store whose first Ensure is stale", base)
		}
		if believed := plannerStatsBelievedRows(context.Background(), reopened.db, plannerStatsEdgesIndex); believed != 100 {
			t.Fatalf("%s believes %d after the reopen, want the pre-growth 100: the Open-time repair "+
				"rebuilt it and there is no stale edge row left to converge on",
				plannerStatsEdgesIndex, believed)
		}
		assertConvergesInThreeBoundaries(t, reopened)
	})
}

// T15. The growth anchor is earned by the index the growth rule READS, never by
// the table the analyzed index happens to sit on.
//
// Seven of the ten critical indexes are on nodes, but only nodes_by_kind (with
// nodes_by_file as its fallback) and edges_by_kind are ever read by
// plannerStatsStaleReason. A pass that rebuilt only nodes_go_receiver_type — the
// work list a receiver-only "missing:" verdict produces, because every other
// index still carries a row — has told the nodes rule nothing. Move the nodes
// anchor for it and the family may grow to FOUR times the size its statistics
// were actually computed at before the next verdict fires.
//
// MUTANT: attribute families by plannerStatsIndexProbes[name].table. The
// receiver-only pass moves the nodes anchor to the current total, and the
// growth verdict at twice the earned anchor never fires.
func TestEnsurePlannerStatsFresh_ReceiverOnlyPassLeavesTheAnchors(t *testing.T) {
	s := freshStatsStore(t, "stats_receiver_anchor.sqlite", 100)
	if first := mustEnsure(t, s); first.Refreshed || first.Stale {
		t.Fatalf("fixture was not fresh: refreshed=%v reason=%q", first.Refreshed, first.Reason)
	}
	anchor := baselineSnapshot(s)
	if !anchor.seeded || anchor.nodes != 200 || anchor.edges != 100 {
		t.Fatalf("baseline = %+v, want the fixture's 200 nodes / 100 edges anchored", anchor)
	}

	// Halfway to the factor: nothing is owed (300 is short of 2*200), but the
	// counters no longer agree with the anchor — so a pass that moves an anchor
	// it did not earn now moves it somewhere observable.
	writeIndexStateCounters(t, s, 0, "repo", 300, 100)

	// The one verdict whose work list touches neither growth rule's index.
	// Every other critical index keeps its row, so the backfill is scoped to
	// nodes_go_receiver_type alone.
	if _, err := s.writerDB.Exec(`DELETE FROM sqlite_stat1 WHERE idx = ?`, plannerStatsReceiverIndex); err != nil {
		t.Fatalf("delete the receiver stat row: %v", err)
	}
	work := growthWorkList(t, s, "missing:"+plannerStatsReceiverIndex)
	if len(work) != 1 || work[0] != plannerStatsReceiverIndex {
		t.Fatalf("work list = %v, want exactly [%s]; this test's premise is gone",
			work, plannerStatsReceiverIndex)
	}

	pass := mustEnsure(t, s)
	if !pass.Refreshed {
		t.Fatalf("the receiver-only verdict was not repaired: stale=%v reason=%q", pass.Stale, pass.Reason)
	}
	if pass.Reason != "missing:"+plannerStatsReceiverIndex {
		t.Fatalf("verdict = %q, want missing:%s", pass.Reason, plannerStatsReceiverIndex)
	}
	if got := baselineSnapshot(s); got != anchor {
		t.Fatalf("baseline = %+v after a pass that rebuilt only %s, want it left at %+v: %s is not the "+
			"row either growth rule reads, so no anchor was earned",
			got, plannerStatsReceiverIndex, anchor, plannerStatsReceiverIndex)
	}

	// Twice the anchor the fixture earned. Attributed by table, the anchor
	// would now sit at 300 and the nodes family would reach 600 — four times
	// the corpus its statistics describe — before anything noticed.
	writeIndexStateCounters(t, s, 0, "repo", 400, 100)
	grown := mustEnsure(t, s)
	if !grown.Refreshed || !strings.HasPrefix(grown.Reason, "growth:nodes") {
		t.Fatalf("at twice the earned anchor: refreshed=%v stale=%v reason=%q, want a nodes growth "+
			"verdict; the receiver-only pass moved the nodes anchor to the total it was run at",
			grown.Refreshed, grown.Stale, grown.Reason)
	}
}

// cursorDone lists, sorted, the indexes the standing resume cursor has already
// finished. A test asserting "the pass stopped after exactly the verdict's own
// index" needs the position itself, not just the reason text.
func cursorDone(s *Store) []string {
	s.plannerStatsMu.Lock()
	defer s.plannerStatsMu.Unlock()
	names := make([]string, 0, len(s.plannerStatsCursor.done))
	for name := range s.plannerStatsCursor.done {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// T16. An UNANCHORED family must converge past the index its own verdict was
// read off, and without the held base it cannot.
//
// A family whose growth anchor component is still zero is judged against its
// BELIEVED row — the pre-seed rule — and the work list rebuilds the index that
// row is read off FIRST, because that is the index whose rebuild can clear the
// verdict at all. Put those two together and the first deferral of an
// unanchored pass repairs its own verdict: the next boundary reads a believed
// row that now agrees with the store, finds nothing stale, retires the cursor
// through the not-stale probe and seeds the component from the current totals.
// The other six nodes indexes — nodes_go_receiver_type among them, whose capped
// probe cannot fire a verdict of its own once it believes plannerStatsHealthProbeCap
// or more — then keep their pre-growth rows until the store doubles AGAIN.
//
// That is not an exotic state. It is the boot regime of every existing store
// (the first Ensure is already stale, so no not-stale probe has ever seeded the
// anchor), and it is the regime of the EDGES family at the boundary right after
// a completed nodes pass, because notePlannerStatsRefresh deliberately anchors
// only the family it rebuilt. Both halves are pinned below, on one store, in the
// order a real daemon meets them.
//
// MUTANT: judge the unanchored family against the believed row while the cursor
// stands (drop the held base from plannerStatsStaleReason's input). The second
// boundary of each half goes quiet with exactly ONE index of the family
// rewritten, and the "every stat row describes the grown corpus" assertions fail
// on the remaining six / two.
func TestEnsurePlannerStatsFresh_UnanchoredFamilyConvergesAcrossADeferral(t *testing.T) {
	// No probe before the growth, so nothing seeds the anchor: freshStatsStore
	// analyzes through refreshPlannerStatsLocked and growStoreToDouble only
	// writes rows and counters. growthWorkList and statRowsFor read the catalog
	// and sqlite_stat1 directly and take no verdict either.
	s := freshStatsStore(t, "stats_unanchored_resume.sqlite", 100)
	if base := baselineSnapshot(s); base.seeded {
		t.Fatalf("the fixture seeded the anchor before any probe (%+v); this test's premise is gone", base)
	}
	growStoreToDouble(t, s)

	restore := plannerStatsPassBudget
	t.Cleanup(func() { plannerStatsPassBudget = restore })

	t.Run("nodes, with no anchor at all", func(t *testing.T) {
		work := growthWorkList(t, s, "growth:"+plannerStatsNodesIndex)
		if len(work) < 3 {
			t.Fatalf("the nodes family holds %d critical index(es) (%v); a deferral cannot leave a "+
				"remainder and this test proves nothing", len(work), work)
		}
		if work[0] != plannerStatsNodesIndex {
			t.Fatalf("work list = %v, want the verdict's own index first", work)
		}
		var receiverInFamily bool
		for _, idx := range work {
			receiverInFamily = receiverInFamily || idx == plannerStatsReceiverIndex
		}
		if !receiverInFamily {
			t.Fatalf("work list = %v, want %s in it: the #651 index is the one a self-repaired verdict "+
				"strands, and its capped probe cannot fire a verdict of its own", work, plannerStatsReceiverIndex)
		}
		before := statRowsFor(t, s, work)

		// Boundary 1: one index, then the budget stops the pass.
		plannerStatsPassBudget = 0
		partial := mustEnsure(t, s)
		plannerStatsPassBudget = restore
		if partial.Refreshed {
			t.Fatal("a pass that spent its budget after one index reported a completed refresh")
		}
		if want := plannerStatsBudgetReason + work[1]; partial.Reason != want {
			t.Fatalf("reason = %q, want %q", partial.Reason, want)
		}
		if got := cursorDone(s); len(got) != 1 || got[0] != work[0] {
			t.Fatalf("cursor finished %v, want exactly [%s]", got, work[0])
		}
		if base := baselineSnapshot(s); base.nodes != 0 {
			t.Fatalf("baseline = %+v after a DEFERRED pass; only a completed pass earns an anchor", base)
		}

		// Boundary 2: the verdict must still be standing, judged against the
		// base it fired on rather than against the row boundary 1 rewrote, and
		// the pass must resume rather than start over.
		holds := s.plannerStatsGateHolds.Load()
		done := mustEnsure(t, s)
		if !done.Refreshed {
			t.Fatalf("the boundary after the deferral was quiet (stale=%v reason=%q): the unanchored "+
				"verdict was judged against the believed row the deferred pass had just rewritten, so "+
				"it cleared itself and %d of %d indexes keep their pre-growth statistics until the "+
				"store doubles again", done.Stale, done.Reason, len(work)-1, len(work))
		}
		if got, want := s.plannerStatsGateHolds.Load()-holds, int64(len(work)-1); got != want {
			t.Errorf("the resumed pass took the write gate %d time(s) for %d remaining index(es) (%v)",
				got, want, work)
		}
		for _, idx := range work {
			got, ok := statRowFor(t, s, idx)
			if !ok {
				t.Errorf("%s has no stat row after the family converged", idx)
				continue
			}
			if got == before[idx] {
				t.Errorf("%s still believes %q after the family converged; it describes the corpus the "+
					"store held before it doubled", idx, before[idx])
				continue
			}
			// Grew, not merely changed. The seven nodes indexes do not all end
			// at the same figure — the partial ones (nodes_go_receiver_type
			// among them) describe their own predicate's subset — so the
			// property that holds for every one of them is that each now
			// believes strictly more than it did over the half-sized corpus.
			if now, was := statRowCount(t, got), statRowCount(t, before[idx]); now <= was {
				t.Errorf("%s believes %d after the family converged, was %d over the half-sized corpus",
					idx, now, was)
			}
		}
		if stat, ok := statRowFor(t, s, plannerStatsNodesIndex); !ok || statRowCount(t, stat) < 300 {
			t.Errorf("%s believes %q, want the whole grown nodes table (>=300)", plannerStatsNodesIndex, stat)
		}
		if base := baselineSnapshot(s); !base.seeded || base.nodes != 400 {
			t.Errorf("baseline = %+v after the completed pass, want the 400 nodes it was run at", base)
		}
		if got := cursorDone(s); len(got) != 0 {
			t.Errorf("cursor = %v after a completed pass, want it retired", got)
		}
	})

	// The edges family reaches this boundary in exactly the unanchored state the
	// half above started in — notePlannerStatsRefresh anchored only the nodes
	// component, on purpose — so the same deferral must not strand it either.
	t.Run("edges, unanchored by the completed nodes pass", func(t *testing.T) {
		if base := baselineSnapshot(s); base.edges != 0 {
			t.Fatalf("baseline = %+v: the nodes pass anchored the edges component too, and the "+
				"unanchored edges regime this half is about no longer exists", base)
		}
		work := growthWorkList(t, s, "growth:"+plannerStatsEdgesIndex)
		if len(work) < 2 {
			t.Fatalf("the edges family holds %d critical index(es) (%v); a deferral cannot leave a "+
				"remainder", len(work), work)
		}
		if work[0] != plannerStatsEdgesIndex {
			t.Fatalf("work list = %v, want the verdict's own index first", work)
		}
		before := statRowsFor(t, s, work)

		plannerStatsPassBudget = 0
		partial := mustEnsure(t, s)
		plannerStatsPassBudget = restore
		if partial.Refreshed {
			t.Fatal("a pass that spent its budget after one index reported a completed refresh")
		}
		if want := plannerStatsBudgetReason + work[1]; partial.Reason != want {
			t.Fatalf("reason = %q, want %q", partial.Reason, want)
		}
		if got := cursorDone(s); len(got) != 1 || got[0] != work[0] {
			t.Fatalf("cursor finished %v, want exactly [%s]", got, work[0])
		}
		if base := baselineSnapshot(s); base.edges != 0 {
			t.Fatalf("baseline = %+v after a DEFERRED edges pass; only a completed pass earns an anchor", base)
		}

		holds := s.plannerStatsGateHolds.Load()
		done := mustEnsure(t, s)
		if !done.Refreshed {
			t.Fatalf("the boundary after the deferral was quiet (stale=%v reason=%q): the unanchored "+
				"edges verdict cleared itself against the row the deferred pass had just rewritten, and "+
				"edges_by_from_line / edges_by_from_line_kind keep their cold-load figures", done.Stale, done.Reason)
		}
		if got, want := s.plannerStatsGateHolds.Load()-holds, int64(len(work)-1); got != want {
			t.Errorf("the resumed pass took the write gate %d time(s) for %d remaining index(es) (%v)",
				got, want, work)
		}
		for _, idx := range work {
			got, ok := statRowFor(t, s, idx)
			if !ok {
				t.Errorf("%s has no stat row after the family converged", idx)
				continue
			}
			if got == before[idx] {
				t.Errorf("%s still believes %q after the family converged", idx, before[idx])
			}
		}
		if base := baselineSnapshot(s); base.edges != 200 {
			t.Errorf("baseline = %+v after the completed edges pass, want the 200 edges it was run at", base)
		}

		quiet := mustEnsure(t, s)
		if quiet.Refreshed || quiet.Stale {
			t.Errorf("the boundary after both families converged was not quiescent: refreshed=%v "+
				"stale=%v reason=%q", quiet.Refreshed, quiet.Stale, quiet.Reason)
		}
	})
}

// T17. A verdict that genuinely clears while a held base stands must have its
// anchor filled on the SAME boundary that retires the cursor.
//
// The two operations the not-stale arm performs interact, and only one order is
// right. seedPlannerStatsBaseline deliberately refuses to fill a component whose
// family still has a held base standing on the cursor — filling it would clear a
// verdict the standing pass is still working through, which is
// TestSeedPlannerStatsBaseline_FillTransition's third case. But this arm is
// reached precisely because the verdict is GONE: the counters fell under a
// shrinking store, or another caller finished the family. The cursor is about to
// be retired one statement later, so the base it holds is owed to nobody, and a
// seed that runs first reads a base that is already meaningless, declines the
// fill, and leaves the family measuring growth against its believed row for one
// more boundary — for nothing.
//
// The shrink here is the decay half: counters back below the growth factor with
// the corpus untouched, which is what a retired view generation does.
//
// MUTANT: seed before retiring the cursor. The nodes component comes back 0 —
// the edges one, which never had a held base, is filled either way, so the
// mutant is not the whole seed going missing but exactly the held family's.
func TestEnsurePlannerStatsFresh_ClearedVerdictFillsTheHeldComponent(t *testing.T) {
	s := freshStatsStore(t, "stats_cleared_fills_held.sqlite", 100)
	if base := baselineSnapshot(s); base.seeded {
		t.Fatalf("the fixture seeded the anchor before any probe (%+v); the unanchored regime this "+
			"test is about is gone", base)
	}
	initial := mustHealth(t, s)
	if initial.Stale || initial.Nodes.Believed == 0 {
		t.Fatalf("fixture was not a fresh unanchored store: stale=%v believed=%d reason=%q",
			initial.Stale, initial.Nodes.Believed, initial.Reason)
	}

	// Counters alone, so the corpus — and therefore every believed row — is
	// exactly what the verdict fired on. The point of this test is the fill,
	// not the staleness, and a growth that moved the rows would confuse the two.
	writeIndexStateCounters(t, s, 0, "repo", 400, 200)
	work := growthWorkList(t, s, "growth:"+plannerStatsNodesIndex)
	if len(work) < 2 {
		t.Fatalf("the nodes family holds %d critical index(es); a deferral cannot leave a remainder", len(work))
	}

	restore := plannerStatsPassBudget
	plannerStatsPassBudget = 0
	t.Cleanup(func() { plannerStatsPassBudget = restore })
	partial := mustEnsure(t, s)
	plannerStatsPassBudget = restore
	if partial.Refreshed {
		t.Fatal("a pass that spent its budget after one index reported a completed refresh")
	}
	if want := plannerStatsBudgetReason + work[1]; partial.Reason != want {
		t.Fatalf("reason = %q, want %q", partial.Reason, want)
	}
	if got := s.plannerStatsHeldBase(); got.nodes != initial.Nodes.Believed {
		t.Fatalf("the deferred unanchored pass held %+v, want the believed row the verdict fired on "+
			"(%d); there is no held base for the not-stale probe to trip over and this test proves "+
			"nothing", got, initial.Nodes.Believed)
	}
	if base := baselineSnapshot(s); base.nodes != 0 {
		t.Fatalf("baseline = %+v after a DEFERRED pass; only a completed pass earns an anchor", base)
	}

	// The store shrinks back under the factor: the verdict clears with no pass
	// of ours completing it, which is the arm under test.
	writeIndexStateCounters(t, s, 0, "repo", 300, 150)
	cleared := mustEnsure(t, s)
	if cleared.Stale || cleared.Refreshed {
		t.Fatalf("the shrunk store still owed a refresh: stale=%v refreshed=%v reason=%q",
			cleared.Stale, cleared.Refreshed, cleared.Reason)
	}
	if got := cursorDone(s); len(got) != 0 {
		t.Errorf("cursor = %v after the verdict cleared, want it retired", got)
	}
	if got := s.plannerStatsHeldBase(); got != (plannerStatsHeldBase{}) {
		t.Errorf("held base = %+v after the verdict cleared, want it retired with the cursor", got)
	}
	want := plannerStatsBaseline{nodes: 300, edges: 150, seeded: true}
	if got := baselineSnapshot(s); got != want {
		t.Errorf("baseline = %+v after the boundary that cleared the verdict, want %+v: the probe "+
			"retired the held base and then declined to fill the component it had just freed, so the "+
			"nodes family keeps measuring growth from its believed row for another boundary", got, want)
	}
}

// A probe that FAILED measured nothing, so it may anchor nothing.
//
// plannerStatsHealth fills the counter figures before the reads that can still
// fail underneath it — the capped receiver count is the last of them — so a
// failed probe hands back a struct whose Nodes.Known is already true and whose
// Actual is already the counter total. Seeding from that freezes the growth
// anchor at a size no probe ever fully established, and every later verdict is
// then measured from it.
//
// The failure is the real one the code documents: SQLite refuses INDEXED BY
// when a partial index cannot serve the stated WHERE clause, which is how a
// drifted predicate announces itself.
//
// MUTANT: seed on the error path as well ('err != nil || !health.Stale'). The
// anchor comes back seeded at the counter totals.
func TestEnsurePlannerStatsFresh_ProbeFailureLeavesTheAnchorUnseeded(t *testing.T) {
	s := freshStatsStore(t, "stats_probe_error.sqlite", 40)
	if base := baselineSnapshot(s); base.seeded {
		t.Fatalf("the fixture seeded the anchor before any probe (%+v); this test's premise is gone", base)
	}

	// Same index name and columns, a predicate the probe's WHERE clause no
	// longer implies.
	if _, err := s.writerDB.Exec(`DROP INDEX ` + quoteSQLiteIdentifier(plannerStatsReceiverIndex)); err != nil {
		t.Fatalf("drop %s: %v", plannerStatsReceiverIndex, err)
	}
	if _, err := s.writerDB.Exec(`CREATE INDEX ` + quoteSQLiteIdentifier(plannerStatsReceiverIndex) +
		` ON nodes(repo_prefix, file_dir, name, id) WHERE ` + nodesGoReceiverTypePredicate +
		` AND repo_prefix = 'no-such-repo'`); err != nil {
		t.Fatalf("recreate %s with a drifted predicate: %v", plannerStatsReceiverIndex, err)
	}

	health, err := s.EnsurePlannerStatsFresh(context.Background())
	if err == nil {
		t.Fatalf("the drifted predicate did not fail the probe (reason=%q); this test's premise is gone",
			health.Reason)
	}
	if !health.Nodes.Known || health.Nodes.Actual == 0 {
		t.Fatalf("the failed probe carried nodes known=%v actual=%d; it stopped before the counter read, "+
			"so the seeding branch was never reachable and this test proves nothing",
			health.Nodes.Known, health.Nodes.Actual)
	}
	if base := baselineSnapshot(s); base.seeded {
		t.Errorf("a failed probe seeded the growth anchor to %+v; every later verdict would be measured "+
			"from a size no probe ever established", base)
	}
}

// The growth anchor is moved only for a family plannerStatsWorkFamilies
// attributes, so that mapping has to cover every index a verdict can be READ
// off — and nothing else.
//
// plannerStatsStaleReason is handed exactly four index names: the nodes index
// (nodes_by_kind, or nodes_by_file when it is the row that carries a
// cardinality), the edges index, and the receiver index. Three of them are rows
// a GROWTH rule reads and therefore rows an ANALYZE can bring back into
// agreement with the store; the receiver index is named only by the believed==0
// rule and earns no anchor, which is T15's property. Every other critical index
// is work a pass may do and never evidence a verdict was answered.
//
// MUTANT: attribute by plannerStatsIndexProbes[name].table. Six extra nodes
// indexes and two extra edge indexes start earning anchors they never proved.
func TestPlannerStatsWorkFamiliesCoversEveryVerdictIndex(t *testing.T) {
	// Every index plannerStatsStaleReason can name, with the family its own
	// rebuild entitles a pass to move.
	nameable := map[string]plannerStatsFamilies{
		plannerStatsNodesIndex:         {nodes: true},
		plannerStatsNodesFallbackIndex: {nodes: true},
		plannerStatsEdgesIndex:         {edges: true},
		plannerStatsReceiverIndex:      {},
	}
	for index, want := range nameable {
		if got := plannerStatsWorkFamilies([]string{index}); got != want {
			t.Errorf("plannerStatsWorkFamilies([%s]) = %+v, want %+v", index, got, want)
		}
	}
	for _, index := range []string{plannerStatsNodesIndex, plannerStatsNodesFallbackIndex, plannerStatsEdgesIndex} {
		if plannerStatsWorkFamilies([]string{index}) == (plannerStatsFamilies{}) {
			t.Errorf("%s is a row a growth rule reads but earns no family: a completed pass over it "+
				"moves no anchor, so the verdict re-fires at every boundary forever", index)
		}
	}
	// And no OTHER critical index earns one. plannerStatsIndexProbes is the
	// same ten-index set as plannerStatsIndexQuery, so a critical index added
	// to one and quietly attributed here fails this.
	for index := range plannerStatsIndexProbes {
		if _, nameableIndex := nameable[index]; nameableIndex {
			continue
		}
		if got := plannerStatsWorkFamilies([]string{index}); got != (plannerStatsFamilies{}) {
			t.Errorf("plannerStatsWorkFamilies([%s]) = %+v, want none: no growth rule reads %s, so a "+
				"pass that rebuilt it has proved nothing about either family", index, got, index)
		}
	}
}

// seedPlannerStatsBaseline's fill transition, pinned on its own: a zero
// component is an unanchored family and is filled by the first probe that finds
// the store fresh; a non-zero one was earned by that family's own completed
// ANALYZE and is never raised here; and a zero component whose family still has
// an unfinished pass standing on the cursor is NOT fillable, because the held
// base on that cursor is the only record of what its verdict fired on and
// anchoring the component at the current totals clears the verdict just as
// surely as overwriting the base would.
//
// MUTANT: assign both components unconditionally. The second and third cases
// both fail.
func TestSeedPlannerStatsBaseline_FillTransition(t *testing.T) {
	measured := graph.PlannerStatsFreshness{
		Nodes: graph.PlannerStatsCardinality{Known: true, Actual: 500},
		Edges: graph.PlannerStatsCardinality{Known: true, Actual: 250},
	}
	setState := func(s *Store, base plannerStatsBaseline, cursor plannerStatsCursor) {
		s.plannerStatsMu.Lock()
		defer s.plannerStatsMu.Unlock()
		s.plannerStatsBaseline = base
		s.plannerStatsCursor = cursor
	}

	t.Run("an unanchored component is filled", func(t *testing.T) {
		s := freshStatsStore(t, "stats_seed_fill.sqlite", 10)
		setState(s, plannerStatsBaseline{}, plannerStatsCursor{})
		s.seedPlannerStatsBaseline(measured)
		want := plannerStatsBaseline{nodes: 500, edges: 250, seeded: true}
		if got := baselineSnapshot(s); got != want {
			t.Errorf("baseline = %+v, want %+v", got, want)
		}
	})

	t.Run("an anchored component is never raised", func(t *testing.T) {
		s := freshStatsStore(t, "stats_seed_noraise.sqlite", 10)
		setState(s, plannerStatsBaseline{nodes: 200, seeded: true}, plannerStatsCursor{})
		s.seedPlannerStatsBaseline(measured)
		want := plannerStatsBaseline{nodes: 200, edges: 250, seeded: true}
		if got := baselineSnapshot(s); got != want {
			t.Errorf("baseline = %+v, want %+v: only that family's own completed ANALYZE may raise its "+
				"anchor, and raising it here suppresses the very verdict the growth it measures owes", got, want)
		}
	})

	t.Run("a component whose family still owes indexes is left alone", func(t *testing.T) {
		s := freshStatsStore(t, "stats_seed_held.sqlite", 10)
		setState(s, plannerStatsBaseline{}, plannerStatsCursor{
			key:  "growth:" + plannerStatsNodesIndex + "|" + plannerStatsNodesIndex,
			done: map[string]bool{plannerStatsNodesIndex: true},
			held: plannerStatsHeldBase{nodes: 200},
		})
		s.seedPlannerStatsBaseline(measured)
		want := plannerStatsBaseline{nodes: 0, edges: 250, seeded: true}
		if got := baselineSnapshot(s); got != want {
			t.Errorf("baseline = %+v, want %+v: anchoring the nodes component at the current totals "+
				"clears the verdict the standing pass is still working through, and its remaining "+
				"indexes keep their pre-growth rows until the store doubles again", got, want)
		}
	})
}
