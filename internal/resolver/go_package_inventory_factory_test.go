package resolver

import (
	"context"
	"iter"
	"reflect"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

type ownershipInventoryStore struct {
	*graph.Graph
	full, compact int
}

func (s *ownershipInventoryStore) NodesInScopeSeq(repos, paths []string, kinds ...graph.NodeKind) iter.Seq[*graph.Node] {
	s.full++
	return graph.NodesInScopeSeq(s.Graph, repos, paths, kinds...)
}
func (s *ownershipInventoryStore) NodesLightInScopeSeq(repos, paths []string) iter.Seq[*graph.Node] {
	return graph.NodesInScopeSeq(s.Graph, repos, paths, graph.KindFile)
}
func (s *ownershipInventoryStore) EdgesInScopeSeq(repos, paths []string, kinds ...graph.EdgeKind) iter.Seq[graph.ScopedEdgeRow] {
	return graph.EdgesInScopeSeq(s.Graph, repos, paths, kinds...)
}

func (s *ownershipInventoryStore) NodesLightByKindsInScopeSeq(repos, paths []string, kinds ...graph.NodeKind) iter.Seq[*graph.Node] {
	s.compact++
	return func(yield func(*graph.Node) bool) {
		for node := range graph.NodesInScopeSeq(s.Graph, repos, paths, kinds...) {
			copyNode := *node
			copyNode.Meta = nil
			if !yield(&copyNode) {
				return
			}
		}
	}
}
func TestGoPackageInventoryFactoryKeepsRichCustomContract(t *testing.T) {
	f := newOwnershipResolverFixture(t, "go")
	g := f.r.graph.(*graph.Graph)
	for _, n := range append([]*graph.Node{f.mainFile}, f.files...) {
		n.Meta = map[string]any{"rich": "retained"}
		g.AddNode(n)
	}
	store := &ownershipInventoryStore{Graph: g}
	f.r.graph = store
	check := func(compact bool) GoPackageOwnershipFactory {
		return func(ctx context.Context, repos []string, files iter.Seq[*graph.Node]) (map[string]GoPackageOwnershipLookup, error) {
			if !reflect.DeepEqual(repos, []string{"repo"}) {
				t.Fatalf("scope=%v", repos)
			}
			count := 0
			for file := range files {
				count++
				if file.Kind != graph.KindFile || file.Language != "go" || file.RepoPrefix != "repo" || file.ID != file.FilePath {
					t.Fatalf("changed consumed fields: %+v", file)
				}
				if compact && file.Meta != nil {
					t.Fatal("compact producer requested rich payload")
				}
				if !compact && file.Meta["rich"] != "retained" {
					t.Fatal("custom rich factory lost metadata")
				}
			}
			if count != 3 {
				t.Fatalf("files=%d", count)
			}
			return nil, ctx.Err()
		}
	}
	f.r.SetGoPackageOwnershipFactory(check(false))
	if err := f.r.prepareGoPackageOwnership(t.Context(), ownershipPending(f), f.r.nodeByID); err != nil {
		t.Fatal(err)
	}
	if store.full != 1 || store.compact != 0 {
		t.Fatalf("ordinary factory projections full=%d compact=%d", store.full, store.compact)
	}
	f.r.SetGoPackageOwnershipInventoryFactory(check(true))
	if err := f.r.prepareGoPackageOwnership(t.Context(), ownershipPending(f), f.r.nodeByID); err != nil {
		t.Fatal(err)
	}
	if store.full != 1 || store.compact != 1 {
		t.Fatalf("opted-in factory projections full=%d compact=%d", store.full, store.compact)
	}
	f.r.SetGoPackageOwnershipFactory(check(false))
	if err := f.r.prepareGoPackageOwnership(t.Context(), ownershipPending(f), f.r.nodeByID); err != nil {
		t.Fatal(err)
	}
	if store.full != 2 {
		t.Fatal("ordinary setter did not restore rich contract")
	}
}
func TestGoPackageInventoryFactoryActualResolutionAndEpoch(t *testing.T) {
	for _, mode := range []string{"all", "files"} {
		t.Run(mode, func(t *testing.T) {
			f := newOwnershipResolverFixture(t, "go")
			store := &ownershipInventoryStore{Graph: f.r.graph.(*graph.Graph)}
			f.r.graph = store
			calls := 0
			factory := ownershipFactoryForFixture(t, f, &calls)
			f.r.SetGoPackageOwnershipInventoryFactory(factory)
			store.AddBatch(nil, ownershipPending(f))
			f.r.clearLookupCache()
			if mode == "all" {
				if _, err := f.r.ResolveAllContext(t.Context()); err != nil {
					t.Fatal(err)
				}
			} else {
				f.r.ResolveFilesAndIncoming([]string{f.mainFile.FilePath})
			}
			if calls != 1 || store.compact != 1 || store.full != 0 {
				t.Fatalf("factory=%d compact=%d full=%d", calls, store.compact, store.full)
			}
			matched := false
			for _, edge := range store.GetOutEdges(f.caller.ID) {
				if edge.Kind == graph.EdgeCalls {
					if edge.To != f.definitions[1].ID {
						t.Fatalf("wrong package: %+v", edge)
					}
					matched = true
				}
			}
			if !matched {
				t.Fatal("positive resolved call missing")
			}
			if len(f.r.goPackageOwnershipPrepared) != 0 {
				t.Fatal("prepared map survived completed pass")
			}
		})
	}
}
