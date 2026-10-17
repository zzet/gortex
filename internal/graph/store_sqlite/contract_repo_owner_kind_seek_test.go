package store_sqlite

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func contractRepoOwnerSeekFixture(t *testing.T) (*Store, []*graph.Edge) {
	t.Helper()
	s := openCatalogStore(t)
	local := &graph.Node{ID: "repo/wire.go::Wire", Kind: graph.KindFunction, FilePath: "repo/wire.go", RepoPrefix: "repo"}
	foreign := &graph.Node{ID: "other/wire.go::Wire", Kind: graph.KindFunction, FilePath: "other/wire.go", RepoPrefix: "other"}
	target := &graph.Node{ID: "repo/Target", Kind: graph.KindContract, FilePath: local.FilePath, RepoPrefix: "repo", Meta: map[string]any{"contract_type": "http"}}
	expected := []*graph.Edge{
		{From: local.ID, To: target.ID, Kind: graph.EdgeProvides, FilePath: local.FilePath, Line: 3, Confidence: 0.75, ConfidenceLabel: "high", Origin: "parser", Meta: map[string]any{graph.MetaDIBinding: graph.DIBindingUseValue, graph.MetaDIToken: "TOKEN"}},
		{From: local.ID, To: target.ID, Kind: graph.EdgeHandlesRoute, FilePath: local.FilePath, Line: 4, Meta: map[string]any{"path": "/route"}},
		{From: "inherited/wire.go::Wire", To: target.ID, Kind: graph.EdgeConsumes, FilePath: "repo/inherited.go", Line: 5, Meta: map[string]any{"contract_owner_repo_prefix": "repo", graph.MetaDIToken: "TOKEN"}},
	}
	other := []*graph.Edge{
		{From: foreign.ID, To: target.ID, Kind: graph.EdgeProvides, FilePath: foreign.FilePath},
		{From: "inherited/other.go::Wire", To: target.ID, Kind: graph.EdgeConsumes, FilePath: "other/inherited.go", Meta: map[string]any{"contract_owner_repo_prefix": "other"}},
		{From: local.ID, To: foreign.ID, Kind: graph.EdgeCalls, FilePath: local.FilePath},
	}
	edges := append(append([]*graph.Edge{}, expected...), other...)
	require.NoError(t, s.AddBatchChecked([]*graph.Node{local, foreign, target}, edges))
	return s, expected
}

func TestContractRepoOwnerQueryUsesKindSeekAndPreservesScope(t *testing.T) {
	s, expected := contractRepoOwnerSeekFixture(t)
	p, err := s.LayerContractRepoProjectionContext(t.Context(), "repo")
	require.NoError(t, err)
	require.Len(t, p.ScalarNodes, 1)
	require.Len(t, p.OwnerRows, len(expected))
	actual := map[string]*graph.Edge{}
	for _, row := range p.OwnerRows {
		require.Equal(t, "repo", row.RepoPrefix)
		actual[string(row.Edge.Kind)] = row.Edge
	}
	for _, edge := range expected {
		require.Equal(t, edge, actual[string(edge.Kind)], "metadata, inherited-source and recording ownership remain complete")
	}
	rows, err := s.db.QueryContext(t.Context(), `EXPLAIN QUERY PLAN `+contractRepoOwnerRowsQuery(), int64(0), "repo", graph.ContractProjectionRowLimit+1)
	require.NoError(t, err)
	var plans []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		require.NoError(t, rows.Scan(&id, &parent, &unused, &detail))
		plans = append(plans, detail)
	}
	require.NoError(t, rows.Err())
	require.NoError(t, rows.Close())
	plan := strings.Join(plans, "\n")
	require.Contains(t, plan, "SEARCH e USING INDEX edges_by_kind", "unrelated edge kinds must not be scanned through a generation-only prefix")
}

func TestContractRepoOwnerQueryRetainsLimitsDecodeAndCancellation(t *testing.T) {
	s, _ := contractRepoOwnerSeekFixture(t)
	tx, err := s.db.BeginTx(t.Context(), &sql.TxOptions{ReadOnly: true})
	require.NoError(t, err)
	budget := 1
	_, err = s.contractProjectionEdges(t.Context(), tx, contractRepoOwnerRowsQuery(), []any{int64(0), "repo", 2}, &budget)
	require.ErrorIs(t, err, graph.ErrContractProjectionLimit)
	require.NoError(t, tx.Rollback())
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	p, err := s.LayerContractRepoProjectionContext(ctx, "repo")
	require.ErrorIs(t, err, context.Canceled)
	require.Equal(t, graph.ContractFileProjection{}, p)
	_, err = s.writerDB.Exec(`UPDATE edges SET meta=x'0001FF' WHERE view_gen=0 AND from_id='repo/wire.go::Wire' AND kind='provides'`)
	require.NoError(t, err)
	p, err = s.LayerContractRepoProjectionContext(t.Context(), "repo")
	require.Error(t, err)
	require.Equal(t, graph.ContractFileProjection{}, p, "no partial result after selected edge metadata corruption")
}
