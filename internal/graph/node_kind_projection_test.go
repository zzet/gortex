package graph

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNodeKindProjectionSelectedReplacementDeletionAndDelta(t *testing.T) {
	base := New()
	old := &Node{ID: "repo/file.go::Old", Kind: KindContract, FilePath: "repo/file.go", RepoPrefix: "repo"}
	other := &Node{ID: "repo/other.go::Keep", Kind: KindTable, FilePath: "repo/other.go", RepoPrefix: "repo"}
	base.AddBatch([]*Node{old, other}, nil)
	layer := NewOverlayLayer()
	newNode := &Node{ID: "repo/file.go::New", Kind: KindFunction, FilePath: old.FilePath, RepoPrefix: "repo"}
	layer.AddNode(old.FilePath, newNode)
	selected := NewOverlaidViewWithLayer(base, layer)
	ids := []string{"", old.ID, newNode.ID, other.ID, newNode.ID, "missing"}
	before := append([]string(nil), ids...)
	rows, err := GetNodeKindsByIDsContext(t.Context(), selected, ids)
	require.NoError(t, err)
	require.Equal(t, before, ids)
	require.Equal(t, map[string]NodeKindRow{newNode.ID: nodeKindRow(newNode), other.ID: nodeKindRow(other)}, rows)
	deleted := NewOverlayLayer()
	deleted.MarkFile(other.FilePath, true)
	rows, err = GetNodeKindsByIDsContext(t.Context(), NewOverlaidViewWithLayer(selected, deleted), ids)
	require.NoError(t, err)
	require.Equal(t, map[string]NodeKindRow{newNode.ID: nodeKindRow(newNode)}, rows)
	dw := NewDeltaWriter(selected, New())
	replacement := *newNode
	replacement.Kind = KindConfigKey
	dw.AddNode(&replacement)
	rows, err = GetNodeKindsByIDsContext(t.Context(), dw, ids)
	require.NoError(t, err)
	require.Equal(t, KindConfigKey, rows[newNode.ID].Kind)
}

type failingNodeKindReader struct {
	Reader
	err error
}

func (r *failingNodeKindReader) GetNodeKindsByIDsContext(context.Context, []string) (map[string]NodeKindRow, error) {
	return map[string]NodeKindRow{"partial": {Kind: KindContract}}, r.err
}
func (r *failingNodeKindReader) Unwrap() Reader { return r.Reader }

func TestNodeKindProjectionCancellationAndCheckedWrapper(t *testing.T) {
	base := New()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	rows, err := GetNodeKindsByIDsContext(ctx, base, nil)
	require.ErrorIs(t, err, context.Canceled)
	require.Nil(t, rows)
	sentinel := errors.New("checked source unavailable")
	rows, err = GetNodeKindsByIDsContext(t.Context(), &failingNodeKindReader{Reader: base, err: sentinel}, []string{"x"})
	require.ErrorIs(t, err, sentinel)
	require.Nil(t, rows, "checked wrapper failure must not unwrap into apparent absence")
}

type selectedLegacyKindWrapper struct {
	Reader
	selected map[string]*Node
	reads    int
}

func (r *selectedLegacyKindWrapper) Unwrap() Reader { return r.Reader }
func (r *selectedLegacyKindWrapper) GetNodesByIDs(ids []string) map[string]*Node {
	r.reads++
	out := make(map[string]*Node)
	for _, id := range ids {
		if n := r.selected[id]; n != nil {
			out[id] = n
		}
	}
	return out
}

type legacyKindLayer struct{ OverlayLayerReader }

func TestNodeKindProjectionPreservesLegacySelectedWrapperAndLayer(t *testing.T) {
	base := New()
	base.AddNode(&Node{ID: "repo/file.go::Selected", Kind: KindContract, FilePath: "repo/file.go"})
	wrapper := &selectedLegacyKindWrapper{Reader: base, selected: map[string]*Node{
		"repo/file.go::Selected": {ID: "repo/file.go::Selected", Kind: KindFunction, FilePath: "repo/file.go"},
	}}
	rows, err := GetNodeKindsByIDsContext(t.Context(), wrapper, []string{"repo/file.go::Selected", "missing"})
	require.NoError(t, err)
	require.Equal(t, KindFunction, rows["repo/file.go::Selected"].Kind, "unwrapping would widen to the wrong kind")
	require.Equal(t, 1, wrapper.reads)
	layer := NewOverlayLayer()
	layer.AddNode("repo/file.go", &Node{ID: "repo/file.go::New", Kind: KindMethod, FilePath: "repo/file.go"})
	view := NewOverlaidViewWithLayer(wrapper, &legacyKindLayer{OverlayLayerReader: layer})
	rows, err = GetNodeKindsByIDsContext(t.Context(), view, []string{"repo/file.go::Selected", "repo/file.go::New"})
	require.NoError(t, err)
	require.Equal(t, map[string]NodeKindRow{"repo/file.go::New": {Kind: KindMethod, FilePath: "repo/file.go"}}, rows)
}
