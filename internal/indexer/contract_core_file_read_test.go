package indexer

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

type contractCoreFileOnlyProbe struct {
	graph.Reader
	nodeCalls, ownershipCalls int
	nodeErr                   error
}

func (r *contractCoreFileOnlyProbe) Unwrap() graph.Reader { return r.Reader }
func (r *contractCoreFileOnlyProbe) LayerContractFileNodesContext(ctx context.Context, repo string, paths []string) (map[string][]*graph.Node, error) {
	r.nodeCalls++
	rows, err := graph.ContractFileNodesContext(ctx, r.Reader, repo, paths)
	if r.nodeErr != nil {
		return rows, r.nodeErr
	}
	return rows, err
}
func (r *contractCoreFileOnlyProbe) LayerContractFileProjectionContext(context.Context, string, []string) (graph.ContractFileProjection, error) {
	r.ownershipCalls++
	return graph.ContractFileProjection{}, graph.ErrContractProjectionUnsupported
}

func TestContractCoreFileReadNeedsNoDerivedOwnershipCapability(t *testing.T) {
	nodes := fileNodeInputs()
	nodes = append(nodes, &graph.Node{ID: "old-contract", Kind: graph.KindContract, FilePath: "repo/handler.go", RepoPrefix: "repo"})
	ordinary := &graph.Edge{From: nodes[1].ID, To: "repo/other.go::Other", Kind: graph.EdgeCalls, FilePath: "repo/handler.go", Meta: map[string]any{"purpose": "ordinary core"}}
	derived := &graph.Edge{From: nodes[1].ID, To: "old-contract", Kind: graph.EdgeHandlesRoute, FilePath: "repo/handler.go"}
	for _, failed := range []bool{false, true} {
		t.Run(map[bool]string{false: "checked_core_only", true: "partial_node_error"}[failed], func(t *testing.T) {
			core := graph.New()
			core.AddBatch(nodes, []*graph.Edge{ordinary, derived})
			reader := &contractCoreFileOnlyProbe{Reader: core}
			if failed {
				reader.nodeErr = context.Canceled
			}
			scratch := newFTSStore(t)
			evidence := &contractFollowupEvidence{Store: scratch, scratch: scratch, ctx: t.Context(), core: reader, guard: &contractReadGuard{open: true}, ledger: newContractUnitLedger(), files: map[string]ContractFollowupFile{"repo/handler.go": {Path: "repo/handler.go", RepoPrefix: "repo"}}, allowedRepos: map[string]bool{"repo": true}}
			evidence.readCore = func(ctx context.Context, file ContractFollowupFile) (ContractFollowupCoreFile, error) {
				return readContractFollowupCoreFile(ctx, reader, file)
			}
			got, edges, err := evidence.loadFile("repo/handler.go")
			require.Equal(t, 1, reader.nodeCalls)
			require.Zero(t, reader.ownershipCalls, "old derived contract ownership must not gate ordinary core evidence")
			if failed {
				require.True(t, errors.Is(err, context.Canceled))
				require.Empty(t, got)
				require.Empty(t, edges)
				require.False(t, evidence.loaded["repo/handler.go"])
				require.Empty(t, scratch.AllNodes())
				return
			}
			require.NoError(t, err)
			require.ElementsMatch(t, nodes[:2], got)
			require.Equal(t, []*graph.Edge{ordinary}, edges, "ordinary outgoing edge metadata remains mandatory; old derived output is excluded")
			for _, node := range got {
				if node.ID == nodes[1].ID {
					node.Meta["signature"] = "private mutation"
				}
			}
			require.NotEqual(t, "private mutation", nodes[1].Meta["signature"], "selected rows stay detached")
		})
	}
}
