package indexer

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
)

// These controls exercise admission without starting the fsnotify loop.
func inertMutationSetWatcher(t *testing.T, name, content string) (string, *Indexer, *Watcher) {
	t.Helper()
	dir, idx, w := inertTestWatcher(t, name, content)
	w.degradedNoFsnotify = true
	return dir, idx, w
}

func awaitMutationSetTicket(t *testing.T, ticket *MutationTicket) MutationResult {
	t.Helper()
	select {
	case result := <-ticket.Done:
		return result
	case <-time.After(5 * time.Second):
		t.Fatal("mutation set ticket did not finish")
		return MutationResult{}
	}
}

func TestWatcherMutationSetParsesWholeFrontierBeforeResolution(t *testing.T) {
	dir := t.TempDir()
	caller := filepath.Join(dir, "caller.go")
	target := filepath.Join(dir, "target.go")
	writeTestFile(t, caller, "package pair\nfunc Caller(){Old()}\n")
	writeTestFile(t, target, "package pair\nfunc Old(){}\n")
	g := graph.New()
	idx := newTestIndexer(g)
	idx.SetRepoPrefix("repo")
	_, err := idx.Index(dir)
	require.NoError(t, err)
	mi := &MultiIndexer{graph: g, repos: map[string]*RepoMetadata{"repo": {RepoPrefix: "repo", RootPath: dir, FileMtimes: idx.FileMtimes()}}, indexers: map[string]*Indexer{"repo": idx}, logger: zap.NewNop()}
	mw, err := NewMultiWatcher(mi, map[string]config.WatchConfig{"repo": {DebounceMs: 60000}}, zap.NewNop())
	require.NoError(t, err)
	w := mw.watchers["repo"]
	w.degradedNoFsnotify = true
	t.Cleanup(func() { require.NoError(t, w.Stop()) })
	writeTestFile(t, caller, "package pair\nfunc Caller(){Renamed()}\n")
	writeTestFile(t, target, "package pair\nfunc Renamed(){}\n")
	var mu sync.Mutex
	resolveCalls, crossCalls := 0, 0
	targetPresent := false
	idx.incrementalResolveFilesHook = func(files []string) {
		mu.Lock()
		defer mu.Unlock()
		resolveCalls++
		targetPresent = len(g.FindNodesByName("Renamed")) == 1
	}
	idx.incrementalCatchupHook = func(kind string, _ []string) {
		if kind == "cross_repo" {
			mu.Lock()
			crossCalls++
			mu.Unlock()
		}
	}
	// The queued point timer must be taken over, not run before the full set.
	old, err := w.EnqueueFileMutation(context.Background(), caller)
	require.NoError(t, err)
	tickets, err := w.EnqueueFileMutations(context.Background(), []string{caller, target, caller})
	require.NoError(t, err)
	require.Len(t, tickets, 2)
	for _, ticket := range tickets {
		result := awaitMutationSetTicket(t, ticket)
		require.NoError(t, result.Err)
		require.True(t, result.Reindexed)
		require.Equal(t, ticket.Generation, result.RequestedGeneration)
		require.Equal(t, ticket.Generation, result.AppliedGeneration)
	}
	prior := awaitMutationSetTicket(t, old)
	require.NoError(t, prior.Err)
	require.Equal(t, tickets[caller].Generation, prior.AppliedGeneration)
	mu.Lock()
	resolves, cross, present := resolveCalls, crossCalls, targetPresent
	mu.Unlock()
	require.Equal(t, 1, resolves)
	require.Equal(t, 1, cross)
	require.True(t, present, "callee must already be parsed when resolving caller")
	from, to := "repo/caller.go::Caller", "repo/target.go::Renamed"
	found := false
	for _, edge := range g.GetOutEdges(from) {
		if edge.Kind == graph.EdgeCalls && edge.To == to {
			found = true
		}
	}
	require.True(t, found, "final exact call edge must match renamed callee")
	require.Empty(t, g.FindNodesByName("Old"))
}

func TestWatcherMutationSetInvalidAdmissionSchedulesNothing(t *testing.T) {
	dir, _, w := inertMutationSetWatcher(t, "a.go", "package a\nfunc A(){}\n")
	t.Cleanup(func() { require.NoError(t, w.Stop()) })
	valid := filepath.Join(dir, "a.go")
	for _, paths := range [][]string{{valid, filepath.Join(t.TempDir(), "foreign.go")}, {valid, filepath.Join(dir, "unsupported.zzz")}} {
		tickets, err := w.EnqueueFileMutations(context.Background(), paths)
		require.NoError(t, err)
		require.Nil(t, tickets)
		w.mu.Lock()
		require.Empty(t, w.pending)
		require.Empty(t, w.mutationWaiters)
		w.mu.Unlock()
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	tickets, err := w.EnqueueFileMutations(ctx, []string{valid, filepath.Join(dir, "b.go")})
	require.ErrorIs(t, err, context.Canceled)
	require.Nil(t, tickets)
}

func TestMultiWatcherMutationSetMixedOwnersRetainsPointFallback(t *testing.T) {
	a, _, wa := inertMutationSetWatcher(t, "a.go", "package a\nfunc A(){}\n")
	b, _, wb := inertMutationSetWatcher(t, "b.go", "package b\nfunc B(){}\n")
	t.Cleanup(func() { require.NoError(t, wa.Stop()); require.NoError(t, wb.Stop()) })
	mw := &MultiWatcher{multi: &MultiIndexer{repos: map[string]*RepoMetadata{"a": {RootPath: a}, "b": {RootPath: b}}}, watchers: map[string]*Watcher{"a": wa, "b": wb}, started: map[string]bool{"a": true, "b": true}}
	paths := []string{filepath.Join(a, "a.go"), filepath.Join(b, "b.go")}
	tickets, err := mw.EnqueueFileMutations(context.Background(), paths)
	require.NoError(t, err)
	require.Nil(t, tickets)
	for _, path := range paths {
		ticket, err := mw.EnqueueFileMutation(context.Background(), path)
		require.NoError(t, err)
		require.NotNil(t, ticket)
		require.NoError(t, awaitMutationSetTicket(t, ticket).Err)
	}
}

func TestWatcherMutationSetAcceptedWorkOutlivesCallerCancellation(t *testing.T) {
	dir, _, w := inertMutationSetWatcher(t, "a.go", "package a\nfunc A(){}\n")
	b := filepath.Join(dir, "b.go")
	writeTestFile(t, b, "package a\nfunc B(){}\n")
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	w.stormBeforeLock = func() { once.Do(func() { close(entered) }); <-release }
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
		require.NoError(t, w.Stop())
	})
	ctx, cancel := context.WithCancel(context.Background())
	tickets, err := w.EnqueueFileMutations(ctx, []string{filepath.Join(dir, "a.go"), b})
	require.NoError(t, err)
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("set drain not admitted")
	}
	cancel()
	close(release)
	for _, ticket := range tickets {
		require.NoError(t, awaitMutationSetTicket(t, ticket).Err)
	}
}

func TestWatcherMutationSetStopJoinsAcceptedDrain(t *testing.T) {
	dir, _, w := inertMutationSetWatcher(t, "a.go", "package a\nfunc A(){}\n")
	b := filepath.Join(dir, "b.go")
	writeTestFile(t, b, "package a\nfunc B(){}\n")
	entered, release := make(chan struct{}), make(chan struct{})
	w.stormBeforeLock = func() { close(entered); <-release }
	tickets, err := w.EnqueueFileMutations(context.Background(), []string{filepath.Join(dir, "a.go"), b})
	require.NoError(t, err)
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		close(release)
		_ = w.Stop()
		t.Fatal("set did not enter drain")
	}
	stopped := make(chan error, 1)
	go func() { stopped <- w.Stop() }()
	select {
	case err := <-stopped:
		close(release)
		t.Fatalf("stop did not join drain: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	select {
	case err := <-stopped:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("stop failed to join")
	}
	for _, ticket := range tickets {
		result := awaitMutationSetTicket(t, ticket)
		require.ErrorIs(t, result.Err, errWatcherStopped)
	}
}

func TestWatcherMutationSetForcesUnchangedMtimeAndDeletion(t *testing.T) {
	dir, idx, w := inertMutationSetWatcher(t, "removed.go", "package pair\nfunc Removed(){}\n")
	t.Cleanup(func() { require.NoError(t, w.Stop()) })
	changed := filepath.Join(dir, "changed.go")
	removed := filepath.Join(dir, "removed.go")
	writeTestFile(t, changed, "package pair\nfunc Before(){}\n")
	require.NoError(t, idx.IndexFile(changed))
	info, err := os.Stat(changed)
	require.NoError(t, err)
	writeTestFile(t, changed, "package pair\nfunc Afterx(){}\n")
	require.NoError(t, os.Chtimes(changed, info.ModTime(), info.ModTime()))
	require.NoError(t, os.Remove(removed))
	tickets, err := w.EnqueueFileMutations(context.Background(), []string{changed, removed})
	require.NoError(t, err)
	require.Len(t, tickets, 2)
	for _, ticket := range tickets {
		require.NoError(t, awaitMutationSetTicket(t, ticket).Err)
	}
	require.Empty(t, idx.graph.FindNodesByName("Before"))
	require.Empty(t, idx.graph.FindNodesByName("Removed"))
	require.Len(t, idx.graph.FindNodesByName("Afterx"), 1)
}

func TestWatcherMutationSetNewPointOwnsOlderWaiter(t *testing.T) {
	dir, _, w := inertMutationSetWatcher(t, "a.go", "package a\nfunc A(){}\n")
	a, b := filepath.Join(dir, "a.go"), filepath.Join(dir, "b.go")
	writeTestFile(t, b, "package a\nfunc B(){}\n")
	entered, release := make(chan struct{}), make(chan struct{})
	w.stormBeforeLock = func() { close(entered); <-release }
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
		require.NoError(t, w.Stop())
	})
	tickets, err := w.EnqueueFileMutations(context.Background(), []string{a, b})
	require.NoError(t, err)
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("drain did not enter")
	}
	writeTestFile(t, a, "package a\nfunc Newest(){}\n")
	successor, err := w.EnqueueFileMutation(context.Background(), a)
	require.NoError(t, err)
	newest := awaitMutationSetTicket(t, successor)
	require.NoError(t, newest.Err)
	prior := awaitMutationSetTicket(t, tickets[a])
	require.NoError(t, prior.Err)
	require.Equal(t, successor.Generation, prior.AppliedGeneration)
	close(release)
	require.NoError(t, awaitMutationSetTicket(t, tickets[b]).Err)
}

func TestWatcherMutationSetFailedPathHasNoPositiveFanout(t *testing.T) {
	dir, _, w := inertMutationSetWatcher(t, "a.go", "package a\nfunc A(){}\n")
	w.config.DebounceMs = 60000
	t.Cleanup(func() { require.NoError(t, w.Stop()) })
	path := filepath.Join(dir, "a.go")
	ticket, err := w.EnqueueFileMutation(context.Background(), path)
	require.NoError(t, err)
	w.completeStormMutationWaiters(map[string]uint64{path: ticket.Generation}, &IndexResult{FailedFiles: []string{path}}, nil, DerivedFanoutCompleteness{Observed: true, Complete: true})
	result := awaitMutationSetTicket(t, ticket)
	require.Error(t, result.Err)
	require.False(t, result.Reindexed)
	require.False(t, result.DerivedFanout.Observed)
	require.False(t, result.DerivedFanout.Complete)
}
