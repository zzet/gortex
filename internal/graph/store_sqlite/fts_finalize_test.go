package store_sqlite

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

func TestBuildSymbolIndexBoundedMerge(t *testing.T) {
	assertLiveFTSFinalize(t, "symbol_fts", (*Store).BuildSymbolIndex)
}

func TestBuildContentIndexBoundedMerge(t *testing.T) {
	assertLiveFTSFinalize(t, "content_fts", (*Store).BuildContentIndex)
}

func assertLiveFTSFinalize(t *testing.T, table string, build func(*Store) error) {
	t.Helper()
	for _, mode := range []string{"live", "view", "coordinated", "error"} {
		t.Run(mode, func(t *testing.T) {
			store, err := Open(filepath.Join(t.TempDir(), "fts.sqlite"))
			require.NoError(t, err)
			t.Cleanup(func() {
				store.ftsMaintenanceObserver, store.bulkFinalizeObserver = nil, nil
				require.NoError(t, store.Close())
			})
			target := store
			switch mode {
			case "view":
				target = store.AtGeneration(1)
			case "coordinated":
				require.True(t, store.BeginCoordinatedBulkLoad())
				defer func() {
					store.ftsMaintenanceObserver, store.bulkFinalizeObserver = nil, nil
					require.NoError(t, store.EndCoordinatedBulkLoad())
				}()
			case "error":
				_, err = store.writerDB.Exec("DROP TABLE " + table)
				require.NoError(t, err)
			}
			var statements []string
			var arguments [][]any
			var events []bulkFinalizeEvent
			store.ftsMaintenanceObserver = func(query string, args []any) {
				statements = append(statements, query)
				arguments = append(arguments, args)
			}
			store.bulkFinalizeObserver = func(event bulkFinalizeEvent) {
				if event.Stage != "fts_merge" {
					return
				}
				if !store.writeMu.TryLock() {
					t.Error("live merge event/observer extends writer hold")
					return
				}
				store.writeMu.Unlock()
				events = append(events, event)
			}
			require.NoError(t, build(target), "FTS maintenance preserves best-effort build semantics")
			if mode == "view" || mode == "coordinated" {
				require.Empty(t, statements)
				require.Empty(t, events)
				if mode == "coordinated" {
					require.True(t, map[string]bool{"symbol_fts": store.deferredFTSOptimize, "content_fts": store.deferredContentFTS}[table])
				}
				return
			}
			require.Equal(t, []string{fmt.Sprintf("INSERT INTO %s(%s, rank) VALUES('merge', ?)", table, table)}, statements)
			require.Equal(t, [][]any{{coldFTSMergePages}}, arguments)
			require.NotContains(t, statements[0], "optimize")
			require.Len(t, events, 1)
			require.Equal(t, table, events[0].Name)
			require.GreaterOrEqual(t, events[0].Elapsed.Nanoseconds(), int64(0))
			if mode == "error" {
				require.Error(t, events[0].Err)
			} else {
				require.NoError(t, events[0].Err)
			}
		})
	}
}

func TestFTSSegmentCountsTrackSeparateBatchesAndBoundedMerge(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "segments.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	// Disable automatic merging in this fixture only, so separate committed
	// insert batches prove the reader is observing real distinct segments.
	for _, table := range []string{"symbol_fts", "content_fts"} {
		_, err = store.writerDB.Exec(fmt.Sprintf("INSERT INTO %s(%s, rank) VALUES('automerge', 0)", table, table))
		require.NoError(t, err)
	}
	for i := range 8 {
		id := fmt.Sprintf("repo/f%d.go::Item%d", i, i)
		store.AddBatch([]*graph.Node{{ID: id, Kind: graph.KindFunction, Name: fmt.Sprintf("Item%d", i), FilePath: fmt.Sprintf("repo/f%d.go", i), RepoPrefix: "repo"}}, nil)
		require.NoError(t, store.BatchUpsertSymbolFTS([]graph.SymbolFTSItem{{NodeID: id, Tokens: fmt.Sprintf("segmenttoken symbol%d", i)}}))
		require.NoError(t, store.AppendContent("repo", []graph.ContentFTSItem{{NodeID: fmt.Sprintf("doc%d", i), FilePath: fmt.Sprintf("doc%d.txt", i), Body: fmt.Sprintf("segmenttoken content%d", i)}}))
	}
	before, err := store.FTSSegments(t.Context())
	require.NoError(t, err)
	require.Greater(t, before.Symbols, int64(1))
	require.Greater(t, before.Content, int64(1))
	viewCounts, err := store.AtGeneration(1).FTSSegments(t.Context())
	require.NoError(t, err)
	require.Equal(t, before, viewCounts, "segment sidecars describe physical tables, not view-local rows")
	require.NoError(t, store.BuildSymbolIndex())
	require.NoError(t, store.BuildContentIndex())
	after, err := store.FTSSegments(t.Context())
	require.NoError(t, err)
	require.LessOrEqual(t, after.Symbols, before.Symbols)
	require.LessOrEqual(t, after.Content, before.Content)
	symbols, err := store.SearchSymbols("segmenttoken", 100)
	require.NoError(t, err)
	require.Len(t, symbols, 8)
	content, err := store.SearchContent("segmenttoken", "repo", 100)
	require.NoError(t, err)
	require.Len(t, content, 8)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	counts, err := store.FTSSegments(ctx)
	require.ErrorIs(t, err, context.Canceled)
	require.Equal(t, FTSSegmentCounts{}, counts, "a failed observation cannot be mistaken for an empty index")
}
