package languages

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
	sitter "github.com/zzet/gortex/internal/parser/tsitter"
)

// Keep the independent legacy walkers as the oracle for every stamped field.
// Same-line statements, loop headers and closures make line-based scope guesses
// observably wrong, while the entire metadata map must retain its other fields.
func TestGoBodyMetrics_FullMetadataAgainstLegacyWalks(t *testing.T) {
	require.Equal(t, goComplexitySkip, loopSignalTables["go"].skip)
	require.Equal(t, goLoopTypes, loopSignalTables["go"].loop)
	for _, tc := range []struct{ name, decl string }{
		{"empty", "func Target() {}"},
		{"nested", "func Target(xs []int) { for _,x := range xs { if x > 0 { for j:=0;j<x;j++ { if j>1 {continue} } } } }"},
		{"closure_pruning", "func Target() { _ = func() { for { for { if true { break } } } } }"},
		{"loop_header", "func Target() { for i:=len(make([]int,3));i<2;i++ { _ = i } }"},
		{"same_line_outside_loop", "func Target() { strings.Contains(\"a\",\"b\"); for {break}; _ = make([]int,2) }"},
		{"loop_calls", "func Target(xs []int) { for range xs { strings.Contains(\"a\",\"b\"); _ = append(xs,1); Target(xs) } }"},
		{"member_chain", "func Target(s Something) { _ = s.a.b.c; for { _ = s.a.b.c; break } }"},
		{"method", "type Server struct{}\n// Target retains docs.\nfunc(s *Server) Target(xs []int) { for range xs { s.Target(xs); _ = new(Server) } }"},
		{"malformed_tail", "func Target() { for { if true {break} } }\nfunc Broken("},
	} {
		for _, ending := range []string{"lf", "crlf"} {
			t.Run(tc.name+"/"+ending, func(t *testing.T) {
				src := []byte("package p\n// Target retains docs.\n" + tc.decl)
				if ending == "crlf" {
					src = []byte(strings.ReplaceAll(string(src), "\n", "\r\n"))
				}
				result, err := NewGoExtractor().Extract("body.go", src)
				require.NoError(t, err)
				defer result.ReleaseTree()
				var actual *graph.Node
				for _, node := range result.Nodes {
					if node.Name == "Target" && (node.Kind == graph.KindFunction || node.Kind == graph.KindMethod) {
						actual = node
						break
					}
				}
				require.NotNil(t, actual)
				require.Equal(t, "Target retains docs.", actual.Meta["doc"])
				want := &graph.Node{Name: actual.Name, Meta: map[string]any{}}
				metrics := map[string]bool{"complexity": true, "cognitive": true, "loop_depth": true, "max_access_depth": true, "linear_scan_in_loop": true, "alloc_in_loop": true, "recursion_in_loop": true}
				for key, value := range actual.Meta {
					if !metrics[key] {
						want.Meta[key] = value
					}
				}
				root := result.Tree.Tree().RootNode()
				var declaration = findGoFuncDecl(root, src, "Target")
				if declaration == nil {
					walkNodes(root, func(n *sitter.Node) {
						if n.Type() == "method_declaration" {
							if name := n.ChildByFieldName("name"); name != nil && name.Content(src) == "Target" {
								declaration = n
							}
						}
					})
				}
				require.NotNil(t, declaration)
				body := goFuncBody(declaration)
				require.NotNil(t, body)
				before := goScratchSemantics(t, result)
				root.WithScratch(func() { StampFunctionMetrics(want, body, "go"); StampLoopSignals(want, body, src, "go") })
				require.Equal(t, want.Meta, actual.Meta, "all metric/signal keys, presence, thresholds and other metadata must match legacy independent walks")
				require.Equal(t, string(before), string(goScratchSemantics(t, result)), "oracle/scratch work must not change owned graph values")
			})
		}
	}
}
