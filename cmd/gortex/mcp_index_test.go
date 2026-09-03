package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/pathkey"
	"github.com/zzet/gortex/internal/platform"
	"github.com/zzet/gortex/internal/testenv"
)

// TestResolveEmbeddedIndex covers what the explicitly enabled embedded
// fallback is allowed to serve when no daemon answers. The opt-in permits
// the mode, not an arbitrary crawl, so inference remains conservative: it
// may serve a project directory, and it may not crawl a directory the user
// never asked it to index (gortexhq/gortex#418).
func TestResolveEmbeddedIndex(t *testing.T) {
	testenv.Sandbox(t)

	mkdir := func(parts ...string) string {
		dir := filepath.Join(parts...)
		require.NoError(t, os.MkdirAll(dir, 0o755))
		return dir
	}

	parent := t.TempDir()
	repoA := mkdir(parent, "repo-a")
	repoB := mkdir(parent, "repo-b")
	plain := t.TempDir() // a source folder with no tracked repo under it

	global := &config.GlobalConfig{
		Repos: []config.RepoEntry{{Path: repoA}, {Path: repoB}},
	}
	cacheNotebook := filepath.Join(platform.DataDir(), "notebook-cache")

	t.Run("explicit --index always wins and keeps the repo-local notebook", func(t *testing.T) {
		// The notebook is committed to git so it travels with the repo.
		// That is right only when the user named the tree.
		plan := resolveEmbeddedIndex(repoA, parent, global)
		canonicalRepoA := pathkey.CanonicalExistingRoot(repoA)
		assert.Equal(t, canonicalRepoA, plan.Index)
		assert.Equal(t, canonicalRepoA, plan.Notebook)
		assert.False(t, plan.Inferred)
		assert.Empty(t, plan.Refusal)
	})

	t.Run("a parent of tracked repos is refused, not crawled", func(t *testing.T) {
		plan := resolveEmbeddedIndex("", parent, global)
		assert.Empty(t, plan.Index,
			"indexing the parent would build a second, unscoped copy of every child")
		assert.NotEmpty(t, plan.Refusal)
		assert.Contains(t, plan.Refusal, "contains tracked repositories")
		assert.Equal(t, cacheNotebook, plan.Notebook,
			"an inferred root must not get a repo-local notebook")
	})

	t.Run("a tracked repo used as the cwd is indexed, not refused", func(t *testing.T) {
		// repoA contains no tracked repo BELOW it; it is one itself.
		plan := resolveEmbeddedIndex("", repoA, global)
		assert.Equal(t, repoA, plan.Index)
		assert.True(t, plan.Inferred)
		assert.Empty(t, plan.Refusal)
	})

	t.Run("an ordinary project directory is still served", func(t *testing.T) {
		// The case the inference exists for: no daemon, user opens their
		// project. Regressing this returns an empty graph, which reads
		// to the user as "gortex is broken".
		plan := resolveEmbeddedIndex("", plain, global)
		assert.Equal(t, plain, plan.Index)
		assert.True(t, plan.Inferred)
		assert.Equal(t, cacheNotebook, plan.Notebook)
	})

	t.Run("unsafe roots are refused", func(t *testing.T) {
		home, err := os.UserHomeDir()
		require.NoError(t, err)
		for _, cwd := range []string{string(filepath.Separator), home} {
			plan := resolveEmbeddedIndex("", cwd, global)
			assert.Empty(t, plan.Index, "must refuse to index %s", cwd)
			assert.NotEmpty(t, plan.Refusal, "refusal for %s must be explained", cwd)
		}
	})

	t.Run("an empty cwd yields no index", func(t *testing.T) {
		plan := resolveEmbeddedIndex("", "", global)
		assert.Empty(t, plan.Index)
		assert.NotEmpty(t, plan.Refusal)
	})

	t.Run("no config known degrades to indexing the cwd", func(t *testing.T) {
		plan := resolveEmbeddedIndex("", parent, nil)
		assert.Equal(t, parent, plan.Index,
			"with no tracked-repo list there is nothing to protect; keep serving the cwd")
	})
}

// TestEmbeddedModeNote checks the degraded-mode banner names the corpus.
// The embedded fallback announced itself only on stderr, which MCP hosts
// routinely discard — so neither the user nor the agent could tell an
// embedded single-tree answer from a daemon-backed one.
func TestEmbeddedModeNote(t *testing.T) {
	served := embeddedModeNote(embeddedIndexPlan{Index: "/src/app"})
	assert.Contains(t, served, "DEGRADED")
	assert.Contains(t, served, "/src/app")

	refused := embeddedModeNote(embeddedIndexPlan{Refusal: "it contains tracked repositories (a, b)"})
	assert.Contains(t, refused, "DEGRADED")
	assert.Contains(t, refused, "contains tracked repositories")
	assert.Contains(t, refused, "return empty",
		"an agent must be told the graph is empty rather than infer it from zero results")
}

// TestTrackedReposUnder pins the containment rule the refusal rests on.
func TestTrackedReposUnder(t *testing.T) {
	parent := t.TempDir()
	repoA := filepath.Join(parent, "repo-a")
	require.NoError(t, os.MkdirAll(repoA, 0o755))
	outside := t.TempDir()

	global := &config.GlobalConfig{Repos: []config.RepoEntry{{Path: repoA}, {Path: outside}}}

	assert.Equal(t, []string{repoA}, trackedReposUnder(parent, global))
	assert.Empty(t, trackedReposUnder(repoA, global),
		"a tracked repo contains no tracked repo below it — index it, don't refuse it")
	assert.Empty(t, trackedReposUnder(t.TempDir(), global))
	assert.Empty(t, trackedReposUnder(parent, nil))
}

// TestEmbeddedFallback_LeavesNoGortexDirInLaunchCWD is the filesystem
// half of #418: even an explicitly enabled embedded server must not create
// a .gortex/ directory in whatever tree the MCP client happened to launch
// from. The notebook sidecar used to open eagerly at startup and leave an
// untracked `.gortex/` in a production repo's `git status`.
func TestEmbeddedFallback_LeavesNoGortexDirInLaunchCWD(t *testing.T) {
	testenv.Sandbox(t)

	parent := t.TempDir()
	repoA := filepath.Join(parent, "repo-a")
	require.NoError(t, os.MkdirAll(repoA, 0o755))
	global := &config.GlobalConfig{Repos: []config.RepoEntry{{Path: repoA}}}

	plan := resolveEmbeddedIndex("", parent, global)
	require.NotEqual(t, parent, plan.Notebook,
		"the notebook root must never be an inferred launch cwd")
	require.NotEqual(t, parent, plan.Index)

	// Opening the notebook at the resolved root must touch the cache,
	// never the launch tree.
	_, statErr := os.Stat(filepath.Join(parent, ".gortex"))
	assert.True(t, os.IsNotExist(statErr),
		"embedded startup created %s/.gortex — that is the untracked directory users saw in git status", parent)
}
