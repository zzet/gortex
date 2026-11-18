package store_sqlite

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func TestSymbolFTSScoringSnapshotRemainsCoherentDuringMutation(t *testing.T) {
	for _, incremental := range []bool{false, true} {
		t.Run(map[bool]string{false: "unready", true: "incremental"}[incremental], func(t *testing.T) {
			store, _, _ := beginManifestGeneration(t)
			nodes, items := ftsDocs("base", 2, func(int) string { return "rare" })
			store.AddBatch(nodes, nil)
			require.NoError(t, store.BatchUpsertSymbolFTS(items))
			require.NoError(t, store.EnsureRowCounters(context.Background()))
			store.rowCountersReady.Store(incremental)
			before, err := store.SymbolFTSStats(context.Background())
			require.NoError(t, err)
			changed := false
			var writeErr error
			// Existing count observer runs AFTER the statistics established the read
			// snapshot, and before the actual prefix SQL. The independent WAL writer
			// changes rows/tokens; the returned weights and hits must remain old together.
			symbolFTSPrefixWalkObserver = func() {
				if changed {
					return
				}
				changed = true
				writeErr = store.BatchUpsertSymbolFTS([]graph.SymbolFTSItem{{NodeID: nodes[0].ID, Tokens: "changed many other tokens"}})
			}
			t.Cleanup(func() { symbolFTSPrefixWalkObserver = nil })
			stats, hits, err := store.SymbolFTSScoringSnapshot(context.Background(), []string{"rare", "changed", ""})
			symbolFTSPrefixWalkObserver = nil
			require.NoError(t, err)
			require.NoError(t, writeErr)
			require.True(t, changed)
			require.Equal(t, before, stats)
			require.Equal(t, int64(2), hits["rare"])
			require.Zero(t, hits["changed"])
			require.Zero(t, hits[""])
			current, err := store.SymbolFTSStats(context.Background())
			require.NoError(t, err)
			require.NotEqual(t, before.Stamp, current.Stamp)
			next, nextHits, err := store.SymbolFTSScoringSnapshot(context.Background(), []string{"rare", "changed", ""})
			require.NoError(t, err)
			require.Equal(t, current, next)
			require.Equal(t, int64(1), nextHits["rare"])
			require.Equal(t, int64(1), nextHits["changed"])
		})
	}
}

func TestSymbolFTSScoringSnapshotTracksGenerationChanges(t *testing.T) {
	ctx := context.Background()
	store, generation, handle := beginManifestGeneration(t)
	nodes, items := ftsDocs("base", 7, func(int) string { return "handler" })
	store.AddBatch(nodes, nil)
	require.NoError(t, store.BatchUpsertSymbolFTS(items))
	require.NoError(t, store.EnsureRowCounters(ctx))
	prefixes := []string{"hand", "handler", "base", "über", ""}
	exact := func() {
		t.Helper()
		stats, hits, err := store.SymbolFTSScoringSnapshot(ctx, prefixes)
		require.NoError(t, err)
		current, err := store.SymbolFTSStats(ctx)
		require.NoError(t, err)
		require.Equal(t, current, stats)
		require.Len(t, hits, len(prefixes))
		for _, prefix := range prefixes {
			if prefix == "" {
				require.Zero(t, hits[prefix])
				continue
			}
			var want int64
			require.NoError(t, store.db.QueryRow(`SELECT count(*) FROM symbol_fts WHERE symbol_fts MATCH ?`, ftsPrefixTerm(prefix)).Scan(&want))
			require.Equal(t, want, hits[prefix], "prefix %q", prefix)
		}
	}
	exact()
	genNodes, genItems := ftsDocs("über", 3, func(int) string { return "handle" })
	handle.AddBatch(genNodes, nil)
	require.NoError(t, handle.BatchUpsertSymbolFTS(genItems))
	require.NoError(t, store.PublishPayloadGeneration(ctx, generation, 8000))
	exact()
	require.NoError(t, store.BatchUpsertSymbolFTS([]graph.SymbolFTSItem{{NodeID: nodes[0].ID, Tokens: "storefront"}}))
	exact()
	require.NoError(t, store.RetirePayloadGeneration(ctx, generation, nil))
	exact()
	// A physical FTS document outside the ownership sidecar also invalidates the baseline.
	store.writeMu.Lock()
	_, err := store.writerDB.Exec(`INSERT INTO symbol_fts(rowid,node_id,repo_prefix,tokens) VALUES(999999,'orphan','','handler orphan')`)
	store.writeMu.Unlock()
	require.NoError(t, err)
	exact()
}

func TestSymbolFTSScoringSnapshotCancellationReturnsNoPartial(t *testing.T) {
	t.Run("held_memo_gate", func(t *testing.T) {
		store, _, _ := beginManifestGeneration(t)
		require.NoError(t, store.EnsureRowCounters(context.Background()))
		state := &symbolFTSPrefixState{terms: map[string]*symbolFTSPrefixBaseline{}}
		symbolFTSPrefixStates.Store(store.storeCore, state)
		state.mu.Lock()
		held := true
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		type result struct {
			stats SymbolFTSStats
			hits  map[string]int64
			err   error
		}
		done := make(chan result, 1)
		go func() {
			stats, hits, err := store.SymbolFTSScoringSnapshot(ctx, []string{"hand"})
			done <- result{stats, hits, err}
		}()
		t.Cleanup(func() {
			cancel()
			if held {
				state.mu.Unlock()
			}
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Error("snapshot waiter did not join")
			}
		})
		require.Eventually(t, func() bool { return state.mu.waiting() > 0 }, time.Second, time.Millisecond)
		cancel()
		select {
		case got := <-done:
			require.ErrorIs(t, got.err, context.Canceled)
			require.Zero(t, got.stats)
			require.Nil(t, got.hits)
			done <- got
		case <-time.After(time.Second):
			t.Fatal("cancelled snapshot waited for memo holder")
		}
		state.mu.Unlock()
		held = false
		_, _, err := store.SymbolFTSScoringSnapshot(context.Background(), []string{"hand"})
		require.NoError(t, err)
	})
	t.Run("partial_prefix", func(t *testing.T) {
		store, _, _ := beginManifestGeneration(t)
		nodes, items := ftsDocs("base", 2, func(int) string { return "handler" })
		store.AddBatch(nodes, nil)
		require.NoError(t, store.BatchUpsertSymbolFTS(items))
		require.NoError(t, store.EnsureRowCounters(context.Background()))
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		walks := 0
		symbolFTSPrefixWalkObserver = func() {
			walks++
			if walks == 2 {
				cancel()
			}
		}
		t.Cleanup(func() { symbolFTSPrefixWalkObserver = nil })
		stats, hits, err := store.SymbolFTSScoringSnapshot(ctx, []string{"hand", "base"})
		symbolFTSPrefixWalkObserver = nil
		require.ErrorIs(t, err, context.Canceled)
		require.Zero(t, stats)
		require.Nil(t, hits)
		require.Equal(t, 2, walks)
		_, hits, err = store.SymbolFTSScoringSnapshot(context.Background(), []string{"hand", "base"})
		require.NoError(t, err)
		require.Equal(t, int64(2), hits["hand"])
		require.Equal(t, int64(2), hits["base"])
	})
}
func TestSymbolFTSScoringSnapshotEmptyAndCancelled(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "empty.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	stats, hits, err := store.SymbolFTSScoringSnapshot(nil, []string{"hand", ""}) //nolint:staticcheck // Exercise the API's documented nil-context normalization.
	require.NoError(t, err)
	require.Zero(t, stats.Rows)
	require.Equal(t, map[string]int64{"hand": 0, "": 0}, hits)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	stats, hits, err = store.SymbolFTSScoringSnapshot(ctx, []string{"hand"})
	require.True(t, errors.Is(err, context.Canceled))
	require.Zero(t, stats)
	require.Nil(t, hits)
}
