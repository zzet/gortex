package resolver

import (
	"iter"
	"reflect"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// providesScanCountingStore counts the repository-scoped provides reads the
// pending-frontier preparation issues.
type providesScanCountingStore struct {
	graph.Store
	scopedProvides int
}

func (s *providesScanCountingStore) EdgesInScopeSeq(repoPrefixes, filePaths []string, kinds ...graph.EdgeKind) iter.Seq[graph.ScopedEdgeRow] {
	for _, kind := range kinds {
		if kind == graph.EdgeProvides {
			s.scopedProvides++
		}
	}
	return graph.EdgesInScopeSeq(s.Store, repoPrefixes, filePaths, kinds...)
}

func (s *providesScanCountingStore) NodesInScopeSeq(repoPrefixes, filePaths []string, kinds ...graph.NodeKind) iter.Seq[*graph.Node] {
	return graph.NodesInScopeSeq(s.Store, repoPrefixes, filePaths, kinds...)
}

func (s *providesScanCountingStore) NodesLightInScopeSeq(repoPrefixes, filePaths []string) iter.Seq[*graph.Node] {
	return graph.NodesLightInScopeSeq(s.Store, repoPrefixes, filePaths)
}

// A wildcard-member pending reference needs the DI provides index for its
// source repositories. The index must hold exactly the bindings a
// repository-scoped read would (provides edges whose source node is in one of
// those repositories), without that read: a repository-scoped edge read walks
// every edge of the repository on a large store.
func TestPendingFrontierProvidesIndexIsBucketedFromOneKindRead(t *testing.T) {
	pendingMember := &graph.Edge{From: "repo/caller.ts::Caller", To: graph.UnresolvedMarker + "*.notify",
		Kind: graph.EdgeCalls, FilePath: "repo/caller.ts", Meta: map[string]any{"receiver_type": "Notifier"}}
	nodes := []*graph.Node{
		{ID: "repo/caller.ts::Caller", Kind: graph.KindFunction, Name: "Caller", FilePath: "repo/caller.ts", RepoPrefix: "repo"},
		{ID: "repo/module.ts::Module", Kind: graph.KindType, Name: "Module", FilePath: "repo/module.ts", RepoPrefix: "repo"},
		{ID: "repo/email.ts::EmailNotifier", Kind: graph.KindType, Name: "EmailNotifier", FilePath: "repo/email.ts", RepoPrefix: "repo"},
		{ID: "other/module.ts::Module", Kind: graph.KindType, Name: "Module", FilePath: "other/module.ts", RepoPrefix: "other"},
	}
	provides := func(from, to, abstract string) *graph.Edge {
		return &graph.Edge{From: from, To: to, Kind: graph.EdgeProvides, FilePath: "x",
			Meta: map[string]any{"provides_for": abstract, "binding": "useClass"}}
	}
	for _, withProvides := range []bool{false, true} {
		g := graph.New()
		edges := []*graph.Edge{pendingMember}
		if withProvides {
			edges = append(edges,
				provides("repo/module.ts::Module", "repo/email.ts::EmailNotifier", "Notifier"),
				provides("repo/module.ts::Module", graph.UnresolvedMarker+"SmsNotifier", "Notifier"),
				// Another repository's binding is not in the source's scope.
				provides("other/module.ts::Module", "other::PagerNotifier", "Notifier"),
				// A binding whose source node is missing belongs to no repository.
				provides("repo/gone.ts::Module", "repo::GoneNotifier", "Notifier"))
		}
		g.AddBatch(nodes, edges)
		store := &providesScanCountingStore{Store: g}
		r := New(store)
		if !pendingNeedsProvidesIndex([]*graph.Edge{pendingMember}) {
			t.Fatal("the fixture's member reference must need the provides index")
		}
		indexes := newPendingFrontierPassIndexes(r)
		indexes.prepare([]*graph.Edge{pendingMember})
		if r.providesForIdx == nil {
			t.Fatalf("provides=%v: the prepared pass left the provides index nil (a later lookup would rebuild it whole)", withProvides)
		}
		if store.scopedProvides != 0 {
			t.Fatalf("provides=%v: the pass paid %d repository-scoped provides read(s)", withProvides, store.scopedProvides)
		}
		want := map[string]map[string]struct{}{}
		if withProvides {
			want["Notifier"] = map[string]struct{}{"EmailNotifier": {}, "SmsNotifier": {}}
		}
		if !reflect.DeepEqual(r.providesForIdx, want) {
			t.Fatalf("provides=%v: index = %#v, want %#v", withProvides, r.providesForIdx, want)
		}
		indexes.close()
	}
}
