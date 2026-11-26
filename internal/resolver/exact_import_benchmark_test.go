package resolver

import (
	"fmt"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// BenchmarkExactImportBinding measures one explicit-import lookup, the path a
// bare JS/TS call takes before the closure fallback, as a file's imports grow.
func BenchmarkExactImportBinding(b *testing.B) {
	for _, imports := range []int{10, 100, 500} {
		b.Run(fmt.Sprintf("imports=%d", imports), func(b *testing.B) {
			files := make(map[string]string, imports+1)
			var page strings.Builder
			for i := range imports {
				files[fmt.Sprintf("lib/d%d.ts", i)] = fmt.Sprintf("export function f%d() {}\n", i)
				fmt.Fprintf(&page, "import { f%d } from '../lib/d%d'\n", i, i)
			}
			page.WriteString("export function run() {\n")
			for i := range 200 {
				fmt.Fprintf(&page, "  f%d()\n", i*7%imports)
			}
			page.WriteString("}\n")
			files["app/page.ts"] = page.String()
			g := buildGraphFromSources(b, files)
			var calls []*graph.Edge
			for _, e := range g.GetOutEdges("app/page.ts::run") {
				if e.Kind == graph.EdgeCalls {
					calls = append(calls, e)
				}
			}
			names := make([]string, len(calls))
			for i, e := range calls {
				names[i] = graph.UnresolvedName(e.To)
			}
			r := New(g)
			var stats ResolveStats
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; b.Loop(); i++ {
				j := i % len(calls)
				if !r.tryResolveExplicitImport(calls[j], names[j], &stats) {
					b.Fatalf("%s did not bind", names[j])
				}
			}
		})
	}
}
