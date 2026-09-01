package indexer

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
)

type gitWatcherStateStore struct {
	graph.Store
	mu     sync.Mutex
	states []graph.RepoIndexState
}

func (store *gitWatcherStateStore) SetRepoIndexState(state graph.RepoIndexState) error {
	store.mu.Lock()
	store.states = append(store.states, state)
	store.mu.Unlock()
	return nil
}

func (store *gitWatcherStateStore) snapshotStates() []graph.RepoIndexState {
	store.mu.Lock()
	defer store.mu.Unlock()
	return append([]graph.RepoIndexState(nil), store.states...)
}

func gitWatcherBoundaryIndexer(t testing.TB, store graph.Store, repoPath, prefix string) *Indexer {
	t.Helper()
	registry := parser.NewRegistry()
	registry.Register(languages.NewGoExtractor())
	cfg := config.Default()
	cfg.Index.Workers = 1
	idx := New(store, registry, cfg.Index, zap.NewNop())
	idx.SetRepoPrefix(prefix)
	idx.rootPath = repoPath
	return idx
}

func runGitWatcherGit(t testing.TB, repoPath string, args ...string) string {
	t.Helper()
	commandArgs := append([]string{"-C", repoPath}, args...)
	cmd := exec.Command("git", commandArgs...)
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, "git %s: %s", strings.Join(args, " "), output)
	return strings.TrimSpace(string(output))
}

func initGitWatcherRepo(t testing.TB) (string, string) {
	t.Helper()
	repoPath := t.TempDir()
	runGitWatcherGit(t, repoPath, "init")
	runGitWatcherGit(t, repoPath, "config", "user.name", "Gortex Test")
	runGitWatcherGit(t, repoPath, "config", "user.email", "gortex@example.invalid")
	runGitWatcherGit(t, repoPath, "config", "commit.gpgsign", "false")
	require.NoError(t, os.WriteFile(filepath.Join(repoPath, "main.go"), []byte("package main\n"), 0o644))
	runGitWatcherGit(t, repoPath, "add", "main.go")
	runGitWatcherGit(t, repoPath, "commit", "-m", "initial")
	return repoPath, runGitWatcherGit(t, repoPath, "rev-parse", "HEAD")
}

func TestGitWatcherFreshnessTailWaitsForLaneAndUsesReplacementIndexer(t *testing.T) {
	repoPath, _ := initGitWatcherRepo(t)
	oldStore := &gitWatcherStateStore{Store: graph.New()}
	newStore := &gitWatcherStateStore{Store: graph.New()}
	oldIndexer := gitWatcherBoundaryIndexer(t, oldStore, repoPath, "repo")
	newIndexer := gitWatcherBoundaryIndexer(t, newStore, repoPath, "repo")
	coordinator := newRepositoryMutationCoordinator(nil)
	oldIndexer.attachRepositoryMutationCoordinator(coordinator)
	newIndexer.attachRepositoryMutationCoordinator(coordinator)

	watcher, err := NewGitWatcher(repoPath, oldIndexer, zap.NewNop())
	require.NoError(t, err)
	t.Cleanup(func() { _ = watcher.Stop() })
	watcher.currentIndexer = func() *Indexer { return newIndexer }
	watcher.lastSHA = "old-sha"

	laneEntered := make(chan struct{})
	releaseLane := make(chan struct{})
	holderDone := make(chan error, 1)
	go func() {
		holderDone <- oldIndexer.coordinateRepositoryMutation(context.Background(), func() error {
			close(laneEntered)
			<-releaseLane
			return nil
		})
	}()
	<-laneEntered

	finalizeDone := make(chan error, 1)
	go func() {
		finalizeDone <- watcher.finalizeReconcile(context.Background(), "new-sha")
	}()
	select {
	case err := <-finalizeDone:
		t.Fatalf("freshness tail bypassed the occupied repository lane: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	watcher.mu.Lock()
	assert.Equal(t, "old-sha", watcher.lastSHA)
	watcher.mu.Unlock()
	assert.Empty(t, oldStore.snapshotStates())
	assert.Empty(t, newStore.snapshotStates())

	close(releaseLane)
	require.NoError(t, <-holderDone)
	require.NoError(t, <-finalizeDone)
	watcher.mu.Lock()
	assert.Equal(t, "new-sha", watcher.lastSHA)
	watcher.mu.Unlock()
	assert.Empty(t, oldStore.snapshotStates(), "retired Indexer must not be restamped")
	require.Len(t, newStore.snapshotStates(), 1, "replacement Indexer must receive the restamp")
}

func TestGitWatcherEmptyCommitFinalizesWithoutFullReindex(t *testing.T) {
	repoPath, oldSHA := initGitWatcherRepo(t)
	runGitWatcherGit(t, repoPath, "commit", "--allow-empty", "-m", "empty")
	newSHA := runGitWatcherGit(t, repoPath, "rev-parse", "HEAD")
	require.NotEqual(t, oldSHA, newSHA)

	store := &gitWatcherStateStore{Store: graph.New()}
	idx := gitWatcherBoundaryIndexer(t, store, repoPath, "repo")
	idx.attachRepositoryMutationCoordinator(newRepositoryMutationCoordinator(nil))
	watcher, err := NewGitWatcher(repoPath, idx, zap.NewNop())
	require.NoError(t, err)
	t.Cleanup(func() { _ = watcher.Stop() })
	watcher.lastSHA = oldSHA

	var batchCalls atomic.Int32
	watcher.batchReindex = func(paths []string) (*IndexResult, error) {
		batchCalls.Add(1)
		return &IndexResult{}, nil
	}
	watcher.reconcile("empty-commit-test")

	assert.Zero(t, batchCalls.Load(), "an empty commit must not escalate nil paths into a full reindex")
	watcher.mu.Lock()
	assert.Equal(t, newSHA, watcher.lastSHA)
	watcher.mu.Unlock()
	require.Len(t, store.snapshotStates(), 1, "empty commit must still restamp repository freshness")
}

func TestGitWatcherGuardedZeroPatchTransitionAcknowledgesOnce(t *testing.T) {
	repoPath, oldSHA := initGitWatcherRepo(t)
	require.NoError(t, os.WriteFile(filepath.Join(repoPath, "main.go"), []byte("package main\n\nfunc First() {}\n"), 0o644))
	runGitWatcherGit(t, repoPath, "add", "main.go")
	runGitWatcherGit(t, repoPath, "commit", "-m", "first change")
	firstSHA := runGitWatcherGit(t, repoPath, "rev-parse", "HEAD")
	require.NotEqual(t, oldSHA, firstSHA)

	store := &gitWatcherStateStore{Store: graph.New()}
	idx := gitWatcherBoundaryIndexer(t, store, repoPath, "repo")
	var executorCalls atomic.Int32
	var guardCalls atomic.Int32
	coordinator := newRepositoryMutationCoordinator(func([]string) (*IndexResult, error) {
		executorCalls.Add(1)
		return &IndexResult{}, nil
	})
	coordinator.guard = func() (bool, error) {
		guardCalls.Add(1)
		return true, nil
	}
	idx.attachRepositoryMutationCoordinator(coordinator)
	watcher, err := NewGitWatcher(repoPath, idx, zap.NewNop())
	require.NoError(t, err)
	t.Cleanup(func() { _ = watcher.Stop() })
	watcher.lastSHA = oldSHA

	var drainedPaths []int
	watcher.drained = func(paths int) { drainedPaths = append(drainedPaths, paths) }

	watcher.reconcile("guarded-zero-patch-first")
	for range 100 {
		watcher.reconcile("guarded-zero-patch-replay")
	}

	watcher.mu.Lock()
	assert.Equal(t, firstSHA, watcher.lastSHA)
	watcher.mu.Unlock()
	assert.Zero(t, executorCalls.Load(), "the dedicated-corpus guard must suppress changed-path mutation")
	assert.Equal(t, int32(2), guardCalls.Load(), "the first transition must guard its batch and freshness tail exactly once")
	assert.Equal(t, []int{0}, drainedPaths, "one guarded transition must emit exactly one zero-patch reconcile")
	assert.Empty(t, store.snapshotStates(), "the dedicated-corpus guard must still suppress the freshness write")

	require.NoError(t, os.WriteFile(filepath.Join(repoPath, "main.go"), []byte("package main\n\nfunc Second() {}\n"), 0o644))
	runGitWatcherGit(t, repoPath, "add", "main.go")
	runGitWatcherGit(t, repoPath, "commit", "-m", "second change")
	secondSHA := runGitWatcherGit(t, repoPath, "rev-parse", "HEAD")
	require.NotEqual(t, firstSHA, secondSHA)
	watcher.reconcile("guarded-zero-patch-second")

	watcher.mu.Lock()
	assert.Equal(t, secondSHA, watcher.lastSHA)
	watcher.mu.Unlock()
	assert.Equal(t, int32(4), guardCalls.Load(), "a genuinely later transition must run through both guards")
	assert.Equal(t, []int{0, 0}, drainedPaths, "a genuinely later ref transition must still reconcile")
}

func TestGitWatcherFinalizationErrorDoesNotAcknowledgeTransition(t *testing.T) {
	repoPath, oldSHA := initGitWatcherRepo(t)
	runGitWatcherGit(t, repoPath, "commit", "--allow-empty", "-m", "empty")
	newSHA := runGitWatcherGit(t, repoPath, "rev-parse", "HEAD")
	require.NotEqual(t, oldSHA, newSHA)

	idx := gitWatcherBoundaryIndexer(t, graph.New(), repoPath, "repo")
	coordinator := newRepositoryMutationCoordinator(nil)
	coordinator.guard = func() (bool, error) {
		return false, errors.New("intentional finalization failure")
	}
	idx.attachRepositoryMutationCoordinator(coordinator)
	watcher, err := NewGitWatcher(repoPath, idx, zap.NewNop())
	require.NoError(t, err)
	t.Cleanup(func() { _ = watcher.Stop() })
	watcher.lastSHA = oldSHA

	var drains atomic.Int32
	watcher.drained = func(int) { drains.Add(1) }
	watcher.reconcile("finalization-error")

	watcher.mu.Lock()
	assert.Equal(t, oldSHA, watcher.lastSHA, "a failed finalization must retain the retry baseline")
	watcher.mu.Unlock()
	assert.Zero(t, drains.Load(), "a failed transition must not emit a successful reconcile")

	coordinator.mu.Lock()
	coordinator.guard = func() (bool, error) { return true, nil }
	coordinator.mu.Unlock()
	watcher.reconcile("finalization-retry")
	watcher.mu.Lock()
	assert.Equal(t, newSHA, watcher.lastSHA, "the retained transition must succeed on retry")
	watcher.mu.Unlock()
	assert.Equal(t, int32(1), drains.Load())
}

func BenchmarkGitWatcherAcknowledgedZeroPatchReplay(b *testing.B) {
	repoPath, oldSHA := initGitWatcherRepo(b)
	require.NoError(b, os.WriteFile(filepath.Join(repoPath, "main.go"), []byte("package main\n\nfunc Changed() {}\n"), 0o644))
	runGitWatcherGit(b, repoPath, "add", "main.go")
	runGitWatcherGit(b, repoPath, "commit", "-m", "changed")

	idx := gitWatcherBoundaryIndexer(b, graph.New(), repoPath, "repo")
	coordinator := newRepositoryMutationCoordinator(nil)
	coordinator.guard = func() (bool, error) { return true, nil }
	idx.attachRepositoryMutationCoordinator(coordinator)
	watcher, err := NewGitWatcher(repoPath, idx, zap.NewNop())
	require.NoError(b, err)
	b.Cleanup(func() { _ = watcher.Stop() })
	watcher.lastSHA = oldSHA

	var drains atomic.Int32
	watcher.drained = func(int) { drains.Add(1) }
	b.ResetTimer()
	for range b.N {
		watcher.reconcile("acknowledged-zero-patch-replay")
	}
	b.StopTimer()
	require.Equal(b, int32(1), drains.Load(), "the transition must be emitted once across all replay attempts")
}
