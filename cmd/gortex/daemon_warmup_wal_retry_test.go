package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"github.com/zzet/gortex/internal/search"
)

// warmupWALRetryFixture is a real store and multi-indexer over one Go
// repository per name, with warmupLoadRepo replaced by load.
type warmupWALRetryFixture struct {
	g     *store_sqlite.Store
	mi    *indexer.MultiIndexer
	state *daemonState
	repos map[string]string // name -> path
}

func newWarmupWALRetryFixture(t *testing.T, names ...string) *warmupWALRetryFixture {
	t.Helper()
	dir := t.TempDir()
	f := &warmupWALRetryFixture{repos: map[string]string{}}
	var entries []config.RepoEntry
	for _, name := range names {
		repo := filepath.Join(dir, name)
		require.NoError(t, os.MkdirAll(repo, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(repo, "main.go"), []byte("package a\nfunc A() {}\n"), 0o644))
		f.repos[name] = repo
		entries = append(entries, config.RepoEntry{Path: repo, Name: name})
	}
	g, err := store_sqlite.Open(filepath.Join(dir, "store.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, g.Close()) })
	cm, err := config.NewConfigManager(filepath.Join(dir, "config.yaml"))
	require.NoError(t, err)
	cm.Global().Repos = entries
	reg := parser.NewRegistry()
	reg.Register(languages.NewGoExtractor())
	mi := indexer.NewMultiIndexer(g, reg, search.NewNull(), cm, zap.NewNop())
	t.Cleanup(func() { require.NoError(t, mi.Close(context.Background())) })
	f.g, f.mi = g, mi
	f.state = &daemonState{graph: g, multiIndexer: mi, configManager: cm}
	return f
}

// stubLoads replaces warmupLoadRepo for the test. load receives the call
// number for the entry (1 for its first load) and the real loader.
func stubLoads(t *testing.T, load func(ctx context.Context, entry config.RepoEntry, call int, real func(context.Context) (string, error)) (string, error)) {
	t.Helper()
	real := warmupLoadRepo
	t.Cleanup(func() { warmupLoadRepo = real })
	var mu sync.Mutex
	calls := map[string]int{}
	warmupLoadRepo = func(ctx context.Context, a warmupRepoAccounting, state *daemonState, entry config.RepoEntry, prior map[string]int64, deferrable bool, logger *zap.Logger) (string, error) {
		mu.Lock()
		calls[entry.Path]++
		call := calls[entry.Path]
		mu.Unlock()
		return load(ctx, entry, call, func(ctx context.Context) (string, error) {
			return real(ctx, a, state, entry, prior, deferrable, logger)
		})
	}
}

func walRefusal(entry config.RepoEntry) error {
	refusal := fmt.Errorf("%w: %w", store_sqlite.ErrWarmupWALPressure, context.DeadlineExceeded)
	return fmt.Errorf("reconciling %s: %w", entry.Path, refusal)
}

func runWarmup(t *testing.T, ctx context.Context, f *warmupWALRetryFixture) {
	t.Helper()
	watcher, _ := warmupDaemonStateContext(ctx, f.state, zap.NewNop(), func() {}, nil)
	if watcher != nil {
		t.Cleanup(func() { _ = watcher.Stop() })
	}
}

// A warmup load refused by the WAL boundary is transient: the reclaim was
// blocked by the other parallel loads. The repository must be retried after the
// parallel phase and end ready, not stay failed until the next restart.
func TestWarmupRetriesRepositoryRefusedByWALPressure(t *testing.T) {
	f := newWarmupWALRetryFixture(t, "repo")
	var mu sync.Mutex
	calls := 0
	stubLoads(t, func(ctx context.Context, entry config.RepoEntry, call int, real func(context.Context) (string, error)) (string, error) {
		mu.Lock()
		calls = call
		mu.Unlock()
		if call == 1 {
			return "reconcile", walRefusal(entry)
		}
		return real(ctx)
	})

	runWarmup(t, context.Background(), f)

	mu.Lock()
	assert.Equal(t, 2, calls, "the refused repository is loaded once more after the parallel phase")
	mu.Unlock()
	load, ok := f.mi.RepoLoadFor(f.repos["repo"])
	require.True(t, ok)
	assert.Equal(t, indexer.RepoLoadReady, load.State, "reason: %s", load.Reason)
	assert.NotNil(t, f.mi.GetMetadata("repo"))
	assert.NotEmpty(t, f.g.GetRepoNodes("repo"))
}

// The retry is made once: a second refusal fails the repository with the
// refusal as its reason.
func TestWarmupWALRetryIsMadeOnce(t *testing.T) {
	f := newWarmupWALRetryFixture(t, "repo")
	var mu sync.Mutex
	calls := 0
	stubLoads(t, func(ctx context.Context, entry config.RepoEntry, call int, real func(context.Context) (string, error)) (string, error) {
		mu.Lock()
		calls = call
		mu.Unlock()
		return "reconcile", walRefusal(entry)
	})

	runWarmup(t, context.Background(), f)

	mu.Lock()
	assert.Equal(t, 2, calls)
	mu.Unlock()
	load, ok := f.mi.RepoLoadFor(f.repos["repo"])
	require.True(t, ok)
	assert.Equal(t, indexer.RepoLoadFailed, load.State)
	assert.Contains(t, load.Reason, store_sqlite.ErrWarmupWALPressure.Error())
}

// A panic in the retry is recovered like a panic in the parallel phase: the
// repository fails with the crash as its reason and warmup completes.
func TestWarmupWALRetryRecoversPanic(t *testing.T) {
	f := newWarmupWALRetryFixture(t, "repo")
	stubLoads(t, func(ctx context.Context, entry config.RepoEntry, call int, real func(context.Context) (string, error)) (string, error) {
		if call == 1 {
			return "reconcile", walRefusal(entry)
		}
		panic("injected retry panic")
	})

	runWarmup(t, context.Background(), f)

	load, ok := f.mi.RepoLoadFor(f.repos["repo"])
	require.True(t, ok)
	assert.Equal(t, indexer.RepoLoadFailed, load.State)
	assert.Contains(t, load.Reason, "injected retry panic")
}

// A warmup cancelled while a repository is refused does not retry it, and the
// repository ends failed, not loading. The cancel may land before or after the
// worker defers the repository; either way the settle pass or the worker ends
// it. TestWarmupWALRetriesStopOnCancel pins the deferred side.
func TestWarmupCancelledBeforeWALRetrySettlesRepository(t *testing.T) {
	f := newWarmupWALRetryFixture(t, "a", "b")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	refused := make(chan struct{})
	var mu sync.Mutex
	aCalls := 0
	stubLoads(t, func(ctx context.Context, entry config.RepoEntry, call int, real func(context.Context) (string, error)) (string, error) {
		if entry.Path == f.repos["a"] {
			mu.Lock()
			aCalls = call
			mu.Unlock()
			if call == 1 {
				defer close(refused)
				return "reconcile", walRefusal(entry)
			}
			return real(ctx)
		}
		// b ends the warmup once a has been deferred.
		<-refused
		cancel()
		return "track", context.Canceled
	})

	runWarmup(t, ctx, f)

	mu.Lock()
	assert.Equal(t, 1, aCalls, "a cancelled warmup does not retry")
	mu.Unlock()
	load, ok := f.mi.RepoLoadFor(f.repos["a"])
	require.True(t, ok)
	assert.Equal(t, indexer.RepoLoadFailed, load.State, "reason: %s", load.Reason)
}

// A refusal caused by anything else still fails the repository: only the WAL
// boundary's own timeout is retried.
func TestWarmupDoesNotRetryOtherLoadFailures(t *testing.T) {
	ctx := t.Context()
	assert.True(t, warmupWALRetryable(ctx, walRefusal(config.RepoEntry{Path: "x"})))
	assert.False(t, warmupWALRetryable(ctx, fmt.Errorf("reconciling x: %w", context.DeadlineExceeded)))
	assert.False(t, warmupWALRetryable(ctx, nil))
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	assert.False(t, warmupWALRetryable(cancelled, walRefusal(config.RepoEntry{Path: "x"})),
		"a cancelled warmup is not retried")
}

// run stops retrying at a cancelled context and hands each remaining
// repository to skip with its refusal, and drains its queue so nothing is
// retried twice.
func TestWarmupWALRetriesStopOnCancel(t *testing.T) {
	var r warmupWALRetries
	refusal := walRefusal(config.RepoEntry{Path: "b"})
	r.add(config.RepoEntry{Path: "a"}, walRefusal(config.RepoEntry{Path: "a"}))
	r.add(config.RepoEntry{Path: "b"}, refusal)
	ctx, cancel := context.WithCancel(t.Context())
	var retried []string
	skipped := map[string]error{}
	skip := func(entry config.RepoEntry, reason error) { skipped[entry.Path] = reason }
	r.run(ctx, func(entry config.RepoEntry) {
		retried = append(retried, entry.Path)
		cancel()
	}, skip)
	assert.Equal(t, []string{"a"}, retried)
	require.Contains(t, skipped, "b")
	assert.ErrorIs(t, skipped["b"], store_sqlite.ErrWarmupWALPressure, "the skip reason carries the refusal")
	r.run(t.Context(), func(entry config.RepoEntry) { retried = append(retried, entry.Path) }, skip)
	assert.Equal(t, []string{"a"}, retried, "a drained queue retries nothing")
}
