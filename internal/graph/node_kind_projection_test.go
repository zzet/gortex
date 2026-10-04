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
