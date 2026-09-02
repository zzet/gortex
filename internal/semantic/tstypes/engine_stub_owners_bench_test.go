package tstypes

// Cost-shape benchmark for issue #729: stubOwnersAt runs once per
// surviving call fact, and every fact on a physical line asks the same
// (line, name) question. A class whose whole body sits on ONE line —
// generated and minified C# does this — therefore concentrates N call
// sites on a single stubsByLine bucket, and without the memo the
// per-fact owner scan grows super-quadratically with N (49x over the
// pre-adoption base at N=1600). Run with -benchtime=1x across the
// member counts to see the growth shape; the fix restores clean
// doubling.

import (
	"fmt"
	"strings"
	"testing"

	"go.uber.org/zap"
)

func BenchmarkEnrichSingleLineClassBody(b *testing.B) {
	for _, members := range []int{200, 400, 800} {
		b.Run(fmt.Sprintf("members=%d", members), func(b *testing.B) {
			var body strings.Builder
			body.WriteString("namespace B {\n    public class App {\n        private Svc worker;\n        ")
			for i := 0; i < members; i++ {
				fmt.Fprintf(&body, "public int M%d() { worker.Run(); return 1; } ", i)
			}
			body.WriteString("\n    }\n}\n")
			files := map[string]string{
				"A/Svc.cs": csSvc,
				"B/App.cs": body.String(),
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				// Enrich mutates the graph, so each iteration pays for a
				// fresh extraction — outside the timer.
				b.StopTimer()
				g, dir := buildFixture(b, files)
				p := NewProvider(CSharpSpec(), zap.NewNop())
				b.StartTimer()
				if _, err := p.Enrich(g, dir); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
