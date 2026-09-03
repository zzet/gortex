package languages

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

// implementsMeta returns the Meta of the implements/extends edge From fromID
// whose unresolved target names base, and whether that edge exists at all
// (its Meta is legitimately nil when nothing was stamped).
func implementsMeta(edges []*graph.Edge, fromID, base string) (map[string]any, bool) {
	for _, e := range edges {
		if e == nil || (e.Kind != graph.EdgeImplements && e.Kind != graph.EdgeExtends) {
			continue
		}
		if e.From == fromID && e.To == "unresolved::"+base {
			return e.Meta, true
		}
	}
	return nil, false
}

// Generic base-list entries carry their CLOSED type arguments on the
// implements/extends edge (target_type_args), so the interface-dispatch
// fan-out can exclude type-impossible implementations: an IBoxStore<Crate>
// receiver can never dispatch into the class implementing IBoxStore<Widget>.
// Open arguments (the declaring type's own type parameters) and non-simple
// arguments (nested generics, arrays, nullables) stamp NOTHING - absence of
// the stamp means "do not filter", never "no arguments".
func TestCSharpExtractor_BaseListTypeArgs(t *testing.T) {
	src := []byte(`namespace App {
    public interface IBoxStore<T> { }
    public class Crate { }

    public class CrateBoxStore : IBoxStore<Crate> { }

    // Open generic: T is the class's own parameter - no stamp.
    public class Relay<T> : IBoxStore<T> { }

    // Qualified argument normalizes to its last segment.
    public class DeepStore : IBoxStore<App.Crate> { }

    // Nested generic argument is not simple - no stamp.
    public class ListStore : IBoxStore<System.Collections.Generic.List<int>> { }

    // Non-generic base keeps no stamp at all.
    public class PlainStore : IBoxStore<Crate>, System.IDisposable { }

    public interface IPair<K, V> { }
    public class PairStore : IPair<int, Crate> { }
}
`)
	e := NewCSharpExtractor()
	result, err := e.Extract("Stores.cs", src)
	require.NoError(t, err)

	closed, ok := implementsMeta(result.Edges, "Stores.cs::CrateBoxStore", "IBoxStore")
	require.True(t, ok, "implements edge must exist")
	assert.Equal(t, "Crate", closed["target_type_args"])

	open, ok := implementsMeta(result.Edges, "Stores.cs::Relay", "IBoxStore")
	require.True(t, ok)
	assert.NotContains(t, open, "target_type_args",
		"an open type parameter closes nothing - no stamp, no filtering")

	qualified, ok := implementsMeta(result.Edges, "Stores.cs::DeepStore", "IBoxStore")
	require.True(t, ok)
	assert.Equal(t, "Crate", qualified["target_type_args"],
		"namespace-qualified argument normalizes to its last segment")

	nested, ok := implementsMeta(result.Edges, "Stores.cs::ListStore", "IBoxStore")
	require.True(t, ok)
	assert.NotContains(t, nested, "target_type_args",
		"nested generic arguments are not simple - no stamp")

	plainIface, ok := implementsMeta(result.Edges, "Stores.cs::PlainStore", "IDisposable")
	require.True(t, ok)
	assert.NotContains(t, plainIface, "target_type_args",
		"a non-generic base entry carries no stamp")

	// Multi-argument closures stamp the full normalized list.
	pair, ok := implementsMeta(result.Edges, "Stores.cs::PairStore", "IPair")
	require.True(t, ok)
	assert.Equal(t, "int,Crate", pair["target_type_args"])
}

// Review findings (G9 gate, first pass): three shapes where a stamp (or a
// receiver reading) treated OPEN or AMBIGUOUS arguments as closed and
// silently suppressed real fan-out edges. Each pins "stamp nothing".
func TestCSharpExtractor_BaseListTypeArgsOpenAndAmbiguousShapes(t *testing.T) {
	src := []byte(`namespace App {
    public interface IBoxStore<T> { }
    public interface IPlain { }
    public class Crate { }
    public class Widget { }
    public class Outer2<T> {
        public interface IInner { }
    }

    // RED 1 (extractor half): a type nested in a generic OUTER uses the
    // outer's parameter - open, not closed.
    public class Outer<T> {
        public class Inner : IBoxStore<T> { }
    }

    // RED 2: one base list closing the SAME interface twice collapses to
    // one edge upstream - the ambiguity is invisible downstream, so
    // neither closure may stamp.
    public class Both : IBoxStore<Crate>, IBoxStore<Widget> { }

    // Important 1: a qualified base whose GENERIC segment is not the
    // final one must not stamp the outer segment's arguments - here onto
    // an interface with no parameters at all.
    public class Q2 : Outer2<int>.IInner { }
}
`)
	e := NewCSharpExtractor()
	result, err := e.Extract("Shapes.cs", src)
	require.NoError(t, err)

	inner, ok := implementsMeta(result.Edges, "Shapes.cs::Inner", "IBoxStore")
	require.True(t, ok)
	assert.NotContains(t, inner, "target_type_args",
		"the enclosing generic type's parameter is open - no stamp")

	both, ok := implementsMeta(result.Edges, "Shapes.cs::Both", "IBoxStore")
	require.True(t, ok)
	assert.NotContains(t, both, "target_type_args",
		"double closure of one interface collapses to one edge - neither closure may stamp")

	q2, ok := implementsMeta(result.Edges, "Shapes.cs::Q2", "IInner")
	require.True(t, ok)
	assert.NotContains(t, q2, "target_type_args",
		"a non-final generic segment's arguments never stamp the final target")
}

// Codex review RED: the gate compares SPELLINGS, but C# types have alias
// spellings. `IBox<System.Int32>` and `IBox<int>` are the SAME constructed
// interface - both sides must stamp the keyword canonical form ("int") or
// the gate suppresses a valid edge. And a `using X = ...` alias is opaque
// at extraction time (it may spell any type), so an argument matching one
// stamps nothing.
func TestCSharpExtractor_TypeArgsAliasCanonicalization(t *testing.T) {
	src := []byte(`using MyCrate = App.Crate;

namespace App {
    public interface IBoxStore<T> { }
    public class Crate { }

    public class IntBoxA : IBoxStore<System.Int32> { }
    public class IntBoxB : IBoxStore<int> { }
    public class IntBoxC : IBoxStore<Int32> { }

    public class AliasStore : IBoxStore<MyCrate> { }

    public class Flow {
        private readonly IBoxStore<System.Int32> _clr;
        private readonly IBoxStore<int> _kw;
        private readonly IBoxStore<MyCrate> _aliased;
    }
}
`)
	e := NewCSharpExtractor()
	result, err := e.Extract("Alias.cs", src)
	require.NoError(t, err)

	for _, cls := range []string{"IntBoxA", "IntBoxB", "IntBoxC"} {
		m, ok := implementsMeta(result.Edges, "Alias.cs::"+cls, "IBoxStore")
		require.True(t, ok, cls)
		assert.Equal(t, "int", m["target_type_args"],
			"%s: every BCL alias spelling folds to the keyword canonical form", cls)
	}

	aliased, ok := implementsMeta(result.Edges, "Alias.cs::AliasStore", "IBoxStore")
	require.True(t, ok)
	assert.NotContains(t, aliased, "target_type_args",
		"a using-alias argument is opaque - no stamp")

	clr := fieldMeta(result.Nodes, "Alias.cs::Flow._clr")
	require.NotNil(t, clr)
	assert.Equal(t, "int", clr["field_type_args"])
	kw := fieldMeta(result.Nodes, "Alias.cs::Flow._kw")
	require.NotNil(t, kw)
	assert.Equal(t, "int", kw["field_type_args"])
	al := fieldMeta(result.Nodes, "Alias.cs::Flow._aliased")
	require.NotNil(t, al)
	assert.NotContains(t, al, "field_type_args",
		"a using-alias argument is opaque - no stamp")
}

// Re-review RED: comment trivia between the tokens of a declared type is
// legal C# and no part of type identity - `IBox</**/Crate>` names the
// SAME constructed interface as `IBox<Crate>`. The raw-text path stamped
// "/**/Crate" as an argument and the gate then filtered the valid
// implementor; deriving the arguments from the parsed type AST keeps
// trivia out of the compared spelling.
func TestCSharpExtractor_FieldTypeArgTriviaIsNotIdentity(t *testing.T) {
	src := []byte(`namespace App {
    public interface IBox<T> { }
    public class Crate { }

    public class Flow {
        private readonly IBox</**/Crate> _box;
        public IBox</* seasonal */ Crate> Prop { get; set; }
    }
}
`)
	e := NewCSharpExtractor()
	result, err := e.Extract("Trivia.cs", src)
	require.NoError(t, err)

	box := fieldMeta(result.Nodes, "Trivia.cs::Flow._box")
	require.NotNil(t, box)
	assert.Equal(t, "Crate", box["field_type_args"],
		"comment trivia inside the argument list is not part of the argument")

	prop := fieldMeta(result.Nodes, "Trivia.cs::Flow.Prop")
	require.NotNil(t, prop)
	assert.Equal(t, "Crate", prop["field_type_args"])
}

// Re-review RED: declaration-side identifier ESCAPES must land in the
// same normalization domain the use side compares in. `class Outer<@T>`
// declares the open parameter T, but the raw spelling "@T" in the
// open-parameter set never meets the use side's normalized "T" - the
// field stamps a closed type called "T" and dispatch filters everything.
// The Unicode escape spelling of the same declaration fails identically.
func TestCSharpExtractor_EscapedDeclarationTypeParamsStayOpen(t *testing.T) {
	src := []byte(`namespace App {
    public interface IBox<T> { }

    public class Outer<@T> {
        private readonly IBox<@T> _verbatim;
        private readonly IBox<T> _bare;
    }

    public class Uni<\u0054> {
        private readonly IBox<T> _viaEscape;
    }
}
`)
	e := NewCSharpExtractor()
	result, err := e.Extract("Esc.cs", src)
	require.NoError(t, err)

	for _, id := range []string{"Esc.cs::Outer._verbatim", "Esc.cs::Outer._bare", "Esc.cs::Uni._viaEscape"} {
		m := fieldMeta(result.Nodes, id)
		require.NotNil(t, m, id)
		assert.NotContains(t, m, "field_type_args",
			"%s: every spelling of the open declaration parameter closes nothing", id)
	}
}

// Re-review RED: alias keys and use-side arguments compared in different
// normalization domains. The alias collector stored "@Entity" while the
// argument normalizer strips the verbatim prefix to "Entity" - the guard
// missed and the alias-opaque argument stamped as a closed type.
func TestCSharpExtractor_VerbatimAliasSpellingsAreOpaque(t *testing.T) {
	src := []byte(`using @Entity = App.Crate;

namespace App {
    public interface IBox<T> { }
    public class Crate { }

    public class Flow {
        private readonly IBox<@Entity> _verbatimUse;
        private readonly IBox<Entity> _bareUse;
    }
}
`)
	e := NewCSharpExtractor()
	result, err := e.Extract("VAlias.cs", src)
	require.NoError(t, err)

	for _, id := range []string{"VAlias.cs::Flow._verbatimUse", "VAlias.cs::Flow._bareUse"} {
		m := fieldMeta(result.Nodes, id)
		require.NotNil(t, m, id)
		assert.NotContains(t, m, "field_type_args",
			"%s: every spelling denoting the alias is opaque - no stamp", id)
	}
}

// Re-review RED (metadata half): the project-global alias stamp must
// carry the CANONICAL identifier, or the resolver-side guard compares
// "@Entity" against normalized stamps and never refuses.
func TestCSharpExtractor_GlobalAliasMetaIsCanonical(t *testing.T) {
	src := []byte(`global using @Entity = App.Crate;
`)
	e := NewCSharpExtractor()
	result, err := e.Extract("GAlias.cs", src)
	require.NoError(t, err)

	var fileMeta map[string]any
	for _, n := range result.Nodes {
		if n != nil && n.Kind == graph.KindFile {
			fileMeta = n.Meta
			break
		}
	}
	require.NotNil(t, fileMeta)
	assert.Equal(t, []string{"Entity"}, fileMeta["global_using_aliases"],
		"the alias identifier is stored canonically - verbatim prefix stripped")
}

// Re-review RED (P2): the synthesized positional-record property carries
// field_type but no field_type_args, so `record Flow(IBox<Crate> Store)`
// never narrows Store's dispatch. The same conservative rules apply: the
// record's own type parameter is open, an alias is opaque.
func TestCSharpExtractor_RecordPositionalPropTypeArgs(t *testing.T) {
	src := []byte(`using MyCrate = App.Crate;

namespace App {
    public interface IBox<T> { }
    public class Crate { }

    public record Flow(IBox<Crate> Store, Crate Plain);
    public record Open<T>(IBox<T> Store);
    public record Aliased(IBox<MyCrate> Store);
}
`)
	e := NewCSharpExtractor()
	result, err := e.Extract("Rec.cs", src)
	require.NoError(t, err)

	store := fieldMeta(result.Nodes, "Rec.cs::Flow.Store")
	require.NotNil(t, store)
	assert.Equal(t, "IBox<Crate>", store["field_type"])
	assert.Equal(t, "Crate", store["field_type_args"],
		"a positional property is a first-class receiver - same stamp as an ordinary property")

	plain := fieldMeta(result.Nodes, "Rec.cs::Flow.Plain")
	require.NotNil(t, plain)
	assert.NotContains(t, plain, "field_type_args")

	open := fieldMeta(result.Nodes, "Rec.cs::Open.Store")
	require.NotNil(t, open)
	assert.NotContains(t, open, "field_type_args",
		"the record's own type parameter is open - no stamp")

	aliased := fieldMeta(result.Nodes, "Rec.cs::Aliased.Store")
	require.NotNil(t, aliased)
	assert.NotContains(t, aliased, "field_type_args",
		"a using-alias argument is opaque - no stamp")
}

// fieldMeta returns the meta of the node with the given ID.
func fieldMeta(result_nodes []*graph.Node, id string) map[string]any {
	for _, n := range result_nodes {
		if n != nil && n.ID == id {
			return n.Meta
		}
	}
	return nil
}

// Field and property nodes carry field_type_args for CLOSED generic
// declared types - the receiver half of the dispatch gate reads this
// stamp instead of re-parsing field_type text, so the open/closed rules
// live in exactly one place (extraction, where the enclosing-type chain
// is visible).
func TestCSharpExtractor_FieldTypeArgsStamp(t *testing.T) {
	src := []byte(`namespace App {
    public interface IBoxStore<T> { }
    public class Crate { }

    public class Flow {
        private readonly IBoxStore<Crate> _store;
        public IBoxStore<Crate> Prop { get; set; }
        private Crate _plain;
    }

    // The enclosing generic's parameter is open even one nesting level
    // down - the field's owner itself declares no parameters.
    public class Outer<T> {
        public class NestedFlow {
            private IBoxStore<T> _open;
        }
    }
}
`)
	e := NewCSharpExtractor()
	result, err := e.Extract("Flow.cs", src)
	require.NoError(t, err)

	store := fieldMeta(result.Nodes, "Flow.cs::Flow._store")
	require.NotNil(t, store)
	assert.Equal(t, "Crate", store["field_type_args"])

	prop := fieldMeta(result.Nodes, "Flow.cs::Flow.Prop")
	require.NotNil(t, prop)
	assert.Equal(t, "Crate", prop["field_type_args"])

	plain := fieldMeta(result.Nodes, "Flow.cs::Flow._plain")
	require.NotNil(t, plain)
	assert.NotContains(t, plain, "field_type_args")

	open := fieldMeta(result.Nodes, "Flow.cs::NestedFlow._open")
	require.NotNil(t, open)
	assert.NotContains(t, open, "field_type_args",
		"the enclosing generic type's parameter is open - no stamp")
}

// RED 3: two same-named member calls on ONE line dedupe to a single
// unresolved companion edge carrying one arbitrary receiver_name - the
// receiver evidence is ambiguous and must say so, or the dispatch gate
// applies one receiver's arguments to the other call's fan-out.
func TestCSharpExtractor_SameLineSameNameCallsMarkReceiverAmbiguous(t *testing.T) {
	src := []byte(`namespace App {
    public class Store { public int Fetch(int id) { return id; } }
    public class Flow {
        private readonly Store _crates;
        private readonly Store _widgets;
        public int Pull() { return _crates.Fetch(_widgets.Fetch(1)); }
        public int Single() { return _crates.Fetch(2); }
    }
}
`)
	e := NewCSharpExtractor()
	result, err := e.Extract("Flow.cs", src)
	require.NoError(t, err)

	var ambiguous, single []*graph.Edge
	for _, ed := range result.Edges {
		if ed == nil || ed.Kind != graph.EdgeCalls || ed.To != "unresolved::*.Fetch" {
			continue
		}
		switch ed.From {
		case "Flow.cs::Flow.Pull":
			ambiguous = append(ambiguous, ed)
		case "Flow.cs::Flow.Single":
			single = append(single, ed)
		}
	}
	require.NotEmpty(t, ambiguous)
	for _, ed := range ambiguous {
		require.NotNil(t, ed.Meta)
		assert.Equal(t, true, ed.Meta["receiver_ambiguous"],
			"two distinct receivers behind one (name,line) site must be marked")
	}
	require.NotEmpty(t, single)
	for _, ed := range single {
		if ed.Meta != nil {
			assert.NotContains(t, ed.Meta, "receiver_ambiguous",
				"a lone-receiver site stays unmarked")
		}
	}
}

// Two members with EQUAL spans declared on one source line: line-keyed
// attribution breaks the tie by extraction order, which is arbitrary,
// so any evidence keyed on the attribution must refuse there. The
// stamp is the refusal; this pin is the assertion the round-5 test of
// the same name carried before its rewrite lost it (round-6
// non-blocking finding 7).
//
// The tied members call DIFFERENT names on DISTINCT receivers: a
// same-name site is already marked by the (name,line) receiver-conflict
// map, and the original same-name fixture pinned only that map - the
// range-tie question itself could be reverted with the suite green
// (issue #726). Here only ambiguousAt can produce the stamp.
func TestCSharpExtractor_TwoMembersOnOneLineMarkReceiverAmbiguous(t *testing.T) {
	src := []byte(`namespace App {
    public class Store { public int Fetch(int id) { return id; } public int Grab(int id) { return id; } }
    public class Flow {
        private readonly Store _crates;
        public int B(Store s) { return s.Fetch(1); } public int A(Store t) { return t.Grab(2); }
        public int Lone(Store s) { return s.Fetch(3); }
    }
}
`)
	result, err := NewCSharpExtractor().Extract("Tie.cs", src)
	require.NoError(t, err)

	tied, lone := 0, 0
	for _, ed := range result.Edges {
		if ed == nil || ed.Kind != graph.EdgeCalls {
			continue
		}
		switch {
		case ed.From == "Tie.cs::Flow.B" && ed.To == "unresolved::*.Fetch",
			ed.From == "Tie.cs::Flow.A" && ed.To == "unresolved::*.Grab":
			tied++
			require.NotNil(t, ed.Meta)
			assert.Equal(t, true, ed.Meta["receiver_ambiguous"],
				"equal-span members tie on the line - the attribution is arbitrary and the evidence must say so")
		case ed.From == "Tie.cs::Flow.Lone" && ed.To == "unresolved::*.Fetch":
			lone++
			if ed.Meta != nil {
				assert.NotContains(t, ed.Meta, "receiver_ambiguous",
					"a line owned by one member stays unmarked")
			}
		}
	}
	require.Equal(t, 2, tied, "fixture: both tied-line member calls must emit")
	require.NotZero(t, lone, "fixture: the control line must emit a member call")
}

// The 2,000-sibling shape from the review: one namespace whose declaration
// list holds thousands of types, each with a generic base entry and a
// generic field. A per-declaration alias scan that re-walks the namespace's
// children makes stamping quadratic in sibling count; the per-file alias
// set must be collected once.
func BenchmarkCSharpExtractSiblingHeavyTypeArgStamps(b *testing.B) {
	var sb []byte
	sb = append(sb, []byte("using MyCrate = App.Crate;\nnamespace App {\n    public interface IBoxStore<T> { }\n    public class Crate { }\n")...)
	for i := 0; i < 2000; i++ {
		n := []byte("    public class Store" + itoa(i) + " : IBoxStore<Crate> {\n        private readonly IBoxStore<Crate> _store;\n    }\n")
		sb = append(sb, n...)
	}
	sb = append(sb, []byte("}\n")...)
	e := NewCSharpExtractor()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := e.Extract("Siblings.cs", sb); err != nil {
			b.Fatal(err)
		}
	}
}

// Aliases x plain fields (issue 727): no stamp is ever possible - every
// declared type is `int` - yet the per-stamp-site helper copied the file's
// whole alias set into a fresh map once per field. 2,000 x 2,000 cost
// 460 MB/op before csharpOpenNames kept the alias half by reference.
func BenchmarkCSharpExtractAliasHeavyPlainFields(b *testing.B) {
	for _, n := range []int{1000, 2000} {
		var sb strings.Builder
		for i := 0; i < n; i++ {
			sb.WriteString("using Alias" + itoa(i) + " = App.Target" + itoa(i) + ";\n")
		}
		sb.WriteString("namespace App {\n    public class Holder {\n")
		for i := 0; i < n; i++ {
			sb.WriteString("        private int _f" + itoa(i) + ";\n")
		}
		sb.WriteString("    }\n}\n")
		src := []byte(sb.String())
		e := NewCSharpExtractor()
		b.Run(itoa(n)+"x"+itoa(n), func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				if _, err := e.Extract("Aliases.cs", src); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
