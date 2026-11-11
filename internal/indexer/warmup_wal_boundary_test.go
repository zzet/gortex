package indexer

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
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

// Only the warmup WAL refusal skips the per-file retry pass
// (TestReconcileRefusedByWarmupWALConvergesOnRetry). Any other
// contract-restatement error keeps it, and a pass whose first chunk already
// re-parsed files still forces the deferred enrich past its completion marker.
func TestContractRestatementErrorKeepsRetryPassAndReparseFlag(t *testing.T) {
	dir := t.TempDir()
	paths := make([]string, incrementalBatchFiles+8) // two chunks
	for i := range paths {
		paths[i] = filepath.Join(dir, fmt.Sprintf("f%03d.go", i))
		writeFile(t, paths[i], fmt.Sprintf("package main\nfunc F%d() {}\n", i))
	}
	idx := newTestIndexer(graph.New())
	restatementErr := errors.New("contract restatement failed")
	calls := 0
	idx.warmupWriteBoundary = func() error {
		calls++
		if calls == 2 {
			return restatementErr
		}
		return nil
	}
	_, err := idx.IncrementalReindexPaths(dir, paths)
	require.ErrorIs(t, err, restatementErr)
	assert.Equal(t, 3, calls, "the per-file retry pass must still run")
	assert.True(t, idx.reparsedThisRun.Load(),
		"the first chunk re-parsed files, so reparsedThisRun must be raised")
}
