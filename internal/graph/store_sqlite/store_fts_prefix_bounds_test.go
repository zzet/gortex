package store_sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

// Both reasons for declining the small ASCII counter retain FTS5's exact
// tokenization and the ownership join, even when foreign rows lie inside the
// generation's rowid bounds. These are count oracles, not latency assertions.
func TestSymbolFTSGenerationPrefixFallbackPreservesOwnershipAndUnicode(t *testing.T) {
	for _, tc := range []struct {
		name, tokens string
		rows         int
	}{
		{"unicode_sparse", "über handler éclair", 6},
		{"large_ascii", "handle request", symbolFTSSmallGeneration + 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			store, generation, handle := beginManifestGeneration(t)
			seed := func(target *Store, label string, count int) {
				nodes, items := ftsDocs(tc.tokens, count, func(int) string { return "" })
				for i := range nodes {
					nodes[i].ID = fmt.Sprintf("%s-%s-%d", nodes[i].ID, label, i)
					items[i].NodeID = nodes[i].ID
				}
				require.NoError(t, target.AddBatchChecked(nodes, nil))
				require.NoError(t, target.BatchUpsertSymbolFTS(items))
			}
			seed(store, "before", 2)
			seed(handle, "first", tc.rows/2)
			seed(store, "inside", 3)
			seed(handle, "last", tc.rows-tc.rows/2)
			seed(store, "after", 2)
			require.NoError(t, store.PublishPayloadGeneration(ctx, generation, 8000))
			st, err := store.SymbolFTSGenerationStats(ctx, generation)
			require.NoError(t, err)
			require.Equal(t, int64(tc.rows), st.Rows)
			require.Greater(t, st.Hi-st.Lo+1, st.Rows, "foreign rows must be interleaved")

			tx, err := store.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
			require.NoError(t, err)
			defer tx.Rollback() //nolint:errcheck // read-only snapshot
			cache := map[int64][]SymbolFTSRow{}
			for _, g := range []int64{generation, 0, -123} {
				for _, prefix := range []string{"hand", "req", "ü", "é", "missing"} {
					term := ftsPrefixTerm(prefix)
					var want int64
					require.NoError(t, tx.QueryRowContext(ctx, `SELECT count(*) FROM symbol_fts
  CROSS JOIN symbol_fts_rowid m ON m.fts_rowid = symbol_fts.rowid AND m.view_gen = ?
 WHERE symbol_fts MATCH ?`, g, term).Scan(&want))
					got, err := store.generationPrefixCountTx(ctx, tx, g, strings.ToLower(prefix), term, cache)
					require.NoError(t, err)
					require.Equal(t, want, got, "generation %d prefix %q", g, prefix)
					if g == generation && prefix == "hand" {
						require.Equal(t, int64(tc.rows), got)
					}
				}
			}
		})
	}
}

// A writer can publish a newer generation and change mutable base rows after
// the prefix bounds are read. The original transaction must keep its counts;
// the next transaction must observe both changes, without reusing old bounds.
func TestSymbolFTSGenerationPrefixFallbackPinsSnapshot(t *testing.T) {
	ctx := t.Context()
	store, generation, handle := beginManifestGeneration(t)
	baseNodes, baseItems := ftsDocs("über handler", 2, func(int) string { return "" })
	require.NoError(t, store.AddBatchChecked(baseNodes, nil))
	require.NoError(t, store.BatchUpsertSymbolFTS(baseItems))
	genNodes, genItems := ftsDocs("über handler", 1, func(int) string { return "" })
	require.NoError(t, handle.AddBatchChecked(genNodes, nil))
	require.NoError(t, handle.BatchUpsertSymbolFTS(genItems))
	require.NoError(t, store.PublishPayloadGeneration(ctx, generation, 8000))

	tx, err := store.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	require.NoError(t, err)
	defer tx.Rollback()                //nolint:errcheck // read-only snapshot
	_, err = symbolFTSStatsTx(ctx, tx) // establish the original scoring snapshot
	require.NoError(t, err)
	changed := false
	var writeErr error
	var next int64
	symbolFTSPrefixWalkObserver = func() {
		if changed {
			return
		}
		changed = true
		writeErr = store.BatchUpsertSymbolFTS([]graph.SymbolFTSItem{{NodeID: baseNodes[0].ID, Tokens: "changed"}})
		if writeErr != nil {
			return
		}
		req := payloadRequest()
		req.LayerID, req.TreeOID, req.CreatedAt = "prefix-next", "prefix-next-tree", 9000
		var nextHandle *Store
		next, nextHandle, writeErr = store.BeginPayloadGeneration(ctx, req)
		if writeErr != nil {
			return
		}
		if writeErr = nextHandle.AddBatchChecked(genNodes, nil); writeErr != nil {
			return
		}
		if writeErr = nextHandle.BatchUpsertSymbolFTS(genItems); writeErr != nil {
			return
		}
		writeErr = store.PublishPayloadGeneration(ctx, next, 9001)
	}
	t.Cleanup(func() { symbolFTSPrefixWalkObserver = nil })
	cache := map[int64][]SymbolFTSRow{}
	got, err := store.generationPrefixCountTx(ctx, tx, 0, "ü", ftsPrefixTerm("ü"), cache)
	symbolFTSPrefixWalkObserver = nil
	require.NoError(t, err)
	require.True(t, changed, "the non-ASCII count must use FTS5")
	require.NoError(t, writeErr)
	require.Equal(t, int64(2), got)
	got, err = store.generationPrefixCountTx(ctx, tx, next, "ü", ftsPrefixTerm("ü"), cache)
	require.NoError(t, err)
	require.Zero(t, got, "new publication must be absent from the old transaction")
	require.NoError(t, tx.Rollback())

	after, err := store.db.BeginTx(context.Background(), &sql.TxOptions{ReadOnly: true})
	require.NoError(t, err)
	defer after.Rollback() //nolint:errcheck // read-only snapshot
	for generation, want := range map[int64]int64{0: 1, generation: 1, next: 1} {
		got, err := store.generationPrefixCountTx(ctx, after, generation, "ü", ftsPrefixTerm("ü"), map[int64][]SymbolFTSRow{})
		require.NoError(t, err)
		require.Equal(t, want, got, "generation %d", generation)
	}
}
