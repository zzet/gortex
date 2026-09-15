package indexer

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/config"
)

// This fixture is intentionally sufficient only for preparation and the public
// already-tracked/error fast paths. It does not claim to be a usable Indexer
// build fixture, and must not be used to exercise the coordinated build tail.
func newTrackPreparationSplitFixture(t *testing.T) (*MultiIndexer, config.RepoEntry) {
	t.Helper()
	// Internal identity/preparation helpers also launch Git, so sanitize the
	// process environment, not only this fixture's explicit git commands.
	// Setenv supplies restoration and rejects parallel tests/ancestors.
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if !strings.HasPrefix(strings.ToUpper(key), "GIT_") {
			continue
		}
		t.Setenv(key, "")
		if err := os.Unsetenv(key); err != nil {
			t.Fatalf("clear inherited Git environment %q: %v", key, err)
		}
	}
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_TERMINAL_PROMPT", "0")
	t.Setenv("GIT_NO_LAZY_FETCH", "1")
	root := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Dir = root
		cmd.Env = append(os.Environ(),
			"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull,
			"GIT_TERMINAL_PROMPT=0", "GIT_NO_LAZY_FETCH=1")
		cmd.Env = append(cmd.Env, privateGitIdentityEnv...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("private git %v: %v: %s", args, err, out)
		}
	}
	git("init", "-q")
	git("-c", "user.name=Private Test", "-c", "user.email=private@example.invalid",
		"-c", "commit.gpgsign=false", "commit", "--allow-empty", "-qm", "fixture")
	// Independent healthy identity control: an invalid Git fixture is setup
	// failure, never evidence that the new preparation contract rejects it.
	if identity, err := DetectIdentity(root); err != nil || identity == nil {
		t.Fatalf("healthy private identity: identity=%+v err=%v", identity, err)
	}
	mi := &MultiIndexer{repos: map[string]*RepoMetadata{}}
	return mi, config.RepoEntry{Path: root, Name: "track-preparation-split"}
}

func TestTrackRepoPreparationSplitInputs(t *testing.T) {
	mi, entry := newTrackPreparationSplitFixture(t)
	original := entry
	hookCalls := 0
	var hookPrefix, hookRoot string
	mi.onRepoTracked = func(prefix, root string) {
		if !mi.mu.TryLock() {
			t.Error("pre-index hook ran while the registry lock was held")
		} else {
			mi.mu.Unlock()
		}
		hookCalls++
		hookPrefix, hookRoot = prefix, root
		if len(mi.repos) != 0 || len(mi.indexers) != 0 {
			t.Error("pre-index hook observed an installed repository/indexer")
		}
	}
	prepared, err := mi.prepareTrackRepo(entry)
	if err != nil || prepared == nil {
		t.Fatalf("healthy preparation: prepared=%+v err=%v", prepared, err)
	}
	if hookCalls != 1 || hookPrefix != prepared.prefix || hookRoot != prepared.absPath {
		t.Fatalf("hook: calls=%d prefix=%q root=%q, prepared=%+v", hookCalls, hookPrefix, hookRoot, prepared)
	}
	if prepared.prefix != entry.Name || !filepath.IsAbs(prepared.absPath) {
		t.Fatalf("prepared namespace/root: %+v", prepared)
	}
	if prepared.identity == nil || prepared.cfg == nil {
		t.Fatalf("missing prepared identity/config: %+v", prepared)
	}
	if prepared.entry.Path != entry.Path || prepared.entry.Name != entry.Name {
		t.Fatalf("explicit entry changed before installation: %+v", prepared.entry)
	}
	if entry.Path != original.Path || entry.Name != original.Name {
		t.Fatalf("input value mutated: before=%+v after=%+v", original, entry)
	}
	if len(mi.repos) != 0 || len(mi.indexers) != 0 {
		t.Fatal("preparation exposed a repository/indexer before a successful build")
	}
}

func TestTrackRepoPreparationSplitConcurrentPlansDoNotReserve(t *testing.T) {
	mi, entry := newTrackPreparationSplitFixture(t)
	var hooks atomic.Int64
	mi.onRepoTracked = func(string, string) { hooks.Add(1) }
	type outcome struct {
		prepared *preparedTrackRepo
		err      error
	}
	start := make(chan struct{})
	results := make(chan outcome, 2)
	for range 2 {
		go func() {
			<-start
			prepared, err := mi.prepareTrackRepo(entry)
			results <- outcome{prepared, err}
		}()
	}
	close(start)
	// Consume and join both preparations before any assertion can end the test.
	first, second := <-results, <-results
	for i, result := range []outcome{first, second} {
		if result.err != nil || result.prepared == nil {
			t.Fatalf("preparation %d: prepared=%+v err=%v", i, result.prepared, result.err)
		}
		if result.prepared.prefix != entry.Name {
			t.Fatalf("preparation %d prefix=%q", i, result.prepared.prefix)
		}
	}
	if hooks.Load() != 2 {
		t.Fatalf("uninstalled contenders must retain both early hooks; got %d", hooks.Load())
	}
	if len(mi.repos) != 0 || len(mi.indexers) != 0 {
		t.Fatal("preparation reserved or installed registry state")
	}
}

func TestTrackRepoPreparationSplitPublicNoop(t *testing.T) {
	for _, samePrefix := range []bool{true, false} {
		name := "same_physical_root_different_prefix"
		if samePrefix {
			name = "existing_prefix"
		}
		t.Run(name, func(t *testing.T) {
			mi, entry := newTrackPreparationSplitFixture(t)
			prepared, err := mi.prepareTrackRepo(entry)
			if err != nil || prepared == nil {
				t.Fatalf("healthy preparation control: prepared=%+v err=%v", prepared, err)
			}
			key := "another-existing-prefix"
			if samePrefix {
				key = prepared.prefix
			}
			mi.repos[key] = &RepoMetadata{RepoPrefix: key, RootPath: prepared.absPath}
			hookCalls := 0
			mi.onRepoTracked = func(string, string) { hookCalls++ }
			result, err := mi.TrackRepoCtx(context.Background(), entry)
			if err != nil || result != nil {
				t.Fatalf("public already-tracked result=%+v err=%v", result, err)
			}
			if hookCalls != 0 || len(mi.repos) != 1 || len(mi.indexers) != 0 {
				t.Fatalf("no-op effects: hooks=%d repos=%d indexers=%d", hookCalls, len(mi.repos), len(mi.indexers))
			}
		})
	}
}

func TestTrackRepoPreparationSplitPublicValidation(t *testing.T) {
	mi, healthy := newTrackPreparationSplitFixture(t)
	if prepared, err := mi.prepareTrackRepo(healthy); err != nil || prepared == nil {
		t.Fatalf("healthy preparation control: prepared=%+v err=%v", prepared, err)
	}
	file := filepath.Join(healthy.Path, "ordinary-file")
	if err := os.WriteFile(file, []byte("private fixture\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	hookCalls := 0
	mi.onRepoTracked = func(string, string) { hookCalls++ }
	for _, tc := range []struct {
		name, path, errorPrefix string
	}{
		{"missing", filepath.Join(healthy.Path, "missing-directory"), "path does not exist: "},
		{"not_directory", file, "path is not a directory: "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			entry := healthy
			entry.Path = tc.path
			result, err := mi.TrackRepoCtx(context.Background(), entry)
			if result != nil || err == nil || !strings.HasPrefix(err.Error(), tc.errorPrefix) {
				t.Fatalf("public validation result=%+v err=%v; want %q", result, err, tc.errorPrefix)
			}
		})
	}
	if hookCalls != 0 || len(mi.repos) != 0 || len(mi.indexers) != 0 {
		t.Fatalf("failed validation effects: hooks=%d repos=%d indexers=%d", hookCalls, len(mi.repos), len(mi.indexers))
	}
}
