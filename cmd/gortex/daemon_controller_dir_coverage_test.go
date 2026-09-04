package main

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/daemon"
	"github.com/zzet/gortex/internal/graph"
)

// seedFileNode adds the file node an indexed file carries. The scope answer
// reads those, exactly as the find_files probe it replaces did.
func seedFileNode(f *probeFixture, rel string) {
	key := probePrefix + "/" + filepath.FromSlash(rel)
	f.store.AddBatch([]*graph.Node{{
		ID:         key,
		Kind:       graph.KindFile,
		Name:       filepath.Base(rel),
		FilePath:   key,
		RepoPrefix: probePrefix,
		Language:   "go",
	}}, nil)
}

// The scope answer carries the same burden the per-file one does: a caller
// that cannot tell "the graph holds nothing under here" from "nothing could be
// asked" turns a warming index into a silent enforcement bypass.
func TestDirCoverageSeparatesAVerdictFromAnAbstention(t *testing.T) {
	t.Run("a scope the graph holds a file under", func(t *testing.T) {
		f := newProbeFixture(t)
		seedFileNode(f, probeFile)

		coverage, err := f.controller.DirCoverage(context.Background(),
			daemon.DirCoverageParams{Path: filepath.Join(f.primaryRoot, "internal")})
		require.NoError(t, err)
		assert.True(t, coverage.Answered)
		assert.True(t, coverage.Tracked)
		assert.True(t, coverage.HasSource, "the graph holds internal/live.go")
	})

	t.Run("the repository root is every file's scope", func(t *testing.T) {
		f := newProbeFixture(t)
		seedFileNode(f, probeFile)

		coverage, err := f.controller.DirCoverage(context.Background(),
			daemon.DirCoverageParams{Path: f.primaryRoot})
		require.NoError(t, err)
		assert.True(t, coverage.Answered)
		assert.True(t, coverage.HasSource,
			"a root scope measures as \".\", which must not be matched against as a path")
	})

	t.Run("a sibling scope the graph holds nothing under abstains", func(t *testing.T) {
		f := newProbeFixture(t)
		seedFileNode(f, probeFile)

		coverage, err := f.controller.DirCoverage(context.Background(),
			daemon.DirCoverageParams{Path: filepath.Join(f.primaryRoot, "docs")})
		require.NoError(t, err)
		assert.True(t, coverage.Answered)
		assert.False(t, coverage.HasSource)
		assert.False(t, coverage.Walked,
			"no indexer owns the scope, so the walk has no opinion and must not claim one")
	})

	t.Run("an untracked scope is answered, not abstained", func(t *testing.T) {
		f := newProbeFixture(t)

		coverage, err := f.controller.DirCoverage(context.Background(),
			daemon.DirCoverageParams{Path: t.TempDir()})
		require.NoError(t, err)
		assert.True(t, coverage.Answered,
			"the daemon looked and placed the scope outside every corpus, which is a verdict")
		assert.False(t, coverage.Tracked)
		assert.False(t, coverage.HasSource)
	})

	t.Run("an unrouted worktree abstains rather than reporting an empty scope", func(t *testing.T) {
		f := newProbeFixture(t)
		f.controller.probeReconcile = func(string) {}
		f.controller.probeActivateCheckout = func(string) bool { return true }

		coverage, err := f.controller.DirCoverage(context.Background(),
			daemon.DirCoverageParams{Path: filepath.Join(f.worktreeRoot, "internal")})
		require.NoError(t, err)
		assert.False(t, coverage.Answered,
			"an unbuilt view is no evidence about the scope")
		assert.False(t, coverage.HasSource)
		assert.True(t, coverage.Tracked)
	})

	// A prefix match without the separator lets one package answer for another.
	t.Run("a scope does not answer for its lexical neighbour", func(t *testing.T) {
		f := newProbeFixture(t)
		seedFileNode(f, "internalx/other.go")

		coverage, err := f.controller.DirCoverage(context.Background(),
			daemon.DirCoverageParams{Path: filepath.Join(f.primaryRoot, "internalx")})
		require.NoError(t, err)
		assert.True(t, coverage.HasSource, "internalx holds its own file")

		f2 := newProbeFixture(t)
		seedFileNode(f2, probeFile)
		coverage, err = f2.controller.DirCoverage(context.Background(),
			daemon.DirCoverageParams{Path: filepath.Join(f2.primaryRoot, "internalx")})
		require.NoError(t, err)
		assert.False(t, coverage.HasSource,
			"internal/live.go must not answer for the internalx scope")
	})

	t.Run("an empty path is not a probe", func(t *testing.T) {
		f := newProbeFixture(t)

		coverage, err := f.controller.DirCoverage(context.Background(), daemon.DirCoverageParams{})
		require.NoError(t, err)
		assert.False(t, coverage.Answered)
	})
}

func TestDirKeyPrefix(t *testing.T) {
	for _, tc := range []struct{ key, want string }{
		{"repo/internal", "repo/internal"},
		{"repo/.", "repo"},
		{".", ""},
		{"internal/hooks", "internal/hooks"},
	} {
		assert.Equal(t, tc.want, dirKeyPrefix(tc.key), "dirKeyPrefix(%q)", tc.key)
	}
}

func TestPathUnderDir(t *testing.T) {
	assert.True(t, pathUnderDir("repo/internal/a.go", "repo/internal"))
	assert.True(t, pathUnderDir("repo\\internal\\a.go", "repo/internal"))
	assert.False(t, pathUnderDir("repo/internalx/a.go", "repo/internal"))
	assert.False(t, pathUnderDir("repo/internal", "repo/internal"))
	assert.False(t, pathUnderDir("other/internal/a.go", "repo/internal"))
}
