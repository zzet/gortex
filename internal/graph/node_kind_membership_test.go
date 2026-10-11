package graph

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNodeKindMembershipSelectedReplacementDeletionAndScope(t *testing.T) {
	base := New()
	base.AddBatch([]*Node{{ID: "a/file.go::Same", Kind: KindContract, FilePath: "a/file.go"}, {ID: "a/delete.go::Deleted", Kind: KindConfigKey, FilePath: "a/delete.go"}, {ID: "a/keep.go::Keep", Kind: KindContractBridge, FilePath: "a/keep.go"}}, nil)
	layer := NewOverlayLayer()
	layer.AddNode("a/file.go", &Node{ID: "a/file.go::Same", Kind: KindFunction, FilePath: "a/file.go"})
	layer.MarkFile("a/delete.go", true)
	selected := NewOverlaidViewWithLayer(base, layer)
	ids := []string{"a/file.go::Same", "a/delete.go::Deleted", "a/keep.go::Keep", "missing", "a/keep.go::Keep", ""}
	owned := []NodeKind{KindContract, KindContractBridge, KindConfigKey}
	rows, err := GetNodeIDsByKindsContext(t.Context(), selected, ids, owned)
	require.NoError(t, err)
	require.Equal(t, map[string]struct{}{"a/keep.go::Keep": {}}, rows)
	dw := NewDeltaWriter(selected, New())
	dw.AddNode(&Node{ID: "a/file.go::Same", Kind: KindConfigKey, FilePath: "a/file.go"})
	rows, err = GetNodeIDsByKindsContext(t.Context(), dw, ids, owned)
	require.NoError(t, err)
	require.Equal(t, map[string]struct{}{"a/file.go::Same": {}, "a/keep.go::Keep": {}}, rows)
	wrapper := &selectedLegacyKindWrapper{Reader: base, selected: map[string]*Node{"a/file.go::Same": {ID: "a/file.go::Same", Kind: KindFunction}}}
	rows, err = GetNodeIDsByKindsContext(t.Context(), wrapper, ids, owned)
	require.NoError(t, err)
	require.Empty(t, rows, "an Unwrap method must not widen the selected scope")
	require.Equal(t, 1, wrapper.reads)
}

type membershipSpy struct {
	Reader
	calls  int
	err    error
	cancel context.CancelFunc
}

func (s *membershipSpy) GetNodeIDsByKindsContext(context.Context, []string, []NodeKind) (map[string]struct{}, error) {
	s.calls++
	if s.cancel != nil {
		s.cancel()
	}
	return map[string]struct{}{"partial": {}}, s.err
}
func TestNodeKindMembershipCheckedFailureAndCancellation(t *testing.T) {
	sentinel := errors.New("membership unavailable")
	spy := &membershipSpy{Reader: New(), err: sentinel}
	rows, err := GetNodeIDsByKindsContext(t.Context(), spy, []string{"x"}, []NodeKind{KindContract})
	require.ErrorIs(t, err, sentinel)
	require.Nil(t, rows)
	require.Equal(t, 1, spy.calls)
	ctx, cancel := context.WithCancel(t.Context())
	spy.err = nil
	spy.cancel = cancel
	rows, err = GetNodeIDsByKindsContext(ctx, spy, []string{"x"}, []NodeKind{KindContract})
	require.ErrorIs(t, err, context.Canceled)
	require.Nil(t, rows)
	fallback := &failingNodeKindReader{Reader: New(), err: sentinel}
	rows, err = GetNodeIDsByKindsContext(t.Context(), fallback, []string{"x"}, []NodeKind{KindContract})
	require.ErrorIs(t, err, sentinel)
	require.Nil(t, rows)
}
