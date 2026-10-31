package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

type ownedCompactStub struct {
	graph.Store
	path     string
	stats    atomic.Int32
	entered  chan struct{}
	canceled chan struct{}
	release  chan struct{}
	joined   atomic.Bool
}

func (s *ownedCompactStub) Path() string { return s.path }
func (s *ownedCompactStub) CompactStats() (int64, int64) {
	s.stats.Add(1)
	return 1<<30 + 1, 2 << 30
}
func (s *ownedCompactStub) CompactContext(ctx context.Context) error {
	close(s.entered)
	<-ctx.Done()
	close(s.canceled)
	<-s.release // Actual cleanup, not the cancellation request, owns completion.
	s.joined.Store(true)
	return ctx.Err()
}

func TestDaemonCompactionWaitsForWarmupCompletionAndJoinsBeforeBackend(t *testing.T) {
	t.Setenv("GORTEX_SKIP_STORE_COMPACT", "0")
	stub := &ownedCompactStub{
		path:    filepath.Join(t.TempDir(), "store.sqlite"),
		entered: make(chan struct{}), canceled: make(chan struct{}), release: make(chan struct{}),
	}
	warmup, task := newDaemonWarmup(t.Context()), newDaemonWarmup(t.Context())
	finishWarmup, scheduled := make(chan struct{}), make(chan struct{})
	var finishOnce, releaseOnce sync.Once
	finish := func() { finishOnce.Do(func() { close(finishWarmup) }) }
	release := func() { releaseOnce.Do(func() { close(stub.release) }) }
	t.Cleanup(func() {
		task.cancel()
		warmup.cancel()
		finish()
		release()
		warmup.wait()
		task.wait()
	})
	core, logs := observer.New(zap.DebugLevel)
	logger := zap.New(core)
	state := &daemonState{graph: stub}
	require.True(t, warmup.start(func(context.Context) {
		logger.Info("daemon: enrichment complete")
		scheduleDaemonCompaction(task, warmup, state, logger)
		close(scheduled)
		<-finishWarmup
	}))
	select {
	case <-scheduled:
	case <-time.After(5 * time.Second):
		t.Fatal("warmup did not schedule its owned tail")
	}
	require.Zero(t, stub.stats.Load(), "even statistics must wait for the producer completion barrier")
	require.False(t, scheduleDaemonCompaction(task, warmup, state, logger), "one attempt per boot")
	finish()
	select {
	case <-stub.entered:
	case <-time.After(5 * time.Second):
		t.Fatalf("post-warmup compaction did not start: %v", logs.All())
	}
	entries := logs.All()
	require.Equal(t, "daemon: enrichment complete", entries[0].Message)
	require.Equal(t, "daemon: one-time store compaction starting — VACUUM may take minutes on a multi-GB store", entries[1].Message)
	closed := make(chan struct{})
	teardown := newDaemonTeardown(func() { task.cancel(); joinDaemonCompaction(task, logger) }, func() error {
		if !stub.joined.Load() {
			t.Error("backend close preceded admitted VACUUM completion")
		}
		close(closed)
		return nil
	})
	done := make(chan error, 1)
	go func() { done <- teardown() }()
	select {
	case <-stub.canceled:
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown failed to cancel compaction")
	}
	select {
	case <-closed:
		t.Fatal("canceled does not mean physically joined")
	default:
	}
	release()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("completed compaction did not release backend teardown")
	}
}

func TestDaemonCanceledCompactionDoesNotMeasureOrLaunch(t *testing.T) {
	warmup, task := newDaemonWarmup(t.Context()), newDaemonWarmup(t.Context())
	t.Cleanup(func() { task.cancel(); warmup.cancel(); warmup.wait(); task.wait() })
	task.cancel()
	require.False(t, scheduleDaemonCompaction(task, warmup, &daemonState{}, zap.NewNop()))
}

func TestMaybeCompactStoreBelowThresholdAfterWarmup(t *testing.T) {
	t.Setenv("GORTEX_SKIP_STORE_COMPACT", "0")
	core, logs := observer.New(zap.DebugLevel)
	stub := &compactStub{path: filepath.Join(t.TempDir(), "store.sqlite"), free: 1, total: 100}
	maybeCompactStoreContext(t.Context(), stub, false, zap.New(core))
	require.Zero(t, stub.compacts)
	entry := logs.FilterMessage("daemon: store compaction not warranted").All()
	require.Len(t, entry, 1)
	require.Equal(t, "below_threshold_after_warmup", entry[0].ContextMap()["reason"])
}

// Inject only the threshold statistics; actual compaction still runs on the
// real boot's SQLite store. A unit test need not allocate a >1GiB freelist to
// prove that a real migration is remembered for one boot only.
type bootCompactStore struct {
	graph.Store
	file  *store_sqlite.Store
	calls int
}

func (s *bootCompactStore) Path() string                 { return s.file.Path() }
func (s *bootCompactStore) CompactStats() (int64, int64) { return 1<<30 + 1, 2 << 30 }
func (s *bootCompactStore) CompactContext(ctx context.Context) error {
	s.calls++
	return s.file.CompactContext(ctx)
}

func compactionBootEnv(t *testing.T) {
	t.Helper()
	t.Setenv("GORTEX_SKIP_STORE_COMPACT", "0")
	root := t.TempDir()
	for key, path := range map[string]string{
		"HOME": "home", "XDG_DATA_HOME": "data", "XDG_CONFIG_HOME": "config", "XDG_CACHE_HOME": "cache", "XDG_STATE_HOME": "state",
	} {
		t.Setenv(key, filepath.Join(root, path))
	}
	configPath := filepath.Join(root, "daemon.yaml")
	require.NoError(t, os.WriteFile(configPath, []byte("semantic:\n  enabled: false\n"), 0o600))
	oldConfig, oldPath := cfgFile, daemonBackendPath
	t.Cleanup(func() { cfgFile, daemonBackendPath = oldConfig, oldPath })
	cfgFile, daemonBackendPath = configPath, filepath.Join(root, "store.sqlite")
}

func TestDaemonMigrationDefersCompactionForOneBoot(t *testing.T) {
	for _, test := range []struct {
		name    string
		version int
	}{
		{name: "in-place migration", version: 39},
		{name: "supported destructive rebuild", version: 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			compactionBootEnv(t)
			initial, err := store_sqlite.Open(daemonBackendPath)
			require.NoError(t, err)
			require.NoError(t, initial.Close())
			db, err := sql.Open("sqlite", daemonBackendPath)
			require.NoError(t, err)
			// v39 exercises a real in-place step; v2 requires the supported
			// destructive topology rebuild, which has no MigrationStarted event.
			_, err = db.Exec(fmt.Sprintf(`PRAGMA user_version=%d`, test.version))
			require.NoError(t, err)
			require.NoError(t, db.Close())
			core, logs := observer.New(zap.DebugLevel)
			logger := zap.New(core)
			observed := 0
			first, err := buildDaemonState(logger, func(p store_sqlite.MigrationProgress) {
				if p.Phase == store_sqlite.MigrationStarted {
					observed++
				}
			})
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, first.shared.Close()) })
			if test.version == 39 {
				require.Positive(t, observed, "the existing observer must still receive in-place migration boundaries")
			} else {
				require.Zero(t, observed, "rebuild is proven by the existing store ownership seam, not an invented observer event")
				require.True(t, storeNeedsRebuild(first.graph))
			}
			require.True(t, first.migratedStore)
			file, ok := first.graph.(*store_sqlite.Store)
			require.True(t, ok)
			stub := &bootCompactStore{Store: first.graph, file: file}
			maybeCompactStoreContext(t.Context(), stub, first.migratedStore, logger)
			require.Zero(t, stub.calls)
			entry := logs.FilterMessage("daemon: store compaction deferred to next boot").All()
			require.Len(t, entry, 1)
			require.Equal(t, "after_migration", entry[0].ContextMap()["reason"])
			require.NoError(t, first.shared.Close())
			second, err := buildDaemonState(logger)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, second.shared.Close()) })
			require.False(t, second.migratedStore, "no durable skip flag may survive to the next boot")
			require.False(t, storeNeedsRebuild(second.graph))
			file, ok = second.graph.(*store_sqlite.Store)
			require.True(t, ok)
			stub = &bootCompactStore{Store: second.graph, file: file}
			maybeCompactStoreContext(t.Context(), stub, second.migratedStore, logger)
			require.Equal(t, 1, stub.calls)
			require.Len(t, logs.FilterMessage("daemon: store compaction finished").All(), 1)
		})
	}
}

func TestWarmupDaemonStateDoesNotRunVacuum(t *testing.T) {
	compactionBootEnv(t)
	state, err := buildDaemonState(zap.NewNop())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, state.shared.Close()) })
	stub := &compactStub{Store: state.graph, path: state.backendPath, free: 1<<30 + 1, total: 2 << 30}
	state.graph = stub
	ready := false
	watcher, _ := warmupDaemonStateContext(t.Context(), state, zap.NewNop(), func() { ready = true }, nil)
	if watcher != nil {
		t.Cleanup(func() { require.NoError(t, watcher.Stop()) })
	}
	require.True(t, ready, "real warmup must reach queryability")
	require.Zero(t, stub.compacts, "even eligible compaction must stay outside warmup")
}
