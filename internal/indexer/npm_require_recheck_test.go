package indexer

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// The require re-check covers every repository until a complete re-check
// ran against the current package snapshot, then only package providers,
// and every repository again once a manifest changes the package set.
func TestNpmRequireRecheckNarrowsOnlyForCheckedSnapshot(t *testing.T) {
	root := t.TempDir()
	writePackageJSON(t, root, `{"name":"p","main":"index.ts"}`)
	backdateTree(t, root)
	mi := &MultiIndexer{configMgr: newTestConfigManager(t), repos: map[string]*RepoMetadata{"prov": {RootPath: root}}}

	_, recheck := mi.npmPackageLookups()
	repos, done := recheck()
	require.Nil(t, repos, "no complete re-check yet: check every repository")
	require.NotNil(t, done)

	_, recheck = mi.npmPackageLookups()
	repos, _ = recheck()
	require.Nil(t, repos, "an unfinished re-check never narrows the next one")

	done()
	_, recheck = mi.npmPackageLookups()
	repos, _ = recheck()
	require.Equal(t, []string{"prov"}, repos)

	writePackageJSON(t, filepath.Join(root, "packages", "q"), `{"name":"q","main":"index.ts"}`)
	writePackageJSON(t, root, `{"name":"p","main":"index.ts","workspaces":["packages/*"]}`)
	_, recheck = mi.npmPackageLookups()
	repos, _ = recheck()
	require.Nil(t, repos, "a changed package set checks every repository again")
}
