package resolver

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"path/filepath"
	"sort"
	"testing"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// inferenceFrontierFixture builds a random two-repository hierarchy: types and
// interfaces (interfaces carry their method names), member methods drawn from a
// small name pool (so method sets overlap and some types satisfy interfaces),
// structural parent edges with mixed origins, a few pre-existing implements and
// overrides edges (some at a lower origin, to exercise provenance upgrades) and,
// with dup, a type with two member methods of one name.
func inferenceFrontierFixture(g graph.Store, seed int64, dup bool) []string {
	rng := rand.New(rand.NewSource(seed))
	names := []string{"Get", "Put", "Close", "Len", "Name"}
	var nodes []*graph.Node
	var edges []*graph.Edge
	var owners []string
	for i := 0; i < 36; i++ {
		repo := "r1"
		if i%3 == 0 {
			repo = "r2"
		}
		file := fmt.Sprintf("%s/p%d/f%d.go", repo, i%5, i)
		id := fmt.Sprintf("%s::T%d", file, i)
		kind := graph.KindType
		var meta map[string]any
		if i%4 == 0 {
			kind = graph.KindInterface
			var methods []string
			for _, name := range names {
				if rng.Intn(3) == 0 {
					methods = append(methods, name)
				}
			}
			if len(methods) == 0 {
				methods = []string{names[rng.Intn(len(names))]}
			}
			meta = map[string]any{"methods": methods}
		}
		nodes = append(nodes, &graph.Node{ID: id, Kind: kind, Name: fmt.Sprintf("T%d", i), FilePath: file,
			StartLine: 10 + i, RepoPrefix: repo, Meta: meta})
		owners = append(owners, id)
		for j, name := range names {
			if rng.Intn(2) == 0 {
				continue
			}
			methodID := fmt.Sprintf("%s.%s", id, name)
			nodes = append(nodes, &graph.Node{ID: methodID, Kind: graph.KindMethod, Name: name, FilePath: file,
				StartLine: 20 + j, RepoPrefix: repo})
			edges = append(edges, &graph.Edge{From: methodID, To: id, Kind: graph.EdgeMemberOf, FilePath: file, Line: 20 + j})
		}
	}
	if dup {
		id := owners[1]
		for k := 0; k < 2; k++ {
			methodID := fmt.Sprintf("%s.Dup%d", id, k)
			nodes = append(nodes, &graph.Node{ID: methodID, Kind: graph.KindMethod, Name: "Get", FilePath: "r1/dup.go",
				StartLine: 90 + k, RepoPrefix: "r1"})
			edges = append(edges, &graph.Edge{From: methodID, To: id, Kind: graph.EdgeMemberOf, FilePath: "r1/dup.go", Line: 90 + k})
		}
	}
	kinds := []graph.EdgeKind{graph.EdgeExtends, graph.EdgeImplements, graph.EdgeComposes}
	origins := []string{graph.OriginASTResolved, graph.OriginASTInferred, graph.OriginLSPDispatch, ""}
	for i := 0; i < 40; i++ {
		from, to := owners[rng.Intn(len(owners))], owners[rng.Intn(len(owners))]
		edges = append(edges, &graph.Edge{From: from, To: to, Kind: kinds[rng.Intn(len(kinds))],
			Origin: origins[rng.Intn(len(origins))], FilePath: "parents.go", Line: i + 1})
	}
	for i := 0; i < 6; i++ {
		from, to := owners[rng.Intn(len(owners))], owners[rng.Intn(len(owners))]
		edges = append(edges, &graph.Edge{From: from, To: to, Kind: graph.EdgeImplements,
			Meta: map[string]any{"via": MetaViaMethodSetInference}, FilePath: "old.go", Line: 500 + i})
	}
	g.AddBatch(nodes, edges)
	// A few overrides that already exist at a low origin.
	var overrides []*graph.Edge
	for i := 0; i < 4; i++ {
		owner := owners[rng.Intn(len(owners))]
		for _, name := range names {
			overrides = append(overrides, &graph.Edge{From: owner + "." + name, To: owners[rng.Intn(len(owners))] + "." + name,
				Kind: graph.EdgeOverrides, Origin: graph.OriginASTInferred, FilePath: "old.go", Line: 600 + i})
			break
		}
	}
	g.AddBatch(nil, overrides)
	return owners
}

func hierarchyRows(t *testing.T, g graph.Store) []string {
	t.Helper()
	var rows []string
	for _, kind := range []graph.EdgeKind{graph.EdgeImplements, graph.EdgeOverrides} {
		for edge := range g.EdgesByKind(kind) {
			meta, _ := json.Marshal(edge.Meta)
			rows = append(rows, fmt.Sprintf("%s|%s|%s|%s|%d|%s|%v|%s|%s", edge.Kind, edge.From, edge.To,
				edge.FilePath, edge.Line, edge.Origin, edge.Confidence, edge.ConfidenceLabel, meta))
		}
	}
	sort.Strings(rows)
	return rows
}

// The frontier forms add exactly the rows the scoped passes add, for random
// frontiers of types and interfaces, in memory and in SQLite, with and without
// duplicate member names (which take the scoped pass).
func TestHierarchyInferenceForFrontierMatchesTheScopedPasses(t *testing.T) {
	for seed := int64(1); seed <= 24; seed++ {
		for _, dup := range []bool{false, true} {
			open := func(label string) graph.Store {
				if seed%2 == 0 {
					return graph.New()
				}
				store, err := store_sqlite.Open(filepath.Join(t.TempDir(), label+".sqlite"))
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = store.Close() })
				return store
			}
			scoped, frontierStore := open("scoped"), open("frontier")
			owners := inferenceFrontierFixture(scoped, seed, dup)
			inferenceFrontierFixture(frontierStore, seed, dup)
			rng := rand.New(rand.NewSource(seed * 7))
			frontier := map[string]bool{}
			for _, id := range owners {
				// Every third seed keeps the frontier inside one repository,
				// so parents in the other one are reached only through it.
				if seed%3 == 0 && id[:3] != "r1/" {
					continue
				}
				if rng.Intn(4) == 0 {
					frontier[id] = true
				}
			}
			if dup {
				frontier[owners[1]] = true
			}
			wantImpl := New(scoped).InferImplementsScoped(frontier, frontier)
			wantOver := New(scoped).InferOverridesScoped(frontier)
			gotImpl := New(frontierStore).InferImplementsForFrontier(frontier)
			gotOver := New(frontierStore).InferOverridesForFrontier(frontier)
			if gotImpl != wantImpl || gotOver != wantOver {
				t.Fatalf("seed %d dup %v: added implements/overrides = %d/%d, scoped %d/%d",
					seed, dup, gotImpl, gotOver, wantImpl, wantOver)
			}
			want, got := hierarchyRows(t, scoped), hierarchyRows(t, frontierStore)
			if fmt.Sprint(want) != fmt.Sprint(got) {
				t.Fatalf("seed %d dup %v: rows differ\nscoped:   %v\nfrontier: %v", seed, dup, want, got)
			}
			if seed == 1 && !dup && wantImpl == 0 && wantOver == 0 {
				t.Fatal("the fixture inferred nothing; the comparison is vacuous")
			}
		}
	}
}

// A frontier child whose parent lives in another repository reaches the
// parent's methods only through the parent edge.
func TestOverridesForFrontierReadsTheParentRepositoryMethods(t *testing.T) {
	build := func() *graph.Graph {
		g := graph.New()
		g.AddBatch([]*graph.Node{
			{ID: "r1/c.go::C", Kind: graph.KindType, Name: "C", RepoPrefix: "r1"},
			{ID: "r1/c.go::C.M", Kind: graph.KindMethod, Name: "M", FilePath: "r1/c.go", RepoPrefix: "r1"},
			{ID: "r2/p.go::P", Kind: graph.KindType, Name: "P", RepoPrefix: "r2"},
			{ID: "r2/p.go::P.M", Kind: graph.KindMethod, Name: "M", FilePath: "r2/p.go", RepoPrefix: "r2"},
		}, []*graph.Edge{
			{From: "r1/c.go::C.M", To: "r1/c.go::C", Kind: graph.EdgeMemberOf},
			{From: "r2/p.go::P.M", To: "r2/p.go::P", Kind: graph.EdgeMemberOf},
			{From: "r1/c.go::C", To: "r2/p.go::P", Kind: graph.EdgeExtends, Origin: graph.OriginASTResolved},
		})
		return g
	}
	frontier := map[string]bool{"r1/c.go::C": true}
	scoped, fromFrontier := build(), build()
	want := New(scoped).InferOverridesScoped(frontier)
	got := New(fromFrontier).InferOverridesForFrontier(frontier)
	if want != 1 || got != want {
		t.Fatalf("overrides added: frontier %d, scoped %d, want 1", got, want)
	}
	if fmt.Sprint(hierarchyRows(t, scoped)) != fmt.Sprint(hierarchyRows(t, fromFrontier)) {
		t.Fatalf("rows differ:\n%v\n%v", hierarchyRows(t, scoped), hierarchyRows(t, fromFrontier))
	}
}
