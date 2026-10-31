package main

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
)

type warmupCancellationExtractor struct {
	parser.Extractor
	entered chan struct{}
	release chan struct{}
	once    sync.Once
	calls   atomic.Int32
}

func (e *warmupCancellationExtractor) Extract(path string, src []byte) (*parser.ExtractionResult, error) {
	e.calls.Add(1)
	e.once.Do(func() { close(e.entered); <-e.release })
	return e.Extractor.Extract(path, src)
}

func TestWarmupDaemonStateCancellationStopsBeforeReadyAndWatcher(t *testing.T) {
	t.Setenv("GORTEX_PARSER_ISOLATION", "0")
	root := t.TempDir()
	t.Setenv("HOME", filepath.Join(root, "home"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(root, "data"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(root, "cache"))
	repo := filepath.Join(root, "repo")
	require.NoError(t, os.Mkdir(repo, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(repo, "main.go"), []byte("package main\nfunc Main() {}\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(repo, "other.go"), []byte("package main\nfunc Other() {}\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(repo, ".gortex.yaml"), []byte("index:\n  workers: 1\n"), 0o600))
	store, err := store_sqlite.Open(filepath.Join(root, "store.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	cm, err := config.NewConfigManager(filepath.Join(root, "config.yaml"))
	require.NoError(t, err)
	require.NoError(t, cm.Global().AddRepo(config.RepoEntry{Path: repo}))
	extractor := &warmupCancellationExtractor{Extractor: languages.NewGoExtractor(), entered: make(chan struct{}), release: make(chan struct{})}
	reg := parser.NewRegistry()
	reg.Register(extractor)
	indexCfg := config.Default().Index
	indexCfg.Workers = 1
	logs, observed := observer.New(zap.InfoLevel)
	logger := zap.New(logs)
	idx := indexer.New(store, reg, indexCfg, logger)
	mi := indexer.NewMultiIndexer(store, reg, idx.Search(), cm, logger)
	t.Cleanup(func() { require.NoError(t, mi.Close(context.Background())) })
	state := &daemonState{graph: store, indexer: idx, multiIndexer: mi, configManager: cm}
	ctx, cancel := context.WithCancel(context.Background())
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(extractor.release) }) }
	var ready, watcherStarted atomic.Bool
	done := make(chan struct{})
	var watcher *indexer.MultiWatcher
	go func() {
		defer close(done)
		watcher, _ = warmupDaemonStateContext(ctx, state, logger, func() { ready.Store(true) }, func(context.Context, *indexer.MultiWatcher) error {
			watcherStarted.Store(true)
			return nil
		})
	}()
	t.Cleanup(func() { cancel(); release(); <-done })
	select {
	case <-extractor.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("real warmup never entered the parser")
	}
	cancel()
	release()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("canceled warmup did not join its parse worker")
	}
	require.Nil(t, watcher)
	require.False(t, ready.Load(), "cancellation must not certify queryability")
	require.False(t, watcherStarted.Load(), "cancellation must not enter watcher handoff")
	require.Equal(t, int32(1), extractor.calls.Load(), "canceled parse admission must not extract the queued file")
	require.Empty(t, observed.FilterMessage("daemon: warmup phase start").FilterField(zap.String("phase", "global_resolve")).All())
	require.Empty(t, observed.FilterMessage("daemon: warmup step start").FilterField(zap.String("step", "watcher_start")).All())
	require.NotEmpty(t, observed.FilterMessage("daemon: warmup canceled").All())
}
