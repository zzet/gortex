package indexer

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/config"
)

type retryTestWatcher struct {
	mu           sync.Mutex
	entered      chan struct{}
	release      chan struct{}
	done         chan struct{}
	ignoreCancel bool
	enterOnce    sync.Once
	doneOnce     sync.Once
	attached     map[string]bool
}

func newRetryTestWatcher(block bool, ignoreCancel bool) *retryTestWatcher {
	watcher := &retryTestWatcher{
		ignoreCancel: ignoreCancel,
		attached:     make(map[string]bool),
	}
	if block {
		watcher.entered = make(chan struct{})
		watcher.release = make(chan struct{})
		watcher.done = make(chan struct{})
	}
	return watcher
}

func (w *retryTestWatcher) AddRepo(prefix string, _ config.WatchConfig) error {
	return w.EnsureRepoContext(context.Background(), prefix, config.WatchConfig{})
}

func (w *retryTestWatcher) EnsureRepoContext(ctx context.Context, prefix string, _ config.WatchConfig) error {
	if w.entered != nil {
		w.enterOnce.Do(func() { close(w.entered) })
		if w.ignoreCancel {
			<-w.release
		} else {
			select {
			case <-w.release:
			case <-ctx.Done():
				w.doneOnce.Do(func() { close(w.done) })
				return ctx.Err()
			}
		}
	}
	w.mu.Lock()
	w.attached[prefix] = true
	w.mu.Unlock()
	if w.done != nil {
		w.doneOnce.Do(func() { close(w.done) })
	}
	return nil
}

func (w *retryTestWatcher) RemoveRepo(prefix string) error {
	w.mu.Lock()
	delete(w.attached, prefix)
	w.mu.Unlock()
	return nil
}

func (w *retryTestWatcher) isAttached(prefix string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.attached[prefix]
}

func startWatcherRetryForTest(t *testing.T, lifecycle *CheckoutLifecycle, prefix string) *watcherRetry {
	t.Helper()
	lifecycle.scheduleWatcherRetry(prefix)
	lifecycle.retryMu.Lock()
	retry := lifecycle.watcherRetries[prefix]
	if retry == nil {
		lifecycle.retryMu.Unlock()
		t.Fatal("watcher retry was not scheduled")
	}
	if retry.timer != nil {
		retry.timer.Stop()
	}
	lifecycle.retryMu.Unlock()
	go lifecycle.runWatcherRetry(prefix, retry)
	return retry
}

func TestCheckoutLifecycleEnsureTrackedWatcherReportsUnavailable(t *testing.T) {
	fixture := newLifecycleFixture(t)
	defer fixture.close()
	fixture.lc.SetWatcherSource(func() RepoWatcher { return nil })

	err := fixture.lc.EnsureTrackedWatcher(context.Background(), "missing-watcher")
	if !errors.Is(err, ErrWatcherUnavailable) {
		t.Fatalf("EnsureTrackedWatcher error = %v, want ErrWatcherUnavailable", err)
	}
	var unavailable *WatcherUnavailableError
	if !errors.As(err, &unavailable) || unavailable.Prefix != "missing-watcher" {
		t.Fatalf("typed unavailable error = %#v, want prefix missing-watcher", unavailable)
	}
}

func TestCheckoutLifecycleWatcherRetryCancelPreventsPrefixABA(t *testing.T) {
	fixture := newLifecycleFixture(t)
	defer fixture.close()
	root := fixture.gitRepo("watcher-retry-aba")
	registration, err := fixture.lc.Register(context.Background(), config.RepoEntry{
		Path: root, Name: "watcher-retry-aba",
	}, TrackSourceCLI)
	if err != nil {
		t.Fatalf("Register: %v", err)
	}

	oldWatcher := newRetryTestWatcher(true, false)
	fixture.lc.SetWatcherSource(func() RepoWatcher { return oldWatcher })
	startWatcherRetryForTest(t, fixture.lc, registration.Prefix)
	select {
	case <-oldWatcher.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("old watcher retry did not enter Ensure")
	}

	fixture.lc.detachWatcherContext(context.Background(), registration.Prefix)
	newWatcher := newRetryTestWatcher(false, false)
	fixture.lc.SetWatcherSource(func() RepoWatcher { return newWatcher })
	if err := fixture.lc.EnsureTrackedWatcher(context.Background(), registration.Prefix); err != nil {
		t.Fatalf("EnsureTrackedWatcher(replacement): %v", err)
	}
	select {
	case <-oldWatcher.done:
	case <-time.After(3 * time.Second):
		t.Fatal("canceled old retry did not return")
	}
	fixture.lc.retryWG.Wait()
	if oldWatcher.isAttached(registration.Prefix) {
		t.Fatal("canceled old retry attached the replacement prefix")
	}
	if !newWatcher.isAttached(registration.Prefix) {
		t.Fatal("replacement watcher was not attached")
	}
	fixture.lc.retryMu.Lock()
	_, pending := fixture.lc.watcherRetries[registration.Prefix]
	fixture.lc.retryMu.Unlock()
	if pending {
		t.Fatal("old retry rescheduled after prefix replacement")
	}
}

func TestCheckoutLifecycleCloseJoinsAdmittedWatcherRetry(t *testing.T) {
	fixture := newLifecycleFixture(t)
	defer fixture.close()
	root := fixture.gitRepo("watcher-retry-close")
	registration, err := fixture.lc.Register(context.Background(), config.RepoEntry{
		Path: root, Name: "watcher-retry-close",
	}, TrackSourceCLI)
	if err != nil {
		t.Fatalf("Register: %v", err)
	}

	blocked := newRetryTestWatcher(true, true)
	fixture.lc.SetWatcherSource(func() RepoWatcher { return blocked })
	startWatcherRetryForTest(t, fixture.lc, registration.Prefix)
	select {
	case <-blocked.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("watcher retry did not enter Ensure")
	}

	closed := make(chan error, 1)
	go func() { closed <- fixture.lc.Close() }()
	select {
	case err := <-closed:
		t.Fatalf("Close returned before admitted watcher retry finished: %v", err)
	case <-time.After(25 * time.Millisecond):
	}
	close(blocked.release)
	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Close did not join admitted watcher retry")
	}
	fixture.lc.retryMu.Lock()
	remaining := len(fixture.lc.watcherRetries)
	fixture.lc.retryMu.Unlock()
	if remaining != 0 {
		t.Fatalf("watcher retries after Close = %d, want 0", remaining)
	}
}

func TestCheckoutLifecycleEnsureConfiguredWatchersExcludesAutomaticOverlay(t *testing.T) {
	fixture := newLifecycleFixture(t)
	defer fixture.close()
	root := fixture.gitRepo("configured-watcher-filter")
	registration, err := fixture.lc.Register(context.Background(), config.RepoEntry{
		Path: root, Name: "configured-watcher-filter",
	}, TrackSourceCLI)
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	const automaticPrefix = "automatic-overlay"
	fixture.mi.mu.Lock()
	fixture.mi.repos[automaticPrefix] = &RepoMetadata{RootPath: filepath.Join(fixture.dir, "automatic")}
	fixture.mi.mu.Unlock()
	fixture.watcher.mu.Lock()
	fixture.watcher.attached = make(map[string]bool)
	fixture.watcher.calls = nil
	fixture.watcher.mu.Unlock()

	if err := fixture.lc.EnsureConfiguredWatchers(context.Background()); err != nil {
		t.Fatalf("EnsureConfiguredWatchers: %v", err)
	}
	if !fixture.watcher.isAttached(registration.Prefix) {
		t.Fatal("configured primary watcher was not repaired")
	}
	if fixture.watcher.isAttached(automaticPrefix) {
		t.Fatal("automatic overlay was over-admitted as an explicit watcher")
	}
}

func TestCheckoutLifecycleEnsureConfiguredWatchersIncludesProjectOnlyRepo(t *testing.T) {
	fixture := newLifecycleFixture(t)
	defer fixture.close()
	root := fixture.gitRepo("project-only-watcher")
	registration, err := fixture.lc.Register(context.Background(), config.RepoEntry{
		Path: root, Name: "project-only-watcher",
	}, TrackSourceCLI)
	if err != nil {
		t.Fatalf("Register: %v", err)
	}

	global := fixture.cm.Global()
	if err := global.RemoveRepo(root); err != nil {
		t.Fatalf("RemoveRepo(top-level): %v", err)
	}
	global.Projects = map[string]config.ProjectConfig{
		"project-only": {Repos: []config.RepoEntry{{Path: root, Name: "project-only-watcher"}}},
	}
	fixture.watcher.mu.Lock()
	fixture.watcher.attached = make(map[string]bool)
	fixture.watcher.calls = nil
	fixture.watcher.mu.Unlock()

	if err := fixture.lc.EnsureConfiguredWatchers(context.Background()); err != nil {
		t.Fatalf("EnsureConfiguredWatchers: %v", err)
	}
	if !fixture.watcher.isAttached(registration.Prefix) {
		t.Fatal("project-only explicit watcher was not repaired")
	}
}
