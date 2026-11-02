package mcp

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"go.uber.org/zap"
)

// This is the composed set path, distinct from the retained two-point MOVE
// control. It uses real physical commits, one real set admission, the actual
// parser/executor, and the existing status renderer. It claims no strict P/B.
func TestMutationSetPhysicalOwnersReachActualSharedExecutorStatus(t *testing.T) {
	extractor := &movePhysicalStatusExtractor{Extractor: languages.NewGoExtractor(), entered: make(chan struct{}, 16), release: make(chan struct{})}
	registry := parser.NewRegistry()
	registry.Register(extractor)
	s, root := setupTestServerWithRegistry(t, registry)
	s.editAckMode = config.EditAckLocal
	paths := []string{filepath.Join(root, "source.go"), filepath.Join(root, "target.go")}
	for i, path := range paths {
		content := []string{"package main\nfunc Before() {}\n", "package main\nfunc OldTarget() {}\n"}[i]
		require.NoError(t, os.WriteFile(path, []byte(content), 0600))
	}
	_, err := s.indexer.IncrementalReindexPaths(root, paths)
	require.NoError(t, err)
	watcher, err := indexer.NewWatcher(s.indexer, config.WatchConfig{Enabled: true, DebounceMs: 5}, zap.NewNop())
	require.NoError(t, err)
	require.NoError(t, watcher.Start([]string{t.TempDir()})) // idle native frontend; explicit set owns the source paths
	var release sync.Once
	t.Cleanup(func() {
		release.Do(func() { close(extractor.release) })
		stopErr := watcher.Stop()
		s.DrainBackground()
		s.indexer.Close()
		require.NoError(t, stopErr)
	})
	s.SetWatcher(watcher)
	ctx, ack, _ := s.withEditAcknowledgement(canonicalTimingContext())
	ctx, unlock, err := acquireMutationPaths(ctx, paths)
	require.NoError(t, err)
	defer unlock()
	records := make([]*mutationCommitRecord, len(paths))
	targets := make([]sourceMutationTarget, len(paths))
	for i, path := range paths {
		content := []string{"package main\nfunc After() {}\n", "package main\nfunc Moved() {}\n"}[i]
		records[i], err = s.commitFileMutation(ctx, "write_file", "", "", filepath.Base(path), path, []byte(content), 0600)
		require.NoError(t, err)
		targets[i] = captureSourceMutationTarget(ctx, path)
	}
	extractor.active.Store(true)
	admissions := s.admitSourceMutationFrontier(ctx, targets)
	unlock()
	outcomes := s.awaitSourceMutationFrontier(ctx, admissions)
	publications := make([]*indexer.PublicationPhaseRecord, len(paths))
	for i, record := range records {
		require.NotNil(t, admissions[i].receipt)
		require.True(t, outcomes[i].Pending)
		record.recordGraph(outcomes[i])
		publications[i] = s.openCommittedPublicationRecord(ctx, record)
		require.NotNil(t, publications[i])
		require.Equal(t, outcomes[i].Generation, publications[i].Snapshot().Ticket)
	}
	require.NotSame(t, publications[0], publications[1])
	ack.finishActual()
	ack.finishReply(nil, nil)
	select {
	case <-extractor.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("actual one-set parser did not enter")
	}
	for _, record := range records {
		status := s.mutationStatusPayload(record)
		require.Equal(t, mutationGraphPending, status["graph_status"])
		require.Nil(t, status["phase_timing"].(indexer.EditTimingWindow).SharedSetExecution)
	}
	release.Do(func() { close(extractor.release) })
	for i, record := range records {
		require.Eventually(t, func() bool {
			status := s.mutationStatusPayload(record)
			window, ok := status["phase_timing"].(indexer.EditTimingWindow)
			return ok && status["graph_status"] == mutationGraphFresh && window.SharedSetExecution != nil
		}, 5*time.Second, time.Millisecond)
		status := s.mutationStatusPayload(record)
		phases := status["publication_phases"].(indexer.PublicationPhaseSnapshot)
		window := status["phase_timing"].(indexer.EditTimingWindow)
		require.Equal(t, publications[i].Snapshot().Ticket, phases.Ticket)
		require.Equal(t, indexer.PublicationSourceMCPEdit, phases.Source)
		require.True(t, window.SharedSetExecution.NonadditiveAcrossMembers)
		require.Equal(t, phases.Ticket, window.SharedSetExecution.MemberGeneration)
		require.Equal(t, 2, window.SharedSetExecution.ExecutorFiles)
		require.Nil(t, window.PatchWallMS)
		require.Nil(t, window.PatchAccounting)
		require.NotContains(t, status, "requested_background")
	}
	parses := parser.ParseCount()
	for range 3 {
		for _, record := range records {
			s.mutationStatusPayload(record)
		}
	}
	require.Equal(t, parses, parser.ParseCount(), "status must not trigger another parse to obtain shared diagnostics")
}
