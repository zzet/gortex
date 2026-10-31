package mcp

import (
	"context"
	"sort"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer"
)

// registerMCPFixtureRepos uses the daemon's registration and shared deferred
// stages. Cross-repository queries need all roots published before resolution
// and contract matching; independent TrackRepo calls do not establish that
// boundary. These fixtures start empty and have no concurrent lifecycle writer.
func registerMCPFixtureRepos(t *testing.T, mi *indexer.MultiIndexer, cm *config.ConfigManager) {
	t.Helper()
	ctx := context.Background()
	mi.BeginParallelBatch()
	defer mi.ResetBatch()
	entries := cm.ActiveRepos()
	mi.ExpectRepos(entries)
	var prefixes []string
	for _, entry := range mi.ResolveConfiguredRepos(ctx, entries) {
		mi.StartRepoLoad(entry.Path)
		result, err := mi.TrackRepoCtx(ctx, entry)
		mi.FinishRepoLoad(entry.Path, err)
		require.NoError(t, err)
		prefixes = append(prefixes, result.RepoPrefix)
	}
	require.NoError(t, mi.RunPreEnrichResolve(ctx, nil, nil))
	mi.RunDeferredPassesAllResult(ctx)
	mi.ReconcileContractEdges()
	// SQLite query fixtures also need their durable resolved-reference sidecar
	// after the shared resolution, rather than after each individual root.
	if rebuilder, ok := mi.Graph().(graph.RefFactsRebuilder); ok {
		sort.Strings(prefixes)
		require.NoError(t, rebuilder.RebuildRefFactsForRepos(prefixes))
	}
	mi.EndBatch()
}
