package store_sqlite

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

func TestStatsHistogramCoveringPlan(t *testing.T) {
	s := openPayloadStore(t)
	for _, column := range []string{"kind", "language"} {
		rows, err := s.db.Query("EXPLAIN QUERY PLAN SELECT "+column+", COUNT(*) FROM nodes WHERE view_gen = ? GROUP BY "+column, 0)
		require.NoError(t, err)
		var plan []string
		for rows.Next() {
			var id, parent, unused int
			var detail string
			require.NoError(t, rows.Scan(&id, &parent, &unused, &detail))
			plan = append(plan, detail)
		}
		require.NoError(t, rows.Err())
		require.NoError(t, rows.Close())
		require.Contains(t, strings.Join(plan, "\n"), "COVERING INDEX nodes_stats_histogram (view_gen=?)")
	}
}

func TestStatsHistogramCoveringPreservesMembershipAndBulkFallback(t *testing.T) {
	s := openPayloadStore(t)
	seedPayloadControlPlane(t, s)
	generation, selected, err := s.BeginPayloadGeneration(t.Context(), payloadRequest())
	require.NoError(t, err)
	nodes := []*graph.Node{
		{ID: "repo/a.go::Known", Kind: graph.KindFunction, Language: "go", RepoPrefix: "repo"},
		{ID: "external", Kind: graph.NodeKind("unknown"), Language: "custom"},
		{ID: "empty", Kind: graph.NodeKind(""), Language: ""},
	}
	require.NoError(t, s.AddBatchChecked(nodes, nil))
	require.NoError(t, selected.AddBatchChecked(nodes[:1], nil))
	check := func() {
		for _, handle := range []*Store{s, selected, s.AtGeneration(generation + 1)} {
			legacy := handle.Stats()
			checked, err := handle.StatsContext(t.Context())
			require.NoError(t, err)
			require.Equal(t, legacy, checked)
		}
		got, err := s.StatsContext(t.Context())
		require.NoError(t, err)
		require.Equal(t, map[string]int{"function": 1, "unknown": 1, "": 1}, got.ByKind)
		require.Equal(t, map[string]int{"go": 1, "custom": 1, "": 1}, got.ByLanguage)
		got, err = selected.StatsContext(t.Context())
		require.NoError(t, err)
		require.Equal(t, map[string]int{"function": 1}, got.ByKind)
		require.Equal(t, map[string]int{"go": 1}, got.ByLanguage)
	}
	check()
	// Prepared readers must remain valid when cold bulk temporarily drops a
	// read index; the query has no mandatory INDEXED BY dependency.
	_, err = s.writerDB.Exec("DROP INDEX nodes_stats_histogram")
	require.NoError(t, err)
	check()
	_, err = s.writerDB.Exec(nodesStatsHistogramIndexDDL)
	require.NoError(t, err)
	check()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	got, err := s.StatsContext(ctx)
	require.ErrorIs(t, err, context.Canceled)
	require.Equal(t, graph.GraphStats{}, got)
}

func TestStatsHistogramCoveringMigratesVersion35(t *testing.T) {
	path := filepath.Join(t.TempDir(), "histograms.sqlite")
	s, err := Open(path)
	require.NoError(t, err)
	require.NoError(t, s.AddBatchChecked([]*graph.Node{{ID: "untitled", Kind: graph.KindFunction}}, nil))
	_, err = s.writerDB.Exec("DROP INDEX nodes_stats_histogram; PRAGMA user_version=35")
	require.NoError(t, err)
	require.NoError(t, s.Close())
	s, err = Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	var version, present int
	require.NoError(t, s.db.QueryRow("PRAGMA user_version").Scan(&version))
	require.Equal(t, 36, version)
	require.NoError(t, s.db.QueryRow("SELECT COUNT(*) FROM sqlite_schema WHERE name='nodes_stats_histogram'").Scan(&present))
	require.Equal(t, 1, present)
	got, err := s.StatsContext(t.Context())
	require.NoError(t, err)
	require.Equal(t, 1, got.ByKind["function"])
	require.Equal(t, 1, got.ByLanguage[""])
}

func TestStatsHistogramCoveringColdBulkLifecycle(t *testing.T) {
	s, err := openPristine(t, filepath.Join(t.TempDir(), "bulk.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	require.True(t, s.BeginCoordinatedBulkLoad())
	var present int
	require.NoError(t, s.db.QueryRow("SELECT COUNT(*) FROM sqlite_schema WHERE name='nodes_stats_histogram'").Scan(&present))
	require.Zero(t, present)
	require.NoError(t, s.AddBatchChecked([]*graph.Node{{ID: "nameless", Kind: graph.KindFunction, Language: "go"}}, nil))
	before, err := s.StatsContext(t.Context())
	require.NoError(t, err)
	require.Equal(t, before, s.Stats())
	require.NoError(t, s.EndCoordinatedBulkLoad())
	require.NoError(t, s.db.QueryRow("SELECT COUNT(*) FROM sqlite_schema WHERE name='nodes_stats_histogram'").Scan(&present))
	require.Equal(t, 1, present)
	after, err := s.StatsContext(t.Context())
	require.NoError(t, err)
	require.Equal(t, before, after)
}
