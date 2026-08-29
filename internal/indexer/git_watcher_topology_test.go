package indexer

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"
)

func TestGitWatcherSignalsWorktreeAddAndRemove(t *testing.T) {
	repo := t.TempDir()
	runTopologyGitCommand(t, repo, "init", "-q")
	if err := os.WriteFile(filepath.Join(repo, "seed.txt"), []byte("seed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runTopologyGitCommand(t, repo, "add", "seed.txt")
	runTopologyGitCommand(t, repo,
		"-c", "user.name=Gortex Test", "-c", "user.email=gortex@example.invalid",
		"commit", "-qm", "seed")

	watcher, err := NewGitWatcher(repo, nil, zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	watcher.debounce = 25 * time.Millisecond
	var callbacks atomic.Int64
	signals := make(chan struct{}, 16)
	watcher.OnWorktreeChange(func(root string) {
		if filepath.Clean(root) != filepath.Clean(repo) {
			t.Errorf("callback root = %q, want %q", root, repo)
		}
		callbacks.Add(1)
		select {
		case signals <- struct{}{}:
		default:
		}
	})
	if err := watcher.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = watcher.Stop() })

	worktree := filepath.Join(t.TempDir(), "linked")
	beforeAdd := callbacks.Load()
	runTopologyGitCommand(t, repo, "worktree", "add", "-q", "-b", "watcher-topology-test", worktree)
	waitForTopologyCallback(t, &callbacks, signals, beforeAdd)
	waitForTopologyQuiet(t, signals, 4*watcher.debounce)

	beforeRemove := callbacks.Load()
	runTopologyGitCommand(t, repo, "worktree", "remove", "--force", worktree)
	waitForTopologyCallback(t, &callbacks, signals, beforeRemove)
}

func waitForTopologyCallback(t *testing.T, count *atomic.Int64, signals <-chan struct{}, after int64) {
	t.Helper()
	timer := time.NewTimer(10 * time.Second)
	defer timer.Stop()
	for count.Load() <= after {
		select {
		case <-signals:
		case <-timer.C:
			t.Fatalf("timed out waiting for worktree topology callback after %d", after)
		}
	}
}

func waitForTopologyQuiet(t *testing.T, signals <-chan struct{}, quiet time.Duration) {
	t.Helper()
	timer := time.NewTimer(quiet)
	defer timer.Stop()
	for {
		select {
		case <-signals:
			if !timer.Stop() {
				<-timer.C
			}
			timer.Reset(quiet)
		case <-timer.C:
			return
		}
	}
}

func runTopologyGitCommand(t *testing.T, dir string, args ...string) {
	t.Helper()
	commandArgs := append([]string{"-C", dir}, args...)
	cmd := exec.Command("git", commandArgs...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, output)
	}
}
