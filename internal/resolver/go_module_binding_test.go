package resolver

import (
	"context"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

func goModuleBindingCall(t *testing.T, g graph.Store, caller string) *graph.Edge {
	t.Helper()
	var calls []*graph.Edge
	for _, edge := range g.GetOutEdges(caller) {
		if edge.Kind == graph.EdgeCalls {
			calls = append(calls, edge)
		}
	}
	if len(calls) != 1 {
		t.Fatalf("got %d calls, want exactly one", len(calls))
	}
	return calls[0]
}

func TestGoModuleCrossPathsRejectLooseLocal(t *testing.T) {
	for _, route := range []string{"retry", "deferred", "incoming"} {
		t.Run(route, func(t *testing.T) {
			g := graph.New()
			source := &graph.Node{ID: "a/main.go::Caller", Name: "Caller", Kind: graph.KindFunction, RepoPrefix: "a", FilePath: "a/main.go", Language: "go"}
			decoy := &graph.Node{ID: "a/wrong/lib/lib.go::Hello", Name: "Hello", Kind: graph.KindFunction, RepoPrefix: "a", FilePath: "a/wrong/lib/lib.go", Language: "go"}
			source.WorkspaceID = "public"
			provider := &graph.Node{ID: "b/lib.go::Hello", Name: "Hello", Kind: graph.KindFunction, RepoPrefix: "b", FilePath: "b/lib.go", Language: "go", WorkspaceID: "private"}
			g.AddBatch([]*graph.Node{source, decoy, provider}, nil)
			e := &graph.Edge{From: source.ID, To: "dep::example.org/lib::Hello", Kind: graph.EdgeCalls, FilePath: source.FilePath}
			g.AddEdge(e)
			cr := NewCrossRepo(g)
			cr.SetGoModules(map[string]string{"a": "example.org/app", "b": "example.org/lib"})
			cr.SetCrossWorkspaceDepLookup(func(string) []CrossWorkspaceDepRule { return nil })
			switch route {
			case "retry":
				if err := cr.RetryGoModuleDeps(context.Background()); err != nil {
					t.Fatal(err)
				}
			case "deferred":
				if err := cr.ResolveDeferredGoModuleEdges([]*graph.Edge{e}); err != nil {
					t.Fatal(err)
				}
			case "incoming":
				cr.ResolveMutationFrontiers(nil, nil, []string{provider.FilePath})
			}
			if row := goModuleBindingCall(t, g, source.ID); row.To != "dep::example.org/lib::Hello" {
				t.Fatalf("%s did not retain the terminal after ownership rejection: %+v", route, row)
			}
		})
	}
}

func TestGoModuleDedicatedWorktreeTie(t *testing.T) {
	for _, prefixes := range [][]string{{"lib@task", "lib"}, {"lib", "lib@task"}, {"lib@task", "fork", "lib"}, {"lib@task"}} {
		for _, route := range []string{"master", "deferred", "retry"} {
			t.Run(route+"/"+prefixes[0], func(t *testing.T) {
				g := graph.New()
				caller := &graph.Node{ID: "app/main.go::Caller", Name: "Caller", Kind: graph.KindFunction, RepoPrefix: "app", FilePath: "app/main.go", Language: "go"}
				g.AddNode(caller)
				modules := map[string]string{"app": "example.org/app"}
				for _, prefix := range prefixes {
					modules[prefix] = "example.org/lib"
					g.AddNode(&graph.Node{ID: prefix + "/lib.go::Hello", Name: "Hello", Kind: graph.KindFunction, RepoPrefix: prefix, FilePath: prefix + "/lib.go", Language: "go"})
				}
				e := &graph.Edge{From: caller.ID, To: "unresolved::extern::example.org/lib::Hello", Kind: graph.EdgeCalls, FilePath: caller.FilePath}
				g.AddEdge(e)
				if route == "master" {
					r := New(g)
					r.SetGoModules(modules)
					r.ResolveAll()
				} else {
					cr := NewCrossRepo(g)
					cr.SetGoModules(modules)
					if route == "retry" {
						e.To = "dep::example.org/lib::Hello"
						if err := cr.RetryGoModuleDeps(context.Background()); err != nil {
							t.Fatal(err)
						}
					} else {
						if err := cr.ResolveDeferredGoModuleEdges([]*graph.Edge{e}); err != nil {
							t.Fatal(err)
						}
					}
				}
				want := "lib/lib.go::Hello"
				if len(prefixes) == 1 {
					want = "lib@task/lib.go::Hello"
				}
				row := goModuleBindingCall(t, g, caller.ID)
				if len(prefixes) == 3 {
					if !isSyntheticResolveTarget(row.To) {
						t.Fatalf("independent fork must remain ambiguous: %+v", row)
					}
				} else if row.To != want {
					t.Fatalf("got %s want %s", row.To, want)
				}
			})
		}
	}
}

func TestGoModuleOutgoingRefreshesIdentity(t *testing.T) {
	g := graph.New()
	source := &graph.Node{ID: "a/main.go::Caller", Name: "Caller", Kind: graph.KindFunction, RepoPrefix: "a", FilePath: "a/main.go", Language: "go"}
	target := &graph.Node{ID: "b/lib.go::Hello", Name: "Hello", Kind: graph.KindFunction, RepoPrefix: "b", FilePath: "b/lib.go", Language: "go"}
	g.AddBatch([]*graph.Node{source, target}, nil)
	e := &graph.Edge{From: source.ID, To: "unresolved::extern::example.org/lib::Hello", Kind: graph.EdgeCalls, FilePath: source.FilePath}
	g.AddEdge(e)
	r := New(g)
	r.SetGoModuleLookup(func() map[string]string { return map[string]string{"a": "example.org/app", "b": "example.org/lib"} })
	r.ResolveFilesOutgoing([]string{source.FilePath})
	if got := goModuleBindingCall(t, g, source.ID); got.To != target.ID || got.Confidence != 0.6 {
		t.Fatalf("outgoing pass missed refreshed module: %+v", got)
	}
}

type goModuleAdmissionStore struct {
	graph.Store
	edge              *graph.Edge
	refusePreparation bool
	moduleReads       int
}

func (s *goModuleAdmissionStore) GetInEdgesByNodeIDs(ids []string) map[string][]*graph.Edge {
	out := make(map[string][]*graph.Edge)
	for _, id := range ids {
		if id == s.edge.To {
			s.moduleReads++
		}
		if id == s.edge.To && !s.refusePreparation || id == "unresolved::Hello" && s.refusePreparation {
			rows := make([]*graph.Edge, graph.MaxIncomingSourceCandidateRows+1)
			for i := range rows {
				rows[i] = s.edge
			}
			out[id] = rows
		}
	}
	return out
}

func TestGoModuleIncomingAdmissionRefused(t *testing.T) {
	for _, preparation := range []bool{false, true} {
		t.Run(map[bool]string{false: "module_leg", true: "earlier_preparation"}[preparation], func(t *testing.T) {
			source := &graph.Node{ID: "a/main.go::Caller", Name: "Caller", Kind: graph.KindFunction, RepoPrefix: "a", FilePath: "a/main.go", Language: "go"}
			target := &graph.Node{ID: "b/lib.go::Hello", Name: "Hello", Kind: graph.KindFunction, RepoPrefix: "b", FilePath: "b/lib.go", Language: "go"}
			const original = "unresolved::extern::example.org/lib::Hello"
			e := &graph.Edge{From: source.ID, To: original, Kind: graph.EdgeCalls, FilePath: source.FilePath}
			s := &goModuleAdmissionStore{Store: graph.New(), edge: e, refusePreparation: preparation}
			s.AddBatch([]*graph.Node{source, target}, []*graph.Edge{e})
			cr := NewCrossRepo(s)
			modules := map[string]string{"a": "example.org/app", "b": "example.org/lib"}
			cr.SetGoModules(modules)
			// A completed unchanged snapshot excludes the unrelated revision migration.
			cr.SetGoModuleDepRetry(&GoModuleDepRetry{modules: modules, complete: true}, 0)
			var resolutionFiles []string
			if preparation {
				resolutionFiles = []string{target.FilePath}
			}
			_, admission := cr.ResolveMutationFrontiersBounded(resolutionFiles, nil, []string{target.FilePath})
			if !admission.Refused || admission.Refusal == nil {
				t.Fatalf("missing refusal: %+v", admission)
			}
			if preparation && s.moduleReads != 0 {
				t.Fatalf("module incoming read ran after an earlier refusal: %d", s.moduleReads)
			}
			if got := goModuleBindingCall(t, s, source.ID); got.To != original {
				t.Fatalf("refused leg changed edge: %+v", got)
			}
		})
	}
}

func TestGoModuleBindingReconcilesReturnedValue(t *testing.T) {
	for _, terminal := range []string{"unresolved::extern::example.org/lib::Hello", "dep::example.org/lib::Hello", "external-call::dep::example.org/lib"} {
		t.Run(terminal, func(t *testing.T) {
			g := graph.New()
			source := &graph.Node{ID: "a/main.go::Caller", Name: "Caller", Kind: graph.KindFunction, RepoPrefix: "a", FilePath: "a/main.go", Language: "go"}
			target := &graph.Node{ID: "b/lib.go::Hello", Name: "Hello", Kind: graph.KindFunction, RepoPrefix: "b", FilePath: "b/lib.go", Language: "go"}
			e := &graph.Edge{From: source.ID, To: terminal, Kind: graph.EdgeCalls, FilePath: source.FilePath, Line: 3, Meta: map[string]any{"go_import": "example.org/lib::Hello"}}
			flow := &graph.Edge{From: "a/unresolved::Hello", To: source.ID, Kind: graph.EdgeValueFlow, FilePath: source.FilePath, Line: 3}
			unrelated := &graph.Edge{From: "a/unresolved::Hello", To: source.ID, Kind: graph.EdgeValueFlow, FilePath: source.FilePath, Line: 4}
			g.AddBatch([]*graph.Node{source, target}, []*graph.Edge{e, flow, unrelated})
			cr := NewCrossRepo(g)
			cr.SetGoModules(map[string]string{"a": "example.org/app", "b": "example.org/lib"})
			if err := cr.ResolveDeferredGoModuleEdges([]*graph.Edge{e}); err != nil {
				t.Fatal(err)
			}
			if flow.From != target.ID {
				t.Fatalf("rebound call left returned value at %s", flow.From)
			}
			if unrelated.From != "a/unresolved::Hello" {
				t.Fatalf("moved unrelated site: %+v", unrelated)
			}
		})
	}
}
