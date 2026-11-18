package graph

import (
	"fmt"
	"sort"
	"strings"
	"testing"
)

// fullInEdgeCounter counts the bottom store's full incoming-row batches.
type fullInEdgeCounter struct {
	*Graph
	fullIn int
}

func (c *fullInEdgeCounter) GetInEdgesByNodeIDs(ids []string) map[string][]*Edge {
	c.fullIn++
	return c.Graph.GetInEdgesByNodeIDs(ids)
}

// A delta's incoming identities are read through the bottom store's identity
// projection, never its full rows: the resolver's identity-first admission
// over a delta decodes no payload below the delta.
func TestDeltaWriterInEdgeIdentitiesReadNoFullRowsBelow(t *testing.T) {
	f := newEdgeClaimFixture()
	below := &fullInEdgeCounter{Graph: f.store()}
	dw := NewDeltaWriter(below, nil)
	dw.EvictFiles([]string{"cfg.go"})
	dw.AddBatch(cloneDeltaNodes(f.cfgNodes), cloneDeltaEdges(append(cloneDeltaEdges(f.cfgEdges), f.incoming...)))
	before := below.fullIn
	identities := InEdgeIdentitiesByNodeIDs(dw, []string{"cfg.go::Load", "cfg.go::Opt"})
	if below.fullIn != before {
		t.Fatalf("the identity read made %d full incoming batches below the delta", below.fullIn-before)
	}
	if len(identities["cfg.go::Load"]) != 7 || len(identities["cfg.go::Opt"]) != 3 {
		t.Fatalf("identities %v; want the defines and 6 calls into Load, 3 references to Opt", identities)
	}
}

// requireSameInIdentities compares the delta's incoming identities with those
// of a store that took the same writes.
func requireSameInIdentities(t *testing.T, label string, dw *DeltaWriter, ref *Graph, ids []string) {
	t.Helper()
	got := dw.GetInEdgeIdentitiesByNodeIDs(ids)
	for _, id := range ids {
		var wantRows, gotRows []string
		for _, e := range ref.GetInEdges(id) {
			wantRows = append(wantRows, fmt.Sprintf("%+v", EdgeIdentityFor(e)))
		}
		for _, identity := range got[id] {
			gotRows = append(gotRows, fmt.Sprintf("%+v", identity))
		}
		sort.Strings(wantRows)
		sort.Strings(gotRows)
		if g, w := strings.Join(gotRows, "\n"), strings.Join(wantRows, "\n"); g != w {
			t.Fatalf("%s: in-edge identities of %s\n got: %s\nwant: %s", label, id, g, w)
		}
	}
}

// The delta's incoming identities are the composed rows' identities through
// an eviction (the incident rows hidden), a re-statement, and a partial one
// (a dropped reference, a moved call).
func TestDeltaWriterInEdgeIdentitiesMatchTheComposedRows(t *testing.T) {
	f := newEdgeClaimFixture()
	ref := f.store()
	dw := NewDeltaWriter(f.store(), nil)
	dw.EvictFiles([]string{"cfg.go"})
	ref.EvictFile("cfg.go")
	requireSameInIdentities(t, "after eviction", dw, ref, f.ids())

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
	requireSameInIdentities(t, "after a partial re-statement", dw, ref, f.ids())
}

// identityReadCounter counts the bottom store's identity projections.
type identityReadCounter struct {
	*Graph
	reads int
}

func (c *identityReadCounter) GetInEdgeIdentitiesByNodeIDs(ids []string) map[string][]EdgeIdentity {
	c.reads++
	return c.Graph.GetInEdgeIdentitiesByNodeIDs(ids)
}

// With the stack's projection cache installed, the incoming identities below
// the delta are read once per stack: every delta's answer still equals a
// store's that took the same writes (the delta's own eviction and
// re-statement applied per delta), and the second delta reads none below.
func TestDeltaWriterInEdgeIdentitiesAreKeptPerStack(t *testing.T) {
	f := newEdgeClaimFixture()
	below := &identityReadCounter{Graph: f.store()}
	cache := NewBaseProjectionCache()
	restate := cloneDeltaEdges(f.cfgEdges)
	for _, e := range f.incoming {
		if e.From == "u2.go::Use" && e.Kind == EdgeReferences {
			continue
		}
		restate = append(restate, cloneDeltaEdge(e))
	}
	var reads []int
	for delta := 0; delta < 2; delta++ {
		ref := f.store()
		dw := NewDeltaWriter(below, nil)
		dw.SetBaseProjectionCache(cache)
		before := below.reads
		dw.EvictFiles([]string{"cfg.go"})
		ref.EvictFile("cfg.go")
		requireSameInIdentities(t, fmt.Sprintf("delta %d after eviction", delta), dw, ref, f.ids())
		dw.AddBatch(cloneDeltaNodes(f.cfgNodes), cloneDeltaEdges(restate))
		ref.AddBatch(cloneDeltaNodes(f.cfgNodes), cloneDeltaEdges(restate))
		requireSameInIdentities(t, fmt.Sprintf("delta %d after a partial re-statement", delta), dw, ref, f.ids())
		reads = append(reads, below.reads-before)
	}
	if reads[0] == 0 || reads[1] != 0 {
		t.Fatalf("identity reads below per delta = %v; want some on the first and none on the second", reads)
	}
}
