package indexer

import (
	"github.com/zzet/gortex/internal/graph"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Given a cached npm/pnpm workspace, when new resolver passes retry imports,
// then unchanged manifests cost no reads; manifest, member and config changes
// rebuild once and produce the same targets as a cold lookup.
func TestNpmPackageLookupCachesManifestWork(t *testing.T) {
	for _, mode := range []string{"npm", "pnpm"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			manifest := `{"name":"root","workspaces":["packages/*"]}`
			if mode == "pnpm" {
				manifest = `{"name":"root"}`
				writeWorkspaceFile(t, root, "pnpm-workspace.yaml", "packages:\n  - 'packages/*'\n")
			}
			writePackageJSON(t, root, manifest)
			writePackageJSON(t, filepath.Join(root, "packages", "a"), `{"name":"p","main":"index.ts"}`)
			require.NoError(t, os.MkdirAll(filepath.Join(root, "packages", "empty"), 0o755))
			backdateTree(t, root)
			cm := newTestConfigManager(t)
			mi := &MultiIndexer{configMgr: cm, repos: map[string]*RepoMetadata{"lib": {RootPath: root}}}
			original := readDiskFile
			reads := 0
			readDiskFile = func(file string) ([]byte, bool) { reads++; return original(file) }
			t.Cleanup(func() { readDiskFile = original })
			first := mi.npmPackageLookup()
			require.Contains(t, first("p"), "lib/packages/a")
			require.Positive(t, reads)
			reads = 0
			for range 3 {
				assert.Equal(t, first("p"), mi.npmPackageLookup()("p"))
			}
			assert.Zero(t, reads, "unchanged resolver passes must reuse manifest/workspace evidence")
			writePackageJSON(t, filepath.Join(root, "packages", "a"), `{"name":"renamed","main":"other.ts"}`)
			fresh := mi.npmPackageLookup()
			assert.Empty(t, fresh("p"))
			require.Contains(t, fresh("renamed"), "lib/packages/a")
			require.Positive(t, reads, "a member-manifest edit must rebuild")
			assert.Contains(t, first("p"), "lib/packages/a", "an active pass keeps immutable evidence")
			for _, dir := range []string{"empty", "new"} {
				reads = 0
				writePackageJSON(t, filepath.Join(root, "packages", dir), `{"name":"added","main":"index.ts"}`)
				lookup := mi.npmPackageLookup()
				cold := (&MultiIndexer{configMgr: cm, repos: mi.repos}).npmPackageLookup()
				assert.Equal(t, cold("added"), lookup("added"), "membership changes must match cold")
				assert.Contains(t, lookup("added"), "lib/packages/"+dir)
				require.Positive(t, reads)
			}
			reads = 0
			cm.LoadWorkspaceConfig("lib", root)
			require.NotEmpty(t, mi.npmPackageLookup()("added"))
			require.Positive(t, reads, "config revisions rebuild workspace placement")
			mi.repos = map[string]*RepoMetadata{}
			assert.Nil(t, mi.npmPackageLookup(), "removed roots cannot retain claims")
		})
	}
}

// backdateTree moves every modification time under root an hour into the
// past, as in a workspace nobody edited just now. Stamps taken within
// manifestRacyWindow of a change cannot prove a file unchanged, so a fixture
// written a moment ago would be read again on every pass.
func backdateTree(t *testing.T, root string) {
	t.Helper()
	old := time.Now().Add(-time.Hour)
	require.NoError(t, filepath.WalkDir(root, func(path string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		return os.Chtimes(path, old, old)
	}))
}

// Given cached package evidence, when an independent input changes without
// changing the others, then a new pass matches cold and the old pass stays fixed.
func TestNpmPackageSnapshotIndependentInvalidation(t *testing.T) {
	for _, mode := range []string{"root_manifest", "pnpm_manifest", "one_root_removed", "workspace_id", "root_relocation"} {
		t.Run(mode, func(t *testing.T) {
			root, other := t.TempDir(), t.TempDir()
			writePackageJSON(t, root, `{"name":"p","main":"index.ts"}`)
			writePackageJSON(t, other, `{"name":"q","main":"index.ts"}`)
			if mode == "pnpm_manifest" {
				writePackageJSON(t, root, `{"name":"root"}`)
				writeWorkspaceFile(t, root, "pnpm-workspace.yaml", "packages:\n  - 'packages/*'\n")
				writePackageJSON(t, filepath.Join(root, "packages", "a"), `{"name":"p","main":"index.ts"}`)
				writePackageJSON(t, filepath.Join(root, "other", "b"), `{"name":"p","main":"next.ts"}`)
			}
			cm := newTestConfigManager(t)
			idx := newTestIndexer(graph.New())
			t.Cleanup(idx.Close)
			idx.SetWorkspaceID("before")
			mi := &MultiIndexer{configMgr: cm, repos: map[string]*RepoMetadata{"lib": {RootPath: root}, "other": {RootPath: other}}, indexers: map[string]*Indexer{"lib": idx}}
			first := mi.npmPackageLookup()
			before := first("p")
			require.NotEmpty(t, before)
			firstQ := first("q")
			require.NotEmpty(t, firstQ)
			revision := cm.Revision()
			switch mode {
			case "root_manifest":
				require.NoError(t, os.WriteFile(filepath.Join(root, "package.json"), []byte(`{"name":"p","exports":"./changed.ts"}`), 0o600))
			case "pnpm_manifest":
				require.NoError(t, os.WriteFile(filepath.Join(root, "pnpm-workspace.yaml"), []byte("packages:\n  - 'other/*'\n"), 0o600))
			case "one_root_removed":
				delete(mi.repos, "other")
			case "workspace_id":
				idx.SetWorkspaceID("after")
			case "root_relocation":
				mi.repos["lib"].RootPath = other
			}
			require.Equal(t, revision, cm.Revision(), "each input must invalidate independently of config revision")
			fresh := mi.npmPackageLookup()
			cold := (&MultiIndexer{configMgr: cm, repos: mi.repos, indexers: mi.indexers}).npmPackageLookup()
			assert.Equal(t, cold("p"), fresh("p"))
			assert.Equal(t, cold("q"), fresh("q"))
			assert.Equal(t, before, first("p"), "an active pass is immutable")
			assert.Equal(t, firstQ, first("q"))
			if mode == "one_root_removed" {
				assert.Empty(t, fresh("q"))
				require.NotEmpty(t, fresh("p"))
			} else {
				assert.NotEqual(t, before, fresh("p"))
			}
		})
	}
}
