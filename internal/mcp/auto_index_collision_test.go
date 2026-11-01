package mcp

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/indexer"
)

func TestAutoIndexCollisionNoRecordImplicit(t *testing.T) {
	s, mi, catalog, _ := newCatalogMCPServer(t)
	owner, loser := filepath.Join(t.TempDir(), "src"), filepath.Join(t.TempDir(), "src")
	gitInitWorktreeRepo(t, owner)
	gitInitWorktreeRepo(t, loser)
	registered, err := s.lifecycle.Register(t.Context(), config.RepoEntry{Path: owner}, indexer.TrackSourceCLI)
	require.NoError(t, err)
	before, found, err := catalog.GetDedicatedGraph(t.Context(), registered.GraphID)
	require.NoError(t, err)
	require.True(t, found)
	configBefore := s.configManager.Global().ReposSnapshot()
	loserFamily := indexer.FamilyIDFor(filepath.Join(loser, ".git"))
	checkouts, err := catalog.ListCheckouts(t.Context(), loserFamily)
	require.NoError(t, err)
	require.Empty(t, checkouts)
	t.Chdir(loser)
	s.autoIndexCWDBackground() // synchronous entry into the real background body
	checkouts, err = catalog.ListCheckouts(t.Context(), loserFamily)
	require.NoError(t, err)
	require.Empty(t, checkouts, "a collision must not record an implicit checkout")
	after, found, err := catalog.GetDedicatedGraph(t.Context(), registered.GraphID)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, before, after)
	require.Equal(t, configBefore, s.configManager.Global().ReposSnapshot())
	require.Equal(t, owner, mi.GetMetadata("src").RootPath)
	require.Empty(t, mi.RepoForFile(loser))
}
