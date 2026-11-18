package indexer

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/excludes"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/thirdparty/fswatcher"
)

// A MultiWatcher outlives IndexRepo replacement, and each Indexer caches its
// own per-folder ignore rules. The rules the watcher checks and forgets must
// be the registered Indexer's: forgetting only the retired construction-time
// Indexer's cache leaves the current Indexer's walks (the folder rescan an
// ignore-file edit starts among them) on the stale rules.
func TestWatcherForgetsIgnoreFileOnReplacementIndexer(t *testing.T) {
	root := t.TempDir()
	ignore := filepath.Join(root, "sub", ".gortexignore")
	writeExcludeFixture(t, ignore, "")
	generated := filepath.Join(root, "sub", "generated", "x.go")
	writeExcludeFixture(t, generated, "package generated\n")

	const prefix = "repo"
	retired := newAdmissionTestIndexer(t, root)
	multi := NewMultiIndexer(graph.New(), newTestRegistry(), nil, nil, zap.NewNop())
	multi.repos[prefix] = &RepoMetadata{RepoPrefix: prefix, RootPath: root}
	multi.indexers[prefix] = retired
	mw, err := NewMultiWatcher(multi, map[string]config.WatchConfig{
		prefix: {DebounceMs: 10, Exclude: excludes.Builtin},
	}, zap.NewNop())
	require.NoError(t, err)
	w := mw.watchers[prefix]
	require.NotNil(t, w)
	t.Cleanup(func() { w.signalStopped(); require.NoError(t, w.Stop()) })
	require.False(t, w.isExcluded(generated))

	current := newAdmissionTestIndexer(t, root)
	current.attachRepositoryMutationCoordinator(multi.repositoryMutationCoordinator(prefix))
	multi.mu.Lock()
	multi.indexers[prefix] = current
	multi.mu.Unlock()
	// The replacement's own walk has cached the folder's rules.
	require.False(t, current.shouldExclude(generated, root, false))

	scans := make(chan map[string]struct{}, 2)
	w.scanFn = func(dirs map[string]struct{}) { scans <- dirs }
	writeExcludeFixture(t, ignore, "generated/\n")
	w.handleEvent(fswatcher.WatchEvent{Path: ignore, Types: []fswatcher.EventType{fswatcher.EventMod}})
	w.asyncWork.Wait()
	assert.True(t, current.shouldExclude(generated, root, false),
		"the registered Indexer's walk must see the new rule")
	assert.True(t, w.isExcluded(generated), "the watcher checks the registered Indexer's rules")
	require.Len(t, scans, 1)
	assert.Equal(t, map[string]struct{}{filepath.Join(root, "sub"): {}}, <-scans)

	writeExcludeFixture(t, ignore, "")
	w.handleEvent(fswatcher.WatchEvent{Path: ignore, Types: []fswatcher.EventType{fswatcher.EventMod}})
	w.asyncWork.Wait()
	assert.False(t, current.shouldExclude(generated, root, false),
		"removing the rule must re-include the file for the registered Indexer's rescan")
	assert.False(t, w.isExcluded(generated))
	require.Len(t, scans, 1)
	<-scans

	// A repository dropped from the registry falls back to the
	// construction-time Indexer instead of failing the check.
	multi.mu.Lock()
	delete(multi.indexers, prefix)
	multi.mu.Unlock()
	assert.False(t, w.isExcluded(generated))
	w.handleEvent(fswatcher.WatchEvent{Path: ignore, Types: []fswatcher.EventType{fswatcher.EventMod}})
	w.asyncWork.Wait()
}
