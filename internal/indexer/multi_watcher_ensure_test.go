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
	"testing"
	"time"

	"github.com/zzet/gortex/internal/config"
)

func TestMultiWatcherEnsureRepoContextPreservesStrictAddContract(t *testing.T) {
	const prefix = "/test/repo"
	mw := &MultiWatcher{
		watchers: map[string]*Watcher{prefix: {}},
		started:  map[string]bool{prefix: true},
	}

	if err := mw.EnsureRepoContext(context.Background(), prefix, config.WatchConfig{}); err != nil {
		t.Fatalf("EnsureRepoContext(existing) error = %v", err)
	}
	if err := mw.AddRepoContext(context.Background(), prefix, config.WatchConfig{}); err == nil {
		t.Fatal("AddRepoContext(existing) error = nil, want strict duplicate error")
	} else if !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("AddRepoContext(existing) error = %v, want duplicate error", err)
	}
	if got := len(mw.watchers); got != 1 {
		t.Fatalf("watcher count = %d, want 1", got)
	}
}

func TestMultiWatcherEnsureRepoContextReplacesConfiguredFailedWatcher(t *testing.T) {
	mw, _, _, _ := setupMultiWatcherTest(t)
	t.Cleanup(func() { _ = mw.Stop() })

	mw.mu.Lock()
	old := mw.watchers["repo-a"]
	mw.started["repo-a"] = false
	mw.startFailures["repo-a"] = "injected startup failure"
	mw.mu.Unlock()

	if err := mw.EnsureRepoContext(context.Background(), "repo-a", config.WatchConfig{
		Enabled: true, DebounceMs: 50,
	}); err != nil {
		t.Fatalf("EnsureRepoContext(failed configured watcher): %v", err)
	}

	mw.mu.Lock()
	replacement := mw.watchers["repo-a"]
	started := mw.started["repo-a"]
	_, failed := mw.startFailures["repo-a"]
	_, forwarding := mw.forwarders["repo-a"]
	mw.mu.Unlock()
	if replacement == nil || replacement == old || !started || failed || !forwarding {
		t.Fatalf("replacement state = watcher:%p old:%p started:%t failed:%t forwarding:%t",
			replacement, old, started, failed, forwarding)
	}
	live, _ := mw.WatchedRepos()
	if live != 1 {
		t.Fatalf("live watchers = %d, want 1", live)
	}
}

func TestMultiWatcherEnsureRepoContextRejectsStoppedWatcher(t *testing.T) {
	mw := &MultiWatcher{stopped: true}

	if err := mw.EnsureRepoContext(context.Background(), "/test/repo", config.WatchConfig{}); err == nil {
		t.Fatal("EnsureRepoContext(stopped) error = nil")
	}
}

func TestMultiWatcherEnsureRepoContextWaitsForRetirementThenPublishesOne(t *testing.T) {
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

	ensureCtx, cancelEnsure := context.WithCancel(context.Background())
	t.Cleanup(cancelEnsure)
	ensured := make(chan error, 1)
	go func() {
		ensured <- mw.EnsureRepoContext(ensureCtx, "repo-a", config.WatchConfig{
			Enabled: true, DebounceMs: 50,
		})
	}()
	assertTopologyLifecycleBlocked(t, ensured, "EnsureRepoContext during prefix retirement")
	cancelEnsure()
	assertTopologyLifecycleBlocked(t, ensured, "ordinary EnsureRepoContext after caller cancellation")

	mw.mu.Lock()
	_, activeBeforeRelease := mw.watchers["repo-a"]
	retirementBeforeRelease := mw.retiringWatchers["repo-a"]
	mw.mu.Unlock()
	if activeBeforeRelease || retirementBeforeRelease == nil {
		t.Fatalf("retirement boundary = active:%t tombstone:%t, want false/true",
			activeBeforeRelease, retirementBeforeRelease != nil)
	}

	releaseOld.Do(func() { close(stopRelease) })
	if err := <-removed; err != nil {
		t.Fatalf("RemoveRepo: %v", err)
	}
	if err := <-ensured; err != nil {
		t.Fatalf("EnsureRepoContext: %v", err)
	}

	mw.mu.Lock()
	replacement := mw.watchers["repo-a"]
	_, retiring := mw.retiringWatchers["repo-a"]
	_, forwarding := mw.forwarders["repo-a"]
	mw.mu.Unlock()
	if replacement == nil || replacement == old || retiring || !forwarding {
		t.Fatalf("replacement state = watcher:%p old:%p retiring:%t forwarding:%t",
			replacement, old, retiring, forwarding)
	}
}

func TestMultiWatcherBoundedRepairContextCancelsRetirementWait(t *testing.T) {
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

	baseCtx, cancel := context.WithCancel(context.Background())
	boundedCtx := context.WithValue(baseCtx, boundedWatcherRepairContextKey{}, struct{}{})
	ensured := make(chan error, 1)
	go func() {
		ensured <- mw.EnsureRepoContext(boundedCtx, "repo-a", config.WatchConfig{
			Enabled: true, DebounceMs: 50,
		})
	}()
	assertTopologyLifecycleBlocked(t, ensured, "bounded repair during prefix retirement")
	cancel()
	select {
	case err := <-ensured:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("bounded repair error = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("bounded repair did not honor cancellation")
	}

	mw.mu.Lock()
	_, active := mw.watchers["repo-a"]
	retirement := mw.retiringWatchers["repo-a"]
	mw.mu.Unlock()
	if active || retirement == nil {
		t.Fatalf("canceled repair changed retirement = active:%t tombstone:%t", active, retirement != nil)
	}
	releaseOld.Do(func() { close(stopRelease) })
	if err := <-removed; err != nil {
		t.Fatalf("RemoveRepo: %v", err)
	}
	assertWatcherPrefixAbsent(t, mw, "repo-a")
}

func TestMultiWatcherStopCancelsEnsureWaitingForRetirement(t *testing.T) {
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

	ensured := make(chan error, 1)
	go func() {
		ensured <- mw.EnsureRepoContext(context.Background(), "repo-a", config.WatchConfig{
			Enabled: true, DebounceMs: 50,
		})
	}()
	assertTopologyLifecycleBlocked(t, ensured, "EnsureRepoContext during prefix retirement")

	stopped := make(chan error, 1)
	go func() { stopped <- mw.Stop() }()
	assertTopologyLifecycleBlocked(t, ensured, "EnsureRepoContext while Stop joins retirement")
	assertTopologyLifecycleBlocked(t, stopped, "Stop while physical retirement is blocked")

	releaseOld.Do(func() { close(stopRelease) })
	if err := <-removed; err != nil {
		t.Fatalf("RemoveRepo: %v", err)
	}
	if err := <-ensured; err == nil {
		t.Fatal("EnsureRepoContext returned nil after Stop")
	}
	if err := <-stopped; err != nil {
		t.Fatalf("Stop: %v", err)
	}
	assertWatcherPrefixAbsent(t, mw, "repo-a")
}

func TestMultiWatcherEnsureRepoContextPlainRootStaysFileOnly(t *testing.T) {
	mw, _, _, _ := setupMultiWatcherTest(t)
	t.Cleanup(func() { _ = mw.Stop() })
	if err := mw.RemoveRepo("repo-a"); err != nil {
		t.Fatalf("RemoveRepo: %v", err)
	}
	if err := mw.EnsureRepoContext(context.Background(), "repo-a", config.WatchConfig{
		Enabled: true, DebounceMs: 50,
	}); err != nil {
		t.Fatalf("EnsureRepoContext(plain root): %v", err)
	}
	mw.mu.Lock()
	started := mw.started["repo-a"]
	_, gitLive := mw.gitWatchers["repo-a"]
	_, topologyLive := mw.topologyFamilyByRepo["repo-a"]
	_, forwarding := mw.forwarders["repo-a"]
	mw.mu.Unlock()
	if !started || !forwarding || gitLive || topologyLive {
		t.Fatalf("plain membership = started:%t forwarding:%t git:%t topology:%t",
			started, forwarding, gitLive, topologyLive)
	}
}

func TestMultiWatcherEnsureRepoContextMalformedGitRollsBackFreshAdmission(t *testing.T) {
	mw, _, repoRoot, _ := setupMultiWatcherTest(t)
	t.Cleanup(func() { _ = mw.Stop() })
	if err := mw.RemoveRepo("repo-a"); err != nil {
		t.Fatalf("RemoveRepo: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repoRoot, ".git"), []byte("malformed gitdir\n"), 0o644); err != nil {
		t.Fatalf("write malformed .git: %v", err)
	}
	if err := mw.EnsureRepoContext(context.Background(), "repo-a", config.WatchConfig{
		Enabled: true, DebounceMs: 50,
	}); err == nil {
		t.Fatal("EnsureRepoContext(malformed .git) error = nil")
	}
	mw.mu.Lock()
	_, watcherLive := mw.watchers["repo-a"]
	_, started := mw.started["repo-a"]
	_, failed := mw.startFailures["repo-a"]
	_, forwarding := mw.forwarders["repo-a"]
	_, gitLive := mw.gitWatchers["repo-a"]
	_, topologyLive := mw.topologyFamilyByRepo["repo-a"]
	mw.mu.Unlock()
	if watcherLive || started || !failed || forwarding || gitLive || topologyLive {
		t.Fatalf("failed Git admission leaked runtime state: watcher:%t started:%t failed:%t forwarding:%t git:%t topology:%t",
			watcherLive, started, failed, forwarding, gitLive, topologyLive)
	}
}

func TestMultiWatcherEnsureRepoContextMalformedGitKeepsLiveWatcherAndRepairs(t *testing.T) {
	mw, _, repoRoot, _ := setupMultiWatcherTest(t)
	t.Cleanup(func() { _ = mw.Stop() })
	if err := mw.RemoveRepo("repo-a"); err != nil {
		t.Fatalf("RemoveRepo: %v", err)
	}
	if err := mw.EnsureRepoContext(context.Background(), "repo-a", config.WatchConfig{
		Enabled: true, DebounceMs: 50,
	}); err != nil {
		t.Fatalf("EnsureRepoContext(plain root): %v", err)
	}
	mw.mu.Lock()
	liveFileWatcher := mw.watchers["repo-a"]
	mw.mu.Unlock()

	gitMarker := filepath.Join(repoRoot, ".git")
	if err := os.WriteFile(gitMarker, []byte("malformed gitdir\n"), 0o644); err != nil {
		t.Fatalf("write malformed .git: %v", err)
	}
	if err := mw.EnsureRepoContext(context.Background(), "repo-a", config.WatchConfig{
		Enabled: true, DebounceMs: 50,
	}); err == nil {
		t.Fatal("EnsureRepoContext(malformed .git) error = nil")
	}
	mw.mu.Lock()
	afterFailure := mw.watchers["repo-a"]
	startedAfterFailure := mw.started["repo-a"]
	_, failed := mw.startFailures["repo-a"]
	_, forwardingAfterFailure := mw.forwarders["repo-a"]
	_, gitAfterFailure := mw.gitWatchers["repo-a"]
	_, topologyAfterFailure := mw.topologyFamilyByRepo["repo-a"]
	mw.mu.Unlock()
	if afterFailure != liveFileWatcher || !startedAfterFailure || !failed ||
		!forwardingAfterFailure || gitAfterFailure || topologyAfterFailure {
		t.Fatalf("failed topology repair disturbed live file watcher: before:%p after:%p started:%t failed:%t forwarding:%t git:%t topology:%t",
			liveFileWatcher, afterFailure, startedAfterFailure, failed,
			forwardingAfterFailure, gitAfterFailure, topologyAfterFailure)
	}

	if err := os.Remove(gitMarker); err != nil {
		t.Fatalf("remove malformed .git: %v", err)
	}
	runEnsureGit(t, repoRoot, "init", "-q")
	if err := os.WriteFile(filepath.Join(repoRoot, "tracked.txt"), []byte("tracked\n"), 0o644); err != nil {
		t.Fatalf("write tracked file: %v", err)
	}
	runEnsureGit(t, repoRoot, "add", "tracked.txt")
	runEnsureGit(t, repoRoot, "-c", "user.name=Gortex Test", "-c", "user.email=gortex@example.invalid", "commit", "-qm", "seed")
	if err := mw.EnsureRepoContext(context.Background(), "repo-a", config.WatchConfig{
		Enabled: true, DebounceMs: 50,
	}); err != nil {
		t.Fatalf("EnsureRepoContext(repaired Git root): %v", err)
	}
	mw.mu.Lock()
	afterRepair := mw.watchers["repo-a"]
	_, repairFailed := mw.startFailures["repo-a"]
	_, gitLive := mw.gitWatchers["repo-a"]
	_, topologyLive := mw.topologyFamilyByRepo["repo-a"]
	mw.mu.Unlock()
	if afterRepair != liveFileWatcher || repairFailed || !gitLive || !topologyLive {
		t.Fatalf("Git topology repair = watcher:%p want:%p failed:%t git:%t topology:%t",
			afterRepair, liveFileWatcher, repairFailed, gitLive, topologyLive)
	}
}

func runEnsureGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	commandArgs := append([]string{"-C", dir}, args...)
	output, err := exec.Command("git", commandArgs...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, output)
	}
}

func BenchmarkMultiWatcherEnsureExisting(b *testing.B) {
	for _, repos := range []int{1, 8, 64} {
		b.Run(fmt.Sprintf("repos=%d", repos), func(b *testing.B) {
			prefixes := make([]string, repos)
			watchers := make(map[string]*Watcher, repos)
			started := make(map[string]bool, repos)
			for i := range prefixes {
				prefixes[i] = fmt.Sprintf("/test/repo-%d", i)
				watchers[prefixes[i]] = &Watcher{}
				started[prefixes[i]] = true
			}
			mw := &MultiWatcher{watchers: watchers, started: started}
			ctx := context.Background()
			cfg := config.WatchConfig{}

			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if err := mw.EnsureRepoContext(ctx, prefixes[i%repos], cfg); err != nil {
					b.Fatalf("EnsureRepoContext(existing) error = %v", err)
				}
			}
			b.StopTimer()
			if got := len(mw.watchers); got != repos {
				b.Fatalf("watcher count = %d, want %d", got, repos)
			}
		})
	}
}
