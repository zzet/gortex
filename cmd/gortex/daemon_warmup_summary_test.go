package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
)

// TestReconcileFileCount pins the filesReindexed arithmetic: a full retrack
// counts its whole FileCount (the incremental fields are meaningless on that
// path — see the ReconcileRepoCtx fullRetrack comment), an incremental
// reconcile counts only the files it actually touched.
func TestReconcileFileCount(t *testing.T) {
	cases := []struct {
		name string
		res  *indexer.IndexResult
		want int
	}{
		{"nil result", nil, 0},
		{
			"full retrack uses FileCount, ignores stale/deleted",
			&indexer.IndexResult{FullRetrack: true, FileCount: 42, StaleFileCount: 1, DeletedFileCount: 1},
			42,
		},
		{
			"incremental reconcile sums stale + deleted",
			&indexer.IndexResult{FullRetrack: false, FileCount: 999, StaleFileCount: 3, DeletedFileCount: 2},
			5,
		},
		{
			"incremental reconcile with no changes",
			&indexer.IndexResult{FullRetrack: false, StaleFileCount: 0, DeletedFileCount: 0},
			0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := reconcileFileCount(tc.res)
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestLogWarmupSummary asserts the one-line warmup recap is emitted exactly
// once, carrying every field the log-joining problem this feature solves
// requires: the per-phase durations, the queryable/total elapsed, and the
// repo/file/enrichment counters.
func TestLogWarmupSummary(t *testing.T) {
	core, logs := observer.New(zap.InfoLevel)
	logger := zap.New(core)

	warmup := &warmupTimings{
		parse:           1500 * time.Millisecond,
		resolve:         250 * time.Millisecond,
		globalResolve:   80 * time.Millisecond,
		endBatch:        40 * time.Millisecond,
		reposChanged:    3,
		filesReindexed:  17,
		enrichScheduled: 2,
	}
	queryable := 1900 * time.Millisecond
	total := 5200 * time.Millisecond

	logWarmupSummary(logger, warmup, queryable, total)

	entries := logs.FilterMessage("daemon: warmup summary").All()
	require.Len(t, entries, 1, "expected exactly one warmup summary line")

	fields := entries[0].ContextMap()
	assert.InDelta(t, 1.5, fields["parse_s"], 0.001)
	assert.InDelta(t, 0.25, fields["resolve_s"], 0.001)
	assert.InDelta(t, 0.08, fields["global_resolve_s"], 0.001)
	assert.InDelta(t, 0.04, fields["end_batch_s"], 0.001)
	assert.InDelta(t, 1.9, fields["queryable_s"], 0.001)
	assert.InDelta(t, 5.2, fields["total_s"], 0.001)
	assert.EqualValues(t, 3, fields["repos_changed"])
	assert.EqualValues(t, 17, fields["files_reindexed"])
	assert.EqualValues(t, 2, fields["enrich_scheduled"])
}

// TestLogWarmupSummary_NilGuards confirms the helper no-ops instead of
// panicking when called without a logger or timings — defensive, since a
// warmup path that returns early hands back a non-nil *warmupTimings but a
// caller could still pass a nil logger in a degraded startup.
func TestLogWarmupSummary_NilGuards(t *testing.T) {
	assert.NotPanics(t, func() {
		logWarmupSummary(nil, &warmupTimings{}, 0, 0)
		logWarmupSummary(zap.NewNop(), nil, 0, 0)
	})
}

// TestWarmupDaemonState_TimingsAggregation exercises warmupDaemonState
// end-to-end against the in-memory backend with two freshly-tracked repos
// (no prior snapshot, so both take the cold TrackRepoCtx path). Confirms
// reposChanged and filesReindexed reflect real per-repo file counts, not
// just "something happened".
func TestWarmupDaemonState_TimingsAggregation(t *testing.T) {
	dir := t.TempDir()

	repoA := filepath.Join(dir, "repo-a")
	require.NoError(t, os.MkdirAll(repoA, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(repoA, "main.go"),
		[]byte("package main\n\nfunc main() {}\n"), 0o644))

	repoB := filepath.Join(dir, "repo-b")
	require.NoError(t, os.MkdirAll(repoB, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(repoB, "lib.go"),
		[]byte("package lib\n\nfunc Foo() {}\n"), 0o644))

	g := graph.New()
	reg := parser.NewRegistry()
	languages.RegisterAll(reg)

	cm, err := config.NewConfigManager(filepath.Join(dir, "config.yaml"))
	require.NoError(t, err)
	require.NoError(t, cm.Global().AddRepo(config.RepoEntry{Path: repoA}))
	require.NoError(t, cm.Global().AddRepo(config.RepoEntry{Path: repoB}))

	idx := indexer.New(g, reg, config.Default().Index, zap.NewNop())
	mi := indexer.NewMultiIndexer(g, reg, idx.Search(), cm, zap.NewNop())

	state := &daemonState{
		graph:         g,
		indexer:       idx,
		multiIndexer:  mi,
		configManager: cm,
	}

	_, timings := warmupDaemonState(state, zap.NewNop(), func() {})
	require.NotNil(t, timings)

	assert.Equal(t, 2, timings.reposChanged, "both freshly-tracked repos count as changed")
	assert.Equal(t, 2, timings.filesReindexed, "one file per repo, both cold-tracked")
	assert.Equal(t, 0, timings.enrichScheduled, "no semantic providers configured")
	assert.GreaterOrEqual(t, timings.parse.Nanoseconds(), int64(0))
}

func TestWarmupDaemonStateLogsFullIndexOrigin(t *testing.T) {
	dir := t.TempDir()
	repo := filepath.Join(dir, "warmup-origin")
	require.NoError(t, os.MkdirAll(repo, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(repo, "main.go"), []byte("package main\nfunc main() {}\n"), 0o644))
	cm, err := config.NewConfigManager(filepath.Join(dir, "config.yaml"))
	require.NoError(t, err)
	require.NoError(t, cm.Global().AddRepo(config.RepoEntry{Path: repo, Name: "warmup-origin"}))
	core, logs := observer.New(zap.InfoLevel)
	logger := zap.New(core)
	g := graph.New()
	reg := parser.NewRegistry()
	reg.Register(languages.NewGoExtractor())
	idx := indexer.New(g, reg, config.Default().Index, logger)
	mi := indexer.NewMultiIndexer(g, reg, idx.Search(), cm, logger)
	state := &daemonState{graph: g, indexer: idx, multiIndexer: mi, configManager: cm}

	_, _ = warmupDaemonState(state, logger, func() {})
	entries := logs.FilterMessage("indexer: full index started").All()
	require.Len(t, entries, 1)
	fields := entries[0].ContextMap()
	assert.Equal(t, "warmup-origin", fields["repo"])
	assert.Equal(t, "daemon_warmup", fields["origin"])
	assert.Equal(t, true, fields["first_index"])
	assert.Equal(t, false, fields["shadow"])
}
