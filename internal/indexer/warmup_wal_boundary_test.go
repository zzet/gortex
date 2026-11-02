package indexer

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

func TestWarmupWALBoundaryRefusesBeforeChunkReadsOrWrites(t *testing.T) {
	counting := &incrementalBatchCountingStore{Store: graph.New()}
	idx := newTestIndexer(counting)
	calls := 0
	idx.warmupWriteBoundary = func() error {
		calls++
		return context.Canceled
	}
	_, reparsed, failed, _ := idx.reindexIncrementalStalePass(
		[]string{"must-not-read.go"}, &reparsePendingEnrichmentBatch{}, false,
	)
	require.Equal(t, 1, calls)
	require.ErrorIs(t, idx.contractRestatementErr, context.Canceled)
	require.Empty(t, reparsed)
	require.Equal(t, []string{"must-not-read.go"}, failed)
	require.Zero(t, counting.getFileNodesByPaths.Load())
	require.Zero(t, counting.addBatch.Load())
}
