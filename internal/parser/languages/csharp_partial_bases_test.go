package languages

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

// A same-file partial type spells its interface paths across TWO
// declarations that share one node ID. The duplicate-declaration branch
// returned before base-list emission, so the second fragment's paths
// never reached the graph - the surviving fragment's stamp then read as
// the type's unique closure and filtered the whole family downstream
// (round-5 finding 5).
//
// Both declarations spelling `partial` is the gate: arity twins and
// other short-ID collisions must never blindly merge bases.
func TestCSharpBaseList_SameFilePartialKeepsBothFragments(t *testing.T) {
	src := []byte(`namespace App {
    public class Crate { }
    public class Widget { }
    public interface IBox<T> { void Put(T item); }
    public interface ICrateBox : IBox<Crate> { }
    public partial class Dual : IBox<Widget> { public void Put(Widget item) { } }
    public partial class Dual : ICrateBox { public void Put(Crate item) { } }
}
`)
	res, err := NewCSharpExtractor().Extract("P.cs", src)
	require.NoError(t, err)

	targets := map[string]bool{}
	for _, e := range csharpBaseEdges(res, "P.cs::Dual") {
		targets[e.To] = true
	}
	assert.True(t, targets["unresolved::IBox"],
		"the first fragment's path must be there, got %v", targets)
	assert.True(t, targets["unresolved::ICrateBox"],
		"the SECOND fragment's path must survive node deduplication, got %v", targets)
}

// `partial` alone proves a keyword, not a type identity: two DISTINCT
// types can share filePath::name and both be partial (round-6 finding
// B4). An arity twin pair - the Result / Result<T> idiom - must not
// merge even when both fragments spell partial; the twin's interface
// would otherwise fan a call through IBox<Crate> out to a type that
// implements nothing.
func TestCSharpBaseList_PartialArityTwinsDoNotMergeBases(t *testing.T) {
	src := []byte(`namespace App {
    public class Crate { }
    public interface IBox<T> { void Put(T item); }
    public partial class Result { public void Put(Crate item) { } }
    public partial class Result<T> : IBox<Crate> { public void Put(Crate item) { } }
}
`)
	res, err := NewCSharpExtractor().Extract("y.cs", src)
	require.NoError(t, err)

	assert.Empty(t, csharpBaseEdges(res, "y.cs::Result"),
		"partial is not an identity proof - the generic twin's bases must not graft onto the bare Result")
}

// Namespace twins: A.Node and B.Node in one file, both partial, are two
// types (round-6 finding B4).
func TestCSharpBaseList_PartialNamespaceTwinsDoNotMergeBases(t *testing.T) {
	src := []byte(`namespace A {
    public interface ITagged { }
    public partial class Node { }
}
namespace B {
    public partial class Node : A.ITagged { }
}
`)
	res, err := NewCSharpExtractor().Extract("ns.cs", src)
	require.NoError(t, err)

	assert.Empty(t, csharpBaseEdges(res, "ns.cs::Node"),
		"two namespaces, two types - no merge")
}

// Nested-vs-top-level twins: Holder.Node and Node, both partial, are
// two types (round-6 finding B4).
func TestCSharpBaseList_PartialNestedTwinDoesNotMergeBases(t *testing.T) {
	src := []byte(`namespace App {
    public interface ITagged { }
    public partial class Node { }
    public class Holder {
        public partial class Node : ITagged { }
    }
}
`)
	res, err := NewCSharpExtractor().Extract("nest.cs", src)
	require.NoError(t, err)

	assert.Empty(t, csharpBaseEdges(res, "nest.cs::Node"),
		"a nested twin is a different type - no merge")
}

// A type has at most ONE base class. The merge restarted extendsTaken
// per fragment, so two partial fragments could mint two extends edges
// (round-6 finding B4, second part); the carried state keeps the first
// and the later fragment's non-interface entry rides implements, the
// same approximation a second non-interface entry gets WITHIN one base
// list.
func TestCSharpBaseList_PartialFragmentsMintOneBaseClass(t *testing.T) {
	src := []byte(`namespace App {
    public class SvcBase { }
    public class Localizable { }
    public partial class Svc : SvcBase { }
    public partial class Svc : Localizable { }
}
`)
	res, err := NewCSharpExtractor().Extract("svc.cs", src)
	require.NoError(t, err)

	extends := 0
	for _, e := range csharpBaseEdges(res, "svc.cs::Svc") {
		if e.Kind == graph.EdgeExtends {
			extends++
		}
	}
	assert.Equal(t, 1, extends, "C# permits exactly one base class per type")
}

// The guard: a non-partial short-ID collision (an arity twin) keeps the
// old behavior - the dropped declaration's bases stay dropped, because
// nothing proves they belong to the surviving node's type.
func TestCSharpBaseList_ArityTwinCollisionDoesNotMergeBases(t *testing.T) {
	src := []byte(`namespace App {
    public interface ITagged { }
    public class Result { }
    public class Result<T> : ITagged { }
}
`)
	res, err := NewCSharpExtractor().Extract("T.cs", src)
	require.NoError(t, err)

	assert.Empty(t, csharpBaseEdges(res, "T.cs::Result"),
		"the twin's base list must not be grafted onto the surviving bare Result")
}
