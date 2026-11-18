package svc

import (
	"context"
	"reflect"
	"sort"
	"testing"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/llm"
	"github.com/zzet/gortex/internal/query"
)

func TestInProcessBackendRepoScopeWithSharedWorkspace(t *testing.T) {
	g := graph.New()
	for _, n := range []*graph.Node{
		{ID: "app/a.go::ScopedTarget", Name: "ScopedTarget", Kind: graph.KindFunction, FilePath: "app/a.go", RepoPrefix: "app", WorkspaceID: "shared", ProjectID: "web"},
		{ID: "app/b.go::ScopedTarget", Name: "ScopedTarget", Kind: graph.KindFunction, FilePath: "app/b.go", RepoPrefix: "app", WorkspaceID: "shared", ProjectID: "worker"},
		{ID: "sibling/a.go::ScopedTarget", Name: "ScopedTarget", Kind: graph.KindFunction, FilePath: "sibling/a.go", RepoPrefix: "sibling", WorkspaceID: "shared", ProjectID: "web"},
	} {
		g.AddNode(n)
	}
	b := NewInProcessBackend(query.NewEngine(g), nil)
	for _, tc := range []struct {
		name  string
		scope llm.Scope
		want  []string
	}{
		{"repo", llm.Scope{Repo: "app"}, []string{"app/a.go::ScopedTarget", "app/b.go::ScopedTarget"}},
		{"repo_project", llm.Scope{Repo: "app", Project: "web"}, []string{"app/a.go::ScopedTarget"}},
		{"missing_repo", llm.Scope{Repo: "missing"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := b.SearchSymbols(context.Background(), "ScopedTarget", tc.scope, 10)
			if err != nil {
				t.Fatal(err)
			}
			var ids []string
			for _, n := range got {
				ids = append(ids, n.ID)
			}
			sort.Strings(ids)
			if !reflect.DeepEqual(ids, tc.want) {
				t.Fatalf("scoped search = %v, want %v", ids, tc.want)
			}
		})
	}
}

func TestInProcessBackendRepoProjectScopeBoundsTraversal(t *testing.T) {
	g := graph.New()
	add := func(id, repo, project string) {
		g.AddNode(&graph.Node{ID: id, Name: id, Kind: graph.KindFunction, FilePath: repo + "/a.go", RepoPrefix: repo, WorkspaceID: "shared", ProjectID: project})
	}
	for _, id := range []string{"root", "dep", "depBeyond", "caller", "callerBeyond"} {
		add(id, "app", "web")
	}
	for _, id := range []string{"depBridge", "callerBridge"} {
		add(id, "app", "worker")
	}
	add("foreign", "sibling", "web")
	for _, e := range [][2]string{{"root", "dep"}, {"root", "depBridge"}, {"depBridge", "depBeyond"}, {"caller", "root"}, {"callerBridge", "root"}, {"callerBeyond", "callerBridge"}, {"foreign", "root"}, {"root", "foreign"}} {
		g.AddEdge(&graph.Edge{From: e[0], To: e[1], Kind: graph.EdgeCalls})
	}
	b := NewInProcessBackend(query.NewEngine(g), nil)
	scope := llm.Scope{Repo: "app", Project: "web"}
	deps, err := b.GetDependencies(context.Background(), "root", scope, 3, 30)
	if err != nil {
		t.Fatal(err)
	}
	var depIDs []string
	for _, n := range deps {
		depIDs = append(depIDs, n.ID)
	}
	sort.Strings(depIDs)
	if !reflect.DeepEqual(depIDs, []string{"dep"}) {
		t.Fatalf("dependencies crossed repo/project boundary: %v", depIDs)
	}
	callers, err := b.GetCallers(context.Background(), "root", scope, 30)
	if err != nil {
		t.Fatal(err)
	}
	var callerIDs []string
	for _, n := range callers {
		callerIDs = append(callerIDs, n.ID)
	}
	sort.Strings(callerIDs)
	if !reflect.DeepEqual(callerIDs, []string{"caller"}) {
		t.Fatalf("callers crossed repo/project boundary: %v", callerIDs)
	}
	for _, seed := range []string{"foreign", "depBridge"} {
		gotDeps, err := b.GetDependencies(context.Background(), seed, scope, 3, 30)
		if err != nil || len(gotDeps) != 0 {
			t.Fatalf("out-of-scope seed %q dependencies=%v err=%v", seed, gotDeps, err)
		}
		gotCallers, err := b.GetCallers(context.Background(), seed, scope, 30)
		if err != nil || len(gotCallers) != 0 {
			t.Fatalf("out-of-scope seed %q callers=%v err=%v", seed, gotCallers, err)
		}
	}
}
