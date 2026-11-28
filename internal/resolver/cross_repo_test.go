package resolver

import (
	"iter"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"pgregory.net/rapid"
)

// --- Unit tests for CrossRepoResolver (Task 7.1) ---

// wireImport adds a resolved EdgeImports edge from callerFile into a
// file node in targetRepo, plus the target file node itself. This is
// the import-reachability *evidence* CrossRepoResolver now requires
// before it will resolve a name-only call across a repo boundary —
// without it, a bare name like `Helper` could land on any repo that
// happens to define a `Helper`, which is the exact name-collision
// false-positive class this guards against.
func wireImport(g graph.Store, callerFile, targetRepo, targetFile string) {
	g.AddNode(&graph.Node{
		ID: targetFile, Kind: graph.KindFile, Name: targetFile,
		FilePath: targetFile, Language: "go", RepoPrefix: targetRepo,
	})
	g.AddEdge(&graph.Edge{
		From: callerFile, To: targetFile,
		Kind: graph.EdgeImports, FilePath: callerFile, Line: 1,
	})
}

func TestCrossRepoResolveAll_SameRepoPreferred(t *testing.T) {
	g := graph.New()

	// Repo A: caller and a target function.
	g.AddNode(&graph.Node{ID: "repoA/pkg/a.go::Caller", Kind: graph.KindFunction, Name: "Caller", FilePath: "repoA/pkg/a.go", Language: "go", RepoPrefix: "repoA"})
	g.AddNode(&graph.Node{ID: "repoA/pkg/b.go::Helper", Kind: graph.KindFunction, Name: "Helper", FilePath: "repoA/pkg/b.go", Language: "go", RepoPrefix: "repoA"})

	// Repo B: same-named function.
	g.AddNode(&graph.Node{ID: "repoB/lib/c.go::Helper", Kind: graph.KindFunction, Name: "Helper", FilePath: "repoB/lib/c.go", Language: "go", RepoPrefix: "repoB"})

	edge := &graph.Edge{From: "repoA/pkg/a.go::Caller", To: "unresolved::Helper", Kind: graph.EdgeCalls, FilePath: "repoA/pkg/a.go", Line: 5}
	g.AddEdge(edge)

	cr := NewCrossRepo(g)
	stats := cr.ResolveAll()

	assert.Equal(t, 1, stats.Resolved)
	assert.Equal(t, 0, stats.CrossRepoEdges)
	assert.Equal(t, "repoA/pkg/b.go::Helper", edge.To)
	assert.False(t, edge.CrossRepo)
}

// With an import edge proving repoA reaches repoB, the cross-repo
// fallback resolves — this is the legitimate cross-repo call case.
func TestCrossRepoResolveAll_CrossRepoFallback(t *testing.T) {
	g := graph.New()

	// Repo A: caller, no matching function.
	g.AddNode(&graph.Node{ID: "repoA/pkg/a.go::Caller", Kind: graph.KindFunction, Name: "Caller", FilePath: "repoA/pkg/a.go", Language: "go", RepoPrefix: "repoA"})

	// Repo B: target function.
	g.AddNode(&graph.Node{ID: "repoB/lib/c.go::Helper", Kind: graph.KindFunction, Name: "Helper", FilePath: "repoB/lib/c.go", Language: "go", RepoPrefix: "repoB"})

	// Evidence: repoA's caller file imports repoB.
	wireImport(g, "repoA/pkg/a.go", "repoB", "repoB/lib/c.go")

	edge := &graph.Edge{From: "repoA/pkg/a.go::Caller", To: "unresolved::Helper", Kind: graph.EdgeCalls, FilePath: "repoA/pkg/a.go", Line: 5}
	g.AddEdge(edge)

	cr := NewCrossRepo(g)
	stats := cr.ResolveAll()

	assert.Equal(t, 1, stats.Resolved)
	assert.Equal(t, 1, stats.CrossRepoEdges)
	assert.Equal(t, "repoB/lib/c.go::Helper", edge.To)
	assert.True(t, edge.CrossRepo)
	assert.Equal(t, 1, stats.ByRepo["repoB"])
}

// Without an import edge, the SAME graph must NOT resolve the call
// across the repo boundary — it stays unresolved. This is the
// regression guard for the M3 false-positive class: a name-only match
// in a repo the caller never imports is never selected.
func TestCrossRepoResolveAll_RefusesUnreachableCrossRepo(t *testing.T) {
	g := graph.New()

	g.AddNode(&graph.Node{ID: "repoA/pkg/a.go::Caller", Kind: graph.KindFunction, Name: "Caller", FilePath: "repoA/pkg/a.go", Language: "go", RepoPrefix: "repoA"})
	g.AddNode(&graph.Node{ID: "repoB/lib/c.go::Helper", Kind: graph.KindFunction, Name: "Helper", FilePath: "repoB/lib/c.go", Language: "go", RepoPrefix: "repoB"})

	edge := &graph.Edge{From: "repoA/pkg/a.go::Caller", To: "unresolved::Helper", Kind: graph.EdgeCalls, FilePath: "repoA/pkg/a.go", Line: 5}
	g.AddEdge(edge)

	cr := NewCrossRepo(g)
	stats := cr.ResolveAll()

	assert.Equal(t, 0, stats.Resolved)
	assert.Equal(t, 0, stats.CrossRepoEdges)
	assert.Equal(t, "unresolved::Helper", edge.To, "no import edge → must stay unresolved")
	assert.False(t, edge.CrossRepo)
}

func TestCrossRepoResolveAll_Unresolvable(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "repoA/a.go::Foo", Kind: graph.KindFunction, Name: "Foo", FilePath: "repoA/a.go", Language: "go", RepoPrefix: "repoA"})

	edge := &graph.Edge{From: "repoA/a.go::Foo", To: "unresolved::NonExistent", Kind: graph.EdgeCalls, FilePath: "repoA/a.go", Line: 5}
	g.AddEdge(edge)

	cr := NewCrossRepo(g)
	stats := cr.ResolveAll()

	assert.Equal(t, 0, stats.Resolved)
	assert.Equal(t, 1, stats.Unresolved)
}

func TestCrossRepoResolveAll_ImportCrossRepo(t *testing.T) {
	g := graph.New()

	g.AddNode(&graph.Node{ID: "repoA/main.go", Kind: graph.KindFile, Name: "main.go", FilePath: "repoA/main.go", Language: "go", RepoPrefix: "repoA"})
	g.AddNode(&graph.Node{ID: "repoB/utils/utils.go", Kind: graph.KindPackage, Name: "utils", QualName: "utils", FilePath: "repoB/utils/utils.go", Language: "go", RepoPrefix: "repoB"})

	edge := &graph.Edge{From: "repoA/main.go", To: "unresolved::import::utils", Kind: graph.EdgeImports, FilePath: "repoA/main.go", Line: 3}
	g.AddEdge(edge)

	cr := NewCrossRepo(g)
	stats := cr.ResolveAll()

	assert.Equal(t, 1, stats.Resolved)
	assert.Equal(t, 1, stats.CrossRepoEdges)
	assert.Equal(t, "repoB/utils/utils.go", edge.To)
	assert.True(t, edge.CrossRepo)
}

func TestCrossRepoResolveAll_ImportQualNamePrefersSameWorkspaceOverEligibleForeign(t *testing.T) {
	for _, tc := range []struct {
		name    string
		reverse bool
	}{
		{name: "forward_insert"},
		{name: "reverse_insert", reverse: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := graph.New()
			const qualName = "github.com/acme/shared"

			caller := &graph.Node{ID: "consumer/main.go", Kind: graph.KindFile, Name: "main.go", FilePath: "consumer/main.go", Language: "go", RepoPrefix: "consumer", WorkspaceID: "consumer-ws"}
			sameWorkspace := &graph.Node{ID: "z-local/shared.go::shared", Kind: graph.KindPackage, Name: "shared", QualName: qualName, FilePath: "z-local/shared.go", Language: "go", RepoPrefix: "z-local", WorkspaceID: "consumer-ws"}
			eligibleForeign := &graph.Node{ID: "a-foreign/shared.go::shared", Kind: graph.KindPackage, Name: "shared", QualName: qualName, FilePath: "a-foreign/shared.go", Language: "go", RepoPrefix: "a-foreign", WorkspaceID: "foreign-ws"}

			g.AddNode(caller)
			candidates := []*graph.Node{eligibleForeign, sameWorkspace}
			if tc.reverse {
				candidates[0], candidates[1] = candidates[1], candidates[0]
			}
			for _, candidate := range candidates {
				g.AddNode(candidate)
			}

			edge := &graph.Edge{From: caller.ID, To: "unresolved::import::" + qualName, Kind: graph.EdgeImports, FilePath: caller.FilePath, Line: 3}
			g.AddEdge(edge)

			cr := NewCrossRepo(g)
			cr.SetCrossWorkspaceDepLookup(func(sourceWorkspaceID string) []CrossWorkspaceDepRule {
				if sourceWorkspaceID != "consumer-ws" {
					return nil
				}
				return []CrossWorkspaceDepRule{{Workspace: "foreign-ws", Modules: []string{qualName}}}
			})
			stats := cr.ResolveAll()

			require.Equal(t, 1, stats.Resolved)
			assert.Equal(t, sameWorkspace.ID, edge.To)
			assert.True(t, edge.CrossRepo)
			assert.Equal(t, 1, stats.CrossRepoEdges)
		})
	}
}

func TestCrossRepoResolveAll_ImportQualNameLeavesEquivalentForeignCandidatesUnresolved(t *testing.T) {
	for _, tc := range []struct {
		name    string
		reverse bool
	}{
		{name: "forward_insert"},
		{name: "reverse_insert", reverse: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := graph.New()
			const qualName = "github.com/acme/shared"

			caller := &graph.Node{ID: "consumer/main.go", Kind: graph.KindFile, Name: "main.go", FilePath: "consumer/main.go", Language: "go", RepoPrefix: "consumer", WorkspaceID: "consumer-ws"}
			candidateA := &graph.Node{ID: "a-foreign/shared.go::shared", Kind: graph.KindPackage, Name: "shared", QualName: qualName, FilePath: "a-foreign/shared.go", Language: "go", RepoPrefix: "a-foreign", WorkspaceID: "foreign-ws"}
			candidateZ := &graph.Node{ID: "z-foreign/shared.go::shared", Kind: graph.KindPackage, Name: "shared", QualName: qualName, FilePath: "z-foreign/shared.go", Language: "go", RepoPrefix: "z-foreign", WorkspaceID: "foreign-ws"}

			g.AddNode(caller)
			candidates := []*graph.Node{candidateA, candidateZ}
			if tc.reverse {
				candidates[0], candidates[1] = candidates[1], candidates[0]
			}
			for _, candidate := range candidates {
				g.AddNode(candidate)
			}

			unresolvedID := "unresolved::import::" + qualName
			edge := &graph.Edge{From: caller.ID, To: unresolvedID, Kind: graph.EdgeImports, FilePath: caller.FilePath, Line: 3}
			g.AddEdge(edge)

			cr := NewCrossRepo(g)
			cr.SetCrossWorkspaceDepLookup(func(sourceWorkspaceID string) []CrossWorkspaceDepRule {
				if sourceWorkspaceID != "consumer-ws" {
					return nil
				}
				return []CrossWorkspaceDepRule{{Workspace: "foreign-ws", Modules: []string{qualName}}}
			})
			stats := cr.ResolveAll()

			require.Equal(t, 0, stats.Resolved)
			assert.Equal(t, unresolvedID, edge.To)
			assert.False(t, edge.CrossRepo)
			assert.Equal(t, 0, stats.CrossRepoEdges)
		})
	}
}

func TestCrossRepoResolveAll_MethodCrossRepo(t *testing.T) {
	g := graph.New()

	g.AddNode(&graph.Node{ID: "repoA/pkg/a.go::Caller", Kind: graph.KindFunction, Name: "Caller", FilePath: "repoA/pkg/a.go", Language: "go", RepoPrefix: "repoA"})
	g.AddNode(&graph.Node{ID: "repoB/lib/b.go::Server.Start", Kind: graph.KindMethod, Name: "Start", FilePath: "repoB/lib/b.go", Language: "go", RepoPrefix: "repoB"})

	// Evidence: repoA's caller file imports repoB.
	wireImport(g, "repoA/pkg/a.go", "repoB", "repoB/lib/b.go")

	edge := &graph.Edge{From: "repoA/pkg/a.go::Caller", To: "unresolved::*.Start", Kind: graph.EdgeCalls, FilePath: "repoA/pkg/a.go", Line: 10}
	g.AddEdge(edge)

	cr := NewCrossRepo(g)
	stats := cr.ResolveAll()

	assert.Equal(t, 1, stats.Resolved)
	assert.Equal(t, 1, stats.CrossRepoEdges)
	assert.Equal(t, "repoB/lib/b.go::Server.Start", edge.To)
	assert.True(t, edge.CrossRepo)
}

// A method call into a repo the caller never imports must stay
// unresolved — the receiver-type name alone is not evidence the call
// crosses to that particular repo.
func TestCrossRepoResolveAll_RefusesUnreachableMethodCall(t *testing.T) {
	g := graph.New()

	g.AddNode(&graph.Node{ID: "repoA/pkg/a.go::Caller", Kind: graph.KindFunction, Name: "Caller", FilePath: "repoA/pkg/a.go", Language: "go", RepoPrefix: "repoA"})
	g.AddNode(&graph.Node{ID: "repoB/lib/b.go::Server.Start", Kind: graph.KindMethod, Name: "Start", FilePath: "repoB/lib/b.go", Language: "go", RepoPrefix: "repoB"})

	edge := &graph.Edge{From: "repoA/pkg/a.go::Caller", To: "unresolved::*.Start", Kind: graph.EdgeCalls, FilePath: "repoA/pkg/a.go", Line: 10}
	g.AddEdge(edge)

	cr := NewCrossRepo(g)
	stats := cr.ResolveAll()

	assert.Equal(t, 0, stats.Resolved)
	assert.Equal(t, "unresolved::*.Start", edge.To)
	assert.False(t, edge.CrossRepo)
}

func TestCrossRepoResolveForRepo(t *testing.T) {
	g := graph.New()

	// Repo A: caller with unresolved edge.
	g.AddNode(&graph.Node{ID: "repoA/a.go::Foo", Kind: graph.KindFunction, Name: "Foo", FilePath: "repoA/a.go", Language: "go", RepoPrefix: "repoA"})
	// Repo B: caller with unresolved edge + target.
	g.AddNode(&graph.Node{ID: "repoB/b.go::Bar", Kind: graph.KindFunction, Name: "Bar", FilePath: "repoB/b.go", Language: "go", RepoPrefix: "repoB"})
	g.AddNode(&graph.Node{ID: "repoB/b.go::Baz", Kind: graph.KindFunction, Name: "Baz", FilePath: "repoB/b.go", Language: "go", RepoPrefix: "repoB"})

	// Evidence: repoA's file imports repoB.
	wireImport(g, "repoA/a.go", "repoB", "repoB/b.go")

	edgeA := &graph.Edge{From: "repoA/a.go::Foo", To: "unresolved::Baz", Kind: graph.EdgeCalls, FilePath: "repoA/a.go", Line: 5}
	edgeB := &graph.Edge{From: "repoB/b.go::Bar", To: "unresolved::Foo", Kind: graph.EdgeCalls, FilePath: "repoB/b.go", Line: 5}
	g.AddEdge(edgeA)
	g.AddEdge(edgeB)

	cr := NewCrossRepo(g)

	// Resolve only repoA edges.
	stats := cr.ResolveForRepo("repoA")

	assert.Equal(t, 1, stats.Resolved)
	assert.Equal(t, 1, stats.CrossRepoEdges)
	assert.Equal(t, "repoB/b.go::Baz", edgeA.To)
	assert.True(t, edgeA.CrossRepo)

	// edgeB should still be unresolved.
	assert.Equal(t, "unresolved::Foo", edgeB.To)
}

// --- Property test for Task 7.2 ---

// Feature: multi-repo-support, Property 10: Cross-repo resolution with same-repo preference
func TestPropertyCrossRepoResolutionSameRepoPreference(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		g := graph.New()

		// Generate a function name.
		funcName := "Func" + rapid.StringMatching(`[A-Z][a-z]{2,8}`).Draw(rt, "funcName")

		// Generate two repo prefixes.
		repoA := "repo-" + rapid.StringMatching(`[a-z]{3,6}`).Draw(rt, "repoA")
		repoB := "repo-" + rapid.StringMatching(`[a-z]{3,6}`).Draw(rt, "repoB")
		// Ensure distinct repos.
		if repoA == repoB {
			repoB = repoB + "x"
		}

		// Decide whether the caller's repo has a same-repo match.
		hasSameRepoMatch := rapid.Bool().Draw(rt, "hasSameRepoMatch")

		// Always add the caller in repoA.
		callerFile := repoA + "/src/caller.go"
		callerID := callerFile + "::" + "Caller"
		g.AddNode(&graph.Node{
			ID: callerID, Kind: graph.KindFunction, Name: "Caller",
			FilePath: callerFile, Language: "go", RepoPrefix: repoA,
		})

		// Always add the target in repoB (cross-repo candidate).
		crossRepoTargetID := repoB + "/lib/target.go::" + funcName
		g.AddNode(&graph.Node{
			ID: crossRepoTargetID, Kind: graph.KindFunction, Name: funcName,
			FilePath: repoB + "/lib/target.go", Language: "go", RepoPrefix: repoB,
		})

		// Evidence: the caller's file imports repoB — without this the
		// cross-repo fallback is (correctly) refused.
		wireImport(g, callerFile, repoB, repoB+"/lib/target.go")

		// Optionally add a same-repo target in repoA.
		sameRepoTargetID := repoA + "/src/target.go::" + funcName
		if hasSameRepoMatch {
			g.AddNode(&graph.Node{
				ID: sameRepoTargetID, Kind: graph.KindFunction, Name: funcName,
				FilePath: repoA + "/src/target.go", Language: "go", RepoPrefix: repoA,
			})
		}

		// Add unresolved edge from caller.
		edge := &graph.Edge{
			From: callerID, To: "unresolved::" + funcName,
			Kind: graph.EdgeCalls, FilePath: callerFile, Line: 10,
		}
		g.AddEdge(edge)

		cr := NewCrossRepo(g)
		stats := cr.ResolveAll()

		require.Equal(rt, 1, stats.Resolved, "edge should be resolved")
		require.Equal(rt, 0, stats.Unresolved, "no edges should remain unresolved")

		if hasSameRepoMatch {
			// Same-repo match preferred.
			require.Equal(rt, sameRepoTargetID, edge.To,
				"same-repo match should be preferred")
			require.False(rt, edge.CrossRepo,
				"same-repo edge should not be marked cross-repo")
			require.Equal(rt, 0, stats.CrossRepoEdges,
				"no cross-repo edges when same-repo match exists")
		} else {
			// Cross-repo fallback — eligible because the caller imports repoB.
			require.Equal(rt, crossRepoTargetID, edge.To,
				"cross-repo target should be used when no same-repo match")
			require.True(rt, edge.CrossRepo,
				"cross-repo edge must have CrossRepo == true")
			require.Equal(rt, 1, stats.CrossRepoEdges,
				"one cross-repo edge expected")
			// Target ID should be a Qualified_Node_ID containing the target's RepoPrefix.
			require.Contains(rt, edge.To, repoB+"/",
				"cross-repo target should use Qualified_Node_ID with target RepoPrefix")
		}
	})
}

// TestCrossRepoResolveAll_RefusesCrossWorkspaceNameCollision is the
// end-to-end regression guard for the M3 false-positive report: a
// caller in one workspace must never resolve a bare-name call to a
// same-named symbol in an unrelated workspace it does not import.
func TestCrossRepoResolveAll_RefusesCrossWorkspaceNameCollision(t *testing.T) {
	g := graph.New()

	// Workspace "gortex": a type method named `string`.
	g.AddNode(&graph.Node{ID: "gortex/edge.go::EdgeKind", Kind: graph.KindType, Name: "EdgeKind", FilePath: "gortex/edge.go", Language: "go", RepoPrefix: "gortex", WorkspaceID: "gortex"})
	g.AddNode(&graph.Node{ID: "gortex/edge.go::caller", Kind: graph.KindFunction, Name: "caller", FilePath: "gortex/edge.go", Language: "go", RepoPrefix: "gortex", WorkspaceID: "gortex"})

	// Unrelated workspace "rcd": a method literally named `string`.
	g.AddNode(&graph.Node{ID: "rcd/models/bot.go::BotClass.string", Kind: graph.KindMethod, Name: "string", FilePath: "rcd/models/bot.go", Language: "go", RepoPrefix: "rcd", WorkspaceID: "rcd"})

	// gortex calls something named `string` — no import into rcd.
	edge := &graph.Edge{From: "gortex/edge.go::caller", To: "unresolved::string", Kind: graph.EdgeCalls, FilePath: "gortex/edge.go", Line: 3}
	g.AddEdge(edge)

	cr := NewCrossRepo(g)
	stats := cr.ResolveAll()

	assert.Equal(t, 0, stats.CrossRepoEdges, "must not cross into an unimported, unrelated workspace")
	assert.Equal(t, "unresolved::string", edge.To)
	assert.False(t, edge.CrossRepo)
}

// TestCrossRepoResolveAll_BindsInboundEdgeIntoChangedRepo locks the warm-restart
// completeness guarantee: when an unchanged consumer repo already holds a bare
// unresolved reference and a provider repo later adds the matching definition,
// the whole-graph cross-repo resolve must bind that inbound reference. A pass
// scoped to only the changed provider's OWN out-edges would never see the
// consumer-owned edge, which is why the pre-enrich cross-repo resolve stays
// whole-graph.
func TestCrossRepoResolveAll_BindsInboundEdgeIntoChangedRepo(t *testing.T) {
	g := graph.New()

	// Unchanged consumer repoA: a call with no local definition — bare unresolved.
	g.AddNode(&graph.Node{ID: "repoA/a.go::Caller", Kind: graph.KindFunction, Name: "Caller", FilePath: "repoA/a.go", Language: "go", RepoPrefix: "repoA"})
	// Changed provider repoB: just added Foo.
	g.AddNode(&graph.Node{ID: "repoB/b.go::Foo", Kind: graph.KindFunction, Name: "Foo", FilePath: "repoB/b.go", Language: "go", RepoPrefix: "repoB"})
	wireImport(g, "repoA/a.go", "repoB", "repoB/b.go")

	inbound := &graph.Edge{From: "repoA/a.go::Caller", To: "unresolved::Foo", Kind: graph.EdgeCalls, FilePath: "repoA/a.go", Line: 5}
	g.AddEdge(inbound)

	// The edge is owned by the unchanged consumer, so it is absent from the
	// changed provider's own out-edge set (graph.GetRepoEdges("repoB")).
	require.Empty(t, g.GetRepoEdges("repoB"),
		"the inbound edge must not appear in the changed provider's own out-edges")

	NewCrossRepo(g).ResolveAll()

	assert.Equal(t, "repoB/b.go::Foo", inbound.To,
		"the whole-graph cross-repo resolve must bind the unchanged consumer's inbound reference into the changed provider")
	assert.True(t, inbound.CrossRepo)
}

type crossRepoPassCountingStore struct {
	graph.Store
	directoryIndexBuilds    int
	dependencyIndexBuilds   int
	reachabilityIndexBuilds int
}

func (s *crossRepoPassCountingStore) FileNodeIdentitiesSeq(repoPrefixes []string) iter.Seq[graph.FileNodeIdentity] {
	s.directoryIndexBuilds++
	return graph.FileNodeIdentitiesSeq(s.Store, repoPrefixes)
}

func (s *crossRepoPassCountingStore) RepoNodeIdentitiesSeq(
	repoPrefixes []string,
	kinds ...graph.NodeKind,
) iter.Seq[graph.RepoNodeIdentity] {
	s.dependencyIndexBuilds++
	return graph.RepoNodeIdentitiesSeq(s.Store, repoPrefixes, kinds...)
}

func (s *crossRepoPassCountingStore) EdgesLightSeq(kinds ...graph.EdgeKind) iter.Seq[*graph.Edge] {
	if len(kinds) == 1 && kinds[0] == graph.EdgeImports {
		s.reachabilityIndexBuilds++
	}
	return graph.EdgesLightSeq(s.Store, kinds...)
}

func TestCrossRepoResolveFilesAndIncoming_BatchesBothDirectionsWithOneIndexBuild(t *testing.T) {
	g := &crossRepoPassCountingStore{Store: graph.New()}
	const (
		changedCallerFile   = "repoA/pkg/caller.go"
		outgoingTargetFile  = "repoB/lib/helper.go"
		incomingCallerFile  = "repoC/pkg/caller.go"
		changedProviderFile = "repoD/lib/added.go"
	)

	g.AddNode(&graph.Node{
		ID: "repoA/pkg/caller.go::Caller", Kind: graph.KindFunction, Name: "Caller",
		FilePath: changedCallerFile, Language: "go", RepoPrefix: "repoA",
	})
	g.AddNode(&graph.Node{
		ID: "repoB/lib/helper.go::Helper", Kind: graph.KindFunction, Name: "Helper",
		FilePath: outgoingTargetFile, Language: "go", RepoPrefix: "repoB",
	})
	wireImport(g, changedCallerFile, "repoB", outgoingTargetFile)
	outgoing := &graph.Edge{
		From: "repoA/pkg/caller.go::Caller", To: "unresolved::Helper",
		Kind: graph.EdgeCalls, FilePath: changedCallerFile, Line: 7,
	}
	g.AddEdge(outgoing)

	g.AddNode(&graph.Node{
		ID: "repoC/pkg/caller.go::Caller", Kind: graph.KindFunction, Name: "Caller",
		FilePath: incomingCallerFile, Language: "go", RepoPrefix: "repoC",
	})
	g.AddNode(&graph.Node{
		ID: "repoD/lib/added.go::Added", Kind: graph.KindFunction, Name: "Added",
		FilePath: changedProviderFile, Language: "go", RepoPrefix: "repoD",
	})
	wireImport(g, incomingCallerFile, "repoD", changedProviderFile)
	incoming := &graph.Edge{
		From: "repoC/pkg/caller.go::Caller", To: "repoD::unresolved::Added",
		Kind: graph.EdgeCalls, FilePath: incomingCallerFile, Line: 11,
	}
	g.AddEdge(incoming)

	cr := NewCrossRepo(g)
	emptyStats := cr.ResolveFilesAndIncoming([]string{"missing.go"})
	require.Zero(t, emptyStats.Resolved)
	require.Zero(t, g.directoryIndexBuilds)
	require.Zero(t, g.dependencyIndexBuilds)
	require.Zero(t, g.reachabilityIndexBuilds)

	stats := cr.ResolveFilesAndIncoming([]string{
		changedProviderFile,
		changedCallerFile,
		changedCallerFile,
	})

	require.Equal(t, 2, stats.Resolved)
	require.Equal(t, 2, stats.CrossRepoEdges)
	assert.Equal(t, "repoB/lib/helper.go::Helper", outgoing.To)
	assert.True(t, outgoing.CrossRepo)
	assert.Equal(t, "repoD/lib/added.go::Added", incoming.To)
	assert.True(t, incoming.CrossRepo)
	require.Equal(t, 1, g.directoryIndexBuilds)
	require.Equal(t, 1, g.dependencyIndexBuilds)
	require.Equal(t, 1, g.reachabilityIndexBuilds)
}

// The cross-repo name-only fallback never lands a Python attribute call on an
// object (`d.get()`, marked py_object_receiver by the extractor) on a free
// function; unmarked calls — a module-qualified Python call, Go's
// `pkg.Func()` — keep binding as before.
func TestCrossRepoResolveAll_PythonObjectMethodCallSkipsFunctions(t *testing.T) {
	cases := []struct {
		name, ext, lang string
		meta            map[string]any
		bind            bool
	}{
		{"python object receiver", ".py", "python", map[string]any{"py_object_receiver": true}, false},
		{"python module receiver", ".py", "python", nil, true},
		{"go package call", ".go", "go", nil, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := graph.New()
			caller, target := "repoA/pkg/b"+tc.ext, "repoA/pkg/a"+tc.ext+"::get"
			g.AddNode(&graph.Node{ID: caller + "::use", Kind: graph.KindFunction, Name: "use", FilePath: caller, Language: tc.lang, RepoPrefix: "repoA"})
			g.AddNode(&graph.Node{ID: target, Kind: graph.KindFunction, Name: "get", FilePath: "repoA/pkg/a" + tc.ext, Language: tc.lang, RepoPrefix: "repoA"})
			edge := &graph.Edge{From: caller + "::use", To: "unresolved::*.get", Kind: graph.EdgeCalls, FilePath: caller, Line: 2, Meta: tc.meta}
			g.AddEdge(edge)

			NewCrossRepo(g).ResolveAll()
			want := "unresolved::*.get"
			if tc.bind {
				want = target
			}
			assert.Equal(t, want, edge.To)
		})
	}
}
