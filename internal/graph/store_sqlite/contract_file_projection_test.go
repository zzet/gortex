package store_sqlite

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func TestContractFileProjectionSQLiteFullRowsAndCheckedFailures(t *testing.T) {
	s := newTestProjectionStore(t)
	file := "/repo/recorded.go"
	source := &graph.Node{ID: "/repo/source.go::handler", Kind: graph.KindFunction, FilePath: "/repo/source.go", RepoPrefix: "repo"}
	target := &graph.Node{ID: "contract", Kind: graph.KindContract, FilePath: "/repo/canonical.go", Meta: map[string]any{"contract_meta": "canonical"}}
	sibling := &graph.Node{ID: "/other/source.go::handler", Kind: graph.KindFunction, FilePath: "/other/source.go", RepoPrefix: "other"}
	legacy := &graph.Node{ID: "legacy", Kind: graph.KindContract, FilePath: file, Meta: map[string]any{"contract_meta": "legacy"}}
	s.AddBatch([]*graph.Node{source, target, sibling, legacy}, []*graph.Edge{
		{From: source.ID, To: target.ID, Kind: graph.EdgeProvides, FilePath: file, Meta: map[string]any{"contract_meta": map[string]any{"file": file, "payload": "owner"}}},
		{From: sibling.ID, To: target.ID, Kind: graph.EdgeConsumes, FilePath: "/other/recorded.go"},
		{From: source.ID, To: legacy.ID, Kind: graph.EdgeHandlesRoute, FilePath: "/repo/foreign.go"},
	})
	p, err := s.LoadContractFileProjectionContext(context.Background(), "repo", []string{file})
	require.NoError(t, err)
	require.Len(t, p.OwnerRows, 3)
	require.Len(t, p.ScalarNodes, 1)
	require.Equal(t, target.Meta, p.Targets[target.ID].Meta)
	for _, row := range p.OwnerRows {
		if row.Edge.Kind == graph.EdgeProvides {
			require.Equal(t, "owner", row.Edge.Meta["contract_meta"].(map[string]any)["payload"])
		}
	}
	p, err = s.LoadContractFileProjectionContext(context.Background(), "repo", []string{source.FilePath})
	require.NoError(t, err)
	require.Len(t, p.OffFileOwnerRows, 2)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	p, err = s.LoadContractFileProjectionContext(ctx, "repo", []string{file})
	require.ErrorIs(t, err, context.Canceled)
	require.Equal(t, graph.ContractFileProjection{}, p)
	// A malformed full row must fail the checked projection, even though some
	// preceding physical queries may already have yielded valid graph rows.
	_, err = s.writerDB.Exec(`UPDATE nodes SET meta = ? WHERE id = ? AND view_gen=0`, []byte("invalid metadata"), target.ID)
	require.NoError(t, err)
	p, err = s.LoadContractFileProjectionContext(context.Background(), "repo", []string{file})
	require.Error(t, err)
	require.Equal(t, graph.ContractFileProjection{}, p)
}

func TestContractFileProjectionSQLiteRecordingIndexPlans(t *testing.T) {
	s := newTestProjectionStore(t)
	for _, composite := range []bool{false, true} {
		if composite {
			_, err := s.writerDB.Exec(edgesByFileGenerationIndexDDL)
			require.NoError(t, err)
		}
		index := "edges_by_file"
		if composite {
			index = edgesByFileGenerationIndexName
		}
		rows, err := s.db.Query(`EXPLAIN QUERY PLAN SELECT e.from_id FROM edges AS e INDEXED BY `+index+` WHERE e.file_path IN (?) AND e.view_gen=? AND e.kind IN `+contractOwnerKindsSQL, "/repo/a.go", 0)
		require.NoError(t, err)
		var plan strings.Builder
		for rows.Next() {
			var id, parent, unused int
			var detail string
			require.NoError(t, rows.Scan(&id, &parent, &unused, &detail))
			plan.WriteString(detail)
		}
		require.NoError(t, rows.Err())
		require.NoError(t, rows.Close())
		require.Contains(t, plan.String(), index)
		if composite {
			require.Contains(t, plan.String(), "file_path=? AND view_gen=?")
		}
		// The actual read supports both catalog states without constructing an index.
		p, err := s.LoadContractFileProjectionContext(context.Background(), "repo", []string{"/repo/a.go"})
		require.NoError(t, err)
		require.Empty(t, p.OwnerRows)
	}
}

func TestContractFileProjectionSQLiteRowBoundNeverReturnsPartial(t *testing.T) {
	s := newTestProjectionStore(t)
	s.AddBatch([]*graph.Node{{ID: "a", Kind: graph.KindFunction}, {ID: "b", Kind: graph.KindFunction}}, nil)
	tx, err := s.db.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	defer func() { require.NoError(t, tx.Rollback()) }()
	budget := 1
	nodes, err := s.contractProjectionNodes(context.Background(), tx, `SELECT `+lookupNodeCols+` FROM nodes WHERE view_gen=0`, nil, &budget)
	require.ErrorIs(t, err, graph.ErrContractProjectionLimit)
	require.Nil(t, nodes)
}
