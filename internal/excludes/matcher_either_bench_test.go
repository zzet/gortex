package excludes

import "testing"

var matchRelEitherBenchmarkResult bool

func BenchmarkMatchRelEitherNoNegation(b *testing.B) {
	m := New(Builtin)
	const rel = ".github/workflows/a/b/c/d/e/f/ci.yml"
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		matchRelEitherBenchmarkResult = m.MatchRelEither(rel)
	}
}

// /** needs conservative parent checks only when the list has a negation.
func BenchmarkNoNegationGlobstarPrefilter(b *testing.B) {
	m := New(append(append([]string{}, Builtin...), "/**"))
	for _, rel := range []string{"src/pkg/server/main.go", "src/pkg/server/"} {
		b.Run("MatchRel/"+rel, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				matchRelEitherBenchmarkResult = m.MatchRel(rel)
			}
		})
	}
	b.Run("MatchRelEither", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			matchRelEitherBenchmarkResult = m.MatchRelEither("src/pkg/server/main.go")
		}
	})
}
