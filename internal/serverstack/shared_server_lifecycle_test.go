package serverstack

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/indexer"
)

func TestLifecycleSharedServerCloseRejectsExtractionAndMutations(t *testing.T) {
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, "toy.go"), []byte("package toy\n\nfunc Toy() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	ss, err := NewSharedServer(SharedServerConfig{
		Lifecycle:         LifecycleOneshot,
		Index:             repo,
		BackendPath:       filepath.Join(t.TempDir(), "embedded.sqlite"),
		Config:            config.Default(),
		Logger:            zap.NewNop(),
		Version:           "test",
		SideStores:        SideStores{NotesDir: t.TempDir(), NotesRepo: "test"},
		SavingsPath:       filepath.Join(t.TempDir(), "sidecar.sqlite"),
		SavingsLegacyJSON: filepath.Join(t.TempDir(), "savings.json"),
	})
	if err != nil {
		t.Fatalf("NewSharedServer: %v", err)
	}
	closed := false
	t.Cleanup(func() {
		if !closed {
			_ = ss.Close()
		}
	})
	if ss.MultiIndexer == nil {
		t.Fatal("shared server did not construct a MultiIndexer")
	}

	err = ss.Close()
	closed = true
	if err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := ss.Indexer.ExtractBuffer("go", "after.go", []byte("package after\n")); !errors.Is(err, indexer.ErrIndexerClosed) {
		t.Fatalf("post-close standalone extraction error = %v, want ErrIndexerClosed", err)
	}

	afterRepo := t.TempDir()
	if err := os.WriteFile(filepath.Join(afterRepo, "after.go"), []byte("package after\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ss.MultiIndexer.TrackRepo(config.RepoEntry{Path: afterRepo, Name: "after"}); err == nil || !strings.Contains(strings.ToLower(err.Error()), "closed") {
		t.Fatalf("post-close mutation error = %v, want closed lifecycle", err)
	}
}

func TestSharedServerCloseJoinsCheckoutLifecycle(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git binary not available")
	}
	repo := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	runSharedServerGit(t, repo, "init", "-q", "-b", "main")
	runSharedServerGit(t, repo, "config", "user.email", "test@example.com")
	runSharedServerGit(t, repo, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(repo, "toy.go"), []byte("package toy\n\nfunc Toy() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runSharedServerGit(t, repo, "add", ".")
	runSharedServerGit(t, repo, "commit", "-q", "-m", "init")

	global := &config.GlobalConfig{}
	global.SetConfigPath(filepath.Join(t.TempDir(), "config.yaml"))
	if err := global.Save(); err != nil {
		t.Fatalf("save global config: %v", err)
	}
	ss, err := NewSharedServer(SharedServerConfig{
		Lifecycle:         LifecycleOneshot,
		Index:             repo,
		BackendPath:       filepath.Join(t.TempDir(), "embedded.sqlite"),
		Config:            config.Default(),
		Global:            global,
		Logger:            zap.NewNop(),
		Version:           "test",
		SideStores:        SideStores{NotesDir: t.TempDir(), NotesRepo: "test"},
		SavingsPath:       filepath.Join(t.TempDir(), "sidecar.sqlite"),
		SavingsLegacyJSON: filepath.Join(t.TempDir(), "savings.json"),
	})
	if err != nil {
		t.Fatalf("NewSharedServer: %v", err)
	}
	closed := false
	t.Cleanup(func() {
		if !closed {
			_ = ss.Close()
		}
	})
	if ss.CheckoutLifecycle == nil {
		t.Fatal("shared server did not construct checkout lifecycle")
	}
	ss.CheckoutLifecycle.SetWatcherSource(func() indexer.RepoWatcher {
		return sharedServerNoopWatcher{}
	})
	if _, err := ss.CheckoutLifecycle.Register(
		context.Background(), config.RepoEntry{Path: repo, Name: "repo"}, indexer.TrackSourceCLI,
	); err != nil {
		t.Fatalf("register dedicated checkout: %v", err)
	}
	if !eventuallySharedServer(time.Second, func() bool {
		return ss.CheckoutLifecycle.LiveCoordinators("") > 0
	}) {
		t.Fatal("dedicated checkout did not start a lifecycle coordinator")
	}

	started := time.Now()
	if err := ss.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	closed = true
	t.Logf("joined shared-server teardown in %s", time.Since(started))
	if live := ss.CheckoutLifecycle.LiveCoordinators(""); live != 0 {
		t.Fatalf("live coordinators after shared-server close = %d, want 0", live)
	}
}

type sharedServerNoopWatcher struct{}

func (sharedServerNoopWatcher) AddRepo(string, config.WatchConfig) error { return nil }
func (sharedServerNoopWatcher) RemoveRepo(string) error                  { return nil }

func runSharedServerGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

func eventuallySharedServer(timeout time.Duration, condition func() bool) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return true
		}
		time.Sleep(time.Millisecond)
	}
	return condition()
}
