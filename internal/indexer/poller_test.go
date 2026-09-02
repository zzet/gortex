package indexer

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/search"
)

// TestPollInterval_ScalesWithProjectSize is the core contract of the
// adaptive poller: a small repo polls often, a large repo polls
// rarely. The interval must be monotonically non-decreasing in the
// node count so adding code never makes the fallback more aggressive.
func TestPollInterval_ScalesWithProjectSize(t *testing.T) {
	small := pollInterval(100)             // a tiny service
	medium := pollInterval(20 * 1000)      // a mid-size repo
	large := pollInterval(500 * 1000)      // a large monorepo
	huge := pollInterval(50 * 1000 * 1000) // absurdly large

	assert.LessOrEqual(t, small, medium,
		"a small repo must not poll less often than a medium one")
	assert.LessOrEqual(t, medium, large,
		"a medium repo must not poll less often than a large one")
	assert.Less(t, small, large,
		"a small repo must poll strictly more often than a large one")
	assert.Equal(t, huge, large,
		"the interval saturates — beyond the ceiling, more nodes don't widen it")
}

// TestPollInterval_Bounds proves the interval is clamped: it never
// drops below the floor (so the fallback can't become a hot loop on a
// tiny repo) and never rises above the ceiling (so a huge repo is
// still swept periodically rather than effectively never).
func TestPollInterval_Bounds(t *testing.T) {
	cases := []struct {
		name      string
		nodeCount int
	}{
		{"empty_repo", 0},
		{"negative_guard", -5},
		{"one_node", 1},
		{"tiny", 50},
		{"mid", 100 * 1000},
		{"enormous", 1 << 30},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := pollInterval(tc.nodeCount)
			assert.GreaterOrEqual(t, d, pollIntervalMin,
				"interval must not drop below the floor")
			assert.LessOrEqual(t, d, pollIntervalMax,
				"interval must not rise above the ceiling")
		})
	}
}

// TestPollInterval_DerivedFromRealSignal checks the interval is a
// genuine function of the indexed node count — wiring newPoller to a
// graph with more nodes must produce an interval at least as long as
// one wired to a near-empty graph.
func TestPollInterval_DerivedFromRealSignal(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "main.go"), "package main\n\nfunc Only() {}\n")

	g := graph.New()
	idx := newTestIndexer(g)
	idx.SetRootPath(dir)
	_, err := idx.Index(dir)
	require.NoError(t, err)

	w, err := NewWatcher(idx, config.WatchConfig{Enabled: true, DebounceMs: 10}, zap.NewNop())
	require.NoError(t, err)

	p := newPoller(w, idx, zap.NewNop())
	// A near-empty repo sits at (or very close to) the floor.
	assert.Equal(t, pollInterval(g.NodeCount()), p.interval,
		"poller interval must be derived from the indexed node count")
	assert.GreaterOrEqual(t, p.interval, pollIntervalMin)
}

// TestPoller_DetectsFilesystemChangeMissedByFsnotify proves the
// fallback works: a tracked file is modified on disk and the poll
// cycle — not fsnotify — re-indexes it. The poll is driven directly
// so the test does not depend on the (deliberately long) adaptive
// interval elapsing.
func TestPoller_DetectsFilesystemChangeMissedByFsnotify(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "main.go")
	writeTestFile(t, path, "package main\n\nfunc Before() {}\n")

	g := graph.New()
	idx := newTestIndexer(g)
	idx.search = search.NewNull()
	idx.SetRootPath(dir)
	_, err := idx.Index(dir)
	require.NoError(t, err)
	require.NotEmpty(t, g.FindNodesByName("Before"))

	w, err := NewWatcher(idx, config.WatchConfig{Enabled: true, DebounceMs: 10}, zap.NewNop())
	require.NoError(t, err)
	p := newPoller(w, idx, zap.NewNop())

	// Rewrite the file with an mtime strictly after the indexed one.
	// This is the change a missed fsnotify event would have left
	// invisible to the graph.
	future := time.Now().Add(2 * time.Second)
	writeTestFile(t, path, "package main\n\nfunc After() {}\n")
	require.NoError(t, os.Chtimes(path, future, future))

	// One poll cycle must notice the advanced mtime and re-index.
	p.poll()

	assert.Empty(t, g.FindNodesByName("Before"),
		"the stale symbol must be evicted by the poll cycle")
	assert.NotEmpty(t, g.FindNodesByName("After"),
		"the poll cycle must re-index a file fsnotify missed")
}

// TestPoller_DetectsDeletedFileMissedByFsnotify covers the delete
// half of the filesystem fallback: a tracked file vanishes from disk
// and the poll cycle evicts it even though no fsnotify remove event
// arrived.
func TestPoller_DetectsDeletedFileMissedByFsnotify(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "gone.go")
	writeTestFile(t, path, "package main\n\nfunc Gone() {}\n")

	g := graph.New()
	idx := newTestIndexer(g)
	idx.search = search.NewNull()
	idx.SetRootPath(dir)
	_, err := idx.Index(dir)
	require.NoError(t, err)
	require.NotEmpty(t, g.FindNodesByName("Gone"))

	w, err := NewWatcher(idx, config.WatchConfig{Enabled: true, DebounceMs: 10}, zap.NewNop())
	require.NoError(t, err)
	p := newPoller(w, idx, zap.NewNop())

	require.NoError(t, os.Remove(path))
	p.poll()

	assert.Empty(t, g.FindNodesByName("Gone"),
		"the poll cycle must evict a file deleted while fsnotify missed it")
}

// TestPoller_DetectsGitHeadMoveMissedByFsnotify is the end-to-end
// proof for the git-HEAD half of the fallback: HEAD moves to a branch
// with different file content and the poll cycle reconciles the graph
// to the new commit without the GitWatcher's fsnotify watch firing.
func TestPoller_DetectsGitHeadMoveMissedByFsnotify(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git binary not available in PATH")
	}

	repoDir := t.TempDir()
	runGit(t, repoDir, "init", "-q", "-b", "main")
	runGit(t, repoDir, "config", "user.email", "test@example.com")
	runGit(t, repoDir, "config", "user.name", "Test")
	runGit(t, repoDir, "config", "commit.gpgsign", "false")

	writeFile(t, filepath.Join(repoDir, "a.go"), "package main\nfunc Alpha() {}\n")
	runGit(t, repoDir, "add", ".")
	runGit(t, repoDir, "commit", "-q", "-m", "main: Alpha")

	// Build a feature branch that replaces a.go with b.go.
	runGit(t, repoDir, "checkout", "-q", "-b", "feature")
	require.NoError(t, os.Remove(filepath.Join(repoDir, "a.go")))
	writeFile(t, filepath.Join(repoDir, "b.go"), "package main\nfunc Beta() {}\n")
	runGit(t, repoDir, "add", "-A")
	runGit(t, repoDir, "commit", "-q", "-m", "feature: Beta")
	runGit(t, repoDir, "checkout", "-q", "main")

	g := graph.New()
	idx := New(g, newTestRegistry(), config.IndexConfig{Workers: 1}, zap.NewNop())
	idx.search = search.NewNull()
	idx.SetRootPath(repoDir)
	_, err := idx.IndexCtx(testCtx(), repoDir)
	require.NoError(t, err)
	require.NotEmpty(t, g.GetFileNodes("a.go"))
	require.Empty(t, g.GetFileNodes("b.go"))

	w, err := NewWatcher(idx, config.WatchConfig{Enabled: true, DebounceMs: 10}, zap.NewNop())
	require.NoError(t, err)
	// newPoller records the HEAD SHA at construction. After it,
	// switch branches — the poll cycle sees the SHA differ.
	p := newPoller(w, idx, zap.NewNop())
	require.NotEmpty(t, p.lastSHA, "poller must capture the starting HEAD SHA")

	runGit(t, repoDir, "checkout", "-q", "feature")
	p.poll()

	assert.Empty(t, g.GetFileNodes("a.go"),
		"after the poll reconcile, Alpha's file must be evicted")
	assert.NotEmpty(t, g.GetFileNodes("b.go"),
		"the poll cycle must index the feature branch's Beta")
}

// TestPoller_RespectsWatcherDisableKnob verifies the poller honours
// the per-repo watcher-disable knob: when WatchConfig.Enabled is
// false, Start must not create the alongside-fsnotify poller. This
// is not the whole story on a slow mount, where the degraded-path
// pollers still start unconditionally regardless of Enabled — see
// TestWatcher_ShippedDefaultStillWatches and the slow-mount tests.
func TestPoller_RespectsWatcherDisableKnob(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "main.go"), "package main\n\nfunc Main() {}\n")

	g := graph.New()
	idx := newTestIndexer(g)
	idx.SetRootPath(dir)
	_, err := idx.Index(dir)
	require.NoError(t, err)

	w, err := NewWatcher(idx, config.WatchConfig{Enabled: false, DebounceMs: 10}, zap.NewNop())
	require.NoError(t, err)
	require.NoError(t, w.Start([]string{dir}))
	t.Cleanup(func() { _ = w.Stop() })

	assert.Nil(t, w.poller,
		"a watcher started with Enabled=false must not run an adaptive poller")
}

// TestPoller_StartedAndStoppedWithWatcher proves the poller shares
// the watcher lifecycle: Start brings it up, Stop tears it down, and
// the whole sequence is deadlock-free.
func TestPoller_StartedAndStoppedWithWatcher(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "main.go"), "package main\n\nfunc Main() {}\n")

	g := graph.New()
	idx := newTestIndexer(g)
	idx.SetRootPath(dir)
	_, err := idx.Index(dir)
	require.NoError(t, err)

	w, err := NewWatcher(idx, config.WatchConfig{Enabled: true, DebounceMs: 10}, zap.NewNop())
	require.NoError(t, err)
	require.NoError(t, w.Start([]string{dir}))
	require.NotNil(t, w.poller, "an enabled watcher must run an adaptive poller")

	done := make(chan struct{})
	go func() {
		_ = w.Stop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("watcher Stop deadlocked tearing down the poller")
	}
}

// TestPoller_StopIdempotent guards the teardown path: Stop must be
// safe whether Start launched the loop, was a no-op (no indexer /
// root), or Stop was already called once.
func TestPoller_StopIdempotent(t *testing.T) {
	// Inert poller — no indexer, Start is a no-op.
	inert := &Poller{done: make(chan struct{})}
	inert.Start()
	inert.Stop()
	inert.Stop() // second call must not panic or block

	// Live poller — Start launches the loop, Stop joins it.
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "main.go"), "package main\n\nfunc Main() {}\n")
	g := graph.New()
	idx := newTestIndexer(g)
	idx.SetRootPath(dir)
	_, err := idx.Index(dir)
	require.NoError(t, err)

	w, err := NewWatcher(idx, config.WatchConfig{Enabled: true}, zap.NewNop())
	require.NoError(t, err)
	p := newPoller(w, idx, zap.NewNop())
	p.Start()

	done := make(chan struct{})
	go func() {
		p.Stop()
		p.Stop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("poller Stop deadlocked")
	}
}

// TestPoller_NotifyFileTriggersReconcile verifies touching the notify file
// forces an immediate reconcile via the fast notify loop, independent of the
// (much longer) adaptive poll interval.
func TestPoller_NotifyFileTriggersReconcile(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "main.go"), "package main\n\nfunc Main() {}\n")

	g := graph.New()
	idx := newTestIndexer(g)
	idx.SetRootPath(dir)
	_, err := idx.Index(dir)
	require.NoError(t, err)

	w, err := NewWatcher(idx, config.WatchConfig{Enabled: true, DebounceMs: 10}, zap.NewNop())
	require.NoError(t, err)
	p := newPoller(w, idx, zap.NewNop())
	require.NotEmpty(t, p.notifyPath, "notify path must default under .gortex")
	require.NoError(t, os.MkdirAll(filepath.Dir(p.notifyPath), 0o755))
	require.NoError(t, os.WriteFile(p.notifyPath, []byte("x"), 0o644))

	swept := make(chan int, 8)
	p.swept = func(n int) { swept <- n }
	p.Start()
	defer p.Stop()

	// Touch the notify file with a strictly later mtime.
	time.Sleep(20 * time.Millisecond)
	future := time.Now().Add(2 * time.Second)
	require.NoError(t, os.Chtimes(p.notifyPath, future, future))

	select {
	case <-swept:
		// the notify loop forced a reconcile within the fast cadence
	case <-time.After(3 * time.Second):
		t.Fatal("notify-file touch did not trigger a reconcile within 3s")
	}
}

// TestPoller_SweepHookReportsWork exercises the swept test hook and
// confirms a poll cycle reports the number of files it re-dispatched
// — the same count the production logger emits.
func TestPoller_SweepHookReportsWork(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "main.go")
	writeTestFile(t, path, "package main\n\nfunc Before() {}\n")

	g := graph.New()
	idx := newTestIndexer(g)
	idx.search = search.NewNull()
	idx.SetRootPath(dir)
	_, err := idx.Index(dir)
	require.NoError(t, err)

	w, err := NewWatcher(idx, config.WatchConfig{Enabled: true, DebounceMs: 10}, zap.NewNop())
	require.NoError(t, err)
	p := newPoller(w, idx, zap.NewNop())

	var swept int
	p.swept = func(n int) { swept = n }

	// No change yet — a clean sweep reports zero work.
	p.poll()
	assert.Equal(t, 0, swept, "a quiet poll cycle must report no work")

	// Now mutate the file and sweep again.
	future := time.Now().Add(2 * time.Second)
	writeTestFile(t, path, "package main\n\nfunc After() {}\n")
	require.NoError(t, os.Chtimes(path, future, future))
	p.poll()
	assert.Equal(t, 1, swept, "the poll cycle must report the one file it re-indexed")
}

// TestPoller_ConcurrentPollsAreSingleFlight proves overlapping timer/notify
// requests never block behind a scan. They dirty one generation and return;
// the active owner performs exactly one coalesced follow-up.
func TestPoller_ConcurrentPollsAreSingleFlight(t *testing.T) {
	idx := newTestIndexer(graph.New())
	idx.SetRootPath(t.TempDir())
	p := newPoller(nil, idx, zap.NewNop())

	firstEntered := make(chan struct{})
	releaseFirst := make(chan struct{})
	var hookMu sync.Mutex
	hookCalls := 0
	p.swept = func(int) {
		hookMu.Lock()
		hookCalls++
		call := hookCalls
		hookMu.Unlock()
		if call == 1 {
			close(firstEntered)
			<-releaseFirst
		}
	}

	ownerDone := make(chan struct{})
	go func() {
		p.poll()
		close(ownerDone)
	}()
	select {
	case <-firstEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("first poll did not reach the scan hook")
	}

	const overlapping = 16
	var callers sync.WaitGroup
	callers.Add(overlapping)
	for i := 0; i < overlapping; i++ {
		go func() {
			defer callers.Done()
			p.poll()
		}()
	}
	callersDone := make(chan struct{})
	go func() {
		callers.Wait()
		close(callersDone)
	}()
	select {
	case <-callersDone:
		// Dirty requests returned without waiting for the active scan.
	case <-time.After(2 * time.Second):
		t.Fatal("overlapping poll callers queued behind the active scan")
	}

	hookMu.Lock()
	assert.Equal(t, 1, hookCalls, "no follow-up may overlap the active scan")
	hookMu.Unlock()
	close(releaseFirst)
	select {
	case <-ownerDone:
	case <-time.After(5 * time.Second):
		t.Fatal("poll owner did not finish its coalesced follow-up")
	}

	hookMu.Lock()
	assert.Equal(t, 2, hookCalls, "all overlapping requests must collapse into one follow-up sweep")
	hookMu.Unlock()
	p.pollMu.Lock()
	assert.False(t, p.pollRunning, "singleflight ownership must clear after the follow-up")
	p.pollMu.Unlock()
}

func TestPoller_SweepUnionsGitAndFilesystemPathsIntoOneBatch(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git binary not available in PATH")
	}
	repoDir := t.TempDir()
	runGit(t, repoDir, "init", "-q", "-b", "main")
	runGit(t, repoDir, "config", "user.email", "test@example.com")
	runGit(t, repoDir, "config", "user.name", "Test")
	runGit(t, repoDir, "config", "commit.gpgsign", "false")

	aPath := filepath.Join(repoDir, "a.go")
	bPath := filepath.Join(repoDir, "b.go")
	writeFile(t, aPath, "package main\nfunc Alpha() {}\n")
	writeFile(t, bPath, "package main\nfunc Beta() {}\n")
	runGit(t, repoDir, "add", ".")
	runGit(t, repoDir, "commit", "-q", "-m", "initial")

	g := graph.New()
	idx := New(g, newTestRegistry(), config.IndexConfig{Workers: 1}, zap.NewNop())
	idx.search = search.NewNull()
	idx.SetRootPath(repoDir)
	_, err := idx.IndexCtx(testCtx(), repoDir)
	require.NoError(t, err)
	w, err := NewWatcher(idx, config.WatchConfig{Enabled: true, DebounceMs: 10}, zap.NewNop())
	require.NoError(t, err)
	p := newPoller(w, idx, zap.NewNop())

	batchCalls := 0
	var submitted []string
	w.batchReindex = func(paths []string) (*IndexResult, error) {
		batchCalls++
		submitted = append([]string(nil), paths...)
		return &IndexResult{}, nil
	}

	// A clean sweep has no work and must not call the batch coordinator.
	p.poll()
	assert.Equal(t, 0, batchCalls)

	future := time.Now().Add(2 * time.Second)
	writeFile(t, aPath, "package main\nfunc AlphaChanged() {}\n")
	writeFile(t, bPath, "package main\nfunc BetaChanged() {}\n")
	require.NoError(t, os.Chtimes(aPath, future, future))
	require.NoError(t, os.Chtimes(bPath, future, future))
	// Commit only a.go. It is visible to both Git and the mtime sweep, while
	// b.go is filesystem-only; the union must contain each path exactly once.
	runGit(t, repoDir, "add", "a.go")
	runGit(t, repoDir, "commit", "-q", "-m", "change a")

	p.poll()
	require.Equal(t, 1, batchCalls, "one sweep must submit one repository batch")
	assert.Equal(t, []string{aPath, bPath}, submitted, "Git/filesystem overlap must be sorted and deduplicated")
	head, err := pollerHeadSHA(repoDir)
	require.NoError(t, err)
	p.mu.Lock()
	settled := p.lastSHA
	p.mu.Unlock()
	assert.Equal(t, head, settled, "successful unified reconciliation advances Git freshness")
}

func TestPoller_GitSHAAdvancesOnlyAfterSuccessfulBatch(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git binary not available in PATH")
	}
	repoDir := t.TempDir()
	runGit(t, repoDir, "init", "-q", "-b", "main")
	runGit(t, repoDir, "config", "user.email", "test@example.com")
	runGit(t, repoDir, "config", "user.name", "Test")
	runGit(t, repoDir, "config", "commit.gpgsign", "false")

	path := filepath.Join(repoDir, "main.go")
	writeFile(t, path, "package main\nfunc Before() {}\n")
	runGit(t, repoDir, "add", ".")
	runGit(t, repoDir, "commit", "-q", "-m", "before")

	g := graph.New()
	idx := New(g, newTestRegistry(), config.IndexConfig{Workers: 1}, zap.NewNop())
	idx.search = search.NewNull()
	idx.SetRootPath(repoDir)
	_, err := idx.IndexCtx(testCtx(), repoDir)
	require.NoError(t, err)
	w, err := NewWatcher(idx, config.WatchConfig{Enabled: true, DebounceMs: 10}, zap.NewNop())
	require.NoError(t, err)
	p := newPoller(w, idx, zap.NewNop())
	oldSHA := p.lastSHA

	future := time.Now().Add(2 * time.Second)
	writeFile(t, path, "package main\nfunc After() {}\n")
	require.NoError(t, os.Chtimes(path, future, future))
	runGit(t, repoDir, "add", "main.go")
	runGit(t, repoDir, "commit", "-q", "-m", "after")
	newSHA, err := pollerHeadSHA(repoDir)
	require.NoError(t, err)

	calls := 0
	w.batchReindex = func(paths []string) (*IndexResult, error) {
		calls++
		require.Equal(t, []string{path}, paths)
		switch calls {
		case 1:
			return nil, errors.New("transient batch failure")
		case 2:
			return &IndexResult{FailedFiles: []string{path}}, nil
		default:
			return &IndexResult{}, nil
		}
	}

	p.poll()
	p.mu.Lock()
	first := p.lastSHA
	p.mu.Unlock()
	assert.Equal(t, oldSHA, first, "an errored batch must leave the Git range retryable")
	p.poll()
	p.mu.Lock()
	second := p.lastSHA
	p.mu.Unlock()
	assert.Equal(t, oldSHA, second, "a partial batch must leave the Git range retryable")
	closedCoordinator := newRepositoryMutationCoordinator(nil)
	require.NoError(t, closedCoordinator.closeAndWait(testCtx()))
	idx.attachRepositoryMutationCoordinator(closedCoordinator)
	p.poll()
	p.mu.Lock()
	third := p.lastSHA
	p.mu.Unlock()
	assert.Equal(t, oldSHA, third, "failed lane admission must leave the Git range retryable")

	idx.attachRepositoryMutationCoordinator(newRepositoryMutationCoordinator(nil))
	p.poll()
	p.mu.Lock()
	fourth := p.lastSHA
	p.mu.Unlock()
	assert.Equal(t, newSHA, fourth, "Git freshness advances only after the batch and freshness tail succeed")
	assert.Equal(t, 4, calls, "the same range must be retried until reconciliation and finalization succeed")
}

func TestPoller_EmptyGitDiffAdvancesWithoutBatch(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git binary not available in PATH")
	}
	repoDir := t.TempDir()
	runGit(t, repoDir, "init", "-q", "-b", "main")
	runGit(t, repoDir, "config", "user.email", "test@example.com")
	runGit(t, repoDir, "config", "user.name", "Test")
	runGit(t, repoDir, "config", "commit.gpgsign", "false")
	writeFile(t, filepath.Join(repoDir, "main.go"), "package main\nfunc Main() {}\n")
	runGit(t, repoDir, "add", ".")
	runGit(t, repoDir, "commit", "-q", "-m", "initial")

	idx := newTestIndexer(graph.New())
	idx.SetRootPath(repoDir)
	_, err := idx.Index(repoDir)
	require.NoError(t, err)
	w, err := NewWatcher(idx, config.WatchConfig{Enabled: true, DebounceMs: 10}, zap.NewNop())
	require.NoError(t, err)
	p := newPoller(w, idx, zap.NewNop())

	runGit(t, repoDir, "commit", "-q", "--allow-empty", "-m", "empty")
	newSHA, err := pollerHeadSHA(repoDir)
	require.NoError(t, err)
	batchCalls := 0
	w.batchReindex = func([]string) (*IndexResult, error) {
		batchCalls++
		return &IndexResult{}, nil
	}

	p.poll()
	assert.Equal(t, 0, batchCalls, "a successful zero-path diff must not reindex")
	p.mu.Lock()
	settled := p.lastSHA
	p.mu.Unlock()
	assert.Equal(t, newSHA, settled, "a successful zero-path diff advances Git freshness")
}

func TestPoller_GitFinalizationWaitsForLaneAndUsesReplacementIndexer(t *testing.T) {
	repoPath, oldSHA := initGitWatcherRepo(t)
	runGitWatcherGit(t, repoPath, "commit", "--allow-empty", "-m", "empty")
	newSHA := runGitWatcherGit(t, repoPath, "rev-parse", "HEAD")
	require.NotEqual(t, oldSHA, newSHA)

	oldStore := &gitWatcherStateStore{Store: graph.New()}
	newStore := &gitWatcherStateStore{Store: graph.New()}
	oldIndexer := gitWatcherBoundaryIndexer(t, oldStore, repoPath, "repo")
	newIndexer := gitWatcherBoundaryIndexer(t, newStore, repoPath, "repo")
	coordinator := newRepositoryMutationCoordinator(nil)
	oldIndexer.attachRepositoryMutationCoordinator(coordinator)
	newIndexer.attachRepositoryMutationCoordinator(coordinator)

	watcher, err := NewWatcher(oldIndexer, config.WatchConfig{Enabled: true, DebounceMs: 10}, zap.NewNop())
	require.NoError(t, err)
	poller := newPoller(watcher, oldIndexer, zap.NewNop())
	poller.lastSHA = oldSHA

	resolvedCurrent := make(chan struct{}, 1)
	watcher.currentIndexer = func() *Indexer {
		resolvedCurrent <- struct{}{}
		return newIndexer
	}

	laneEntered := make(chan struct{})
	releaseLane := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseLane) }) }
	defer release()
	holderDone := make(chan error, 1)
	go func() {
		holderDone <- oldIndexer.coordinateRepositoryMutation(testCtx(), func() error {
			close(laneEntered)
			<-releaseLane
			return nil
		})
	}()
	<-laneEntered

	finalizeDone := make(chan error, 1)
	go func() {
		finalizeDone <- poller.finalizeGitHead(pollGitObservation{
			oldSHA: oldSHA, newSHA: newSHA, diffSucceeded: true,
		})
	}()
	select {
	case err := <-finalizeDone:
		t.Fatalf("freshness tail bypassed the occupied repository lane: %v", err)
	case <-resolvedCurrent:
		t.Error("current Indexer was resolved before repository-lane admission")
	case <-time.After(100 * time.Millisecond):
	}
	poller.mu.Lock()
	assert.Equal(t, oldSHA, poller.lastSHA)
	poller.mu.Unlock()
	assert.Empty(t, oldStore.snapshotStates())
	assert.Empty(t, newStore.snapshotStates())

	release()
	require.NoError(t, <-holderDone)
	require.NoError(t, <-finalizeDone)
	select {
	case <-resolvedCurrent:
	case <-time.After(time.Second):
		t.Fatal("freshness tail did not resolve the current Indexer after admission")
	}
	poller.mu.Lock()
	assert.Equal(t, newSHA, poller.lastSHA)
	poller.mu.Unlock()
	assert.Empty(t, oldStore.snapshotStates(), "retired Indexer must not be restamped")
	require.Len(t, newStore.snapshotStates(), 1, "replacement Indexer must receive the restamp")
}

func TestPoller_MultiWatcherUsesCurrentIndexerFileMtimesAfterReplacement(t *testing.T) {
	dir := t.TempDir()
	retiredPath := filepath.Join(dir, "retired.go")
	currentPath := filepath.Join(dir, "current.go")
	writeTestFile(t, retiredPath, "package main\nfunc Retired() {}\n")
	writeTestFile(t, currentPath, "package main\nfunc Current() {}\n")

	const prefix = "repo"
	g := graph.New()
	oldIndexer := newTestIndexer(g)
	oldIndexer.SetRootPath(dir)
	oldIndexer.mtimeMu.Lock()
	oldIndexer.fileMtimes = map[string]int64{"retired.go": 1}
	oldIndexer.mtimeMu.Unlock()

	multi := NewMultiIndexer(g, newTestRegistry(), nil, nil, zap.NewNop())
	multi.repos[prefix] = &RepoMetadata{RepoPrefix: prefix, RootPath: dir}
	multi.indexers[prefix] = oldIndexer
	multiWatcher, err := NewMultiWatcher(multi, map[string]config.WatchConfig{
		prefix: {DebounceMs: 10},
	}, zap.NewNop())
	require.NoError(t, err)
	watcher := multiWatcher.watchers[prefix]
	require.NotNil(t, watcher)
	poller := newPoller(watcher, oldIndexer, zap.NewNop())

	currentIndexer := newTestIndexer(g)
	currentIndexer.SetRootPath(dir)
	currentIndexer.mtimeMu.Lock()
	currentIndexer.fileMtimes = map[string]int64{"current.go": 1}
	currentIndexer.mtimeMu.Unlock()
	currentIndexer.attachRepositoryMutationCoordinator(multi.repositoryMutationCoordinator(prefix))
	multi.mu.Lock()
	multi.indexers[prefix] = currentIndexer
	multi.mu.Unlock()

	batchCalls := 0
	var submitted []string
	watcher.batchReindex = func(paths []string) (*IndexResult, error) {
		batchCalls++
		submitted = append([]string(nil), paths...)
		return &IndexResult{}, nil
	}
	poller.poll()

	require.Equal(t, 1, batchCalls)
	assert.Equal(t, []string{currentPath}, submitted,
		"poll discovery must use the replacement Indexer's FileMtimes, not the retired construction-time map")
}

func TestPoller_TransientStatErrorIsPreservedOutsideSharedBatch(t *testing.T) {
	dir := t.TempDir()
	loopPath := filepath.Join(dir, "loop.go")
	if err := os.Symlink("loop.go", loopPath); err != nil {
		t.Skipf("self-referential symlink unavailable on this platform: %v", err)
	}
	_, statErr := os.Stat(loopPath)
	if statErr == nil || os.IsNotExist(statErr) {
		t.Skipf("platform does not expose a deterministic non-ENOENT stat error for a symlink loop: %v", statErr)
	}

	idx := newTestIndexer(graph.New())
	idx.SetRootPath(dir)
	idx.mtimeMu.Lock()
	idx.fileMtimes = map[string]int64{"loop.go": 1}
	idx.mtimeMu.Unlock()
	watcher, err := NewWatcher(idx, config.WatchConfig{Enabled: true, DebounceMs: 10}, zap.NewNop())
	require.NoError(t, err)
	poller := newPoller(watcher, idx, zap.NewNop())

	batchCalls := 0
	var submitted []string
	watcher.batchReindex = func(paths []string) (*IndexResult, error) {
		batchCalls++
		submitted = append([]string(nil), paths...)
		return &IndexResult{}, nil
	}
	poller.poll()
	assert.Equal(t, 0, batchCalls,
		"a transient stat failure alone must preserve the tracked path without entering the shared batch")

	validPath := filepath.Join(dir, "valid.go")
	writeTestFile(t, validPath, "package main\nfunc Valid() {}\n")
	idx.mtimeMu.Lock()
	idx.fileMtimes["valid.go"] = 1
	idx.mtimeMu.Unlock()
	poller.poll()
	require.Equal(t, 1, batchCalls)
	assert.Equal(t, []string{validPath}, submitted,
		"the transiently unreadable path must not poison an unrelated valid batch")
}
