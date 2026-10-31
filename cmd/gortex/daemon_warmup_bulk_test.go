package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
)

func TestDrainGoModAndFinalizeWarmupBulkOrdersDrainBeforeFinalize(t *testing.T) {
	var order []string
	wantErr := errors.New("finalize failed")

	_, _, err := drainGoModAndFinalizeWarmupBulk(
		func() { order = append(order, "drain") },
		func() error {
			order = append(order, "finalize")
			return wantErr
		},
	)

	if !errors.Is(err, wantErr) {
		t.Fatalf("finalize error = %v, want %v", err, wantErr)
	}
	if len(order) != 2 || order[0] != "drain" || order[1] != "finalize" {
		t.Fatalf("operation order = %v, want [drain finalize]", order)
	}
}

func TestDrainGoModAndFinalizeWarmupBulkWithoutBulkFinalizer(t *testing.T) {
	drains := 0
	_, finalizeElapsed, err := drainGoModAndFinalizeWarmupBulk(func() { drains++ }, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if drains != 1 {
		t.Fatalf("drains = %d, want 1", drains)
	}
	if finalizeElapsed != 0 {
		t.Fatalf("finalize elapsed = %s, want 0", finalizeElapsed)
	}
}

// TestWarmupFailedFinalSealGivesUpTheCoordinatedWindow: a cold warmup whose
// final seal fails logs the failure and closes the coordinated window, rather
// than keeping its relaxed-durability writer until the daemon shuts down. The
// writer is back at its own synchronous level, the planner probe no longer
// sees a live window, and the store's repair rebuilds the index the seal
// could not.
func TestWarmupFailedFinalSealGivesUpTheCoordinatedWindow(t *testing.T) {
	t.Setenv("GORTEX_PARSER_ISOLATION", "0")
	root := t.TempDir()
	t.Setenv("HOME", filepath.Join(root, "home"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(root, "data"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(root, "cache"))
	repo := filepath.Join(root, "repo")
	require.NoError(t, os.Mkdir(repo, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(repo, "main.go"), []byte("package main\nfunc Main() {}\n"), 0o600))
	store, err := store_sqlite.Open(filepath.Join(root, "store.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	ctx := context.Background()
	writerSync, err := store.WriterPragmaForTest(ctx, "synchronous")
	require.NoError(t, err)
	cm, err := config.NewConfigManager(filepath.Join(root, "config.yaml"))
	require.NoError(t, err)
	require.NoError(t, cm.Global().AddRepo(config.RepoEntry{Path: repo}))
	reg := parser.NewRegistry()
	reg.Register(languages.NewGoExtractor())
	logs, observed := observer.New(zap.InfoLevel)
	logger := zap.New(logs)
	idx := indexer.New(store, reg, config.Default().Index, logger)
	mi := indexer.NewMultiIndexer(store, reg, idx.Search(), cm, logger)
	t.Cleanup(func() { require.NoError(t, mi.Close(context.Background())) })
	state := &daemonState{graph: store, indexer: idx, multiIndexer: mi, configManager: cm}
	injected := errors.New("injected final seal failure")
	restore := store.SetBulkSealHookForTest(func(index string) error {
		if index == "nodes_by_kind" {
			return injected
		}
		return nil
	})

	_, _ = warmupDaemonState(state, logger, func() {})
	restore()
	require.NotEmpty(t, observed.FilterMessage("daemon: coordinated cold bulk-load finalize failed").All(),
		"the fixture must fail the coordinated window's final seal")
	gotSync, err := store.WriterPragmaForTest(ctx, "synchronous")
	require.NoError(t, err)
	assert.Equal(t, writerSync, gotSync, "writer durability must be restored")
	health, err := store.PlannerStatsHealth(ctx)
	require.NoError(t, err)
	assert.NotEqual(t, "bulk_window_active", health.Reason, "the failed window must not stay open")
	deadline := time.Now().Add(10 * time.Second)
	for health.Reason == "dense_indexes_missing" && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
		health, err = store.PlannerStatsHealth(ctx)
		require.NoError(t, err)
	}
	assert.NotEqual(t, "dense_indexes_missing", health.Reason, "the repair must rebuild the index the seal left out")
}
