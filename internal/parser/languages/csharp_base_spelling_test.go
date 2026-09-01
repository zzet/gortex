package languages

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
)

func csharpBaseEdges(res *parser.ExtractionResult, fromID string) []*graph.Edge {
	var out []*graph.Edge
	for _, e := range res.Edges {
		if e.From == fromID && (e.Kind == graph.EdgeImplements || e.Kind == graph.EdgeExtends) {
			out = append(out, e)
		}
	}
	return out
}

// Verbatim `@` and unicode-escape spellings in BASE-LIST position are
// legal respellings of the same identifier, but the base name was used
// RAW - so `@IRack` missed the I-prefix interface test (landing as
// EdgeExtends), missed the duplicate count against a plainly-spelled
// sibling (letting both stamp their closures), spelled its unresolved
// target `unresolved::@IRack`, and an alias spelled `@RY` missed the
// alias-sentinel lookup entirely (round-5 finding 7).
func TestCSharpBaseList_VerbatimAndEscapedSpellingsCanonicalize(t *testing.T) {
	srcTemplate := func(base string) []byte {
		return []byte(`using @RY = App.IRack<App.Slat>;
namespace App {
    public sealed class Slat { }
    public sealed class Board { }
    public interface IRack<T> { void Put(T item); }
    public sealed class Dual : ` + base + `, IRack<Board> {
        public void Put(Slat item) { }
        public void Put(Board item) { }
    }
}
`)
	}

	t.Run("verbatim direct base", func(t *testing.T) {
		res, err := NewCSharpExtractor().Extract("V.cs", srcTemplate("@IRack<Slat>"))
		require.NoError(t, err)
		edges := csharpBaseEdges(res, "V.cs::Dual")
		require.Len(t, edges, 2)
		for _, e := range edges {
			assert.Equal(t, "unresolved::IRack", e.To,
				"the spelling is not the identity - both entries name IRack")
			assert.Equal(t, graph.EdgeImplements, e.Kind,
				"the I-prefix test must see IRack, not @IRack")
			if e.Meta != nil {
				assert.Nil(t, e.Meta["target_type_args"],
					"two entries closing the same erased base must stamp NOTHING - the raw spelling let both stamp")
			}
		}
	})

	t.Run("escaped direct base", func(t *testing.T) {
		res, err := NewCSharpExtractor().Extract("U.cs", srcTemplate(`\u0049Rack<Slat>`))
		require.NoError(t, err)
		edges := csharpBaseEdges(res, "U.cs::Dual")
		require.Len(t, edges, 2)
		for _, e := range edges {
			assert.Equal(t, "unresolved::IRack", e.To,
				"the escape decodes to IRack - one erased base, spelled twice")
			assert.Equal(t, graph.EdgeImplements, e.Kind)
			if e.Meta != nil {
				assert.Nil(t, e.Meta["target_type_args"])
			}
		}
	})

	t.Run("escaped alias reference meets the verbatim declaration", func(t *testing.T) {
		res, err := NewCSharpExtractor().Extract("E.cs", srcTemplate(`\u0052Y`))
		require.NoError(t, err)
		edges := csharpBaseEdges(res, "E.cs::Dual")
		require.Len(t, edges, 2)
		for _, e := range edges {
			if e.Meta != nil {
				assert.Nil(t, e.Meta["target_type_args"],
					"the escaped spelling names the @-declared alias RY - the sentinel must still fire")
			}
		}
	})

	t.Run("verbatim alias base trips the alias sentinel", func(t *testing.T) {
		res, err := NewCSharpExtractor().Extract("A.cs", srcTemplate("@RY"))
		require.NoError(t, err)
		edges := csharpBaseEdges(res, "A.cs::Dual")
		require.Len(t, edges, 2)
		for _, e := range edges {
			if e.Meta != nil {
				assert.Nil(t, e.Meta["target_type_args"],
					"an alias-spelled base makes every sibling's target unprovable - nothing may stamp")
			}
		}
	})
}
