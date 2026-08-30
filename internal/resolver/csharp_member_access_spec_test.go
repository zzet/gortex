package resolver

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

// accessEdge returns the first EdgeReads/EdgeWrites out of fromID whose
// target (resolved or unresolved) names member.
func accessEdge(g graph.Store, fromID, member string) *graph.Edge {
	for _, e := range g.GetOutEdges(fromID) {
		if e.Kind != graph.EdgeReads && e.Kind != graph.EdgeWrites {
			continue
		}
		if graph.IsUnresolvedTarget(e.To) {
			if name := graph.UnresolvedName(e.To); name == "*."+member || name == member {
				return e
			}
			continue
		}
		if n := g.GetNode(e.To); n != nil && n.Name == member {
			return e
		}
	}
	return nil
}

// A typed C# property read must bind the receiver type's own property —
// not a same-named property on an unrelated class — through the existing
// resolveFieldRef cascade.
func TestResolveCSharp_MemberAccessBindsReceiverField(t *testing.T) {
	g := buildCSharpResolverGraph(t, map[string]string{
		"Lib.cs": `namespace App {
    public class StencilDecoy { public string Title { get; set; } }
    public class Plaque { public string Title { get; set; } }
    public class Reader {
        public string Get() { Plaque p = new Plaque(); return p.Title; }
        public void Set() { Plaque p = new Plaque(); p.Title = "x"; }
    }
}`,
	})
	New(g).ResolveAll()

	read := accessEdge(g, "Lib.cs::Reader.Get", "Title")
	require.NotNil(t, read, "p.Title must produce a read edge")
	assert.True(t, strings.Contains(read.To, "Plaque.Title"),
		"typed receiver binds the receiver type's property, got %q", read.To)
	assert.False(t, strings.Contains(read.To, "StencilDecoy"),
		"decoy must not win, got %q", read.To)
	assert.GreaterOrEqual(t, read.Confidence, 0.9,
		"exact receiver-type field match binds confidently")

	write := accessEdge(g, "Lib.cs::Reader.Set", "Title")
	require.NotNil(t, write, "p.Title = ... must produce a write edge")
	assert.Equal(t, graph.EdgeWrites, write.Kind)
	assert.True(t, strings.Contains(write.To, "Plaque.Title"),
		"write binds the same way, got %q", write.To)
}

// A field used as a bare identifier — a call receiver or an assignment
// target, the idiomatic forms — must land on the ENCLOSING type's own
// field node through the same cascade, with a same-named field on an
// unrelated class never stealing the bind. This is what turns
// find_usages on a field from structurally empty into the read/write
// story of the field's actual life.
func TestResolveCSharp_FieldIdentifierUsesBindOwnField(t *testing.T) {
	g := buildCSharpResolverGraph(t, map[string]string{
		"Lib.cs": `namespace App {
    public class Store { public void Add(int n) { } }
    public class Decoy { private Store _store; }
    public class Ledger {
        private Store _store;
        public Ledger(Store store) { _store = store; }
        public void Post() { _store.Add(1); }
    }
}`,
	})
	New(g).ResolveAll()

	read := accessEdge(g, "Lib.cs::Ledger.Post", "_store")
	require.NotNil(t, read, "_store.Add(1) must produce a read edge of the field")
	assert.Equal(t, graph.EdgeReads, read.Kind)
	assert.True(t, strings.Contains(read.To, "Ledger._store"),
		"binds the enclosing type's own field, got %q", read.To)
	assert.False(t, strings.Contains(read.To, "Decoy"),
		"the same-named field on an unrelated class must not win, got %q", read.To)
	assert.GreaterOrEqual(t, read.Confidence, 0.9,
		"enclosing-type receiver evidence binds confidently")

	write := accessEdge(g, "Lib.cs::Ledger.<init>", "_store")
	require.NotNil(t, write, "_store = store in the ctor must produce a write edge")
	assert.Equal(t, graph.EdgeWrites, write.Kind)
	assert.True(t, strings.Contains(write.To, "Ledger._store"),
		"the write binds the same field, got %q", write.To)

	// The find_usages shape: the field node's incoming edges now carry
	// the field's life story — one read, one write, nothing phantom.
	reads, writes := 0, 0
	for _, e := range g.GetInEdges("Lib.cs::Ledger._store") {
		switch e.Kind {
		case graph.EdgeReads:
			reads++
		case graph.EdgeWrites:
			writes++
		}
	}
	assert.Equal(t, 1, reads, "exactly the one authored read site")
	assert.Equal(t, 1, writes, "exactly the one authored write site")
}
