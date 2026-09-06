package main

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
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

// A Windows daemon can serve both newer slash keys and retained native keys.
// Probe native filesystem paths against each stored spelling in both views.
func TestCoverageAcceptsStoredPathSpellings(t *testing.T) {
	const rel = "pkg/nested/a.go"
	for _, spelling := range []struct {
		name string
		key  string
	}{
		{name: "slash", key: probePrefix + "/" + rel},
		{name: "native", key: probePrefix + "/" + filepath.FromSlash(rel)},
	} {
		for _, routed := range []bool{false, true} {
			viewName := "base"
			if routed {
				viewName = "worktree"
			}
			t.Run(spelling.name+"/"+viewName, func(t *testing.T) {
				f := newProbeFixture(t)
				key := spelling.key
				f.store.AddBatch([]*graph.Node{{
					ID: key, Kind: graph.KindFile, Name: "a.go",
					FilePath: key, RepoPrefix: probePrefix, Language: "go",
				}, {
					ID: key + "::BaseSymbol", Kind: graph.KindFunction, Name: "BaseSymbol",
					FilePath: key, RepoPrefix: probePrefix, Language: "go",
				}}, nil)
				root := f.primaryRoot
				if routed {
					f.routeWorktreeFile(t, key)
					root = f.worktreeRoot
				}

				ctx := context.Background()
				file, err := f.controller.FileCoverage(ctx, daemon.FileCoverageParams{
					Path: filepath.Join(root, filepath.FromSlash(rel)),
				})
				require.NoError(t, err)
				assert.True(t, file.Answered)
				assert.True(t, file.Tracked)
				assert.True(t, file.Held)
				assert.True(t, file.Covered)
				assert.Equal(t, 1, file.Symbols)

				scope, err := f.controller.DirCoverage(ctx, daemon.DirCoverageParams{
					Path: filepath.Join(root, "pkg", "nested"),
				})
				require.NoError(t, err)
				assert.True(t, scope.Answered)
				assert.True(t, scope.HasSource)
				assert.False(t, scope.Walked)
			})
		}
	}
}

func TestFileCoveragePrefersCanonicalKeysWithoutDoubleCounting(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("native and slash graph keys coincide on this platform")
	}
	f := newProbeFixture(t)
	// The fixture already holds two symbols under the native key. A newer
	// writer's canonical copy must be authoritative, without adding both.
	canonical := probePrefix + "/" + probeFile
	f.store.AddBatch([]*graph.Node{{
		ID: canonical + "::Canonical", Kind: graph.KindFunction, Name: "Canonical",
		FilePath: canonical, RepoPrefix: probePrefix, Language: "go",
	}}, nil)
	coverage, err := f.controller.FileCoverage(context.Background(), daemon.FileCoverageParams{
		Path: filepath.Join(f.primaryRoot, filepath.FromSlash(probeFile)),
	})
	require.NoError(t, err)
	assert.True(t, coverage.Covered)
	assert.Equal(t, 1, coverage.Symbols)
}

func TestPathUnderDirPreservesPOSIXBackslashNames(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("backslashes are separators on Windows")
	}
	assert.False(t, pathUnderDir(`repo/pkg\nested/a.go`, "repo/pkg/nested"))
	assert.True(t, pathUnderDir(`repo/pkg\nested/a.go`, `repo/pkg\nested`))
}

// The admission flags are what let a caller tell "not indexed yet" from "never
// will be". They come from the same gate the index walk runs, reached through
// the indexer that owns the path — a lookup that silently resolves to no
// indexer would leave both flags false forever and nothing would fail.
func TestFileCoverageCarriesTheWalksAdmissionVerdict(t *testing.T) {
	c, mi, _, dir := buildCatalogController(t)
	root := filepath.Join(dir, "repo")
	for rel, body := range map[string]string{
		"pkg/nested/a.go":         "package nested\n\nfunc Exported() {}\n",
		"node_modules/x/index.js": "module.exports = {}\n",
	} {
		abs := filepath.Join(root, filepath.FromSlash(rel))
		require.NoError(t, os.MkdirAll(filepath.Dir(abs), 0o755))
		require.NoError(t, os.WriteFile(abs, []byte(body), 0o644))
	}
	result, err := mi.TrackRepoCtx(context.Background(), config.RepoEntry{Path: root})
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, 1, result.FileCount, "index result: %+v", result)
	require.Empty(t, result.Errors, "index result: %+v", result)

	ctx := context.Background()

	indexed, err := c.FileCoverage(ctx, daemon.FileCoverageParams{
		Path: filepath.Join(root, "pkg", "nested", "a.go"),
	})
	require.NoError(t, err)
	require.True(t, indexed.Answered)
	assert.True(t, indexed.Tracked, "coverage: %+v; index result: %+v", indexed, result)
	assert.True(t, indexed.Held, "coverage: %+v; index result: %+v", indexed, result)
	assert.True(t, indexed.Covered, "coverage: %+v; index result: %+v", indexed, result)
	assert.False(t, indexed.Unindexable,
		"a covered file is never asked about, so the walk's opinion costs nothing")
	assert.False(t, indexed.Excluded)
	for _, rel := range []string{".", "pkg", "pkg/nested"} {
		scope, err := c.DirCoverage(ctx, daemon.DirCoverageParams{
			Path: filepath.Join(root, filepath.FromSlash(rel)),
		})
		require.NoError(t, err)
		assert.True(t, scope.Answered, "%s: %+v", rel, scope)
		assert.True(t, scope.HasSource, "%s: %+v", rel, scope)
		assert.False(t, scope.Walked, "a held scope needs no admission walk: %s", rel)
	}

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
