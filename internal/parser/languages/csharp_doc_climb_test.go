package languages

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A doc comment belongs to the declaration directly beneath it. The
// upward climb skips "wrapper" lines (a decorator, a bare `export` /
// `public` line) so a doc above a wrapper still reaches its declaration,
// but a SIBLING declaration that merely starts with a modifier is not a
// wrapper: climbing past `public int A { get; set; }` hands A's doc to
// every undocumented member below it, and the climb itself is linear in
// the member count per member (quadratic per file).
func TestCSharpDoc_DoesNotInheritSiblingDoc(t *testing.T) {
	src := []byte(`namespace App {
    public sealed class Dto {
        /// <summary>Doc for A.</summary>
        public int A { get; set; }
        public int B { get; set; }

        private int _c;
        // note on D
        private int _d;
        private int _e;
    }
}
`)
	res, err := NewCSharpExtractor().Extract("Dto.cs", src)
	require.NoError(t, err)
	docs := map[string]any{}
	for _, n := range res.Nodes {
		if n.Meta != nil {
			if d, ok := n.Meta["doc"]; ok {
				docs[n.Name] = d
			}
		}
	}
	assert.Equal(t, "Doc for A.", docs["A"], "the documented member keeps its own doc")
	assert.NotContains(t, docs, "B", "an undocumented sibling must not inherit the doc above the member before it")
	assert.NotContains(t, docs, "_c", "a blank line and a sibling do not carry a doc down")
	assert.Equal(t, "note on D", docs["_d"], "a plain // comment directly above a field is its doc")
	assert.NotContains(t, docs, "_e", "the field after a documented field is undocumented")
}
