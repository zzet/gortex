package indexer

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/indexer/source"
)

// BenchmarkDerivedGenerationBuildStrategy measures the cold-build path whose
// accidental direct-to-SQLite routing made each dedicated repository spend
// most of its wall time in resolver/store round trips. Fixture and store setup
// stay outside the timer; both strategies build and publish the same derived
// payload, under the production file/byte and process-wide shadow budgets.
func BenchmarkDerivedGenerationBuildStrategy(b *testing.B) {
	// Enough source files to make resolver/store round trips visible without
	// letting the quadratic clone corpus dominate the strategy under test.
	tree := derivedShadowCorpus(2048)
	root := builderTempDir(b, "derived-shadow-benchmark-source")
	builderWriteTree(b, root, tree)

	for _, strategy := range []struct {
		name           string
		shadowMaxFiles string
	}{
		{name: "direct_sqlite", shadowMaxFiles: "0"},
		{name: "bounded_shadow", shadowMaxFiles: "1000000"},
	} {
		b.Run(strategy.name, func(b *testing.B) {
			b.Setenv("GORTEX_SHADOW_MAX_FILES", strategy.shadowMaxFiles)
			b.Setenv("GORTEX_SHADOW_MAX_BYTES", "1073741824")
			b.ReportAllocs()
			var files, nodes, edges int
			for i := 0; i < b.N; i++ {
				b.StopTimer()
				store, err := store_sqlite.Open(filepath.Join(
					b.TempDir(), fmt.Sprintf("derived-shadow-%d.sqlite", i)))
				if err != nil {
					b.Fatalf("open benchmark store: %v", err)
				}
				target, err := source.NewFilesystemSource(root)
				if err != nil {
					_ = store.Close()
					b.Fatalf("open benchmark source: %v", err)
				}
				builder := builderNewBuilder(store)
				builder.Config.Workers = 4
				clonesOff := false
				builder.Config.Coverage.Clones.Enabled = &clonesOff
				builder.Logger = zap.NewNop()
				request := derivedShadowBuildRequest(root, target, tree)
				request.Identity.LayerID = fmt.Sprintf("%s-%d", derivedShadowLayerID, i)

				b.StartTimer()
				generationID, report, buildErr := builder.Build(context.Background(), request)
				b.StopTimer()

				closeSourceErr := target.Close()
				if buildErr != nil {
					_ = store.Close()
					b.Fatalf("build derived generation: %v", buildErr)
				}
				if closeSourceErr != nil {
					_ = store.Close()
					b.Fatalf("close benchmark source: %v", closeSourceErr)
				}
				if generationID <= 0 || report.NodeCount == 0 || report.EdgeCount == 0 {
					_ = store.Close()
					b.Fatalf("empty benchmark payload: generation=%d nodes=%d edges=%d",
						generationID, report.NodeCount, report.EdgeCount)
				}
				files, nodes, edges = len(report.IndexedPaths), report.NodeCount, report.EdgeCount
				if err := store.Close(); err != nil {
					b.Fatalf("close benchmark store: %v", err)
				}
			}
			b.ReportMetric(float64(files), "files/op")
			b.ReportMetric(float64(nodes), "nodes/op")
			b.ReportMetric(float64(edges), "edges/op")
		})
	}
}
