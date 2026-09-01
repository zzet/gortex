package languages

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

// Calls inside property accessor bodies used to vanish outright: the
// call-owner lookup admitted only methods and constructors, so a call
// whose enclosing member was a property found no owner and was dropped
// at the funcRanges gate — not misattributed, gone (round-23 catch AC1,
// 3 of 4 sites in the probe cell). Properties now record byte extents
// and join the owner lookup, so every accessor form owns its calls.
//
// Each row is one accessor shape; the assertion is the call edge FROM
// the property node. The expression-bodied METHOD control at the bottom
// pins the one shape that always worked, so a regression that silenced
// calls wholesale cannot pass this test.
func TestCSharpExtractor_AccessorBodyCallsAttributeToTheProperty(t *testing.T) {
	src := []byte(`namespace App {
    public class Crank {
        public int Turn() { return 1; }
        public void Push(int v) { }
        public int Get() { return 2; }
        public void Prime(int v) { }
        public static int Spin() { return 3; }
        public int Feed(System.Func<int> f) { return f(); }
    }

    public class GaugeFace {
        private readonly Crank _crank = new Crank();
        private int _stored;

        public int Reading {
            get { return _crank.Turn(); }
            set { _stored = value + _crank.Turn(); }
        }

        public int Snapshot => _crank.Turn();

        public int Flow {
            get => _crank.Get();
            set => _crank.Push(value);
        }

        public int Sealed {
            get { return _crank.Get(); }
            init { _crank.Prime(value); }
        }

        public static int Global {
            get { return Crank.Spin(); }
        }

        public int Wrapped {
            get { return _crank.Feed(() => _crank.Turn()); }
        }

        public int Plain { get; set; }

        public int Tally() => _crank.Turn();
    }

    public class Box<T> {
        private readonly Crank _crank = new Crank();
        public int Item => _crank.Turn();
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
		{"block get", "App.cs::GaugeFace.Reading", "Turn", 2}, // get + set both call Turn
		{"expression-bodied property", "App.cs::GaugeFace.Snapshot", "Turn", 1},
		{"expression-bodied get", "App.cs::GaugeFace.Flow", "Get", 1},
		{"expression-bodied set", "App.cs::GaugeFace.Flow", "Push", 1},
		{"init accessor", "App.cs::GaugeFace.Sealed", "Prime", 1},
		{"static property accessor", "App.cs::GaugeFace.Global", "Spin", 1},
		{"lambda inside accessor", "App.cs::GaugeFace.Wrapped", "Turn", 1},
		{"generic container property", "App.cs::Box.Item", "Turn", 1},
	} {
		t.Run(row.shape, func(t *testing.T) {
			edges := callEdgesFrom(result.Edges, row.owner, row.method)
			assert.Len(t, edges, row.count,
				"accessor-body call must attribute to the property node")
		})
	}

	t.Run("expression-bodied method control", func(t *testing.T) {
		require.Len(t, callEdgesFrom(result.Edges, "App.cs::GaugeFace.Tally", "Turn"), 1,
			"the one site AC1 left alive must stay alive")
	})

	t.Run("auto-property emits no calls", func(t *testing.T) {
		var fromPlain int
		for _, ed := range result.Edges {
			if ed.From == "App.cs::GaugeFace.Plain" && ed.Kind == graph.EdgeCalls {
				fromPlain++
			}
		}
		assert.Zero(t, fromPlain, "an auto-property has no body to own calls")
	})
}

// A property with an initializer spans its `= Call()` bytes, so the
// initializer call belongs to the property node the same way an
// accessor-body call does.
func TestCSharpExtractor_PropertyInitializerCallAttributesToTheProperty(t *testing.T) {
	src := []byte(`namespace App {
    public class Seeder {
        public static int SeedValue() { return 7; }
    }
    public class Config {
        public int Seeded { get; set; } = Seeder.SeedValue();
    }
}
`)
	e := NewCSharpExtractor()
	result, err := e.Extract("App.cs", src)
	require.NoError(t, err)

	assert.Len(t, callEdgesFrom(result.Edges, "App.cs::Config.Seeded", "SeedValue"), 1,
		"property-initializer call must attribute to the property node")
}

// A field initializer is executable code with no method around it. The
// field declarator's own byte span owns the call — per DECLARATOR, so a
// multi-declarator line (`int a = F(), b = G();`) hands each call to
// the field it actually initializes.
func TestCSharpExtractor_FieldInitializerCallAttributesToItsField(t *testing.T) {
	src := []byte(`namespace App {
    public class Seeder {
        public static int A() { return 1; }
        public static int B() { return 2; }
    }
    public class Config {
        private int _lone = Seeder.A();
        private int _first = Seeder.A(), _second = Seeder.B();
        private static readonly int Shared = Seeder.B();
        private int _bare;
    }
}
`)
	e := NewCSharpExtractor()
	result, err := e.Extract("App.cs", src)
	require.NoError(t, err)

	for _, row := range []struct{ shape, owner, method string }{
		{"single declarator", "App.cs::Config._lone", "A"},
		{"first of two declarators", "App.cs::Config._first", "A"},
		{"second of two declarators", "App.cs::Config._second", "B"},
		{"static readonly", "App.cs::Config.Shared", "B"},
	} {
		t.Run(row.shape, func(t *testing.T) {
			assert.Len(t, callEdgesFrom(result.Edges, row.owner, row.method), 1,
				"initializer call must attribute to its own field node")
		})
	}
}

// findCallEdge returns the single EdgeCalls from fromID whose unresolved
// target names method, failing the test on any other cardinality.
func findCallEdge(t *testing.T, edges []*graph.Edge, fromID, method string) *graph.Edge {
	t.Helper()
	found := callEdgesFrom(edges, fromID, method)
	require.Len(t, found, 1, "expected exactly one %s call from %s", method, fromID)
	return found[0]
}

// Inside a set/init accessor `value` is ALWAYS the implicit parameter -
// a same-named member is only reachable via this.value - and its type
// is DECLARED: it is the property type. The seed is therefore an
// OFFSET-SCOPED record over each set/init accessor's byte extent, never
// an owner-wide entry: in the GETTER the bare name means a member
// (possibly inherited, possibly declared in another partial fragment),
// and an owner-wide seed typed those sites with the property type - a
// confident wrong answer. The scope registration also keeps the
// field-identifier lane from minting a field-use edge for the
// parameter.
func TestCSharpExtractor_SetterValueCarriesThePropertyType(t *testing.T) {
	src := []byte(`namespace App {
    public class Crank {
        public int Turn() { return 1; }
        public void Prime(int v) { }
    }
    public class Widget {
        public int Spin() { return 2; }
    }
    public class Sink {
        public Crank Feed {
            set { _ = value.Turn(); }
        }
        public Crank Boot {
            get { return null; }
            init { value.Prime(1); }
        }
    }
    public class Clash {
        private readonly Crank value = new Crank();
        public Widget Feed {
            set { _ = value.Spin(); }
        }
        public Crank Show {
            get { return value.Turn(); }
        }
    }
    public class Dirty {
        public Crank Both {
            get { Crank value = new Crank(); return value; }
            set { value.Prime(2); }
        }
    }
    public class VBase {
        protected Crank value = new Crank();
    }
    public class VDerived : VBase {
        public Widget Feed {
            get { _ = value.Turn(); return null; }
            set { }
        }
    }
}
`)
	e := NewCSharpExtractor()
	result, err := e.Extract("App.cs", src)
	require.NoError(t, err)

	t.Run("set accessor", func(t *testing.T) {
		ed := findCallEdge(t, result.Edges, "App.cs::Sink.Feed", "Turn")
		assert.Equal(t, "Crank", ed.Meta["receiver_type"], "value is declared Crank by the property")
		assert.Nil(t, ed.Meta["receiver_name"], "value is a parameter, not field evidence")
	})
	t.Run("init accessor", func(t *testing.T) {
		ed := findCallEdge(t, result.Edges, "App.cs::Sink.Boot", "Prime")
		assert.Equal(t, "Crank", ed.Meta["receiver_type"])
	})
	t.Run("member named value never beats the setter parameter", func(t *testing.T) {
		ed := findCallEdge(t, result.Edges, "App.cs::Clash.Feed", "Spin")
		assert.Equal(t, "Widget", ed.Meta["receiver_type"],
			"in a set accessor value IS the parameter; the Crank field needs this.value")
		var reads int
		for _, r := range result.Edges {
			if r.From == "App.cs::Clash.Feed" && r.Kind == graph.EdgeReads {
				reads++
			}
		}
		assert.Zero(t, reads, "the parameter must not mint field-use evidence")
	})
	t.Run("getter value means the member", func(t *testing.T) {
		ed := findCallEdge(t, result.Edges, "App.cs::Clash.Show", "Turn")
		assert.Nil(t, ed.Meta["receiver_type"],
			"the seed is set/init-scoped; the getter's value is the field, left to field evidence")
	})
	t.Run("typed getter local named value cannot kill the seed", func(t *testing.T) {
		ed := findCallEdge(t, result.Edges, "App.cs::Dirty.Both", "Prime")
		assert.Equal(t, "Crank", ed.Meta["receiver_type"],
			"the setter-span record answers Found at the setter site regardless of getter locals")
	})
	t.Run("inherited value member is not property-typed", func(t *testing.T) {
		ed := findCallEdge(t, result.Edges, "App.cs::VDerived.Feed", "Turn")
		assert.NotEqual(t, "Widget", ed.Meta["receiver_type"],
			"the getter's value is the inherited Crank field; stamping the property type is a confident wrong answer")
	})
}

// A C# 13 partial property splits declaration and implementation across
// fragments, and either may extract first. The seen[] dedup mints one
// node, but the extents (and the value spans) must come from the
// body-bearing fragment - keyed to the declaring fragment, every
// accessor call in the implementation died ownerless, byte-for-byte the
// AC1 failure mode this branch exists to fix.
func TestCSharpExtractor_PartialPropertyImplementationOwnsItsCalls(t *testing.T) {
	src := []byte(`namespace App {
    public class Crank {
        public int Turn() { return 1; }
        public void Prime(int v) { }
    }
    public partial class KPart {
        private readonly Crank _crank = new Crank();
        public partial int P { get; set; }
    }
    public partial class KPart {
        public partial int P {
            get { return _crank.Turn(); }
            set { value.ToString(); _crank.Prime(value); }
        }
    }
}
`)
	e := NewCSharpExtractor()
	result, err := e.Extract("App.cs", src)
	require.NoError(t, err)

	assert.Len(t, callEdgesFrom(result.Edges, "App.cs::KPart.P", "Turn"), 1,
		"the implementing fragment's get body owns its call")
	assert.Len(t, callEdgesFrom(result.Edges, "App.cs::KPart.P", "Prime"), 1,
		"the implementing fragment's set body owns its call")
}

// The reference-form and MediatR lanes attribute by their own ranges
// lookup; keyed to methods only, an accessor body split its evidence -
// the calls edge on the property, the instantiates edge on the FILE
// node, and the MediatR dispatch placeholder dropped outright.
func TestCSharpExtractor_ReferenceFormsInAccessorsOwnTheProperty(t *testing.T) {
	src := []byte(`namespace App {
    public class Crank {
        public int Turn() { return 1; }
    }
    public class PingCmd { }
    public class Holder {
        private readonly object _mediator = null;
        public int Made {
            get { var c = new Crank(); return c.Turn(); }
        }
        public int Kick {
            get { ((MediatR.IMediator)_mediator).Send(new PingCmd()); return 1; }
        }
    }
}
`)
	e := NewCSharpExtractor()
	result, err := e.Extract("App.cs", src)
	require.NoError(t, err)

	t.Run("instantiates edge owner", func(t *testing.T) {
		var fromProp, fromFile bool
		for _, ed := range result.Edges {
			if ed.Kind != graph.EdgeInstantiates || !strings.Contains(ed.To, "Crank") {
				continue
			}
			switch ed.From {
			case "App.cs::Holder.Made":
				fromProp = true
			case "App.cs":
				fromFile = true
			}
		}
		assert.True(t, fromProp, "the accessor body's new Crank() belongs to the property")
		assert.False(t, fromFile, "the file node is not the owner of code inside a member")
	})

	t.Run("mediatr dispatch site in an accessor", func(t *testing.T) {
		var found bool
		for _, ed := range result.Edges {
			if ed.From == "App.cs::Holder.Kick" && ed.To == "unresolved::*.Handle" {
				found = true
			}
		}
		assert.True(t, found, "the Send site inside the accessor must stamp its dispatch placeholder")
	})
}

// Property and field nodes are call owners now, and the resolver's
// scoped-usings narrowing reads the caller's scope_ns - a member kind
// without the stamp answered the namespace question with an empty
// scope. Same stamp methods and constructors already carry.
func TestCSharpExtractor_PropertyAndFieldCarryScopeNS(t *testing.T) {
	src := []byte(`namespace App.Inner {
    public class Holder {
        private int _seed = 1;
        public int Prop { get; set; }
    }
}
`)
	e := NewCSharpExtractor()
	result, err := e.Extract("App.cs", src)
	require.NoError(t, err)

	for _, id := range []string{"App.cs::Holder.Prop", "App.cs::Holder._seed"} {
		var found *graph.Node
		for _, n := range result.Nodes {
			if n.ID == id {
				found = n
			}
		}
		require.NotNil(t, found, id)
		assert.Equal(t, "App.Inner", found.Meta["scope_ns"], "%s must carry its enclosing namespace", id)
	}
}

// The discriminating shape for "initializer-less declarators record
// nothing": a bare field sharing a line with a call-bearing member. If
// the bare declarator entered the owner set its 1-line range would tie
// the line; staying out, the expression-bodied property owns its call
// byte-precisely and the bare field owns nothing. Also pins two
// expression-bodied properties sharing one line - each owns its own
// call through its own byte span.
func TestCSharpExtractor_OneLineNeighborsKeepTheirOwners(t *testing.T) {
	src := []byte(`namespace App {
    public class Crank {
        public int Turn() { return 1; }
        public int Spin() { return 2; }
    }
    public class Tight {
        private readonly Crank _c = new Crank();
        private int _bare; public int Q => _c.Turn();
        public int A => _c.Turn(); public int B => _c.Spin();
    }
}
`)
	e := NewCSharpExtractor()
	result, err := e.Extract("App.cs", src)
	require.NoError(t, err)

	qEdges := callEdgesFrom(result.Edges, "App.cs::Tight.Q", "Turn")
	require.Len(t, qEdges, 1,
		"the property owns its call despite the bare field on its line")
	assert.Nil(t, qEdges[0].Meta["receiver_ambiguous"],
		"a codeless declarator must not enter the owner set - its 1-line range would tie the line and stamp a refusal on the property's call")
	var fromBare int
	for _, ed := range result.Edges {
		if ed.From == "App.cs::Tight._bare" && ed.Kind == graph.EdgeCalls {
			fromBare++
		}
	}
	assert.Zero(t, fromBare, "an initializer-less declarator holds no code and owns no calls")
	assert.Len(t, callEdgesFrom(result.Edges, "App.cs::Tight.A", "Turn"), 1,
		"first of two same-line properties owns its own call")
	assert.Len(t, callEdgesFrom(result.Edges, "App.cs::Tight.B", "Spin"), 1,
		"second of two same-line properties owns its own call")
	assert.Empty(t, callEdgesFrom(result.Edges, "App.cs::Tight.A", "Spin"),
		"no cross-theft between same-line properties")
}
