package indexer

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/search"
)

// TestMultiIndexer_FixtureScope_WorkspaceFromGortexYAML verifies that
// indexFixtureScope picks up the workspace slug declared in a repo's own
// `.gortex.yaml` — the typical first-party setup. Repos with a
// different workspace slug must be excluded.
func TestMultiIndexer_FixtureScope_WorkspaceFromGortexYAML(t *testing.T) {
	repoA := setupRepoDir(t, "repo-a")
	repoB := setupRepoDir(t, "repo-b")
	require.NoError(t, os.WriteFile(filepath.Join(repoA, ".gortex.yaml"),
		[]byte("workspace: alpha\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(repoB, ".gortex.yaml"),
		[]byte("workspace: beta\n"), 0o644))

	tmpCfg := filepath.Join(t.TempDir(), "config.yaml")
	gc := &config.GlobalConfig{
		Repos: []config.RepoEntry{
			{Path: repoA, Name: "repo-a"},
			{Path: repoB, Name: "repo-b"},
		},
	}
	gc.SetConfigPath(tmpCfg)
	require.NoError(t, gc.Save())

	cm, err := config.NewConfigManager(tmpCfg)
	require.NoError(t, err)

	g := graph.New()
	mi := NewMultiIndexer(g, newTestRegistry(), search.NewNull(), cm, zap.NewNop())

	results, err := mi.indexFixtureScope("alpha", "")
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Contains(t, results, "repo-a")
	assert.NotContains(t, results, "repo-b")
	assert.Nil(t, mi.GetMetadata("repo-b"))
}

// TestMultiIndexer_FixtureScope_RepoEntryOverride verifies that a
// global-config RepoEntry.Workspace override beats the repo's own
// `.gortex.yaml::workspace` slug — the user pinning OSS / read-only
// repos into a workspace without leaving an artifact in them.
func TestMultiIndexer_FixtureScope_RepoEntryOverride(t *testing.T) {
	repoA := setupRepoDir(t, "repo-a")
	require.NoError(t, os.WriteFile(filepath.Join(repoA, ".gortex.yaml"),
		[]byte("workspace: upstream\n"), 0o644))

	tmpCfg := filepath.Join(t.TempDir(), "config.yaml")
	gc := &config.GlobalConfig{
		Repos: []config.RepoEntry{
			{Path: repoA, Name: "repo-a", Workspace: "local-pin"},
		},
	}
	gc.SetConfigPath(tmpCfg)
	require.NoError(t, gc.Save())

	cm, err := config.NewConfigManager(tmpCfg)
	require.NoError(t, err)

	g := graph.New()
	mi := NewMultiIndexer(g, newTestRegistry(), search.NewNull(), cm, zap.NewNop())

	// Override wins over .gortex.yaml: "upstream" matches nothing.
	_, err = mi.indexFixtureScope("upstream", "")
	require.Error(t, err)

	// "local-pin" matches the override.
	results, err := mi.indexFixtureScope("local-pin", "")
	require.NoError(t, err)
	require.Len(t, results, 1)
}

// TestMultiIndexer_FixtureScope_FallsBackToRepoPrefix verifies the
// default: repos with no `.gortex.yaml::workspace` and no
// RepoEntry.Workspace use the repo prefix as their workspace slug.
func TestMultiIndexer_FixtureScope_FallsBackToRepoPrefix(t *testing.T) {
	repoA := setupRepoDir(t, "repo-a")
	repoB := setupRepoDir(t, "repo-b")

	tmpCfg := filepath.Join(t.TempDir(), "config.yaml")
	gc := &config.GlobalConfig{
		Repos: []config.RepoEntry{
			{Path: repoA, Name: "repo-a"},
			{Path: repoB, Name: "repo-b"},
		},
	}
	gc.SetConfigPath(tmpCfg)
	require.NoError(t, gc.Save())

	cm, err := config.NewConfigManager(tmpCfg)
	require.NoError(t, err)

	g := graph.New()
	mi := NewMultiIndexer(g, newTestRegistry(), search.NewNull(), cm, zap.NewNop())

	results, err := mi.indexFixtureScope("repo-b", "")
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Contains(t, results, "repo-b")
}

// TestMultiIndexer_FixtureScope_ProjectNarrowsInsideWorkspace verifies
// that workspace + project filters are AND-combined. Two repos in the
// same workspace but different projects → only the matching project
// gets indexed.
func TestMultiIndexer_FixtureScope_ProjectNarrowsInsideWorkspace(t *testing.T) {
	repoA := setupRepoDir(t, "repo-a")
	repoB := setupRepoDir(t, "repo-b")
	require.NoError(t, os.WriteFile(filepath.Join(repoA, ".gortex.yaml"),
		[]byte("workspace: shared\nproject: api\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(repoB, ".gortex.yaml"),
		[]byte("workspace: shared\nproject: worker\n"), 0o644))

	tmpCfg := filepath.Join(t.TempDir(), "config.yaml")
	gc := &config.GlobalConfig{
		Repos: []config.RepoEntry{
			{Path: repoA, Name: "repo-a"},
			{Path: repoB, Name: "repo-b"},
		},
	}
	gc.SetConfigPath(tmpCfg)
	require.NoError(t, gc.Save())

	cm, err := config.NewConfigManager(tmpCfg)
	require.NoError(t, err)

	g := graph.New()
	mi := NewMultiIndexer(g, newTestRegistry(), search.NewNull(), cm, zap.NewNop())

	results, err := mi.indexFixtureScope("shared", "api")
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Contains(t, results, "repo-a")
}

// TestMultiIndexer_FixtureScope_NoMatchErrors verifies that a typo'd
// filter surfaces as an error rather than producing a silent empty
// graph.
func TestMultiIndexer_FixtureScope_NoMatchErrors(t *testing.T) {
	repoA := setupRepoDir(t, "repo-a")

	tmpCfg := filepath.Join(t.TempDir(), "config.yaml")
	gc := &config.GlobalConfig{
		Repos: []config.RepoEntry{{Path: repoA, Name: "repo-a"}},
	}
	gc.SetConfigPath(tmpCfg)
	require.NoError(t, gc.Save())

	cm, err := config.NewConfigManager(tmpCfg)
	require.NoError(t, err)

	g := graph.New()
	mi := NewMultiIndexer(g, newTestRegistry(), search.NewNull(), cm, zap.NewNop())

	_, err = mi.indexFixtureScope("nonexistent", "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "scope filter matched zero")
	assert.Equal(t, 0, g.NodeCount(), "no nodes should be added on a zero-match scope")
}

// TestMultiIndexer_FixtureScope_EmptyFiltersEqualsIndexAll verifies
// that empty/empty filters degrade to IndexAll behaviour: every
// active repo is indexed.
func TestMultiIndexer_FixtureScope_EmptyFiltersEqualsIndexAll(t *testing.T) {
	repoA := setupRepoDir(t, "repo-a")
	repoB := setupRepoDir(t, "repo-b")
	require.NoError(t, os.WriteFile(filepath.Join(repoA, ".gortex.yaml"),
		[]byte("workspace: alpha\n"), 0o644))

	tmpCfg := filepath.Join(t.TempDir(), "config.yaml")
	gc := &config.GlobalConfig{
		Repos: []config.RepoEntry{
			{Path: repoA, Name: "repo-a"},
			{Path: repoB, Name: "repo-b"},
		},
	}
	gc.SetConfigPath(tmpCfg)
	require.NoError(t, gc.Save())

	cm, err := config.NewConfigManager(tmpCfg)
	require.NoError(t, err)

	g := graph.New()
	mi := NewMultiIndexer(g, newTestRegistry(), search.NewNull(), cm, zap.NewNop())

	results, err := mi.indexFixtureScope("", "")
	require.NoError(t, err)
	assert.Len(t, results, 2)
	assert.Contains(t, results, "repo-a")
	assert.Contains(t, results, "repo-b")
}
