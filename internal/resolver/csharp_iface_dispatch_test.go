package resolver

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser/languages"
)

// isIfaceDispatchEdge reports whether e is a fan-out edge minted by the C#
// interface-dispatch synthesizer.
func isIfaceDispatchEdge(e *graph.Edge) bool {
	return e != nil && e.Kind == graph.EdgeCalls && e.Meta != nil &&
		e.Meta[MetaSynthesizedBy] == SynthCSharpIfaceDispatch
}

// buildCSharpResolverGraph extracts each C# fixture with the real extractor and
// loads its nodes/edges into a fresh graph — the same unresolved shape a live
// index produces, ready for New(g).ResolveAll().
func buildCSharpResolverGraph(t *testing.T, files map[string]string) graph.Store {
	t.Helper()
	g := graph.New()
	e := languages.NewCSharpExtractor()
	for path, src := range files {
		r, err := e.Extract(path, []byte(src))
		require.NoError(t, err, "csharp extract %s", path)
		for _, n := range r.Nodes {
			g.AddNode(n)
		}
		for _, ed := range r.Edges {
			g.AddEdge(ed)
		}
	}
	return g
}

// TestResolveCSharpInterfaceDispatch_EndToEnd drives the full path: the
// extractor emits the interface member + the through-interface call, ResolveAll
// binds the call to the interface member, and the synthesizer fans it out to
// both concrete implementations at the speculative tier.
func TestResolveCSharpInterfaceDispatch_EndToEnd(t *testing.T) {
	g := buildCSharpResolverGraph(t, map[string]string{
		"IConverter.cs": `namespace App {
    public interface IConverter {
        string Convert(int n);
    }
}`,
		"English.cs": `namespace App {
    public class EnglishConverter : IConverter {
        public string Convert(int n) { return "en"; }
    }
}`,
		"Ukrainian.cs": `namespace App {
    public class UkrainianConverter : IConverter {
        public string Convert(int n) { return "uk"; }
    }
}`,
		"Runner.cs": `namespace App {
    public class Runner {
        public void Run(IConverter c) {
            IConverter conv = c;
            conv.Convert(1);
        }
    }
}`,
	})
	New(g).ResolveAll()

	callerID := "Runner.cs::Runner.Run"
	ifaceMember := "IConverter.cs::IConverter.Convert"

	// (a) the through-interface call binds to the interface member.
	require.Contains(t, callTargetsFrom(g, callerID), ifaceMember,
		"through-interface call should bind to the interface member node")

	// (b) fan-out to both implementations at the ast_inferred tier.
	n := ResolveCSharpInterfaceDispatch(g)
	require.Equal(t, 2, n, "one fan-out edge per implementation")

	fanout := map[string]*graph.Edge{}
	for _, e := range g.GetOutEdges(callerID) {
		if isIfaceDispatchEdge(e) {
			fanout[e.To] = e
		}
	}
	for _, id := range []string{"English.cs::EnglishConverter.Convert", "Ukrainian.cs::UkrainianConverter.Convert"} {
		e := fanout[id]
		require.NotNil(t, e, "expected fan-out edge to %s", id)
		assert.Equal(t, graph.OriginASTInferred, e.Origin)
		assert.False(t, e.IsSpeculative(),
			"fan-out must NOT be speculative or the default find_usages filter drops it")
		assert.Equal(t, SynthCSharpIfaceDispatch, e.Meta[MetaSynthesizedBy])
	}

	// (c) find_usages-equivalent: the through-interface call site surfaces as an
	// in-edge on each concrete implementation AND survives the default
	// speculative filter, so find_usages(<Impl>.Convert) returns it.
	for _, id := range []string{"English.cs::EnglishConverter.Convert", "Ukrainian.cs::UkrainianConverter.Convert"} {
		found := false
		for _, e := range g.GetInEdges(id) {
			if e.From == callerID && e.Kind == graph.EdgeCalls && !e.IsSpeculative() {
				found = true
				break
			}
		}
		assert.True(t, found, "through-interface usage of %s must be a default-visible in-edge", id)
	}
}

// TestResolveCSharpInterfaceDispatch_FamilyCascade drives the sibling
// self-call mechanism end-to-end: subclasses reach the interface through an
// abstract base class (extends -> implements), and a subclass's own recursive
// Convert call — which binds to its OWN method node, never the interface —
// must still surface as a usage of the interface member and of every sibling
// implementation, mirroring the reference resolver's family union.
func TestResolveCSharpInterfaceDispatch_FamilyCascade(t *testing.T) {
	g := buildCSharpResolverGraph(t, map[string]string{
		"IConverter.cs": `namespace App {
    public interface IConverter {
        string Convert(long n);
    }
}`,
		"BaseConverter.cs": `namespace App {
    public abstract class BaseConverter : IConverter {
        public abstract string Convert(long n);
    }
}`,
		"Afrikaans.cs": `namespace App {
    public class AfrikaansConverter : BaseConverter {
        public override string Convert(long n) {
            if (n < 0) {
                return "minus " + Convert(-n);
            }
            return "af";
        }
    }
}`,
		"Serbian.cs": `namespace App {
    public class SerbianConverter : BaseConverter {
        public override string Convert(long n) { return "sr"; }
    }
}`,
		"Danish.cs": `namespace App {
    public class DanishConverter : BaseConverter {
        public override string Convert(long n) { return Convert(n, false); }
        public string Convert(long n, bool suffix) {
            if (n < 0) {
                return "minus " + Convert(-n, suffix);
            }
            return "da";
        }
    }
}`,
		"Unrelated.cs": `namespace App {
    public class Codec {
        public string Convert(long n) { return "x"; }
    }
}`,
	})
	New(g).ResolveAll()

	selfCaller := "Afrikaans.cs::AfrikaansConverter.Convert"

	// Precondition: the recursive call binds to the caller's own method node
	// (same-class resolution) — NOT the interface — which is exactly why an
	// interface-anchored fan-out misses it.
	require.Contains(t, callTargetsFrom(g, selfCaller), selfCaller,
		"the recursive Convert call should bind to the class's own method")

	n := ResolveCSharpInterfaceDispatch(g)
	require.Greater(t, n, 0, "the family cascade must land fan-out edges")

	// The self-call site must surface on the sibling implementation and on the
	// interface member — the find_usages-equivalent in-edge walk, default tier.
	for _, id := range []string{"Serbian.cs::SerbianConverter.Convert", "IConverter.cs::IConverter.Convert"} {
		found := false
		for _, e := range g.GetInEdges(id) {
			if e.From == selfCaller && e.Kind == graph.EdgeCalls && !e.IsSpeculative() {
				found = true
				break
			}
		}
		assert.True(t, found, "the sibling self-call must be a default-visible usage of %s", id)
	}

	// Overload split: C# overloads mint one node per declaration (Convert,
	// Convert_L<line>, ...) and the recursive call inside Danish's second
	// overload binds to one of Danish's own nodes — the cascade must still
	// carry it to the sibling implementation, whichever overload node it
	// landed on.
	danishAttributed := false
	for _, e := range g.GetInEdges("Serbian.cs::SerbianConverter.Convert") {
		if strings.HasPrefix(e.From, "Danish.cs::DanishConverter.Convert") &&
			e.Kind == graph.EdgeCalls && !e.IsSpeculative() {
			danishAttributed = true
			break
		}
	}
	assert.True(t, danishAttributed,
		"a self-call bound to an overload node must still cascade to sibling implementations")

	// Precision: the unrelated same-named method is outside the
	// implements-family and must receive nothing.
	for _, e := range g.GetInEdges("Unrelated.cs::Codec.Convert") {
		assert.NotEqual(t, SynthCSharpIfaceDispatch, edgeMetaValue(e, MetaSynthesizedBy),
			"a same-named method with no implements/extends link must not join the family")
	}
}

// edgeMetaValue reads one Meta key off an edge, tolerating nil Meta.
func edgeMetaValue(e *graph.Edge, key string) any {
	if e == nil || e.Meta == nil {
		return nil
	}
	return e.Meta[key]
}

// TestResolveCSharpInterfaceDispatch_UnresolvedHierarchy models the pipeline
// state the synthesizer actually runs in: call edges already bound, base-list
// targets still `unresolved::Name` (hierarchy settles in later passes). The
// pass must bind those names itself — exact, same-repo, unique — and cascade.
func TestResolveCSharpInterfaceDispatch_UnresolvedHierarchy(t *testing.T) {
	g := buildCSharpResolverGraph(t, map[string]string{
		"IConverter.cs": `namespace App {
    public interface IConverter {
        string Convert(long n);
    }
}`,
		"BaseConverter.cs": `namespace App {
    public abstract class BaseConverter : IConverter {
        public abstract string Convert(long n);
    }
}`,
		"Afrikaans.cs": `namespace App {
    public class AfrikaansConverter : BaseConverter {
        public override string Convert(long n) { return "af"; }
    }
}`,
		"Serbian.cs": `namespace App {
    public class SerbianConverter : BaseConverter {
        public override string Convert(long n) { return "sr"; }
    }
}`,
	})
	// NO ResolveAll: base-list edges keep their unresolved:: targets. Bind one
	// call edge by hand — the state the resolver leaves calls in by synth time.
	selfCaller := "Afrikaans.cs::AfrikaansConverter.Convert"
	g.AddEdge(&graph.Edge{From: selfCaller, To: selfCaller, Kind: graph.EdgeCalls,
		FilePath: "Afrikaans.cs", Line: 3, Origin: graph.OriginASTResolved})

	n := ResolveCSharpInterfaceDispatch(g)
	require.Greater(t, n, 0, "the cascade must work over unresolved base-list targets")

	for _, id := range []string{"Serbian.cs::SerbianConverter.Convert", "IConverter.cs::IConverter.Convert"} {
		found := false
		for _, e := range g.GetInEdges(id) {
			if e.From == selfCaller && isIfaceDispatchEdge(e) {
				found = true
				break
			}
		}
		assert.True(t, found, "self-call must cascade to %s through unresolved hierarchy names", id)
	}
}

// TestResolveCSharpInterfaceDispatch_WeakSourceGate pins the precision rule
// for text_matched sources: a name-only binding that lands on a family member
// from an UNRELATED same-named method (a different interface's Convert) must
// not fan into the family, while an intra-family text_matched self-call (the
// shape overload self-calls bind at) must.
func TestResolveCSharpInterfaceDispatch_WeakSourceGate(t *testing.T) {
	g := graph.New()
	addType := func(file, name string) string {
		id := file + "::" + name
		g.AddNode(&graph.Node{ID: id, Kind: graph.KindType, Name: name, FilePath: file, Language: "csharp"})
		return id
	}
	addMethod := func(file, typ, name string, iface bool) string {
		id := file + "::" + typ + "." + name
		meta := map[string]any{"receiver": typ}
		if iface {
			meta["iface_member"] = true
		}
		g.AddNode(&graph.Node{ID: id, Kind: graph.KindMethod, Name: name, FilePath: file, Language: "csharp", Meta: meta})
		g.AddEdge(&graph.Edge{From: id, To: file + "::" + typ, Kind: graph.EdgeMemberOf, FilePath: file})
		return id
	}

	// Family 1: IConv.Do <- ConvA.Do, ConvB.Do
	iconv := addType("IConv.cs", "IConv")
	iconvDo := addMethod("IConv.cs", "IConv", "Do", true)
	_ = iconvDo
	convA := addType("A.cs", "ConvA")
	aDo := addMethod("A.cs", "ConvA", "Do", false)
	convB := addType("B.cs", "ConvB")
	bDo := addMethod("B.cs", "ConvB", "Do", false)
	g.AddEdge(&graph.Edge{From: convA, To: iconv, Kind: graph.EdgeImplements, FilePath: "A.cs", Origin: graph.OriginASTInferred})
	g.AddEdge(&graph.Edge{From: convB, To: iconv, Kind: graph.EdgeImplements, FilePath: "B.cs", Origin: graph.OriginASTInferred})

	// Unrelated type with a same-named method, outside any family hierarchy.
	ord := addType("Ord.cs", "Ordinalizer")
	ordDo := addMethod("Ord.cs", "Ordinalizer", "Do", false)

	// Cross-family pollution: the Ordinalizer's own Do call was text-matched
	// onto a family member. It must NOT fan into the family.
	g.AddEdge(&graph.Edge{From: ordDo, To: aDo, Kind: graph.EdgeCalls, FilePath: "Ord.cs", Line: 7,
		Origin: graph.OriginTextMatched})
	// Intra-family text_matched self-call (the overload self-call shape): the
	// caller is a family member, so it fans to the sibling and the interface.
	g.AddEdge(&graph.Edge{From: aDo, To: aDo, Kind: graph.EdgeCalls, FilePath: "A.cs", Line: 9,
		Origin: graph.OriginTextMatched})

	// Method-set inference pollution: an inference-marked implements edge
	// (the shape the structural inference pass mints — every Convert-bearing
	// class "implements" a single-method interface) must NOT pull the
	// unrelated type into the family.
	g.AddEdge(&graph.Edge{From: ord, To: iconv, Kind: graph.EdgeImplements, FilePath: "Ord.cs",
		Meta: map[string]any{"via": MetaViaMethodSetInference}})

	n := ResolveCSharpInterfaceDispatch(g)
	require.Equal(t, 2, n, "only the intra-family self-call fans (to sibling + interface member)")

	for _, e := range g.GetInEdges(ordDo) {
		assert.False(t, isIfaceDispatchEdge(e),
			"a method-set-inferred implements edge must not admit a type into the family")
	}

	var fromCallers []string
	for _, e := range g.GetInEdges(bDo) {
		if isIfaceDispatchEdge(e) {
			fromCallers = append(fromCallers, e.From)
		}
	}
	assert.Equal(t, []string{aDo}, fromCallers,
		"the sibling receives the family self-call but never the unrelated text-matched caller")
}

// TestResolveCSharpInterfaceDispatch_FanoutTierAndCap uses a hand-built graph to
// pin the fan-out shape, provenance/tier, dedup, and the fan-out cap.
func TestResolveCSharpInterfaceDispatch_FanoutTierAndCap(t *testing.T) {
	newGraph := func(nImpls int) graph.Store {
		g := graph.New()
		g.AddNode(&graph.Node{ID: "I.cs::IC", Kind: graph.KindInterface, Name: "IC", FilePath: "I.cs", Language: "csharp"})
		g.AddNode(&graph.Node{ID: "I.cs::IC.Do", Kind: graph.KindMethod, Name: "Do", FilePath: "I.cs", Language: "csharp",
			Meta: map[string]any{"receiver": "IC", "iface_member": true}})
		g.AddEdge(&graph.Edge{From: "I.cs::IC.Do", To: "I.cs::IC", Kind: graph.EdgeMemberOf, FilePath: "I.cs"})
		for i := 0; i < nImpls; i++ {
			typ := fmt.Sprintf("Impl%d", i)
			file := typ + ".cs"
			g.AddNode(&graph.Node{ID: file + "::" + typ, Kind: graph.KindType, Name: typ, FilePath: file, Language: "csharp"})
			g.AddNode(&graph.Node{ID: file + "::" + typ + ".Do", Kind: graph.KindMethod, Name: "Do", FilePath: file, Language: "csharp",
				Meta: map[string]any{"receiver": typ}})
			g.AddEdge(&graph.Edge{From: file + "::" + typ + ".Do", To: file + "::" + typ, Kind: graph.EdgeMemberOf, FilePath: file})
			g.AddEdge(&graph.Edge{From: file + "::" + typ, To: "I.cs::IC", Kind: graph.EdgeImplements, FilePath: file, Origin: graph.OriginASTInferred})
		}
		g.AddNode(&graph.Node{ID: "C.cs::App.Run", Kind: graph.KindMethod, Name: "Run", FilePath: "C.cs", Language: "csharp",
			Meta: map[string]any{"receiver": "App"}})
		g.AddEdge(&graph.Edge{From: "C.cs::App.Run", To: "I.cs::IC.Do", Kind: graph.EdgeCalls, FilePath: "C.cs", Line: 3})
		return g
	}

	t.Run("fan-out tier + dedup", func(t *testing.T) {
		g := newGraph(2)
		require.Equal(t, 2, ResolveCSharpInterfaceDispatch(g))

		fanout := map[string]*graph.Edge{}
		for _, e := range g.GetOutEdges("C.cs::App.Run") {
			if isIfaceDispatchEdge(e) {
				fanout[e.To] = e
			}
		}
		require.Len(t, fanout, 2)
		for _, id := range []string{"Impl0.cs::Impl0.Do", "Impl1.cs::Impl1.Do"} {
			e := fanout[id]
			require.NotNil(t, e, id)
			assert.Equal(t, graph.OriginASTInferred, e.Origin)
			assert.False(t, e.IsSpeculative())
			assert.Equal(t, SynthCSharpIfaceDispatch, e.Meta[MetaSynthesizedBy])
			assert.Equal(t, "C.cs", e.FilePath)
			assert.Equal(t, 3, e.Line)
		}

		// Idempotent: a second run must not duplicate fan-out edges.
		ResolveCSharpInterfaceDispatch(g)
		got := 0
		for _, e := range g.GetOutEdges("C.cs::App.Run") {
			if isIfaceDispatchEdge(e) {
				got++
			}
		}
		assert.Equal(t, 2, got, "re-run must not duplicate fan-out edges")
	})

	t.Run("fan-out cap drops noise", func(t *testing.T) {
		g := newGraph(csharpIfaceDispatchCap + 1)
		assert.Equal(t, 0, ResolveCSharpInterfaceDispatch(g),
			"a fan-out wider than the cap is dropped as noise")
	})
}

// TestResolveCSharpInterfaceDispatch_DecoratorForwardingNeverSelfEdges pins
// the decorator/facade shape: a class that implements the SAME interface it
// wraps and forwards the same-named member through an injected inner
// instance. The forwarding call binds to the interface member, and the
// caller is itself a family member — the fan-out must reach the sibling
// implementation but must NEVER fan back onto the caller: a synthesized
// from==to edge reads as "the symbol is its own caller" in find_usages
// consumers. Real recursion is the binder's edge to mint, never this
// synthesizer's.
func TestResolveCSharpInterfaceDispatch_DecoratorForwardingNeverSelfEdges(t *testing.T) {
	g := buildCSharpResolverGraph(t, map[string]string{
		"IConverter.cs": `namespace App {
    public interface IConverter {
        string Convert(int n);
    }
}`,
		"English.cs": `namespace App {
    public class EnglishConverter : IConverter {
        public string Convert(int n) { return "en"; }
    }
}`,
		"Caching.cs": `namespace App {
    public class CachingConverter : IConverter {
        private readonly IConverter _inner;
        public CachingConverter(IConverter inner) { _inner = inner; }
        public string Convert(int n) {
            IConverter inner = _inner;
            return inner.Convert(n);
        }
    }
}`,
	})
	New(g).ResolveAll()

	callerID := "Caching.cs::CachingConverter.Convert"
	require.Contains(t, callTargetsFrom(g, callerID), "IConverter.cs::IConverter.Convert",
		"the forwarding call should bind to the interface member")

	ResolveCSharpInterfaceDispatch(g)

	var fanoutTargets []string
	for _, e := range g.GetOutEdges(callerID) {
		if !isIfaceDispatchEdge(e) {
			continue
		}
		require.NotEqual(t, callerID, e.To,
			"a dispatch fan-out must never claim the caller dispatches to itself (from==to)")
		fanoutTargets = append(fanoutTargets, e.To)
	}
	assert.Contains(t, fanoutTargets, "English.cs::EnglishConverter.Convert",
		"the legitimate fan-out to the sibling implementation must survive the self guard")
}
