package store_sqlite

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	modernsqlite "modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

func TestFindEdgesByIdentitiesSQLiteExactPayloadAndReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "exact-identity.sqlite")
	store, err := Open(path)
	require.NoError(t, err)

	first := &graph.Edge{
		From: "repo:a", To: "repo:b", Kind: graph.EdgeCalls,
		FilePath: "repo/a.go", Line: 17,
		Confidence: 0.4, Origin: "parser", Meta: map[string]any{"version": "old"},
	}
	sameSite := &graph.Edge{
		From: "repo:a", To: "repo:c", Kind: graph.EdgeCalls,
		FilePath: "repo/a.go", Line: 17,
		Confidence: 0.8, Origin: "lsp", Meta: map[string]any{"version": "other"},
	}
	store.AddEdge(first)
	store.AddEdge(sameSite)

	firstKey := graph.EdgeIdentityFor(first)
	sameSiteKey := graph.EdgeIdentityFor(sameSite)
	found := store.FindEdgesByIdentities([]graph.EdgeIdentity{sameSiteKey, sameSiteKey})
	require.Len(t, found, 1)
	require.Equal(t, sameSite.To, found[sameSiteKey].To)
	require.NotContains(t, found, firstKey, "unrequested same-site edge must not leak into an exact lookup")

	require.True(t, store.RemoveEdge(first.From, first.To, first.Kind))
	replacement := &graph.Edge{
		From: first.From, To: first.To, Kind: first.Kind,
		FilePath: first.FilePath, Line: first.Line,
		Confidence: 0.95, ConfidenceLabel: "high", Origin: "watcher",
		Tier: "confirmed", CrossRepo: true, Meta: map[string]any{"version": "new"},
	}
	store.AddEdge(replacement)

	found = store.FindEdgesByIdentities([]graph.EdgeIdentity{firstKey, sameSiteKey})
	require.Len(t, found, 2)
	require.Equal(t, replacement.Confidence, found[firstKey].Confidence)
	require.Equal(t, replacement.ConfidenceLabel, found[firstKey].ConfidenceLabel)
	require.Equal(t, replacement.Origin, found[firstKey].Origin)
	require.Equal(t, replacement.Tier, found[firstKey].Tier)
	require.Equal(t, replacement.CrossRepo, found[firstKey].CrossRepo)
	require.Equal(t, "new", found[firstKey].Meta["version"])

	require.NoError(t, store.Close())
	reopened, err := Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = reopened.Close() })

	found = reopened.FindEdgesByIdentities([]graph.EdgeIdentity{firstKey, sameSiteKey})
	require.Len(t, found, 2)
	require.Equal(t, replacement.Confidence, found[firstKey].Confidence)
	require.Equal(t, replacement.Origin, found[firstKey].Origin)
	require.Equal(t, "new", found[firstKey].Meta["version"])
	require.Equal(t, "other", found[sameSiteKey].Meta["version"])
}

func TestFindEdgesByIdentitiesSQLiteUsesExistingUniqueIndexWithoutTempTree(t *testing.T) {
	store := openReindexReceiptTestStore(t)
	uniqueIndex := exactEdgeIdentityUniqueIndex(t, store)
	query := exactEdgeIdentityQuery(2)
	require.NotContains(t, strings.ToUpper(query), "DISTINCT")

	rows, err := store.db.Query("EXPLAIN QUERY PLAN "+query,
		"a", "b", graph.EdgeCalls, "a.go", 1,
		"c", "d", graph.EdgeCalls, "c.go", 2,
		baseViewGeneration,
	)
	require.NoError(t, err)
	defer rows.Close()

	var details []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		require.NoError(t, rows.Scan(&id, &parent, &unused, &detail))
		details = append(details, detail)
	}
	require.NoError(t, rows.Err())
	plan := strings.Join(details, "\n")
	// The index name alone is not enough: a whole-index scan names it too.
	// The property under lock is the per-key probe, so the step that reads
	// the identity index has to be a SEARCH.
	var identityStep string
	for _, detail := range details {
		if strings.Contains(detail, uniqueIndex) {
			identityStep = strings.TrimSpace(detail)
			break
		}
	}
	require.NotEmpty(t, identityStep, "identity matching must read the six-column UNIQUE index")
	require.True(t, strings.HasPrefix(identityStep, "SEARCH"),
		"identity matching must seek the six-column UNIQUE index, not scan it: %s", identityStep)
	require.Contains(t, plan, "INTEGER PRIMARY KEY", "full rows must be re-fetched through ordered row-id seeks")
	require.NotContains(t, strings.ToUpper(plan), "TEMP B-TREE")
}

func TestFindEdgesByIdentitiesSQLiteBoundsStatementsByRuntimeVariableLimit(t *testing.T) {
	store := openReindexReceiptTestStore(t)
	const keyCount = 16_384
	identities := make([]graph.EdgeIdentity, 0, keyCount+100)
	for i := 0; i < keyCount; i++ {
		identities = append(identities, graph.EdgeIdentity{
			From: fmt.Sprintf("repo:from:%05d", i), To: fmt.Sprintf("repo:to:%05d", i),
			Kind: graph.EdgeCalls, FilePath: fmt.Sprintf("repo/file/%05d.go", i), Line: i,
		})
	}
	identities = append(identities, identities[:100]...)

	found, stats := store.findEdgesByIdentities(identities)
	require.Empty(t, found)
	require.Equal(t, sqliteBatchVariableHardCap, store.batchVariableLimit,
		"the modernc runtime limit should be clamped by the bounded 8192-variable policy")
	rowsPerStatement := batchRowsForVariableLimit(store.batchVariableLimit, exactEdgeIdentityParamsPerRow, exactEdgeIdentityMaxRows)
	expectedStatements := (keyCount + rowsPerStatement - 1) / rowsPerStatement
	require.Equal(t, expectedStatements, stats.Statements)
	require.LessOrEqual(t, stats.Statements, 12, "a 16K page must not regress into small fixed chunks or N+1")
	require.Equal(t, rowsPerStatement, stats.MaxKeys)
	require.Zero(t, stats.Retries)
}

func TestFindEdgesByIdentitiesSQLiteBoundsRetainedArgumentBytes(t *testing.T) {
	store := openReindexReceiptTestStore(t)
	const keyCount = 1600
	padding := strings.Repeat("x", 900)
	identities := make([]graph.EdgeIdentity, 0, keyCount)
	for i := 0; i < keyCount; i++ {
		suffix := fmt.Sprintf("-%04d", i)
		identities = append(identities, graph.EdgeIdentity{
			From:     "repo:from:" + padding + suffix,
			To:       "repo:to:" + padding + suffix,
			Kind:     graph.EdgeCalls,
			FilePath: "repo/file/" + padding + suffix + ".go",
			Line:     i,
		})
	}

	found, stats := store.findEdgesByIdentities(identities)
	require.Empty(t, found)
	require.Greater(t, stats.Statements, 1, "the 4MiB byte policy must split a variable-safe batch")
	require.LessOrEqual(t, stats.MaxBoundBytes, sqliteBatchMaxBoundBytes)
	require.Less(t, stats.MaxKeys, keyCount)
}

func TestFindEdgesByIdentitiesSQLiteAdaptsToConnectionVariableLimit(t *testing.T) {
	store := openReindexReceiptTestStore(t)
	store.db.SetMaxOpenConns(1)
	store.db.SetMaxIdleConns(1)

	conn, err := store.db.Conn(context.Background())
	require.NoError(t, err)
	previous, err := modernsqlite.Limit(conn, int(sqlite3.SQLITE_LIMIT_VARIABLE_NUMBER), 80)
	require.NoError(t, err)
	require.Greater(t, previous, 80)
	require.NoError(t, conn.Close())

	store.writeMu.Lock()
	store.batchVariableLimit = sqliteBatchVariableHardCap
	store.writeMu.Unlock()

	identities := make([]graph.EdgeIdentity, 100)
	for i := range identities {
		identities[i] = graph.EdgeIdentity{
			From: fmt.Sprintf("from-%03d", i), To: fmt.Sprintf("to-%03d", i),
			Kind: graph.EdgeCalls, FilePath: fmt.Sprintf("file-%03d.go", i), Line: i,
		}
	}
	found, stats := store.findEdgesByIdentities(identities)
	require.Empty(t, found)
	require.Positive(t, stats.Retries)
	require.Less(t, stats.Statements, len(identities), "adaptive retry must remain set-oriented")
	require.LessOrEqual(t, store.batchVariableLimit, 80)
}

func exactEdgeIdentityUniqueIndex(t *testing.T, store *Store) string {
	t.Helper()
	rows, err := store.db.Query(`PRAGMA index_list('edges')`)
	require.NoError(t, err)

	var uniqueIndexes []string
	for rows.Next() {
		var sequence, unique, partial int
		var name, origin string
		require.NoError(t, rows.Scan(&sequence, &name, &unique, &origin, &partial))
		if unique == 1 {
			uniqueIndexes = append(uniqueIndexes, name)
		}
	}
	require.NoError(t, rows.Err())
	require.NoError(t, rows.Close())

	for _, name := range uniqueIndexes {
		indexRows, queryErr := store.db.Query(`PRAGMA index_info('` + name + `')`)
		require.NoError(t, queryErr)
		var columns []string
		for indexRows.Next() {
			var sequence, columnID int
			var column string
			require.NoError(t, indexRows.Scan(&sequence, &columnID, &column))
			columns = append(columns, column)
		}
		require.NoError(t, indexRows.Err())
		require.NoError(t, indexRows.Close())
		if strings.Join(columns, ",") == "from_id,to_id,kind,file_path,line,view_gen" {
			return name
		}
	}
	t.Fatal("edges table has no six-column logical-identity UNIQUE index")
	return ""
}
