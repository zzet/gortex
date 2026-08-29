package indexer

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/gitstate"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"github.com/zzet/gortex/internal/search"
)

type topologyWatchFixture struct {
	commonDir    string
	worktreesDir string
	roots        []string
	inventory    *gitstate.FamilyInventory

	inventoryCalls    atomic.Int64
	registrationCalls atomic.Int64
	mu                sync.Mutex
	active            map[string]int
}

func newTopologyWatchFixture(tb testing.TB, worktrees int) *topologyWatchFixture {
	tb.Helper()
	base := tb.TempDir()
	commonDir := filepath.Join(base, "common.git")
	worktreesDir := filepath.Join(commonDir, "worktrees")
	if err := os.MkdirAll(worktreesDir, 0o755); err != nil {
		tb.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(commonDir, "refs", "heads"), 0o755); err != nil {
		tb.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(commonDir, "HEAD"), []byte("ref: refs/heads/main\n"), 0o644); err != nil {
		tb.Fatal(err)
	}

	fixture := &topologyWatchFixture{
		commonDir:    commonDir,
		worktreesDir: worktreesDir,
		roots:        make([]string, 0, worktrees),
		active:       make(map[string]int),
	}
	records := make([]gitstate.WorktreeRecord, 0, worktrees)
	for i := 0; i < worktrees; i++ {
		name := fmt.Sprintf("worktree-%03d", i)
		root := filepath.Join(base, "roots", name)
		admin := filepath.Join(worktreesDir, name)
		if err := os.MkdirAll(root, 0o755); err != nil {
			tb.Fatal(err)
		}
		if err := os.MkdirAll(admin, 0o755); err != nil {
			tb.Fatal(err)
		}
		fixture.roots = append(fixture.roots, root)
		records = append(records, gitstate.WorktreeRecord{
			Path:           root,
			AdminName:      name,
			IsMain:         i == 0,
			RootAccessible: true,
		})
	}
	fixture.inventory = &gitstate.FamilyInventory{
		CommonDir: commonDir,
		GitDir:    commonDir,
		Records:   records,
	}
	return fixture
}

func (fixture *topologyWatchFixture) watcher(index int) *GitWatcher {
	root := fixture.roots[index%len(fixture.roots)]
	return &GitWatcher{
		repoPath:          root,
		logger:            zap.NewNop(),
		debounce:          5 * time.Millisecond,
		gitDir:            fixture.commonDir,
		commonDir:         fixture.commonDir,
		worktreesDir:      fixture.worktreesDir,
		topologyRetryBase: 5 * time.Millisecond,
		topologyRetryMax:  20 * time.Millisecond,
		refPaths:          make(map[string]struct{}),
		refAdd:            func(string) error { return nil },
		topologyPaths:     make(map[string]struct{}),
		worktreeRoots:     make(map[string]struct{}),
		worktreeAdminDirs: make(map[string]struct{}),
		inventory: func(context.Context, string) (*gitstate.FamilyInventory, error) {
			fixture.inventoryCalls.Add(1)
			return fixture.inventory, nil
		},
		topologyAdd: func(path string) error {
			path = filepath.Clean(path)
			fixture.registrationCalls.Add(1)
			fixture.mu.Lock()
			fixture.active[path]++
			fixture.mu.Unlock()
			return nil
		},
		topologyRemove: func(path string) error {
			path = filepath.Clean(path)
			fixture.mu.Lock()
			fixture.active[path]--
			if fixture.active[path] == 0 {
				delete(fixture.active, path)
			}
			fixture.mu.Unlock()
			return nil
		},
	}
}

func (fixture *topologyWatchFixture) activeStats() (unique, duplicates, registrations int) {
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	for _, count := range fixture.active {
		if count > 0 {
			unique++
			registrations += count
		}
		if count > 1 {
			duplicates += count - 1
		}
	}
	return unique, duplicates, registrations
}

func (fixture *topologyWatchFixture) resetActive() {
	fixture.mu.Lock()
	fixture.active = make(map[string]int)
	fixture.mu.Unlock()
}

func newTopologyRegistry() *MultiWatcher {
	return &MultiWatcher{
		watchers:             make(map[string]*Watcher),
		gitWatchers:          make(map[string]*GitWatcher),
		started:              make(map[string]bool),
		startFailures:        make(map[string]string),
		topologyFamilies:     make(map[string]*topologyWatchFamily),
		topologyFamilyByRepo: make(map[string]string),
		topologyDispatches:   make(map[*topologyDispatchEpoch]struct{}),
		forwarders:           make(map[string]*watcherForwarder),
		retiringWatchers:     make(map[string]*watcherRetirement),
		pendingWatcherAdds:   make(map[string]*pendingWatcherAdd),
		logger:               zap.NewNop(),
		events:               make(chan GraphChangeEvent, 1),
		done:                 make(chan struct{}),
	}
}

func installTopologyWatcher(mw *MultiWatcher, prefix string, watcher *GitWatcher) {
	mw.mu.Lock()
	drain, err := mw.installStartedGitWatcherLocked(prefix, watcher)
	mw.mu.Unlock()
	if err != nil {
		panic(err)
	}
	if drain != nil {
		waitTopologyDispatchDrains(drain)
	}
}

func removeTopologyWatcher(mw *MultiWatcher, prefix string) {
	mw.mu.Lock()
	drain := mw.unregisterTopologyWatcherLocked(prefix)
	delete(mw.gitWatchers, prefix)
	mw.mu.Unlock()
	waitTopologyDispatchDrains(drain)
}

func topologyFamilySnapshot(mw *MultiWatcher) (families int, owner string, members int) {
	mw.mu.Lock()
	defer mw.mu.Unlock()
	families = len(mw.topologyFamilies)
	for _, family := range mw.topologyFamilies {
		owner = family.owner
		members = len(family.members)
		break
	}
	return families, owner, members
}

func watcherTopologySnapshot(watcher *GitWatcher) (owned bool, paths int) {
	watcher.mu.Lock()
	defer watcher.mu.Unlock()
	return watcher.topologyOwned, len(watcher.topologyPaths)
}

func makeTopologyWatcherStopSafe(t *testing.T, watcher *GitWatcher) {
	t.Helper()
	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		t.Fatalf("fsnotify.NewWatcher: %v", err)
	}
	watcher.mu.Lock()
	watcher.fsw = fsw
	watcher.done = make(chan struct{})
	watcher.stopped = make(chan struct{})
	watcher.mu.Unlock()
	t.Cleanup(func() { _ = watcher.Stop() })
}

func topologyDispatchEpochSnapshot(t testing.TB, mw *MultiWatcher, prefix string) *topologyDispatchEpoch {
	t.Helper()
	mw.mu.Lock()
	defer mw.mu.Unlock()
	family := mw.topologyFamilies[mw.topologyFamilyByRepo[prefix]]
	if family == nil || family.dispatch == nil {
		t.Fatalf("repo %q has no topology dispatch epoch", prefix)
	}
	return family.dispatch
}

func waitForTopologyEpochInvalidation(t *testing.T, mw *MultiWatcher, epoch *topologyDispatchEpoch) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mw.mu.Lock()
		accepting := epoch != nil && epoch.accepting
		mw.mu.Unlock()
		if !accepting {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("topology dispatch epoch remained accepting")
}

func assertTopologyLifecycleBlocked(t *testing.T, result <-chan error, operation string) {
	t.Helper()
	select {
	case err := <-result:
		t.Fatalf("%s returned before its admitted callback drained: %v", operation, err)
	case <-time.After(25 * time.Millisecond):
	}
}

func waitForTopologyCount(t *testing.T, count *atomic.Int64, want int64) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if count.Load() >= want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("topology callback count = %d, want at least %d", count.Load(), want)
}

func assertOneTopologyCallback(t *testing.T, count *atomic.Int64, debounce time.Duration) {
	t.Helper()
	waitForTopologyCount(t, count, 1)
	time.Sleep(4 * debounce)
	if got := count.Load(); got != 1 {
		t.Fatalf("topology callback count = %d, want exactly 1", got)
	}
}

func TestMultiWatcherTopologyInitialAddFailureRejectsOwner(t *testing.T) {
	fixture := newTopologyWatchFixture(t, 1)
	watcher := fixture.watcher(0)
	makeTopologyWatcherStopSafe(t, watcher)
	originalAdd := watcher.topologyAdd
	fail := true
	watcher.topologyAdd = func(path string) error {
		if fail {
			fail = false
			return errors.New("injected topology registration failure")
		}
		return originalAdd(path)
	}
	mw := newTopologyRegistry()

	mw.mu.Lock()
	drain, err := mw.installStartedGitWatcherLocked("repo-000", watcher)
	mw.mu.Unlock()
	waitTopologyDispatchDrains(drain)
	if err == nil || !strings.Contains(err.Error(), "injected topology registration failure") {
		t.Fatalf("initial topology admission error = %v", err)
	}
	families, owner, members := topologyFamilySnapshot(mw)
	if families != 0 || owner != "" || members != 0 {
		t.Fatalf("failed admission published family = families:%d owner:%q members:%d", families, owner, members)
	}
	mw.mu.Lock()
	_, hasGit := mw.gitWatchers["repo-000"]
	_, hasFamily := mw.topologyFamilyByRepo["repo-000"]
	mw.mu.Unlock()
	if hasGit || hasFamily {
		t.Fatalf("failed admission published watcher membership = git:%t family:%t", hasGit, hasFamily)
	}
	if owned, paths := watcherTopologySnapshot(watcher); owned || paths != 0 {
		t.Fatalf("failed owner state = owned:%t paths:%d, want false/0", owned, paths)
	}
	if unique, duplicates, active := fixture.activeStats(); unique != 0 || duplicates != 0 || active != 0 {
		t.Fatalf("failed admission registrations = %d/%d/%d, want 0/0/0", unique, duplicates, active)
	}

	// The same exact admission is retryable once the external watch service
	// recovers; exactly one owner and one copy of each path are published.
	installTopologyWatcher(mw, "repo-000", watcher)
	families, owner, members = topologyFamilySnapshot(mw)
	if families != 1 || owner != "repo-000" || members != 1 {
		t.Fatalf("retried family = families:%d owner:%q members:%d", families, owner, members)
	}
	unique, duplicates, active := fixture.activeStats()
	expected := 2*len(fixture.roots) + 2
	if unique != expected || duplicates != 0 || active != expected {
		t.Fatalf("retried registrations = %d/%d/%d, want %d/0/%d", unique, duplicates, active, expected, expected)
	}
	removeTopologyWatcher(mw, "repo-000")
}

func TestMultiWatcherTopologyRefreshFailureRetriesWithoutDroppingWatcher(t *testing.T) {
	fixture := newTopologyWatchFixture(t, 1)
	watcher := fixture.watcher(0)
	makeTopologyWatcherStopSafe(t, watcher)
	mw := newTopologyRegistry()
	mw.watchers["repo-000"] = &Watcher{}
	installTopologyWatcher(mw, "repo-000", watcher)
	var topologyCallbacks atomic.Int64
	mw.OnWorktreeChange(func(string, string) { topologyCallbacks.Add(1) })
	// Drain the owner-election synthetic nudge; the assertion below measures
	// only the nudge caused by recovery from the injected degraded state.
	assertOneTopologyCallback(t, &topologyCallbacks, watcher.debounce)
	topologyCallbacks.Store(0)

	target := fixture.roots[0]
	watcher.removeTopologyPath(target)
	originalAdd := watcher.topologyAdd
	var attempts atomic.Int64
	watcher.topologyAdd = func(path string) error {
		if filepath.Clean(path) == filepath.Clean(target) && attempts.Add(1) == 1 {
			return errors.New("injected refresh registration failure")
		}
		return originalAdd(path)
	}

	watcher.refreshTopologyWatches()
	if reason := watcher.topologyDegradedReason(); !strings.Contains(reason, "injected refresh registration failure") {
		t.Fatalf("degraded reason = %q", reason)
	}
	if reason := mw.DegradedReason(); !strings.Contains(reason, "Git worktree topology watcher is degraded") {
		t.Fatalf("multi-watcher degraded reason = %q", reason)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if watcher.topologyDegradedReason() == "" && attempts.Load() >= 2 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if reason := watcher.topologyDegradedReason(); reason != "" {
		t.Fatalf("topology retry did not recover: %s", reason)
	}
	assertOneTopologyCallback(t, &topologyCallbacks, watcher.debounce)
	mw.mu.Lock()
	published := mw.gitWatchers["repo-000"] == watcher && mw.topologyFamilyByRepo["repo-000"] != ""
	mw.mu.Unlock()
	if !published {
		t.Fatal("refresh failure removed the established watcher")
	}
	unique, duplicates, active := fixture.activeStats()
	expected := 2*len(fixture.roots) + 2
	if unique != expected || duplicates != 0 || active != expected {
		t.Fatalf("recovered registrations = %d/%d/%d, want %d/0/%d", unique, duplicates, active, expected, expected)
	}
	removeTopologyWatcher(mw, "repo-000")
}

func TestGitWatcherStopWaitsForTopologyRetry(t *testing.T) {
	fixture := newTopologyWatchFixture(t, 1)
	watcher := fixture.watcher(0)
	makeTopologyWatcherStopSafe(t, watcher)
	mw := newTopologyRegistry()
	installTopologyWatcher(mw, "repo-000", watcher)

	target := fixture.roots[0]
	watcher.removeTopologyPath(target)
	originalAdd := watcher.topologyAdd
	var attempts atomic.Int64
	retryEntered := make(chan struct{})
	retryRelease := make(chan struct{})
	var release sync.Once
	t.Cleanup(func() { release.Do(func() { close(retryRelease) }) })
	watcher.topologyAdd = func(path string) error {
		if filepath.Clean(path) != filepath.Clean(target) {
			return originalAdd(path)
		}
		switch attempts.Add(1) {
		case 1:
			return errors.New("injected retryable topology failure")
		case 2:
			close(retryEntered)
			<-retryRelease
		}
		return originalAdd(path)
	}

	watcher.refreshTopologyWatches()
	select {
	case <-retryEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("topology retry did not enter the injected registration barrier")
	}
	stopped := make(chan error, 1)
	go func() { stopped <- watcher.Stop() }()
	assertTopologyLifecycleBlocked(t, stopped, "GitWatcher.Stop during topology retry")
	release.Do(func() { close(retryRelease) })
	if err := <-stopped; err != nil {
		t.Fatalf("GitWatcher.Stop: %v", err)
	}
	attemptsAfterStop := attempts.Load()
	time.Sleep(4 * watcher.debounce)
	if got := attempts.Load(); got != attemptsAfterStop {
		t.Fatalf("topology registration attempts after Stop = %d, want %d", got, attemptsAfterStop)
	}
	watcher.topologyRetryMu.Lock()
	closing := watcher.topologyRetryClosing
	timer := watcher.topologyRetryTimer
	watcher.topologyRetryMu.Unlock()
	if !closing || timer != nil {
		t.Fatalf("retry teardown = closing:%t timer:%v, want true/nil", closing, timer != nil)
	}
	removeTopologyWatcher(mw, "repo-000")
}

func TestGitWatcherStartRejectsRefWatchFailure(t *testing.T) {
	for _, relativePath := range []string{"HEAD", "packed-refs", filepath.Join("refs", "heads")} {
		relativePath := relativePath
		t.Run(strings.ReplaceAll(relativePath, string(filepath.Separator), "_"), func(t *testing.T) {
			watcher := newGitRefWatcherFixture(t)
			watcher.refAdd = func(path string) error {
				if strings.HasSuffix(filepath.ToSlash(filepath.Clean(path)), filepath.ToSlash(relativePath)) {
					return fmt.Errorf("injected %s registration failure", relativePath)
				}
				return nil
			}

			err := watcher.Start()
			if err == nil || !strings.Contains(err.Error(), "injected "+relativePath+" registration failure") {
				t.Fatalf("GitWatcher.Start error = %v, want injected ref registration failure", err)
			}
			watcher.mu.Lock()
			loopStarted := watcher.loopStarted
			watcher.mu.Unlock()
			if loopStarted {
				t.Fatal("failed ref admission launched the Git watcher loop")
			}
		})
	}
}

func TestGitWatcherRefRefreshFailureUsesSharedRetry(t *testing.T) {
	watcher := newGitRefWatcherFixture(t)
	watcher.refAdd = func(string) error { return nil }
	if err := watcher.Start(); err != nil {
		t.Fatalf("GitWatcher.Start: %v", err)
	}

	watcher.mu.Lock()
	headPath := filepath.Join(watcher.gitDir, "HEAD")
	watcher.mu.Unlock()
	if !watcher.invalidateRefWatch(headPath) {
		t.Fatal("HEAD ref watch was not registered")
	}
	var attempts atomic.Int64
	watcher.refAdd = func(path string) error {
		if filepath.Clean(path) == filepath.Clean(headPath) && attempts.Add(1) == 1 {
			return errors.New("injected runtime HEAD registration failure")
		}
		return nil
	}

	err := watcher.refreshRequiredWatchesChecked()
	watcher.recordTopologyRefresh(err)
	if reason := watcher.topologyDegradedReason(); !strings.Contains(reason, "injected runtime HEAD registration failure") {
		t.Fatalf("degraded reason = %q", reason)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if watcher.topologyDegradedReason() == "" && attempts.Load() >= 2 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if reason := watcher.topologyDegradedReason(); reason != "" {
		t.Fatalf("ref retry did not recover: %s", reason)
	}
	watcher.topologyRetryMu.Lock()
	attemptCount := watcher.topologyRetryAttempts
	timer := watcher.topologyRetryTimer
	watcher.topologyRetryMu.Unlock()
	if attemptCount != 0 || timer != nil {
		t.Fatalf("successful ref refresh did not reset retry state: attempts=%d timer=%v", attemptCount, timer != nil)
	}
}

func TestGitWatcherRefRecoveryReconcilesMissedHeadAdvance(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "repo")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("create repo: %v", err)
	}
	runGitWatcherTestCommand(t, root, "init", "-q")
	trackedPath := filepath.Join(root, "main.go")
	if err := os.WriteFile(trackedPath, []byte("package main\n\nfunc Version() int { return 1 }\n"), 0o644); err != nil {
		t.Fatalf("write initial source: %v", err)
	}
	runGitWatcherTestCommand(t, root, "add", "main.go")
	runGitWatcherTestCommand(t, root,
		"-c", "user.name=Gortex Test", "-c", "user.email=gortex@example.invalid",
		"commit", "-qm", "initial")

	manager, err := config.NewConfigManager(filepath.Join(base, "config.yaml"))
	if err != nil {
		t.Fatalf("NewConfigManager: %v", err)
	}
	manager.Global().Repos = []config.RepoEntry{{Path: root, Name: "ref-recovery"}}
	if err := manager.Global().Save(); err != nil {
		t.Fatalf("save config: %v", err)
	}
	store := graph.New()
	registry := parser.NewRegistry()
	registry.Register(languages.NewGoExtractor())
	multi := NewMultiIndexer(store, registry, search.NewNull(), manager, zap.NewNop())
	if _, err := multi.IndexAll(); err != nil {
		t.Fatalf("IndexAll: %v", err)
	}
	t.Cleanup(func() {
		if err := multi.Close(context.Background()); err != nil {
			t.Errorf("MultiIndexer.Close: %v", err)
		}
	})
	idx := multi.GetIndexer("ref-recovery")
	if idx == nil {
		t.Fatal("indexed repository is unavailable")
	}

	watcher, err := NewGitWatcher(root, idx, zap.NewNop())
	if err != nil {
		t.Fatalf("NewGitWatcher: %v", err)
	}
	watcher.setTopologyOwner(false)
	watcher.debounce = 5 * time.Millisecond
	watcher.topologyRetryBase = 5 * time.Millisecond
	watcher.topologyRetryMax = 20 * time.Millisecond
	watcher.currentIndexer = func() *Indexer { return multi.GetIndexer("ref-recovery") }
	watcher.batchReindex = func(paths []string) (*IndexResult, error) {
		return multi.IncrementalReindexRepo("ref-recovery", paths)
	}
	// The test drives ref registration explicitly; no real fsnotify event can
	// mask the missed-event recovery assertion.
	watcher.refAdd = func(string) error { return nil }
	var topologyNudges atomic.Int64
	watcher.topologyChange = func(string) { topologyNudges.Add(1) }
	reconciled := make(chan struct{})
	var reconcileOnce sync.Once
	watcher.drained = func(int) { reconcileOnce.Do(func() { close(reconciled) }) }
	if err := watcher.Start(); err != nil {
		t.Fatalf("GitWatcher.Start: %v", err)
	}
	t.Cleanup(func() {
		if err := watcher.Stop(); err != nil {
			t.Errorf("GitWatcher.Stop: %v", err)
		}
	})

	watcher.mu.Lock()
	oldSHA := watcher.lastSHA
	headPath := filepath.Join(watcher.gitDir, "HEAD")
	watcher.mu.Unlock()
	if oldSHA == "" {
		t.Fatal("GitWatcher did not capture initial HEAD")
	}
	if !watcher.invalidateRefWatch(headPath) {
		t.Fatal("HEAD watch was not present before injected outage")
	}
	var registrationAttempts atomic.Int64
	watcher.refAdd = func(path string) error {
		if filepath.Clean(path) == filepath.Clean(headPath) && registrationAttempts.Add(1) == 1 {
			return errors.New("injected HEAD watch outage")
		}
		return nil
	}

	if err := os.WriteFile(trackedPath, []byte("package main\n\nfunc Version() int { return 2 }\n"), 0o644); err != nil {
		t.Fatalf("write advanced source: %v", err)
	}
	runGitWatcherTestCommand(t, root, "add", "main.go")
	runGitWatcherTestCommand(t, root,
		"-c", "user.name=Gortex Test", "-c", "user.email=gortex@example.invalid",
		"commit", "-qm", "advance-during-outage")
	newSHA := strings.TrimSpace(runGitWatcherTestOutput(t, root, "rev-parse", "HEAD"))
	if newSHA == "" || newSHA == oldSHA {
		t.Fatalf("advanced HEAD = %q, initial = %q", newSHA, oldSHA)
	}

	err = watcher.refreshRequiredWatchesChecked()
	if err == nil || !strings.Contains(err.Error(), "injected HEAD watch outage") {
		t.Fatalf("outage refresh error = %v", err)
	}
	watcher.recordTopologyRefresh(err)
	select {
	case <-reconciled:
	case <-time.After(3 * time.Second):
		t.Fatal("recovered ref watch did not reconcile the missed HEAD advance")
	}
	watcher.mu.Lock()
	caughtUpSHA := watcher.lastSHA
	watcher.mu.Unlock()
	if caughtUpSHA != newSHA {
		t.Fatalf("recovered watcher HEAD = %q, want %q", caughtUpSHA, newSHA)
	}
	if attempts := registrationAttempts.Load(); attempts < 2 {
		t.Fatalf("HEAD registration attempts = %d, want failure plus retry", attempts)
	}
	time.Sleep(4 * watcher.debounce)
	if got := topologyNudges.Load(); got != 0 {
		t.Fatalf("follower recovery emitted %d family topology nudges, want 0", got)
	}
}

func runGitWatcherTestCommand(t testing.TB, dir string, args ...string) {
	t.Helper()
	_ = runGitWatcherTestOutput(t, dir, args...)
}

func runGitWatcherTestOutput(t testing.TB, dir string, args ...string) string {
	t.Helper()
	commandArgs := append([]string{"-C", dir}, args...)
	command := exec.Command("git", commandArgs...)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, output)
	}
	return string(output)
}

func TestTopologyRetryDelayBacksOffCapsAndCoalesces(t *testing.T) {
	base := 10 * time.Millisecond
	maximum := 40 * time.Millisecond
	for attempt, want := range []time.Duration{base, 2 * base, maximum, maximum, maximum} {
		if got := topologyRetryDelay(base, maximum, uint32(attempt)); got != want {
			t.Fatalf("topologyRetryDelay attempt %d = %s, want %s", attempt, got, want)
		}
	}

	watcher := &GitWatcher{
		topologyOwned:     true,
		topologyRetryBase: time.Hour,
		topologyRetryMax:  time.Hour,
	}
	for i := 0; i < 32; i++ {
		watcher.recordTopologyRefresh(errors.New("persistent registration failure"))
	}
	watcher.topologyRetryMu.Lock()
	attempts := watcher.topologyRetryAttempts
	timer := watcher.topologyRetryTimer
	watcher.topologyRetryMu.Unlock()
	if attempts != 1 || timer == nil {
		t.Fatalf("coalesced retry state = attempts:%d timer:%v, want 1/true", attempts, timer != nil)
	}
	watcher.cancelTopologyRetry()
}

func newGitRefWatcherFixture(t *testing.T) *GitWatcher {
	t.Helper()
	root := t.TempDir()
	command := exec.Command("git", "-C", root, "init", "-q")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, output)
	}
	if err := os.WriteFile(filepath.Join(root, ".git", "packed-refs"), []byte("# pack-refs with: peeled fully-peeled sorted\n"), 0o644); err != nil {
		t.Fatalf("write packed-refs: %v", err)
	}
	watcher, err := NewGitWatcher(root, nil, zap.NewNop())
	if err != nil {
		t.Fatalf("NewGitWatcher: %v", err)
	}
	watcher.setTopologyOwner(false)
	if reason := watcher.topologyDegradedReason(); reason != "" {
		t.Fatalf("pre-start follower demotion degraded watcher: %s", reason)
	}
	watcher.topologyRetryBase = 5 * time.Millisecond
	watcher.topologyRetryMax = 20 * time.Millisecond
	t.Cleanup(func() {
		if err := watcher.Stop(); err != nil {
			t.Errorf("GitWatcher.Stop: %v", err)
		}
	})
	return watcher
}

func BenchmarkGitWatcherRecoveryScheduling(b *testing.B) {
	for _, owner := range []bool{false, true} {
		name := "follower"
		if owner {
			name = "owner"
		}
		b.Run(name, func(b *testing.B) {
			watcher := &GitWatcher{
				logger:        zap.NewNop(),
				debounce:      time.Hour,
				topologyOwned: owner,
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				watcher.scheduleReconcile("git-watch-recovered")
				watcher.scheduleTopologyChange("topology-watch-recovered")
			}
			b.StopTimer()
			watcher.mu.Lock()
			fireTimer := watcher.fireTimer
			topologyTimer := watcher.topologyTimer
			watcher.mu.Unlock()
			if fireTimer == nil {
				b.Fatal("recovery did not retain a per-watcher reconcile timer")
			}
			fireTimer.Stop()
			if owner && topologyTimer == nil {
				b.Fatal("owner recovery did not retain a topology timer")
			}
			if !owner && topologyTimer != nil {
				b.Fatal("follower recovery retained a family topology timer")
			}
			if topologyTimer != nil {
				topologyTimer.Stop()
			}
		})
	}
}

func BenchmarkGitWatcherPersistentFailureCoalescing(b *testing.B) {
	watcher := &GitWatcher{
		topologyOwned:     true,
		topologyRetryBase: time.Hour,
		topologyRetryMax:  time.Hour,
	}
	failure := errors.New("persistent registration failure")
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		watcher.recordTopologyRefresh(failure)
	}
	b.StopTimer()
	watcher.topologyRetryMu.Lock()
	attempts := watcher.topologyRetryAttempts
	timer := watcher.topologyRetryTimer
	watcher.topologyRetryMu.Unlock()
	if attempts != 1 || timer == nil {
		b.Fatalf("coalesced retry state = attempts:%d timer:%v, want 1/true", attempts, timer != nil)
	}
	watcher.cancelTopologyRetry()
}

func TestMultiWatcherTopologyFamilySharesInventoryAndRegistrations(t *testing.T) {
	for _, prefixes := range []int{1, 8, 64} {
		t.Run(fmt.Sprintf("prefixes_%d", prefixes), func(t *testing.T) {
			fixture := newTopologyWatchFixture(t, prefixes)
			mw := newTopologyRegistry()
			watchers := make([]*GitWatcher, 0, prefixes)
			for i := 0; i < prefixes; i++ {
				watcher := fixture.watcher(i)
				watchers = append(watchers, watcher)
				installTopologyWatcher(mw, fmt.Sprintf("repo-%03d", i), watcher)
			}

			families, owner, members := topologyFamilySnapshot(mw)
			if families != 1 || members != prefixes || owner != "repo-000" {
				t.Fatalf("family state = families:%d owner:%q members:%d", families, owner, members)
			}
			if got := fixture.inventoryCalls.Load(); got != 1 {
				t.Fatalf("inventory calls = %d, want 1", got)
			}
			expectedPaths := 2*prefixes + 2 // common dir, worktrees dir, roots, admin dirs
			unique, duplicates, registrations := fixture.activeStats()
			if unique != expectedPaths || registrations != expectedPaths || duplicates != 0 {
				t.Fatalf("topology registrations = unique:%d registrations:%d duplicates:%d, want %d/%d/0", unique, registrations, duplicates, expectedPaths, expectedPaths)
			}

			owners := 0
			for _, watcher := range watchers {
				owned, paths := watcherTopologySnapshot(watcher)
				if owned {
					owners++
					if paths != expectedPaths {
						t.Fatalf("owner paths = %d, want %d", paths, expectedPaths)
					}
				} else if paths != 0 {
					t.Fatalf("follower registered %d topology paths", paths)
				}
			}
			if owners != 1 {
				t.Fatalf("topology owners = %d, want 1", owners)
			}

			var callbacks atomic.Int64
			mw.OnWorktreeChange(func(string, string) { callbacks.Add(1) })
			assertOneTopologyCallback(t, &callbacks, watchers[0].debounce)
			callbacks.Store(0)
			for _, watcher := range watchers {
				watcher.scheduleTopologyChange("test-family-event")
			}
			assertOneTopologyCallback(t, &callbacks, watchers[0].debounce)
			if got := fixture.inventoryCalls.Load(); got != 2 {
				t.Fatalf("inventory calls after one family event = %d, want 2 total (startup + event)", got)
			}

			for i := range watchers {
				removeTopologyWatcher(mw, fmt.Sprintf("repo-%03d", i))
			}
			if unique, duplicates, registrations := fixture.activeStats(); unique != 0 || duplicates != 0 || registrations != 0 {
				t.Fatalf("topology paths remained after family removal: %d/%d/%d", unique, duplicates, registrations)
			}
		})
	}
}

func TestMultiWatcherTopologyOwnerTransfersAndCleansFamily(t *testing.T) {
	fixture := newTopologyWatchFixture(t, 4)
	mw := newTopologyRegistry()
	watchers := make(map[string]*GitWatcher)
	for i := 0; i < 4; i++ {
		prefix := fmt.Sprintf("repo-%02d", i)
		watchers[prefix] = fixture.watcher(i)
		installTopologyWatcher(mw, prefix, watchers[prefix])
	}
	if got := fixture.inventoryCalls.Load(); got != 1 {
		t.Fatalf("initial inventory calls = %d, want 1", got)
	}

	var callbacks atomic.Int64
	mw.OnWorktreeChange(func(string, string) { callbacks.Add(1) })
	assertOneTopologyCallback(t, &callbacks, watchers["repo-00"].debounce)
	callbacks.Store(0)
	watchers["repo-00"].scheduleTopologyChange("queued-before-transfer")

	removeTopologyWatcher(mw, "repo-00")
	families, owner, members := topologyFamilySnapshot(mw)
	if families != 1 || owner != "repo-01" || members != 3 {
		t.Fatalf("transferred family = families:%d owner:%q members:%d", families, owner, members)
	}
	if got := fixture.inventoryCalls.Load(); got < 2 || got > 3 {
		t.Fatalf("inventory calls after transfer = %d, want ownership refresh plus at most one queued nudge", got)
	}
	if owned, paths := watcherTopologySnapshot(watchers["repo-00"]); owned || paths != 0 {
		t.Fatalf("removed owner state = owned:%t paths:%d", owned, paths)
	}
	if unique, duplicates, registrations := fixture.activeStats(); unique != 10 || duplicates != 0 || registrations != 10 {
		t.Fatalf("active transfer registrations = %d/%d/%d, want 10/0/10", unique, duplicates, registrations)
	}

	// unregisterTopologyWatcherLocked must preserve a mutation that was still
	// pending in the retired owner's debounce window. Do not manually nudge the
	// promoted owner: the transfer itself queues exactly one new-epoch refresh.
	assertOneTopologyCallback(t, &callbacks, watchers["repo-01"].debounce)
	if got := fixture.inventoryCalls.Load(); got != 3 {
		t.Fatalf("inventory calls after transfer nudge = %d, want 3", got)
	}

	for _, prefix := range []string{"repo-03", "repo-02", "repo-01"} {
		removeTopologyWatcher(mw, prefix)
	}
	families, _, members = topologyFamilySnapshot(mw)
	if families != 0 || members != 0 || len(mw.topologyFamilyByRepo) != 0 {
		t.Fatalf("family registry survived last removal: families:%d members:%d reverse:%d", families, members, len(mw.topologyFamilyByRepo))
	}
	if unique, duplicates, registrations := fixture.activeStats(); unique != 0 || duplicates != 0 || registrations != 0 {
		t.Fatalf("topology paths survived last removal: %d/%d/%d", unique, duplicates, registrations)
	}
}

func TestMultiWatcherTopologyRemoveRepoDrainsAdmittedCallback(t *testing.T) {
	fixture := newTopologyWatchFixture(t, 1)
	mw := newTopologyRegistry()
	var callbacks atomic.Int64
	entered := make(chan struct{})
	release := make(chan struct{})
	mw.OnWorktreeChange(func(string, string) {
		// Re-entering an ordinary MultiWatcher method proves the external
		// callback is not invoked while mw.mu is held.
		mw.WatchedRepos()
		close(entered)
		<-release
		callbacks.Add(1)
	})

	watcher := fixture.watcher(0)
	makeTopologyWatcherStopSafe(t, watcher)
	installTopologyWatcher(mw, "repo-00", watcher)
	mw.mu.Lock()
	mw.watchers["repo-00"] = &Watcher{}
	mw.mu.Unlock()

	epoch := topologyDispatchEpochSnapshot(t, mw, "repo-00")
	dispatch, admitted := mw.admitWorktreeTopologyChange("repo-00", watcher, epoch, watcher.repoPath)
	if !admitted {
		t.Fatal("owner callback was not admitted")
	}
	removed := make(chan error, 1)
	go func() { removed <- mw.RemoveRepo("repo-00") }()
	waitForTopologyEpochInvalidation(t, mw, epoch)
	assertTopologyLifecycleBlocked(t, removed, "RemoveRepo before callback invocation")

	go dispatch.invoke()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("admitted callback did not begin")
	}
	assertTopologyLifecycleBlocked(t, removed, "RemoveRepo during callback invocation")
	close(release)
	if err := <-removed; err != nil {
		t.Fatalf("RemoveRepo: %v", err)
	}
	if got := callbacks.Load(); got != 1 {
		t.Fatalf("callbacks = %d, want 1", got)
	}

	mw.dispatchWorktreeTopologyChange("repo-00", watcher, epoch, watcher.repoPath)
	if got := callbacks.Load(); got != 1 {
		t.Fatalf("removed epoch admitted a callback after RemoveRepo: %d", got)
	}
}

func TestMultiWatcherTopologyStopDrainsAdmittedCallback(t *testing.T) {
	fixture := newTopologyWatchFixture(t, 1)
	mw := newTopologyRegistry()
	var callbacks atomic.Int64
	entered := make(chan struct{})
	release := make(chan struct{})
	mw.OnWorktreeChange(func(string, string) {
		mw.WatchedRepos()
		close(entered)
		<-release
		callbacks.Add(1)
	})

	watcher := fixture.watcher(0)
	makeTopologyWatcherStopSafe(t, watcher)
	installTopologyWatcher(mw, "repo-00", watcher)
	epoch := topologyDispatchEpochSnapshot(t, mw, "repo-00")
	dispatch, admitted := mw.admitWorktreeTopologyChange("repo-00", watcher, epoch, watcher.repoPath)
	if !admitted {
		t.Fatal("owner callback was not admitted")
	}

	first := make(chan error, 1)
	second := make(chan error, 1)
	go func() { first <- mw.Stop() }()
	waitForTopologyEpochInvalidation(t, mw, epoch)
	go func() { second <- mw.Stop() }()
	assertTopologyLifecycleBlocked(t, first, "first Stop before callback invocation")
	assertTopologyLifecycleBlocked(t, second, "concurrent Stop before callback invocation")

	go dispatch.invoke()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("admitted callback did not begin")
	}
	assertTopologyLifecycleBlocked(t, first, "first Stop during callback invocation")
	assertTopologyLifecycleBlocked(t, second, "concurrent Stop during callback invocation")
	close(release)
	if err := <-first; err != nil {
		t.Fatalf("first Stop: %v", err)
	}
	if err := <-second; err != nil {
		t.Fatalf("concurrent Stop: %v", err)
	}
	if got := callbacks.Load(); got != 1 {
		t.Fatalf("callbacks = %d, want 1", got)
	}

	mw.dispatchWorktreeTopologyChange("repo-00", watcher, epoch, watcher.repoPath)
	if got := callbacks.Load(); got != 1 {
		t.Fatalf("stopped epoch admitted a callback after Stop: %d", got)
	}
}

func TestMultiWatcherTopologyZeroFlightTransferActivatesOneNudge(t *testing.T) {
	fixture := newTopologyWatchFixture(t, 2)
	mw := newTopologyRegistry()
	var callbacks atomic.Int64
	mw.OnWorktreeChange(func(prefix, _ string) {
		if prefix == "repo-01" {
			callbacks.Add(1)
		}
	})

	oldOwner := fixture.watcher(0)
	newOwner := fixture.watcher(1)
	installTopologyWatcher(mw, "repo-00", oldOwner)
	installTopologyWatcher(mw, "repo-01", newOwner)

	mw.mu.Lock()
	oldEpoch := mw.topologyFamilies[canonicalGitCommonDir(fixture.commonDir)].dispatch
	if oldEpoch == nil {
		mw.mu.Unlock()
		t.Fatal("old owner has no topology dispatch epoch")
	}
	if oldEpoch.inFlight != 0 {
		mw.mu.Unlock()
		t.Fatalf("old epoch in-flight = %d, want zero", oldEpoch.inFlight)
	}
	drain := mw.unregisterTopologyWatcherLocked("repo-00")
	delete(mw.gitWatchers, "repo-00")
	mw.mu.Unlock()
	waitTopologyDispatchDrains(drain)

	families, owner, members := topologyFamilySnapshot(mw)
	if families != 1 || owner != "repo-01" || members != 1 {
		t.Fatalf("transferred family = families:%d owner:%q members:%d", families, owner, members)
	}
	assertOneTopologyCallback(t, &callbacks, newOwner.debounce)
	removeTopologyWatcher(mw, "repo-01")
}

func TestMultiWatcherTopologyHandoffDefersSurvivorChurnUntilDrain(t *testing.T) {
	fixture := newTopologyWatchFixture(t, 3)
	mw := newTopologyRegistry()
	var survivorCallbacks atomic.Int64
	mw.OnWorktreeChange(func(prefix, _ string) {
		if prefix == "repo-01" {
			survivorCallbacks.Add(1)
		}
	})

	oldOwner := fixture.watcher(0)
	survivor := fixture.watcher(1)
	transient := fixture.watcher(2)
	installTopologyWatcher(mw, "repo-00", oldOwner)
	installTopologyWatcher(mw, "repo-01", survivor)
	oldEpoch := topologyDispatchEpochSnapshot(t, mw, "repo-00")
	oldDispatch, admitted := mw.admitWorktreeTopologyChange("repo-00", oldOwner, oldEpoch, oldOwner.repoPath)
	if !admitted {
		t.Fatal("old owner callback was not admitted")
	}

	mw.mu.Lock()
	drain := mw.unregisterTopologyWatcherLocked("repo-00")
	delete(mw.gitWatchers, "repo-00")
	family := mw.topologyFamilies[canonicalGitCommonDir(fixture.commonDir)]
	mw.mu.Unlock()
	if family == nil || family.handoff != oldEpoch {
		t.Fatal("old epoch was not published as family handoff")
	}

	installTopologyWatcher(mw, "repo-02", transient)
	families, owner, members := topologyFamilySnapshot(mw)
	if families != 1 || owner != "" || members != 2 {
		t.Fatalf("family after registration during handoff = families:%d owner:%q members:%d", families, owner, members)
	}
	removeTopologyWatcher(mw, "repo-02")
	families, owner, members = topologyFamilySnapshot(mw)
	if families != 1 || owner != "" || members != 1 {
		t.Fatalf("family after removal during handoff = families:%d owner:%q members:%d", families, owner, members)
	}

	oldDispatch.invoke()
	waitTopologyDispatchDrains(drain)
	families, owner, members = topologyFamilySnapshot(mw)
	if families != 1 || owner != "repo-01" || members != 1 {
		t.Fatalf("family after handoff drain = families:%d owner:%q members:%d", families, owner, members)
	}
	assertOneTopologyCallback(t, &survivorCallbacks, survivor.debounce)
	removeTopologyWatcher(mw, "repo-01")
}

func TestMultiWatcherTopologyStopDuringHandoffSuppressesElection(t *testing.T) {
	fixture := newTopologyWatchFixture(t, 2)
	mw := newTopologyRegistry()
	var survivorCallbacks atomic.Int64
	mw.OnWorktreeChange(func(prefix, _ string) {
		if prefix == "repo-01" {
			survivorCallbacks.Add(1)
		}
	})

	oldOwner := fixture.watcher(0)
	survivor := fixture.watcher(1)
	makeTopologyWatcherStopSafe(t, oldOwner)
	makeTopologyWatcherStopSafe(t, survivor)
	installTopologyWatcher(mw, "repo-00", oldOwner)
	installTopologyWatcher(mw, "repo-01", survivor)
	oldEpoch := topologyDispatchEpochSnapshot(t, mw, "repo-00")
	oldDispatch, admitted := mw.admitWorktreeTopologyChange("repo-00", oldOwner, oldEpoch, oldOwner.repoPath)
	if !admitted {
		t.Fatal("old owner callback was not admitted")
	}

	mw.mu.Lock()
	drain := mw.unregisterTopologyWatcherLocked("repo-00")
	delete(mw.gitWatchers, "repo-00")
	mw.mu.Unlock()
	stopped := make(chan error, 1)
	go func() { stopped <- mw.Stop() }()
	assertTopologyLifecycleBlocked(t, stopped, "stop during topology handoff")

	oldDispatch.invoke()
	waitTopologyDispatchDrains(drain)
	if err := <-stopped; err != nil {
		t.Fatalf("stop multi-watcher: %v", err)
	}
	if got := survivorCallbacks.Load(); got != 0 {
		t.Fatalf("survivor callbacks after stop = %d, want 0", got)
	}
	mw.mu.Lock()
	families := len(mw.topologyFamilies)
	dispatches := len(mw.topologyDispatches)
	reverse := len(mw.topologyFamilyByRepo)
	mw.mu.Unlock()
	if families != 0 || dispatches != 0 || reverse != 0 {
		t.Fatalf("topology state after stop = families:%d dispatches:%d reverse:%d", families, dispatches, reverse)
	}
}

func TestMultiWatcherTopologyEmptyShellReusesFamilyDuringHandoff(t *testing.T) {
	fixture := newTopologyWatchFixture(t, 2)
	mw := newTopologyRegistry()
	var replacementCallbacks atomic.Int64
	mw.OnWorktreeChange(func(prefix, _ string) {
		if prefix == "repo-01" {
			replacementCallbacks.Add(1)
		}
	})

	oldOwner := fixture.watcher(0)
	replacement := fixture.watcher(1)
	installTopologyWatcher(mw, "repo-00", oldOwner)
	oldEpoch := topologyDispatchEpochSnapshot(t, mw, "repo-00")
	oldDispatch, admitted := mw.admitWorktreeTopologyChange("repo-00", oldOwner, oldEpoch, oldOwner.repoPath)
	if !admitted {
		t.Fatal("old owner callback was not admitted")
	}

	mw.mu.Lock()
	key := canonicalGitCommonDir(fixture.commonDir)
	family := mw.topologyFamilies[key]
	drain := mw.unregisterTopologyWatcherLocked("repo-00")
	delete(mw.gitWatchers, "repo-00")
	mw.mu.Unlock()
	installTopologyWatcher(mw, "repo-01", replacement)

	mw.mu.Lock()
	current := mw.topologyFamilies[key]
	if current == nil {
		mw.mu.Unlock()
		t.Fatal("topology family shell disappeared during handoff")
	}
	owner := current.owner
	dispatch := current.dispatch
	handoff := current.handoff
	members := len(current.members)
	mw.mu.Unlock()
	if current != family || owner != "" || dispatch != nil || handoff != oldEpoch || members != 1 {
		t.Fatalf("family shell replaced during handoff: same:%t owner:%q dispatch:%v handoff:%p members:%d", current == family, owner, dispatch, handoff, members)
	}

	oldDispatch.invoke()
	waitTopologyDispatchDrains(drain)
	families, owner, members := topologyFamilySnapshot(mw)
	if families != 1 || owner != "repo-01" || members != 1 {
		t.Fatalf("replacement family after drain = families:%d owner:%q members:%d", families, owner, members)
	}
	assertOneTopologyCallback(t, &replacementCallbacks, replacement.debounce)
	removeTopologyWatcher(mw, "repo-01")
}

func TestMultiWatcherTopologyOwnerTransferDrainsOnlyOldEpoch(t *testing.T) {
	fixture := newTopologyWatchFixture(t, 2)
	mw := newTopologyRegistry()
	var oldCallbacks atomic.Int64
	var newCallbacks atomic.Int64
	mw.OnWorktreeChange(func(prefix, _ string) {
		mw.WatchedRepos()
		if prefix == "repo-00" {
			oldCallbacks.Add(1)
		} else if prefix == "repo-01" {
			newCallbacks.Add(1)
		}
	})

	oldOwner := fixture.watcher(0)
	newOwner := fixture.watcher(1)
	installTopologyWatcher(mw, "repo-00", oldOwner)
	installTopologyWatcher(mw, "repo-01", newOwner)
	oldEpoch := topologyDispatchEpochSnapshot(t, mw, "repo-00")
	oldDispatch, admitted := mw.admitWorktreeTopologyChange("repo-00", oldOwner, oldEpoch, oldOwner.repoPath)
	if !admitted {
		t.Fatal("old owner callback was not admitted")
	}

	transferred := make(chan error, 1)
	go func() {
		mw.mu.Lock()
		drain := mw.unregisterTopologyWatcherLocked("repo-00")
		delete(mw.gitWatchers, "repo-00")
		mw.mu.Unlock()
		waitTopologyDispatchDrains(drain)
		transferred <- nil
	}()
	waitForTopologyEpochInvalidation(t, mw, oldEpoch)
	assertTopologyLifecycleBlocked(t, transferred, "owner transfer")

	families, owner, members := topologyFamilySnapshot(mw)
	if families != 1 || owner != "" || members != 1 {
		t.Fatalf("handoff family = families:%d owner:%q members:%d", families, owner, members)
	}
	mw.mu.Lock()
	family := mw.topologyFamilies[canonicalGitCommonDir(fixture.commonDir)]
	if family == nil {
		mw.mu.Unlock()
		t.Fatal("topology family disappeared during handoff")
	}
	dispatch := family.dispatch
	handoff := family.handoff
	mw.mu.Unlock()
	if dispatch != nil || handoff != oldEpoch {
		t.Fatalf("handoff state = dispatch:%v handoff:%p, want old epoch %p", dispatch, handoff, oldEpoch)
	}
	if _, admitted := mw.admitWorktreeTopologyChange("repo-01", newOwner, oldEpoch, newOwner.repoPath); admitted {
		t.Fatal("new owner callback admitted before old epoch drained")
	}

	oldDispatch.invoke()
	if got := oldCallbacks.Load(); got != 1 {
		t.Fatalf("old admitted callback count = %d, want 1", got)
	}
	if err := <-transferred; err != nil {
		t.Fatalf("owner transfer: %v", err)
	}

	families, owner, members = topologyFamilySnapshot(mw)
	if families != 1 || owner != "repo-01" || members != 1 {
		t.Fatalf("transferred family = families:%d owner:%q members:%d", families, owner, members)
	}
	newEpoch := topologyDispatchEpochSnapshot(t, mw, "repo-01")
	if newEpoch == oldEpoch || newEpoch.number <= oldEpoch.number {
		t.Fatalf("owner transfer did not advance epoch: old=%d new=%d", oldEpoch.number, newEpoch.number)
	}
	assertOneTopologyCallback(t, &newCallbacks, newOwner.debounce)
	removeTopologyWatcher(mw, "repo-01")
}

func waitForTopologyCondition(t *testing.T, message string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal(message)
}

func TestMultiWatcherTopologyRetainedDispatchOutlivesCallbackAndBlocksStop(t *testing.T) {
	fixture := newTopologyWatchFixture(t, 1)
	mw := newTopologyRegistry()
	retainedContext := make(chan context.Context, 1)
	retainedRelease := make(chan func(), 1)
	mw.OnWorktreeChangeContext(func(ctx context.Context, _, _ string) {
		asyncCtx, release := mw.RetainTopologyDispatch(ctx)
		retainedContext <- asyncCtx
		retainedRelease <- release
	})

	watcher := fixture.watcher(0)
	makeTopologyWatcherStopSafe(t, watcher)
	installTopologyWatcher(mw, "repo-00", watcher)
	epoch := topologyDispatchEpochSnapshot(t, mw, "repo-00")
	dispatch, admitted := mw.admitWorktreeTopologyChange("repo-00", watcher, epoch, watcher.repoPath)
	if !admitted {
		t.Fatal("owner callback was not admitted")
	}
	dispatch.invoke()
	asyncCtx := <-retainedContext
	release := <-retainedRelease

	mw.mu.Lock()
	retained := mw.activeTopologyDispatchTokenLocked(asyncCtx)
	inFlight := epoch.inFlight
	mw.mu.Unlock()
	if retained == nil || retained.epoch != epoch || inFlight != 1 {
		t.Fatalf("retained dispatch = token:%t epoch:%t in-flight:%d, want true/true/1", retained != nil, retained != nil && retained.epoch == epoch, inFlight)
	}

	stopped := make(chan error, 1)
	go func() { stopped <- mw.Stop() }()
	waitForTopologyEpochInvalidation(t, mw, epoch)
	assertTopologyLifecycleBlocked(t, stopped, "Stop with retained async dispatch")

	release()
	release() // exact-once: a duplicate controller completion cannot underflow.
	if err := <-stopped; err != nil {
		t.Fatalf("Stop: %v", err)
	}
	mw.mu.Lock()
	inFlight = epoch.inFlight
	_, registered := mw.topologyDispatches[epoch]
	mw.mu.Unlock()
	if inFlight != 0 || registered {
		t.Fatalf("released dispatch = in-flight:%d registered:%t, want 0/false", inFlight, registered)
	}
}

func TestMultiWatcherTopologyReentrantStopDefersOwnLease(t *testing.T) {
	fixture := newTopologyWatchFixture(t, 1)
	mw := newTopologyRegistry()
	returned := make(chan error, 1)
	release := make(chan struct{})
	mw.OnWorktreeChangeContext(func(ctx context.Context, _, _ string) {
		returned <- mw.StopContext(ctx)
		<-release
	})

	watcher := fixture.watcher(0)
	makeTopologyWatcherStopSafe(t, watcher)
	installTopologyWatcher(mw, "repo-00", watcher)
	epoch := topologyDispatchEpochSnapshot(t, mw, "repo-00")
	dispatch, admitted := mw.admitWorktreeTopologyChange("repo-00", watcher, epoch, watcher.repoPath)
	if !admitted {
		t.Fatal("owner callback was not admitted")
	}
	invoked := make(chan struct{})
	go func() {
		dispatch.invoke()
		close(invoked)
	}()
	if err := <-returned; err != nil {
		t.Fatalf("reentrant StopContext: %v", err)
	}

	external := make(chan error, 1)
	go func() { external <- mw.Stop() }()
	assertTopologyLifecycleBlocked(t, external, "external Stop during reentrant callback")
	close(release)
	select {
	case <-invoked:
	case <-time.After(2 * time.Second):
		t.Fatal("callback did not release its dispatch lease")
	}
	if err := <-external; err != nil {
		t.Fatalf("external Stop: %v", err)
	}
}

func TestMultiWatcherTopologyReentrantRemoveDefersOwnLease(t *testing.T) {
	fixture := newTopologyWatchFixture(t, 1)
	mw := newTopologyRegistry()
	returned := make(chan error, 1)
	release := make(chan struct{})
	mw.OnWorktreeChangeContext(func(ctx context.Context, prefix, _ string) {
		returned <- mw.RemoveRepoContext(ctx, prefix)
		<-release
	})

	watcher := fixture.watcher(0)
	makeTopologyWatcherStopSafe(t, watcher)
	installTopologyWatcher(mw, "repo-00", watcher)
	mw.mu.Lock()
	mw.watchers["repo-00"] = &Watcher{}
	mw.mu.Unlock()
	epoch := topologyDispatchEpochSnapshot(t, mw, "repo-00")
	dispatch, admitted := mw.admitWorktreeTopologyChange("repo-00", watcher, epoch, watcher.repoPath)
	if !admitted {
		t.Fatal("owner callback was not admitted")
	}
	invoked := make(chan struct{})
	go func() {
		dispatch.invoke()
		close(invoked)
	}()
	if err := <-returned; err != nil {
		t.Fatalf("reentrant RemoveRepoContext: %v", err)
	}

	external := make(chan error, 1)
	go func() { external <- mw.RemoveRepo("repo-00") }()
	assertTopologyLifecycleBlocked(t, external, "external RemoveRepo during reentrant callback")
	close(release)
	select {
	case <-invoked:
	case <-time.After(2 * time.Second):
		t.Fatal("callback did not release its dispatch lease")
	}
	if err := <-external; err != nil {
		t.Fatalf("external RemoveRepo: %v", err)
	}
}

func TestMultiWatcherTopologyReentrantOwnerTransfer(t *testing.T) {
	fixture := newTopologyWatchFixture(t, 2)
	mw := newTopologyRegistry()
	transferred := make(chan string, 1)
	mw.OnWorktreeChangeContext(func(ctx context.Context, prefix, _ string) {
		if prefix != "repo-00" {
			return
		}
		if err := mw.RemoveRepoContext(ctx, prefix); err != nil {
			transferred <- "error: " + err.Error()
			return
		}
		_, owner, _ := topologyFamilySnapshot(mw)
		transferred <- owner
	})

	oldOwner := fixture.watcher(0)
	newOwner := fixture.watcher(1)
	makeTopologyWatcherStopSafe(t, oldOwner)
	makeTopologyWatcherStopSafe(t, newOwner)
	installTopologyWatcher(mw, "repo-00", oldOwner)
	installTopologyWatcher(mw, "repo-01", newOwner)
	mw.mu.Lock()
	mw.watchers["repo-00"] = &Watcher{}
	mw.mu.Unlock()
	oldEpoch := topologyDispatchEpochSnapshot(t, mw, "repo-00")
	dispatch, admitted := mw.admitWorktreeTopologyChange("repo-00", oldOwner, oldEpoch, oldOwner.repoPath)
	if !admitted {
		t.Fatal("old owner callback was not admitted")
	}
	invoked := make(chan struct{})
	go func() {
		dispatch.invoke()
		close(invoked)
	}()
	select {
	case owner := <-transferred:
		if owner != "" {
			t.Fatalf("owner visible inside callback = %q, want empty handoff", owner)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("callback-triggered owner transfer deadlocked")
	}
	select {
	case <-invoked:
	case <-time.After(2 * time.Second):
		t.Fatal("owner-transfer callback did not return")
	}
	families, owner, members := topologyFamilySnapshot(mw)
	if families != 1 || owner != "repo-01" || members != 1 {
		t.Fatalf("family after callback release = families:%d owner:%q members:%d", families, owner, members)
	}
	waitForTopologyCondition(t, "old owner retirement did not finish", func() bool {
		mw.mu.Lock()
		defer mw.mu.Unlock()
		return mw.retiringWatchers["repo-00"] == nil
	})
	removeTopologyWatcher(mw, "repo-01")
}

func TestMultiWatcherTopologyRetiringPhysicalStopsJoinGlobalStop(t *testing.T) {
	fixture := newTopologyWatchFixture(t, 1)
	mw := newTopologyRegistry()
	watcherStopEntered := make(chan struct{})
	watcherStopRelease := make(chan struct{})
	var releaseWatcher sync.Once
	w := &Watcher{
		events:             make(chan GraphChangeEvent),
		done:               make(chan struct{}),
		degradedNoFsnotify: true,
		stopAdmissionClosed: func() {
			close(watcherStopEntered)
			<-watcherStopRelease
		},
	}

	gw := fixture.watcher(0)
	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		t.Fatalf("fsnotify.NewWatcher: %v", err)
	}
	gw.mu.Lock()
	gw.fsw = fsw
	gw.done = make(chan struct{})
	gw.stopped = make(chan struct{})
	gw.loopStarted = true
	gw.mu.Unlock()
	var releaseGit sync.Once
	t.Cleanup(func() {
		releaseWatcher.Do(func() { close(watcherStopRelease) })
		releaseGit.Do(func() { close(gw.stopped) })
		_ = gw.Stop()
	})

	mw.mu.Lock()
	mw.watchers["repo-00"] = w
	mw.started["repo-00"] = true
	mw.startForwarderLocked("repo-00", w)
	mw.installStartedGitWatcherLocked("repo-00", gw)
	mw.mu.Unlock()

	removed := make(chan error, 1)
	go func() { removed <- mw.RemoveRepo("repo-00") }()
	select {
	case <-watcherStopEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("retiring Watcher.Stop did not begin")
	}
	stopped := make(chan error, 1)
	go func() { stopped <- mw.Stop() }()
	assertTopologyLifecycleBlocked(t, removed, "RemoveRepo at blocked Watcher.Stop")
	assertTopologyLifecycleBlocked(t, stopped, "global Stop at blocked Watcher.Stop")

	releaseWatcher.Do(func() { close(watcherStopRelease) })
	select {
	case <-gw.done:
	case <-time.After(2 * time.Second):
		t.Fatal("retiring GitWatcher.Stop did not begin")
	}
	assertTopologyLifecycleBlocked(t, removed, "RemoveRepo at blocked GitWatcher.Stop")
	assertTopologyLifecycleBlocked(t, stopped, "global Stop at blocked GitWatcher.Stop")
	releaseGit.Do(func() { close(gw.stopped) })
	if err := <-removed; err != nil {
		t.Fatalf("RemoveRepo: %v", err)
	}
	if err := <-stopped; err != nil {
		t.Fatalf("Stop: %v", err)
	}
}

func TestMultiWatcherTopologyForwardersJoinEveryRemoval(t *testing.T) {
	mw := newTopologyRegistry()
	for iteration := 0; iteration < 64; iteration++ {
		w := &Watcher{events: make(chan GraphChangeEvent), done: make(chan struct{})}
		mw.mu.Lock()
		mw.watchers["repo-00"] = w
		forwarder := mw.startForwarderLocked("repo-00", w)
		mw.mu.Unlock()
		if err := mw.RemoveRepo("repo-00"); err != nil {
			t.Fatalf("iteration %d RemoveRepo: %v", iteration, err)
		}
		select {
		case <-forwarder.done:
		default:
			t.Fatalf("iteration %d forwarder was not joined", iteration)
		}
		mw.mu.Lock()
		forwarders := len(mw.forwarders)
		retirements := len(mw.retiringWatchers)
		mw.mu.Unlock()
		if forwarders != 0 || retirements != 0 {
			t.Fatalf("iteration %d retained forwarders:%d retirements:%d", iteration, forwarders, retirements)
		}
	}
	if err := mw.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}

func TestMultiWatcherTopologyRemoveAddWaitsForRetirement(t *testing.T) {
	mw, _, _, _ := setupMultiWatcherTest(t)
	if err := mw.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	var releaseOld sync.Once
	stopEntered := make(chan struct{})
	stopRelease := make(chan struct{})
	mw.mu.Lock()
	old := mw.watchers["repo-a"]
	mw.mu.Unlock()
	old.stopAdmissionClosed = func() {
		close(stopEntered)
		<-stopRelease
	}
	t.Cleanup(func() {
		releaseOld.Do(func() { close(stopRelease) })
		_ = mw.Stop()
	})

	removed := make(chan error, 1)
	go func() { removed <- mw.RemoveRepo("repo-a") }()
	select {
	case <-stopEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("old watcher did not enter physical Stop")
	}
	added := make(chan error, 1)
	go func() {
		added <- mw.AddRepo("repo-a", config.WatchConfig{Enabled: true, DebounceMs: 50})
	}()
	assertTopologyLifecycleBlocked(t, added, "AddRepo during prefix retirement")
	mw.mu.Lock()
	_, activeBeforeRelease := mw.watchers["repo-a"]
	retirementBeforeRelease := mw.retiringWatchers["repo-a"]
	mw.mu.Unlock()
	if activeBeforeRelease || retirementBeforeRelease == nil {
		t.Fatalf("retirement boundary = active:%t tombstone:%t, want false/true", activeBeforeRelease, retirementBeforeRelease != nil)
	}

	releaseOld.Do(func() { close(stopRelease) })
	if err := <-removed; err != nil {
		t.Fatalf("RemoveRepo: %v", err)
	}
	if err := <-added; err != nil {
		t.Fatalf("AddRepo: %v", err)
	}
	mw.mu.Lock()
	replacement := mw.watchers["repo-a"]
	_, retiring := mw.retiringWatchers["repo-a"]
	_, forwarding := mw.forwarders["repo-a"]
	mw.mu.Unlock()
	if replacement == nil || replacement == old || retiring || !forwarding {
		t.Fatalf("replacement state = watcher:%p old:%p retiring:%t forwarding:%t", replacement, old, retiring, forwarding)
	}
}

func TestMultiWatcherTopologyReentrantRemoveAddEventuallyReplaces(t *testing.T) {
	mw, _, _, _ := setupMultiWatcherTest(t)
	if err := mw.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = mw.Stop() })
	mw.mu.Lock()
	old := mw.watchers["repo-a"]
	mw.mu.Unlock()

	fixture := newTopologyWatchFixture(t, 1)
	gw := fixture.watcher(0)
	makeTopologyWatcherStopSafe(t, gw)
	result := make(chan error, 1)
	cfg := config.WatchConfig{Enabled: true, DebounceMs: 50}
	mw.OnWorktreeChangeContext(func(ctx context.Context, prefix, _ string) {
		if err := mw.RemoveRepoContext(ctx, prefix); err != nil {
			result <- err
			return
		}
		result <- mw.AddRepoContext(ctx, prefix, cfg)
	})
	installTopologyWatcher(mw, "repo-a", gw)
	epoch := topologyDispatchEpochSnapshot(t, mw, "repo-a")
	dispatch, admitted := mw.admitWorktreeTopologyChange("repo-a", gw, epoch, gw.repoPath)
	if !admitted {
		t.Fatal("owner callback was not admitted")
	}
	invoked := make(chan struct{})
	go func() {
		dispatch.invoke()
		close(invoked)
	}()
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("reentrant remove/add: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("reentrant remove/add deadlocked")
	}
	select {
	case <-invoked:
	case <-time.After(2 * time.Second):
		t.Fatal("reentrant callback did not return")
	}

	waitForTopologyCondition(t, "queued replacement was not installed", func() bool {
		mw.mu.Lock()
		defer mw.mu.Unlock()
		replacement := mw.watchers["repo-a"]
		return replacement != nil && replacement != old &&
			mw.retiringWatchers["repo-a"] == nil &&
			mw.pendingWatcherAdds["repo-a"] == nil &&
			mw.forwarders["repo-a"] != nil
	})
	mw.mu.Lock()
	watcherCount := len(mw.watchers)
	mw.mu.Unlock()
	if watcherCount != 2 {
		t.Fatalf("live watcher count = %d, want exactly 2", watcherCount)
	}
}

func TestMultiWatcherTopologyLaterRemoveCancelsQueuedReplacement(t *testing.T) {
	mw, _, _, _ := setupMultiWatcherTest(t)
	if err := mw.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = mw.Stop() })

	fixture := newTopologyWatchFixture(t, 1)
	gw := fixture.watcher(0)
	makeTopologyWatcherStopSafe(t, gw)
	result := make(chan error, 1)
	cfg := config.WatchConfig{Enabled: true, DebounceMs: 50}
	mw.OnWorktreeChangeContext(func(ctx context.Context, prefix, _ string) {
		if err := mw.RemoveRepoContext(ctx, prefix); err != nil {
			result <- err
			return
		}
		if err := mw.AddRepoContext(ctx, prefix, cfg); err != nil {
			result <- err
			return
		}
		result <- mw.RemoveRepoContext(ctx, prefix)
	})
	installTopologyWatcher(mw, "repo-a", gw)
	epoch := topologyDispatchEpochSnapshot(t, mw, "repo-a")
	dispatch, admitted := mw.admitWorktreeTopologyChange("repo-a", gw, epoch, gw.repoPath)
	if !admitted {
		t.Fatal("owner callback was not admitted")
	}
	invoked := make(chan struct{})
	go func() {
		dispatch.invoke()
		close(invoked)
	}()
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("remove/add/remove: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("remove/add/remove callback deadlocked")
	}
	select {
	case <-invoked:
	case <-time.After(2 * time.Second):
		t.Fatal("remove/add/remove callback did not return")
	}
	waitForTopologyCondition(t, "later remove allowed queued replacement resurrection", func() bool {
		mw.mu.Lock()
		defer mw.mu.Unlock()
		return mw.watchers["repo-a"] == nil &&
			mw.retiringWatchers["repo-a"] == nil &&
			mw.pendingWatcherAdds["repo-a"] == nil &&
			mw.forwarders["repo-a"] == nil
	})
}

func TestMultiWatcherTopologyRemoveCancelsRetryingIntentWithoutWatcher(t *testing.T) {
	mw := newTopologyRegistry()
	pending := &pendingWatcherAdd{cfg: config.WatchConfig{Enabled: true}}
	mw.mu.Lock()
	mw.pendingWatcherAdds["repo-00"] = pending
	mw.mu.Unlock()
	if err := mw.RemoveRepo("repo-00"); err != nil {
		t.Fatalf("RemoveRepo canceling pending intent: %v", err)
	}
	mw.mu.Lock()
	remaining := mw.pendingWatcherAdds["repo-00"]
	mw.mu.Unlock()
	if remaining != nil {
		t.Fatal("RemoveRepo left a pending replacement intent without a live watcher")
	}
	if err := mw.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}

func waitPendingWatcherAddDone(t *testing.T, pending *pendingWatcherAdd) {
	t.Helper()
	select {
	case <-pending.done:
	case <-time.After(3 * time.Second):
		t.Fatal("pending watcher add did not finish")
	}
}

func assertWatcherPrefixAbsent(t *testing.T, mw *MultiWatcher, prefix string) {
	t.Helper()
	mw.mu.Lock()
	defer mw.mu.Unlock()
	if mw.watchers[prefix] != nil || mw.gitWatchers[prefix] != nil || mw.forwarders[prefix] != nil || mw.retiringWatchers[prefix] != nil || mw.pendingWatcherAdds[prefix] != nil {
		t.Fatalf("prefix %q survived removal: watcher=%v git=%v forwarder=%v retirement=%v pending=%v", prefix, mw.watchers[prefix] != nil, mw.gitWatchers[prefix] != nil, mw.forwarders[prefix] != nil, mw.retiringWatchers[prefix] != nil, mw.pendingWatcherAdds[prefix] != nil)
	}
}

func TestMultiWatcherPendingAddRemoveBeforeAdmissionDoesNotResurrect(t *testing.T) {
	mw, _, _, _ := setupMultiWatcherTest(t)
	if err := mw.Start(); err != nil {
		t.Fatalf("start multi-watcher: %v", err)
	}
	defer func() { _ = mw.Stop() }()
	if err := mw.RemoveRepo("repo-a"); err != nil {
		t.Fatalf("remove initial watcher: %v", err)
	}

	claimed := make(chan struct{})
	release := make(chan struct{})
	mw.mu.Lock()
	mw.pendingAddClaimed = func() {
		close(claimed)
		<-release
	}
	mw.queuePendingWatcherAddLocked("repo-a", config.WatchConfig{Enabled: true, DebounceMs: 50})
	pending := mw.pendingWatcherAdds["repo-a"]
	mw.mu.Unlock()

	select {
	case <-claimed:
	case <-time.After(3 * time.Second):
		t.Fatal("pending watcher add was not claimed")
	}
	if err := mw.RemoveRepo("repo-a"); err != nil {
		t.Fatalf("cancel pending watcher add: %v", err)
	}
	close(release)
	waitPendingWatcherAddDone(t, pending)
	assertWatcherPrefixAbsent(t, mw, "repo-a")
}

func TestMultiWatcherPendingAddStopBeforeAdmissionDoesNotResurrect(t *testing.T) {
	mw, _, _, _ := setupMultiWatcherTest(t)
	if err := mw.Start(); err != nil {
		t.Fatalf("start multi-watcher: %v", err)
	}
	if err := mw.RemoveRepo("repo-a"); err != nil {
		t.Fatalf("remove initial watcher: %v", err)
	}

	claimed := make(chan struct{})
	release := make(chan struct{})
	mw.mu.Lock()
	mw.pendingAddClaimed = func() {
		close(claimed)
		<-release
	}
	mw.queuePendingWatcherAddLocked("repo-a", config.WatchConfig{Enabled: true, DebounceMs: 50})
	pending := mw.pendingWatcherAdds["repo-a"]
	mw.mu.Unlock()

	select {
	case <-claimed:
	case <-time.After(3 * time.Second):
		t.Fatal("pending watcher add was not claimed")
	}
	if err := mw.Stop(); err != nil {
		t.Fatalf("stop multi-watcher: %v", err)
	}
	close(release)
	waitPendingWatcherAddDone(t, pending)
	assertWatcherPrefixAbsent(t, mw, "repo-a")
}

func TestMultiWatcherPendingAddRemoveAfterAdmissionJoinsPublication(t *testing.T) {
	mw, _, _, _ := setupMultiWatcherTest(t)
	if err := mw.Start(); err != nil {
		t.Fatalf("start multi-watcher: %v", err)
	}
	defer func() { _ = mw.Stop() }()
	if err := mw.RemoveRepo("repo-a"); err != nil {
		t.Fatalf("remove initial watcher: %v", err)
	}

	admitted := make(chan struct{})
	release := make(chan struct{})
	mw.mu.Lock()
	mw.pendingAddAdmitted = func() {
		close(admitted)
		<-release
	}
	mw.queuePendingWatcherAddLocked("repo-a", config.WatchConfig{Enabled: true, DebounceMs: 50})
	pending := mw.pendingWatcherAdds["repo-a"]
	mw.mu.Unlock()

	select {
	case <-admitted:
	case <-time.After(3 * time.Second):
		t.Fatal("pending watcher add was not admitted")
	}
	removed := make(chan error, 1)
	go func() { removed <- mw.RemoveRepo("repo-a") }()
	assertTopologyLifecycleBlocked(t, removed, "remove during watcher publication")
	close(release)
	if err := <-removed; err != nil {
		t.Fatalf("remove published watcher: %v", err)
	}
	waitPendingWatcherAddDone(t, pending)
	assertWatcherPrefixAbsent(t, mw, "repo-a")
}

func TestMultiWatcherPendingAddGenerationReplacementSurvivesOldWorker(t *testing.T) {
	mw, _, _, _ := setupMultiWatcherTest(t)
	if err := mw.Start(); err != nil {
		t.Fatalf("start multi-watcher: %v", err)
	}
	defer func() { _ = mw.Stop() }()
	if err := mw.RemoveRepo("repo-a"); err != nil {
		t.Fatalf("remove initial watcher: %v", err)
	}

	firstClaimed := make(chan struct{})
	releaseFirst := make(chan struct{})
	var claims atomic.Int64
	mw.mu.Lock()
	mw.pendingAddClaimed = func() {
		if claims.Add(1) == 1 {
			close(firstClaimed)
			<-releaseFirst
		}
	}
	cfg := config.WatchConfig{Enabled: true, DebounceMs: 50}
	mw.queuePendingWatcherAddLocked("repo-a", cfg)
	oldPending := mw.pendingWatcherAdds["repo-a"]
	mw.mu.Unlock()

	select {
	case <-firstClaimed:
	case <-time.After(3 * time.Second):
		t.Fatal("old pending watcher add was not claimed")
	}
	if err := mw.RemoveRepo("repo-a"); err != nil {
		t.Fatalf("cancel old generation: %v", err)
	}
	mw.mu.Lock()
	mw.queuePendingWatcherAddLocked("repo-a", cfg)
	newPending := mw.pendingWatcherAdds["repo-a"]
	mw.mu.Unlock()
	if newPending == oldPending {
		t.Fatal("replacement reused canceled pending generation")
	}
	close(releaseFirst)
	waitPendingWatcherAddDone(t, oldPending)
	waitPendingWatcherAddDone(t, newPending)

	mw.mu.Lock()
	watcher := mw.watchers["repo-a"]
	pending := mw.pendingWatcherAdds["repo-a"]
	mw.mu.Unlock()
	if watcher == nil || pending != nil {
		t.Fatalf("replacement state = watcher:%v pending:%v, want one live watcher", watcher != nil, pending != nil)
	}
	if err := mw.RemoveRepo("repo-a"); err != nil {
		t.Fatalf("remove replacement watcher: %v", err)
	}
}

func TestMultiWatcherTopologyForeignTokenDoesNotBypassRetirement(t *testing.T) {
	first := newTopologyWatchFixture(t, 1)
	second := newTopologyWatchFixture(t, 1)
	mw := newTopologyRegistry()
	result := make(chan error, 1)
	mw.OnWorktreeChangeContext(func(ctx context.Context, prefix, _ string) {
		if prefix == "first" {
			result <- mw.AddRepoContext(ctx, "second", config.WatchConfig{Enabled: true})
		}
	})

	firstWatcher := first.watcher(0)
	secondWatcher := second.watcher(0)
	makeTopologyWatcherStopSafe(t, firstWatcher)
	makeTopologyWatcherStopSafe(t, secondWatcher)
	installTopologyWatcher(mw, "first", firstWatcher)
	installTopologyWatcher(mw, "second", secondWatcher)
	firstEpoch := topologyDispatchEpochSnapshot(t, mw, "first")
	secondEpoch := topologyDispatchEpochSnapshot(t, mw, "second")
	if firstEpoch == secondEpoch || firstEpoch.drained == secondEpoch.drained {
		t.Fatal("independent families unexpectedly share a dispatch epoch")
	}

	retirement := &watcherRetirement{
		prefix:        "second",
		topologyDrain: secondEpoch.drained,
		done:          make(chan struct{}),
	}
	var releaseRetirement sync.Once
	release := func() {
		releaseRetirement.Do(func() {
			mw.mu.Lock()
			if mw.retiringWatchers["second"] == retirement {
				delete(mw.retiringWatchers, "second")
			}
			mw.watchers["second"] = &Watcher{}
			close(retirement.done)
			mw.mu.Unlock()
		})
	}
	t.Cleanup(release)
	mw.mu.Lock()
	mw.retiringWatchers["second"] = retirement
	mw.mu.Unlock()

	dispatch, admitted := mw.admitWorktreeTopologyChange("first", firstWatcher, firstEpoch, firstWatcher.repoPath)
	if !admitted {
		t.Fatal("first-family callback was not admitted")
	}
	invoked := make(chan struct{})
	go func() {
		dispatch.invoke()
		close(invoked)
	}()
	assertTopologyLifecycleBlocked(t, result, "foreign-family AddRepoContext")

	release()
	if err := <-result; err != nil {
		t.Fatalf("foreign-family AddRepoContext after retirement: %v", err)
	}
	select {
	case <-invoked:
	case <-time.After(2 * time.Second):
		t.Fatal("first-family callback did not return")
	}
	mw.mu.Lock()
	delete(mw.watchers, "second")
	mw.mu.Unlock()
	removeTopologyWatcher(mw, "first")
	removeTopologyWatcher(mw, "second")
}

func TestMultiWatcherTopologyFamiliesRemainIndependent(t *testing.T) {
	first := newTopologyWatchFixture(t, 8)
	second := newTopologyWatchFixture(t, 8)
	mw := newTopologyRegistry()
	for i := 0; i < 8; i++ {
		installTopologyWatcher(mw, fmt.Sprintf("first-%02d", i), first.watcher(i))
		installTopologyWatcher(mw, fmt.Sprintf("second-%02d", i), second.watcher(i))
	}
	mw.mu.Lock()
	families := len(mw.topologyFamilies)
	owners := 0
	for _, family := range mw.topologyFamilies {
		if family.owner != "" {
			owners++
		}
	}
	mw.mu.Unlock()
	if families != 2 || owners != 2 {
		t.Fatalf("independent family state = families:%d owners:%d", families, owners)
	}
	if first.inventoryCalls.Load() != 1 || second.inventoryCalls.Load() != 1 {
		t.Fatalf("inventory calls = first:%d second:%d, want 1 each", first.inventoryCalls.Load(), second.inventoryCalls.Load())
	}
	for name, fixture := range map[string]*topologyWatchFixture{"first": first, "second": second} {
		if unique, duplicates, registrations := fixture.activeStats(); unique != 18 || duplicates != 0 || registrations != 18 {
			t.Fatalf("%s registrations = %d/%d/%d, want 18/0/18", name, unique, duplicates, registrations)
		}
	}
}

func TestMultiWatcherTopologyCanonicalizesCommonDirectoryAliases(t *testing.T) {
	fixture := newTopologyWatchFixture(t, 2)
	alias := filepath.Join(t.TempDir(), "common-alias")
	if err := os.Symlink(fixture.commonDir, alias); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	first := fixture.watcher(0)
	second := fixture.watcher(1)
	second.commonDir = alias
	second.worktreesDir = filepath.Join(alias, "worktrees")

	mw := newTopologyRegistry()
	installTopologyWatcher(mw, "first", first)
	installTopologyWatcher(mw, "second", second)
	families, _, members := topologyFamilySnapshot(mw)
	if families != 1 || members != 2 {
		t.Fatalf("aliased common dirs formed %d families with %d members", families, members)
	}
	if got := fixture.inventoryCalls.Load(); got != 1 {
		t.Fatalf("aliased common dirs ran %d inventories, want 1", got)
	}
}

func TestMultiWatcherTopologyStartFailureCannotClaimOwnership(t *testing.T) {
	fixture := newTopologyWatchFixture(t, 2)
	mw := newTopologyRegistry()
	mw.mu.Lock()
	mw.installStartedGitWatcherLocked("repo-00-failed", nil)
	mw.mu.Unlock()
	installTopologyWatcher(mw, "repo-01", fixture.watcher(0))
	installTopologyWatcher(mw, "repo-02", fixture.watcher(1))

	families, owner, members := topologyFamilySnapshot(mw)
	if families != 1 || owner != "repo-01" || members != 2 {
		t.Fatalf("post-failure family = families:%d owner:%q members:%d", families, owner, members)
	}
	if _, exists := mw.gitWatchers["repo-00-failed"]; exists {
		t.Fatal("failed watcher was installed")
	}
	if got := fixture.inventoryCalls.Load(); got != 1 {
		t.Fatalf("post-failure inventory calls = %d, want 1", got)
	}
}

func TestMultiWatcherTopologyRegistryConcurrentLifecycle(t *testing.T) {
	fixture := newTopologyWatchFixture(t, 16)
	mw := newTopologyRegistry()
	watchers := make([]*GitWatcher, 16)
	for i := range watchers {
		watchers[i] = fixture.watcher(i)
	}

	var wg sync.WaitGroup
	for i, watcher := range watchers {
		prefix := fmt.Sprintf("repo-%02d", i)
		wg.Add(1)
		go func() {
			defer wg.Done()
			for iteration := 0; iteration < 20; iteration++ {
				installTopologyWatcher(mw, prefix, watcher)
				removeTopologyWatcher(mw, prefix)
			}
		}()
	}
	wg.Wait()
	if families, _, members := topologyFamilySnapshot(mw); families != 0 || members != 0 {
		t.Fatalf("concurrent lifecycle left families:%d members:%d", families, members)
	}
	if unique, duplicates, registrations := fixture.activeStats(); unique != 0 || duplicates != 0 || registrations != 0 {
		t.Fatalf("concurrent lifecycle left topology paths: %d/%d/%d", unique, duplicates, registrations)
	}

	stopped := newTopologyRegistry()
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := stopped.Stop(); err != nil {
				t.Errorf("concurrent Stop: %v", err)
			}
		}()
	}
	wg.Wait()
	if err := stopped.Stop(); err != nil {
		t.Fatalf("idempotent Stop: %v", err)
	}
}

func resetTopologyWatcherForBenchmark(watcher *GitWatcher) {
	watcher.mu.Lock()
	if watcher.topologyTimer != nil {
		watcher.topologyTimer.Stop()
	}
	watcher.topologyTimer = nil
	watcher.topologyOwned = false
	watcher.topologyPaths = make(map[string]struct{})
	watcher.worktreeRoots = make(map[string]struct{})
	watcher.worktreeAdminDirs = make(map[string]struct{})
	watcher.mu.Unlock()
}

func newRealTopologyFamilyBenchmark(b *testing.B, prefixes int) (string, []string) {
	b.Helper()
	base := b.TempDir()
	repo := filepath.Join(base, "main")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		b.Fatal(err)
	}
	runTopologyBenchmarkGitCommand(b, repo, "init", "-q")
	if err := os.WriteFile(filepath.Join(repo, "seed.txt"), []byte("seed\n"), 0o644); err != nil {
		b.Fatal(err)
	}
	runTopologyBenchmarkGitCommand(b, repo, "add", "seed.txt")
	runTopologyBenchmarkGitCommand(b, repo,
		"-c", "user.name=Gortex Benchmark", "-c", "user.email=gortex@example.invalid",
		"commit", "-qm", "seed")

	roots := []string{repo}
	for i := 1; i < prefixes; i++ {
		root := filepath.Join(base, "linked", fmt.Sprintf("worktree-%03d", i))
		runTopologyBenchmarkGitCommand(b, repo, "worktree", "add", "-q", "-b", fmt.Sprintf("benchmark-%03d", i), root, "HEAD")
		roots = append(roots, root)
	}
	return filepath.Join(repo, ".git"), roots
}

func runTopologyBenchmarkGitCommand(b *testing.B, dir string, args ...string) {
	b.Helper()
	commandArgs := append([]string{"-C", dir}, args...)
	command := exec.Command("git", commandArgs...)
	if output, err := command.CombinedOutput(); err != nil {
		b.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, output)
	}
}

func BenchmarkMultiWatcherTopologyFamilyRealInventory(b *testing.B) {
	for _, prefixes := range []int{1, 8, 64} {
		b.Run(fmt.Sprintf("prefixes_%d", prefixes), func(b *testing.B) {
			commonDir, roots := newRealTopologyFamilyBenchmark(b, prefixes)
			var inventoryCalls atomic.Int64
			watchers := make([]*GitWatcher, prefixes)
			for i := range watchers {
				watchers[i] = &GitWatcher{
					repoPath:          roots[i],
					logger:            zap.NewNop(),
					commonDir:         commonDir,
					worktreesDir:      filepath.Join(commonDir, "worktrees"),
					topologyPaths:     make(map[string]struct{}),
					worktreeRoots:     make(map[string]struct{}),
					worktreeAdminDirs: make(map[string]struct{}),
					inventory: func(ctx context.Context, dir string) (*gitstate.FamilyInventory, error) {
						inventoryCalls.Add(1)
						return gitstate.Inventory(ctx, dir)
					},
					topologyAdd:    func(string) error { return nil },
					topologyRemove: func(string) error { return nil },
				}
			}

			inventoryCalls.Store(0)
			b.ReportAllocs()
			b.ResetTimer()
			for iteration := 0; iteration < b.N; iteration++ {
				mw := newTopologyRegistry()
				for i, watcher := range watchers {
					resetTopologyWatcherForBenchmark(watcher)
					installTopologyWatcher(mw, fmt.Sprintf("repo-%03d", i), watcher)
				}
			}
			b.StopTimer()

			inventories := inventoryCalls.Load()
			if inventories != int64(b.N) {
				b.Fatalf("inventory calls = %d, want %d", inventories, b.N)
			}
			owned, paths := watcherTopologySnapshot(watchers[0])
			if !owned {
				b.Fatal("first successfully installed watcher was not owner")
			}
			expectedPaths := 2
			if prefixes > 1 {
				expectedPaths = 2*prefixes + 1
			}
			if paths != expectedPaths {
				b.Fatalf("owner paths = %d, want %d", paths, expectedPaths)
			}
			for _, follower := range watchers[1:] {
				if owned, paths := watcherTopologySnapshot(follower); owned || paths != 0 {
					b.Fatalf("follower state = owned:%t paths:%d", owned, paths)
				}
			}
			b.ReportMetric(float64(inventories)/float64(b.N), "inventory/op")
			b.ReportMetric(float64(paths), "topology-paths/op")
			b.ReportMetric(0, "duplicate-paths/op")
		})
	}
}

func BenchmarkMultiWatcherEnsureMissingFamily(b *testing.B) {
	for _, prefixes := range []int{1, 8, 64} {
		b.Run(fmt.Sprintf("prefixes_%d", prefixes), func(b *testing.B) {
			_, roots := newRealTopologyFamilyBenchmark(b, prefixes)
			base := b.TempDir()
			cm, err := config.NewConfigManager(filepath.Join(base, "config.yaml"))
			if err != nil {
				b.Fatalf("create config manager: %v", err)
			}
			entries := make([]config.RepoEntry, prefixes)
			watchConfigs := make(map[string]config.WatchConfig, prefixes)
			for i, root := range roots {
				prefix := fmt.Sprintf("repo-%03d", i)
				entries[i] = config.RepoEntry{Path: root, Name: prefix}
				watchConfigs[prefix] = config.WatchConfig{
					Enabled: true, DebounceMs: 50, StormThreshold: -1,
				}
			}
			cm.Global().Repos = entries
			if err := cm.Global().Save(); err != nil {
				b.Fatalf("save benchmark config: %v", err)
			}
			g := graph.New()
			registry := parser.NewRegistry()
			registry.Register(languages.NewGoExtractor())
			mi := NewMultiIndexer(g, registry, search.NewNull(), cm, zap.NewNop())
			if _, err := mi.IndexAll(); err != nil {
				b.Fatalf("index benchmark family: %v", err)
			}
			b.Cleanup(func() { _ = mi.Close(context.Background()) })

			expectedPaths := 2 * prefixes
			if prefixes > 1 {
				expectedPaths++ // the shared worktrees directory exists only with linked worktrees
			}
			b.ReportAllocs()
			b.ResetTimer()
			for iteration := 0; iteration < b.N; iteration++ {
				mw, err := NewMultiWatcher(mi, map[string]config.WatchConfig{}, zap.NewNop())
				if err != nil {
					b.Fatalf("create empty multi-watcher: %v", err)
				}
				if err := mw.Start(); err != nil {
					b.Fatalf("start empty multi-watcher: %v", err)
				}
				for i := 0; i < prefixes; i++ {
					prefix := fmt.Sprintf("repo-%03d", i)
					if err := mw.EnsureRepoContext(context.Background(), prefix, watchConfigs[prefix]); err != nil {
						b.Fatalf("ensure %s: %v", prefix, err)
					}
				}
				b.StopTimer()

				families, owner, members := topologyFamilySnapshot(mw)
				if families != 1 || owner == "" || members != prefixes {
					b.Fatalf("family state = families:%d owner:%q members:%d, want 1/nonempty/%d",
						families, owner, members, prefixes)
				}
				mw.mu.Lock()
				ownerWatcher := mw.gitWatchers[owner]
				liveWatchers := len(mw.watchers)
				liveGitWatchers := len(mw.gitWatchers)
				liveForwarders := len(mw.forwarders)
				mw.mu.Unlock()
				owned, paths := watcherTopologySnapshot(ownerWatcher)
				if !owned || paths != expectedPaths || liveWatchers != prefixes ||
					liveGitWatchers != prefixes || liveForwarders != prefixes {
					b.Fatalf("live state = owned:%t paths:%d watchers:%d git:%d forwarders:%d, want true/%d/%d/%d/%d",
						owned, paths, liveWatchers, liveGitWatchers, liveForwarders,
						expectedPaths, prefixes, prefixes, prefixes)
				}
				for i := 0; i < prefixes; i++ {
					prefix := fmt.Sprintf("repo-%03d", i)
					if err := mw.RemoveRepoContext(context.Background(), prefix); err != nil {
						b.Fatalf("remove %s: %v", prefix, err)
					}
				}
				if err := mw.Stop(); err != nil {
					b.Fatalf("stop multi-watcher: %v", err)
				}
				mw.mu.Lock()
				leakedWatchers := len(mw.watchers)
				leakedGitWatchers := len(mw.gitWatchers)
				leakedForwarders := len(mw.forwarders)
				leakedRetirements := len(mw.retiringWatchers)
				leakedPending := len(mw.pendingWatcherAdds)
				leakedFamilies := len(mw.topologyFamilies)
				mw.mu.Unlock()
				if leakedWatchers+leakedGitWatchers+leakedForwarders+leakedRetirements+leakedPending+leakedFamilies != 0 {
					b.Fatalf("teardown leaks = watchers:%d git:%d forwarders:%d retirements:%d pending:%d families:%d",
						leakedWatchers, leakedGitWatchers, leakedForwarders,
						leakedRetirements, leakedPending, leakedFamilies)
				}
				b.StartTimer()
			}
			b.StopTimer()
			b.ReportMetric(float64(prefixes), "watchers/op")
			b.ReportMetric(float64(expectedPaths), "topology-paths/op")
			b.ReportMetric(0, "teardown-leaks/op")
		})
	}
}

func BenchmarkMultiWatcherTopologyFamilyRegistration(b *testing.B) {
	for _, prefixes := range []int{1, 8, 64} {
		b.Run(fmt.Sprintf("prefixes_%d", prefixes), func(b *testing.B) {
			fixture := newTopologyWatchFixture(b, prefixes)
			watchers := make([]*GitWatcher, prefixes)
			for i := range watchers {
				watchers[i] = fixture.watcher(i)
			}
			fixture.inventoryCalls.Store(0)
			fixture.registrationCalls.Store(0)

			b.ReportAllocs()
			b.ResetTimer()
			for iteration := 0; iteration < b.N; iteration++ {
				fixture.resetActive()
				mw := newTopologyRegistry()
				for i, watcher := range watchers {
					resetTopologyWatcherForBenchmark(watcher)
					installTopologyWatcher(mw, fmt.Sprintf("repo-%03d", i), watcher)
				}
			}
			b.StopTimer()

			inventories := fixture.inventoryCalls.Load()
			registrations := fixture.registrationCalls.Load()
			expectedPaths := 2*prefixes + 2
			unique, duplicates, active := fixture.activeStats()
			if inventories != int64(b.N) {
				b.Fatalf("inventory calls = %d, want %d", inventories, b.N)
			}
			if unique != expectedPaths || duplicates != 0 || active != expectedPaths {
				b.Fatalf("final registrations = %d/%d/%d, want %d/0/%d", unique, duplicates, active, expectedPaths, expectedPaths)
			}
			b.ReportMetric(float64(inventories)/float64(b.N), "inventory/op")
			b.ReportMetric(float64(registrations)/float64(b.N), "topology-paths/op")
			b.ReportMetric(0, "duplicate-paths/op")
		})
	}
}

func setupMultiWatcherBenchmark(b *testing.B) *MultiWatcher {
	b.Helper()
	base := b.TempDir()
	repoADir := filepath.Join(base, "repo-a")
	repoBDir := filepath.Join(base, "repo-b")
	for _, dir := range []string{repoADir, repoBDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			b.Fatalf("create repo dir: %v", err)
		}
	}
	if err := os.WriteFile(filepath.Join(repoADir, "main.go"), []byte("package main\n\nfunc HelloA() {}\n"), 0o644); err != nil {
		b.Fatalf("write repo-a: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repoBDir, "main.go"), []byte("package main\n\nfunc HelloB() {}\n"), 0o644); err != nil {
		b.Fatalf("write repo-b: %v", err)
	}

	cm, err := config.NewConfigManager(filepath.Join(base, "config.yaml"))
	if err != nil {
		b.Fatalf("create config manager: %v", err)
	}
	cm.Global().Repos = []config.RepoEntry{
		{Path: repoADir, Name: "repo-a"},
		{Path: repoBDir, Name: "repo-b"},
	}
	if err := cm.Global().Save(); err != nil {
		b.Fatalf("save benchmark config: %v", err)
	}

	g := graph.New()
	reg := parser.NewRegistry()
	reg.Register(languages.NewGoExtractor())
	mi := NewMultiIndexer(g, reg, search.NewNull(), cm, zap.NewNop())
	if _, err := mi.IndexAll(); err != nil {
		b.Fatalf("index benchmark repos: %v", err)
	}
	configs := map[string]config.WatchConfig{
		"repo-a": {Enabled: true, DebounceMs: 50, Exclude: []string{"**/*.tmp"}},
		"repo-b": {Enabled: true, DebounceMs: 50, Exclude: []string{"**/*.tmp"}},
	}
	mw, err := NewMultiWatcher(mi, configs, zap.NewNop())
	if err != nil {
		b.Fatalf("create multi-watcher: %v", err)
	}
	return mw
}

func BenchmarkMultiWatcherPendingAddAdmission(b *testing.B) {
	mw := setupMultiWatcherBenchmark(b)
	if err := mw.Start(); err != nil {
		b.Fatalf("start multi-watcher: %v", err)
	}
	if err := mw.RemoveRepo("repo-a"); err != nil {
		b.Fatalf("remove initial watcher: %v", err)
	}
	admitted := make(chan struct{})
	release := make(chan struct{})
	removed := make(chan error, 1)
	mw.pendingAddAdmitted = func() {
		admitted <- struct{}{}
		<-release
	}
	cfg := config.WatchConfig{Enabled: true, DebounceMs: 50, Exclude: []string{"**/*.tmp"}}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		mw.mu.Lock()
		mw.queuePendingWatcherAddLocked("repo-a", cfg)
		pending := mw.pendingWatcherAdds["repo-a"]
		mw.mu.Unlock()
		<-admitted
		go func() { removed <- mw.RemoveRepo("repo-a") }()
		release <- struct{}{}
		if err := <-removed; err != nil {
			b.Fatalf("remove admitted watcher: %v", err)
		}
		<-pending.done
	}
	b.StopTimer()

	mw.mu.Lock()
	watcher := mw.watchers["repo-a"]
	gitWatcher := mw.gitWatchers["repo-a"]
	pending := mw.pendingWatcherAdds["repo-a"]
	forwarder := mw.forwarders["repo-a"]
	retirement := mw.retiringWatchers["repo-a"]
	started := mw.started["repo-a"]
	mw.mu.Unlock()
	if watcher != nil || gitWatcher != nil || pending != nil || forwarder != nil || retirement != nil || started {
		b.Fatalf("admission lifecycle leaked repo-a state: watcher:%v git:%v pending:%v forwarder:%v retirement:%v started:%v", watcher != nil, gitWatcher != nil, pending != nil, forwarder != nil, retirement != nil, started)
	}
	if err := mw.Stop(); err != nil {
		b.Fatalf("stop multi-watcher: %v", err)
	}
	b.ReportMetric(1, "pending-admit-remove/op")
}

func BenchmarkMultiWatcherPendingAddCancellation(b *testing.B) {
	mw := newTopologyRegistry()
	claimed := make(chan struct{})
	release := make(chan struct{})
	mw.pendingAddClaimed = func() {
		claimed <- struct{}{}
		<-release
	}
	cfg := config.WatchConfig{Enabled: true, DebounceMs: 50}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		mw.mu.Lock()
		mw.queuePendingWatcherAddLocked("repo-00", cfg)
		pending := mw.pendingWatcherAdds["repo-00"]
		mw.mu.Unlock()
		<-claimed
		if err := mw.RemoveRepo("repo-00"); err != nil {
			b.Fatalf("cancel pending watcher add: %v", err)
		}
		release <- struct{}{}
		<-pending.done
	}
	b.StopTimer()

	mw.mu.Lock()
	watchers := len(mw.watchers)
	gitWatchers := len(mw.gitWatchers)
	pending := len(mw.pendingWatcherAdds)
	forwarders := len(mw.forwarders)
	retirements := len(mw.retiringWatchers)
	mw.mu.Unlock()
	if watchers != 0 || gitWatchers != 0 || pending != 0 || forwarders != 0 || retirements != 0 {
		b.Fatalf("pending cancellation leaked state: watchers:%d git:%d pending:%d forwarders:%d retirements:%d", watchers, gitWatchers, pending, forwarders, retirements)
	}
	b.ReportMetric(1, "pending-cancel/op")
}

func BenchmarkMultiWatcherTopologyHandoffCompletion(b *testing.B) {
	b.Run("zero_flight", func(b *testing.B) {
		fixture := newTopologyWatchFixture(b, 2)
		mw := newTopologyRegistry()
		oldOwner := fixture.watcher(0)
		newOwner := fixture.watcher(1)
		oldOwner.debounce = 0
		newOwner.debounce = 0
		nudged := make(chan struct{}, 1)
		var nudges atomic.Int64
		mw.OnWorktreeChange(func(prefix, _ string) {
			if prefix == "repo-01" {
				nudges.Add(1)
				nudged <- struct{}{}
			}
		})

		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			installTopologyWatcher(mw, "repo-00", oldOwner)
			installTopologyWatcher(mw, "repo-01", newOwner)
			mw.mu.Lock()
			family := mw.topologyFamilies[canonicalGitCommonDir(fixture.commonDir)]
			if family == nil || family.dispatch == nil || family.dispatch.inFlight != 0 {
				mw.mu.Unlock()
				b.Fatal("zero-flight owner epoch was not ready")
			}
			drain := mw.unregisterTopologyWatcherLocked("repo-00")
			delete(mw.gitWatchers, "repo-00")
			mw.mu.Unlock()
			waitTopologyDispatchDrains(drain)
			<-nudged
			removeTopologyWatcher(mw, "repo-01")
		}
		b.StopTimer()

		wantInventory := int64(3 * b.N)
		if got := fixture.inventoryCalls.Load(); got != wantInventory {
			b.Fatalf("inventory calls = %d, want %d", got, wantInventory)
		}
		if got := nudges.Load(); got != int64(b.N) {
			b.Fatalf("owner-transfer nudges = %d, want %d", got, b.N)
		}
		mw.mu.Lock()
		families := len(mw.topologyFamilies)
		reverse := len(mw.topologyFamilyByRepo)
		dispatches := len(mw.topologyDispatches)
		watchers := len(mw.gitWatchers)
		mw.mu.Unlock()
		if families != 0 || reverse != 0 || dispatches != 0 || watchers != 0 {
			b.Fatalf("topology state leaked: families:%d reverse:%d dispatches:%d watchers:%d", families, reverse, dispatches, watchers)
		}
		if unique, duplicates, registrations := fixture.activeStats(); unique != 0 || duplicates != 0 || registrations != 0 {
			b.Fatalf("topology paths leaked: unique:%d duplicates:%d registrations:%d", unique, duplicates, registrations)
		}
		b.ReportMetric(3, "inventory/op")
		b.ReportMetric(1, "owner-transfer-nudge/op")
	})

	b.Run("retained", func(b *testing.B) {
		fixture := newTopologyWatchFixture(b, 2)
		mw := newTopologyRegistry()
		oldOwner := fixture.watcher(0)
		newOwner := fixture.watcher(1)
		oldOwner.debounce = 0
		newOwner.debounce = 0
		retained := make(chan func(), 1)
		nudged := make(chan struct{}, 1)
		var nudges atomic.Int64
		mw.OnWorktreeChangeContext(func(ctx context.Context, prefix, _ string) {
			switch prefix {
			case "repo-00":
				_, release := mw.RetainTopologyDispatch(ctx)
				retained <- release
			case "repo-01":
				nudges.Add(1)
				nudged <- struct{}{}
			}
		})

		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			installTopologyWatcher(mw, "repo-00", oldOwner)
			installTopologyWatcher(mw, "repo-01", newOwner)
			oldEpoch := topologyDispatchEpochSnapshot(b, mw, "repo-00")
			dispatch, admitted := mw.admitWorktreeTopologyChange("repo-00", oldOwner, oldEpoch, oldOwner.repoPath)
			if !admitted {
				b.Fatal("old owner callback was not admitted")
			}
			dispatch.invoke()
			release := <-retained

			mw.mu.Lock()
			drain := mw.unregisterTopologyWatcherLocked("repo-00")
			delete(mw.gitWatchers, "repo-00")
			family := mw.topologyFamilies[canonicalGitCommonDir(fixture.commonDir)]
			if family == nil || family.owner != "" || family.dispatch != nil || family.handoff != oldEpoch {
				mw.mu.Unlock()
				b.Fatal("retained epoch did not enter topology handoff")
			}
			mw.mu.Unlock()
			release()
			waitTopologyDispatchDrains(drain)
			<-nudged
			removeTopologyWatcher(mw, "repo-01")
		}
		b.StopTimer()

		wantInventory := int64(3 * b.N)
		if got := fixture.inventoryCalls.Load(); got != wantInventory {
			b.Fatalf("inventory calls = %d, want %d", got, wantInventory)
		}
		if got := nudges.Load(); got != int64(b.N) {
			b.Fatalf("owner-transfer nudges = %d, want %d", got, b.N)
		}
		mw.mu.Lock()
		families := len(mw.topologyFamilies)
		reverse := len(mw.topologyFamilyByRepo)
		dispatches := len(mw.topologyDispatches)
		watchers := len(mw.gitWatchers)
		mw.mu.Unlock()
		if families != 0 || reverse != 0 || dispatches != 0 || watchers != 0 {
			b.Fatalf("topology state leaked: families:%d reverse:%d dispatches:%d watchers:%d", families, reverse, dispatches, watchers)
		}
		if unique, duplicates, registrations := fixture.activeStats(); unique != 0 || duplicates != 0 || registrations != 0 {
			b.Fatalf("topology paths leaked: unique:%d duplicates:%d registrations:%d", unique, duplicates, registrations)
		}
		b.ReportMetric(3, "inventory/op")
		b.ReportMetric(1, "owner-transfer-nudge/op")
	})
}

func BenchmarkMultiWatcherTopologyRetainedDispatch(b *testing.B) {
	fixture := newTopologyWatchFixture(b, 1)
	mw := newTopologyRegistry()
	mw.OnWorktreeChangeContext(func(ctx context.Context, _, _ string) {
		_, release := mw.RetainTopologyDispatch(ctx)
		release()
	})
	watcher := fixture.watcher(0)
	installTopologyWatcher(mw, "repo-00", watcher)
	epoch := topologyDispatchEpochSnapshot(b, mw, "repo-00")
	fixture.inventoryCalls.Store(0)

	b.ReportAllocs()
	b.ResetTimer()
	for iteration := 0; iteration < b.N; iteration++ {
		dispatch, admitted := mw.admitWorktreeTopologyChange("repo-00", watcher, epoch, watcher.repoPath)
		if !admitted {
			b.Fatal("topology callback was not admitted")
		}
		dispatch.invoke()
	}
	b.StopTimer()

	mw.mu.Lock()
	inFlight := epoch.inFlight
	mw.mu.Unlock()
	if inFlight != 0 {
		b.Fatalf("retained dispatches leaked %d leases", inFlight)
	}
	if inventories := fixture.inventoryCalls.Load(); inventories != 0 {
		b.Fatalf("retained dispatch ran %d inventories, want 0", inventories)
	}
	b.ReportMetric(0, "inventory/op")
	b.ReportMetric(1, "retained-dispatch/op")
}

func BenchmarkMultiWatcherTopologyDispatchDrain(b *testing.B) {
	fixture := newTopologyWatchFixture(b, 1)
	mw := newTopologyRegistry()
	mw.OnWorktreeChange(func(string, string) {})
	watcher := fixture.watcher(0)
	installTopologyWatcher(mw, "repo-00", watcher)
	fixture.inventoryCalls.Store(0)

	b.ReportAllocs()
	b.ResetTimer()
	for iteration := 0; iteration < b.N; iteration++ {
		mw.mu.Lock()
		family := mw.topologyFamilies[mw.topologyFamilyByRepo["repo-00"]]
		epoch := family.dispatch
		mw.mu.Unlock()

		dispatch, admitted := mw.admitWorktreeTopologyChange("repo-00", watcher, epoch, watcher.repoPath)
		if !admitted {
			b.Fatal("topology callback was not admitted")
		}
		mw.mu.Lock()
		drain := mw.invalidateTopologyDispatchEpochLocked(family)
		mw.electTopologyOwnerLocked(family)
		mw.mu.Unlock()
		dispatch.invoke()
		waitTopologyDispatchDrains(drain)
	}
	b.StopTimer()

	if inventories := fixture.inventoryCalls.Load(); inventories != 0 {
		b.Fatalf("dispatch drain ran %d inventories, want 0", inventories)
	}
	b.ReportMetric(0, "inventory/op")
	b.ReportMetric(1, "dispatch-drain/op")
}
