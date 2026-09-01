package languages

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The declaration side mints canonical node IDs for verbatim-declared
// types (`interface @event` -> `<file>::event`), so every lookup keyed
// by a type NAME must reduce to the same canonical identifier domain or
// the two halves never meet (issue #723). These pins cover the
// name-keyed lookups on the parser side; the reference-side
// canonicalizeCSharpTypeRef split is pinned through the resolver in
// internal/resolver/csharp_verbatim_type_ref_test.go.

// csharpBaseNameCounts keyed the duplicate-base prescan by the RAW
// spelling while emitCSharpBaseList looks the entry up by the canonical
// typeID - the miss made the stamp guard fail closed, so a
// verbatim-declared type silently lost target_type_args while a
// plainly-spelled sibling with the IDENTICAL base list kept it.
func TestCSharpBaseList_VerbatimDeclaredTypeKeepsTypeArgStamps(t *testing.T) {
	src := []byte(`namespace App {
    public class Widget { }
    public interface IOther<T> { }
    public class @event : IOther<Widget> { }
    public class plain  : IOther<Widget> { }
}
`)
	res, err := NewCSharpExtractor().Extract("T.cs", src)
	require.NoError(t, err)

	plainEdges := csharpBaseEdges(res, "T.cs::plain")
	require.Len(t, plainEdges, 1)
	require.NotNil(t, plainEdges[0].Meta)
	require.NotNil(t, plainEdges[0].Meta["target_type_args"],
		"the plainly-spelled control must stamp - if this fails the fixture is broken, not the canon")

	verbEdges := csharpBaseEdges(res, "T.cs::event")
	require.Len(t, verbEdges, 1,
		"the verbatim-declared type's base edge rides its canonical node ID")
	require.NotNil(t, verbEdges[0].Meta,
		"identical base list, identical evidence - the spelling of the DECLARING type must not drop the stamp")
	assert.Equal(t, plainEdges[0].Meta["target_type_args"], verbEdges[0].Meta["target_type_args"],
		"@event and plain close the same base with the same argument - equal stamps")
}

// csharpPartialIdentityOf compared enclosing namespace and type-chain
// spellings RAW, so two fragments of a genuinely partial type failed
// sameType when only one fragment spelled an ENCLOSING scope verbatim,
// and the merge was refused. One subtest per identity component, so
// each canonicalization hunk is individually revert-red.
func TestCSharpPartialMerge_VerbatimEnclosingSpellings(t *testing.T) {
	collect := func(t *testing.T, path, src string) map[string]bool {
		t.Helper()
		res, err := NewCSharpExtractor().Extract(path, []byte(src))
		require.NoError(t, err)
		targets := map[string]bool{}
		for _, e := range csharpBaseEdges(res, path+"::Box") {
			targets[e.To] = true
		}
		return targets
	}

	t.Run("outer type spelled verbatim in one fragment", func(t *testing.T) {
		targets := collect(t, "P.cs", `namespace App {
    public interface IA { void A(); }
    public interface IB { void B(); }
    public partial class @Outer { public partial class Box : IA { public void A() { } } }
    public partial class Outer  { public partial class Box : IB { public void B() { } } }
}
`)
		assert.True(t, targets["unresolved::IA"] && targets["unresolved::IB"],
			"@Outer and Outer are the same identifier - both fragments' bases must merge onto one Box, got %v", targets)
	})

	t.Run("namespace spelled verbatim in one fragment", func(t *testing.T) {
		targets := collect(t, "N.cs", `namespace @App {
    public interface IA { void A(); }
    public partial class Outer { public partial class Box : IA { public void A() { } } }
}
namespace App {
    public interface IB { void B(); }
    public partial class Outer { public partial class Box : IB { public void B() { } } }
}
`)
		assert.True(t, targets["unresolved::IA"] && targets["unresolved::IB"],
			"@App and App are the same namespace - both fragments' bases must merge onto one Box, got %v", targets)
	})
}
