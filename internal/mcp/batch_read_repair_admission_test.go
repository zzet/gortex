package mcp

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/search"
)

func TestBatchActualReaderCapturedBeforeFinalVerdictCompletesAfterAdmission(t *testing.T) {
	root := setupMiniRepo(t, "batch-reader-admission")
	path := filepath.Join(root, "main.go")
	before, err := os.ReadFile(path)
	require.NoError(t, err)
	after := string(before) + "\nfunc AddedByBatchReader() { Hello() }\n"
	gc := &config.GlobalConfig{Repos: []config.RepoEntry{{Path: root, Name: "fixture"}}}
	gc.SetConfigPath(filepath.Join(t.TempDir(), "config.yaml"))
	require.NoError(t, gc.Save())
	cm, err := config.NewConfigManager(gc.ConfigPath())
	require.NoError(t, err)
	entered, resume := make(chan struct{}), make(chan struct{})
	var armed atomic.Bool
	var enterOnce, releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(resume) }) }
	core, _ := observer.New(zap.InfoLevel)
	logger := zap.New(core, zap.Hooks(func(entry zapcore.Entry) error {
		if armed.Load() && entry.Message == "indexer: reconcile phase started" {
			enterOnce.Do(func() { close(entered) })
			<-resume
		}
		return nil
	}))
	g := graph.New()
	mi := indexer.NewMultiIndexer(g, testRegistry(), search.NewNull(), cm, logger)
	t.Cleanup(func() { require.NoError(t, mi.Close(context.Background())) })
	_, err = mi.IndexAll()
	require.NoError(t, err)
	idx := mi.GetIndexer("fixture")
	require.NotNil(t, idx)
	watcher := &canonicalPublicationWatcher{hold: true}
	s := newAtomicBatchTestServer(t, watcher)
	s.mutationReindexWait = time.Nanosecond
	type execution struct {
		observation indexer.ReadRepairObservation
		err         error
	}
	done := make(chan execution, 1)
	started, joined := false, false
	t.Cleanup(func() {
		release()
		if started && !joined {
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Error("batch reader was not joined")
			}
		}
		for _, ch := range watcher.results {
			close(ch)
		}
	})
	transaction := "batch-reader-before-admission"
	captured := false
	var record *mutationCommitRecord
	s.batchDurabilityOverride = &batchDurabilityOps{syncDirectory: func(dir string) error {
		if filepath.Clean(dir) != filepath.Clean(root) || captured {
			return syncBatchDirectory(dir)
		}
		captured = true
		value, ok := s.batchTransactions.Load(transaction)
		require.True(t, ok)
		refs := s.batchFileObservationReferences(value.(*batchTransactionState).snapshot())
		require.Len(t, refs, 1)
		s.mutationCommits.mu.Lock()
		record = s.mutationCommits.byID[refs[0].Receipt]
		s.mutationCommits.mu.Unlock()
		require.NotNil(t, record)
		record.mu.RLock()
		require.Equal(t, mutationDiskInFlight, record.disk, "durable transaction verdict is still pending")
		require.Equal(t, gitBlobSHA([]byte(after)), record.newSHA, "real forward writer already returned these exact bytes")
		require.False(t, record.committedAt.IsZero())
		require.Nil(t, record.publication)
		record.mu.RUnlock()
		observed := s.readRepairObserver(t.Context(), path, idx)
		require.NotNil(t, observed, "reader must capture the same writer-return-known record before final batch verdict/admission")
		armed.Store(true)
		started = true
		go func() {
			var observation indexer.ReadRepairObservation
			_, repairErr := mi.IncrementalReindexRepoObserved("fixture", []string{path}, &indexer.ReadRepairObserver{Complete: func(o indexer.ReadRepairObservation) { observation = o; observed.Complete(o) }})
			done <- execution{observation, repairErr}
		}()
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			t.Fatal("actual reader did not start before batch verdict")
		}
		return syncBatchDirectory(dir)
	}}
	req := editOperationRequest("batch_edit", "")
	req.Params.Arguments = map[string]any{"transaction_id": transaction, "edits": []any{map[string]any{"op": "edit_file", "path": path, "old_string": string(before), "new_string": after}}}
	result, err := s.boundToolHandler(s.handleAtomicBatchEdit)(t.Context(), req)
	require.NoError(t, err)
	require.False(t, result.IsError, "%v", result.Content)
	require.True(t, captured)
	record.mu.RLock()
	require.Equal(t, mutationDiskCommitted, record.disk)
	require.NotNil(t, record.publication)
	require.NotNil(t, record.readRepairAdmission.Value())
	writerReturned := record.committedAt
	ticket, generation := record.reindexReceipt, record.reindexGeneration
	record.mu.RUnlock()
	release()
	var actual execution
	select {
	case actual = <-done:
		joined = true
	case <-time.After(5 * time.Second):
		t.Fatal("actual reader did not finish after admission")
	}
	require.NoError(t, actual.err)
	require.True(t, actual.observation.AcceptedFor(idx, gitBlobSHA([]byte(after))), "actual executor must apply the offered after-image")
	require.False(t, actual.observation.AcceptedAt().Before(writerReturned))
	require.NotEmpty(t, g.FindNodesByName("AddedByBatchReader"))
	record.mu.RLock()
	retained := record.readRepair
	require.NotNil(t, retained, "successful final verdict and actual admission must validate the earlier exact candidate")
	require.Equal(t, ticket, retained.requestedReceipt)
	require.Equal(t, generation, retained.requestedGeneration)
	require.Equal(t, gitBlobSHA([]byte(after)), retained.acceptedBlobSHA)
	record.mu.RUnlock()
	runtime.KeepAlive(idx)
}
