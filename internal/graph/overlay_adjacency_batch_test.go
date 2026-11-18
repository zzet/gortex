package graph

import (
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
)

func adjacencyBatchOldChain(base Reader, layers []OverlayLayerReader) Reader {
	for _, layer := range layers {
		base = NewOverlaidViewWithLayer(base, layer)
	}
	return base
}

func TestImmutableAdjacencyBatchMatchesEveryLayerOrder(t *testing.T) {
	base := New()
	ids := []string{"a.go::A", "b.go::B", "c.go::C", "foreign.go::F", "outside.go::X"}
	for _, id := range ids {
		base.AddNode(&Node{ID: id, FilePath: IDFile(id), Kind: KindFunction})
	}
	for _, edge := range []*Edge{
		{From: ids[0], To: ids[1], Kind: EdgeCalls, FilePath: "a.go"},
		{From: ids[0], To: ids[2], Kind: EdgeCalls, FilePath: "foreign.go"},
		{From: ids[1], To: ids[0], Kind: EdgeCalls, FilePath: "b.go"},
		{From: ids[3], To: ids[1], Kind: EdgeCalls, FilePath: "foreign.go"},
		{From: ids[0], To: ids[3], Kind: EdgeCalls},
		{From: ids[4], To: ids[2], Kind: EdgeCalls, FilePath: "foreign.go"},
	} {
		base.AddEdge(edge)
	}
	replace := NewOverlayLayer()
	replace.MarkFile("a.go", false)
	replace.AddNode("a.go", &Node{ID: ids[0], FilePath: "a.go", Kind: KindFunction, StartLine: 8})
	replace.AddEdge(&Edge{From: ids[0], To: ids[1], Kind: EdgeCalls, FilePath: "a.go", Line: 8})
	deleted := NewOverlayLayer()
	deleted.MarkFile("b.go", true)
	deleted.MarkRemoved("C", ids[2])
	restored := NewOverlayLayer()
	restored.MarkFile("b.go", false)
	restored.AddNode("b.go", &Node{ID: ids[1], FilePath: "b.go", Kind: KindFunction, StartLine: 19})
	restored.AddEdge(&Edge{From: ids[1], To: ids[0], Kind: EdgeCalls, FilePath: "b.go", Line: 19})
	identity := NewOverlayLayer()
	identity.MarkRemoved("F", ids[3])
	// This identity lies outside the file covered by the layer. The original
	// contract says it replaces that source's adjacency even when a foreign
	// file recorded the lower edge.
	identity.MarkFile("metadata.go", false)
	identity.AddNode("metadata.go", &Node{ID: ids[4], FilePath: "metadata.go", Kind: KindFunction})
	identity.AddEdge(&Edge{From: ids[4], To: ids[0], Kind: EdgeCalls, FilePath: "metadata.go"})
	all := []OverlayLayerReader{replace, deleted, restored, identity}
	var permutations func([]int, []int)
	permutations = func(order, rest []int) {
		if len(rest) != 0 {
			for i, next := range rest {
				remaining := append(slices.Clone(rest[:i]), rest[i+1:]...)
				permutations(append(slices.Clone(order), next), remaining)
			}
			return
		}
		layers := make([]OverlayLayerReader, len(order))
		for i, index := range order {
			layers[i] = all[index]
		}
		old := adjacencyBatchOldChain(base, layers)
		batch := NewOverlaidViewWithLayers(base, layers)
		require.NotNil(t, batch.adjacencyBatch)
		requested := append(slices.Clone(ids), "", ids[0], "missing.go::Missing")
		original := slices.Clone(requested)
		require.Equal(t, old.GetOutEdgesByNodeIDs(requested), batch.GetOutEdgesByNodeIDs(requested), "outgoing order %v", order)
		require.Equal(t, old.GetInEdgesByNodeIDs(requested), batch.GetInEdgesByNodeIDs(requested), "incoming order %v", order)
		require.Equal(t, original, requested, "batch mutated its caller's ID slice")
		require.Equal(t, old.GetNodesByIDs(requested), batch.GetNodesByIDs(requested), "node rows order %v", order)
		oldKinds, oldErr := GetNodeKindsByIDsContext(t.Context(), old, requested)
		kinds, err := batch.GetNodeKindsByIDsContext(t.Context(), requested)
		require.Equal(t, oldErr, err)
		require.Equal(t, oldKinds, kinds, "kind rows order %v", order)
		for _, id := range requested {
			require.Equal(t, old.GetNode(id), batch.GetNode(id))
			require.Equal(t, old.GetOutEdges(id), batch.GetOutEdges(id), "point outgoing %s order %v", id, order)
			require.Equal(t, old.GetInEdges(id), batch.GetInEdges(id), "point incoming %s order %v", id, order)
		}
	}
	permutations(nil, []int{0, 1, 2, 3})
}

type nonBatchOverlayLayer struct{ OverlayLayerReader }

func TestImmutableAdjacencyBatchRetainsFallbackAndEmptySemantics(t *testing.T) {
	empty := NewOverlayLayer()
	for _, base := range []Reader{nil, New()} {
		for _, layers := range [][]OverlayLayerReader{
			{empty}, {empty, empty}, {empty, &nonBatchOverlayLayer{empty}}, {empty, nil},
		} {
			old := adjacencyBatchOldChain(base, layers)
			batch := NewOverlaidViewWithLayers(base, layers)
			for _, ids := range [][]string{nil, {}, {""}, {"x", "x", ""}} {
				require.Equal(t, old.GetNodesByIDs(ids), batch.GetNodesByIDs(ids))
				oldKinds, oldErr := GetNodeKindsByIDsContext(t.Context(), old, ids)
				kinds, err := batch.GetNodeKindsByIDsContext(t.Context(), ids)
				require.Equal(t, oldErr, err)
				require.Equal(t, oldKinds, kinds)
				require.Equal(t, old.GetOutEdgesByNodeIDs(ids), batch.GetOutEdgesByNodeIDs(ids))
				require.Equal(t, old.GetInEdgesByNodeIDs(ids), batch.GetInEdgesByNodeIDs(ids))
			}
		}
	}
}

type batchUnknownRevisionReader struct{ Reader }

func TestImmutableAdjacencyBatchPreservesCanonicalRevisionWitness(t *testing.T) {
	base := New()
	layers := []OverlayLayerReader{NewOverlayLayer(), NewOverlayLayer()}
	batch := NewOverlaidViewWithLayers(base, layers)
	before, known := batch.SearchMutationRevision()
	require.True(t, known)
	base.AddNode(&Node{ID: "canonical.go::Changed", FilePath: "canonical.go", Kind: KindFunction})
	after, known := batch.SearchMutationRevision()
	require.True(t, known)
	require.Greater(t, after, before, "a batch must not conceal canonical node-only mutation behind immutable layers")
	unknown := NewOverlaidViewWithLayers(batchUnknownRevisionReader{Reader: base}, layers)
	_, known = unknown.SearchMutationRevision()
	require.False(t, known, "an unsupported base cannot fabricate an authoritative zero witness")
}
