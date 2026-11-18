package resolver

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func TestCrossRepoPreservesGoPackageGuardAcrossWithdrawalAndRestore(t *testing.T) {
	for name, open := range map[string]func(*testing.T) graph.Store{
		"memory": func(*testing.T) graph.Store { return graph.New() },
		"sqlite": openIdentitySQLite,
	} {
		t.Run(name, func(t *testing.T) {
			g := open(t)
			callerFile := &graph.Node{ID: "repo/consumer/c.go", Kind: graph.KindFile, FilePath: "repo/consumer/c.go", Language: "go", RepoPrefix: "repo"}
			caller := &graph.Node{ID: callerFile.ID + "::Consume", Kind: graph.KindFunction, Name: "Consume", FilePath: callerFile.ID, Language: "go", RepoPrefix: "repo"}
			producerFile := &graph.Node{ID: "repo/producer/p.go", Kind: graph.KindFile, FilePath: "repo/producer/p.go", Language: "go", RepoPrefix: "repo"}
			producer := &graph.Node{ID: producerFile.ID + "::Produce", Kind: graph.KindFunction, Name: "Produce", FilePath: producerFile.ID, Language: "go", RepoPrefix: "repo"}
			decoyFile := &graph.Node{ID: "repo/decoy/producer/p.go", Kind: graph.KindFile, FilePath: "repo/decoy/producer/p.go", Language: "go", RepoPrefix: "repo"}
			decoy := &graph.Node{ID: decoyFile.ID + "::Produce", Kind: graph.KindFunction, Name: "Produce", FilePath: decoyFile.ID, Language: "go", RepoPrefix: "repo"}
			foreign := &graph.Node{ID: "other/producer/p.go::Produce", Kind: graph.KindFunction, Name: "Produce", FilePath: "other/producer/p.go", Language: "go", RepoPrefix: "other"}
			importEdge := &graph.Edge{From: callerFile.ID, To: producerFile.ID, Kind: graph.EdgeImports, FilePath: callerFile.ID, Line: 3}
			g.AddBatch([]*graph.Node{callerFile, caller, producerFile, producer, decoyFile, decoy, foreign}, []*graph.Edge{
				importEdge,
				{From: caller.ID, To: producer.ID, Kind: graph.EdgeCalls, FilePath: callerFile.ID, Line: 5},
			})
			// The real definition is withdrawn. Preserve the caller's edge and
			// genuine import the way the indexer's restub/carry path does.
			call := storedOutEdge(t, g, caller.ID, graph.EdgeCalls)
			oldTo := call.To
			graph.StashRestubProvenance(call)
			call.To = "unresolved::Produce"
			g.ReindexEdges([]graph.EdgeReindex{{Edge: call, OldTo: oldTo}})
			g.EvictFile(producerFile.ID)
			withdrawn := &graph.Node{ID: producerFile.ID + "::Withdrawn", Kind: graph.KindFunction, Name: "Withdrawn", FilePath: producerFile.ID, Language: "go", RepoPrefix: "repo"}
			g.AddBatch([]*graph.Node{producerFile, withdrawn}, []*graph.Edge{importEdge})
			r := scopedResolver(t, g)
			r.SetPriorDeclarations(map[string]DeclarationSurface{producerFile.ID: DeclarationSurfaceOf([]*graph.Node{producer})})
			r.ResolveFilesAndIncoming([]string{producerFile.ID})
			r.SetPriorDeclarations(nil)
			call = storedOutEdge(t, g, caller.ID, graph.EdgeCalls)
			require.Equal(t, "unresolved::Produce", call.To, "the per-repo pass refuses the unrelated package")
			require.Equal(t, true, call.Meta["guard_reverted"])
			cr := NewCrossRepo(g)
			cr.ResolveAll()
			call = storedOutEdge(t, g, caller.ID, graph.EdgeCalls)
			require.Equal(t, "unresolved::Produce", call.To, "cross-repo fallback must not resurrect the refused same-repo decoy")
			require.Empty(t, call.Origin)
			require.Zero(t, call.Confidence)
			require.False(t, call.CrossRepo, "an unimported repository is not a substitute for the withdrawn package")

			g.AddNode(producer)
			// Run the cross-repo path directly while the reference is still
			// unresolved and marked: the positive cannot be supplied by an
			// earlier per-repo rebind.
			cr.ResolveAll()
			call = storedOutEdge(t, g, caller.ID, graph.EdgeCalls)
			require.Equal(t, producer.ID, call.To, "restoration must select the actually imported package, regardless of candidate order")
			require.False(t, call.CrossRepo)
		})
	}
}

func TestCrossRepoGuardedGoSamePackageCallRemainsEligible(t *testing.T) {
	for _, test := range []struct {
		name, callerPath, targetPath, repo string
		imported                           bool
	}{
		{"prefixed", "repo/pkg/c.go", "repo/pkg/p.go", "repo", false},
		{"root", "c.go", "p.go", "", false},
		{"unprefixed_import", "consumer/c.go", "producer/p.go", "", true},
		{"unprefixed_root_import", "consumer/c.go", "p.go", "", true},
		{"missing_placement", "c.go", "", "", false},
		{"missing_caller_placement", "", "p.go", "", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			g := graph.New()
			caller := &graph.Node{ID: test.callerPath + "::Call", Kind: graph.KindFunction, Name: "Call", FilePath: test.callerPath, Language: "go", RepoPrefix: test.repo}
			target := &graph.Node{ID: test.targetPath + "::Produce", Kind: graph.KindFunction, Name: "Produce", FilePath: test.targetPath, Language: "go", RepoPrefix: test.repo}
			g.AddBatch([]*graph.Node{caller, target}, []*graph.Edge{{From: caller.ID, To: "unresolved::Produce", Kind: graph.EdgeCalls, FilePath: caller.FilePath, Meta: map[string]any{"guard_reverted": true}}})
			if test.imported {
				wireImport(g, caller.FilePath, test.repo, target.FilePath)
			}
			NewCrossRepo(g).ResolveAll()
			want := target.ID
			if test.callerPath == "" || test.targetPath == "" {
				want = "unresolved::Produce"
			}
			require.Equal(t, want, storedOutEdge(t, g, caller.ID, graph.EdgeCalls).To)
		})
	}
}

// The package guard runs after heuristic placeholder-source reconciliation.
// Whole and incremental resolution must retract only the refused call site's
// returned-value source, preserving a legitimate source at a different site.
func TestPackageGuardRevertsDataflowSourceAtTheRejectedCallSite(t *testing.T) {
	for backend, open := range map[string]func(*testing.T) graph.Store{
		"memory": func(*testing.T) graph.Store { return graph.New() },
		"sqlite": openIdentitySQLite,
	} {
		for _, scope := range []string{"whole", "incoming"} {
			t.Run(backend+"/"+scope, func(t *testing.T) {
				t.Setenv("GORTEX_RESOLVE_FROM_RECONCILE", "")
				g := open(t)
				caller := "pkg/a/a.go::Caller"
				file := "pkg/a/a.go"
				target := "pkg/b/b.go::Target"
				call := &graph.Edge{From: caller, To: "unresolved::Target", Kind: graph.EdgeCalls, FilePath: file, Line: 5}
				guardFixture(t, g, call)
				g.AddBatch(nil, []*graph.Edge{
					{From: "unresolved::Target", To: caller, Kind: graph.EdgeValueFlow, FilePath: file, Line: 5},
					{From: target, To: caller, Kind: graph.EdgeValueFlow, FilePath: file, Line: 9},
					{From: caller, To: "unresolved::Other", Kind: graph.EdgeCalls, FilePath: file, Line: 6},
					{From: "unresolved::Other", To: caller, Kind: graph.EdgeValueFlow, FilePath: file, Line: 6},
				})
				r := scopedResolver(t, g)
				if scope == "whole" {
					_, err := r.ResolveAllContext(t.Context())
					require.NoError(t, err)
				} else {
					r.ResolveFilesAndIncoming([]string{"pkg/b/b.go", "pkg/c/c.go"})
				}
				var callSeen, refusedFlow, validFlow, otherSite bool
				for _, edge := range g.GetOutEdges(caller) {
					if edge.Kind == graph.EdgeCalls && edge.Line == 5 {
						require.Equal(t, "unresolved::Target", edge.To)
						require.Equal(t, true, edge.Meta["guard_reverted"])
						callSeen = true
					}
				}
				for _, edge := range g.GetInEdges(caller) {
					if edge.Kind != graph.EdgeValueFlow {
						continue
					}
					switch edge.Line {
					case 5:
						require.Equal(t, "unresolved::Target", edge.From, "a rejected call cannot lend its guessed value producer")
						refusedFlow = true
					case 6:
						require.Equal(t, "pkg/c/c.go::Other", edge.From, "an actually imported call still lifts its returned-value source")
						validFlow = true
					case 9:
						require.Equal(t, target, edge.From, "retraction is exact-site, not target-wide")
						otherSite = true
					}
				}
				require.True(t, callSeen)
				require.True(t, refusedFlow)
				require.True(t, validFlow)
				require.True(t, otherSite)
			})
		}
	}
}
