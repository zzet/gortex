package indexer

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/resolver"
	"github.com/zzet/gortex/internal/search"
)

func absRootForTest(t *testing.T, root string) string {
	t.Helper()
	abs, err := filepath.Abs(root)
	require.NoError(t, err)
	return abs
}

// A repository with no JS/TS file never walks for resolution inputs: not on
// the full index, not on a file save, and not on a whole-root rescan.
func TestResolutionDiscovery_GoOnlyRepositorySkipsWalk(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "main.go")
	writeFileMk(t, file, "package main\nfunc main() {}\n")
	abs := absRootForTest(t, root)
	invalidateResolutionInputs(abs)
	idx, store := newSQLiteIndexer(t)
	_, err := idx.Index(root)
	require.NoError(t, err)

	bumpMtime(t, file, "package main\nfunc main() { helper() }\nfunc helper() {}\n")
	_, err = idx.IncrementalReindexPaths(root, []string{file})
	require.NoError(t, err)
	_, err = idx.IncrementalReindexPaths(root, nil)
	require.NoError(t, err)

	require.Zero(t, resolutionDiscoveryWalkCount(abs))
	digest, err := store.TSAliasDigest(idx.repoPrefix)
	require.NoError(t, err)
	require.Equal(t, inertResolutionDigest, digest, "a pass that read no input certifies the inert set")
	require.True(t, idx.censusResolutionCurrent(abs))
	require.Zero(t, resolutionDiscoveryWalkCount(abs))
}

// The first JS/TS files in a Go repository still get alias-directed bindings
// and a certified policy.
func TestResolutionDiscovery_FirstJSFilesInGoRepository(t *testing.T) {
	root := t.TempDir()
	writeFileMk(t, filepath.Join(root, "main.go"), "package main\nfunc main() {}\n")
	abs := absRootForTest(t, root)
	invalidateResolutionInputs(abs)
	idx, store := newSQLiteIndexer(t)
	_, err := idx.Index(root)
	require.NoError(t, err)
	require.Zero(t, resolutionDiscoveryWalkCount(abs))

	configPath := aliasRebindFixture(t, root)
	paths := []string{
		configPath,
		filepath.Join(root, "old/auth.ts"),
		filepath.Join(root, "new/auth.ts"),
		filepath.Join(root, "pages/index.ts"),
	}
	_, err = idx.IncrementalReindexPaths(root, paths)
	require.NoError(t, err)
	require.Equal(t, "old/auth.ts::useRedirect", callTargetForName(t, idx.Graph(), "pages/index.ts", "run"))
	digest, err := store.TSAliasDigest(idx.repoPrefix)
	require.NoError(t, err)
	require.NotEmpty(t, digest)
}

// A certified digest records its inputs beside it.
func TestResolutionDiscovery_CertifiedDigestRecordsInputs(t *testing.T) {
	root := t.TempDir()
	aliasRebindFixture(t, root)
	abs := absRootForTest(t, root)
	invalidateResolutionInputs(abs)
	idx, store := newSQLiteIndexer(t)
	_, err := idx.Index(root)
	require.NoError(t, err)
	require.Equal(t, int64(1), resolutionDiscoveryWalkCount(abs))
	inputs, err := store.TSAliasInputs(idx.repoPrefix)
	require.NoError(t, err)
	require.Contains(t, inputs, "tsconfig.json")
	require.NotEmpty(t, inputs["tsconfig.json"])
}

// A failed full resolve does not fail an index whose resolution inputs are
// unchanged; it withholds certification so the next pass resolves again.
func TestResolutionDiscovery_FailedFullResolveWithUnchangedInputsCompletes(t *testing.T) {
	root := t.TempDir()
	aliasRebindFixture(t, root)
	invalidateResolutionInputs(absRootForTest(t, root))
	idx, store := newSQLiteIndexer(t)
	_, err := idx.Index(root)
	require.NoError(t, err)
	certified, err := store.TSAliasDigest(idx.repoPrefix)
	require.NoError(t, err)
	require.NotEmpty(t, certified)

	healthy := idx.resolver
	idx.resolver = resolver.New(failingResolveStore{Store: store, err: errors.New("injected pending page failure")})
	result, err := idx.Index(root)
	idx.resolver = healthy
	require.NoError(t, err)
	require.NotNil(t, result)
	digest, err := store.TSAliasDigest(idx.repoPrefix)
	require.NoError(t, err)
	require.Empty(t, digest, "a failed resolve withholds the certificate")

	_, err = idx.Index(root)
	require.NoError(t, err)
	digest, err = store.TSAliasDigest(idx.repoPrefix)
	require.NoError(t, err)
	require.Equal(t, certified, digest)
}

// A clean warm restart rechecks the recorded inputs instead of walking the
// checkout, and the next save reuses the restored inputs.
func TestResolutionDiscovery_CleanWarmRestartDoesNotWalk(t *testing.T) {
	root := t.TempDir()
	configPath := aliasRebindFixture(t, root)
	parent := filepath.Join(root, ".nuxt", "aliases.json")
	writeFileMk(t, configPath, `{"extends":"./.nuxt/aliases.json"}`)
	writeFileMk(t, parent, `{"compilerOptions":{"paths":{"~/*":["../old/*"]}}}`)

	entry := config.RepoEntry{Path: root, Name: "repo"}
	configFile := filepath.Join(t.TempDir(), "config.yaml")
	gc := &config.GlobalConfig{Repos: []config.RepoEntry{entry}}
	gc.SetConfigPath(configFile)
	require.NoError(t, gc.Save())
	cm, err := config.NewConfigManager(configFile)
	require.NoError(t, err)
	idx, store := newSQLiteIndexer(t)
	abs := absRootForTest(t, root)
	start := func() *MultiIndexer {
		invalidateResolutionInputs(abs)
		return NewMultiIndexer(store, idx.registry, search.NewNull(), cm, zap.NewNop())
	}
	first := start()
	_, err = first.TrackRepoCtx(t.Context(), entry)
	require.NoError(t, err)
	mtimes := first.FileMtimes("repo")

	walks := resolutionDiscoveryWalkCount(abs)
	second := start()
	result, err := second.ReconcileRepoCtx(t.Context(), entry, mtimes)
	require.NoError(t, err)
	require.Zero(t, result.StaleFileCount)
	require.Equal(t, walks, resolutionDiscoveryWalkCount(abs), "a clean restart must not walk the checkout")

	caller := filepath.Join(root, "pages/index.ts")
	bumpMtime(t, caller, "import { useRedirect } from '~/auth'\nexport function run() { useRedirect(); return useRedirect() }\n")
	_, err = second.GetIndexer("repo").IncrementalReindexPaths(root, []string{caller})
	require.NoError(t, err)
	require.Equal(t, walks, resolutionDiscoveryWalkCount(abs), "the first save after a restart reuses the restored inputs")
	require.Equal(t, "repo/old/auth.ts::useRedirect", callTargetForName(t, store, "repo/pages/index.ts", "run"))
}

// Recorded inputs that no longer match fall back to the walk.
func TestResolutionDiscovery_RestoreRejectsChangedInputs(t *testing.T) {
	root := t.TempDir()
	configPath := aliasRebindFixture(t, root)
	abs := absRootForTest(t, root)
	current, err := discoverResolutionInputs(abs, nil)
	require.NoError(t, err)
	restored := restoreResolutionInputs(abs, current.digest, current.reads)
	require.NotNil(t, restored)
	require.Equal(t, current.digest, restored.digest)

	require.Nil(t, restoreResolutionInputs(abs, "other-digest", current.reads))
	require.NoError(t, os.WriteFile(configPath, []byte(`{"compilerOptions":{"paths":{"~/*":["new/*"]}}}`), 0o644))
	require.Nil(t, restoreResolutionInputs(abs, current.digest, current.reads))
}

// A save rereads an input only when its stat moved; a touch without a content
// change keeps the inputs, and a deleted point needs no walk.
func TestResolutionDiscovery_SaveRechecksInputsByStat(t *testing.T) {
	root := t.TempDir()
	configPath := aliasRebindFixture(t, root)
	abs := absRootForTest(t, root)
	current, err := discoverResolutionInputs(abs, nil)
	require.NoError(t, err)
	caller := filepath.Join(abs, "pages", "index.ts")
	require.True(t, current.unchangedForPoints(abs, []string{caller}, nil))

	later := time.Now().Add(time.Hour).Truncate(time.Second)
	require.NoError(t, os.Chtimes(configPath, later, later))
	require.True(t, current.unchangedForPoints(abs, []string{caller}, nil), "a touch keeps the inputs")
	require.Equal(t, later.UnixNano(), current.stats["tsconfig.json"].mtime, "the new stat is remembered")

	gone := filepath.Join(abs, "pages", "gone.ts")
	require.True(t, current.unchangedForPoints(abs, []string{gone}, nil), "a deleted point is not a discovery trigger")

	require.NoError(t, os.WriteFile(configPath, []byte(`{"compilerOptions":{"paths":{"~/*":["new/*"]}}}`), 0o644))
	require.False(t, current.unchangedForPoints(abs, []string{caller}, nil))
}

// A generated import list that appeared in an ignored directory since the
// record is invisible to a clean census, so restore must refuse it.
func TestResolutionDiscovery_RestoreRejectsNewGeneratedList(t *testing.T) {
	root := t.TempDir()
	writeFileMk(t, filepath.Join(root, "nuxt.config.ts"), "export default defineNuxtConfig({})\n")
	abs := absRootForTest(t, root)
	current, err := discoverResolutionInputs(abs, nil)
	require.NoError(t, err)
	require.NotNil(t, restoreResolutionInputs(abs, current.digest, current.reads))

	writeFileMk(t, filepath.Join(root, ".nuxt", "imports.d.ts"), "export { useAuth } from '../auth'\n")
	require.Nil(t, restoreResolutionInputs(abs, current.digest, current.reads))
}

// A stat taken in the same mtime tick as a later same-size rewrite would match
// it, so a recent stat never stands in for the bytes.
func TestResolutionDiscovery_RacyStatDoesNotVouchForInputs(t *testing.T) {
	root := t.TempDir()
	configPath := aliasRebindFixture(t, root)
	abs := absRootForTest(t, root)
	tick := time.Now().Truncate(time.Second)
	require.NoError(t, os.Chtimes(configPath, tick, tick))
	current, err := discoverResolutionInputs(abs, nil)
	require.NoError(t, err)
	require.True(t, current.stats["tsconfig.json"].racy)

	original, err := os.ReadFile(configPath)
	require.NoError(t, err)
	rewritten := []byte(strings.Replace(string(original), "old/*", "new/*", 1))
	require.Len(t, rewritten, len(original))
	require.NoError(t, os.WriteFile(configPath, rewritten, 0o644))
	require.NoError(t, os.Chtimes(configPath, tick, tick)) // same tick, same size
	caller := filepath.Join(abs, "pages", "index.ts")
	require.False(t, current.unchangedForPoints(abs, []string{caller}, nil))
}
