package graph

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRepoExactNamesVisitorFallbackPreservesOverlayAuthority(t *testing.T) {
	base := New()
	base.AddNode(&Node{ID: "a/covered.go::old", Name: "Match", RepoPrefix: "a", FilePath: "a/covered.go"})
	base.AddNode(&Node{ID: "a/deleted.go::old", Name: "Match", RepoPrefix: "a", FilePath: "a/deleted.go"})
	base.AddNode(&Node{ID: "a/visible.go::old", Name: "Match", RepoPrefix: "a", FilePath: "a/visible.go"})
	base.AddNode(&Node{ID: "b/visible.go::other", Name: "Match", RepoPrefix: "b", FilePath: "b/visible.go"})
	layer := NewOverlayLayer()
	layer.MarkFile("a/covered.go", false)
	layer.MarkFile("a/deleted.go", true)
	layer.AddNode("a/covered.go", &Node{ID: "a/covered.go::new", Name: "Match", RepoPrefix: "a", FilePath: "a/covered.go"})
	view := NewOverlaidViewWithLayer(base, layer)
	var ids []string
	require.NoError(t, VisitNodesByNamesInRepoContext(t.Context(), view, []string{"Match"}, "a", func(n *Node) bool { ids = append(ids, n.ID); return true }))
	require.ElementsMatch(t, []string{"a/covered.go::new", "a/visible.go::old"}, ids)
	seen := 0
	require.NoError(t, VisitNodesByNamesInRepoContext(t.Context(), view, []string{"Match"}, "a", func(*Node) bool { seen++; return false }))
	require.Equal(t, 1, seen)
	cause := errors.New("checked selected reader failed")
	broken := &exactNamesVisitorStub{Reader: base, err: cause}
	require.ErrorIs(t, VisitNodesByNamesInRepoContext(t.Context(), broken, []string{"Match", "missing"}, "a", func(*Node) bool { return true }), cause)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	require.ErrorIs(t, VisitNodesByNamesInRepoContext(ctx, view, []string{"Match"}, "a", func(*Node) bool { t.Fatal("canceled visit delivered a row"); return true }), context.Canceled)
}
