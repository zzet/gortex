package languages

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	sitter "github.com/zzet/gortex/internal/parser/tsitter"
)

// Keep the row-based implementation as an independent oracle for offsets
// obtained from real tolerant Go ASTs, including same-line declarations.
func TestGoDocOffsetsPreserveRowSemantics(t *testing.T) {
	const source = `package p

// F documents an indented function.
  func F() {}

// T documents a type.
type T struct {
 // A documents both fields.
 A, B string
 // Embedded documents an embedded field.
 *Embedded
}
type Embedded struct{}

// M documents a method.
func (*T) M() {}

// Group documents a grouped declaration.
type (
 // Alias documents the type spec.
 Alias = T
 // Other documents another type spec.
 Other int
)

// Same documents the first same-line declaration.
func Same() {}; func Next() {}

// Last documents the final declaration.
func Last() {}`
	for _, tc := range []struct{ name, source string }{
		{"lf", source},
		{"crlf", strings.ReplaceAll(source, "\n", "\r\n")},
		{"incomplete", source + "\n// Missing body\nfunc Incomplete("},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := []byte(tc.source)
			result, err := NewGoExtractor().Extract("docs.go", src)
			require.NoError(t, err)
			defer result.ReleaseTree()
			count := 0
			walkNodes(result.Tree.Tree().RootNode(), func(node *sitter.Node) {
				got := ExtractDocAboveByte(src, int(node.StartByte()), DocLangSlashSlash)
				want := ExtractDocAbove(src, int(node.StartPoint().Row), DocLangSlashSlash)
				require.Equal(t, want, got, "kind=%s byte=%d", node.Type(), node.StartByte())
				count++
			})
			require.Greater(t, count, 30)
			docs := map[string]string{}
			for _, n := range result.Nodes {
				if doc, ok := n.Meta["doc"].(string); ok {
					docs[n.Name] = doc
				}
			}
			require.Equal(t, "F documents an indented function.", docs["F"])
			require.Equal(t, "M documents a method.", docs["M"])
			require.Equal(t, "A documents both fields.", docs["A"])
			require.Equal(t, docs["A"], docs["B"])
			require.Equal(t, "Last documents the final declaration.", docs["Last"])
		})
	}
}
