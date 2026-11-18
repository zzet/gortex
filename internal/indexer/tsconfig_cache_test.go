package indexer

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/gitstate"
)

// Both targets declare the same symbol, so name-only fallback cannot stand
// in for an alias-directed binding.
func aliasRebindFixture(t *testing.T, root string) string {
	t.Helper()
	configPath := filepath.Join(root, "tsconfig.json")
	writeFileMk(t, configPath, `{"compilerOptions":{"paths":{"~/*":["old/*"]}}}`)
	writeFileMk(t, filepath.Join(root, "old/auth.ts"), "export function useRedirect() { return 1 }\n")
	writeFileMk(t, filepath.Join(root, "new/auth.ts"), "export function useRedirect() { return 2 }\n")
	writeFileMk(t, filepath.Join(root, "pages/index.ts"), "import { useRedirect } from '~/auth'\nexport function run() { return useRedirect() }\n")
	return configPath
}

func TestTSConfigPathAlias_ConfigChangeOnFullReindex(t *testing.T) {
	root := t.TempDir()
	configPath := aliasRebindFixture(t, root)
	idx, _ := newSQLiteIndexer(t)
	_, err := idx.Index(root)
	require.NoError(t, err)
	require.Equal(t, "old/auth.ts::useRedirect", callTargetForName(t, idx.Graph(), "pages/index.ts", "run"))
	writeFileMk(t, configPath, `{"compilerOptions":{"paths":{"~/*":["new/*"]}}}`)
	_, err = idx.Index(root)
	require.NoError(t, err)
	require.Equal(t, "new/auth.ts::useRedirect", callTargetForName(t, idx.Graph(), "pages/index.ts", "run"), "same-root reindex must not keep the old config")
}

func TestTSConfigPathAlias_NewConfigAfterNegativeFullScan(t *testing.T) {
	root := t.TempDir()
	writeFileMk(t, filepath.Join(root, "lib/auth.ts"), "export function useRedirect() { return 1 }\n")
	writeFileMk(t, filepath.Join(root, "pages/index.ts"), "import { useRedirect } from '~/auth'\nexport function run() { return useRedirect() }\n")
	idx, _ := newSQLiteIndexer(t)
	_, err := idx.Index(root)
	require.NoError(t, err)
	require.Nil(t, loadTSAliasCollection(root), "absence of configs is cached too")
	writeFileMk(t, filepath.Join(root, "tsconfig.json"), `{"compilerOptions":{"paths":{"~/*":["lib/*"]}}}`)
	idx, _ = newSQLiteIndexer(t)
	_, err = idx.Index(root)
	require.NoError(t, err)
	require.Equal(t, "lib/auth.ts::useRedirect", callTargetForName(t, idx.Graph(), "pages/index.ts", "run"))
}

func TestTSConfigPathAlias_ConfigChangeOnIncrementalReindex(t *testing.T) {
	root := t.TempDir()
	configPath := aliasRebindFixture(t, root)
	caller := filepath.Join(root, "pages/index.ts")
	idx, _ := newSQLiteIndexer(t)
	_, err := idx.Index(root)
	require.NoError(t, err)
	require.Equal(t, "old/auth.ts::useRedirect", callTargetForName(t, idx.Graph(), "pages/index.ts", "run"))
	writeFileMk(t, configPath, `{"compilerOptions":{"paths":{"~/*":["new/*"]}}}`)
	bumpMtime(t, caller, "import { useRedirect } from '~/auth'\nexport function run() { useRedirect(); return useRedirect() }\n")
	_, err = idx.IncrementalReindexPaths(root, []string{caller})
	require.NoError(t, err)
	require.Equal(t, "new/auth.ts::useRedirect", callTargetForName(t, idx.Graph(), "pages/index.ts", "run"))
}

func TestTSConfigPathAlias_ConfigOnlyIncrementalReindex(t *testing.T) {
	root := t.TempDir()
	configPath := aliasRebindFixture(t, root)
	idx, _ := newSQLiteIndexer(t)
	_, err := idx.Index(root)
	require.NoError(t, err)
	writeFileMk(t, configPath, `{"compilerOptions":{"paths":{"~/*":["new/*"]}}}`)
	_, err = idx.IncrementalReindexPaths(root, []string{configPath})
	require.NoError(t, err)
	require.Equal(t, "new/auth.ts::useRedirect", callTargetForName(t, idx.Graph(), "pages/index.ts", "run"))
}

func TestTSConfigPathAlias_CancelledRebindKeepsGraphAndRetries(t *testing.T) {
	root := t.TempDir()
	configPath := aliasRebindFixture(t, root)
	idx, _ := newSQLiteIndexer(t)
	_, err := idx.Index(root)
	require.NoError(t, err)
	writeFileMk(t, configPath, `{"compilerOptions":{"paths":{"~/*":["new/*"]}}}`)

	budget := newIndexMemoryAdmissionBudget(1)
	held, err := budget.acquire(t.Context(), 1)
	require.NoError(t, err)
	defer held.Release()
	idx.indexMemoryAdmission = budget
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := idx.IndexCtx(ctx, root)
		done <- err
	}()
	waitForIndexMemoryWaiters(t, budget, 1)
	cancel()
	require.ErrorIs(t, <-done, context.Canceled)
	require.Equal(t, "old/auth.ts::useRedirect", callTargetForName(t, idx.Graph(), "pages/index.ts", "run"))

	// The lookup cache has already seen the new config. The graph has not.
	_, err = idx.IncrementalReindexPaths(root, []string{configPath})
	require.NoError(t, err)
	require.Equal(t, "new/auth.ts::useRedirect", callTargetForName(t, idx.Graph(), "pages/index.ts", "run"))
}

func TestTSConfigPathAlias_RebindPreservesExplicitScope(t *testing.T) {
	root := t.TempDir()
	configPath := aliasRebindFixture(t, root)
	goFile := filepath.Join(root, "keep.go")
	unrelated := filepath.Join(root, "other.go")
	writeFileMk(t, goFile, "package p\nfunc Before() {}\n")
	writeFileMk(t, unrelated, "package p\nfunc Other() {}\n")
	idx, _ := newSQLiteIndexer(t)
	idx.config.Merkle = false
	_, err := idx.Index(root)
	require.NoError(t, err)
	stamp, err := os.Stat(goFile)
	require.NoError(t, err)
	writeFileMk(t, goFile, "package p\nfunc After() {}\n")
	require.NoError(t, os.Chtimes(goFile, stamp.ModTime(), stamp.ModTime()))
	require.NoError(t, os.Remove(unrelated))
	writeFileMk(t, configPath, `{"compilerOptions":{"paths":{"~/*":["new/*"]}}}`)
	require.NoError(t, idx.IndexFile(goFile))
	require.NotEmpty(t, fnNodeID(t, idx.Graph(), "keep.go", "After"))
	require.NotEmpty(t, fnNodeID(t, idx.Graph(), "other.go", "Other"))
	require.Equal(t, "new/auth.ts::useRedirect", callTargetForName(t, idx.Graph(), "pages/index.ts", "run"))
}

func TestTSConfigPathAlias_WarmSaveKeepsBoundedProof(t *testing.T) {
	root := t.TempDir()
	configPath := aliasRebindFixture(t, root)
	idx, _ := newSQLiteIndexer(t)
	_, err := idx.Index(root)
	require.NoError(t, err)
	proof := newBuildContentProof(root, gitstate.DirtySnapshot{})
	idx.contentProof = proof
	_, err = idx.IncrementalReindexPaths(root, []string{"pages/index.ts"})
	require.NoError(t, err)
	require.False(t, proof.isUnbounded())
	set, _ := buildReadSetFrom(proof.readSet(t.Context(), nil, nil))
	require.True(t, readSetHolds(set, filepath.Base(configPath)))
}
