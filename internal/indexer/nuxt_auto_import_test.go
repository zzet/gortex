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
