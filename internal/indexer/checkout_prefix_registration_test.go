package indexer

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/config"
)

func TestRegisterCollisionNoCatalogNoWatcherNoSave(t *testing.T) {
	f := newLifecycleFixture(t)
	t.Cleanup(f.close)
	root := f.gitRepo("src")
	indexed, err := f.mi.TrackRepoCtx(t.Context(), config.RepoEntry{Path: root, Name: "src"})
	require.NoError(t, err)
	require.NotNil(t, indexed)
	before, err := os.ReadFile(f.cfgPath)
	require.NoError(t, err)
	beforeConfig, beforeRevision := f.cm.GetRepoConfig("src"), f.cm.Revision()
	beforePrefixes := f.cm.WorkspacePrefixes()
	beforeInvalidated, beforeAnalysed := f.notify.counts()
	// Exercise a real nil track result, then withdraw its registry witness at
	// the recording boundary. A previous successful no-op is not authority to
	// recreate catalog/watcher/config state after ownership has disappeared.
	f.lc.afterRegisterTrack = func() {
		f.mi.mu.Lock()
		delete(f.mi.repos, "src")
		f.mi.mu.Unlock()
	}
	result, err := f.lc.Register(t.Context(), config.RepoEntry{Path: root, Name: "src"}, TrackSourceCLI)
	var collision *PrefixCollisionError
	require.ErrorAs(t, err, &collision)
	require.Equal(t, "src", collision.Prefix)
	require.Equal(t, "unknown", collision.OwnerState)
	require.Empty(t, collision.ExistingPath, "unknown ownership must not invent another root")
	require.Empty(t, result.Prefix)
	families, err := f.catalog.ListRepositoryFamilies(t.Context())
	require.NoError(t, err)
	require.Empty(t, families, "registration must stop before catalog allocation")
	after, err := os.ReadFile(f.cfgPath)
	require.NoError(t, err)
	require.Equal(t, before, after)
	require.Equal(t, beforeConfig, f.cm.GetRepoConfig("src"))
	require.Equal(t, beforeRevision, f.cm.Revision(), "nil track must not publish a prepared candidate config")
	require.Equal(t, beforePrefixes, f.cm.WorkspacePrefixes())
	f.watcher.mu.Lock()
	calls := append([]string(nil), f.watcher.calls...)
	f.watcher.mu.Unlock()
	require.Empty(t, calls)
	invalidated, analysed := f.notify.counts()
	require.Equal(t, beforeInvalidated, invalidated)
	require.Equal(t, beforeAnalysed, analysed)
}

func TestRecordImplicitRequiresExactRegisteredRoot(t *testing.T) {
	f := newLifecycleFixture(t)
	t.Cleanup(f.close)
	require.ErrorIs(t, f.lc.RecordImplicit(t.Context(), ""), ErrCheckoutNotTracked,
		"an empty implicit root must not silently resolve to the process cwd")
	owner := f.gitRepo("owner")
	_, err := f.lc.Register(t.Context(), config.RepoEntry{Path: owner}, TrackSourceCLI)
	require.NoError(t, err)
	child := filepath.Join(owner, "nested")
	require.NoError(t, os.MkdirAll(child, 0o700))
	require.Equal(t, "owner", f.lc.ResolvePrefix(child), "containment remains useful for reads")
	before, err := os.ReadFile(f.cfgPath)
	require.NoError(t, err)
	graphs, err := f.catalog.ListDedicatedGraphs(t.Context(), f.familyOf("owner").FamilyID)
	require.NoError(t, err)
	nodes, edges := f.store.NodeCount(), f.store.EdgeCount()
	f.watcher.mu.Lock()
	watcherCalls := append([]string(nil), f.watcher.calls...)
	f.watcher.mu.Unlock()
	invalidated, analysed := f.notify.counts()
	err = f.lc.RecordImplicit(t.Context(), child)
	var collision *PrefixCollisionError
	require.ErrorAs(t, err, &collision, "an ancestor prefix is not exact registration authority")
	require.Equal(t, "unknown", collision.OwnerState)
	require.Equal(t, nodes, f.store.NodeCount())
	require.Equal(t, edges, f.store.EdgeCount())
	afterGraphs, err := f.catalog.ListDedicatedGraphs(t.Context(), f.familyOf("owner").FamilyID)
	require.NoError(t, err)
	require.Equal(t, graphs, afterGraphs)
	after, err := os.ReadFile(f.cfgPath)
	require.NoError(t, err)
	require.Equal(t, before, after)
	f.watcher.mu.Lock()
	afterWatcherCalls := append([]string(nil), f.watcher.calls...)
	f.watcher.mu.Unlock()
	require.Equal(t, watcherCalls, afterWatcherCalls)
	afterInvalidated, afterAnalysed := f.notify.counts()
	require.Equal(t, invalidated, afterInvalidated)
	require.Equal(t, analysed, afterAnalysed)
}
