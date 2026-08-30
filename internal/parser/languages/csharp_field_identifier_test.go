package languages

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

// C# fields are read through bare identifiers, not dotted access: an
// injected field is used as a call receiver (`_store.Add(1)`), an access
// receiver (`_store.Count`), or an assignment target (`_store = store`).
// None of those positions previously emitted any edge naming the FIELD
// itself, so find_usages on a field answered empty no matter how often
// the class used it. Only the unidiomatic `this._store` spelling left a
// trace. These tests pin the field-identifier emission: reads/writes of
// the enclosing type's own fields, with declared locals, parameters, and
// builtin-typed locals shadowing the field name.
func TestCSharpExtractor_FieldIdentifierUses(t *testing.T) {
	src := []byte(`namespace App {
    public class Store {
        public void Add(int n) { }
        public int Count { get; set; }
    }
    public class Ledger {
        private readonly Store _store;
        private int _total;
        private int _unused;

        public Ledger(Store store) { _store = store; }

        public void Post() { _store.Add(1); }
        public int Peek() { return _store.Count; }
        public void Tally() { _total = 5; }
        public void Shadowed() { var _store = new Store(); _store.Add(2); }
        public void ShadowParam(Store _store) { _store.Add(3); }
    }
}
`)
	e := NewCSharpExtractor()
	result, err := e.Extract("Ledger.cs", src)
	require.NoError(t, err)

	// Call-receiver read: `_store.Add(1)` reads the field _store.
	post := accessEdges(result.Edges, "Ledger.cs::Ledger.Post", "_store")
	require.Len(t, post, 1, "a field used as a call receiver is one read of the field")
	assert.Equal(t, graph.EdgeReads, post[0].Kind)
	require.NotNil(t, post[0].Meta)
	assert.Equal(t, "Ledger", post[0].Meta["receiver_type"],
		"the field's implicit receiver is the enclosing type")

	// Access-receiver read: `_store.Count` reads the field too (the Count
	// read is the access emitter's existing edge, asserted separately).
	peek := accessEdges(result.Edges, "Ledger.cs::Ledger.Peek", "_store")
	require.Len(t, peek, 1, "a field used as an access receiver is one read of the field")
	assert.Equal(t, graph.EdgeReads, peek[0].Kind)
	count := accessEdges(result.Edges, "Ledger.cs::Ledger.Peek", "Count")
	require.Len(t, count, 1, "the member access itself still emits its own read")

	// Constructor assignment: `_store = store` writes the field.
	ctor := accessEdges(result.Edges, "Ledger.cs::Ledger.<init>", "_store")
	require.Len(t, ctor, 1, "bare assignment lhs is one write of the field")
	assert.Equal(t, graph.EdgeWrites, ctor[0].Kind)
	assert.Equal(t, "Ledger", ctor[0].Meta["receiver_type"])

	// Bare-identifier assignment beyond the ctor: `_total = 5`.
	tally := accessEdges(result.Edges, "Ledger.cs::Ledger.Tally", "_total")
	require.Len(t, tally, 1)
	assert.Equal(t, graph.EdgeWrites, tally[0].Kind)

	// Shadowing: a declared local or parameter with the field's name owns
	// the identifier — no field edge may be minted from those methods.
	assert.Empty(t, accessEdges(result.Edges, "Ledger.cs::Ledger.Shadowed", "_store"),
		"a local named like the field shadows it")
	assert.Empty(t, accessEdges(result.Edges, "Ledger.cs::Ledger.ShadowParam", "_store"),
		"a parameter named like the field shadows it")

	// Control: the untouched field keeps its honest empty.
	for _, ed := range result.Edges {
		if ed.To == "unresolved::*._unused" {
			t.Fatalf("unused field gained an edge from %s", ed.From)
		}
	}
}

// Field reads INSIDE lambda bodies. The original production miss that
// motivated the field-identifier emitter reproduced with the field read
// sitting inside a `() => { ... }` block lambda in a dictionary
// initializer assigned in the constructor — post-fix, that exact shape
// still answered zero edges. Pin both lambda positions: a block lambda
// inside a collection-initializer entry (the production shape) and a
// plain expression lambda in an ordinary method. The edge's owner is the
// enclosing DECLARED member (ctor/method) — lambdas don't mint owners of
// their own.
func TestCSharpExtractor_FieldIdentifierUsesInsideLambdas(t *testing.T) {
	src := []byte(`namespace App {
    public class Store {
        public int Read(Func<int, bool> f) { return 1; }
    }
    public class Reporter {
        private readonly Store _lookup;
        private readonly Dictionary<string, Func<string>> _tags;

        public Reporter(Store lookup) {
            _lookup = lookup;
            _tags = new Dictionary<string, Func<string>>
            {
                { "A", () =>
                    {
                        var item = _lookup.Read(x => x > 0);
                        return item.ToString();
                    }
                },
            };
        }

        public int Direct() {
            return _lookup.Read(x => x > 2);
        }
    }
}
`)
	e := NewCSharpExtractor()
	result, err := e.Extract("Reporter.cs", src)
	require.NoError(t, err)

	// The ctor owns two _lookup edges: the assignment write and the read
	// from inside the initializer entry's block lambda.
	ctor := accessEdges(result.Edges, "Reporter.cs::Reporter.<init>", "_lookup")
	require.Len(t, ctor, 2, "ctor: assignment write + block-lambda call-receiver read")
	kinds := map[graph.EdgeKind]int{}
	for _, ed := range ctor {
		kinds[ed.Kind]++
	}
	assert.Equal(t, 1, kinds[graph.EdgeWrites], "the `_lookup = lookup` assignment")
	assert.Equal(t, 1, kinds[graph.EdgeReads], "the read inside the block lambda")

	// An expression lambda argument does not swallow the receiving
	// field's read in an ordinary method either.
	direct := accessEdges(result.Edges, "Reporter.cs::Reporter.Direct", "_lookup")
	require.Len(t, direct, 1, "expression-lambda argument: the field is still the call receiver")
	assert.Equal(t, graph.EdgeReads, direct[0].Kind)
}

// fieldEdgesTo filters a result's EdgeReads/EdgeWrites by the unresolved
// field name alone, owner-agnostic — for constructor shapes whose owner
// IDs are the variable under test.
func fieldEdgesTo(edges []*graph.Edge, name string) []*graph.Edge {
	var out []*graph.Edge
	for _, ed := range edges {
		if ed.Kind != graph.EdgeReads && ed.Kind != graph.EdgeWrites {
			continue
		}
		if ed.To == "unresolved::*."+name {
			out = append(out, ed)
		}
	}
	return out
}

// Constructor-shape coverage beyond the classic block-bodied instance
// ctor: expression-bodied ctors, static ctors, and C# 12 primary
// constructors all put field writes/reads in positions the emitter must
// still own. Each class isolates one shape with its own field name so a
// miss identifies itself.
func TestCSharpExtractor_FieldIdentifierUsesAcrossCtorShapes(t *testing.T) {
	src := []byte(`namespace App {
    public class Store {
        public void Add(int n) { }
    }

    public class ExprCtor {
        private Store _expr;
        public ExprCtor(Store s) => _expr = s;
    }

    public class StaticCtor {
        private static Store _shared;
        static StaticCtor() { _shared = new Store(); }
        public void Use() { _shared.Add(1); }
    }

    public class Primary(Store s) {
        private readonly Store _prim = s;
        public void Use() { _prim.Add(2); }
    }
}
`)
	e := NewCSharpExtractor()
	result, err := e.Extract("Ctors.cs", src)
	require.NoError(t, err)

	// Expression-bodied ctor: `=> _expr = s` writes the field.
	expr := fieldEdgesTo(result.Edges, "_expr")
	require.Len(t, expr, 1, "expression-bodied ctor assignment is one write")
	assert.Equal(t, graph.EdgeWrites, expr[0].Kind)

	// Static ctor write + ordinary static-field read.
	shared := fieldEdgesTo(result.Edges, "_shared")
	require.Len(t, shared, 2, "static-ctor write + Use() call-receiver read")
	sharedKinds := map[graph.EdgeKind]int{}
	for _, ed := range shared {
		sharedKinds[ed.Kind]++
	}
	assert.Equal(t, 1, sharedKinds[graph.EdgeWrites])
	assert.Equal(t, 1, sharedKinds[graph.EdgeReads])

	// Primary-ctor class: the field is still usable from methods (the
	// `= s` initializer itself is the declaration, not a tracked write).
	prim := fieldEdgesTo(result.Edges, "_prim")
	require.Len(t, prim, 1, "primary-ctor class: method call-receiver read")
	assert.Equal(t, graph.EdgeReads, prim[0].Kind)
}
