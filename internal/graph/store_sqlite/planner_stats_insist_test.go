package store_sqlite

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

// A store loaded without statistics owes its refresh. At the boundary that
// ends a whole index (graph.WithPlannerStatsLoadBoundary) the pass runs to the
// end although the build lane is held; at runtime the same verdict stays
// cooperative and defers to the edit cycle, as it always did. Afterwards the
// name lookups seek by name.
func TestAnOwedPlannerStatsRefreshRunsAtTheLoadBoundaryOnly(t *testing.T) {
	s := openPayloadStore(t)
	_, names, _ := keyListGraph(t, s, 400)
	_, _ = s.writerDB.Exec(`ANALYZE sqlite_schema`)
	_, err := s.writerDB.Exec(`DELETE FROM sqlite_stat1`)
	require.NoError(t, err)
	_, err = s.writerDB.Exec(`ANALYZE sqlite_schema`)
	require.NoError(t, err)
	recycleStatsReadPool(s.db, s.writerDB)
	lane := &fakeBuildLane{}
	lane.install(s)
	lane.held.Store(true) // an edit cycle, or the index holding the lane

	runtime, err := s.EnsurePlannerStatsFresh(context.Background())
	require.NoError(t, err)
	t.Logf("owed verdict at runtime: refreshed=%v reason=%q", runtime.Refreshed, runtime.Reason)
	require.False(t, runtime.Refreshed, "an owed refresh ran through an edit cycle at runtime")
	require.True(t, strings.HasPrefix(runtime.Reason, plannerStatsEditCycleReason), "reason=%q", runtime.Reason)

	h, err := s.EnsurePlannerStatsFresh(graph.WithPlannerStatsLoadBoundary(context.Background()))
	require.NoError(t, err)
	t.Logf("owed verdict at the load boundary: refreshed=%v reason=%q", h.Refreshed, h.Reason)
	require.True(t, h.Refreshed, "the owed refresh deferred at the load boundary: reason=%q", h.Reason)
	require.True(t, h.Reason == "no_stats" || strings.HasPrefix(h.Reason, "missing:"), "reason=%q", h.Reason)
	_, ok := statRowFor(t, s, "nodes_by_name")
	require.True(t, ok, "no statistics row for nodes_by_name after the refresh")
	require.Contains(t, planOfUnboundReads(t, s, names), "nodes_by_name (name=? AND view_gen=?)")
}

// planOfUnboundReads is the plan of the repository name lookup.
func planOfUnboundReads(t *testing.T, s *Store, names []string) string {
	t.Helper()
	return planOfUnbound(t, s, repoNamesSeekSQL)
}

// The starved state of a store fresh from a whole index: four statistics rows
// (the sentinels of the check's families), none for the indexes the name
// lookups choose between. The check reads it stale ("missing:" the first
// planner index that holds rows and has no row), the owed refresh at the
// boundary that ends the index runs although the lane is held, and afterwards
// every planner index that holds rows has a statistics row.
func TestTheStarvedStatisticsStateIsOwedAndRefreshedWhole(t *testing.T) {
	s := openPayloadStore(t)
	_, _, _ = keyListGraph(t, s, 400)
	applyStarvedStats(t, s)
	_, err := s.writerDB.Exec(`ANALYZE sqlite_schema`)
	require.NoError(t, err)
	recycleStatsReadPool(s.db, s.writerDB)

	h, err := s.PlannerStatsHealth(context.Background())
	require.NoError(t, err)
	t.Logf("check on the starved state: stale=%v reason=%q", h.Stale, h.Reason)
	require.True(t, h.Stale, "the starved state read fresh")
	require.True(t, strings.HasPrefix(h.Reason, "missing:"), "reason=%q", h.Reason)

	lane := &fakeBuildLane{}
	lane.install(s)
	lane.held.Store(true)
	// The boundary that ends the whole index (the index holds the lane).
	done, err := s.EnsurePlannerStatsFresh(graph.WithPlannerStatsLoadBoundary(context.Background()))
	require.NoError(t, err)
	t.Logf("refresh: refreshed=%v reason=%q", done.Refreshed, done.Reason)
	require.True(t, done.Refreshed, "reason=%q", done.Reason)

	hasStat := s.plannerStatsIndexesWithStats(context.Background())
	for _, idx := range explicitNodeEdgeIndexes(t, s) {
		spec, known := plannerStatsIndexProbes[idx.name]
		if !known {
			_, outside := plannerStatsIndexesOutside[idx.name]
			require.True(t, outside, "index %s is neither a planner index nor left out", idx.name)
			continue
		}
		var hasRows bool
		require.NoError(t, s.db.QueryRow(spec.existsQuery(idx.name)).Scan(&hasRows))
		if hasRows {
			require.True(t, hasStat[idx.name], "index %s holds rows and has no statistics row after the refresh", idx.name)
		}
	}
	after, err := s.PlannerStatsHealth(context.Background())
	require.NoError(t, err)
	require.False(t, after.Stale, "still stale after the refresh: %q", after.Reason)
}

type schemaIndex struct{ name, table string }

// explicitNodeEdgeIndexes lists the explicit indexes on nodes and edges in the
// store's schema (SQLite's own automatic indexes excluded).
func explicitNodeEdgeIndexes(t *testing.T, s *Store) []schemaIndex {
	t.Helper()
	rows, err := s.db.Query(`SELECT name, tbl_name FROM sqlite_schema WHERE type = 'index' AND tbl_name IN ('nodes', 'edges') AND sql IS NOT NULL ORDER BY name`)
	require.NoError(t, err)
	defer rows.Close()
	var out []schemaIndex
	for rows.Next() {
		var ix schemaIndex
		require.NoError(t, rows.Scan(&ix.name, &ix.table))
		out = append(out, ix)
	}
	require.NoError(t, rows.Err())
	return out
}

// The check's rule covers every index the planner can choose for a read that
// takes a list of keys: every non-partial index on nodes and edges, and every
// partial one such a read's predicate can imply, is a planner index
// (plannerStatsIndexProbes, on its own table); the other partial indexes are
// named with the predicate that keeps them out (plannerStatsIndexesOutside). This fails when the schema
// gains an index the check does not name. The lazy edges_by_file_generation is
// built first, so the schema is the one a daemon reaches.
func TestPlannerStatsCoversEveryIndexOnNodesAndEdges(t *testing.T) {
	s := openPayloadStore(t)
	_, err := s.writerDB.Exec(edgesByFileGenerationIndexDDL)
	require.NoError(t, err)
	indexes := explicitNodeEdgeIndexes(t, s)
	require.GreaterOrEqual(t, len(indexes), 20, "precondition: the schema's indexes")
	covered := 0
	for _, ix := range indexes {
		if reason, outside := plannerStatsIndexesOutside[ix.name]; outside {
			partial := strings.Contains(strings.Join(strings.Fields(indexSQL(t, s, ix.name)), " "), " WHERE ")
			require.Equal(t, partial, strings.HasPrefix(reason, "partial"), "index %s: the reason it is left out does not match its DDL", ix.name)
			continue
		}
		covered++
		spec, known := plannerStatsIndexProbes[ix.name]
		require.True(t, known, "index %s on %s is neither in plannerStatsIndexProbes nor left out with its reason: the statistics check would not see it", ix.name, ix.table)
		require.Equal(t, ix.table, spec.table, "index %s", ix.name)
		// The probe's predicate is the index's own (INDEXED BY refuses one
		// that does not imply it).
		var hasRows bool
		require.NoError(t, s.db.QueryRow(spec.existsQuery(ix.name)).Scan(&hasRows), "index %s", ix.name)
	}
	present, err := s.plannerStatsPresentIndexList(context.Background())
	require.NoError(t, err)
	require.Len(t, present, covered, "the present list is every covered index on nodes and edges")
}

func indexSQL(t *testing.T, s *Store, name string) string {
	t.Helper()
	var ddl string
	require.NoError(t, s.db.QueryRow(`SELECT sql FROM sqlite_schema WHERE type = 'index' AND name = ?`, name).Scan(&ddl))
	return ddl
}
