package mcp

import (
	"bytes"
	"context"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/search"
)

// Hold the actual executor after its start but before extraction/application.
// Initial indexing does not contain the marker and follows the ordinary path.
type straddlingReadRepairExtractor struct {
	parser.Extractor
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (e *straddlingReadRepairExtractor) Extract(path string, src []byte) (*parser.ExtractionResult, error) {
	if bytes.Contains(src, []byte("AddedByStraddlingReader")) {
		e.once.Do(func() { close(e.entered) })
		<-e.release
	}
	return e.Extractor.Extract(path, src)
}

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
	registry := testRegistry()
	extractor, ok := registry.GetByExtension(".go")
	require.True(t, ok)
	held := &straddlingReadRepairExtractor{Extractor: extractor, entered: make(chan struct{}), release: make(chan struct{})}
	registry.Register(held)
	g := graph.New()
	mi := indexer.NewMultiIndexer(g, registry, search.NewNull(), cm, zap.NewNop())
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
	release := func() { releaseOnce.Do(func() { close(held.release) }) }
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
		// Test-only visible-byte setup, followed by the real atomic commit of
		// exactly those bytes. No production write or scheduling hook is added.
		bumpFile(t, path, string(after))
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
		case <-held.entered:
		case <-time.After(5 * time.Second):
			t.Fatal("actual executor did not enter extraction before commit")
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
	require.True(t, observed.observation.AcceptedFor(idx, gitBlobSHA(after)))
	require.NotEmpty(t, g.FindNodesByName("AddedByStraddlingReader"))
	require.NotNil(t, record.snapshot().readRepair, "accepted publication after commit must retain its independent executor window")
	runtime.KeepAlive(idx)
}
