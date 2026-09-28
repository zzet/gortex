package store_sqlite

import (
	"context"
	"fmt"
	"os"
	"sort"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

func readGenerationRows(t *testing.T, s *Store, generation int64) []SymbolFTSRow {
	t.Helper()
	var out []SymbolFTSRow
	var after int64
	for {
		page, next, err := s.SymbolFTSGenerationRows(context.Background(), generation, "", after, 1000)
		require.NoError(t, err)
		out = append(out, page...)
		if next == 0 {
			return out
		}
		after = next
	}
}

func withoutTokenCounts(rows []SymbolFTSRow) []SymbolFTSRow {
	out := append([]SymbolFTSRow(nil), rows...)
	for i := range out {
		out[i].TokenCount = 0
	}
	sort.Slice(out, func(i, j int) bool { return out[i].RowID < out[j].RowID })
	return out
}

// The rows a generation's build writes through the batch path are handed over
// once, exactly as the store holds them (rowid, node, repository, tokens), with
// deletes applied; the base corpus is never kept; a second take finds nothing.
func TestFreshSymbolFTSRowsAreTheGenerationsOwn(t *testing.T) {
	ctx := context.Background()
	store, generationID, handle := beginManifestGeneration(t)
	nodes, items := ftsDocs("fresh", 30, func(i int) string { return fmt.Sprintf("handler route%d", i) })
	handle.AddBatch(nodes, nil)
	require.NoError(t, handle.BatchUpsertSymbolFTS(items[:20]))
	require.NoError(t, handle.BatchUpsertSymbolFTS(items[10:])) // 10 rewritten, 10 new
	require.NoError(t, handle.BatchDeleteSymbolFTS([]string{items[3].NodeID, items[25].NodeID}))
	baseNodes, baseItems := ftsDocs("base", 5, func(int) string { return "handler" })
	for i := range baseNodes {
		baseNodes[i].ID += "-base"
		baseItems[i].NodeID = baseNodes[i].ID
	}
	store.AddBatch(baseNodes, nil)
	require.NoError(t, store.BatchUpsertSymbolFTS(baseItems))
	require.NoError(t, store.PublishPayloadGeneration(ctx, generationID, 8000))

	rows, complete := store.TakeFreshSymbolFTSRows(generationID)
	require.True(t, complete)
	want := withoutTokenCounts(readGenerationRows(t, store, generationID))
	require.Len(t, want, 28)
	require.Equal(t, want, rows)
	st, err := store.SymbolFTSGenerationStats(ctx, generationID)
	require.NoError(t, err)
	require.Equal(t, st.Rows, int64(len(rows)))
	require.Equal(t, [2]int64{st.Lo, st.Hi}, [2]int64{rows[0].RowID, rows[len(rows)-1].RowID})

	again, complete := store.TakeFreshSymbolFTSRows(generationID)
	require.False(t, complete)
	require.Empty(t, again)
	base, complete := store.TakeFreshSymbolFTSRows(0)
	require.False(t, complete, "the base corpus is never kept")
	require.Empty(t, base)
}

// A generation another writer touched, or one past the row bound, is handed
// over incomplete; past the byte bound the generation that began first is
// dropped.
func TestFreshSymbolFTSRowsBounds(t *testing.T) {
	t.Run("another writer", func(t *testing.T) {
		store, generationID, handle := beginManifestGeneration(t)
		_, items := ftsDocs("w", 4, func(int) string { return "handler" })
		require.NoError(t, handle.BatchUpsertSymbolFTS(items))
		require.NoError(t, handle.ResetSymbolFTS(payloadRepo))
		require.NoError(t, handle.BatchUpsertSymbolFTS(items))
		_, complete := store.TakeFreshSymbolFTSRows(generationID)
		require.False(t, complete)
	})
	t.Run("row bound", func(t *testing.T) {
		previous := freshFTSMaxRowsPerGeneration
		freshFTSMaxRowsPerGeneration = 10
		t.Cleanup(func() { freshFTSMaxRowsPerGeneration = previous })
		store, generationID, handle := beginManifestGeneration(t)
		_, items := ftsDocs("r", 11, func(int) string { return "handler" })
		require.NoError(t, handle.BatchUpsertSymbolFTS(items))
		rows, complete := store.TakeFreshSymbolFTSRows(generationID)
		require.False(t, complete)
		require.Empty(t, rows)
	})
	t.Run("byte bound", func(t *testing.T) {
		store, generationID, handle := beginManifestGeneration(t)
		_, items := ftsDocs("b", 4, func(int) string { return "handler" })
		require.NoError(t, handle.BatchUpsertSymbolFTS(items))
		b := store.freshFTSBuffers()
		b.mu.Lock()
		kept := b.bytes
		b.mu.Unlock()
		previous := freshFTSMaxBytes
		freshFTSMaxBytes = kept + kept/2
		t.Cleanup(func() { freshFTSMaxBytes = previous })
		// A later generation's rows push the first one out.
		later := store.AtGeneration(generationID + 1000)
		later.recordFreshFTS(generationID+1000, []SymbolFTSRow{{RowID: 1 << 40, NodeID: "x", Tokens: string(make([]byte, kept))}})
		_, complete := store.TakeFreshSymbolFTSRows(generationID)
		require.False(t, complete, "the generation that began first was not dropped at the byte bound")
		rows, complete := store.TakeFreshSymbolFTSRows(generationID + 1000)
		require.True(t, complete)
		require.Len(t, rows, 1)
	})
}

// Rows written per second through the batch path with and without the
// fresh-rows buffer. Set A1_FRESH_RATE=1 to run it (it is a measurement, not a
// check): edit-sized batches of 400 documents, 20 per round, alternating the
// buffer off and on for 10 rounds on one store.
func TestFreshSymbolFTSRowsWriteRate(t *testing.T) {
	if os.Getenv("A1_FRESH_RATE") != "1" {
		t.Skip("set A1_FRESH_RATE=1")
	}
	store, generationID, handle := beginManifestGeneration(t)
	_ = store
	const batch, batches, rounds = 400, 20, 10
	var off, on []float64
	seq := 0
	run := func(buffered bool) float64 {
		freshFTSOff.Store(!buffered)
		started := time.Now()
		for i := 0; i < batches; i++ {
			items := make([]graph.SymbolFTSItem, batch)
			for j := range items {
				seq++
				items[j] = graph.SymbolFTSItem{NodeID: fmt.Sprintf("%s/rate%07d.go::R%07d", payloadRepo, seq, seq), Tokens: fmt.Sprintf("rate%d handler checkout coordinator route flip", seq)}
			}
			require.NoError(t, handle.BatchUpsertSymbolFTS(items))
		}
		elapsed := time.Since(started)
		// Keep the buffer from reaching its bound across rounds.
		store.TakeFreshSymbolFTSRows(generationID)
		return float64(batch*batches) / elapsed.Seconds()
	}
	t.Cleanup(func() { freshFTSOff.Store(false) })
	for r := 0; r < rounds; r++ {
		off = append(off, run(false))
		on = append(on, run(true))
	}
	sort.Float64s(off)
	sort.Float64s(on)
	t.Logf("rows/s, median of %d rounds of %d x %d: without buffer %.0f (min %.0f max %.0f), with buffer %.0f (min %.0f max %.0f), ratio %.3f",
		rounds, batches, batch, off[rounds/2], off[0], off[rounds-1], on[rounds/2], on[0], on[rounds-1], on[rounds/2]/off[rounds/2])
}

// Closing a store drops its fresh-rows buffers.
func TestFreshSymbolFTSRowsAreDroppedWithTheStore(t *testing.T) {
	store, _, handle := beginManifestGeneration(t)
	_, items := ftsDocs("close", 3, func(int) string { return "handler" })
	require.NoError(t, handle.BatchUpsertSymbolFTS(items))
	core := store.storeCore
	_, ok := freshFTS.Load(core)
	require.True(t, ok, "precondition: rows were buffered")
	require.NoError(t, store.Close())
	_, ok = freshFTS.Load(core)
	require.False(t, ok, "a closed store's fresh rows are still held")
}
