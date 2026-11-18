package indexer

import (
	"iter"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/graph"
)

// contractHydrationProbe preserves the graph's projection capabilities while
// observing the two repository-wide projections used to restore a registry.
type contractHydrationProbe struct {
	*graph.Graph
	ownerReads, ownerRows, scalarReads, scalarRows int
}

func (p *contractHydrationProbe) RepoEdgesByKinds(repos []string, kinds []graph.EdgeKind) []graph.RepoEdgeRow {
	rows := p.Graph.RepoEdgesByKinds(repos, kinds)
	if len(kinds) == 2 && ((kinds[0] == graph.EdgeProvides && kinds[1] == graph.EdgeConsumes) ||
		(kinds[1] == graph.EdgeProvides && kinds[0] == graph.EdgeConsumes)) {
		p.ownerReads++
		p.ownerRows += len(rows)
	}
	return rows
}

func (p *contractHydrationProbe) NodesInScopeSeq(repos, files []string, kinds ...graph.NodeKind) iter.Seq[*graph.Node] {
	inner := graph.NodesInScopeSeq(p.Graph, repos, files, kinds...)
	if len(files) != 0 || len(kinds) != 1 || kinds[0] != graph.KindContract {
		return inner
	}
	p.scalarReads++
	return func(yield func(*graph.Node) bool) {
		for node := range inner {
			p.scalarRows++
			if !yield(node) {
				return
			}
		}
	}
}

func (p *contractHydrationProbe) NodesLightInScopeSeq(repos, files []string) iter.Seq[*graph.Node] {
	return graph.NodesLightInScopeSeq(p.Graph, repos, files)
}

func (p *contractHydrationProbe) EdgesInScopeSeq(repos, files []string, kinds ...graph.EdgeKind) iter.Seq[graph.ScopedEdgeRow] {
	return graph.EdgesInScopeSeq(p.Graph, repos, files, kinds...)
}

func TestContractFreeGoEditPreservesUnrelatedOwners(t *testing.T) {
	for _, edit := range []struct {
		name, source string
	}{
		{"return_constant", "package fixture\n\nfunc Value() int { return 2 }\n"},
		{"comment", "package fixture\n\n// Value supplies the local default.\nfunc Value() int { return 1 }\n"},
	} {
		t.Run(edit.name, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "value.go")
			writeFile(t, path, "package fixture\n\nfunc Value() int { return 1 }\n")
			writeFile(t, filepath.Join(root, "owners.go"), "package fixture\n\nfunc Provider() {}\nfunc Consumer() {}\n")
			g := graph.New()
			initial := newTestIndexer(g)
			initial.SetRepoPrefix("fixture")
			t.Cleanup(initial.Close)
			_, err := initial.Index(root)
			require.NoError(t, err)
			initial.Close()

			provider := g.FindNodesByNameInRepo("Provider", "fixture")
			consumer := g.FindNodesByNameInRepo("Consumer", "fixture")
			require.Len(t, provider, 1)
			require.Len(t, consumer, 1)
			owners := []contracts.Contract{
				{ID: "shared-unrelated-contract", Type: contracts.ContractHTTP, Role: contracts.RoleProvider,
					SymbolID: provider[0].ID, FilePath: provider[0].FilePath, RepoPrefix: "fixture",
					WorkspaceID: "workspace", ProjectID: "project", Line: 3, Confidence: 0.8,
					Meta: map[string]any{"method": "GET", "path": "/unrelated"}},
				{ID: "shared-unrelated-contract", Type: contracts.ContractHTTP, Role: contracts.RoleConsumer,
					SymbolID: consumer[0].ID, FilePath: consumer[0].FilePath, RepoPrefix: "fixture",
					WorkspaceID: "workspace", ProjectID: "project", Line: 4, Confidence: 0.9,
					Meta: map[string]any{"method": "GET", "path": "/unrelated", "client": "local"}},
			}
			nodes, edges, missing := contractGraphRows(g, owners, true)
			require.Zero(t, missing)
			g.AddBatch(nodes, edges)
			before := contracts.LoadRegistryFromGraphWithScope(g, "fixture", "workspace", "project").ByRepo("fixture")
			require.True(t, contractSetsEqual(owners, before), "fixture must retain both complete shared-ID owner records")

			probe := &contractHydrationProbe{Graph: g}
			idx := newTestIndexer(probe)
			idx.SetRepoPrefix("fixture")
			idx.SetWorkspaceID("workspace")
			idx.SetProjectID("project")
			idx.storeRootPath(root)
			t.Cleanup(idx.Close)
			require.Nil(t, idx.contractRegistry, "exercise a cold nonempty incremental batch")
			writeFile(t, path, edit.source)
			require.NoError(t, idx.IndexFile(path))

			after := contracts.LoadRegistryFromGraphWithScope(g, "fixture", "workspace", "project").ByRepo("fixture")
			require.True(t, contractSetsEqual(before, after), "a contract-free edit must preserve every unrelated owner payload")
			require.Empty(t, contracts.LoadRegistryFromGraph(g, "fixture").ByFile("fixture/value.go"))
			require.Len(t, g.FindNodesByNameInRepo("Value", "fixture"), 1)
			require.Zero(t, probe.ownerReads, "ordinary contract-free edits must not load repository ownership")
			require.Zero(t, probe.scalarReads, "ordinary contract-free edits must not project repository contract nodes")
			require.Nil(t, idx.contractRegistry, "a local proof must not masquerade as a complete cached registry")
			require.Equal(t, 1, idx.contractUnchangedFiles)
			t.Logf("whole_repo_owner_reads=%d owner_rows=%d whole_repo_scalar_reads=%d scalar_rows=%d registry_hydrated=%t registry_load=%s",
				probe.ownerReads, probe.ownerRows, probe.scalarReads, probe.scalarRows,
				idx.contractRegistry != nil, idx.contractRegistryLoad)
		})
	}
}
