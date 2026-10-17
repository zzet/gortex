package graph

import (
	"fmt"
	"sort"
	"strings"
	"testing"
)

// fullOutEdgeCounter counts the bottom store's full outgoing-row batches.
type fullOutEdgeCounter struct {
	*Graph
	fullOut int
}

func (c *fullOutEdgeCounter) GetOutEdgesByNodeIDs(ids []string) map[string][]*Edge {
	c.fullOut++
	return c.Graph.GetOutEdgesByNodeIDs(ids)
}

// renderCandidates renders every requested endpoint and site answer.
func renderCandidates(set EdgeCandidateSet, endpoints []EdgeEndpoint, sites []EdgeSite) string {
	var rows []string
	for _, key := range endpoints {
		if e := set.Endpoint(key.From, key.To); e != nil {
			rows = append(rows, "endpoint "+deltaEdgeRender(e))
		}
	}
	for _, key := range sites {
		for _, e := range set.Site(key.From, key.Line, key.Kind) {
			rows = append(rows, fmt.Sprintf("site %s:%d:%s %s", key.From, key.Line, key.Kind, deltaEdgeRender(e)))
		}
	}
	sort.Strings(rows)
	return strings.Join(rows, "\n")
}

// A delta's edge candidates are the candidates of a store that took the same
// writes — through an eviction and a partial re-statement — and are read
// from the bottom store by predicate, never as the sources' whole out-edges;
// a caller re-pointing a candidate leaves the store below untouched.
func TestDeltaWriterEdgeCandidatesAreProbedNotReadWhole(t *testing.T) {
	f := newEdgeClaimFixture()
	below := &fullOutEdgeCounter{Graph: f.store()}
	ref := f.store()
	dw := NewDeltaWriter(below, nil)
	var endpoints []EdgeEndpoint
	var sites []EdgeSite
	for _, e := range append(append([]*Edge(nil), f.edges...), &Edge{From: "u3.go::Use", To: "cfg.go::Load", Line: 11}) {
		endpoints = append(endpoints, EdgeEndpoint{From: e.From, To: e.To})
		sites = append(sites, EdgeSite{From: e.From, Line: e.Line, Kind: e.Kind}, EdgeSite{From: e.From, Line: e.Line})
	}
	check := func(label string) {
		t.Helper()
		before := below.fullOut
		got := renderCandidates(dw.GetEdgeCandidates(endpoints, sites), endpoints, sites)
		if below.fullOut != before {
			t.Fatalf("%s: %d full outgoing batches read below the delta", label, below.fullOut-before)
		}
		if want := renderCandidates(ref.GetEdgeCandidates(endpoints, sites), endpoints, sites); got != want {
			t.Fatalf("%s: candidates\n got: %s\nwant: %s", label, got, want)
		}
	}
	check("untouched")
	dw.EvictFiles([]string{"cfg.go"})
	ref.EvictFile("cfg.go")
	check("after eviction")
	restate := cloneDeltaEdges(f.cfgEdges)
	for _, e := range f.incoming {
		if e.From == "u2.go::Use" && e.Kind == EdgeReferences {
			continue
		}
		c := cloneDeltaEdge(e)
		if c.From == "u3.go::Use" && c.Line == 9 {
			c.Line = 11
		}
		restate = append(restate, c)
	}
	dw.AddBatch(cloneDeltaNodes(f.cfgNodes), cloneDeltaEdges(restate))
	ref.AddBatch(cloneDeltaNodes(f.cfgNodes), cloneDeltaEdges(restate))
	check("after a partial re-statement")

	for _, e := range dw.GetEdgeCandidates(endpoints, sites).byEndpoint[EdgeEndpoint{From: "u1.go::Use", To: "other.go::F0"}] {
		e.From = "moved"
	}
	for _, e := range below.GetOutEdges("u1.go::Use") {
		if e.From != "u1.go::Use" {
			t.Fatal("re-pointing a candidate changed the store below")
		}
	}
}
