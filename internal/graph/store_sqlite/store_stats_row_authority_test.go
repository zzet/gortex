package store_sqlite

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

func requirePhysicalStats(t *testing.T, s *Store, nodes, edges int) {
	t.Helper()
	for _, contextual := range []bool{false, true} {
		var got graph.GraphStats
		if contextual {
			var err error
			got, err = s.StatsContext(t.Context())
			require.NoError(t, err)
		} else {
			got = s.Stats()
		}
		require.Equal(t, nodes, got.TotalNodes)
		require.Equal(t, edges, got.TotalEdges)
		kindSum, languageSum := 0, 0
		for _, n := range got.ByKind {
			kindSum += n
		}
		for _, n := range got.ByLanguage {
			languageSum += n
		}
		require.Equal(t, nodes, kindSum)
		require.Equal(t, nodes, languageSum)
	}
}

// Writes after an index-state snapshot include additions, deduplication and
// removals. The physical row authority follows them; index metadata does not.
func TestGlobalStatsRowAuthorityTracksLaterWrites(t *testing.T) {
	s, err := openPristine(t, filepath.Join(t.TempDir(), "stats.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	nodes, edges := rowCounterFixture("repo/a.go", 3)
	s.AddBatch(nodes, edges)
	require.NoError(t, s.SetRepoIndexState(graph.RepoIndexState{RepoPrefix: "repo", NodeCount: 3, EdgeCount: 3}))
	require.NoError(t, s.EnsureRowCounters(t.Context()))
	require.True(t, s.RowCountersReady())
	requirePhysicalStats(t, s, 3, 3)

	laterNodes, laterEdges := rowCounterFixture("repo/later.go", 2)
	s.AddBatch(laterNodes, laterEdges)
	s.AddBatch(laterNodes, laterEdges) // INSERT OR IGNORE/upsert must not overcount.
	s.AddNode(&graph.Node{ID: "external::Target", Kind: graph.KindFunction, Name: "Target", FilePath: "external.go"})
	requirePhysicalStats(t, s, 6, 5)
	s.writeMu.Lock()
	_, err = s.writerDB.Exec(`DELETE FROM edges WHERE view_gen = 0 AND file_path = 'repo/later.go' AND line = 0`)
	s.writeMu.Unlock()
	require.NoError(t, err)
	requirePhysicalStats(t, s, 6, 4)
	st, found, err := s.GetRepoIndexState("repo")
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, 3, st.NodeCount, "the fix must not rewrite per-repo index provenance")
	require.Equal(t, 3, st.EdgeCount)
}

// Fault the legacy count statements: if an installed-counter read ever falls
// back to a physical COUNT query, it fails rather than silently paying a scan.
func TestGlobalStatsInstalledAuthorityAvoidsExactCountQueries(t *testing.T) {
	s, err := openPristine(t, filepath.Join(t.TempDir(), "no-count.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	nodes, edges := rowCounterFixture("repo/a.go", 4)
	s.AddBatch(nodes, edges)
	require.NoError(t, s.EnsureRowCounters(t.Context()))
	require.NoError(t, s.stmtNodeCount.Close())
	require.NoError(t, s.stmtEdgeCount.Close())
	requirePhysicalStats(t, s, 4, 4)
}

func TestGlobalStatsRowAuthorityPreservesSelectedGenerationAndFallback(t *testing.T) {
	for _, installed := range []bool{false, true} {
		name := "uninstalled"
		if installed {
			name = "installed"
		}
		t.Run(name, func(t *testing.T) {
			if !installed {
				t.Setenv("GORTEX_SQLITE_ROW_COUNTERS", "0")
			}
			s, generation, handle := beginManifestGeneration(t)
			baseNodes, baseEdges := rowCounterFixture("repo/shared.go", 3)
			layerNodes, layerEdges := rowCounterFixture("repo/shared.go", 2)
			s.AddBatch(baseNodes, baseEdges)
			handle.AddBatch(layerNodes, layerEdges)
			require.NoError(t, s.SetRepoIndexState(graph.RepoIndexState{RepoPrefix: "repo", NodeCount: 99, EdgeCount: 99}))
			require.NoError(t, handle.SetRepoIndexState(graph.RepoIndexState{RepoPrefix: "repo", NodeCount: 88, EdgeCount: 88}))
			if installed {
				require.NoError(t, s.EnsureRowCounters(t.Context()))
			} else {
				s.rowCountersReady.Store(false)
			}
			requirePhysicalStats(t, s, 3, 3)
			requirePhysicalStats(t, handle, 2, 2)
			requirePhysicalStats(t, s.AtGeneration(generation+1), 0, 0)
		})
	}
}

func TestGlobalStatsCounterFailuresReturnNoPartialResult(t *testing.T) {
	s, err := openPristine(t, filepath.Join(t.TempDir(), "failures.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	nodes, edges := rowCounterFixture("repo/a.go", 2)
	s.AddBatch(nodes, edges)
	require.NoError(t, s.EnsureRowCounters(t.Context()))
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	got, err := s.StatsContext(ctx)
	require.ErrorIs(t, err, context.Canceled)
	require.Equal(t, graph.GraphStats{}, got)

	// A corrupt numeric row must be refused, not replaced with stale repo
	// metadata or a usable partial histogram.
	s.writeMu.Lock()
	_, err = s.writerDB.Exec(`UPDATE generation_row_counts SET nodes = 'not-a-count' WHERE view_gen = 0`)
	s.writeMu.Unlock()
	require.NoError(t, err)
	got, err = s.StatsContext(t.Context())
	require.Error(t, err)
	require.Equal(t, graph.GraphStats{}, got)
}
