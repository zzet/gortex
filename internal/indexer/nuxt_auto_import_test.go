package indexer

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNuxtAutoImports_ConfigOnlyIncrementalReindex(t *testing.T) {
	root := t.TempDir()
	writeFileMk(t, filepath.Join(root, ".gitignore"), ".nuxt/\n")
	list := filepath.Join(root, ".nuxt/imports.d.ts")
	writeFileMk(t, list, "export { useRedirect } from '../old/auth';\n")
	writeFileMk(t, filepath.Join(root, "old/auth.ts"), "export function useRedirect() { return 1 }\n")
	writeFileMk(t, filepath.Join(root, "new/auth.ts"), "export function useRedirect() { return 2 }\n")
	writeFileMk(t, filepath.Join(root, "pages/index.ts"), "export function run() { return useRedirect() }\n")
	idx, _ := newSQLiteIndexer(t)
	_, err := idx.Index(root)
	require.NoError(t, err)
	require.Equal(t, "old/auth.ts::useRedirect", callTargetForName(t, idx.Graph(), "pages/index.ts", "run"))
	writeFileMk(t, list, "export { useRedirect } from '../new/auth';\n")
	_, err = idx.IncrementalReindexPaths(root, []string{list})
	require.NoError(t, err)
	require.Equal(t, "new/auth.ts::useRedirect", callTargetForName(t, idx.Graph(), "pages/index.ts", "run"))
}

func TestNuxtAutoImports_UseOnlyInstalledSnapshot(t *testing.T) {
	checkout, snapshot := t.TempDir(), t.TempDir()
	writeFileMk(t, filepath.Join(checkout, ".nuxt/imports.d.ts"), "export { useAuth } from '../live/auth';\n")
	writeFileMk(t, filepath.Join(snapshot, ".nuxt/imports.d.ts"), "export { useAuth } from '../committed/auth';\n")
	idx := npmIndexer(t, checkout, snapshotOf(t, snapshot))
	stem, name := idx.resolveNuxtAutoImport("pages/index.ts", "useAuth")
	require.Equal(t, "committed/auth", stem)
	require.Equal(t, "useAuth", name)
	idx.SetContentSource(snapshotOf(t, t.TempDir()))
	stem, _ = idx.resolveNuxtAutoImport("pages/index.ts", "useAuth")
	require.Empty(t, stem, "missing generated metadata must not fall back to the working copy or previous snapshot")
}
