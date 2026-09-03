package mcp

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/query"
)

func dispatchGraph(t *testing.T) *graph.Graph {
	t.Helper()
	g := graph.New()
	g.AddNode(&graph.Node{ID: "pkg.go::I", Kind: graph.KindInterface, Name: "I"})
	g.AddNode(&graph.Node{ID: "pkg.go::A", Kind: graph.KindType, Name: "A"})
	g.AddNode(&graph.Node{ID: "pkg.go::B", Kind: graph.KindType, Name: "B"})
	g.AddEdge(&graph.Edge{From: "pkg.go::A", To: "pkg.go::I", Kind: graph.EdgeImplements})
	g.AddEdge(&graph.Edge{From: "pkg.go::B", To: "pkg.go::I", Kind: graph.EdgeImplements})
	return g
}

func TestDispatchImplementorCount(t *testing.T) {
	s := &Server{graph: dispatchGraph(t)}
	ctx := context.Background()
	require.Equal(t, 2, s.dispatchImplementorCount(ctx, "pkg.go::I"))
	require.Equal(t, 0, s.dispatchImplementorCount(ctx, "pkg.go::A"))
	require.Equal(t, 0, s.dispatchImplementorCount(ctx, "pkg.go::missing"))
}

func TestAttachRelatedToolsCue_DispatchHeavy(t *testing.T) {
	s := &Server{graph: dispatchGraph(t), session: &sessionState{}}

	// A dispatch-heavy target gets the find_implementations cue.
	sg := &query.SubGraph{}
	s.attachRelatedToolsCue(context.Background(), sg, "pkg.go::I")
	require.Contains(t, sg.RelatedTools, "find_implementations")

	// Once per session: a second dispatch-heavy result does NOT repeat it.
	sg2 := &query.SubGraph{}
	s.attachRelatedToolsCue(context.Background(), sg2, "pkg.go::I")
	require.Empty(t, sg2.RelatedTools, "the cue is emitted at most once per session")
}

func TestAttachRelatedToolsCue_NotDispatchHeavy(t *testing.T) {
	s := &Server{graph: dispatchGraph(t), session: &sessionState{}}
	// A plain symbol with no implementors gets no cue.
	sg := &query.SubGraph{}
	s.attachRelatedToolsCue(context.Background(), sg, "pkg.go::A")
	require.Empty(t, sg.RelatedTools)
}

func TestAttachRelatedToolsCue_YieldsToEmptinessCaveats(t *testing.T) {
	s := &Server{graph: dispatchGraph(t), session: &sessionState{}}
	// A zero-edge / tier-filtered result owns the "why empty" story — the
	// discovery cue must not compete with it.
	sg := &query.SubGraph{Caveat: &graph.ZeroEdgeCaveat{}}
	s.attachRelatedToolsCue(context.Background(), sg, "pkg.go::I")
	require.Empty(t, sg.RelatedTools)
}
