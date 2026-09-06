package store_sqlite_test

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

func scalarBackendFixture(t *testing.T, backend string) graph.Store {
	t.Helper()
	if backend == "memory" {
		return graph.New()
	}
	store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	return store
}

func scalarBackendSnapshot(t *testing.T, value any) string {
	t.Helper()
	if node, ok := value.(*graph.Node); ok && node != nil {
		value = struct {
			Node *graph.Node
			Meta map[string]any
		}{node, node.Meta}
	}
	encoded, err := json.Marshal(value)
	require.NoError(t, err)
	return string(encoded)
}

func TestContractOwnerReplacementRetiresOnlyRemovedScalar(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		for _, sameRepo := range []bool{false, true} {
			name := "cross_repository"
			if sameRepo {
				name = "same_repository"
			}
			t.Run(backend+"/"+name, func(t *testing.T) {
				store := scalarBackendFixture(t, backend)
				repoA, repoB := "a", "b"
				if sameRepo {
					repoB = repoA
				}
				const id = "env::SHARED"
				fileA, fileB := repoA+"/provider.env", repoB+"/consumer.go"
				sourceA := &graph.Node{ID: "source-a", Kind: graph.KindFile, RepoPrefix: repoA, FilePath: fileA}
				sourceB := &graph.Node{ID: "source-b", Kind: graph.KindFunction, RepoPrefix: repoB, FilePath: fileB}
				canonical := &graph.Node{ID: id, Kind: graph.KindContract, RepoPrefix: repoA, FilePath: fileA, WorkspaceID: "workspace-a", ProjectID: "project-a", Meta: map[string]any{"type": "env", "role": "provider", "symbol_id": "", "contract_meta": map[string]any{"var": "SHARED", "nested": []any{"a", true}}}}
				edgeA := &graph.Edge{From: sourceA.ID, To: id, Kind: graph.EdgeProvides, FilePath: fileA, Line: 1}
				edgeB := &graph.Edge{From: sourceB.ID, To: id, Kind: graph.EdgeConsumes, FilePath: fileB, Line: 7, Meta: map[string]any{"contract_owner_repo_prefix": repoB, "contract_owner_workspace_id": "workspace-b", "contract_owner_meta": map[string]any{"var": "SHARED", "payload": []any{"b", true}}}}
				store.AddBatch([]*graph.Node{sourceA, sourceB, canonical}, []*graph.Edge{edgeA, edgeB})
				rows := graph.ReadRepoEdgesByKinds(store, []string{repoB}, []graph.EdgeKind{graph.EdgeConsumes})
				require.Len(t, rows, 1)
				beforeB := scalarBackendSnapshot(t, struct {
					Edge *graph.Edge
					Meta map[string]any
				}{rows[0].Edge, rows[0].Edge.Meta})
				beforeMeta := store.GetNode(id).Meta
				frontierA := graph.ContractOwnerReplacement{RepoPrefix: repoA, FilePaths: []string{fileA}, TouchedNodeIDs: []string{id, id}}
				result, err := graph.ReplaceContractOwners(store, frontierA)
				require.NoError(t, err)
				assert.Equal(t, 1, result.NodesChanged)
				assert.Equal(t, 1, result.EdgesRemoved)
				assert.Zero(t, result.NodesRemoved)
				remaining := store.GetNode(id)
				require.NotNil(t, remaining)
				assert.Equal(t, true, remaining.Meta["contract_owner_removed"], "retained canonical must not resurrect removed scalar A")
				assert.Equal(t, scalarBackendSnapshot(t, beforeMeta["contract_meta"]), scalarBackendSnapshot(t, remaining.Meta["contract_meta"]))
				rows = graph.ReadRepoEdgesByKinds(store, []string{repoB}, []graph.EdgeKind{graph.EdgeConsumes})
				require.Len(t, rows, 1)
				assert.Equal(t, beforeB, scalarBackendSnapshot(t, struct {
					Edge *graph.Edge
					Meta map[string]any
				}{rows[0].Edge, rows[0].Edge.Meta}), "sibling owner payload must survive unchanged")
				repeated, err := graph.ReplaceContractOwners(store, frontierA)
				require.NoError(t, err)
				assert.Equal(t, graph.ContractOwnerReplaceResult{}, repeated, "repeat replacement is mutation-free")
				last, err := graph.ReplaceContractOwners(store, graph.ContractOwnerReplacement{RepoPrefix: repoB, FilePaths: []string{fileB}, TouchedNodeIDs: []string{id}})
				require.NoError(t, err)
				assert.Equal(t, 1, last.NodesRemoved)
				assert.Nil(t, store.GetNode(id))
				assert.Empty(t, store.GetInEdges(id))
				assert.Empty(t, store.GetOutEdges(id))
				assert.NotNil(t, store.GetNode(sourceB.ID), "contract replacement does not evict source declarations")
			})
		}
	}
}

func TestContractOwnerReplacementPreservesUnremovedLegacyScalar(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			store := scalarBackendFixture(t, backend)
			const id = "env::SCALAR_ONLY_SIBLING"
			const fileA, fileB = "a/consumer.go", "b/provider.env"
			source := &graph.Node{ID: "source-a", Kind: graph.KindFunction, RepoPrefix: "a", FilePath: fileA}
			canonical := &graph.Node{ID: id, Kind: graph.KindContract, RepoPrefix: "b", FilePath: fileB, WorkspaceID: "workspace-b", ProjectID: "project-b", Meta: map[string]any{"type": "env", "role": "provider", "symbol_id": "", "contract_meta": map[string]any{"var": "SCALAR_ONLY_SIBLING", "nested": []any{"b", true}}}}
			store.AddBatch([]*graph.Node{source, canonical}, []*graph.Edge{{From: source.ID, To: id, Kind: graph.EdgeConsumes, FilePath: fileA}})
			before := scalarBackendSnapshot(t, store.GetNode(id))
			result, err := graph.ReplaceContractOwners(store, graph.ContractOwnerReplacement{RepoPrefix: "a", FilePaths: []string{fileA}, TouchedNodeIDs: []string{id}})
			require.NoError(t, err)
			assert.Equal(t, 1, result.EdgesRemoved)
			assert.Zero(t, result.NodesChanged)
			assert.Zero(t, result.NodesRemoved)
			require.NotNil(t, store.GetNode(id), "B's scalar is a live legacy record without an owner edge")
			assert.Equal(t, before, scalarBackendSnapshot(t, store.GetNode(id)))
			last, err := graph.ReplaceContractOwners(store, graph.ContractOwnerReplacement{RepoPrefix: "b", FilePaths: []string{fileB}, TouchedNodeIDs: []string{id}})
			require.NoError(t, err)
			assert.Equal(t, 1, last.NodesRemoved)
			assert.Nil(t, store.GetNode(id))
		})
	}
}
