package mcp

import (
	"context"
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

// This intentionally specifies the unresolved boundary: execution begins
// before commit bookkeeping, but its actual applied bytes publish afterward.
// The existing wholly-before-commit rejection remains a separate control.
func TestReadRepairActualExecutorStraddlesCommittedBookkeeping(t *testing.T) {
	root := setupMiniRepo(t, "straddling-reader")
	path := filepath.Join(root, "main.go")
	gc := &config.GlobalConfig{Repos: []config.RepoEntry{{Path: root, Name: "fixture"}}}
	gc.SetConfigPath(filepath.Join(t.TempDir(), "config.yaml"))
	require.NoError(t, gc.Save())
	cm, err := config.NewConfigManager(gc.ConfigPath())
	require.NoError(t, err)
	entered, resume := make(chan struct{}), make(chan struct{})
	var armed atomic.Bool
	var enteredOnce sync.Once
	core, _ := observer.New(zap.InfoLevel)
	logger := zap.New(core, zap.Hooks(func(entry zapcore.Entry) error {
		if armed.Load() && entry.Message == "indexer: reconcile phase started" {
			enteredOnce.Do(func() { close(entered) })
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
	s := newMutationServer()
	w := &readRepairHeldWatcher{canonicalPublicationWatcher: canonicalPublicationWatcher{hold: true}, enqueued: make(chan struct{})}
	s.watcher = w
	ctx, cancel := context.WithCancel(canonicalTimingContext())
	t.Cleanup(cancel)
	after := []byte("package main\nfunc Hello() {}\nfunc AddedByStraddlingReader() {}\n")
	type executionResult struct {
		observation indexer.ReadRepairObservation
		err         error
	}
	done := make(chan executionResult, 1)
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(resume) }) }
	started := false
	joined := false
	t.Cleanup(func() {
		release()
		if started && !joined {
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Error("straddling executor was not joined")
			}
		}
	})
	s.mutationPreCommitHook = func(record *mutationCommitRecord) {
		require.Equal(t, mutationDiskInFlight, record.diskStatus())
		observer := s.readRepairObserver(ctx, path, idx)
		require.NotNil(t, observer)
		// R has started before its first phase log; source discovery/read is
		// still ahead. Hold there so the real atomic write supplies the exact
		// bytes/inode consumed by the executor. No early test-only write is used.
		armed.Store(true)
		started = true
		go func() {
			var observation indexer.ReadRepairObservation
			_, repairErr := mi.IncrementalReindexRepoObserved("fixture", []string{path}, &indexer.ReadRepairObserver{
				Complete: func(o indexer.ReadRepairObservation) {
					observation = o
					observer.Complete(o)
				},
			})
			done <- executionResult{observation: observation, err: repairErr}
		}()
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			t.Fatal("actual executor did not enter its first phase before commit")
		}
	}
	record, err := s.commitFileMutation(ctx, "write_file", "logical-straddling", "straddling fixture", "main.go", path, after, 0o600)
	require.NoError(t, err)
	waiterDone := make(chan mutationReindexOutcome, 1)
	go func() { waiterDone <- s.mutationReindexState(ctx, path) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-waiterDone:
		case <-time.After(5 * time.Second):
			t.Error("watcher waiter was not joined")
		}
	})
	require.Eventually(t, func() bool {
		record.mu.RLock()
		defer record.mu.RUnlock()
		return record.readRepairAdmission.Value() != nil
	}, 5*time.Second, time.Millisecond)
	// Admission is bound, without waiting for the edit ACK or its timer.
	release()
	var observed executionResult
	select {
	case observed = <-done:
		joined = true
	case <-time.After(5 * time.Second):
		t.Fatal("actual executor did not finish after commit")
	}
	require.NoError(t, observed.err)
	start, end := observed.observation.Bounds()
	require.True(t, start.Before(record.committedAt), "real execution must straddle bookkeeping")
	require.True(t, end.After(record.committedAt))
	require.False(t, observed.observation.AcceptedAt().Before(record.committedAt))
	require.True(t, observed.observation.AcceptedFor(idx, gitBlobSHA(after)))
	require.NotEmpty(t, g.FindNodesByName("AddedByStraddlingReader"))
	require.NotNil(t, record.snapshot().readRepair, "accepted publication after commit must retain its independent executor window")
	runtime.KeepAlive(idx)
}
