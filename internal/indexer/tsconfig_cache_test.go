package indexer

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestTSConfigPathAlias_ConfigChangeOnFullReindex(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, "tsconfig.json")
	writeFileMk(t, configPath, `{"compilerOptions":{"paths":{"~/*":["old/*"]}}}`)
	writeFileMk(t, filepath.Join(root, "old/auth.ts"), "export function useRedirect() { return 1 }\n")
	writeFileMk(t, filepath.Join(root, "new/auth.ts"), "export function useRedirect() { return 2 }\n")
	writeFileMk(t, filepath.Join(root, "pages/index.ts"), "import { useRedirect } from '~/auth'\nexport function run() { return useRedirect() }\n")
	idx, _ := newSQLiteIndexer(t)
	_, err := idx.Index(root)
	require.NoError(t, err)
	require.Equal(t, "old/auth.ts::useRedirect", callTargetForName(t, idx.Graph(), "pages/index.ts", "run"))
	writeFileMk(t, configPath, `{"compilerOptions":{"paths":{"~/*":["new/*"]}}}`)
	_, err = idx.Index(root)
	require.NoError(t, err)
	require.Equal(t, "new/auth.ts::useRedirect", callTargetForName(t, idx.Graph(), "pages/index.ts", "run"), "same-root reindex must not keep the old config")
}

func TestTSConfigPathAlias_NegativeCacheOnRetrack(t *testing.T) {
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
	configPath := filepath.Join(root, "tsconfig.json")
	writeFileMk(t, configPath, `{"compilerOptions":{"paths":{"~/*":["old/*"]}}}`)
	writeFileMk(t, filepath.Join(root, "old/auth.ts"), "export function useRedirect() { return 1 }\n")
	writeFileMk(t, filepath.Join(root, "new/auth.ts"), "export function useRedirect() { return 2 }\n")
	caller := filepath.Join(root, "pages/index.ts")
	writeFileMk(t, caller, "import { useRedirect } from '~/auth'\nexport function run() { return useRedirect() }\n")
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
	configPath := filepath.Join(root, "tsconfig.json")
	writeFileMk(t, configPath, `{"compilerOptions":{"paths":{"~/*":["old/*"]}}}`)
	writeFileMk(t, filepath.Join(root, "old/auth.ts"), "export function useRedirect() { return 1 }\n")
	writeFileMk(t, filepath.Join(root, "new/auth.ts"), "export function useRedirect() { return 2 }\n")
	writeFileMk(t, filepath.Join(root, "pages/index.ts"), "import { useRedirect } from '~/auth'\nexport function run() { return useRedirect() }\n")
	idx, _ := newSQLiteIndexer(t)
	_, err := idx.Index(root)
	require.NoError(t, err)
	writeFileMk(t, configPath, `{"compilerOptions":{"paths":{"~/*":["new/*"]}}}`)
	_, err = idx.IncrementalReindexPaths(root, []string{configPath})
	require.NoError(t, err)
	require.Equal(t, "new/auth.ts::useRedirect", callTargetForName(t, idx.Graph(), "pages/index.ts", "run"))
}

