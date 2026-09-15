package resolver

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/zzet/gortex/internal/graph"
)

// namedCallEdge returns the EdgeCalls edge leaving fromID whose target
// names `name` — resolved (node Name) or stubbed (`unresolved::name`,
// `unresolved::*.name`).
func namedCallEdge(t *testing.T, g graph.Store, fromID, name string) *graph.Edge {
	t.Helper()
	for _, e := range g.GetOutEdges(fromID) {
		if e.Kind != graph.EdgeCalls {
			continue
		}
		if graph.IsUnresolvedTarget(e.To) {
			if n := graph.UnresolvedName(e.To); n == name || n == "*."+name {
				return e
			}
			continue
		}
		if n := g.GetNode(e.To); n != nil && n.Name == name {
			return e
		}
	}
	t.Fatalf("no %s call edge from %s", name, fromID)
	return nil
}

const csUsingStaticHelpers = `namespace Util {
    public static class Helpers {
        public static int Clamp(int x) { return x; }
    }
}`

// A same-name instance method in the CALLER's directory: the locality
// cascade's same-package tier binds it today.
const csUsingStaticDecoy = `namespace Other {
    public class Decoy {
        public int Clamp(int x) { return x; }
    }
}`

// TestResolveCSharp_UsingStaticBindsBareCallToTargetMember: `using static
// Util.Helpers;` brings Helpers' static members into scope by simple name,
// so a bare `Clamp(1)` is Helpers.Clamp — an explicit directive naming the
// owner outranks directory locality, which means nothing in C#.
func TestResolveCSharp_UsingStaticBindsBareCallToTargetMember(t *testing.T) {
	g := buildCSharpResolverGraph(t, map[string]string{
		"Util/Helpers.cs": csUsingStaticHelpers,
		"App/Decoy.cs":    csUsingStaticDecoy,
		"App/Caller.cs": `using static Util.Helpers;
namespace App {
    public class Runner {
        public int Run() { return Clamp(1); }
    }
}`,
	})
	New(g).ResolveAll()

	e := namedCallEdge(t, g, "App/Caller.cs::Runner.Run", "Clamp")
	assert.Equal(t, "Util/Helpers.cs::Helpers.Clamp", e.To,
		"the using static names the owner class — the bare call is its member, not the same-directory decoy")
	assert.Equal(t, graph.OriginASTResolved, e.Origin)
	assert.Equal(t, "using_static", e.Meta["resolution"])
}

// TestResolveCSharp_GlobalUsingStaticBindsBareCallUnitWide: the global
// form is compilation-scoped — a `global using static` in Usings.cs puts
// the members in scope for a caller file that declares no using at all.
func TestResolveCSharp_GlobalUsingStaticBindsBareCallUnitWide(t *testing.T) {
	g := buildCSharpResolverGraph(t, map[string]string{
		"Util/Helpers.cs": csUsingStaticHelpers,
		"App/Decoy.cs":    csUsingStaticDecoy,
		"Usings.cs":       "global using static Util.Helpers;\n",
		"App/Caller.cs": `namespace App {
    public class Runner {
        public int Run() { return Clamp(1); }
    }
}`,
	})
	New(g).ResolveAll()

	e := namedCallEdge(t, g, "App/Caller.cs::Runner.Run", "Clamp")
	assert.Equal(t, "Util/Helpers.cs::Helpers.Clamp", e.To,
		"a global using static declared in a sibling file is visible to the whole unit")
	assert.Equal(t, graph.OriginASTResolved, e.Origin)
}

// TestResolveCSharp_GlobalUsingStaticBindsOnScopedResolve: the per-save
// path reads the same unit-wide visibility as the full pass.
func TestResolveCSharp_GlobalUsingStaticBindsOnScopedResolve(t *testing.T) {
	g := buildCSharpResolverGraph(t, map[string]string{
		"Util/Helpers.cs": csUsingStaticHelpers,
		"App/Decoy.cs":    csUsingStaticDecoy,
		"Usings.cs":       "global using static Util.Helpers;\n",
		"App/Caller.cs": `namespace App {
    public class Runner {
        public int Run() { return Clamp(1); }
    }
}`,
	})
	New(g).ResolveFileAndIncoming("App/Caller.cs")

	e := namedCallEdge(t, g, "App/Caller.cs::Runner.Run", "Clamp")
	assert.Equal(t, "Util/Helpers.cs::Helpers.Clamp", e.To,
		"the scoped resolve must see the unit's global using static too")
}

// TestResolveCSharp_EnclosingTypeMemberOutranksUsingStatic: simple-name
// lookup finds members of the enclosing type and its bases BEFORE the
// using-static imports of the enclosing namespaces (C# spec §12.8.4), so
// an inherited Clamp claims the call and the directive's member does not.
// The base lives in ANOTHER file and directory so the same-file pick
// cannot answer for the gate: only the in-graph ancestor walk can.
func TestResolveCSharp_EnclosingTypeMemberOutranksUsingStatic(t *testing.T) {
	g := buildCSharpResolverGraph(t, map[string]string{
		"Util/Helpers.cs": csUsingStaticHelpers,
		"Core/Base.cs": `namespace App {
    public class Base {
        public int Clamp(int x) { return x; }
    }
}`,
		"App/Caller.cs": `using static Util.Helpers;
namespace App {
    public class Runner : Base {
        public int Run() { return Clamp(1); }
    }
}`,
	})
	New(g).ResolveAll()

	e := namedCallEdge(t, g, "App/Caller.cs::Runner.Run", "Clamp")
	assert.NotEqual(t, "using_static", e.Meta["resolution"],
		"an inherited member claims the simple name before any using static is consulted, got %q", e.To)
	assert.NotContains(t, e.To, "Helpers.Clamp", "the directive's member must not steal an inherited call")
}

// TestResolveCSharp_SameFileBaseMemberStaysWithTheSameFilePick: the
// same-file tier already answers a same-file base member; the using-static
// tier sits behind it and must leave that bind alone.
func TestResolveCSharp_SameFileBaseMemberStaysWithTheSameFilePick(t *testing.T) {
	g := buildCSharpResolverGraph(t, map[string]string{
		"Util/Helpers.cs": csUsingStaticHelpers,
		"App/Caller.cs": `using static Util.Helpers;
namespace App {
    public class Base {
        public int Clamp(int x) { return x; }
    }
    public class Runner : Base {
        public int Run() { return Clamp(1); }
    }
}`,
	})
	New(g).ResolveAll()

	e := namedCallEdge(t, g, "App/Caller.cs::Runner.Run", "Clamp")
	assert.Equal(t, "App/Caller.cs::Base.Clamp", e.To)
	assert.NotEqual(t, "using_static", e.Meta["resolution"])
}

// TestResolveCSharp_UsingStaticNeverBindsInstanceMembers: `using static`
// is legal on any type with static members, but only the STATIC members
// come into scope by simple name — an instance member is reachable only
// through an instance. A bare `Save(1)` under `using static App.Order;`
// is not Order.Save.
func TestResolveCSharp_UsingStaticNeverBindsInstanceMembers(t *testing.T) {
	g := buildCSharpResolverGraph(t, map[string]string{
		"Model/Order.cs": `namespace App {
    public class Order {
        public static Order Create(int id) { return new Order(); }
        public int Save(int x) { return x; }
    }
}`,
		"Svc/Caller.cs": `using static App.Order;
namespace Svc {
    public class Runner {
        public int Run() { return Save(1); }
        public object Make() { return Create(1); }
    }
}`,
	})
	New(g).ResolveAll()

	save := namedCallEdge(t, g, "Svc/Caller.cs::Runner.Run", "Save")
	assert.NotEqual(t, "using_static", save.Meta["resolution"],
		"an instance member is not imported by using static, got %q", save.To)
	if save.To == "Model/Order.cs::Order.Save" {
		assert.NotEqual(t, graph.OriginASTResolved, save.Origin, "a locality guess may land there, a proven bind may not")
	}
	create := namedCallEdge(t, g, "Svc/Caller.cs::Runner.Make", "Create")
	assert.Equal(t, "Model/Order.cs::Order.Create", create.To, "the static member of the same type does bind")
	assert.Equal(t, "using_static", create.Meta["resolution"])
}

// TestResolveCSharp_UsingStaticNeverBindsExtensionMethodsAsStatic: the
// spec (ECMA §14.6.4) is explicit — a using-static directive does not
// import extension methods as static methods, it only makes them
// available for extension invocation. A bare `Format(...)` under
// `using static App.Extensions;` does not compile, so it is never
// Extensions.Format here.
func TestResolveCSharp_UsingStaticNeverBindsExtensionMethodsAsStatic(t *testing.T) {
	g := buildCSharpResolverGraph(t, map[string]string{
		"Ext/Extensions.cs": `namespace App {
    public static class Extensions {
        public static string Format(this string s) { return s; }
    }
}`,
		"Svc/Caller.cs": `using static App.Extensions;
namespace Svc {
    public class Runner {
        public string Run() { return Format("x"); }
    }
}`,
	})
	New(g).ResolveAll()

	e := namedCallEdge(t, g, "Svc/Caller.cs::Runner.Run", "Format")
	assert.NotEqual(t, "using_static", e.Meta["resolution"],
		"an extension method is not a static import, got %q", e.To)
	if e.To == "Ext/Extensions.cs::Extensions.Format" {
		assert.NotEqual(t, graph.OriginASTResolved, e.Origin)
	}
}

// TestResolveCSharp_UsingStaticSuffixMatchIsAnchored: a directive written
// inside a namespace block may spell its target relative to that
// namespace, so `namespace App { using static Util.H; }` names App.Util.H
// — but only because App encloses the directive. A top-level
// `using static Helpers;` must not reach a class merely because its FQN
// ends in ".Helpers".
func TestResolveCSharp_UsingStaticSuffixMatchIsAnchored(t *testing.T) {
	g := buildCSharpResolverGraph(t, map[string]string{
		"Deep/Helpers.cs": `namespace Vendor.Deep {
    public static class Helpers {
        public static int Clamp(int x) { return x; }
    }
}`,
		"App/Caller.cs": `using static Helpers;
namespace App {
    public class Runner {
        public int Run() { return Clamp(1); }
    }
}`,
	})
	New(g).ResolveAll()

	e := namedCallEdge(t, g, "App/Caller.cs::Runner.Run", "Clamp")
	assert.NotEqual(t, "using_static", e.Meta["resolution"],
		"Vendor.Deep.Helpers is not what a top-level `using static Helpers;` denotes, got %q", e.To)
}

// TestResolveCSharp_UsingStaticInsideNamespaceBlockResolvesRelative: the
// positive half of the anchoring — the enclosing namespace App makes
// `Util.Helpers` denote App.Util.Helpers.
func TestResolveCSharp_UsingStaticInsideNamespaceBlockResolvesRelative(t *testing.T) {
	g := buildCSharpResolverGraph(t, map[string]string{
		"Util/Helpers.cs": `namespace App.Util {
    public static class Helpers {
        public static int Clamp(int x) { return x; }
    }
}`,
		"App/Decoy.cs": csUsingStaticDecoy,
		"App/Caller.cs": `namespace App {
    using static Util.Helpers;
    public class Runner {
        public int Run() { return Clamp(1); }
    }
}`,
	})
	New(g).ResolveAll()

	e := namedCallEdge(t, g, "App/Caller.cs::Runner.Run", "Clamp")
	assert.Equal(t, "Util/Helpers.cs::Helpers.Clamp", e.To,
		"the directive is anchored by its enclosing namespace App")
	assert.Equal(t, "using_static", e.Meta["resolution"])
}

// TestResolveCSharp_OuterTypeStaticMemberOutranksUsingStatic: a nested
// type sees every lexically enclosing type's members before any
// using-static import — and the graph records no outer-type chain, so
// the tier must sit BEHIND the same-file pick, which already binds this
// case correctly, rather than ahead of it.
func TestResolveCSharp_OuterTypeStaticMemberOutranksUsingStatic(t *testing.T) {
	g := buildCSharpResolverGraph(t, map[string]string{
		"Util/Helpers.cs": csUsingStaticHelpers,
		"App/Caller.cs": `using static Util.Helpers;
namespace App {
    public class Outer {
        public static int Clamp(int x) { return x; }
        public class Inner {
            public int Run() { return Clamp(1); }
        }
    }
}`,
	})
	New(g).ResolveAll()

	e := namedCallEdge(t, g, "App/Caller.cs::Inner.Run", "Clamp")
	assert.Contains(t, e.To, "App/Caller.cs::", "the outer type's static member wins, got %q", e.To)
	assert.NotEqual(t, "using_static", e.Meta["resolution"])
}

// TestResolveCSharp_SameOwnerOverloadsSurviveTheGuard: two overloads of
// the target both accept the call's arity, so the member is a guess but
// the OWNER is proven by the directive. The pick must land in the owner
// and must survive the cross-package guard, which otherwise reverts a
// weak-tier bind to an un-imported directory.
func TestResolveCSharp_SameOwnerOverloadsSurviveTheGuard(t *testing.T) {
	g := buildCSharpResolverGraph(t, map[string]string{
		"Util/Helpers.cs": `namespace Util {
    public static class Helpers {
        public static int Clamp(int x) { return x; }
        public static long Clamp(long x) { return x; }
    }
}`,
		"App/Decoy.cs": csUsingStaticDecoy,
		"App/Caller.cs": `using static Util.Helpers;
namespace App {
    public class Runner {
        public int Run() { return Clamp(1); }
    }
}`,
	})
	New(g).ResolveAll()

	e := namedCallEdge(t, g, "App/Caller.cs::Runner.Run", "Clamp")
	assert.Contains(t, e.To, "Util/Helpers.cs::Helpers.Clamp",
		"the owner is proven even when the overload is not, got %q", e.To)
	assert.Equal(t, "using_static", e.Meta["resolution"])
	assert.Equal(t, graph.OriginASTInferred, e.Origin, "a guessed overload is not an exact bind")
}

// TestResolveCSharp_RestubbedUsingStaticBindDropsStaleTag: the incremental
// path restubs a using-static bind to its bare shape but keeps Meta. When
// the directive is gone the tier no longer applies, and whatever the
// cascade binds next must not inherit the using_static tag — the same
// hygiene the extension_method tag gets in resolveFunctionCall.
func TestResolveCSharp_RestubbedUsingStaticBindDropsStaleTag(t *testing.T) {
	g := buildCSharpResolverGraph(t, map[string]string{
		"Util/Helpers.cs": csUsingStaticHelpers,
		"App/Decoy.cs":    csUsingStaticDecoy,
		"App/Caller.cs": `using static Util.Helpers;
namespace App {
    public class Runner {
        public int Run() { return Clamp(1); }
    }
}`,
	})
	r := New(g)
	r.ResolveAll()
	e := namedCallEdge(t, g, "App/Caller.cs::Runner.Run", "Clamp")
	if !assert.Equal(t, "Util/Helpers.cs::Helpers.Clamp", e.To, "fixture: the directive binds first") {
		return
	}

	// Simulate the save that removed the directive: the file node loses
	// its stamp, the bound edge is restubbed the way restubCSharpExtensionBinds
	// does it (target + provenance cleared, Meta kept).
	for _, n := range g.GetFileNodes("App/Caller.cs") {
		if n.Kind == graph.KindFile {
			delete(n.Meta, "using_static")
		}
	}
	oldTo := e.To
	graph.StashRestubProvenance(e)
	e.To = graph.UnresolvedMarker + "Clamp"
	g.ReindexEdges([]graph.EdgeReindex{{Edge: e, OldTo: oldTo}})

	r.ResolveFileAndIncoming("App/Caller.cs")
	e = namedCallEdge(t, g, "App/Caller.cs::Runner.Run", "Clamp")
	assert.NotEqual(t, "Util/Helpers.cs::Helpers.Clamp", e.To,
		"without the directive the tier must not rebind to Helpers")
	assert.Nil(t, e.Meta["resolution"], "the stale using_static tag must not ride the new bind, got %q -> %v", e.To, e.Meta)
}

// TestResolveCSharp_LocalFunctionShadowsUsingStatic (PR #797 review P1):
// the enclosing method's local declaration space comes before any
// using-static import (C# spec §12.8.4), so a local function named
// Clamp is the callee. No node exists for it; the honest answer is the
// unresolved stub main already leaves, never the directive's member.
func TestResolveCSharp_LocalFunctionShadowsUsingStatic(t *testing.T) {
	g := buildCSharpResolverGraph(t, map[string]string{
		"Util/Helpers.cs": csUsingStaticHelpers,
		"App/Caller.cs": `using static Util.Helpers;
namespace App {
    public class Runner {
        public int Run() {
            int Clamp(int x) { return x + 1; }
            return Clamp(1);
        }
    }
}`,
	})
	New(g).ResolveAll()

	e := namedCallEdge(t, g, "App/Caller.cs::Runner.Run", "Clamp")
	assert.True(t, graph.IsUnresolvedTarget(e.To),
		"a local function shadows the directive; got %q (%s, %.2f)", e.To, e.Origin, e.Confidence)
	assert.Nil(t, e.Meta["resolution"])
}

// TestResolveCSharp_LocalFunctionShadowsSameFilePick: the same rule
// binds every tier, not just the using-static one — the same-file pick
// used to hand the shadowed call to a same-file method at 0.9.
func TestResolveCSharp_LocalFunctionShadowsSameFilePick(t *testing.T) {
	g := buildCSharpResolverGraph(t, map[string]string{
		"App/Caller.cs": `namespace App {
    public class Runner {
        public int Clamp(int x) { return x; }
        public int Run() {
            int Clamp(int x) { return x + 1; }
            return Clamp(1);
        }
    }
}`,
	})
	New(g).ResolveAll()

	e := namedCallEdge(t, g, "App/Caller.cs::Runner.Run", "Clamp")
	assert.True(t, graph.IsUnresolvedTarget(e.To),
		"a local function shadows the same-file method too; got %q (%s, %.2f)", e.To, e.Origin, e.Confidence)
}

// TestResolveCSharp_DelegateParameterShadowsUsingStatic: a delegate-typed
// parameter invoked by its simple name is the same class — the parameter
// is the callee, the directive's member is not.
func TestResolveCSharp_DelegateParameterShadowsUsingStatic(t *testing.T) {
	g := buildCSharpResolverGraph(t, map[string]string{
		"Util/Helpers.cs": csUsingStaticHelpers,
		"App/Caller.cs": `using static Util.Helpers;
namespace App {
    public class Runner {
        public int Run(System.Func<int, int> Clamp) { return Clamp(1); }
    }
}`,
	})
	New(g).ResolveAll()

	e := namedCallEdge(t, g, "App/Caller.cs::Runner.Run", "Clamp")
	assert.True(t, graph.IsUnresolvedTarget(e.To),
		"a delegate parameter shadows the directive; got %q (%s, %.2f)", e.To, e.Origin, e.Confidence)
}

// TestResolveCSharp_LocalShadowSurvivesTheCrossRepoPass: the per-repo
// refusal is not the last word in a multi-repo daemon — CrossRepoResolver
// runs after it and its first tier binds a leftover unresolved call to
// the first same-repo function with no evidence at all. Observed on the
// lab daemon: the stamped stub came back as the same-directory decoy at
// confidence 0. The verdict must hold there too.
func TestResolveCSharp_LocalShadowSurvivesTheCrossRepoPass(t *testing.T) {
	g := buildCSharpResolverGraph(t, map[string]string{
		"Util/Helpers.cs": csUsingStaticHelpers,
		"App/Decoy.cs":    csUsingStaticDecoy,
		"App/Caller.cs": `using static Util.Helpers;
namespace App {
    public class Runner {
        public int Run() {
            int Clamp(int x) { return x + 1; }
            return Clamp(1);
        }
    }
}`,
	})
	New(g).ResolveAll()
	NewCrossRepo(g).ResolveAll()

	e := namedCallEdge(t, g, "App/Caller.cs::Runner.Run", "Clamp")
	assert.True(t, graph.IsUnresolvedTarget(e.To),
		"the cross-repo pass must not overturn the local-shadow verdict; got %q (%s, %.2f)", e.To, e.Origin, e.Confidence)
}

// TestResolveCSharp_TwoUsingStaticOwnersRefuse: two visible targets both
// declaring the name is a compile-time ambiguity — the rule binds neither,
// and with no locality evidence either the call stays unresolved.
func TestResolveCSharp_TwoUsingStaticOwnersRefuse(t *testing.T) {
	g := buildCSharpResolverGraph(t, map[string]string{
		"Util/Helpers.cs": csUsingStaticHelpers,
		"Util/More.cs": `namespace Util {
    public static class More {
        public static int Clamp(int x) { return x; }
    }
}`,
		"App/Caller.cs": `using static Util.Helpers;
using static Util.More;
namespace App {
    public class Runner {
        public int Run() { return Clamp(1); }
    }
}`,
	})
	New(g).ResolveAll()

	e := namedCallEdge(t, g, "App/Caller.cs::Runner.Run", "Clamp")
	assert.NotEqual(t, "using_static", e.Meta["resolution"],
		"neither owner is proven — the rule must not pick one, got %q", e.To)
}

// TestResolveCSharp_ExternalUsingStaticTargetLeavesBareCallUnresolved: the
// target class is not in the graph (`System.Math`), so the bare `Sqrt`
// stays the bare stub it was extracted as. Pinned on purpose — qualifying
// it onto the external owner is NOT sound with the resolver's evidence: the
// same bare shape is what a local function, a delegate invocation,
// `nameof(...)`, or a member inherited from an external base leaves
// behind. See the file comment in csharp_using_static.go.
func TestResolveCSharp_ExternalUsingStaticTargetLeavesBareCallUnresolved(t *testing.T) {
	g := buildCSharpResolverGraph(t, map[string]string{
		"Usings.cs": "global using static System.Math;\n",
		"Geometry.cs": `namespace App {
    public static class Geometry {
        public static double Diagonal(double w, double h) { return Sqrt(w * w + h * h); }
    }
}`,
	})
	New(g).ResolveAll()

	e := namedCallEdge(t, g, "Geometry.cs::Geometry.Diagonal", "Sqrt")
	assert.Equal(t, "unresolved::Sqrt", e.To)
	assert.Nil(t, e.Meta["receiver_name"])
	assert.Nil(t, e.Meta["resolution"])
}
