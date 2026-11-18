package store_sqlite

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// This public-path discriminator also compiles on the pre-stream parent.
// Exact counts, rather than an elapsed-time bound, establish the behavior.
func TestSymbolFTSAddedASCIIStreamCountsQueryPrefixesExactly(t *testing.T) {
	ctx := t.Context()
	store, _, _ := beginManifestGeneration(t)
	seedPrefixBatchDocuments(t, store, "stream-base", "handler request", 2)
	require.NoError(t, store.EnsureRowCounters(ctx))
	prefixes := []string{"hand", "req", "missing", "ha", ""}
	baseline, _, err := store.SymbolFTSScoringSnapshot(ctx, prefixes)
	require.NoError(t, err)
	generation, handle := beginPrefixBatchGeneration(t, store, "stream-added")
	seedPrefixBatchDocuments(t, handle, "stream-added-a", "Handler handler request handle_request", 2500)
	foreign, foreignHandle := beginPrefixBatchGeneration(t, store, "stream-interleaved")
	seedPrefixBatchDocuments(t, foreignHandle, "stream-interleaved", "handler request", 3)
	seedPrefixBatchDocuments(t, handle, "stream-added-b", "Handler handler request handle_request", symbolFTSSmallGeneration+1-2500)
	require.NoError(t, store.PublishPayloadGeneration(ctx, generation, 9001))
	require.NoError(t, store.PublishPayloadGeneration(ctx, foreign, 9001))
	walks := 0
	symbolFTSPrefixWalkObserver = func() { walks++ }
	t.Cleanup(func() { symbolFTSPrefixWalkObserver = nil })
	stats, hits, err := store.SymbolFTSScoringSnapshot(ctx, prefixes)
	symbolFTSPrefixWalkObserver = nil
	require.NoError(t, err)
	require.Equal(t, baseline.Rows+int64(symbolFTSSmallGeneration+4), stats.Rows)
	require.Zero(t, walks, "multiple added-only ASCII prefixes use a complete stream rather than MATCH")
	for _, prefix := range prefixes {
		var want int64
		if prefix != "" {
			require.NoError(t, store.db.QueryRowContext(ctx, `SELECT count(*) FROM symbol_fts WHERE symbol_fts MATCH ?`, ftsPrefixTerm(prefix)).Scan(&want))
		}
		require.Equal(t, want, hits[prefix], "prefix %q counts each matching document once", prefix)
	}
	walks = 0
	symbolFTSPrefixWalkObserver = func() { walks++ }
	_, repeated, err := store.SymbolFTSScoringSnapshot(ctx, prefixes)
	symbolFTSPrefixWalkObserver = nil
	require.NoError(t, err)
	require.Equal(t, hits, repeated)
	require.Zero(t, walks, "the ordinary unchanged baseline remains reusable")
}

func TestSymbolFTSAddedASCIIStreamFallsBackForLateUnicode(t *testing.T) {
	ctx := t.Context()
	store, _, _ := beginManifestGeneration(t)
	seedPrefixBatchDocuments(t, store, "unicode-base", "handler request", 2)
	require.NoError(t, store.EnsureRowCounters(ctx))
	prefixes := []string{"hand", "req"}
	_, _, err := store.SymbolFTSScoringSnapshot(ctx, prefixes)
	require.NoError(t, err)
	generation, handle := beginPrefixBatchGeneration(t, store, "unicode-added")
	seedPrefixBatchDocuments(t, handle, "unicode-added-ascii", "handler request", symbolFTSSmallGeneration)
	foreign, foreignHandle := beginPrefixBatchGeneration(t, store, "unicode-interleaved")
	seedPrefixBatchDocuments(t, foreignHandle, "unicode-interleaved", "handler request", 3)
	seedPrefixBatchDocuments(t, handle, "unicode-added-last", "über handler request", 1)
	for _, g := range []int64{generation, foreign} {
		require.NoError(t, store.PublishPayloadGeneration(ctx, g, 9001))
	}
	walks := 0
	symbolFTSPrefixWalkObserver = func() { walks++ }
	t.Cleanup(func() { symbolFTSPrefixWalkObserver = nil })
	_, hits, err := store.SymbolFTSScoringSnapshot(ctx, prefixes)
	symbolFTSPrefixWalkObserver = nil
	require.NoError(t, err)
	require.Equal(t, 2, walks, "a late Unicode row discards the entire generation's ASCII stream and retains MATCH")
	for _, prefix := range prefixes {
		var want int64
		require.NoError(t, store.db.QueryRowContext(ctx, `SELECT count(*) FROM symbol_fts WHERE symbol_fts MATCH ?`, ftsPrefixTerm(prefix)).Scan(&want))
		require.Equal(t, want, hits[prefix], "prefix %q must not retain partial ASCII contributions", prefix)
	}
}

func TestSymbolFTSAddedASCIIStreamHonorsDifferentTermBaselines(t *testing.T) {
	ctx := t.Context()
	store, _, _ := beginManifestGeneration(t)
	seedPrefixBatchDocuments(t, store, "different-base", "handler request", 2)
	require.NoError(t, store.EnsureRowCounters(ctx))
	_, _, err := store.SymbolFTSScoringSnapshot(ctx, []string{"hand", "req"})
	require.NoError(t, err)
	first, firstHandle := beginPrefixBatchGeneration(t, store, "different-first")
	seedPrefixBatchDocuments(t, firstHandle, "different-first", "handler", symbolFTSSmallGeneration+1)
	require.NoError(t, store.PublishPayloadGeneration(ctx, first, 9001))
	_, _, err = store.SymbolFTSScoringSnapshot(ctx, []string{"hand"})
	require.NoError(t, err)
	second, secondHandle := beginPrefixBatchGeneration(t, store, "different-second")
	seedPrefixBatchDocuments(t, secondHandle, "different-second", "request", symbolFTSSmallGeneration+1)
	require.NoError(t, store.PublishPayloadGeneration(ctx, second, 9001))
	walks := 0
	symbolFTSPrefixWalkObserver = func() { walks++ }
	t.Cleanup(func() { symbolFTSPrefixWalkObserver = nil })
	_, hits, err := store.SymbolFTSScoringSnapshot(ctx, []string{"hand", "req"})
	symbolFTSPrefixWalkObserver = nil
	require.NoError(t, err)
	require.Equal(t, 1, walks, "the first generation needs only req; only the second qualifies for a shared stream")
	for _, prefix := range []string{"hand", "req"} {
		var want int64
		require.NoError(t, store.db.QueryRowContext(ctx, `SELECT count(*) FROM symbol_fts WHERE symbol_fts MATCH ?`, ftsPrefixTerm(prefix)).Scan(&want))
		require.Equal(t, want, hits[prefix], "prefix %q uses its own added-generation set", prefix)
	}
}

func TestSymbolFTSAddedASCIIStreamLeavesSinglePrefixOnMatch(t *testing.T) {
	ctx := t.Context()
	store, _, _ := beginManifestGeneration(t)
	seedPrefixBatchDocuments(t, store, "single-base", "handler", 2)
	require.NoError(t, store.EnsureRowCounters(ctx))
	_, _, err := store.SymbolFTSScoringSnapshot(ctx, []string{"hand"})
	require.NoError(t, err)
	generation, handle := beginPrefixBatchGeneration(t, store, "single-added")
	seedPrefixBatchDocuments(t, handle, "single-added", "handler", symbolFTSSmallGeneration+1)
	require.NoError(t, store.PublishPayloadGeneration(ctx, generation, 9001))
	walks := 0
	symbolFTSPrefixWalkObserver = func() { walks++ }
	t.Cleanup(func() { symbolFTSPrefixWalkObserver = nil })
	_, hits, err := store.SymbolFTSScoringSnapshot(ctx, []string{"hand", " hand "})
	symbolFTSPrefixWalkObserver = nil
	require.NoError(t, err)
	require.Equal(t, 1, walks, "equivalent prefixes do not manufacture a second eligible term")
	require.Equal(t, int64(symbolFTSSmallGeneration+3), hits["hand"])
	require.Equal(t, hits["hand"], hits[" hand "])
}
