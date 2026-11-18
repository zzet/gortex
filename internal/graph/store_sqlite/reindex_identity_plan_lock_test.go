package store_sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

// Statements that join an identity list to edges must probe the full key even
// when the first cold load analyzed one edge and a later batch grew without
// updating repo counters.
func TestIdentityListPlansLockedAfterSmallCorpusGrowth(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	require.True(t, s.BeginCoordinatedBulkLoad())
	s.AddBatch([]*graph.Node{{
		ID: "seed/caller", Kind: graph.KindFile, FilePath: "seed/f.go", RepoPrefix: "seed",
	}}, []*graph.Edge{{
		From: "seed/caller", To: "seed/target", Kind: graph.EdgeImports, FilePath: "seed/f.go", Line: 1,
	}})
	require.NoError(t, s.EndCoordinatedBulkLoad())
	writeIndexStateCounters(t, s, 0, "seed", 1, 1)
	require.EqualValues(t, 1, plannerStatsBelievedRows(t.Context(), s.db, plannerStatsEdgesIndex))

	const edgeCount = 16384
	edges := make([]*graph.Edge, edgeCount)
	batch := make([]graph.EdgeReindex, edgeCount)
	for i := range edges {
		file := fmt.Sprintf("repo/f%05d.go", i)
		e := &graph.Edge{From: file, To: "target/pkg", Kind: graph.EdgeImports, FilePath: file, Line: 1, Origin: "old"}
		edges[i] = e
		updated := *e
		updated.Origin = "refreshed"
		batch[i] = graph.EdgeReindex{Edge: &updated, OldTo: e.To, RefreshIdentity: true, OldFilePath: e.FilePath, OldLine: e.Line}
	}
	s.AddBatch(nil, edges)
	health := mustEnsure(t, s)
	require.False(t, health.Stale, "direct graph writes have not updated repo_index_state")
	require.False(t, health.Refreshed)
	require.EqualValues(t, 1, health.Edges.Actual)
	require.Equal(t, edgeCount+1, s.EdgeCount())

	const wantedRows = 160
	args := make([]any, 0, wantedRows*reindexKeyParamsPerRow+1)
	for _, e := range edges[:wantedRows] {
		args = append(args, e.From, e.To, string(e.Kind), e.FilePath, e.Line)
	}
	args = append(args, int64(baseViewGeneration))
	values := multiValues(wantedRows, reindexKeyParamsPerRow)
	crossRepo, crossRepoArgs := crossRepoFlagStatement(baseViewGeneration, edges[:wantedRows])
	statements := []struct {
		name  string
		query string
		args  []any
	}{
		{"reindex_read", sqliteReindexRowsSQL(wantedRows), args},
		{"exact_delete", edgeExactDeleteByIdentitySQL(values), args},
		{"provenance_origins", edgeOriginsByIdentitySQL(values), args},
		{"insert_preflight", existingEdgeIdentitiesSQL(values), args},
		{"cross_repo_flag", crossRepo, crossRepoArgs},
		{"exact_identity_lookup", exactEdgeIdentityQuery(wantedRows), args},
	}

	for _, regime := range []string{"tiny_stats", "fresh_stats"} {
		if !t.Run(regime, func(t *testing.T) {
			if regime == "fresh_stats" {
				writeIndexStateCounters(t, s, 0, "seed", 1, edgeCount+1)
				refreshed := mustEnsure(t, s)
				require.True(t, refreshed.Refreshed)
				require.True(t, strings.HasPrefix(refreshed.Reason, "growth:edges_by_kind"))
			}
			withReceiverPlanLockWriter(t, s, func(ctx context.Context, conn *sql.Conn) {
				for _, st := range statements {
					t.Run(st.name, func(t *testing.T) {
						plan := strings.Join(explainOnConn(t, ctx, conn, st.query, st.args...), "\n")
						require.Contains(t, plan, "(from_id=? AND to_id=? AND kind=? AND file_path=? AND line=? AND view_gen=?)", "%s", plan)
						require.NotContains(t, plan, "edges_by_from_line (view_gen=?)", "%s", plan)
					})
				}
			})
		}) {
			return
		}
		if regime == "tiny_stats" {
			// The refresh still reads and deletes the right rows under these plans.
			stats, err := s.reindexEdgesSetOriented(batch)
			require.NoError(t, err)
			require.Equal(t, edgeCount, stats.deletedRows)
			require.Equal(t, edgeCount, stats.insertedRows)
			var refreshed int
			require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM edges WHERE origin = ?`, "refreshed").Scan(&refreshed))
			require.Equal(t, edgeCount, refreshed)
			require.Equal(t, edgeCount+1, s.EdgeCount())
		}
	}
}
