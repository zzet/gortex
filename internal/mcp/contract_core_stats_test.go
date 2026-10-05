package mcp

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/query"
)

type contractCoreStatsStore struct {
	*store_sqlite.Store
	checked, legacy, repoScans, counterReads int
}

func (s *contractCoreStatsStore) StatsContext(ctx context.Context) (graph.GraphStats, error) {
	s.checked++
	return s.Store.StatsContext(ctx)
}
func (s *contractCoreStatsStore) Stats() graph.GraphStats {
	s.legacy++
	return s.Store.Stats()
}
func (s *contractCoreStatsStore) RepoStats() map[string]graph.GraphStats {
	s.repoScans++
	return s.Store.RepoStats()
}
func (s *contractCoreStatsStore) AllRepoMemoryEstimates() map[string]graph.RepoMemoryEstimate {
	s.counterReads++
	return s.Store.AllRepoMemoryEstimates()
}

func TestContractCoreStatsSelectedStoreRetainsCapabilities(t *testing.T) {
	store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "stats.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	store.AddBatch([]*graph.Node{
		{ID: "repo/a.go::A", Kind: graph.KindFunction, FilePath: "repo/a.go", RepoPrefix: "repo"},
		{ID: "repo/b.go::B", Kind: graph.KindFunction, FilePath: "repo/b.go", RepoPrefix: "repo"},
		{ID: "repo/route", Kind: graph.KindContract, RepoPrefix: "repo"},
		{ID: "untracked/f.go::F", Kind: graph.KindFunction, RepoPrefix: "untracked"},
	}, []*graph.Edge{
		{From: "repo/a.go::A", To: "repo/b.go::B", Kind: graph.EdgeCalls, Line: 7, Meta: map[string]any{"evidence": "core"}},
		{From: "repo/a.go::A", To: "repo/route", Kind: graph.EdgeProvides},
	})
	// Counter parity is with the exact selected reader, including its existing
	// treatment of absent repo counters, rather than a newly scanned corpus.
	require.NoError(t, store.SetRepoIndexState(graph.RepoIndexState{RepoPrefix: "repo", NodeCount: 12, EdgeCount: 8}))
	selected := &contractCoreStatsStore{Store: store}
	srv := &Server{graph: selected, engine: query.NewEngine(selected)}
	ctx := withContractCoreReadErrors(t.Context())
	wantStats, err := srv.engineFor(ctx).StatsContext(ctx)
	require.NoError(t, err)
	wantTotals := perRepoTotals(srv.readerFor(ctx))
	require.Equal(t, map[string]repoTotal{"repo": {nodes: 12, edges: 8}}, wantTotals)
	installContractCoreKindTestRuntime(t, srv)
	reader := srv.readerFor(ctx)
	gotStats, err := srv.engineFor(ctx).StatsContext(ctx)
	require.NoError(t, err)
	require.Equal(t, wantStats, gotStats)
	require.Equal(t, wantTotals, perRepoTotals(reader))
	require.Equal(t, 2, selected.checked)
	require.Zero(t, selected.legacy)
	require.Zero(t, selected.repoScans, "runtime core must retain the selected counter path")
	require.Equal(t, 2, selected.counterReads)
	require.Implements(t, (*graph.FilteredContainingNameReader)(nil), reader)
	require.Implements(t, (*graph.BoundedFileNodeReader)(nil), reader)
	require.Implements(t, (*graph.ScopedNodeProjectionSequencer)(nil), reader)
	require.Implements(t, (*graph.ScopedKindSummarySequencer)(nil), reader)
	csr, ok := reader.(interface {
		centralityNodePresence() graph.NodePresenceByIDsReader
	})
	require.True(t, ok)
	require.NotNil(t, csr.centralityNodePresence())
	// Stats forwarding must not expose raw contract adjacency.
	rows := reader.GetOutEdges("repo/a.go::A")
	require.Len(t, rows, 1)
	require.Equal(t, "repo/b.go::B", rows[0].To)
	require.Equal(t, "core", rows[0].Meta["evidence"])
	require.NoError(t, contractCoreReadError(ctx))
}

type contractCoreStatsLegacy struct {
	graph.Reader
	stats  graph.GraphStats
	calls  int
	cancel context.CancelFunc
}

func (s *contractCoreStatsLegacy) Stats() graph.GraphStats {
	s.calls++
	if s.cancel != nil {
		s.cancel()
	}
	return s.stats
}

type contractCoreStatsChecked struct {
	*contractCoreStatsLegacy
	checked int
	err     error
}

func (s *contractCoreStatsChecked) StatsContext(context.Context) (graph.GraphStats, error) {
	s.checked++
	if s.cancel != nil {
		s.cancel()
	}
	return s.stats, s.err
}

func TestContractCoreStatsErrorsCancellationAndLegacyFallback(t *testing.T) {
	sentinel := errors.New("selected stats read failed")
	for _, name := range []string{"checked", "legacy", "error", "canceled-before", "checked-canceled-during", "legacy-canceled-during"} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			legacy := &contractCoreStatsLegacy{Reader: graph.New(), stats: graph.GraphStats{TotalNodes: 17}}
			checked := &contractCoreStatsChecked{contractCoreStatsLegacy: legacy}
			var selected graph.Reader = checked
			var wantErr error
			switch name {
			case "legacy":
				selected = legacy
			case "error":
				checked.err, wantErr = sentinel, sentinel
			case "canceled-before":
				cancel()
				wantErr = context.Canceled
			case "checked-canceled-during":
				legacy.cancel, wantErr = cancel, context.Canceled
			case "legacy-canceled-during":
				selected, legacy.cancel, wantErr = legacy, cancel, context.Canceled
			}
			reader := newContractCoreEdges(selected, ctx, nil).(interface {
				StatsContext(context.Context) (graph.GraphStats, error)
			})
			stats, err := reader.StatsContext(ctx)
			if wantErr != nil {
				require.ErrorIs(t, err, wantErr)
				require.Equal(t, graph.GraphStats{}, stats, "partial or post-cancellation statistics must be discarded")
			} else {
				require.NoError(t, err)
				require.Equal(t, legacy.stats, stats)
			}
			if name == "canceled-before" {
				require.Zero(t, checked.checked)
				require.Zero(t, legacy.calls)
			} else if selected == legacy {
				require.Equal(t, 1, legacy.calls)
				require.Zero(t, checked.checked)
			} else {
				require.Equal(t, 1, checked.checked)
				require.Zero(t, legacy.calls, "checked errors must never retry legacy Stats")
			}
		})
	}
}

func TestContractCoreStatsSelectedOverlayRetainsFallback(t *testing.T) {
	base := graph.New()
	base.AddBatch([]*graph.Node{
		{ID: "repo/a.go::Old", Kind: graph.KindFunction, FilePath: "repo/a.go", RepoPrefix: "repo"},
		{ID: "repo/deleted.go::Gone", Kind: graph.KindFunction, FilePath: "repo/deleted.go", RepoPrefix: "repo"},
		{ID: "foreign/f.go::F", Kind: graph.KindFunction, FilePath: "foreign/f.go", RepoPrefix: "foreign"},
	}, nil)
	layer := graph.NewOverlayLayer()
	layer.MarkFile("repo/a.go", false)
	layer.MarkFile("repo/deleted.go", true)
	layer.AddNode("repo/a.go", &graph.Node{ID: "repo/a.go::New", Kind: graph.KindType, FilePath: "repo/a.go", RepoPrefix: "repo"})
	selected := graph.NewOverlaidView(base, layer)
	reader := newContractCoreEdges(selected, t.Context(), nil)
	hook := reader.(interface {
		contractCoreRepoMemoryEstimates() (map[string]graph.RepoMemoryEstimate, bool)
	})
	_, supported := hook.contractCoreRepoMemoryEstimates()
	require.False(t, supported, "composed readers must not inherit underlying counters")
	require.Equal(t, perRepoTotals(selected), perRepoTotals(reader))
	require.Equal(t, map[string]repoTotal{"repo": {nodes: 1}, "foreign": {nodes: 1}}, perRepoTotals(reader))
	want, err := selected.StatsContext(t.Context())
	require.NoError(t, err)
	got, err := query.NewEngine(reader).StatsContext(t.Context())
	require.NoError(t, err)
	require.Equal(t, want, *got)
	// Hiding optional capabilities on the selected reader keeps the same
	// legacy fallback instead of unwrapping its overlay to the base graph.
	hidden := struct{ graph.Reader }{selected}
	require.Equal(t, perRepoTotals(selected), perRepoTotals(newContractCoreEdges(hidden, t.Context(), nil)))
	stats, err := query.NewEngine(newContractCoreEdges(hidden, t.Context(), nil)).StatsContext(t.Context())
	require.NoError(t, err)
	require.Equal(t, selected.Stats(), *stats)
}

type contractCoreEmptyCounters struct{ graph.Reader }

func (*contractCoreEmptyCounters) AllRepoMemoryEstimates() map[string]graph.RepoMemoryEstimate {
	return nil
}

func TestContractCoreStatsEmptyCountersDoNotScan(t *testing.T) {
	base := graph.New()
	base.AddBatch([]*graph.Node{{ID: "repo/a.go::A", Kind: graph.KindFunction, RepoPrefix: "repo"}}, nil)
	selected := &contractCoreEmptyCounters{Reader: base}
	require.NotEmpty(t, perRepoTotals(struct{ graph.Reader }{base}))
	require.Empty(t, perRepoTotals(selected))
	require.Equal(t, perRepoTotals(selected), perRepoTotals(newContractCoreEdges(selected, t.Context(), nil)), "supported empty counters must not silently fall back to corpus totals")
}
