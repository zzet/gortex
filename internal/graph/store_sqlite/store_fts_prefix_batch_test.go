package store_sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

func seedPrefixBatchDocuments(t *testing.T, target *Store, label, tokens string, count int) {
	t.Helper()
	nodes, items := ftsDocs(tokens, count, func(int) string { return "" })
	for i := range nodes {
		nodes[i].ID = fmt.Sprintf("%s-%s-%d", nodes[i].ID, label, i)
		items[i].NodeID = nodes[i].ID
	}
	require.NoError(t, target.AddBatchChecked(nodes, nil))
	require.NoError(t, target.BatchUpsertSymbolFTS(items))
}

func beginPrefixBatchGeneration(t *testing.T, store *Store, label string) (int64, *Store) {
	t.Helper()
	req := payloadRequest()
	req.LayerID, req.TreeOID, req.CreatedAt = label, label+"-tree", 9000
	generation, handle, err := store.BeginPayloadGeneration(t.Context(), req)
	require.NoError(t, err)
	return generation, handle
}

// The public incremental snapshot must batch its added generations, rather
// than merely expose a batched helper that the scorer never uses.
func TestSymbolFTSPrefixBatchCountsAddedGenerationsWithOneWalk(t *testing.T) {
	ctx := t.Context()
	store, _, _ := beginManifestGeneration(t)
	seedPrefixBatchDocuments(t, store, "base", "handler", 2)
	require.NoError(t, store.EnsureRowCounters(ctx))
	prefixes := []string{"hand", "ü", "é", "missing", ""}
	baseline, _, err := store.SymbolFTSScoringSnapshot(ctx, prefixes)
	require.NoError(t, err)
	for i, tc := range []struct {
		tokens string
		count  int
	}{
		{"über handler éclair", 3},
		{"über handler", 2},
		{"handler request", symbolFTSSmallGeneration + 1},
		{"handler", 4},
	} {
		label := fmt.Sprintf("batch-%d", i)
		generation, handle := beginPrefixBatchGeneration(t, store, label)
		seedPrefixBatchDocuments(t, handle, label, tc.tokens, tc.count)
		require.NoError(t, store.PublishPayloadGeneration(ctx, generation, 9001))
	}
	walks := 0
	symbolFTSPrefixWalkObserver = func() { walks++ }
	t.Cleanup(func() { symbolFTSPrefixWalkObserver = nil })
	stats, hits, err := store.SymbolFTSScoringSnapshot(ctx, prefixes)
	symbolFTSPrefixWalkObserver = nil
	require.NoError(t, err)
	require.Equal(t, len(prefixes)-1, walks, "one MATCH per nonempty prefix, not per fallback generation")
	require.Equal(t, baseline.Rows+int64(3+2+symbolFTSSmallGeneration+1+4), stats.Rows)
	require.Equal(t, int64(5), hits["ü"])
	require.Equal(t, int64(3), hits["é"])
	for _, prefix := range prefixes {
		var want int64
		if prefix != "" {
			require.NoError(t, store.db.QueryRowContext(ctx, `SELECT count(*) FROM symbol_fts WHERE symbol_fts MATCH ?`, ftsPrefixTerm(prefix)).Scan(&want))
		}
		require.Equal(t, want, hits[prefix], "prefix %q", prefix)
	}
	walks = 0
	symbolFTSPrefixWalkObserver = func() { walks++ }
	_, repeated, err := store.SymbolFTSScoringSnapshot(ctx, prefixes)
	symbolFTSPrefixWalkObserver = nil
	require.NoError(t, err)
	require.Equal(t, hits, repeated)
	require.Zero(t, walks, "the unchanged revision baseline still reuses exact counts")
}

func TestSymbolFTSPrefixBatchPreservesSparseOwnershipAndASCIIFastPath(t *testing.T) {
	ctx := t.Context()
	store, first, firstHandle := beginManifestGeneration(t)
	second, secondHandle := beginPrefixBatchGeneration(t, store, "second")
	small, smallHandle := beginPrefixBatchGeneration(t, store, "small")
	seedPrefixBatchDocuments(t, store, "before", "über handler éclair", 2)
	seedPrefixBatchDocuments(t, firstHandle, "first-a", "über handler éclair", 2)
	seedPrefixBatchDocuments(t, store, "inside", "über handler éclair", 3)
	seedPrefixBatchDocuments(t, secondHandle, "second", "über handler", 3)
	seedPrefixBatchDocuments(t, firstHandle, "first-b", "über handler éclair", 2)
	seedPrefixBatchDocuments(t, smallHandle, "ascii", "handler", 2)
	seedPrefixBatchDocuments(t, store, "after", "über handler éclair", 2)
	for _, generation := range []int64{first, second, small} {
		require.NoError(t, store.PublishPayloadGeneration(ctx, generation, 9001))
	}
	tx, err := store.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	require.NoError(t, err)
	defer tx.Rollback() //nolint:errcheck // read-only snapshot
	for _, prefix := range []string{"hand", "ü", "é", "missing"} {
		var want int64
		for _, generation := range []int64{first, second, small} {
			var n int64
			require.NoError(t, tx.QueryRowContext(ctx, `SELECT count(*) FROM symbol_fts
 CROSS JOIN symbol_fts_rowid m ON m.fts_rowid = symbol_fts.rowid AND m.view_gen = ?
 WHERE symbol_fts MATCH ?`, generation, ftsPrefixTerm(prefix)).Scan(&n))
			want += n
		}
		walks := 0
		symbolFTSPrefixWalkObserver = func() { walks++ }
		t.Cleanup(func() { symbolFTSPrefixWalkObserver = nil })
		got, err := store.generationsPrefixCountTx(ctx, tx, []int64{first, second, small, first, -123}, prefix, ftsPrefixTerm(prefix), map[int64][]SymbolFTSRow{})
		symbolFTSPrefixWalkObserver = nil
		require.NoError(t, err)
		require.Equal(t, want, got, "prefix %q excludes interleaved foreign documents and duplicate ownership requests", prefix)
		require.Equal(t, 1, walks)
	}
	walks := 0
	symbolFTSPrefixWalkObserver = func() { walks++ }
	got, err := store.generationsPrefixCountTx(ctx, tx, []int64{small, -123}, "hand", ftsPrefixTerm("hand"), map[int64][]SymbolFTSRow{})
	symbolFTSPrefixWalkObserver = nil
	require.NoError(t, err)
	require.Equal(t, int64(2), got)
	require.Zero(t, walks, "small ASCII generations retain the no-MATCH counter")
	got, err = store.generationsPrefixCountTx(ctx, tx, nil, "ü", ftsPrefixTerm("ü"), map[int64][]SymbolFTSRow{})
	require.NoError(t, err)
	require.Zero(t, got)
}

func TestSymbolFTSPrefixBatchPinsSnapshotAndCancels(t *testing.T) {
	ctx := t.Context()
	store, generation, handle := beginManifestGeneration(t)
	seedPrefixBatchDocuments(t, store, "base", "über", 2)
	seedPrefixBatchDocuments(t, handle, "generation", "über", 3)
	require.NoError(t, store.PublishPayloadGeneration(ctx, generation, 9001))
	tx, err := store.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	require.NoError(t, err)
	defer tx.Rollback() //nolint:errcheck // read-only snapshot
	_, err = symbolFTSStatsTx(ctx, tx)
	require.NoError(t, err)
	var next int64
	var writeErr error
	walks := 0
	symbolFTSPrefixWalkObserver = func() {
		walks++
		req := payloadRequest()
		req.LayerID, req.TreeOID, req.CreatedAt = "during-batch", "during-batch-tree", 10000
		var nextHandle *Store
		next, nextHandle, writeErr = store.BeginPayloadGeneration(ctx, req)
		if writeErr != nil {
			return
		}
		nodes, items := ftsDocs("über", 1, func(int) string { return "" })
		if writeErr = nextHandle.AddBatchChecked(nodes, nil); writeErr != nil {
			return
		}
		if writeErr = nextHandle.BatchUpsertSymbolFTS(items); writeErr != nil {
			return
		}
		writeErr = store.PublishPayloadGeneration(ctx, next, 10001)
	}
	t.Cleanup(func() { symbolFTSPrefixWalkObserver = nil })
	got, err := store.generationsPrefixCountTx(ctx, tx, []int64{0, generation}, "ü", ftsPrefixTerm("ü"), map[int64][]SymbolFTSRow{})
	symbolFTSPrefixWalkObserver = nil
	require.NoError(t, err)
	require.NoError(t, writeErr)
	require.Equal(t, 1, walks)
	require.Equal(t, int64(5), got)
	got, err = store.generationsPrefixCountTx(ctx, tx, []int64{next}, "ü", ftsPrefixTerm("ü"), map[int64][]SymbolFTSRow{})
	require.NoError(t, err)
	require.Zero(t, got, "the new publication is absent from the pinned transaction")
	require.NoError(t, tx.Rollback())

	after, err := store.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	require.NoError(t, err)
	defer after.Rollback() //nolint:errcheck // read-only snapshot
	got, err = store.generationsPrefixCountTx(ctx, after, []int64{0, generation, next}, "ü", ftsPrefixTerm("ü"), map[int64][]SymbolFTSRow{})
	require.NoError(t, err)
	require.Equal(t, int64(6), got)
	cancelled, cancel := context.WithCancel(ctx)
	defer cancel()
	symbolFTSPrefixWalkObserver = cancel
	got, err = store.generationsPrefixCountTx(cancelled, after, []int64{0, generation, next}, "ü", ftsPrefixTerm("ü"), map[int64][]SymbolFTSRow{})
	symbolFTSPrefixWalkObserver = nil
	require.ErrorIs(t, err, context.Canceled)
	require.Zero(t, got, "cancelled batched SQL must not return a partial count")
}
