package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/daemon"
	"github.com/zzet/gortex/internal/graph"
)

// The coverage answer's whole job at the hook boundary is to say whether it is
// an answer. A caller that cannot tell "the graph does not hold this file"
// from "nothing could be asked" turns every daemon hiccup into a silent
// enforcement bypass, which is the failure the flags exist to prevent.
func TestFileCoverageSeparatesAVerdictFromAnAbstention(t *testing.T) {
	t.Run("a routed worktree answers about its own view", func(t *testing.T) {
		f := newProbeFixture(t)
		f.routeWorktree(t)

		coverage, err := f.controller.FileCoverage(context.Background(),
			daemon.FileCoverageParams{Path: filepath.Join(f.worktreeRoot, probeFile)})
		require.NoError(t, err)
		assert.True(t, coverage.Answered, "the composed view was read")
		assert.True(t, coverage.Tracked)
		assert.True(t, coverage.Held, "the graph holds the file")
		assert.True(t, coverage.Covered)
		assert.Equal(t, 1, coverage.Symbols)
	})

	t.Run("an unrouted worktree abstains rather than reporting uncovered", func(t *testing.T) {
		f := newProbeFixture(t)
		f.controller.probeReconcile = func(string) {}
		f.controller.probeActivateCheckout = func(string) bool { return true }

		coverage, err := f.controller.FileCoverage(context.Background(),
			daemon.FileCoverageParams{Path: filepath.Join(f.worktreeRoot, probeFile)})
		require.NoError(t, err)
		assert.False(t, coverage.Covered, "nothing describes this working copy yet")
		assert.False(t, coverage.Answered,
			"an unbuilt view is no evidence about the file; reporting one would let a native read through on a covered path")
		assert.True(t, coverage.Tracked,
			"the checkout is registered even though nothing can answer for it yet")
	})

	t.Run("an untracked path is answered, not abstained", func(t *testing.T) {
		f := newProbeFixture(t)

		coverage, err := f.controller.FileCoverage(context.Background(),
			daemon.FileCoverageParams{Path: filepath.Join(t.TempDir(), "elsewhere.go")})
		require.NoError(t, err)
		assert.True(t, coverage.Answered,
			"the daemon looked and placed the path outside every corpus, which is a verdict")
		assert.False(t, coverage.Tracked)
		assert.False(t, coverage.Covered)
		assert.False(t, coverage.Held)
	})

	t.Run("a held file with no definitions is Held but not Covered", func(t *testing.T) {
		f := newProbeFixture(t)
		docKey := probePrefix + "/" + filepath.FromSlash("docs/notes.go")
		f.store.AddBatch([]*graph.Node{{
			ID:         docKey,
			Kind:       graph.KindFile,
			Name:       "notes.go",
			FilePath:   docKey,
			RepoPrefix: probePrefix,
			Language:   "go",
		}}, nil)

		coverage, err := f.controller.FileCoverage(context.Background(),
			daemon.FileCoverageParams{Path: filepath.Join(f.primaryRoot, "docs/notes.go")})
		require.NoError(t, err)
		assert.True(t, coverage.Answered)
		assert.True(t, coverage.Held, "the walk indexed the file")
		assert.False(t, coverage.Covered, "it defines nothing")
		assert.Zero(t, coverage.Symbols)
	})
}

// The admission flags are what let a caller tell "not indexed yet" from "never
// will be". They come from the same gate the index walk runs, reached through
// the indexer that owns the path — a lookup that silently resolves to no
// indexer would leave both flags false forever and nothing would fail.
func TestFileCoverageCarriesTheWalksAdmissionVerdict(t *testing.T) {
	c, mi, _, dir := buildCatalogController(t)
	root := filepath.Join(dir, "repo")
	for rel, body := range map[string]string{
		"pkg/a.go":                "package pkg\n\nfunc Exported() {}\n",
		"node_modules/x/index.js": "module.exports = {}\n",
	} {
		abs := filepath.Join(root, filepath.FromSlash(rel))
		require.NoError(t, os.MkdirAll(filepath.Dir(abs), 0o755))
		require.NoError(t, os.WriteFile(abs, []byte(body), 0o644))
	}
	_, err := mi.TrackRepoCtx(context.Background(), config.RepoEntry{Path: root})
	require.NoError(t, err)

	ctx := context.Background()

	indexed, err := c.FileCoverage(ctx, daemon.FileCoverageParams{
		Path: filepath.Join(root, "pkg", "a.go"),
	})
	require.NoError(t, err)
	require.True(t, indexed.Answered)
	assert.True(t, indexed.Covered, "the walk indexed this file")
	assert.False(t, indexed.Unindexable,
		"a covered file is never asked about, so the walk's opinion costs nothing")
	assert.False(t, indexed.Excluded)

	vendored, err := c.FileCoverage(ctx, daemon.FileCoverageParams{
		Path: filepath.Join(root, "node_modules", "x", "index.js"),
	})
	require.NoError(t, err)
	require.True(t, vendored.Answered)
	assert.False(t, vendored.Covered)
	assert.True(t, vendored.Unindexable, "the walk will never hold a vendored file")
	assert.True(t, vendored.Excluded, "and an ignore rule is why")
}

// A path no indexer owns must leave both admission flags false as an
// abstention. Reading that silence as "indexable" would make every untracked
// path look like source the walk simply has not reached.
func TestFileCoverageAdmissionAbstainsWithoutAnOwningIndexer(t *testing.T) {
	f := newProbeFixture(t)

	coverage, err := f.controller.FileCoverage(context.Background(),
		daemon.FileCoverageParams{Path: filepath.Join(f.primaryRoot, "pkg", "unknown.go")})
	require.NoError(t, err)
	assert.False(t, coverage.Unindexable)
	assert.False(t, coverage.Excluded)
}
