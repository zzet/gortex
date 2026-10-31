package store_sqlite

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// TestFailedSealReportsDenseIndexesMissing: a cold window that closes without
// rebuilding its dense indexes must still restore durability and close, and
// must then say which indexes it left absent instead of letting the planner
// probe read their absence as a clean store.
func TestFailedSealReportsDenseIndexesMissing(t *testing.T) {
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
			integrityOK(t, s.db)
		})
	}
}
