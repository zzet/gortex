package store_sqlite

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/graph"
)

// shortenDenseIndexRepairBackoff makes a skipped repair retry within
// milliseconds for the rest of the test.
func shortenDenseIndexRepairBackoff(t *testing.T) {
	t.Helper()
	base, maxDelay := denseIndexRepairBaseDelay, denseIndexRepairMaxDelay
	denseIndexRepairBaseDelay, denseIndexRepairMaxDelay = 5*time.Millisecond, 20*time.Millisecond
	t.Cleanup(func() { denseIndexRepairBaseDelay, denseIndexRepairMaxDelay = base, maxDelay })
}

// TestFailedSealReportsDenseIndexesMissing: a cold window that closes without
// rebuilding its dense indexes must still restore durability and close, must
// say which indexes it left absent instead of letting the planner probe read
// their absence as a clean store, must not retry the rebuild while the volume
// is full, and must rebuild them in one maintenance attempt once it is not,
// followed by the statistics pass the rebuilt indexes need.
func TestFailedSealReportsDenseIndexesMissing(t *testing.T) {
	shortenDenseIndexRepairBackoff(t)
	allDroppable := make([]string, 0, len(bulkDroppableIndexes))
	for _, idx := range bulkDroppableIndexes {
		allDroppable = append(allDroppable, idx.name)
	}
	for _, tc := range []struct {
		name string
		// fail closes the window the case's way and returns the indexes it
		// expects to be left missing, in declaration order.
		fail func(t *testing.T, s *Store) []string
	}{
		{"flush seal error", func(t *testing.T, s *Store) []string {
			s.BeginBulkLoad()
			if s.bulkConn == nil {
				t.Fatal("bulk fast path did not engage")
			}
			s.AddNode(&graph.Node{ID: "repo/a.go::A", Kind: graph.KindFunction, Name: "A"})
			injected := errors.New("injected seal failure")
			defer s.SetBulkSealHookForTest(func(index string) error {
				if index == "nodes_by_kind" || index == "edges_by_kind" {
					return injected
				}
				return nil
			})()
			err := s.FlushBulk()
			if !errors.Is(err, injected) {
				t.Fatalf("FlushBulk error = %v, want the injected seal failure", err)
			}
			for _, name := range []string{"nodes_by_kind", "edges_by_kind"} {
				if !strings.Contains(err.Error(), name) {
					t.Fatalf("FlushBulk error %q does not name %s", err, name)
				}
			}
			return []string{"nodes_by_kind", "edges_by_kind"}
		}},
		{"abort before seal", func(t *testing.T, s *Store) []string {
			if !s.BeginCoordinatedBulkLoad() {
				t.Fatal("coordinated fast path did not engage")
			}
			s.AddNode(&graph.Node{ID: "repo/a.go::A", Kind: graph.KindFunction, Name: "A"})
			if err := s.AbortCoordinatedBulkLoad(); err != nil {
				t.Fatalf("AbortCoordinatedBulkLoad: %v", err)
			}
			return allDroppable
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := openTempStore(t)
			var full atomic.Bool
			full.Store(true)
			diskAvail := func(string) (uint64, error) {
				if full.Load() {
					return 0, nil
				}
				return 1 << 40, nil
			}
			s.denseRepairDiskAvail.Store(&diskAvail)
			syncBefore := pragmaIntDB(t, s.writerDB, "synchronous")
			want := tc.fail(t, s)

			if s.bulkConn != nil || s.coordinatedBulkLoad || s.bulkWindowOpen.Load() {
				t.Fatal("window left open after closing unsealed")
			}
			if got := pragmaIntDB(t, s.writerDB, "synchronous"); got != syncBefore {
				t.Fatalf("writer synchronous = %d, want restored %d", got, syncBefore)
			}
			health, err := s.PlannerStatsHealth(context.Background())
			if err != nil {
				t.Fatalf("PlannerStatsHealth: %v", err)
			}
			if health.Reason != plannerStatsDenseIndexesMissingReason || health.Stale {
				t.Fatalf("health reason=%q stale=%v, want %q and not stale", health.Reason, health.Stale, plannerStatsDenseIndexesMissingReason)
			}
			if !reflect.DeepEqual(health.DenseIndexesMissing, want) {
				t.Fatalf("DenseIndexesMissing = %v, want %v", health.DenseIndexesMissing, want)
			}
			present := indexNames(t, s.db)
			for _, name := range allDroppable {
				missing := false
				for _, w := range want {
					missing = missing || w == name
				}
				if present[name] == missing {
					t.Fatalf("index %s present=%v, want present=%v", name, present[name], !missing)
				}
			}

			// While the volume reads as full the lane keeps skipping on its
			// backoff and never reaches a CREATE INDEX.
			waitUntil(t, "a repair attempt skipped for free space", func() bool { return s.denseIndexRepairSkips.Load() >= 2 })
			if got := s.denseIndexRepairAttempts.Load(); got != 0 {
				t.Fatalf("repair attempts while full = %d, want 0", got)
			}
			if health, err := s.PlannerStatsHealth(context.Background()); err != nil || health.Reason != plannerStatsDenseIndexesMissingReason {
				t.Fatalf("health while full = %q, %v; want %q", health.Reason, err, plannerStatsDenseIndexesMissingReason)
			}

			// Once space is back, the next retry rebuilds every listed index.
			full.Store(false)
			waitUntil(t, "the dense index repair", func() bool { return s.denseIndexRepairs.Load() == 1 })
			if got := s.denseIndexRepairAttempts.Load(); got != 1 {
				t.Fatalf("repair attempts = %d, want one", got)
			}
			health, err = s.PlannerStatsHealth(context.Background())
			if err != nil {
				t.Fatalf("PlannerStatsHealth after repair: %v", err)
			}
			if health.Reason == plannerStatsDenseIndexesMissingReason || len(health.DenseIndexesMissing) != 0 {
				t.Fatalf("after repair reason=%q missing=%v, want both cleared", health.Reason, health.DenseIndexesMissing)
			}
			present = indexNames(t, s.db)
			for _, name := range allDroppable {
				if !present[name] {
					t.Fatalf("index %s missing after the repair", name)
				}
			}
			// A rebuilt index has no sqlite_stat1 row, and once the repair is
			// done nothing else asks for a statistics pass, so the repair must
			// ask for one itself. nodes_by_kind holds the case's node.
			waitUntil(t, "statistics for the rebuilt nodes_by_kind", func() bool {
				return s.plannerStatsIndexesWithStats(context.Background())["nodes_by_kind"]
			})
			integrityOK(t, s.db)
		})
	}
}

// TestDenseIndexRepairBuildsFromTheRecordedDDL: the repair runs the DDL the
// closing window recorded with each missing name, never a lookup in
// bulkDroppableIndexes made when the lane gets to it. In-package cases replace
// that list to force a seal failure and put it back while the lane may still be
// on its way to the repair; a lookup there reads the list unsynchronised with
// that restore, and finds no DDL for a name the record still holds.
func TestDenseIndexRepairBuildsFromTheRecordedDDL(t *testing.T) {
	shortenDenseIndexRepairBackoff(t)
	s, _ := openTempStore(t)
	var full atomic.Bool
	full.Store(true)
	diskAvail := func(string) (uint64, error) {
		if full.Load() {
			return 0, nil
		}
		return 1 << 40, nil
	}
	s.denseRepairDiskAvail.Store(&diskAvail)

	originalIndexes := bulkDroppableIndexes
	bulkDroppableIndexes = append(append([]bulkDroppableIndex(nil), originalIndexes...), bulkDroppableIndex{
		name: "forced_bad_index",
		ddl:  `CREATE INDEX forced_bad_index ON definitely_missing_table(id)`,
	})
	s.BeginBulkLoad()
	if s.bulkConn == nil {
		bulkDroppableIndexes = originalIndexes
		t.Fatal("bulk fast path did not engage")
	}
	s.AddNode(&graph.Node{ID: "repo/a.go::A", Kind: graph.KindFunction, Name: "A"})
	err := s.FlushBulk()
	bulkDroppableIndexes = originalIndexes
	if err == nil || !strings.Contains(err.Error(), "forced_bad_index") {
		t.Fatalf("FlushBulk error = %v, want the forced index's DDL failure", err)
	}

	// The list no longer names the index; the record does, with the DDL that
	// cannot succeed. Every attempt must run it and fail, not report a repair.
	full.Store(false)
	waitUntil(t, "a finished repair attempt", func() bool {
		return s.denseIndexRepairs.Load() > 0 || s.denseIndexRepairAttempts.Load() >= 2
	})
	if got := s.denseIndexRepairs.Load(); got != 0 {
		t.Fatalf("repairs = %d for an index whose DDL cannot run, want 0", got)
	}
	if got := s.denseIndexesMissingNames(); !reflect.DeepEqual(got, []string{"forced_bad_index"}) {
		t.Fatalf("missing record = %v, want [forced_bad_index]", got)
	}
}

// TestDenseIndexRepairWaitsForAPinnedWriter: a repair that comes due while a
// window pins the writer defers, as VACUUM does, instead of building under
// that window, and builds on a later retry once the window has closed. A
// generation window is the one that can open on the populated store a failed
// seal leaves behind.
func TestDenseIndexRepairWaitsForAPinnedWriter(t *testing.T) {
	shortenDenseIndexRepairBackoff(t)
	s, _ := openTempStore(t)
	var full atomic.Bool
	full.Store(true)
	diskAvail := func(string) (uint64, error) {
		if full.Load() {
			return 0, nil
		}
		return 1 << 40, nil
	}
	s.denseRepairDiskAvail.Store(&diskAvail)
	s.BeginBulkLoad()
	if s.bulkConn == nil {
		t.Fatal("bulk fast path did not engage")
	}
	s.AddNode(&graph.Node{ID: "repo/a.go::A", Kind: graph.KindFunction, Name: "A"})
	injected := errors.New("injected seal failure")
	restore := s.SetBulkSealHookForTest(func(index string) error {
		if index == "nodes_by_kind" {
			return injected
		}
		return nil
	})
	err := s.FlushBulk()
	restore()
	if !errors.Is(err, injected) {
		t.Fatalf("FlushBulk error = %v, want the injected seal failure", err)
	}
	engaged, err := s.BeginGenerationBulkLoad(3)
	if err != nil || !engaged {
		t.Fatalf("BeginGenerationBulkLoad = (%v, %v), want engaged", engaged, err)
	}

	full.Store(false)
	waitUntil(t, "two repair attempts under the pinned writer", func() bool { return s.denseIndexRepairAttempts.Load() >= 2 })
	if got := s.denseIndexRepairs.Load(); got != 0 {
		t.Fatalf("repairs under a pinned writer = %d, want 0", got)
	}
	if got := s.denseIndexesMissingNames(); !reflect.DeepEqual(got, []string{"nodes_by_kind"}) {
		t.Fatalf("missing record under a pinned writer = %v, want [nodes_by_kind]", got)
	}
	if indexNames(t, s.db)["nodes_by_kind"] {
		t.Fatal("nodes_by_kind was built under the pinned writer")
	}

	if err := s.EndGenerationBulkLoad(); err != nil {
		t.Fatalf("EndGenerationBulkLoad: %v", err)
	}
	waitUntil(t, "the repair after the window closed", func() bool { return s.denseIndexRepairs.Load() == 1 })
	if !indexNames(t, s.db)["nodes_by_kind"] {
		t.Fatal("nodes_by_kind missing after the repair")
	}
	if got := s.denseIndexesMissingNames(); len(got) != 0 {
		t.Fatalf("missing record after the repair = %v, want empty", got)
	}
}

func waitUntil(t *testing.T, what string, done func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !done() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}
