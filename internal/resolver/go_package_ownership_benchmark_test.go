package resolver

import (
	"context"
	"testing"
)

var goPackageOwnershipRetainSink bool

// Candidate-loop microbenchmark only: source staging and parsing are outside
// the timer. The package-level sink prevents dead-code elimination.
func BenchmarkGoPackageOwnershipCandidate(b *testing.B) {
	for _, enabled := range []bool{false, true} {
		name := "nil"
		if enabled {
			name = "certified"
		}
		b.Run(name, func(b *testing.B) {
			f := newOwnershipResolverFixture(b, "go")
			if enabled {
				installFixtureOwnership(b, f, "example.test/fixture/misc/graph", f.files[1].FilePath)
			}
			gate := f.r.goImportGateForEdge(ownershipPending(f)[0], "example.test/fixture/misc/graph")
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				goPackageOwnershipRetainSink = gate.retainNode(f.definitions[i%2])
			}
		})
	}
}

// Scoped staging bookkeeping only; the callback consumes real scoped file
// rows but uses fixture authority, not Git/filesystem reads or full indexing.
func BenchmarkGoPackageOwnershipScopedStaging(b *testing.B) {
	for _, mode := range []string{"disabled", "same_epoch", "new_epoch"} {
		b.Run(mode, func(b *testing.B) {
			f := newOwnershipResolverFixture(b, "go")
			calls := 0
			if mode != "disabled" {
				f.r.SetGoPackageOwnershipFactory(ownershipFactoryForFixture(b, f, &calls))
			}
			pending := ownershipPending(f)
			ctx := context.Background()
			if err := f.r.prepareGoPackageOwnership(ctx, pending, f.r.nodeByID); err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if mode == "new_epoch" {
					f.r.clearGoPackageOwnership()
				}
				if err := f.r.prepareGoPackageOwnership(ctx, pending, f.r.nodeByID); err != nil {
					b.Fatal(err)
				}
			}
			b.StopTimer()
			b.ReportMetric(float64(calls), "factory_calls")
		})
	}
}
