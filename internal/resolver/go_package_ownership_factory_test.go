package resolver

import (
	"context"
	"errors"
	"iter"
	"reflect"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

func ownershipFactoryForFixture(t testing.TB, f ownershipResolverFixture, calls *int) GoPackageOwnershipFactory {
	t.Helper()
	installFixtureOwnership(t, f, "example.test/fixture/misc/graph", f.files[1].FilePath)
	lookup := f.r.goPackageOwnership
	f.r.SetGoPackageOwnership(nil)
	return func(ctx context.Context, prefixes []string, files iter.Seq[*graph.Node]) (map[string]GoPackageOwnershipLookup, error) {
		*calls++
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !reflect.DeepEqual(prefixes, []string{"repo"}) {
			t.Errorf("unbounded repo scope: %v", prefixes)
		}
		count := 0
		for file := range files {
			if file == nil || file.Kind != graph.KindFile || file.RepoPrefix != "repo" || file.Language != "go" {
				t.Errorf("wrong scoped rich file: %+v", file)
			}
			count++
		}
		if count != 3 {
			t.Errorf("projected file count=%d want 3", count)
		}
		return map[string]GoPackageOwnershipLookup{"repo": lookup}, nil
	}
}

func ownershipPending(f ownershipResolverFixture) []*graph.Edge {
	return []*graph.Edge{
		{From: f.caller.ID, To: "unresolved::extern::example.test/fixture/misc/graph::Use", Kind: graph.EdgeCalls, FilePath: f.mainFile.FilePath},
		{From: f.mainFile.ID, To: "unresolved::import::example.test/fixture/misc/graph", Kind: graph.EdgeImports, FilePath: f.mainFile.FilePath},
	}
}

func TestGoPackageOwnershipFactoryScopedEpochAndInterleave(t *testing.T) {
	f := newOwnershipResolverFixture(t, "go")
	f.r.graph.AddBatch([]*graph.Node{{ID: "unrelated/large.go", Kind: graph.KindFile, FilePath: "unrelated/large.go", RepoPrefix: "unrelated", Language: "go"}}, nil)
	calls := 0
	f.r.SetGoPackageOwnershipFactory(ownershipFactoryForFixture(t, f, &calls))
	pending := ownershipPending(f)
	for i := 0; i < 2; i++ {
		if err := f.r.prepareGoPackageOwnership(t.Context(), pending, f.r.nodeByID); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 1 {
		t.Fatalf("factory repeated within epoch: %d", calls)
	}
	gate := f.r.goImportGateForEdge(pending[0], "example.test/fixture/misc/graph")
	if gate.retainNode(f.definitions[0]) || !gate.retainNode(f.definitions[1]) {
		t.Fatal("staged factory not used by gate")
	}
	f.r.clearPassIndexes()
	if err := f.r.prepareGoPackageOwnership(t.Context(), pending, f.r.nodeByID); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("new pass reused old facts: %d", calls)
	}
	indexes := newResolveAllPassIndexes(f.r)
	defer indexes.close()
	changed, err := indexes.refreshAfterInterleave(t.Context(), pending, true)
	if err != nil || !changed || calls != 3 {
		t.Fatalf("interleave did not rebuild facts: changed=%v calls=%d err=%v", changed, calls, err)
	}
}

func TestGoPackageOwnershipFactoryNoWorkAndUnknownLanguageAreFree(t *testing.T) {
	f := newOwnershipResolverFixture(t, "python")
	f.r.SetGoPackageOwnershipFactory(func(context.Context, []string, iter.Seq[*graph.Node]) (map[string]GoPackageOwnershipLookup, error) {
		t.Fatal("prepared module evidence without Go pending work")
		return nil, nil
	})
	for _, item := range []struct {
		pending []*graph.Edge
		sources map[string]*graph.Node
	}{
		{nil, nil}, {ownershipPending(f), f.r.nodeByID}, {ownershipPending(f), map[string]*graph.Node{}},
		{[]*graph.Edge{{From: f.caller.ID, To: "unresolved::unrelated_name"}}, f.r.nodeByID},
	} {
		if err := f.r.prepareGoPackageOwnership(t.Context(), item.pending, item.sources); err != nil {
			t.Fatal(err)
		}
	}
	if len(f.r.goPackageOwnershipPrepared) != 0 {
		t.Fatal("no-work call published coverage")
	}
}

func TestGoPackageOwnershipFactoryCancellationPrecedesActualResolveWrites(t *testing.T) {
	f := newOwnershipResolverFixture(t, "go")
	pending := ownershipPending(f)
	f.r.graph.AddBatch(nil, pending)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	calls := 0
	f.r.SetGoPackageOwnershipFactory(func(context.Context, []string, iter.Seq[*graph.Node]) (map[string]GoPackageOwnershipLookup, error) {
		calls++
		cancel()
		return nil, ctx.Err()
	})
	_, err := f.r.ResolveAllContext(ctx)
	if !errors.Is(err, context.Canceled) || calls != 1 {
		t.Fatalf("cancellation=%v factory_calls=%d", err, calls)
	}
	if len(f.r.goPackageOwnershipPrepared) != 0 {
		t.Fatal("published canceled partial facts")
	}
	for _, node := range []*graph.Node{f.mainFile, f.caller} {
		for _, edge := range f.r.graph.GetOutEdges(node.ID) {
			if !graph.IsUnresolvedTarget(edge.To) {
				t.Errorf("resolution mutated before canceled preparation returned: %+v", edge)
			}
		}
	}
}

func TestGoPackageOwnershipFactoryActualResolverEntryPaths(t *testing.T) {
	for _, mode := range []string{"all", "file", "file_and_incoming", "files_and_incoming"} {
		t.Run(mode, func(t *testing.T) {
			f := newOwnershipResolverFixture(t, "go")
			f.r.graph.AddBatch(nil, ownershipPending(f))
			calls := 0
			f.r.SetGoPackageOwnershipFactory(ownershipFactoryForFixture(t, f, &calls))
			f.r.clearLookupCache()
			switch mode {
			case "all":
				if _, err := f.r.ResolveAllContext(t.Context()); err != nil {
					t.Fatal(err)
				}
			case "file":
				f.r.ResolveFile(f.mainFile.FilePath)
			case "file_and_incoming":
				f.r.ResolveFileAndIncoming(f.mainFile.FilePath)
			case "files_and_incoming":
				f.r.ResolveFilesAndIncoming([]string{f.mainFile.FilePath})
			}
			if calls != 1 {
				t.Fatalf("factory calls=%d want 1", calls)
			}
			for _, tc := range []struct {
				from, target string
				kind         graph.EdgeKind
			}{
				{f.caller.ID, f.definitions[1].ID, graph.EdgeCalls}, {f.mainFile.ID, f.files[1].ID, graph.EdgeImports},
			} {
				edges := f.r.graph.GetOutEdges(tc.from)
				matched := 0
				for _, edge := range edges {
					if edge.Kind == tc.kind {
						matched++
						if edge.To != tc.target {
							t.Errorf("%s target=%s want=%s", mode, edge.To, tc.target)
						}
					}
				}
				if matched != 1 {
					t.Errorf("%s matching edge count=%d", mode, matched)
				}
			}
			if len(f.r.goPackageOwnershipPrepared) != 0 {
				t.Fatal("pass retained source facts after return")
			}
		})
	}
}
