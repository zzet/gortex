package languages

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

func TestGoPragmas_ExactAboveDeclarationFlags(t *testing.T) {
	for _, tc := range []struct {
		name, prefix, decl string
		want               map[string]any
	}{
		{"export", "//export Target\n", "func Target() {}", map[string]any{"cgo_export": true}},
		{"linkname", "//go:linkname Target hidden\n", "func Target() {}", map[string]any{"go_linkname": true}},
		{"nearest_linkname", "//export Target\n//go:linkname Target hidden\n", "func Target() {}", map[string]any{"go_linkname": true}},
		{"nearest_export", "//go:linkname Target hidden\n//export Target\n", "func Target() {}", map[string]any{"cgo_export": true}},
		{"fifth_line", "//export Target\n\n\n\n\n", "func Target() {}", map[string]any{"cgo_export": true}},
		{"sixth_line", "//export Target\n\n\n\n\n\n", "func Target() {}", map[string]any{}},
		{"blocked", "//export Target\nvar barrier=1\n", "func Target() {}", map[string]any{}},
		{"indented", "  //export Target\n", "  func Target() {}", map[string]any{"cgo_export": true}},
		{"same_line_declarations", "//export Target\n", "func Before() {}; func Target() {}", map[string]any{"cgo_export": true}},
		{"inside_body", "", "func Target() {\n//export Target\n}", map[string]any{}},
		{"method", "type Server struct{}\n//export Target\n", "func(s *Server) Target() {}", map[string]any{"cgo_export": true}},
	} {
		for _, ending := range []string{"lf", "crlf"} {
			t.Run(tc.name+"/"+ending, func(t *testing.T) {
				src := "package p\n" + tc.prefix + tc.decl
				if ending == "crlf" {
					src = strings.ReplaceAll(src, "\n", "\r\n")
				}
				result, err := NewGoExtractor().Extract("pragmas.go", []byte(src))
				require.NoError(t, err)
				defer result.ReleaseTree()
				var target *graph.Node
				for _, node := range result.Nodes {
					if node.Name == "Target" && (node.Kind == graph.KindFunction || node.Kind == graph.KindMethod) {
						target = node
						break
					}
				}
				require.NotNil(t, target)
				got := map[string]any{}
				for _, key := range []string{"cgo_export", "go_linkname"} {
					if value, ok := target.Meta[key]; ok {
						got[key] = value
					}
				}
				require.Equal(t, tc.want, got, "pragma precedence, scan bound and same-line behavior must retain the row-based contract")
			})
		}
	}
}
