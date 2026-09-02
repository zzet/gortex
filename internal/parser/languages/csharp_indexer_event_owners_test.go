package languages

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

// Indexers and events with accessors were not emitted as nodes at all,
// so the owner lookup could not name the member holding their accessor
// bodies and every call in them was dropped at the funcRanges gate -
// the same loss properties took before they recorded byte extents. Both
// kinds now mint a member node and record their declaration span, so an
// accessor body owns its calls the way a property accessor does.
//
// The indexer has no name in the grammar, so it is spelled `this[]`:
// no legal C# identifier can collide with it, unlike the CLR metadata
// name `Item`, which a class may genuinely declare alongside an indexer
// under [IndexerName].
func TestCSharpExtractor_IndexerAndEventBodiesOwnTheirCalls(t *testing.T) {
	src := []byte(`namespace App {
    public class Hoist {
        public int Lift() { return 1; }
        public void Drop(int v) { }
        public void Bind(System.EventHandler h) { }
        public void Free(System.EventHandler h) { }
    }

    public class Derrick {
        private readonly Hoist _hoist = new Hoist();

        public int this[int i] {
            get { return _hoist.Lift(); }
            set { _hoist.Drop(i); }
        }

        public event System.EventHandler Swing {
            add { _hoist.Bind(value); }
            remove { _hoist.Free(value); }
        }

        public int Plain() { return _hoist.Lift(); }
    }

    public class Gantry {
        private readonly Hoist _hoist = new Hoist();

        public int this[int i] => _hoist.Lift();
    }
}
`)
	e := NewCSharpExtractor()
	result, err := e.Extract("App.cs", src)
	require.NoError(t, err)

	for _, row := range []struct {
		shape, owner, method string
		count                int
	}{
		{"indexer get accessor", "App.cs::Derrick.this[]", "Lift", 1},
		{"indexer set accessor", "App.cs::Derrick.this[]", "Drop", 1},
		{"event add accessor", "App.cs::Derrick.Swing", "Bind", 1},
		{"event remove accessor", "App.cs::Derrick.Swing", "Free", 1},
		{"expression-bodied indexer", "App.cs::Gantry.this[]", "Lift", 1},
	} {
		t.Run(row.shape, func(t *testing.T) {
			assert.Len(t, callEdgesFrom(result.Edges, row.owner, row.method), row.count,
				"accessor-body call must attribute to the member that holds it")
		})
	}

	t.Run("ordinary method control", func(t *testing.T) {
		require.Len(t, callEdgesFrom(result.Edges, "App.cs::Derrick.Plain", "Lift"), 1,
			"the shape that always worked must stay alive")
	})

	t.Run("members are nodes with their owner and kind", func(t *testing.T) {
		byID := map[string]*graph.Node{}
		for _, n := range result.Nodes {
			byID[n.ID] = n
		}
		for id, kind := range map[string]string{
			"App.cs::Derrick.this[]": "indexer",
			"App.cs::Derrick.Swing":  "event_accessor",
			"App.cs::Gantry.this[]":  "indexer",
		} {
			n := byID[id]
			if !assert.NotNil(t, n, "member node %s must exist", id) {
				continue
			}
			assert.Equal(t, graph.KindField, n.Kind, "%s rides the property-shaped member kind", id)
			assert.Equal(t, kind, n.Meta["kind"], "%s carries its member kind", id)
		}
		var memberOf int
		for _, ed := range result.Edges {
			if ed.Kind == graph.EdgeMemberOf && ed.To == "App.cs::Derrick" &&
				(ed.From == "App.cs::Derrick.this[]" || ed.From == "App.cs::Derrick.Swing") {
				memberOf++
			}
		}
		assert.Equal(t, 2, memberOf, "both members belong to their declaring type")
	})
}

// Two indexers on one type differ only by parameter list, so they
// collide on the name-keyed node ID. Overloaded METHODS already resolve
// this by suffixing the second declaration with its line; indexers take
// the same route rather than inventing a second convention, so each
// overload keeps its own node and owns its own body.
func TestCSharpExtractor_OverloadedIndexersEachOwnTheirBody(t *testing.T) {
	src := []byte(`namespace App {
    public class Hoist {
        public int Lift() { return 1; }
        public int Haul() { return 2; }
    }
    public class Winder {
        private readonly Hoist _hoist = new Hoist();
        public int this[int i] { get { return _hoist.Lift(); } }
        public int this[string k] { get { return _hoist.Haul(); } }
    }
}
`)
	e := NewCSharpExtractor()
	result, err := e.Extract("App.cs", src)
	require.NoError(t, err)

	assert.Len(t, callEdgesFrom(result.Edges, "App.cs::Winder.this[]", "Lift"), 1,
		"the first indexer keeps the unsuffixed node")
	assert.Len(t, callEdgesFrom(result.Edges, "App.cs::Winder.this[]_L9", "Haul"), 1,
		"the second indexer takes the line-suffixed node, as overloaded methods do")
}

// A partial indexer is NOT an overload: C# 13 splits one member across a
// declaring fragment and an implementing one, and either may extract
// first. Suffixing the second would leave the name-keyed ID - the one
// anything queries - pointing at the bodyless fragment while the code
// sat under a line-suffixed twin. The `partial` modifier separates the
// two cases exactly, because C# requires both fragments to spell it and
// forbids it on an overload.
func TestCSharpExtractor_PartialIndexerMergesOntoOneMember(t *testing.T) {
	src := []byte(`namespace App {
    public class Hoist { public int Lift() { return 1; } }
    public partial class Winder {
        private Hoist _h = new Hoist();
        public partial int this[int i] { get; }
    }
    public partial class Winder {
        public partial int this[int i] { get { return _h.Lift(); } }
    }
}
`)
	e := NewCSharpExtractor()
	result, err := e.Extract("App.cs", src)
	require.NoError(t, err)

	var ids []string
	for _, n := range result.Nodes {
		if strings.Contains(n.ID, "this[]") {
			ids = append(ids, n.ID)
		}
	}
	assert.Equal(t, []string{"App.cs::Winder.this[]"}, ids,
		"the two fragments are one member, not a member and a line-suffixed twin")
	assert.Len(t, callEdgesFrom(result.Edges, "App.cs::Winder.this[]", "Lift"), 1,
		"the implementing fragment's call rides the canonical node")
}

// The extraction half of issue #728. An indexer sharing a physical line
// with a property put the semantic tier in a bind: the indexer owned no
// node, so the extractor's line fallback parked its body call on the
// property, and #722's caller adoption promoted that stub to a confident
// resolved edge on a member whose entire body is `=> 1`. The byte-extent
// refusal added in #720's revision stopped the false attribution; the
// member node here is what turns the refusal back into a real edge.
func TestCSharpExtractor_SharedLineIndexerDoesNotStealThePropertysOwnership(t *testing.T) {
	src := []byte(`namespace App {
    public class Hoist {
        public int Lift() { return 1; }
    }
    public class Ledge {
        private readonly Hoist _hoist = new Hoist();
        public int Slot => 1; public int this[int i] { get { return _hoist.Lift(); } }
    }
}
`)
	e := NewCSharpExtractor()
	result, err := e.Extract("App.cs", src)
	require.NoError(t, err)

	// This half pins #720's byte-extent refusal, not the emission: it
	// holds with emission disabled too, because the refusal already
	// stopped the property collecting the call. It is the second
	// assertion that this change makes true.
	assert.Empty(t, callEdgesFrom(result.Edges, "App.cs::Ledge.Slot", "Lift"),
		"a property whose body is `=> 1` can call nothing")
	assert.Len(t, callEdgesFrom(result.Edges, "App.cs::Ledge.this[]", "Lift"), 1,
		"the indexer sharing the line owns its own accessor call")
}
