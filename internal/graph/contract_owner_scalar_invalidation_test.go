package graph

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type scalarInvalidatorUnderTest interface {
	InvalidateContractOwnerScalars(ContractOwnerReplacement) int
}

func TestContractOwnerScalarInvalidationPreservesIdentityAndReceipts(t *testing.T) {
	g := New()
	invalidator, ok := any(g).(scalarInvalidatorUnderTest)
	require.True(t, ok, "native conditional invalidator must be installed")
	const id = "env::REMOVED"
	original := &Node{ID: id, Kind: KindContract, RepoPrefix: "a", FilePath: "a/.env", Meta: map[string]any{"role": "provider", "contract_meta": map[string]any{"var": "REMOVED"}}}
	g.AddNode(original)
	beforeRevision := g.nodeMutGen.Load()
	beforeCount := g.NodeCount()
	token := g.BeginMutationReceipt()
	frontier := ContractOwnerReplacement{RepoPrefix: "a", FilePaths: []string{"a/.env"}, TouchedNodeIDs: []string{id, id}}
	require.Equal(t, 1, invalidator.InvalidateContractOwnerScalars(frontier))
	receipt := g.EndMutationReceipt(token)
	assert.False(t, receipt.Complete)
	assert.Equal(t, beforeCount, g.NodeCount())
	assert.Equal(t, beforeRevision+1, g.nodeMutGen.Load())
	updated := g.GetNode(id)
	require.NotNil(t, updated)
	assert.NotSame(t, original, updated, "replace under node lock rather than mutating an escaped old pointer")
	assert.NotContains(t, original.Meta, "contract_owner_removed")
	assert.Equal(t, true, updated.Meta["contract_owner_removed"])
	assert.Equal(t, original.Meta["contract_meta"], updated.Meta["contract_meta"])
	assert.Equal(t, updated, g.GetFileNodes("a/.env")[0], "secondary index points at updated node")
	assert.Zero(t, invalidator.InvalidateContractOwnerScalars(frontier), "repeat invalidation is a no-op")
	assert.Equal(t, beforeRevision+1, g.nodeMutGen.Load())
}

func TestContractOwnerScalarInvalidationDoesNotOverwriteSibling(t *testing.T) {
	g := New()
	invalidator, ok := any(g).(scalarInvalidatorUnderTest)
	require.True(t, ok, "native conditional invalidator must be installed")
	const id = "env::SHARED"
	frontier := ContractOwnerReplacement{RepoPrefix: "a", FilePaths: []string{"a/.env"}, TouchedNodeIDs: []string{id}}
	for range 256 {
		g.AddNode(&Node{ID: id, Kind: KindContract, RepoPrefix: "a", FilePath: "a/.env", Meta: map[string]any{"role": "provider"}})
		sibling := &Node{ID: id, Kind: KindContract, RepoPrefix: "b", FilePath: "b/.env", Meta: map[string]any{"role": "consumer", "sibling_payload": "must survive"}}
		var workers sync.WaitGroup
		workers.Add(2)
		start := make(chan struct{})
		go func() { defer workers.Done(); <-start; invalidator.InvalidateContractOwnerScalars(frontier) }()
		go func() { defer workers.Done(); <-start; g.AddNode(sibling) }()
		close(start)
		workers.Wait()
		current := g.GetNode(id)
		require.Same(t, sibling, current, "regardless of ordering, A-only invalidation must never replace B")
		require.NotContains(t, current.Meta, "contract_owner_removed")
	}
}

func TestContractOwnerScalarInvalidationRejectsWrongFrontierAndCurrentRecords(t *testing.T) {
	g := New()
	invalidator, ok := any(g).(scalarInvalidatorUnderTest)
	require.True(t, ok)
	for _, tc := range []struct {
		name        string
		node        *Node
		replacement ContractOwnerReplacement
	}{
		{"wrong_repo", &Node{ID: "c", Kind: KindContract, RepoPrefix: "b", FilePath: "a/.env"}, ContractOwnerReplacement{RepoPrefix: "a", FilePaths: []string{"a/.env"}, TouchedNodeIDs: []string{"c"}}},
		{"wrong_file", &Node{ID: "c", Kind: KindContract, RepoPrefix: "a", FilePath: "a/other.env"}, ContractOwnerReplacement{RepoPrefix: "a", FilePaths: []string{"a/.env"}, TouchedNodeIDs: []string{"c"}}},
		{"wrong_kind", &Node{ID: "c", Kind: KindFile, RepoPrefix: "a", FilePath: "a/.env"}, ContractOwnerReplacement{RepoPrefix: "a", FilePaths: []string{"a/.env"}, TouchedNodeIDs: []string{"c"}}},
		{"already_owner_backed", &Node{ID: "c", Kind: KindContract, RepoPrefix: "a", FilePath: "a/.env", Meta: map[string]any{"contract_owner_record": true}}, ContractOwnerReplacement{RepoPrefix: "a", FilePaths: []string{"a/.env"}, TouchedNodeIDs: []string{"c"}}},
		{"current_replacement", &Node{ID: "c", Kind: KindContract, RepoPrefix: "a", FilePath: "a/.env"}, ContractOwnerReplacement{RepoPrefix: "a", FilePaths: []string{"a/.env"}, TouchedNodeIDs: []string{"c"}, Nodes: []*Node{{ID: "c", Kind: KindContract}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g.AddNode(tc.node)
			before := g.nodeMutGen.Load()
			assert.Zero(t, invalidator.InvalidateContractOwnerScalars(tc.replacement))
			assert.Same(t, tc.node, g.GetNode("c"))
			assert.Equal(t, before, g.nodeMutGen.Load())
		})
	}
}
