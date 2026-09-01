package indexer

import (
	"context"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/search"
)

// TestRunDeferredPassesRacesUntrackRepo drives the deferred receipt tail
// concurrently with repository removal through public methods only. The
// receipt-exact name frontier iterates the indexer registry to enumerate
// repo prefixes; UntrackRepo mutates that map under mi.mu, so an
// unsynchronized read is a detector-visible race and, being concurrent map
// iteration versus write, can crash the daemon outright.
func TestRunDeferredPassesRacesUntrackRepo(t *testing.T) {
	repoA := setupRepoDir(t, "repo-a")
	repoB := setupRepoDir(t, "repo-b")
	repoC := setupRepoDir(t, "repo-c")

	tmpCfg := filepath.Join(t.TempDir(), "config.yaml")
	gc := &config.GlobalConfig{
		Repos: []config.RepoEntry{
			{Path: repoA, Name: "repo-a"},
			{Path: repoB, Name: "repo-b"},
			{Path: repoC, Name: "repo-c"},
		},
	}
	gc.SetConfigPath(tmpCfg)
	require.NoError(t, gc.Save())
	cm, err := config.NewConfigManager(tmpCfg)
	require.NoError(t, err)

	g := graph.New()
	mi := NewMultiIndexer(g, newTestRegistry(), search.NewNull(), cm, zap.NewNop())
	_, err = mi.IndexAll()
	require.NoError(t, err)

	// Park a resolution-relevant eviction inside the deferred receipt
	// window so the tail's exact consumption reaches the name-frontier
	// resolve that enumerates repo prefixes from the indexer registry.
	run := mi.BeginDeferredPasses(context.Background(), nil)
	g.EvictFile("repo-a/main.go")

	done := make(chan struct{})
	go func() {
		defer close(done)
		run.FinishTailResult()
	}()
	mi.UntrackRepo("repo-b")
	// TrackRepoCtx is the sibling registry writer; interleave it so the
	// tail also races an insertion, not only removals.
	if _, err := mi.TrackRepoCtx(context.Background(), config.RepoEntry{Path: repoB, Name: "repo-b"}); err != nil {
		t.Fatalf("re-track repo-b: %v", err)
	}
	mi.UntrackRepo("repo-c")
	<-done
}

// TestBackfillWorkspaceSlugsRacesUntrackRepo covers the second reader of the
// global repo list. The workspace-slug backfill maps every configured entry to
// its prefix so a user-level workspace override survives the stamp; UntrackRepo
// removes an entry from that same slice under the config mutation mutex. The
// backfill runs on the pre-enrich resolve path, which the deferred receipt tail
// reaches concurrently with repository removal, so the read has to take the
// snapshot rather than iterate the live slice.
func TestBackfillWorkspaceSlugsRacesUntrackRepo(t *testing.T) {
	repoA := setupRepoDir(t, "repo-a")
	repoB := setupRepoDir(t, "repo-b")
	repoC := setupRepoDir(t, "repo-c")

	tmpCfg := filepath.Join(t.TempDir(), "config.yaml")
	gc := &config.GlobalConfig{
		Repos: []config.RepoEntry{
			{Path: repoA, Name: "repo-a"},
			{Path: repoB, Name: "repo-b"},
			{Path: repoC, Name: "repo-c"},
		},
	}
	gc.SetConfigPath(tmpCfg)
	require.NoError(t, gc.Save())
	cm, err := config.NewConfigManager(tmpCfg)
	require.NoError(t, err)

	g := graph.New()
	mi := NewMultiIndexer(g, newTestRegistry(), search.NewNull(), cm, zap.NewNop())
	_, err = mi.IndexAll()
	require.NoError(t, err)

	// The reader runs until the registry writes are done rather than for a
	// fixed count, so the overlap does not depend on their relative speed.
	var stop atomic.Bool
	done := make(chan struct{})
	go func() {
		defer close(done)
		for !stop.Load() {
			mi.BackfillWorkspaceSlugsWithImpact()
		}
	}()
	mi.UntrackRepo("repo-b")
	if _, err := mi.TrackRepoCtx(context.Background(), config.RepoEntry{Path: repoB, Name: "repo-b"}); err != nil {
		t.Fatalf("re-track repo-b: %v", err)
	}
	mi.UntrackRepo("repo-c")
	stop.Store(true)
	<-done
}
