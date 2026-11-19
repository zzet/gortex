package indexer

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/gitstate"
)

func TestNuxtAutoImports_ConfigOnlyIncrementalReindex(t *testing.T) {
	root := t.TempDir()
	writeFileMk(t, filepath.Join(root, ".gitignore"), ".nuxt/\n")
	list := filepath.Join(root, ".nuxt/imports.d.ts")
	writeFileMk(t, list, "export { useRedirect } from '../old/auth';\n")
	writeFileMk(t, filepath.Join(root, "old/auth.ts"), "export function useRedirect() { return 1 }\n")
	unrelated := filepath.Join(root, "unrelated.go")
	writeFileMk(t, unrelated, "package unrelated\nfunc Keep() {}\n")
	writeFileMk(t, filepath.Join(root, "pages/index.ts"), "export function run() { return useRedirect() }\n")
	idx, _ := newSQLiteIndexer(t)
	_, err := idx.Index(root)
	require.NoError(t, err)
	require.Equal(t, "old/auth.ts::useRedirect", callTargetForName(t, idx.Graph(), "pages/index.ts", "run"))
	other, _ := newSQLiteIndexer(t)
	_, err = other.Index(root)
	require.NoError(t, err)
	require.NoError(t, os.Remove(unrelated))
	writeFileMk(t, filepath.Join(root, "new/auth.ts"), "export function useRedirect() { return 2 }\n")
	writeFileMk(t, list, "export { useRedirect } from '../new/auth';\n")
	_, err = other.IncrementalReindexPaths(root, []string{list})
	require.NoError(t, err)
	require.Equal(t, "new/auth.ts::useRedirect", callTargetForName(t, other.Graph(), "pages/index.ts", "run"))
	_, err = idx.IncrementalReindexPaths(root, []string{list})
	require.NoError(t, err)
	require.Equal(t, "new/auth.ts::useRedirect", callTargetForName(t, idx.Graph(), "pages/index.ts", "run"))
	require.NotNil(t, idx.Graph().GetNode("unrelated.go::Keep"), "config-only scope must not consume an unrelated deletion")
}

func TestNuxtExplicitExtension(t *testing.T) {
	root := t.TempDir()
	writeFileMk(t, filepath.Join(root, ".nuxt/imports.d.ts"), "export { useAuth } from '../lib/auth.js'")
	writeFileMk(t, filepath.Join(root, "lib/auth.ts"), "export function useAuth() { return 1 }")
	writeFileMk(t, filepath.Join(root, "lib/auth.js"), "export function useAuth() { return 2 }")
	writeFileMk(t, filepath.Join(root, "pages/index.ts"), "export function run() { return useAuth() }")
	idx, _ := newSQLiteIndexer(t)
	_, err := idx.Index(root)
	require.NoError(t, err)
	require.Equal(t, "lib/auth.js::useAuth", callTargetForName(t, idx.Graph(), "pages/index.ts", "run"))
}

func TestNuxtPointSaveMetadataProof(t *testing.T) {
	root := t.TempDir()
	caller := filepath.Join(root, "pages/index.ts")
	writeFileMk(t, caller, "export function run() { return useAuth() }")
	idx, _ := newSQLiteIndexer(t)
	_, err := idx.Index(root)
	require.NoError(t, err)

	// Preparation is ignored by many watchers: the only event can be the caller save.
	list := filepath.Join(root, ".nuxt/imports.d.ts")
	writeFileMk(t, list, "export { useAuth } from '../lib/auth'")
	writeFileMk(t, filepath.Join(root, "lib/auth.ts"), "export function useAuth() { return 1 }")
	_, err = idx.incrementalPointWatcherPath(root, caller)
	require.NoError(t, err)
	require.Equal(t, "lib/auth.ts::useAuth", callTargetForName(t, idx.Graph(), "pages/index.ts", "run"))

	idx.contentProof = newBuildContentProof(root, gitstate.DirtySnapshot{})
	idx.refreshResolutionInputs(root, caller)
	target, _ := idx.resolveNuxtAutoImport("pages/index.ts", "useAuth")
	require.Equal(t, "lib/auth", target)
	require.False(t, idx.contentProof.isUnbounded(), "unchanged point metadata must not require a full sample")
	require.Equal(t, gitstate.BlobSHA256([]byte("export { useAuth } from '../lib/auth'")), idx.contentProof.proven()[".nuxt/imports.d.ts"])
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
