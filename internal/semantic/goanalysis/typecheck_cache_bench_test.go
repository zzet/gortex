package goanalysis

import (
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/semantic"
	"go.uber.org/zap"
)

// BenchmarkTypecheckCacheRepositoryLoad measures the first handle-rooted load
// separately from a retained-closure hit. It reads this checkout's server
// package without changing its source or writing graph output. A cold compiler
// state does not imply a cold Go build cache: keep that distinction when
// comparing a newly registered checkout with later edits in the same checkout.
func BenchmarkTypecheckCacheRepositoryLoad(b *testing.B) {
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		b.Fatal("cannot locate benchmark source")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(source), "..", "..", ".."))
	scope := semantic.CheckoutCompilerScope{HandleRoots: true, StripSiblingBodies: true, TypecheckCache: true}
	plan := planHandleRoots(root, root, "", map[string]struct{}{"internal/server/handler.go": {}}, scope)
	if plan.full || plan.empty() {
		b.Fatalf("expected handle-rooted server package: %+v", plan)
	}
	digest := goManifestDigest(root)
	for _, cold := range []bool{true, false} {
		name := "retained"
		if cold {
			name = "cold"
		}
		b.Run(name, func(b *testing.B) {
			provider := NewProvider(ModeTypeCheck, false, zap.NewNop())
			defer provider.Close()
			load := func(p *Provider) semantic.CompilerCacheStats {
				b.Helper()
				var stats semantic.CompilerCacheStats
				program, err := p.loadCheckoutProgramCached(b.Context(), root, digest, plan, scope, &stats)
				if program.release != nil {
					defer program.release()
				}
				if err != nil {
					b.Fatal(err)
				}
				if program.bypass != "" || program.program.files == 0 {
					b.Fatalf("expected a nonempty cached compiler load: bypass=%q, files=%d", program.bypass, program.program.files)
				}
				return stats
			}
			if !cold {
				load(provider)
			}
			var listMS, parseMS, checkMS, validateMS int64
			var misses, hits, exportReads, filesParsed, filesReused int
			b.ResetTimer()
			started := time.Now()
			for i := 0; i < b.N; i++ {
				if cold {
					provider = NewProvider(ModeTypeCheck, false, zap.NewNop())
				}
				stats := load(provider)
				listMS += stats.GoListMs
				parseMS += stats.ParseMs
				checkMS += stats.CheckMs
				validateMS += stats.ValidateMs
				misses += stats.ClosureMisses
				hits += stats.ClosureHits
				exportReads += stats.ExportReads
				filesParsed += stats.FilesParsed
				filesReused += stats.FilesReused
				if cold {
					provider.Close()
				}
			}
			elapsed := time.Since(started)
			b.StopTimer()
			perOp := float64(b.N)
			b.ReportMetric(float64(listMS)/perOp, "go-list-ms/op")
			b.ReportMetric(float64(parseMS)/perOp, "parse-ms/op")
			b.ReportMetric(float64(checkMS)/perOp, "check-ms/op")
			b.ReportMetric(float64(validateMS)/perOp, "validate-ms/op")
			b.ReportMetric(float64(misses)/perOp, "closure-misses/op")
			b.ReportMetric(float64(hits)/perOp, "closure-hits/op")
			b.ReportMetric(float64(exportReads)/perOp, "export-reads/op")
			b.ReportMetric(float64(filesParsed)/perOp, "files-parsed/op")
			b.ReportMetric(float64(filesReused)/perOp, "files-reused/op")
			b.ReportMetric(float64(elapsed.Milliseconds())/perOp, "load-ms/op")
		})
	}
}
