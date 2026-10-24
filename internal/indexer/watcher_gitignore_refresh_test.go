package indexer

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/zzet/gortex/internal/thirdparty/fswatcher"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/search"
)

// No native event delivery or timers are required: these are the production
// backend/admission predicates and the real synchronous point producer.
func newGitignoreRefreshWatcher(t *testing.T, tracked, initialIgnoreDir, initialPatterns string) (*MultiIndexer, *Watcher, <-chan error) {
	t.Helper()
	writeFile(t, filepath.Join(tracked, "keep.go"), "package app\n\nfunc Kept() {}\n")
	if initialIgnoreDir != "" {
		require.NoError(t, os.WriteFile(filepath.Join(initialIgnoreDir, ".gitignore"), []byte(initialPatterns), 0o644))
	}
	cm := newTestConfigManager(t)
	cm.Global().Repos = []config.RepoEntry{{Path: tracked, Name: "app"}}
	mi := NewMultiIndexer(graph.New(), newTestRegistry(), search.NewNull(), cm, zap.NewNop())
	t.Cleanup(func() { require.NoError(t, mi.Close(context.Background())) })
	_, err := mi.IndexAll()
	require.NoError(t, err)
	mw, err := NewConfigManagedMultiWatcher(mi, map[string]config.WatchConfig{"app": cm.GetRepoConfig("app").Watch}, zap.NewNop())
	require.NoError(t, err)
	w := mw.watchers["app"]
	require.NotNil(t, w)
	w.degradedNoFsnotify = true // Stop joins work without an unstarted native loop.
	t.Cleanup(func() { require.NoError(t, w.Stop()) })
	require.NotEmpty(t, mi.graph.FindNodesByName("Kept"))
	reconciled := make(chan error, 8)
	w.reconcileFn = func() {
		_, err := mi.IncrementalReindexRepo("app", nil)
		reconciled <- err
	}
	return mi, w, reconciled
}

func TestMultiWatcherRefreshesGitignoreBeforeNewSourceAdmission(t *testing.T) {
	for _, scope := range []string{"tracked_root", "ancestor", "new_intermediate"} {
		t.Run(scope, func(t *testing.T) {
			root := t.TempDir()
			// The ignore-chain authority uses the actual .git boundary on disk;
			// like the existing config chain tests, no Git process is needed.
			require.NoError(t, os.Mkdir(filepath.Join(root, ".git"), 0o755))
			tracked := root
			ignoreDir := root
			initialIgnoreDir := root
			if scope != "tracked_root" {
				tracked = filepath.Join(root, "projects", "app")
				require.NoError(t, os.MkdirAll(tracked, 0o755))
			}
			if scope == "new_intermediate" {
				ignoreDir = filepath.Dir(tracked)
				initialIgnoreDir = "" // This chain member appears after tracking.
			}
			mi, w, reconciled := newGitignoreRefreshWatcher(t, tracked, initialIgnoreDir, "# initially allowed\n")
			path := filepath.Join(tracked, "ignored.go")
			filter := &watcherExcludeFilter{w: w}
			require.True(t, filter.ShouldInclude(path), "precondition: initial policy admits this source path")

			// Commit-style ordering: ignore policy lands before the new untracked
			// file. An ancestor edit need not emit any event inside tracked.
			require.NoError(t, os.WriteFile(filepath.Join(ignoreDir, ".gitignore"), []byte("ignored.go\n"), 0o644))
			writeFile(t, path, "package app\n\nfunc MustStayIgnored() {}\n")
			require.False(t, filter.ShouldInclude(path), "raw backend admission must observe the new policy")
			require.True(t, w.isExcluded(path), "semantic admission must agree with the backend")
			ticket, err := w.EnqueueFileMutation(context.Background(), path)
			require.NoError(t, err)
			require.Nil(t, ticket, "ignored source must not acquire a mutation generation")
			awaitGitignoreReconcile(t, reconciled)
			require.Empty(t, mi.graph.FindNodesByName("MustStayIgnored"))
			require.NotEmpty(t, mi.graph.FindNodesByName("Kept"))
		})
	}
}

// Ignore changes affect existing graph membership even if no source file is
// subsequently touched. The real coalesced reconcile must both withdraw and
// rediscover under the new policy; a predicate-only fix cannot pass this.
func TestMultiWatcherRefreshesGitignoreMembershipWithoutSourceEdits(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(root, ".git"), 0o755))
	tracked := filepath.Join(root, "projects", "app")
	require.NoError(t, os.MkdirAll(tracked, 0o755))
	dropped := filepath.Join(tracked, "drop.go")
	restored := filepath.Join(tracked, "restored.go")
	writeFile(t, dropped, "package app\n\nfunc Dropped() {}\n")
	writeFile(t, restored, "package app\n\nfunc Restored() {}\n")
	mi, w, reconciled := newGitignoreRefreshWatcher(t, tracked, root, "restored.go\n")
	filter := &watcherExcludeFilter{w: w}
	require.True(t, filter.ShouldInclude(dropped))
	require.False(t, filter.ShouldInclude(restored), "precondition: ancestor policy excludes the file")
	require.NotEmpty(t, mi.graph.FindNodesByName("Dropped"))
	require.Empty(t, mi.graph.FindNodesByName("Restored"))

	// Only the ancestor's policy changes. Neither source receives a watcher
	// event; any ordinary admission check inside tracked must notice the new
	// chain snapshot and coalesce one real policy reconcile.
	require.NoError(t, os.WriteFile(filepath.Join(root, ".gitignore"), []byte("drop.go\n"), 0o644))
	require.False(t, filter.ShouldInclude(dropped), "new policy must reach backend admission")
	require.True(t, filter.ShouldInclude(restored), "the stale backend must not hide re-included source")
	require.True(t, w.isExcluded(dropped))
	require.False(t, w.isExcluded(restored))
	awaitGitignoreReconcile(t, reconciled)
	require.Empty(t, mi.graph.FindNodesByName("Dropped"), "newly excluded existing payload must leave")
	require.NotEmpty(t, mi.graph.FindNodesByName("Restored"), "unignore must rediscover untouched disk source")
	require.NotEmpty(t, mi.graph.FindNodesByName("Kept"))

	// The real point producer must use the refreshed policy as well. This
	// also covers a path subsequently saved after it is re-included.
	writeFile(t, restored, "package app\n\nfunc RestoredNow() {}\n")
	require.NoError(t, w.patchGraphAfterReceiptCheck(restored, ChangeModified))
	require.NotEmpty(t, mi.graph.FindNodesByName("RestoredNow"))
	require.Empty(t, mi.graph.FindNodesByName("Restored"))
}

func awaitGitignoreReconcile(t *testing.T, reconciled <-chan error) {
	t.Helper()
	select {
	case err := <-reconciled:
		require.NoError(t, err)
	case <-time.After(30 * time.Second):
		t.Fatal("policy-change reconcile did not finish")
	}
}

// Callers of the original constructor retain their explicit static policy,
// even when the MultiIndexer happens to have a ConfigManager available.
func TestMultiWatcherPreservesCallerSuppliedExcludePolicy(t *testing.T) {
	root := t.TempDir()
	cm := newTestConfigManager(t)
	cm.LoadWorkspaceConfig("app", root)
	mi := NewMultiIndexer(graph.New(), newTestRegistry(), search.NewNull(), cm, zap.NewNop())
	t.Cleanup(func() { require.NoError(t, mi.Close(context.Background())) })
	idx := newTestIndexer(mi.graph)
	idx.SetRootPath(root)
	idx.SetRepoPrefix("app")
	mi.repos["app"] = &RepoMetadata{RepoPrefix: "app", RootPath: root}
	mi.indexers["app"] = idx
	mw, err := NewMultiWatcher(mi, map[string]config.WatchConfig{"app": {Exclude: []string{"custom.go"}}}, zap.NewNop())
	require.NoError(t, err)
	w := mw.watchers["app"]
	w.degradedNoFsnotify = true
	t.Cleanup(func() { require.NoError(t, w.Stop()) })
	filter := &watcherExcludeFilter{w: w}
	custom := filepath.Join(root, "custom.go")
	ordinary := filepath.Join(root, "ordinary.go")
	require.False(t, filter.ShouldInclude(custom))
	require.True(t, filter.ShouldInclude(ordinary))
	require.NoError(t, os.WriteFile(filepath.Join(root, ".gitignore"), []byte("ordinary.go\n"), 0o644))
	require.False(t, filter.ShouldInclude(custom), "explicit custom exclusion must not be discarded")
	require.True(t, filter.ShouldInclude(ordinary), "this caller did not opt into ConfigManager's live policy")
	require.True(t, w.isExcluded(custom))
	require.False(t, w.isExcluded(ordinary))
}

// Refresh must preserve the include override while applying the parent rule,
// and an unchanged ConfigManager snapshot must retain the compiled matcher.
func TestMultiWatcherRefreshPreservesNegationIncludesAndWarnings(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(root, ".git"), 0o755))
	writeFile(t, filepath.Join(root, ".gortex.yaml"), "include:\n  - vendor/keep.go\n")
	writeExcludeFixture(t, filepath.Join(root, "vendor/keep.go"), "package app\nfunc Included() {}\n")
	writeExcludeFixture(t, filepath.Join(root, "dir/keep.go"), "package app\nfunc Negated() {}\n")
	mi, w, reconciled := newGitignoreRefreshWatcher(t, root, root, "# initially open\n")
	idx := mi.GetIndexer("app")
	core, logs := observer.New(zap.WarnLevel)
	idx.logger = zap.New(core)
	filter := &watcherExcludeFilter{w: w}
	included := filepath.Join(root, "vendor/keep.go")
	negated := filepath.Join(root, "dir/keep.go")
	require.True(t, filter.ShouldInclude(included))
	require.NotEmpty(t, mi.graph.FindNodesByName("Included"))
	require.NotEmpty(t, mi.graph.FindNodesByName("Negated"))

	for _, rules := range []string{"dir/\n!/dir/keep.go\n", "dir/\n!/dir/keep.go\n# second refresh\n"} {
		require.NoError(t, os.WriteFile(filepath.Join(root, ".gitignore"), []byte(rules), 0o644))
		t.Run(fmt.Sprintf("refresh_%d", len(rules)), func(t *testing.T) {
			require.False(t, filter.ShouldInclude(negated), "negation cannot reopen an excluded parent")
			require.True(t, filter.ShouldInclude(included), "refresh must rebuild includes too")
			require.False(t, w.isExcluded(included))
			awaitGitignoreReconcile(t, reconciled)
			require.NotEmpty(t, mi.graph.FindNodesByName("Included"))
			require.Empty(t, mi.graph.FindNodesByName("Negated"))
			require.True(t, idx.excludeMatcher().IncludesRel("vendor/keep.go"))
		})
	}
	require.Len(t, logs.FilterMessage("indexer: re-include is blocked by an excluded parent; reopen each parent folder (for example: !/agent/, /agent/*, !/agent/skills/)").All(), 1,
		"warning must be keyed by repo and pattern across refreshes")
	matcher := w.currentExcludeMatcher()
	indexMatcher := idx.excludeMatcher()
	require.Same(t, matcher, w.currentExcludeMatcher(), "no-change snapshot must reuse the matcher")
	_, err := mi.IncrementalReindexRepo("app", nil)
	require.NoError(t, err)
	require.Same(t, indexMatcher, idx.excludeMatcher())
	select {
	case err := <-reconciled:
		t.Fatalf("unchanged policy scheduled another reconcile: %v", err)
	default:
	}

	// Includes override only the flat list, never a per-folder ignore file.
	writeFile(t, filepath.Join(root, "vendor/.gortexignore"), "keep.go\n")
	w.forgetDirIgnores(filepath.Join(root, "vendor/.gortexignore"), []fswatcher.EventType{fswatcher.EventMod})
	require.True(t, filter.ShouldInclude(included), "backend defers per-folder rules")
	require.True(t, w.isExcluded(included))
}
