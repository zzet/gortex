package graph

import (
	"fmt"
	"sort"
	"strings"
)

// edgeClaimFixture is a configuration file three users call into, each user
// with a wide outgoing set of its own (the shape that made an eviction claim
// thousands of sources whole).
type edgeClaimFixture struct {
	nodes []*Node
	edges []*Edge
	// cfgNodes / cfgEdges are cfg.go's rows; incoming the users' edges into it.
	cfgNodes []*Node
	cfgEdges []*Edge
	incoming []*Edge
}

const edgeClaimFanout = 20

func newEdgeClaimFixture() edgeClaimFixture {
	var f edgeClaimFixture
	node := func(id, path string, kind NodeKind) *Node {
		name := id
		if i := strings.LastIndex(id, "::"); i >= 0 {
			name = id[i+2:]
		}
		return &Node{ID: id, Name: name, Kind: kind, FilePath: path, Language: "go", RepoPrefix: ""}
	}
	f.cfgNodes = []*Node{
		node("cfg.go", "cfg.go", KindFile),
		node("cfg.go::Load", "cfg.go", KindFunction),
		node("cfg.go::Opt", "cfg.go", KindType),
	}
	f.nodes = append(f.nodes, f.cfgNodes...)
	f.nodes = append(f.nodes, node("other.go", "other.go", KindFile))
	for k := 0; k < edgeClaimFanout; k++ {
		f.nodes = append(f.nodes, node(fmt.Sprintf("other.go::F%d", k), "other.go", KindFunction))
	}
	f.cfgEdges = []*Edge{
		{From: "cfg.go::Load", To: "other.go::F0", Kind: EdgeCalls, FilePath: "cfg.go", Line: 7},
		{From: "cfg.go", To: "cfg.go::Load", Kind: EdgeDefines, FilePath: "cfg.go", Line: 1},
	}
	f.edges = append(f.edges, f.cfgEdges...)
	for u := 1; u <= 3; u++ {
		path := fmt.Sprintf("u%d.go", u)
		use := path + "::Use"
		f.nodes = append(f.nodes, node(path, path, KindFile), node(use, path, KindFunction))
		f.incoming = append(f.incoming,
			&Edge{From: use, To: "cfg.go::Load", Kind: EdgeCalls, FilePath: path, Line: 3, Origin: "ast_resolved"},
			&Edge{From: use, To: "cfg.go::Load", Kind: EdgeCalls, FilePath: path, Line: 9, Origin: "ast_resolved"},
			&Edge{From: use, To: "cfg.go::Opt", Kind: EdgeReferences, FilePath: path, Line: 4},
			&Edge{From: path, To: "cfg.go", Kind: EdgeImports, FilePath: path, Line: 1},
		)
		for k := 0; k < edgeClaimFanout; k++ {
			f.edges = append(f.edges, &Edge{From: use, To: fmt.Sprintf("other.go::F%d", k), Kind: EdgeCalls, FilePath: path, Line: 10 + k})
		}
	}
	f.edges = append(f.edges, f.incoming...)
	return f
}

func (f edgeClaimFixture) store() *Graph {
	g := New()
	g.AddBatch(cloneDeltaNodes(f.nodes), cloneDeltaEdges(f.edges))
	return g
}

// edgeSetRender renders a set of edges order-independently.
func edgeSetRender(edges []*Edge) string {
	rows := make([]string, 0, len(edges))
	for _, e := range edges {
		if e != nil {
			rows = append(rows, deltaEdgeRender(e))
		}
	}
	sort.Strings(rows)
	return strings.Join(rows, "\n")
}

func (f edgeClaimFixture) ids() []string {
	out := make([]string, 0, len(f.nodes))
	for _, n := range f.nodes {
		out = append(out, n.ID)
	}
	return out
}
