package languages

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
	sitter "github.com/zzet/gortex/internal/parser/tsitter"
)

// Pin complete, ordered edges before changing traversal. In particular, a type
// parameter declared later still suppresses an earlier same-named reference;
// nested containers must retain the original preorder and per-line deduping.
func TestGoTypeUse_OrderedFullEdgesAndLaterParameters(t *testing.T) {
	const source = `package p
type Item struct{}
type Other struct{}
type Box[T any] struct{}
var Early Box[Future]
func Handler[K any](p map[Item]chan Box[Other]) {
 var later chan map[Other]Box[Item]
 _ = later
}
type DeclaredLater[Future any] struct{}
`
	for _, tc := range []struct{ name, source string }{
		{"lf", source}, {"crlf", strings.ReplaceAll(source, "\n", "\r\n")},
		{"no_final_newline", strings.TrimSuffix(source, "\n")},
		{"malformed_tail", source + "func Broken[Z any]("},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result, err := NewGoExtractor().Extract("type_refs.go", []byte(tc.source))
			require.NoError(t, err)
			defer result.ReleaseTree()
			var got []*graph.Edge
			for _, edge := range result.Edges {
				if edge.Meta["ref_context"] == "generic_arg" {
					got = append(got, edge)
				}
			}
			var want []*graph.Edge
			for _, item := range []struct {
				name string
				line int
			}{{"Item", 6}, {"Box", 6}, {"Other", 6}, {"Other", 7}, {"Box", 7}, {"Item", 7}} {
				want = append(want, &graph.Edge{From: "type_refs.go::Handler", To: "unresolved::" + item.name, Kind: graph.EdgeReferences, FilePath: "type_refs.go", Line: item.line, Origin: graph.OriginASTResolved, Meta: map[string]any{"ref_context": "generic_arg"}})
			}
			require.Equal(t, want, got, "all edge fields, order and scope must match the original two-pass extraction")
			before := goScratchSemantics(t, result)
			root := result.Tree.Tree().RootNode()
			root.WithScratch(func() { walkNodes(root, func(node *sitter.Node) { require.NotEmpty(t, node.Type()) }) })
			require.Equal(t, string(before), string(goScratchSemantics(t, result)), "graph values must own their data after traversal scratch reuse")
		})
	}
}
