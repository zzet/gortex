package store_sqlite

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func streamCountFixture(t *testing.T, tokens []string) (*Store, int64) {
	t.Helper()
	store, generation, handle := beginManifestGeneration(t)
	nodes, items := ftsDocs("", len(tokens), func(i int) string { return tokens[i] })
	require.NoError(t, handle.AddBatchChecked(nodes, nil))
	require.NoError(t, handle.BatchUpsertSymbolFTS(items))
	require.NoError(t, store.PublishPayloadGeneration(t.Context(), generation, 9001))
	return store, generation
}

func TestSymbolFTSAddedStreamRequiresCompleteBoundedASCII(t *testing.T) {
	for _, tc := range []struct {
		name     string
		tokens   []string
		rows     int
		bytes    int64
		expected int64
	}{
		{"late unicode", []string{"hand req", "hand req", "über hand req"}, 10, 1024, 3},
		{"late byte cap", []string{"hand req", "hand req", "hand req"}, 10, 15, 3},
		{"row cap", []string{"hand req", "hand req", "hand req"}, 2, 1024, 3},
		{"truncated EOF", []string{"hand req", "hand req", "hand req"}, 2, 1024, 2},
		{"unexpected EOF", []string{"hand req", "hand req", "hand req"}, 10, 1024, 4},
		{"late oversized row", []string{"hand req", "hand req", strings.Repeat("h", symbolFTSStreamSingleRow+1)}, 10, symbolFTSStreamBytes, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			store, generation := streamCountFixture(t, tc.tokens)
			tx, err := store.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
			require.NoError(t, err)
			defer tx.Rollback() //nolint:errcheck // read-only snapshot
			budget := symbolFTSStreamBudget{rows: tc.rows, bytes: tc.bytes}
			counts, err := symbolFTSStreamAddedCounts(ctx, tx, generation, tc.expected, []string{"hand", "req"}, &budget)
			require.NoError(t, err)
			require.Nil(t, counts, "neither a partial count nor a proved zero may survive a declined scan")
			// The exact existing fallback remains available in the same snapshot.
			for _, prefix := range []string{"hand", "req"} {
				got, err := store.generationsPrefixCountTx(ctx, tx, []int64{generation}, prefix, ftsPrefixTerm(prefix), map[int64][]SymbolFTSRow{})
				require.NoError(t, err)
				var want int64
				require.NoError(t, tx.QueryRowContext(ctx, `SELECT count(*) FROM symbol_fts CROSS JOIN symbol_fts_rowid m ON m.fts_rowid=symbol_fts.rowid AND m.view_gen=? WHERE symbol_fts MATCH ?`, generation, ftsPrefixTerm(prefix)).Scan(&want))
				require.Equal(t, want, got)
			}
		})
	}
}

func TestSymbolFTSAddedStreamPreservesSparseOwnershipAndSharesBudget(t *testing.T) {
	ctx := t.Context()
	store, first, firstHandle := beginManifestGeneration(t)
	second, secondHandle := beginPrefixBatchGeneration(t, store, "stream-foreign")
	seedPrefixBatchDocuments(t, firstHandle, "stream-first-a", "hand hand req", 2)
	seedPrefixBatchDocuments(t, secondHandle, "stream-foreign", "hand req", 3)
	seedPrefixBatchDocuments(t, firstHandle, "stream-first-b", "hand", 2)
	for _, generation := range []int64{first, second} {
		require.NoError(t, store.PublishPayloadGeneration(ctx, generation, 9001))
	}
	tx, err := store.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	require.NoError(t, err)
	defer tx.Rollback() //nolint:errcheck // read-only snapshot
	budget := symbolFTSStreamBudget{rows: 6, bytes: 1024}
	counts, err := symbolFTSStreamAddedCounts(ctx, tx, first, 4, []string{"hand", "req", "missing"}, &budget)
	require.NoError(t, err)
	require.Equal(t, map[string]int64{"hand": 4, "req": 2, "missing": 0}, counts)
	require.Equal(t, 2, budget.rows)
	declined, err := symbolFTSStreamAddedCounts(ctx, tx, second, 3, []string{"hand", "req"}, &budget)
	require.NoError(t, err)
	require.Nil(t, declined, "the remaining generation cannot reset a query's scan allowance")
	for _, prefix := range []string{"hand", "req", "missing"} {
		var want int64
		require.NoError(t, tx.QueryRowContext(ctx, `SELECT count(*) FROM symbol_fts CROSS JOIN symbol_fts_rowid m ON m.fts_rowid=symbol_fts.rowid AND m.view_gen=? WHERE symbol_fts MATCH ?`, first, ftsPrefixTerm(prefix)).Scan(&want))
		require.Equal(t, want, counts[prefix])
	}
}

func TestSymbolFTSAddedStreamPinsSnapshotAndRejectsErrors(t *testing.T) {
	ctx := t.Context()
	store, generation := streamCountFixture(t, []string{"hand req", "hand req"})
	tx, err := store.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	require.NoError(t, err)
	defer tx.Rollback() //nolint:errcheck // read-only snapshot
	_, err = symbolFTSStatsTx(ctx, tx)
	require.NoError(t, err)
	next, nextHandle := beginPrefixBatchGeneration(t, store, "stream-next")
	seedPrefixBatchDocuments(t, nextHandle, "stream-next", "hand req", 1)
	require.NoError(t, store.PublishPayloadGeneration(ctx, next, 9001))
	budget := symbolFTSStreamBudget{rows: 10, bytes: 1024}
	counts, err := symbolFTSStreamAddedCounts(ctx, tx, generation, 2, []string{"hand", "req"}, &budget)
	require.NoError(t, err)
	require.Equal(t, map[string]int64{"hand": 2, "req": 2}, counts)
	counts, err = symbolFTSStreamAddedCounts(ctx, tx, next, 1, []string{"hand", "req"}, &budget)
	require.NoError(t, err)
	require.Nil(t, counts, "a later generation cannot enter the pinned snapshot")
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	counts, err = symbolFTSStreamAddedCounts(cancelled, tx, generation, 2, []string{"hand", "req"}, &budget)
	require.ErrorIs(t, err, context.Canceled)
	require.Nil(t, counts)
	require.NoError(t, tx.Rollback())
	counts, err = symbolFTSStreamAddedCounts(ctx, tx, generation, 2, []string{"hand", "req"}, &budget)
	require.ErrorIs(t, err, sql.ErrTxDone)
	require.Nil(t, counts, "a failed query is never a count certificate")
}

func TestSymbolFTSAddedStreamDeclinesUnsupportedPrefixes(t *testing.T) {
	ctx := t.Context()
	store, generation := streamCountFixture(t, []string{"handler request"})
	tx, err := store.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	require.NoError(t, err)
	defer tx.Rollback() //nolint:errcheck // read-only snapshot
	for _, prefixes := range [][]string{{"hand", "ü"}, {"hand", "hand req"}, {"hand", "hand"}, {"hand"}, make([]string, symbolFTSStreamPrefixes+1)} {
		budget := symbolFTSStreamBudget{rows: 10, bytes: 1024}
		counts, err := symbolFTSStreamAddedCounts(ctx, tx, generation, 1, prefixes, &budget)
		require.NoError(t, err)
		require.Nil(t, counts)
		require.Equal(t, 10, budget.rows, "unsupported terms never start the stream")
	}
}
