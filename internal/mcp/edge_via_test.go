package mcp

import (
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/query"
)

func TestEnrichSubGraphEdgesSetsVia(t *testing.T) {
	sg := &query.SubGraph{Edges: []*graph.Edge{
		{From: "a", To: "b", Kind: graph.EdgeCalls, Origin: graph.OriginASTInferred, Meta: map[string]any{"via": "observer.channel"}},
		{From: "c", To: "d", Kind: graph.EdgeCalls, Origin: graph.OriginASTResolved}, // no via
	}}
	enrichSubGraphEdges(sg)
	if sg.Edges[0].Via != "observer channel" {
		t.Errorf("edge[0].Via = %q, want 'observer channel'", sg.Edges[0].Via)
	}
	if sg.Edges[1].Via != "" {
		t.Errorf("edge[1].Via = %q, want empty (no via meta)", sg.Edges[1].Via)
	}
}

func TestEncodeSubGraphEmitsViaColumn(t *testing.T) {
	sg := &query.SubGraph{
		Nodes: []*graph.Node{{ID: "a", Kind: graph.KindMethod, Name: "a"}, {ID: "b", Kind: graph.KindMethod, Name: "b"}},
		Edges: []*graph.Edge{
			{From: "a", To: "b", Kind: graph.EdgeCalls, Origin: graph.OriginASTInferred, Via: "observer channel"},
		},
	}
	out, err := encodeSubGraph("walk_graph", sg, nil)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	if !strings.Contains(s, "via") {
		t.Errorf("GCX subgraph edges header missing 'via' column:\n%s", s)
	}
	if !strings.Contains(s, "observer channel") {
		t.Errorf("GCX subgraph missing via label 'observer channel':\n%s", s)
	}
}
