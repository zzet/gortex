package store_sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func TestContractReceiptStampReadsUsePayloadFreeCoveringIndex(t *testing.T) {
	s := openCatalogStore(t)
	s.stopCheckpointLoop()
	s.stopMaintenanceLane()
	top := s.AtGeneration(7)
	row := boundaryStorageReceipt("large.go", "large")
	row.CheckoutID = "owner"
	row.Payload = []byte(`{"records":[],"evidence":"` + strings.Repeat("x", 1<<20) + `"}`)
	second := boundaryStorageReceipt("é.go", "unicode")
	second.CheckoutID = row.CheckoutID
	require.NoError(t, top.SetContractBoundaryReceiptsContext(t.Context(), []graph.ContractBoundaryReceipt{row, second}))
	other := row
	other.CheckoutID = "other"
	other.Fingerprint = "other"
	require.NoError(t, top.SetContractBoundaryReceiptsContext(t.Context(), []graph.ContractBoundaryReceipt{other}))
	require.NoError(t, s.AtGeneration(8).SetContractBoundaryReceiptsContext(t.Context(), []graph.ContractBoundaryReceipt{row}))
	var mu sync.Mutex
	var queries []string
	readStatementObserver = func(q string) {
		if strings.HasPrefix(strings.TrimSpace(q), "SELECT file_path,fingerprint,source_fingerprint,accepted FROM generation_contract_boundary_receipt") {
			mu.Lock()
			queries = append(queries, q)
			mu.Unlock()
		}
	}
	t.Cleanup(func() { readStatementObserver = nil })
	all, err := top.ContractBoundaryReceiptStampsContext(t.Context(), "repo", "owner", nil)
	require.NoError(t, err)
	require.Len(t, all, 2)
	mu.Lock()
	firstQueries := append([]string(nil), queries...)
	mu.Unlock()
	require.NotEmpty(t, firstQueries)
	for _, q := range firstQueries {
		plan := planOfUnbound(t, s, q)
		require.Contains(t, plan, "USING COVERING INDEX contract_boundary_receipt_stamps", "the actual stamp projection must avoid BLOB primary records: %s", plan)
	}
	wanted := []string{row.FilePath, row.FilePath, second.FilePath}
	for i := 0; i < lookupChunkSize+3; i++ {
		wanted = append(wanted, fmt.Sprintf("repo/missing-%05d.go", i))
	}
	point, err := top.ContractBoundaryReceiptStampsContext(t.Context(), "repo", "owner", wanted)
	require.NoError(t, err)
	require.Equal(t, all, point, "chunks, duplicates and absent paths retain the exact physical stamp rows")
	missing, err := top.ContractBoundaryReceiptStampsContext(t.Context(), "repo", "missing-actor", wanted)
	require.NoError(t, err)
	require.Empty(t, missing)
	mu.Lock()
	used := append([]string(nil), queries...)
	mu.Unlock()
	require.NotEmpty(t, used, "plan the statements the real stamp API issued")
	for _, q := range used {
		plan := planOfUnbound(t, s, q)
		require.Contains(t, plan, "USING COVERING INDEX contract_boundary_receipt_stamps", "stamp seeks must not touch receipt BLOB records: %s", plan)
	}
	require.False(t, point[row.FilePath].Accepted)
	require.NoError(t, top.AcceptContractBoundaryReceiptsContext(t.Context(), []graph.ContractBoundaryReceipt{row}))
	point, err = top.ContractBoundaryReceiptStampsContext(t.Context(), "repo", "owner", []string{row.FilePath})
	require.NoError(t, err)
	require.True(t, point[row.FilePath].Accepted)
	row.Fingerprint = "changed"
	row.SourceFingerprint = "source-changed"
	require.NoError(t, top.SetContractBoundaryReceiptsContext(t.Context(), []graph.ContractBoundaryReceipt{row}))
	point, err = top.ContractBoundaryReceiptStampsContext(t.Context(), "repo", "owner", []string{row.FilePath})
	require.NoError(t, err)
	require.False(t, point[row.FilePath].Accepted)
	require.Equal(t, row.Fingerprint, point[row.FilePath].Fingerprint)
	require.Equal(t, row.SourceFingerprint, point[row.FilePath].SourceFingerprint)
	full, found, err := top.ContractBoundaryReceiptContext(t.Context(), "repo", "owner", row.FilePath)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, row.Payload, full.Payload, "the index never changes stored evidence")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	partial, err := top.ContractBoundaryReceiptStampsContext(ctx, "repo", "owner", wanted)
	require.ErrorIs(t, err, context.Canceled)
	require.Nil(t, partial)
}

// The predecessor differs by precisely this additive index; every receipt,
// catalog and graph table retains its complete immediately-prior v36 shape.
func contractStampV36Fixture(t *testing.T) (string, graph.ContractBoundaryReceipt) {
	t.Helper()
	const ddl = `CREATE INDEX IF NOT EXISTS contract_boundary_receipt_stamps ON generation_contract_boundary_receipt(view_gen,repo_prefix,checkout_id,file_path,fingerprint,source_fingerprint,accepted);`
	require.Equal(t, 1, strings.Count(schemaSQL, ddl))
	path := filepath.Join(t.TempDir(), "v36.sqlite")
	db, err := sql.Open("sqlite", path)
	require.NoError(t, err)
	defer db.Close()
	_, err = db.Exec(strings.Replace(schemaSQL, ddl, "", 1))
	require.NoError(t, err)
	row := boundaryStorageReceipt("retained.go", "retained")
	row.Accepted = true
	row.Payload = []byte(`{"records":[],"evidence":"` + strings.Repeat("r", 1<<20) + `"}`)
	encoded, err := json.Marshal(row)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO generation_contract_boundary_receipt(view_gen,repo_prefix,checkout_id,file_path,version,fingerprint,source_fingerprint,accepted,receipt) VALUES(9,?,?,?,?,?,?,?,?)`, row.RepoPrefix, row.CheckoutID, row.FilePath, row.Version, row.Fingerprint, row.SourceFingerprint, row.Accepted, encoded)
	require.NoError(t, err)
	_, err = db.Exec(`PRAGMA user_version=36`)
	require.NoError(t, err)
	var indexes int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM sqlite_schema WHERE type='index' AND name='contract_boundary_receipt_stamps'`).Scan(&indexes))
	require.Zero(t, indexes)
	require.NoError(t, db.Close())
	return path, row
}

func TestContractReceiptStampIndexUpgradesV36WithoutChangingPayload(t *testing.T) {
	path, want := contractStampV36Fixture(t)
	for i := 0; i < 2; i++ {
		s, err := Open(path)
		require.NoError(t, err)
		var version int
		require.NoError(t, s.db.QueryRow(`PRAGMA user_version`).Scan(&version))
		require.Equal(t, currentSchemaVersion, version)
		got, found, err := s.AtGeneration(9).ContractBoundaryReceiptContext(t.Context(), want.RepoPrefix, want.CheckoutID, want.FilePath)
		require.NoError(t, err)
		require.True(t, found)
		require.Equal(t, want, *got)
		stamps, err := s.AtGeneration(9).ContractBoundaryReceiptStampsContext(t.Context(), want.RepoPrefix, want.CheckoutID, []string{want.FilePath})
		require.NoError(t, err)
		require.Equal(t, want.Fingerprint, stamps[want.FilePath].Fingerprint)
		require.True(t, stamps[want.FilePath].Accepted)
		require.NoError(t, s.Close())
	}
}

func TestContractReceiptStampIndexOpenFailureDoesNotStampAndRetries(t *testing.T) {
	path, want := contractStampV36Fixture(t)
	db, err := sql.Open("sqlite", path)
	require.NoError(t, err)
	_, err = db.Exec(`CREATE TABLE contract_boundary_receipt_stamps(dummy INTEGER)`)
	require.NoError(t, err)
	require.NoError(t, db.Close())
	failed, err := Open(path)
	if failed != nil {
		_ = failed.Close()
	}
	require.Error(t, err)
	require.Nil(t, failed)
	db, err = sql.Open("sqlite", path)
	require.NoError(t, err)
	var version int
	require.NoError(t, db.QueryRow(`PRAGMA user_version`).Scan(&version))
	require.Equal(t, 36, version)
	_, err = db.Exec(`DROP TABLE contract_boundary_receipt_stamps`)
	require.NoError(t, err)
	require.NoError(t, db.Close())
	reopened, err := Open(path)
	require.NoError(t, err)
	defer reopened.Close()
	got, found, err := reopened.AtGeneration(9).ContractBoundaryReceiptContext(t.Context(), want.RepoPrefix, want.CheckoutID, want.FilePath)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, want, *got)
	require.NoError(t, reopened.db.QueryRow(`PRAGMA user_version`).Scan(&version))
	require.Equal(t, currentSchemaVersion, version)
}
