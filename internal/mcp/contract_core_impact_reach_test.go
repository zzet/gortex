package mcp

import (
	"context"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/analysis"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/query"
	"github.com/zzet/gortex/internal/reach"
)

const (
	impactReachSeed   = "repo/a.go::Seed"
	impactReachDepth1 = "repo/b.go::Direct"
	impactReachDepth2 = "repo/c.go::Middle"
	impactReachDepth3 = "repo/d.go::Outer"
)

// impactReachHidden are the sources whose only edges into the chain are ones
// the contract-core wrapper hides.
var impactReachHidden = []string{
	"repo/routes.go::Router",
	"repo/bean.go::BeanUser",
	"repo/bean.go::BeanMiddle",
	"repo/api.yaml::Contract",
}

func newImpactReachStore(t *testing.T) *store_sqlite.Store {
	t.Helper()
	store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "impact.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	node := func(id string, kind graph.NodeKind) *graph.Node {
		path, _, _ := strings.Cut(id, "::")
		return &graph.Node{ID: id, Name: id, Kind: kind, FilePath: path, RepoPrefix: "repo", Language: "go", StartLine: 1}
	}
	nodes := []*graph.Node{
		node("repo/a.go", graph.KindFile),
		node(impactReachSeed, graph.KindFunction),
		node(impactReachDepth1, graph.KindFunction),
		node(impactReachDepth2, graph.KindFunction),
		node(impactReachDepth3, graph.KindFunction),
		node("repo/routes.go::Router", graph.KindFunction),
		node("repo/bean.go::BeanUser", graph.KindFunction),
		node("repo/bean.go::BeanMiddle", graph.KindFunction),
		node("repo/api.yaml::Contract", graph.KindContract),
	}
	calls := func(from, to string) *graph.Edge {
		return &graph.Edge{From: from, To: to, Kind: graph.EdgeCalls, Confidence: 1, FilePath: "repo/x.go", Line: 1}
	}
	spring := func(from, to string) *graph.Edge {
		edge := calls(from, to)
		edge.Meta = map[string]any{"via": "spring.Bean"}
		return edge
	}
	edges := []*graph.Edge{
		calls(impactReachDepth1, impactReachSeed),
		calls(impactReachDepth2, impactReachDepth1),
		calls(impactReachDepth3, impactReachDepth2),
		{From: "repo/a.go", To: impactReachSeed, Kind: graph.EdgeDefines},
		// Hidden at depth 1 and depth 2.
		{From: "repo/routes.go::Router", To: impactReachSeed, Kind: graph.EdgeHandlesRoute, Confidence: 1},
		spring("repo/bean.go::BeanUser", impactReachSeed),
		spring("repo/bean.go::BeanMiddle", impactReachDepth1),
		{From: "repo/api.yaml::Contract", To: impactReachSeed, Kind: graph.EdgeConsumes, Confidence: 1},
	}
	// The seed's own locals: more member_of rows than the live walk's whole
	// edge budget, read before any caller further up the chain.
	for i := range 60 {
		id := impactReachSeed + "#local:v" + strconv.Itoa(i)
		nodes = append(nodes, &graph.Node{ID: id, Name: "v" + strconv.Itoa(i), Kind: graph.KindLocal, FilePath: "repo/a.go", RepoPrefix: "repo", Language: "go", StartLine: 2})
		edges = append(edges, &graph.Edge{From: id, To: impactReachSeed, Kind: graph.EdgeMemberOf, Confidence: 1})
	}
	store.AddBatch(nodes, edges)
	return store
}

func impactReachIDs(result *analysis.ImpactResult) map[int][]string {
	out := make(map[int][]string)
	for depth := 1; depth <= 3; depth++ {
		for _, entry := range result.ByDepth[depth] {
			out[depth] = append(out[depth], entry.ID)
		}
	}
	return out
}

// Under the contract runtime every request reader is the contract-core
// wrapper. Impact must still run the bounded reach traversal through it: a
// seed whose own locals outnumber the live walk's edge budget keeps its
// callers at every depth, and no edge the wrapper hides contributes one.
func TestImpactThroughContractCoreReaderReachesEveryDepth(t *testing.T) {
	analyze := func(t *testing.T, installed bool) *analysis.ImpactResult {
		store := newImpactReachStore(t)
		srv := NewServer(query.NewEngine(store), store, nil, nil, zap.NewNop(), nil)
		if installed {
			installContractCoreKindTestRuntime(t, srv)
		}
		reader := srv.readerFor(t.Context())
		if installed {
			_, isStore := reader.(graph.Store)
			require.False(t, isStore, "the request reader is the wrapper, not the store")
		}
		return analysis.AnalyzeImpactContext(t.Context(), reader, []string{impactReachSeed}, nil, nil)
	}
	unwrapped := analyze(t, false)
	wrapped := analyze(t, true)

	got := impactReachIDs(wrapped)
	require.Equal(t, []string{impactReachDepth1}, got[1])
	require.Equal(t, []string{impactReachDepth2}, got[2])
	require.Equal(t, []string{impactReachDepth3}, got[3])
	require.False(t, wrapped.Truncated)

	// The store answers as it always has: every edge, hidden ones included.
	raw := impactReachIDs(unwrapped)
	for _, id := range []string{impactReachDepth1, impactReachDepth2, impactReachDepth3} {
		require.Contains(t, append(append(raw[1], raw[2]...), raw[3]...), id)
	}
	hidden := make(map[string]bool, len(impactReachHidden))
	for _, id := range impactReachHidden {
		hidden[id] = true
		require.Contains(t, append(raw[1], raw[2]...), id, "the fixture's hidden edge is in the store")
	}
	// The wrapper's answer is the store's with the hidden sources removed,
	// in the same order and with the same edge confidences.
	for depth := 1; depth <= 3; depth++ {
		var want []analysis.ImpactEntry
		for _, entry := range unwrapped.ByDepth[depth] {
			if !hidden[entry.ID] {
				want = append(want, entry)
			}
		}
		require.Equalf(t, want, wrapped.ByDepth[depth], "depth %d", depth)
	}
}

// An overlay is not the store, so its wrapper reports a reach miss and impact
// walks the overlay's own edges: a caller only the buffer has is counted.
func TestImpactThroughContractCoreOverlayReaderWalksLive(t *testing.T) {
	store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "overlay.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	store.AddBatch([]*graph.Node{
		{ID: impactReachSeed, Name: "Seed", Kind: graph.KindFunction, FilePath: "repo/a.go", RepoPrefix: "repo"},
		{ID: impactReachDepth1, Name: "Direct", Kind: graph.KindFunction, FilePath: "repo/b.go", RepoPrefix: "repo"},
	}, []*graph.Edge{
		{From: impactReachDepth1, To: impactReachSeed, Kind: graph.EdgeCalls, Confidence: 1},
	})
	srv := NewServer(query.NewEngine(store), store, nil, nil, zap.NewNop(), nil)
	installContractCoreKindTestRuntime(t, srv)

	const fresh = "repo/fresh.go::Fresh"
	layer := graph.NewOverlayLayer()
	layer.MarkFile("repo/fresh.go", false)
	layer.AddNode("repo/fresh.go", &graph.Node{ID: fresh, Name: "Fresh", Kind: graph.KindFunction, FilePath: "repo/fresh.go", RepoPrefix: "repo"})
	layer.AddEdge(&graph.Edge{From: fresh, To: impactReachSeed, Kind: graph.EdgeCalls, Confidence: 1})
	ctx := WithOverlayView(context.Background(), graph.NewOverlaidView(store, layer))

	reader := srv.readerFor(ctx)
	probe, ok := reader.(interface {
		ImpactReachContext(context.Context, string) (d1, d2, d3 []reach.Entry, hit, truncated bool)
	})
	require.True(t, ok, "every wrapper variant carries the probe")
	_, _, _, hit, truncated := probe.ImpactReachContext(ctx, impactReachSeed)
	require.False(t, hit, "an overlay must not be answered from the base store")
	require.False(t, truncated)

	result := analysis.AnalyzeImpactContext(ctx, reader, []string{impactReachSeed}, nil, nil)
	require.ElementsMatch(t, []string{impactReachDepth1, fresh}, impactReachIDs(result)[1])
	require.Len(t, store.AllNodes(), 2, "the overlay request must not mutate the base store")
}
