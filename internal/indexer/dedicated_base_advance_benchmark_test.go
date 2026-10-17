package indexer

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

// Component replay only. The ready observation is captured before timing and
// its Git root is deliberately unavailable, so a cache regression cannot hide
// extra source work behind a fast local filesystem.
func BenchmarkDedicatedBaseCurrentReadyReplay(b *testing.B) {
	f := newDedicatedAdvanceFixture(b)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	f.ensure(b, ctx)
	f.commitFile(b, "package dedicated\nfunc AdvancementMarker() int { return 1 }\n")
	initial := f.ensure(b, ctx)
	if initial.Claim.BaseGenerationID <= 0 {
		b.Fatal("ready benchmark did not establish an adopted sparse child")
	}
	observation, err := f.observe(b, ctx)
	if err != nil {
		b.Fatal(err)
	}
	observation.RootPath = filepath.Join(b.TempDir(), "unavailable-git-root")
	observe := func(context.Context) (dedicatedBaseObservation, error) { return observation, nil }
	checkWrites, err := installDedicatedWriteAudit(ctx, f.builder.Store, f.request.StorePath)
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		result, err := f.publisher.ensureCurrent(ctx, f.leases, observe)
		if err != nil || result.Claim.GenerationID != initial.Claim.GenerationID || !result.Adoption.AlreadyAdopted || !result.Report.Coalesced {
			b.Fatalf("ready replay changed publication: result=%+v err=%v", result, err)
		}
	}
	b.StopTimer()
	if err := checkWrites(); err != nil {
		b.Fatal(err)
	}
}

// A real growing adopted ancestry, unlike the fixed-parent delta-builder
// benchmark. Fixture Git commits are prepared outside the timer. Observation,
// planning, reads, physical writes and guarded adoption are timed. The small
// fixture and excluded commit creation make this a component measurement, not
// a daemon/SSD-write benchmark or evidence of a production compaction bound.
func BenchmarkDedicatedBaseCurrentGrowingAncestry(b *testing.B) {
	f := newDedicatedAdvanceFixture(b)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	f.ensure(b, ctx)
	fullRoots, paths := 0, 0
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		f.commitFile(b, fmt.Sprintf("package dedicated\nfunc AdvancementMarker() int { return %d }\n", i))
		b.StartTimer()
		result := f.ensure(b, ctx)
		if result.Report.Coalesced {
			b.Fatal("changed tree reused without a physical build")
		}
		if result.Claim.BaseGenerationID == 0 {
			fullRoots++
		}
		paths += len(result.Report.IndexedPaths)
	}
	b.StopTimer()
	b.ReportMetric(float64(fullRoots), "full_reseeds")
	if b.N > 0 {
		b.ReportMetric(float64(paths)/float64(b.N), "indexed_paths/op")
	}
}
