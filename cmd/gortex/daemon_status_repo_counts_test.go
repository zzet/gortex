package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/daemon"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"github.com/zzet/gortex/internal/search"
)

// `gortex daemon status` per-repo counts — the regression surface for issues
// #261/#270, where a tracked repo rendered a near-empty row.
//
// Every one of those reports had the same root cause: the repo's nodes were
// minted with repo_prefix="" (single-repo mode) while its status row was
// looked up by its real prefix. Both backends make that bucket invisible to
// such a lookup — the in-memory shard counters skip empty-prefix nodes by
// design, and the SQLite GROUP BY excludes repo_prefix='' rows — so status
// fell back to a frozen RepoMetadata.NodeCount and disagreed with
// `gortex query stats` over the same graph.
//
// That class is now structurally impossible: every repo is prefixed, so a
// tracked repo always has a live per-prefix bucket. These tests pin the
// resulting invariants instead of reproducing the old desync.

// A tracked repo's row must track the LIVE graph, not a metadata snapshot
// frozen by whatever last replaced mi.repos[prefix]. This is the part of
// #261 that was never about prefixes: enrichment and watcher writes land
// nodes without re-stamping the metadata.
func TestStatus_TrackedRepoRowFollowsTheLiveGraph(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "lib.go"),
		[]byte("package lib\n\nfunc A() {}\nfunc B() { A() }\n"), 0o644))

	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	gc := &config.GlobalConfig{Repos: []config.RepoEntry{{Path: dir}}}
	gc.SetConfigPath(cfgPath)
	require.NoError(t, gc.Save())
	cm, err := config.NewConfigManager(cfgPath)
	require.NoError(t, err)

	g := graph.New()
	reg := parser.NewRegistry()
	languages.RegisterAll(reg)
	mi := indexer.NewMultiIndexer(g, reg, search.NewNull(), cm, zap.NewNop())
	_, err = mi.TrackRepoCtx(context.Background(), config.RepoEntry{Path: dir})
	require.NoError(t, err)

	metas := mi.AllMetadata()
	require.Len(t, metas, 1, "exactly one repo is configured")
	var prefix string
	var frozenNodes int
	for p, m := range metas {
		prefix = p
		frozenNodes = m.NodeCount
	}
	require.NotEmpty(t, prefix, "even a lone repo carries a real prefix")

	// The lone repo's nodes are in its own bucket — the condition that used
	// to be false, and that every #261/#270 report hinged on.
	require.Positive(t, g.RepoMemoryEstimate(prefix).NodeCount,
		"a lone repo must have a populated per-prefix bucket")

	// Simulate the live graph growing out from under the frozen metadata
	// snapshot — an enrichment or watcher write that lands nodes without
	// going through a path that re-stamps mi.repos[prefix].
	g.AddNode(&graph.Node{
		ID:         prefix + "/extra.go::Extra",
		Kind:       graph.KindFunction,
		Name:       "Extra",
		FilePath:   prefix + "/extra.go",
		RepoPrefix: prefix,
		Language:   "go",
	})

	c := &realController{graph: g, multiIndexer: mi, configManager: cm, logger: zap.NewNop()}
	warmingResp, err := c.Status(context.Background())
	require.NoError(t, err)
	require.Len(t, warmingResp.TrackedRepos, 1)
	assert.Equal(t, frozenNodes, warmingResp.TrackedRepos[0].Nodes,
		"warming status must retain metadata counts instead of overwriting them with unavailable exact totals")

	c.enriched.Store(true)
	resp, err := c.Status(context.Background())
	require.NoError(t, err)
	require.Len(t, resp.TrackedRepos, 1)

	liveNodes := g.NodeCount()
	require.Greater(t, liveNodes, frozenNodes,
		"sanity: the live graph must have outgrown the frozen metadata for this test to mean anything")
	assert.Equal(t, prefix, resp.TrackedRepos[0].Prefix)
	assert.Equal(t, liveNodes, resp.TrackedRepos[0].Nodes,
		"a sole tracked repo owns the whole store, so its row must agree with `gortex query stats`")
}

// With two repos, each row reports its own live per-prefix bucket and the
// rows together account for the store. Before the flip, a repo indexed while
// it was alone kept repo_prefix="" nodes after a second repo joined, so one
// row went near-empty and status had to attribute an "unaccounted pool" to
// guess its size back.
func TestStatus_TwoReposEachReportTheirOwnBucket(t *testing.T) {
	dirA := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dirA, "a.go"),
		[]byte("package a\n\nfunc A() {}\nfunc A2() { A() }\n"), 0o644))
	dirB := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dirB, "b.go"),
		[]byte("package b\n\nfunc B() {}\nfunc B2() { B() }\nfunc B3() { B2() }\n"), 0o644))

	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	gc := &config.GlobalConfig{Repos: []config.RepoEntry{{Path: dirA}}}
	gc.SetConfigPath(cfgPath)
	require.NoError(t, gc.Save())
	cm, err := config.NewConfigManager(cfgPath)
	require.NoError(t, err)

	reg := parser.NewRegistry()
	languages.RegisterAll(reg)

	// A is indexed while it is the only configured repo.
	g := graph.New()
	mi1 := indexer.NewMultiIndexer(g, reg, search.NewNull(), cm, zap.NewNop())
	_, err = mi1.TrackRepoCtx(context.Background(), config.RepoEntry{Path: dirA})
	require.NoError(t, err)
	var priorA map[string]int64
	for _, m := range mi1.AllMetadata() {
		priorA = m.FileMtimes
	}
	require.NotEmpty(t, priorA, "the first index must have recorded mtimes to reconcile against")

	// B is added while the daemon is "down", then a warm restart reconciles
	// A from its persisted mtimes and tracks B fresh.
	require.NoError(t, cm.Global().AddRepo(config.RepoEntry{Path: dirB}))
	mi2 := indexer.NewMultiIndexer(g, reg, search.NewNull(), cm, zap.NewNop())
	_, err = mi2.ReconcileRepoCtx(context.Background(), config.RepoEntry{Path: dirA}, priorA)
	require.NoError(t, err)
	_, err = mi2.TrackRepoCtx(context.Background(), config.RepoEntry{Path: dirB})
	require.NoError(t, err)

	metas := mi2.AllMetadata()
	require.Len(t, metas, 2)
	var prefixA, prefixB string
	for p, m := range metas {
		if m.RootPath == dirA {
			prefixA = p
		} else {
			prefixB = p
		}
	}
	require.NotEmpty(t, prefixA)
	require.NotEmpty(t, prefixB)

	// Both buckets are populated. A surviving a solo index then a warm
	// reconcile is exactly the case that used to leave A's bucket empty.
	bucketA := g.RepoMemoryEstimate(prefixA).NodeCount
	bucketB := g.RepoMemoryEstimate(prefixB).NodeCount
	require.Positive(t, bucketA, "A's per-prefix bucket must be populated after a warm reconcile")
	require.Positive(t, bucketB, "B's per-prefix bucket must be populated")

	c := &realController{graph: g, multiIndexer: mi2, configManager: cm, logger: zap.NewNop()}
	c.enriched.Store(true)
	resp, err := c.Status(context.Background())
	require.NoError(t, err)
	require.Len(t, resp.TrackedRepos, 2)

	rows := map[string]daemon.TrackedRepoStatus{}
	for _, r := range resp.TrackedRepos {
		rows[r.Prefix] = r
	}
	assert.Equal(t, bucketA, rows[prefixA].Nodes, "A's row reports its own live bucket")
	assert.Equal(t, bucketB, rows[prefixB].Nodes, "B's row reports its own live bucket")
	assert.Equal(t, g.NodeCount(), rows[prefixA].Nodes+rows[prefixB].Nodes,
		"with every repo prefixed, the rows account for the whole store exactly — no unattributed pool")
}
