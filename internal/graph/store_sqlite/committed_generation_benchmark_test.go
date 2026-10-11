package store_sqlite

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

const committedGenerationBenchmarkID int64 = 1

// BenchmarkSQLiteCommittedGenerationAblation replays the store-level shape of
// dedicated-base publication: generation zero already contains a payload and
// generation one inserts the same logical node and edge IDs. Ordinary indexes
// therefore receive duplicate keys in the same ranges while the table primary
// keys and generation indexes distinguish the positive view generation.
//
// The cases separate coordinated-cold index suppression from the real
// BeginGenerationBulkLoad path, match synchronous settings in both directions,
// and include a no-generation-bulk comparator. Run with GOMAXPROCS=1 and
// -benchtime=1x. Synthetic rows exclude symbol/content FTS, catalog publication,
// parsing, and resolver work, so the result measures only the Store writer.
func BenchmarkSQLiteCommittedGenerationAblation(b *testing.B) {
	nodeCount := indexMaintenanceBenchmarkEnv(
		b, "GORTEX_SQLITE_BENCH_NODES", indexMaintenanceBenchmarkDefaultNodes, 100_000,
	)
	edgesPerNode := indexMaintenanceBenchmarkEnv(
		b, "GORTEX_SQLITE_BENCH_EDGES_PER_NODE", indexMaintenanceBenchmarkDefaultEdgesPerNode, 100,
	)
	prefillScale := indexMaintenanceBenchmarkEnv(
		b, "GORTEX_SQLITE_BENCH_PREFILL_SCALE", indexMaintenanceBenchmarkDefaultPrefillScale, 20,
	)
	generatedEdges := int64(nodeCount) * int64(edgesPerNode) * int64(prefillScale+2)
	if generatedEdges > indexMaintenanceBenchmarkMaxGeneratedEdges {
		b.Fatalf("generated edge count %d exceeds bounded limit %d", generatedEdges, indexMaintenanceBenchmarkMaxGeneratedEdges)
	}

	payload := makeIndexMaintenanceBenchmarkPayload("same-id-payload", nodeCount, edgesPerNode)
	prefill := makeIndexMaintenanceBenchmarkPayload("background-prefill", nodeCount*prefillScale, edgesPerNode)

	cases := []struct {
		name              string
		coordinatedCold   bool
		generationBulk    bool
		overrideSync      bool
		synchronousPragma int64
	}{
		{name: "cold_suppressed_actual_pragmas", coordinatedCold: true},
		{name: "cold_suppressed_sync_normal", coordinatedCold: true, overrideSync: true, synchronousPragma: 1},
		{name: "generation_live_actual_pragmas", generationBulk: true},
		{name: "generation_live_sync_off", generationBulk: true, overrideSync: true, synchronousPragma: 0},
		{name: "generation_live_no_bulk"},
	}

	for _, tc := range cases {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			var setupElapsed time.Duration
			var insertElapsed time.Duration
			var closeElapsed time.Duration
			var pragmas indexMaintenanceBenchmarkPragmas

			for i := 0; i < b.N; i++ {
				b.StopTimer()
				store, err := openPristine(b, filepath.Join(b.TempDir(), fmt.Sprintf("store-%d.sqlite", i)))
				if err != nil {
					b.Fatal(err)
				}
				if tc.coordinatedCold && !store.BeginCoordinatedBulkLoad() {
					_ = store.Close()
					b.Fatal("fresh store refused coordinated cold bulk load")
				}

				setupStarted := time.Now()
				store.AddBatch(prefill.nodes, prefill.edges)
				store.AddBatch(payload.nodes, payload.edges)
				setupElapsed += time.Since(setupStarted)

				generation := store.AtGeneration(committedGenerationBenchmarkID)
				if tc.generationBulk {
					opened, err := store.BeginGenerationBulkLoad(committedGenerationBenchmarkID)
					if err != nil {
						_ = store.Close()
						b.Fatalf("begin generation bulk load: %v", err)
					}
					if !opened {
						_ = store.Close()
						b.Fatal("empty positive generation refused generation bulk load")
					}
				}
				if tc.overrideSync {
					setCommittedGenerationBenchmarkSynchronous(b, store, tc.synchronousPragma)
				}
				pragmas = readIndexMaintenanceBenchmarkPragmas(b, store)

				b.StartTimer()
				started := time.Now()
				generation.AddBatch(payload.nodes, payload.edges)
				insertElapsed += time.Since(started)
				b.StopTimer()

				closeStarted := time.Now()
				if tc.generationBulk {
					if err := store.EndGenerationBulkLoad(); err != nil {
						_ = store.Close()
						b.Fatal(err)
					}
				}
				if tc.coordinatedCold {
					if err := store.EndCoordinatedBulkLoad(); err != nil {
						_ = store.Close()
						b.Fatal(err)
					}
				}
				closeElapsed += time.Since(closeStarted)

				if got, want := store.NodeCount(), len(prefill.nodes)+len(payload.nodes); got != want {
					_ = store.Close()
					b.Fatalf("generation-zero node parity: got %d, want %d", got, want)
				}
				if got, want := store.EdgeCount(), len(prefill.edges)+len(payload.edges); got != want {
					_ = store.Close()
					b.Fatalf("generation-zero edge parity: got %d, want %d", got, want)
				}
				if got, want := generation.NodeCount(), len(payload.nodes); got != want {
					_ = store.Close()
					b.Fatalf("generation-one node parity: got %d, want %d", got, want)
				}
				if got, want := generation.EdgeCount(), len(payload.edges); got != want {
					_ = store.Close()
					b.Fatalf("generation-one edge parity: got %d, want %d", got, want)
				}
				if store.GetNode(payload.nodes[len(payload.nodes)-1].ID) == nil {
					_ = store.Close()
					b.Fatal("generation-zero same-ID sentinel was not durable")
				}
				if generation.GetNode(payload.nodes[len(payload.nodes)-1].ID) == nil {
					_ = store.Close()
					b.Fatal("generation-one same-ID sentinel was not durable")
				}
				if err := store.Close(); err != nil {
					b.Fatal(err)
				}
			}

			insertedRows := int64(b.N * (len(payload.nodes) + len(payload.edges)))
			finalRows := int64(b.N * (len(prefill.nodes) + len(prefill.edges) + 2*(len(payload.nodes)+len(payload.edges))))
			if insertedRows > 0 {
				b.ReportMetric(float64(insertElapsed.Nanoseconds())/float64(insertedRows), "insert-ns/row")
				b.ReportMetric(float64(insertedRows)/insertElapsed.Seconds(), "insert-rows/s")
			}
			if finalRows > 0 {
				endToEnd := setupElapsed + insertElapsed + closeElapsed
				b.ReportMetric(float64(endToEnd.Nanoseconds())/float64(finalRows), "end-to-end-ns/row")
				b.ReportMetric(float64(finalRows)/float64(b.N), "final-input-rows/op")
			}
			b.ReportMetric(float64(setupElapsed.Nanoseconds())/float64(b.N), "setup-gen0-ns/op")
			b.ReportMetric(float64(closeElapsed.Nanoseconds())/float64(b.N), "close-window-ns/op")
			b.ReportMetric(float64(pragmas.cacheSize), "pragma-cache-size")
			b.ReportMetric(float64(pragmas.mmapSize), "pragma-mmap-bytes")
			b.ReportMetric(float64(pragmas.tempStore), "pragma-temp-store")
			b.ReportMetric(float64(pragmas.synchronous), "pragma-synchronous")
			b.ReportMetric(float64(pragmas.walAutoCheckpoint), "pragma-wal-autocheckpoint")
			b.Logf("effective writer journal_mode=%s", pragmas.journalMode)
		})
	}
}

func setCommittedGenerationBenchmarkSynchronous(b *testing.B, store *Store, value int64) {
	b.Helper()
	if store.bulkConn == nil {
		b.Fatal("synchronous override requires a pinned bulk writer")
	}
	if _, err := store.bulkConn.ExecContext(
		context.Background(),
		fmt.Sprintf("PRAGMA synchronous = %d", value),
	); err != nil {
		b.Fatalf("set PRAGMA synchronous=%d: %v", value, err)
	}
}
