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
	require.Contains(t, plan, "SEARCH e USING INDEX "+contractOwnerGenerationIndexName+" (view_gen=? AND kind=?)", "historical ownership rows and unrelated edge kinds must stay outside the seek")
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

// Same owner identities in several physical generations must not leak into a
// selected generation. The plan assertion is independent of machine timing.
func TestContractRepoOwnerQuerySeeksGenerationAndPreservesFullRows(t *testing.T) {
	s, expected := contractRepoOwnerSeekFixture(t)
	for _, generation := range []int64{41, 42, 43} {
		h := s.AtGeneration(generation)
		source := &graph.Node{ID: "repo/wire.go::Wire", Kind: graph.KindFunction, FilePath: "repo/wire.go", RepoPrefix: "repo"}
		edges := make([]*graph.Edge, len(expected))
		for i, row := range expected {
			e := *row
			e.Line += int(generation)
			edges[i] = &e
		}
		require.NoError(t, h.AddBatchChecked([]*graph.Node{source}, edges))
		p, err := h.LayerContractRepoProjectionContext(t.Context(), "repo")
		require.NoError(t, err)
		require.Len(t, p.OwnerRows, len(edges))
		got := map[string]*graph.Edge{}
		for _, row := range p.OwnerRows {
			got[string(row.Edge.Kind)] = row.Edge
		}
		for _, e := range edges {
			require.Equal(t, e, got[string(e.Kind)])
		}
	}
	empty, err := s.AtGeneration(44).LayerContractRepoProjectionContext(t.Context(), "repo")
	require.NoError(t, err)
	require.Empty(t, empty.OwnerRows)
	p, err := s.LayerContractRepoProjectionContext(t.Context(), "repo")
	require.NoError(t, err)
	require.Len(t, p.OwnerRows, len(expected))
	for _, row := range p.OwnerRows {
		require.Less(t, row.Edge.Line, 41)
	}
	// Reopening an existing database installs the new sparse index too.
	_, err = s.writerDB.Exec(`DROP INDEX ` + contractOwnerGenerationIndexName)
	require.NoError(t, err)
	require.NoError(t, s.Close())
	reopened, err := Open(s.dbPath)
	require.NoError(t, err)
	defer reopened.Close()
	p, err = reopened.AtGeneration(41).LayerContractRepoProjectionContext(t.Context(), "repo")
	require.NoError(t, err)
	require.Len(t, p.OwnerRows, len(expected))
}
