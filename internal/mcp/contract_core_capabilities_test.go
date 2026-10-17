package mcp

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/analysis"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/query"
	"github.com/zzet/gortex/internal/search"
	"github.com/zzet/gortex/internal/search/rerank"
)

// newContractCoreContentServer serves an unrouted (primary) session from a
// SQLite store whose content index holds one section.
func newContractCoreContentServer(t *testing.T) *Server {
	t.Helper()
	store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "content.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	store.AddNode(&graph.Node{
		ID:       "doc.txt::doc:section-0",
		Kind:     graph.KindDoc,
		FilePath: "doc.txt",
		Meta:     map[string]any{"data_class": "content", "section_text": "snippet"},
	})
	require.NoError(t, store.AppendContent("", []graph.ContentFTSItem{
		{NodeID: "doc.txt::doc:section-0", FilePath: "doc.txt", Body: "the zzcontentterm appears only in the full body"},
	}))
	require.NoError(t, store.BuildContentIndex())
	return &Server{
		graph:      store,
		session:    newSessionState(),
		tokenStats: &tokenStats{},
		symHistory: &symbolHistory{entries: make(map[string][]SymbolModification)},
		sessions:   newSessionMap(),
		toolScopes: newScopeRegistry(),
	}
}

// The daemon installs the contract runtime by default; a primary session's
// content channel must still merge the graph's content section.
func TestContractCoreContentChannelKeepsDocNodeOnPrimarySession(t *testing.T) {
	for _, installed := range []bool{false, true} {
		t.Run(fmt.Sprintf("runtime_installed_%v", installed), func(t *testing.T) {
			s := newContractCoreContentServer(t)
			if installed {
				installContractCoreKindTestRuntime(t, s)
				_, wrapped := s.readerFor(t.Context()).(contractCoreWrapped)
				require.True(t, wrapped, "the runtime wraps the primary reader")
			}
			_, ok := s.contentSearcherFor(t.Context())
			require.True(t, ok, "the primary session keeps its content index")
			var ids []string
			for _, n := range s.mergeContentChannel(t.Context(), "zzcontentterm", nil, 10) {
				ids = append(ids, n.ID)
			}
			require.Contains(t, ids, "doc.txt::doc:section-0")
		})
	}
}

// Binding a wrapped reader to a request context binds the selected reader and
// keeps the contract filter, the contract set and the edge timing.
func TestContractCoreBindReadContextKeepsEdgeFilter(t *testing.T) {
	store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "bind.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	store.AddBatch([]*graph.Node{
		{ID: "repo/a.go::F", Name: "F", Kind: graph.KindFunction, FilePath: "repo/a.go", RepoPrefix: "repo"},
		{ID: "repo/b.go::G", Name: "G", Kind: graph.KindFunction, FilePath: "repo/b.go", RepoPrefix: "repo"},
		{ID: "repo/c.yaml::C", Name: "C", Kind: graph.KindContract, FilePath: "repo/c.yaml", RepoPrefix: "repo"},
		{ID: "repo/d.go::D", Name: "D", Kind: graph.KindFunction, FilePath: "repo/d.go", RepoPrefix: "repo"},
	}, []*graph.Edge{
		{From: "repo/b.go::G", To: "repo/a.go::F", Kind: graph.EdgeCalls},
		{From: "repo/c.yaml::C", To: "repo/a.go::F", Kind: graph.EdgeCalls},
		{From: "repo/d.go::D", To: "repo/a.go::F", Kind: graph.EdgeCalls},
	})
	callers := func(r graph.Reader) []string {
		var out []string
		for _, e := range r.GetInEdges("repo/a.go::F") {
			out = append(out, e.From)
		}
		return out
	}
	require.ElementsMatch(t, []string{"repo/b.go::G", "repo/c.yaml::C", "repo/d.go::D"}, callers(store))

	timing := &rerank.CoreEdgeTiming{}
	cases := []struct {
		name string
		ids  map[string]bool
		want []string
	}{
		// No contract set: endpoint kinds decide, so the contract node goes.
		{"endpoint_kinds", nil, []string{"repo/b.go::G", "repo/d.go::D"}},
		// A selected contract set hides exactly its identities.
		{"contract_ids", map[string]bool{"repo/d.go::D": true}, []string{"repo/b.go::G", "repo/c.yaml::C"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wrapped := wrapContractCoreEdges(&contractCoreEdges{Reader: store, ctx: t.Context(), contractIDs: tc.ids, edgeTiming: timing})
			require.ElementsMatch(t, tc.want, callers(wrapped))

			bindCtx, cancel := context.WithCancel(t.Context())
			defer cancel()
			bound := graph.BindReadContext(wrapped, bindCtx)
			require.IsType(t, wrapped, bound, "binding keeps the capability variant")
			core := bound.(contractCoreWrapped).contractCore()
			require.Equal(t, tc.ids, core.contractIDs)
			require.Same(t, timing, core.edgeTiming)
			boundStore, ok := contractCoreSelectedReader(bound).(*store_sqlite.Store)
			require.True(t, ok)
			require.NotSame(t, store, boundStore, "the selected store is bound to the request")
			require.ElementsMatch(t, tc.want, callers(bound), "the bound reader still filters adjacency")

			cancel()
			for edge := range bound.EdgesByKind(graph.EdgeCalls) {
				t.Fatalf("an abandoned request kept paging edges: %v", edge)
			}
		})
	}
}

// allNodesCountingStore counts full node-table reads behind the wrapper.
type allNodesCountingStore struct {
	*store_sqlite.Store
	allNodes int
}

func (s *allNodesCountingStore) AllNodes() []*graph.Node {
	s.allNodes++
	return s.Store.AllNodes()
}

// Betweenness (suggested_review_questions) scans function and method IDs
// through the wrapper without reading the whole node table, and a selected
// reader without the ID projection answers from its own kind iterators.
func TestContractCoreNodeIDsByKindsSkipsAllNodes(t *testing.T) {
	store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "ids.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	store.AddBatch([]*graph.Node{
		{ID: "repo/a.go::A", Name: "A", Kind: graph.KindFunction, FilePath: "repo/a.go", RepoPrefix: "repo"},
		{ID: "repo/a.go::T.M", Name: "M", Kind: graph.KindMethod, FilePath: "repo/a.go", RepoPrefix: "repo"},
		{ID: "repo/a.go::T", Name: "T", Kind: graph.KindType, FilePath: "repo/a.go", RepoPrefix: "repo"},
		{ID: "repo/c.yaml::C", Name: "C", Kind: graph.KindContract, FilePath: "repo/c.yaml", RepoPrefix: "repo"},
	}, []*graph.Edge{
		{From: "repo/a.go::A", To: "repo/a.go::T.M", Kind: graph.EdgeCalls},
	})
	counting := &allNodesCountingStore{Store: store}
	wrapped := newContractCoreEdges(counting, t.Context(), nil)
	analysis.ComputeBetweenness(wrapped)
	require.Zero(t, counting.allNodes, "betweenness read the whole node table through the wrapper")

	kinds := []graph.NodeKind{graph.KindFunction, graph.KindMethod, graph.KindFunction}
	want := []string{"repo/a.go::A", "repo/a.go::T.M"}
	scan, ok := wrapped.(graph.NodeIDsByKinds)
	require.True(t, ok)
	require.ElementsMatch(t, want, scan.NodeIDsByKinds(kinds))
	overlaid := graph.NewOverlaidView(store, graph.NewOverlayLayer())
	_, overlaidScans := any(overlaid).(graph.NodeIDsByKinds)
	require.False(t, overlaidScans, "the fallback case needs a reader without the ID projection")
	fallback := newContractCoreEdges(overlaid, t.Context(), nil).(graph.NodeIDsByKinds)
	require.ElementsMatch(t, want, fallback.NodeIDsByKinds(kinds))
	require.Empty(t, fallback.NodeIDsByKinds(nil))
}

// fixedQueryEmbedder embeds every query as the same vector.
type fixedQueryEmbedder struct{ vec []float32 }

func (e fixedQueryEmbedder) Embed(context.Context, string) ([]float32, error) { return e.vec, nil }
func (e fixedQueryEmbedder) EmbedBatch(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i := range texts {
		out[i] = e.vec
	}
	return out, nil
}
func (e fixedQueryEmbedder) Dimensions() int { return len(e.vec) }
func (e fixedQueryEmbedder) Close() error    { return nil }

// The post-rerank cosine refinement reads the selected reader's stored
// vectors through the wrapper, and refines nothing when that reader has none.
func TestContractCoreCosineRefinementReadsSelectedVectors(t *testing.T) {
	store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "vectors.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	ids := []string{"repo/a.go::A", "repo/a.go::B", "repo/a.go::C"}
	var nodes []*graph.Node
	for _, id := range ids {
		nodes = append(nodes, &graph.Node{ID: id, Name: id[len(id)-1:], Kind: graph.KindFunction, FilePath: "repo/a.go", RepoPrefix: "repo"})
	}
	store.AddBatch(nodes, nil)
	// C points nearly along the query, B at 45 degrees, A orthogonally.
	require.NoError(t, store.UpsertEmbedding("repo/a.go::A", []float32{0, 1, 0}))
	require.NoError(t, store.UpsertEmbedding("repo/a.go::B", []float32{1, 1, 0}))
	require.NoError(t, store.UpsertEmbedding("repo/a.go::C", []float32{10, 0.1, 0}))

	embedder := fixedQueryEmbedder{vec: []float32{1, 0, 0}}
	engine := query.NewEngine(store)
	engine.SetSearch(search.NewSwappable(search.NewHybrid(search.NewNull(), search.NewVector(3), embedder)))
	refine := func(r graph.Reader) []string {
		var cands []*rerank.Candidate
		for _, n := range nodes {
			cands = append(cands, &rerank.Candidate{Node: n})
		}
		var out []string
		for _, c := range engine.WithComposedView(r, nil, t.Context(), false).RefineByCosine("q", cands, 0) {
			out = append(out, c.Node.ID)
		}
		return out
	}
	refined := []string{"repo/a.go::C", "repo/a.go::B", "repo/a.go::A"}
	require.Equal(t, refined, refine(store), "the raw store refines")
	wrapped := newContractCoreEdges(store, t.Context(), nil)
	require.Equal(t, refined, refine(wrapped), "the wrapper keeps the selected store's vectors")
	require.Equal(t, refined, refine(graph.BindReadContext(wrapped, t.Context())), "and so does the bound wrapper")
	require.Equal(t, ids, refine(newContractCoreEdges(graph.New(), t.Context(), nil)), "a selected reader without vectors refines nothing")
}

// Search assist's per-fragment exact-name rescue reads the selected reader's
// batched name lookup, so a primary session keeps it under the runtime.
func TestContractCoreSearchAssistKeepsExactNameRescue(t *testing.T) {
	for _, installed := range []bool{false, true} {
		t.Run(fmt.Sprintf("runtime_installed_%v", installed), func(t *testing.T) {
			s := newContractCoreContentServer(t)
			store := s.graph.(*store_sqlite.Store)
			store.AddNode(&graph.Node{ID: "repo/a.go::BillingInvoice", Name: "BillingInvoice", Kind: graph.KindFunction, FilePath: "repo/a.go", RepoPrefix: "repo"})
			s.engine = query.NewEngine(store)
			s.engine.SetSearch(search.NewSwappable(search.NewNull()))
			if installed {
				installContractCoreKindTestRuntime(t, s)
			}
			eng := s.engineFor(t.Context())
			_, wrapped := eng.Reader().(contractCoreWrapped)
			require.Equal(t, installed, wrapped, "the runtime wraps the engine's reader")
			names, ok := graphReaderFromEngine(eng)
			require.True(t, ok, "the primary session keeps the batched name lookup")
			require.Same(t, store, names)
			merged, _ := fetchAndMergeBM25TimedContext(t.Context(), eng, "billing invoice", []string{"BillingInvoice"}, 10, query.QueryOptions{}, nil)
			var ids []string
			for _, n := range merged {
				ids = append(ids, n.ID)
			}
			require.Contains(t, ids, "repo/a.go::BillingInvoice", "the exact-name rescue ran")
		})
	}
}

// Diff joins in internal/analysis probe this exact shape; the wrapper hands
// them the selected store's deadline-aware lookup, and a selected reader
// without one answers as their GetFileNodes fallback does.
func TestContractCoreFileNodesContextKeepsDeadline(t *testing.T) {
	type fileNodesContext interface {
		GetFileNodesContext(context.Context, string) []*graph.Node
	}
	ids := func(nodes []*graph.Node) []string {
		var out []string
		for _, n := range nodes {
			out = append(out, n.ID)
		}
		return out
	}
	node := &graph.Node{ID: "repo/a.go::A", Name: "A", Kind: graph.KindFunction, FilePath: "repo/a.go", RepoPrefix: "repo"}
	canceled, cancel := context.WithCancel(t.Context())
	cancel()

	store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "files.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	store.AddNode(node)
	wrapped, ok := newContractCoreEdges(store, t.Context(), nil).(fileNodesContext)
	require.True(t, ok, "the wrapper keeps the deadline-aware file lookup")
	require.Equal(t, []string{node.ID}, ids(wrapped.GetFileNodesContext(t.Context(), "repo/a.go")))
	require.Equal(t, ids(store.GetFileNodesContext(canceled, "repo/a.go")), ids(wrapped.GetFileNodesContext(canceled, "repo/a.go")), "the request context reaches the store")
	require.Empty(t, wrapped.GetFileNodesContext(canceled, "repo/a.go"), "a canceled request reads no file rows")

	memory := graph.New()
	memory.AddNode(node)
	plain := newContractCoreEdges(memory, t.Context(), nil).(fileNodesContext)
	require.Equal(t, ids(memory.GetFileNodes("repo/a.go")), ids(plain.GetFileNodesContext(canceled, "repo/a.go")), "without the lookup the wrapper answers as GetFileNodes")
}
