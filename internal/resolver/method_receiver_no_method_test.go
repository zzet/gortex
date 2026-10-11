package resolver

import (
	"iter"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

type receiverFallbackReadCounter struct {
	graph.Store
	fileReads, packageReads, kindReads, edgeReads int
}

func (s *receiverFallbackReadCounter) GetFileNodes(path string) []*graph.Node {
	s.fileReads++
	return s.Store.GetFileNodes(path)
}

func (s *receiverFallbackReadCounter) NodesInFilesByKind(paths []string, kinds []graph.NodeKind) []*graph.Node {
	s.packageReads++
	return s.Store.(graph.NodesInFilesByKindFinder).NodesInFilesByKind(paths, kinds)
}

func (s *receiverFallbackReadCounter) NodesByKind(kind graph.NodeKind) iter.Seq[*graph.Node] {
	s.kindReads++
	return s.Store.NodesByKind(kind)
}

func (s *receiverFallbackReadCounter) GetOutEdgesByNodeIDs(ids []string) map[string][]*graph.Edge {
	s.edgeReads++
	return s.Store.GetOutEdgesByNodeIDs(ids)
}

func (s *receiverFallbackReadCounter) EdgesByKind(kind graph.EdgeKind) iter.Seq[*graph.Edge] {
	s.edgeReads++
	return s.Store.EdgesByKind(kind)
}

func (s *receiverFallbackReadCounter) reset() {
	s.fileReads, s.packageReads, s.kindReads, s.edgeReads = 0, 0, 0, 0
}

func TestRebindGoMethodReceiversForFileSkipsNoMethodFallbackReads(t *testing.T) {
	for _, test := range []struct {
		name     string
		kind     graph.NodeKind
		language string
		cached   bool
		empty    bool
	}{
		{name: "free-function", kind: graph.KindFunction, language: "go"},
		{name: "cached-free-function", kind: graph.KindFunction, language: "go", cached: true},
		{name: "cached-empty", cached: true, empty: true},
		{name: "unknown-kind", kind: "future-kind", language: "go"},
		{name: "empty-kind", language: "go"},
		{name: "non-go-method", kind: graph.KindMethod, language: "typescript"},
		{name: "unknown-language-method", kind: graph.KindMethod, language: "future-language"},
		{name: "empty-language-method", kind: graph.KindMethod},
	} {
		t.Run(test.name, func(t *testing.T) {
			const file = "repo/pkg/edited.go"
			g := graph.New()
			g.AddBatch([]*graph.Node{
				{ID: "repo/pkg/types.go", Kind: graph.KindFile, FilePath: "repo/pkg/types.go", RepoPrefix: "repo", Language: "go"},
				{ID: "repo/pkg/types.go::T", Kind: graph.KindType, Name: "T", FilePath: "repo/pkg/types.go", RepoPrefix: "repo", Language: "go"},
			}, nil)
			if !test.empty {
				g.AddBatch([]*graph.Node{
					{ID: file, Kind: graph.KindFile, FilePath: file, RepoPrefix: "repo", Language: "go"},
					{ID: file + "::Edited", Kind: test.kind, FilePath: file, RepoPrefix: "repo", Language: test.language},
				}, []*graph.Edge{{From: file + "::Edited", To: file + "::T", Kind: graph.EdgeMemberOf, FilePath: file}})
			}
			store := &receiverFallbackReadCounter{Store: g}
			r := New(store)
			r.buildDirIndexes()
			defer r.clearDirIndexes()
			if test.cached {
				facts := g.GetFileNodes(file)
				if !test.empty {
					// Only nonnil full-file method facts count.
					facts = append(facts, nil)
				}
				r.incrementalNodesByFile = map[string][]*graph.Node{file: facts}
			}
			before := g.GetOutEdges(file + "::Edited")
			store.reset()
			r.rebindGoMethodReceiversForFile(file)
			require.Zero(t, store.packageReads)
			require.Zero(t, store.kindReads)
			require.Zero(t, store.edgeReads)
			if test.cached {
				require.Zero(t, store.fileReads)
			} else {
				require.Equal(t, 1, store.fileReads)
			}
			require.Empty(t, r.receiverTypeIdxByDir, "no package cache may be populated without a Go method")
			require.Equal(t, before, g.GetOutEdges(file+"::Edited"), "unknown/non-Go source edges remain unchanged")
		})
	}
}

func TestRebindGoMethodReceiversForFileUsesSelectedFullDeltaFacts(t *testing.T) {
	for _, test := range []struct {
		name      string
		hasMethod bool
		deleted   bool
	}{
		{name: "selected-function-hides-old-method"},
		{name: "carried-method-among-mixed-nodes", hasMethod: true},
		{name: "deleted-file", deleted: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			const file = "repo/pkg/edited.go"
			const methodID = file + "::T.Method"
			const canonical = "repo/pkg/types.go::T"
			g := graph.New()
			g.AddBatch([]*graph.Node{
				{ID: "repo/pkg/types.go", Kind: graph.KindFile, FilePath: "repo/pkg/types.go", RepoPrefix: "repo", Language: "go"},
				{ID: canonical, Kind: graph.KindType, Name: "T", FilePath: "repo/pkg/types.go", RepoPrefix: "repo", Language: "go"},
				{ID: file, Kind: graph.KindFile, FilePath: file, RepoPrefix: "repo", Language: "go"},
				{ID: methodID, Kind: graph.KindMethod, FilePath: file, RepoPrefix: "repo", Language: "go"},
			}, []*graph.Edge{{From: methodID, To: file + "::T", Kind: graph.EdgeMemberOf, FilePath: file}})
			layer := graph.NewOverlayLayer()
			layer.MarkFile(file, false)
			layer.AddNode(file, &graph.Node{ID: file, Kind: graph.KindFile, FilePath: file, RepoPrefix: "repo", Language: "go"})
			free := &graph.Node{ID: file + "::Free", Kind: graph.KindFunction, FilePath: file, RepoPrefix: "repo", Language: "go"}
			layer.AddNode(file, free)
			if test.hasMethod {
				layer.AddNode(file, &graph.Node{ID: methodID, Kind: graph.KindMethod, FilePath: file, RepoPrefix: "repo", Language: "go", Meta: map[string]any{"signature": "func (*T) Method()"}})
				layer.AddEdge(&graph.Edge{From: methodID, To: file + "::T", Kind: graph.EdgeMemberOf, FilePath: file})
				for _, n := range []*graph.Node{
					{ID: file + "::Unknown", Kind: "future-kind", Language: "go"},
					{ID: file + "::ForeignLanguage", Kind: graph.KindMethod, Language: "typescript"},
				} {
					n.FilePath, n.RepoPrefix = file, "repo"
					layer.AddNode(file, n)
					layer.AddEdge(&graph.Edge{From: n.ID, To: file + "::T", Kind: graph.EdgeMemberOf, FilePath: file})
				}
			}
			below := graph.NewOverlaidView(g, layer)
			delta := graph.NewDeltaWriter(below, graph.New())
			// A real edit touches only the free function. The delta carries
			// the entire selected file, including any unchanged Go method.
			delta.AddNode(free)
			if test.deleted {
				delta.EvictFile(file)
			}
			facts := delta.GetFileNodes(file)
			if test.deleted {
				require.Empty(t, facts, "a real selected file deletion has no surviving facts")
			}
			store := &receiverFallbackReadCounter{Store: delta}
			r := New(store)
			r.buildDirIndexes()
			defer r.clearDirIndexes()
			r.incrementalNodesByFile = map[string][]*graph.Node{file: facts}
			store.reset()
			r.rebindGoMethodReceiversForFile(file)
			// The real incremental tail flushes deferred retargets before returning.
			r.flushIncrementalAttributionReindexes()
			require.Zero(t, store.fileReads, "use the exact selected full-file cache")
			if test.hasMethod {
				require.Equal(t, 1, store.packageReads)
				require.Equal(t, 1, store.edgeReads)
				require.True(t, hasEdgeKind(delta, methodID, canonical, graph.EdgeMemberOf))
				for _, id := range []string{file + "::Unknown", file + "::ForeignLanguage"} {
					require.True(t, hasEdgeKind(delta, id, file+"::T", graph.EdgeMemberOf), "the mixed non-Go-method facts must not be rebound")
				}
			} else {
				require.Zero(t, store.packageReads)
				require.Zero(t, store.edgeReads)
				require.Nil(t, delta.GetNode(methodID), "selected file replacement hides the old method")
			}
			require.True(t, hasEdgeKind(g, methodID, file+"::T", graph.EdgeMemberOf), "fallback writes must not mutate the base")
			require.Zero(t, store.kindReads)
		})
	}
}
