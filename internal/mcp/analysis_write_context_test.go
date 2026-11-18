package mcp

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

func TestAnalysisAbortUsesFreshSameViewCleanupContext(t *testing.T) {
	store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "abort.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	selected := store.AtGeneration(19).ViewScopedAnalysis()
	id, accepted, err := selected.BeginAnalysisGeneration(selected.AnalysisMutationRevision(), graph.AnalysisGenerationHeader{FormatVersion: 1})
	require.NoError(t, err)
	require.True(t, accepted)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// Rebinding must replace the canceled operation context, not reset the
	// selected view or inherit cancellation into the cleanup attempt.
	canceledWriter := bindAnalysisWriteContext(selected, ctx)
	require.NoError(t, abortAnalysisGeneration(ctx, canceledWriter, id))
	accepted, err = selected.ActivateAnalysisGeneration(selected.AnalysisMutationRevision(), id)
	require.NoError(t, err)
	require.False(t, accepted, "abort did not actually move the building generation out of building state")
	_, found, err := selected.LoadActiveAnalysisHeader(1)
	require.NoError(t, err)
	require.False(t, found)
}

func TestAnalysisAbortHeldGateRetainsCleanupDeadlineWithoutReleasingHolder(t *testing.T) {
	store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "held-abort.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	id, accepted, err := store.BeginAnalysisGeneration(store.AnalysisMutationRevision(), graph.AnalysisGenerationHeader{FormatVersion: 1})
	require.NoError(t, err)
	require.True(t, accepted)
	release, err := store.HoldWriteGate(context.Background())
	require.NoError(t, err)
	var once sync.Once
	unlock := func() { once.Do(release) }
	t.Cleanup(unlock)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan error, 1)
	go func() { done <- abortAnalysisGeneration(ctx, store, id) }()
	select {
	case err = <-done:
		require.ErrorIs(t, err, context.DeadlineExceeded)
	case <-time.After(5 * time.Second):
		t.Fatal("cleanup gate admission ignored its own budget")
	}
	probeCtx, stopProbe := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer stopProbe()
	extraRelease, probeErr := store.HoldWriteGate(probeCtx)
	if extraRelease != nil {
		extraRelease()
	}
	require.ErrorIs(t, probeErr, context.DeadlineExceeded, "cleanup deadline released another owner's gate")
	unlock()
	require.NoError(t, store.AbortAnalysisGeneration(id), "deadline did not release/finish the other holder's work")
	_, found, err := store.LoadActiveAnalysisHeader(1)
	require.NoError(t, err)
	require.False(t, found)
}

func TestAnalysisWriteContextBindingPreservesDecoratedWriter(t *testing.T) {
	store, _ := buildAnalysisCacheTestGraph(t, 2)
	t.Cleanup(func() { _ = store.Close() })
	wrapped := &mutateBeforeAnalysisCommitStore{
		countingAnalysisCacheStore: newCountingAnalysisCacheStore(store),
		node:                       &graph.Node{ID: "binding-mutation", Kind: graph.KindFunction, Name: "Mutation"},
	}
	bound := bindAnalysisWriteContext(wrapped, context.Background())
	require.Same(t, wrapped, bound, "promoted binding must not erase wrapper admission/publication semantics")
	revision := store.AnalysisMutationRevision()
	installed := false
	require.False(t, bound.CommitAnalysisSnapshot(revision, func() { installed = true }))
	require.False(t, installed)
	require.NotNil(t, store.GetNode("binding-mutation"))
}
