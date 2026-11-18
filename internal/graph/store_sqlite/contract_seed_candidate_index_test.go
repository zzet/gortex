package store_sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func seedCandidateRawRows(t *testing.T, tx *sql.Tx, query string, args ...any) []string {
	t.Helper()
	rows, err := tx.QueryContext(t.Context(), query, args...)
	require.NoError(t, err)
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id, kind, language string
		var meta []byte
		require.NoError(t, rows.Scan(&id, &kind, &language, &meta))
		raw, err := json.Marshal([][]byte{[]byte(id), []byte(kind), []byte(language), meta})
		require.NoError(t, err)
		out = append(out, string(raw))
	}
	require.NoError(t, rows.Err())
	require.NoError(t, rows.Close())
	return out
}

func TestContractSeedCandidateIndexCoversAllValidationRows(t *testing.T) {
	s := openPayloadStore(t)
	nodes := []*graph.Node{
		{ID: "repo/field", Kind: graph.KindField, RepoPrefix: "repo", Meta: map[string]any{"ordinary": "not a seed"}},
		{ID: "repo/type", Kind: graph.KindType, RepoPrefix: "repo", Meta: map[string]any{"spring_config_keys": nil}},
		{ID: "repo/interface-é", Kind: graph.KindInterface, RepoPrefix: "repo", Language: "go", Meta: map[string]any{"ordinary": "é"}},
		{ID: "repo/method", Kind: graph.KindMethod, RepoPrefix: "repo", Language: "java", Meta: map[string]any{"signature": "kept"}},
		{ID: "repo/noncandidate", Kind: graph.KindConfigKey, RepoPrefix: "repo", Meta: map[string]any{"spring_config_keys": "outside kinds"}},
		{ID: "other/field", Kind: graph.KindField, RepoPrefix: "other"},
	}
	require.NoError(t, s.AddBatchChecked(nodes, nil))
	// Keep legacy bytes, escaped top-level keys and nil values authoritative.
	_, err := s.writerDB.Exec(`UPDATE nodes SET meta=? WHERE id='repo/type' AND view_gen=0`, []byte(`{"spring_config_\u006beys":null,"kept":"legacy"}`))
	require.NoError(t, err)
	kinds, _ := json.Marshal([]string{string(graph.KindField), string(graph.KindType), string(graph.KindInterface), string(graph.KindMethod)})
	tx, err := s.db.BeginTx(t.Context(), &sql.TxOptions{ReadOnly: true})
	require.NoError(t, err)
	old := seedCandidateRawRows(t, tx, repoNodesByKindsProjectionQuery(false, "n.id,n.kind,n.language,n.meta"), string(kinds), "repo", int64(0), int64(0))
	current := seedCandidateRawRows(t, tx, contractSeedCandidateRowsQuery(), string(kinds), "repo", int64(0))
	require.NoError(t, tx.Rollback())
	require.Len(t, old, 4)
	require.Equal(t, old, current, "all candidate metadata bytes, including non-seeds, retain exact ordered projection")
	require.Contains(t, planOfUnbound(t, s, contractSeedCandidateRowsQuery()), "USING COVERING INDEX "+contractSeedCandidateIndexName)
	want, err := contractSeedNodesWideReference(t, s, "repo")
	require.NoError(t, err)
	got, err := s.ContractSeedNodesContext(t.Context(), "repo")
	require.NoError(t, err)
	require.Equal(t, want, got)
	require.Len(t, got, 2)
	// A non-seed's malformed metadata must never become a certified absence.
	_, err = s.writerDB.Exec(`UPDATE nodes SET meta=x'00ff' WHERE id='repo/field' AND view_gen=0`)
	require.NoError(t, err)
	_, err = contractSeedNodesWideReference(t, s, "repo")
	require.Error(t, err)
	got, err = s.ContractSeedNodesContext(t.Context(), "repo")
	require.Error(t, err)
	require.Nil(t, got)
}

func TestContractSeedCandidateIndexOpenRepairPreservesRawPayload(t *testing.T) {
	s := openPayloadStore(t)
	require.NoError(t, s.AddBatchChecked([]*graph.Node{{ID: "repo/legacy", Kind: graph.KindType, RepoPrefix: "repo", Meta: map[string]any{"spring_config_keys": "kept"}}}, nil))
	legacy := []byte(`{"spring_config_keys":null,"raw":"unchanged"}`)
	_, err := s.writerDB.Exec(`UPDATE nodes SET meta=? WHERE id='repo/legacy' AND view_gen=0`, legacy)
	require.NoError(t, err)
	want, err := s.ContractSeedNodesContext(t.Context(), "repo")
	require.NoError(t, err)
	require.Len(t, want, 1)
	_, err = s.writerDB.Exec(`DROP INDEX ` + contractSeedCandidateIndexName)
	require.NoError(t, err)
	path := s.dbPath
	require.NoError(t, s.Close())
	reopened, err := Open(path)
	require.NoError(t, err)
	defer reopened.Close()
	require.True(t, indexNames(t, reopened.db)[contractSeedCandidateIndexName])
	var raw []byte
	require.NoError(t, reopened.db.QueryRow(`SELECT meta FROM nodes WHERE id='repo/legacy' AND view_gen=0`).Scan(&raw))
	require.Equal(t, legacy, raw)
	got, err := reopened.ContractSeedNodesContext(context.Background(), "repo")
	require.NoError(t, err)
	require.Equal(t, want, got, "installing the additive index never rewrites graph payloads")
}
