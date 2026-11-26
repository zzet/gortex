package indexer

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

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

// A repository with no JS/TS file never walks for alias configs: not on the
// full index, not on a file save, and not on a whole-root rescan.
func TestTSAliasDiscovery_GoOnlyRepositorySkipsWalk(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "main.go")
	writeFileMk(t, file, "package main\nfunc main() {}\n")
	idx, store := newSQLiteIndexer(t)
	_, err := idx.Index(root)
	require.NoError(t, err)

	bumpMtime(t, file, "package main\nfunc main() { helper() }\nfunc helper() {}\n")
	_, err = idx.IncrementalReindexPaths(root, []string{file})
	require.NoError(t, err)
	_, err = idx.IncrementalReindexPaths(root, nil)
	require.NoError(t, err)

	require.Zero(t, aliasDiscoveryWalkCount(absRootForTest(t, root)))
	digest, err := store.TSAliasDigest(idx.repoPrefix)
	require.NoError(t, err)
	require.Empty(t, digest, "nothing consumed an alias, so nothing is certified")
}

// The first JS/TS files in a Go repository still get alias-directed bindings
// and a certified policy.
func TestTSAliasDiscovery_FirstJSFilesInGoRepository(t *testing.T) {
	root := t.TempDir()
	writeFileMk(t, filepath.Join(root, "main.go"), "package main\nfunc main() {}\n")
	idx, store := newSQLiteIndexer(t)
	_, err := idx.Index(root)
	require.NoError(t, err)
	require.Zero(t, aliasDiscoveryWalkCount(absRootForTest(t, root)))

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

// A certified policy records its config inputs beside the digest.
func TestTSAliasDiscovery_CertifiedDigestRecordsInputs(t *testing.T) {
	root := t.TempDir()
	aliasRebindFixture(t, root)
	idx, store := newSQLiteIndexer(t)
	_, err := idx.Index(root)
	require.NoError(t, err)
	require.Equal(t, int64(1), aliasDiscoveryWalkCount(absRootForTest(t, root)))
	inputs, err := store.TSAliasInputs(idx.repoPrefix)
	require.NoError(t, err)
	require.Contains(t, inputs, "tsconfig.json")
	require.NotEmpty(t, inputs["tsconfig.json"])
}

// A failed full resolve no longer fails an index whose alias policy is
// already certified; it only withholds certification of a changed policy.
func TestTSConfigPathAlias_FailedFullResolveWithUnchangedPolicyCompletes(t *testing.T) {
	root := t.TempDir()
	aliasRebindFixture(t, root)
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
	require.Equal(t, certified, digest)
}

// A clean warm restart rechecks the recorded config inputs instead of
// walking the checkout, and the next save reuses the restored scopes.
func TestTSConfigPathAlias_CleanWarmRestartDoesNotWalk(t *testing.T) {
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
	start := func() *MultiIndexer {
		invalidateTSAliasCollection(absRootForTest(t, root))
		return NewMultiIndexer(store, idx.registry, search.NewNull(), cm, zap.NewNop())
	}
	first := start()
	_, err = first.TrackRepoCtx(t.Context(), entry)
	require.NoError(t, err)
	mtimes := first.FileMtimes("repo")

	abs := absRootForTest(t, root)
	walks := aliasDiscoveryWalkCount(abs)
	second := start()
	result, err := second.ReconcileRepoCtx(t.Context(), entry, mtimes)
	require.NoError(t, err)
	require.Zero(t, result.StaleFileCount)
	require.Equal(t, walks, aliasDiscoveryWalkCount(abs), "a clean restart must not walk the checkout")

	caller := filepath.Join(root, "pages/index.ts")
	bumpMtime(t, caller, "import { useRedirect } from '~/auth'\nexport function run() { useRedirect(); return useRedirect() }\n")
	_, err = second.GetIndexer("repo").IncrementalReindexPaths(root, []string{caller})
	require.NoError(t, err)
	require.Equal(t, walks, aliasDiscoveryWalkCount(abs), "the first save after a restart reuses the restored scopes")
	require.Equal(t, "repo/old/auth.ts::useRedirect", callTargetForName(t, store, "repo/pages/index.ts", "run"))
}

// Recorded inputs that no longer match fall back to the walk.
func TestTSConfigPathAlias_RestoreRejectsChangedInputs(t *testing.T) {
	root := t.TempDir()
	configPath := aliasRebindFixture(t, root)
	abs := absRootForTest(t, root)
	invalidateTSAliasCollection(abs)
	s := workingAliases(abs, nil, []string{abs})
	inputs := s.relInputs(abs)
	invalidateTSAliasCollection(abs)
	require.NotNil(t, restoreAliases(abs, s.digest, inputs))

	invalidateTSAliasCollection(abs)
	require.NoError(t, os.WriteFile(configPath, []byte(`{"compilerOptions":{"paths":{"~/*":["new/*"]}}}`), 0o644))
	require.Nil(t, restoreAliases(abs, s.digest, inputs))
	require.Nil(t, restoreAliases(abs, "other-digest", s.relInputs(abs)))
}
