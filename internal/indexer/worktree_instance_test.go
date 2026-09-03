package indexer

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/search"
)

// gitInitRepo initialises a fresh git repo at dir with deterministic
// commit settings, independent of the developer's global git config.
func gitInitRepo(t *testing.T, dir string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(dir, 0o755))
	runGit(t, dir, "init", "-q", "-b", "main")
	runGit(t, dir, "config", "user.email", "test@example.com")
	runGit(t, dir, "config", "user.name", "Test")
	runGit(t, dir, "config", "commit.gpgsign", "false")
}

// setupCollidingWorktree builds a canonical checkout and a linked git
// worktree that SHARE A BASENAME (the issue's `oas-orm` collision) so
// both would otherwise resolve to the same repo prefix. The worktree is
// checked out on `branch`; when wtWorkspace is non-empty it gets a
// `.gortex.yaml` declaring that workspace. Returns (canonPath, wtPath).
func setupCollidingWorktree(t *testing.T, basename, branch, wtWorkspace string) (string, string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git binary not available in PATH")
	}
	canon := filepath.Join(t.TempDir(), basename)
	gitInitRepo(t, canon)
	writeFile(t, filepath.Join(canon, "lib.go"), "package lib\n\nfunc Canonical() {}\n")
	runGit(t, canon, "add", ".")
	runGit(t, canon, "commit", "-q", "-m", "init")

	wt := filepath.Join(t.TempDir(), basename)
	runGit(t, canon, "worktree", "add", "-q", "-b", branch, wt)
	writeFile(t, filepath.Join(wt, "feature.go"), "package lib\n\nfunc Feature() {}\n")
	if wtWorkspace != "" {
		writeFile(t, filepath.Join(wt, ".gortex.yaml"), "workspace: "+wtWorkspace+"\n")
	}
	return canon, wt
}

func newWorktreeTestIndexer(t *testing.T, repos ...config.RepoEntry) (*graph.Graph, *MultiIndexer, *config.ConfigManager) {
	t.Helper()
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	gc := &config.GlobalConfig{Repos: repos}
	gc.SetConfigPath(cfgPath)
	require.NoError(t, gc.Save())
	cm, err := config.NewConfigManager(cfgPath)
	require.NoError(t, err)
	g := graph.New()
	mi := NewMultiIndexer(g, newTestRegistry(), search.NewNull(), cm, zap.NewNop())
	return g, mi, cm
}

// TestWorktreeInstance_IndexAll_AutoSeparatesByWorkspace is the core
// regression for issue #47: a worktree of an already-tracked repo, whose
// `.gortex.yaml` declares a different workspace, must be indexed as an
// INDEPENDENT instance instead of silently coalescing into the canonical.
func TestWorktreeInstance_IndexAll_AutoSeparatesByWorkspace(t *testing.T) {
	canon, wt := setupCollidingWorktree(t, "oas-orm", "task", "task-ws")

	g, mi, _ := newWorktreeTestIndexer(t,
		config.RepoEntry{Path: canon}, // canonical, base prefix oas-orm
		config.RepoEntry{Path: wt},    // worktree declaring workspace task-ws
	)
	results, err := mi.IndexAll()
	require.NoError(t, err)

	// Two independent instances, not one coalesced entry.
	require.Len(t, results, 2, "the worktree must not coalesce into the canonical")
	canonMeta := mi.GetMetadata("oas-orm")
	wtMeta := mi.GetMetadata("oas-orm@task-ws")
	require.NotNil(t, canonMeta, "canonical keeps the base prefix")
	require.NotNil(t, wtMeta, "worktree is tracked under a derived prefix")
	assert.False(t, canonMeta.IsWorktree)
	assert.True(t, wtMeta.IsWorktree)
	assert.Equal(t, realpath(t, canon), realpath(t, canonMeta.RootPath))
	assert.Equal(t, realpath(t, wt), realpath(t, wtMeta.RootPath))

	// Each instance's files are namespaced under its own prefix and do
	// not bleed into the other.
	assert.NotEmpty(t, g.GetFileNodes("oas-orm/lib.go"))
	assert.NotEmpty(t, g.GetFileNodes("oas-orm@task-ws/feature.go"))
	assert.Empty(t, g.GetFileNodes("oas-orm/feature.go"),
		"the worktree's branch-only file must not appear under the canonical")

	// Per-instance workspace membership.
	inTask := mi.ReposInWorkspace("task-ws")
	assert.True(t, inTask["oas-orm@task-ws"], "worktree joins task-ws")
	assert.False(t, inTask["oas-orm"], "canonical does not join task-ws")

	// A session launched from inside the worktree resolves to its
	// workspace, not the canonical's.
	ws, _, prefix, ok := mi.ScopeForCWD(wt)
	require.True(t, ok)
	assert.Equal(t, "task-ws", ws)
	assert.Equal(t, "oas-orm@task-ws", prefix)

	// ResolveFilePath disambiguates the two overlapping prefixes
	// (`oas-orm` vs `oas-orm@task-ws`) by longest match, mapping each
	// prefixed graph path back to its own checkout.
	assert.Equal(t, realpath(t, filepath.Join(wt, "feature.go")),
		realpath(t, mi.ResolveFilePath("oas-orm@task-ws/feature.go")))
	assert.Equal(t, realpath(t, filepath.Join(canon, "lib.go")),
		realpath(t, mi.ResolveFilePath("oas-orm/lib.go")))
}

// TestWorktreeInstance_PlainWorktreeStillCoalesces guards the opposite
// case: a worktree that declares NO distinct workspace (and is not
// flagged) must not derive a workspace instance. It keeps the legacy
// disposition instead — a tracked path whose basename collides with the
// canonical's, so it lands under a path-hash-suffixed prefix rather than
// an `@workspace` instance sibling.
func TestWorktreeInstance_PlainWorktreeStillCoalesces(t *testing.T) {
	canon, wt := setupCollidingWorktree(t, "oas-orm", "task", "") // no .gortex.yaml

	g, mi, _ := newWorktreeTestIndexer(t,
		config.RepoEntry{Path: canon},
		config.RepoEntry{Path: wt},
	)
	results, err := mi.IndexAll()
	require.NoError(t, err)

	// Same basename, same (default) workspace → the canonical keeps the
	// bare "oas-orm" prefix and no "oas-orm@..." sibling is derived.
	canonMeta := mi.GetMetadata("oas-orm")
	require.NotNil(t, canonMeta)
	for prefix := range mi.AllMetadata() {
		assert.NotContains(t, prefix, "@",
			"a plain worktree must not spawn a separate instance, got %q", prefix)
	}

	// Name shape alone is a weak guard — it passes just as happily when
	// the worktree is folded into the canonical's prefix as when it is
	// tracked beside it. Pin the disposition with counts: the canonical
	// indexes only its own checkout, and the worktree is a second
	// tracked path under a hash-suffixed (not instance-tagged) prefix.
	require.Len(t, mi.AllMetadata(), 2)
	require.Len(t, results, 2, "each tracked path runs its own index pass")
	assert.False(t, canonMeta.IsWorktree)
	assert.Equal(t, realpath(t, canon), realpath(t, canonMeta.RootPath))
	assert.Equal(t, 1, canonMeta.FileCount, "the canonical sees only its own file")

	var wtPrefix string
	for prefix, meta := range mi.AllMetadata() {
		if prefix == "oas-orm" {
			continue
		}
		wtPrefix = prefix
		assert.True(t, meta.IsWorktree, "the second instance is the worktree")
		assert.True(t, strings.HasPrefix(prefix, "oas-orm-"),
			"a basename collision resolves by path hash, got %q", prefix)
		assert.Equal(t, realpath(t, wt), realpath(t, meta.RootPath))
		assert.Equal(t, 2, meta.FileCount, "the worktree sees the branch file too")
	}
	require.NotEmpty(t, wtPrefix)

	// Every metadata rollup reports the counts of its own pass.
	for prefix, meta := range mi.AllMetadata() {
		result := results[prefix]
		require.NotNil(t, result, "no index result for %q", prefix)
		assert.Equal(t, meta.NodeCount, result.NodeCount, "node count for %q", prefix)
		assert.Equal(t, meta.EdgeCount, result.EdgeCount, "edge count for %q", prefix)
		assert.Equal(t, meta.FileCount, result.FileCount, "file count for %q", prefix)
		assert.NotZero(t, meta.NodeCount, "instance %q indexed nothing", prefix)
	}

	// The branch-only file never bleeds into the canonical's prefix.
	assert.NotEmpty(t, g.GetFileNodes("oas-orm/lib.go"))
	assert.Empty(t, g.GetFileNodes("oas-orm/feature.go"))
	assert.NotEmpty(t, g.GetFileNodes(wtPrefix+"/feature.go"))
}

// TestWorktreeInstance_TrackCtx_NoCoalesce_PersistsName exercises the
// interactive runtime path: track the canonical, then track the worktree
// as a brand-new path. The derived prefix must be returned and persisted
// as the entry Name so a daemon restart reproduces it.
func TestWorktreeInstance_TrackCtx_NoCoalesce_PersistsName(t *testing.T) {
	canon, wt := setupCollidingWorktree(t, "oas-orm", "task", "task-ws")

	// Pre-list canon plus a placeholder so multi-repo prefixing is on
	// from the first track (single-repo mode would skip prefixes).
	placeholder := filepath.Join(t.TempDir(), "placeholder")
	require.NoError(t, os.MkdirAll(placeholder, 0o755))
	_, mi, cm := newWorktreeTestIndexer(t,
		config.RepoEntry{Path: canon},
		config.RepoEntry{Path: placeholder},
	)

	rCanon, err := mi.TrackRepoCtx(testCtx(), config.RepoEntry{Path: canon})
	require.NoError(t, err)
	require.NotNil(t, rCanon)
	assert.Equal(t, "oas-orm", rCanon.RepoPrefix)

	rWt, err := mi.TrackRepoCtx(testCtx(), config.RepoEntry{Path: wt})
	require.NoError(t, err)
	require.NotNil(t, rWt, "worktree must not coalesce into the canonical")
	assert.Equal(t, "oas-orm@task-ws", rWt.RepoPrefix)

	// The derived prefix is persisted as the entry Name (wt was a fresh
	// path, so AddRepo recorded it).
	entry := cm.Global().FindRepoByPrefix("oas-orm@task-ws")
	require.NotNil(t, entry, "the worktree instance must be persisted to config")
	assert.Equal(t, "oas-orm@task-ws", entry.Name)
	assert.Equal(t, realpath(t, wt), realpath(t, mustAbs(t, entry.Path)))

	// Re-tracking the same worktree path is an idempotent no-op.
	again, err := mi.TrackRepoCtx(testCtx(), config.RepoEntry{Path: wt})
	require.NoError(t, err)
	assert.Nil(t, again, "re-tracking the worktree must be already-tracked")
}

// TestWorktreeInstance_DeterministicAcrossRestart verifies the derived
// prefix is stable: a fresh MultiIndexer over the same config reproduces
// the same instance prefixes (the auto rule is intrinsic, so it does not
// depend on a persisted Name or on tracking order).
func TestWorktreeInstance_DeterministicAcrossRestart(t *testing.T) {
	canon, wt := setupCollidingWorktree(t, "oas-orm", "task", "task-ws")

	for _, pass := range []string{"first", "second"} {
		_, mi, _ := newWorktreeTestIndexer(t,
			config.RepoEntry{Path: canon},
			config.RepoEntry{Path: wt},
		)
		_, err := mi.IndexAll()
		require.NoError(t, err, pass)
		require.NotNil(t, mi.GetMetadata("oas-orm"), pass)
		require.NotNil(t, mi.GetMetadata("oas-orm@task-ws"), pass)
	}
}

// TestWorktreeInstance_TwoWorktreesSameWorkspaceDisambiguated covers the
// degenerate collision: two different worktrees of the same repo that
// both declare the same workspace must still get distinct prefixes.
func TestWorktreeInstance_TwoWorktreesSameWorkspaceDisambiguated(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git binary not available in PATH")
	}
	canon := filepath.Join(t.TempDir(), "oas-orm")
	gitInitRepo(t, canon)
	writeFile(t, filepath.Join(canon, "lib.go"), "package lib\n\nfunc Canonical() {}\n")
	runGit(t, canon, "add", ".")
	runGit(t, canon, "commit", "-q", "-m", "init")

	wtA := filepath.Join(t.TempDir(), "oas-orm")
	runGit(t, canon, "worktree", "add", "-q", "-b", "a", wtA)
	writeFile(t, filepath.Join(wtA, ".gortex.yaml"), "workspace: shared\n")
	writeFile(t, filepath.Join(wtA, "a.go"), "package lib\n\nfunc A() {}\n")

	wtB := filepath.Join(t.TempDir(), "oas-orm")
	runGit(t, canon, "worktree", "add", "-q", "-b", "b", wtB)
	writeFile(t, filepath.Join(wtB, ".gortex.yaml"), "workspace: shared\n")
	writeFile(t, filepath.Join(wtB, "b.go"), "package lib\n\nfunc B() {}\n")

	g, mi, _ := newWorktreeTestIndexer(t,
		config.RepoEntry{Path: canon},
		config.RepoEntry{Path: wtA},
		config.RepoEntry{Path: wtB},
	)
	results, err := mi.IndexAll()
	require.NoError(t, err)

	// Both worktrees want "oas-orm@shared"; one keeps it, the other gets
	// a path-hash suffix. Exactly three distinct instances total.
	assert.Len(t, mi.AllMetadata(), 3)
	shared := 0
	for prefix := range mi.AllMetadata() {
		if prefix == "oas-orm@shared" || (len(prefix) > len("oas-orm@shared-") && prefix[:len("oas-orm@shared-")] == "oas-orm@shared-") {
			shared++
		}
	}
	assert.Equal(t, 2, shared, "two distinct worktree instances under the shared workspace")

	// Each instance did its own indexing pass, and its metadata carries
	// that pass's counts — not just a distinct name.
	require.Len(t, results, 3, "every instance produces its own index result")
	for prefix, meta := range mi.AllMetadata() {
		result := results[prefix]
		require.NotNil(t, result, "no index result for instance %q", prefix)
		assert.Equal(t, meta.NodeCount, result.NodeCount, "node count for %q", prefix)
		assert.Equal(t, meta.EdgeCount, result.EdgeCount, "edge count for %q", prefix)
		assert.Equal(t, meta.FileCount, result.FileCount, "file count for %q", prefix)
		assert.NotZero(t, meta.NodeCount, "instance %q indexed nothing", prefix)
	}

	// The branch-only files land under their own instance's prefix, so
	// the three instances hold disjoint content.
	assert.NotEmpty(t, g.GetFileNodes("oas-orm/lib.go"))
	assert.Empty(t, g.GetFileNodes("oas-orm/a.go"))
	assert.Empty(t, g.GetFileNodes("oas-orm/b.go"))
}

// TestEffectiveRepoPrefix_MatchesRegisteredPrefix guarantees the warm-
// restart keying helper agrees with the prefix the indexer actually
// registers — otherwise a disk-backed reconcile would key snapshot
// mtimes by the wrong prefix and re-index the worktree every restart.
func TestEffectiveRepoPrefix_MatchesRegisteredPrefix(t *testing.T) {
	canon, wt := setupCollidingWorktree(t, "oas-orm", "task", "task-ws")
	_, mi, cm := newWorktreeTestIndexer(t,
		config.RepoEntry{Path: canon},
		config.RepoEntry{Path: wt},
	)
	_, err := mi.IndexAll()
	require.NoError(t, err)

	// The helper reproduces the registered prefix for both checkouts.
	assert.Equal(t, "oas-orm", EffectiveRepoPrefix(cm, config.RepoEntry{Path: canon}))
	assert.Equal(t, "oas-orm@task-ws", EffectiveRepoPrefix(cm, config.RepoEntry{Path: wt}))
	require.NotNil(t, mi.GetMetadata(EffectiveRepoPrefix(cm, config.RepoEntry{Path: wt})))
	require.NotNil(t, mi.GetMetadata(EffectiveRepoPrefix(cm, config.RepoEntry{Path: canon})))

	// An explicit Name is honoured verbatim (the interactive-track case).
	assert.Equal(t, "custom", EffectiveRepoPrefix(cm, config.RepoEntry{Path: wt, Name: "custom"}))
}

func mustAbs(t *testing.T, p string) string {
	t.Helper()
	abs, err := filepath.Abs(p)
	require.NoError(t, err)
	return abs
}

func TestSanitizeInstanceTag(t *testing.T) {
	cases := map[string]string{
		"task-ws":       "task-ws",
		"feature/foo":   "feature-foo", // branch slashes can't reach a node-ID
		"a@b":           "a-b",
		"  spaced  ":    "spaced",
		"--edges--":     "edges",
		"keep.dots_1":   "keep.dots_1",
		"weird*chars!":  "weird-chars",
		"":              "",
		"///":           "",
		"release/1.2.3": "release-1.2.3",
	}
	for in, want := range cases {
		assert.Equal(t, want, sanitizeInstanceTag(in), "sanitizeInstanceTag(%q)", in)
	}
}

func TestWorktreeInstanceName_Unit(t *testing.T) {
	nonGit := t.TempDir() // not a git working tree

	// No declared workspace and no flag → keep the base prefix.
	name, sep := WorktreeInstanceName(nonGit, "lib", "", false)
	assert.False(t, sep)
	assert.Equal(t, "lib", name)

	// Declared workspace equal to the base prefix is not a separation
	// signal.
	name, sep = WorktreeInstanceName(nonGit, "lib", "lib", false)
	assert.False(t, sep)
	assert.Equal(t, "lib", name)

	// A declared workspace on a path that is NOT a linked worktree does
	// not auto-separate (only real worktrees do).
	name, sep = WorktreeInstanceName(nonGit, "lib", "other-ws", false)
	assert.False(t, sep)
	assert.Equal(t, "lib", name)

	// The explicit flag forces separation; the declared workspace is the
	// tag.
	name, sep = WorktreeInstanceName(nonGit, "lib", "task-ws", true)
	assert.True(t, sep)
	assert.Equal(t, "lib@task-ws", name)

	// Forced separation with no declared workspace and no resolvable
	// branch falls back to a stable path hash.
	name, sep = WorktreeInstanceName(nonGit, "lib", "", true)
	assert.True(t, sep)
	assert.True(t, len(name) > len("lib@"))
	assert.Equal(t, "lib@"+shortPathHash(nonGit), name)
	// Deterministic for the same path.
	name2, _ := WorktreeInstanceName(nonGit, "lib", "", true)
	assert.Equal(t, name, name2)
}
