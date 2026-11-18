package store_sqlite

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/graph"
)

const (
	indexMaintenanceBenchmarkDefaultNodes        = 10_000
	indexMaintenanceBenchmarkDefaultEdgesPerNode = 10
	indexMaintenanceBenchmarkDefaultPrefillScale = 3
	indexMaintenanceBenchmarkMaxGeneratedEdges   = 1_000_000
)

type indexMaintenanceBenchmarkPayload struct {
	nodes []*graph.Node
	edges []*graph.Edge
}

type indexMaintenanceBenchmarkPragmas struct {
	cacheSize         int64
	mmapSize          int64
	tempStore         int64
	synchronous       int64
	walAutoCheckpoint int64
	journalMode       string
}

// BenchmarkSQLiteIndexMaintenanceAblation measures the production AddBatch
// path while changing one condition at a time: live secondary indexes, an
// already-populated database, and a concurrent writer. Run it with
// GOMAXPROCS=1 and -benchtime=1x for a bounded causal sample. The workload can
// be changed with GORTEX_SQLITE_BENCH_NODES, GORTEX_SQLITE_BENCH_EDGES_PER_NODE,
// and GORTEX_SQLITE_BENCH_PREFILL_SCALE; the combined generated edge count is
// capped so an accidental invocation cannot create an unbounded fixture.
//
// The benchmark uses synthetic structural rows and the real store writer. It
// does not exercise symbol/content FTS or predict whole-corpus wall time.
func BenchmarkSQLiteIndexMaintenanceAblation(b *testing.B) {
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

	payload := makeIndexMaintenanceBenchmarkPayload("payload", nodeCount, edgesPerNode)
	prefill := makeIndexMaintenanceBenchmarkPayload("prefill", nodeCount*prefillScale, edgesPerNode)
	concurrent := makeIndexMaintenanceBenchmarkPayload("concurrent", nodeCount, edgesPerNode)

	cases := []struct {
		name             string
		coordinatedCold  bool
		prefill          bool
		concurrentWriter bool
	}{
		{name: "cold_indexes_suppressed_empty", coordinatedCold: true},
		{name: "live_indexes_empty"},
		{name: "cold_indexes_suppressed_prefilled", coordinatedCold: true, prefill: true},
		{name: "live_indexes_prefilled", prefill: true},
		{name: "cold_indexes_suppressed_prefilled_concurrent_writer", coordinatedCold: true, prefill: true, concurrentWriter: true},
		{name: "live_indexes_prefilled_concurrent_writer", prefill: true, concurrentWriter: true},
	}

	for _, tc := range cases {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			var setupElapsed time.Duration
			var insertElapsed time.Duration
			var sealElapsed time.Duration
			var insertedRows int64
			var finalRows int64
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
				pragmas = readIndexMaintenanceBenchmarkPragmas(b, store)

				expectedNodes := len(payload.nodes)
				expectedEdges := len(payload.edges)
				iterationFinalRows := int64(expectedNodes + expectedEdges)
				if tc.prefill {
					setupStarted := time.Now()
					store.AddBatch(prefill.nodes, prefill.edges)
					setupElapsed += time.Since(setupStarted)
					expectedNodes += len(prefill.nodes)
					expectedEdges += len(prefill.edges)
					iterationFinalRows += int64(len(prefill.nodes) + len(prefill.edges))
				}

				b.StartTimer()
				started := time.Now()
				iterationInsertedRows := int64(len(payload.nodes) + len(payload.edges))
				if tc.concurrentWriter {
					start := make(chan struct{})
					var writers sync.WaitGroup
					writers.Add(1)
					go func() {
						defer writers.Done()
						<-start
						store.AddBatch(concurrent.nodes, concurrent.edges)
					}()
					close(start)
					store.AddBatch(payload.nodes, payload.edges)
					writers.Wait()
					iterationInsertedRows += int64(len(concurrent.nodes) + len(concurrent.edges))
					iterationFinalRows += int64(len(concurrent.nodes) + len(concurrent.edges))
					expectedNodes += len(concurrent.nodes)
					expectedEdges += len(concurrent.edges)
				} else {
					store.AddBatch(payload.nodes, payload.edges)
				}
				insertElapsed += time.Since(started)
				insertedRows += iterationInsertedRows
				finalRows += iterationFinalRows
				b.StopTimer()

				if tc.coordinatedCold {
					sealStarted := time.Now()
					if err := store.EndCoordinatedBulkLoad(); err != nil {
						_ = store.Close()
						b.Fatal(err)
					}
					sealElapsed += time.Since(sealStarted)
				}
				if got := store.NodeCount(); got != expectedNodes {
					_ = store.Close()
					b.Fatalf("node parity: got %d, want %d", got, expectedNodes)
				}
				if got := store.EdgeCount(); got != expectedEdges {
					_ = store.Close()
					b.Fatalf("edge parity: got %d, want %d", got, expectedEdges)
				}
				if got := store.GetNode(payload.nodes[len(payload.nodes)-1].ID); got == nil {
					_ = store.Close()
					b.Fatal("payload sentinel was not durable")
				}
				if tc.prefill && store.GetNode(prefill.nodes[len(prefill.nodes)-1].ID) == nil {
					_ = store.Close()
					b.Fatal("prefill sentinel was not durable")
				}
				if tc.concurrentWriter && store.GetNode(concurrent.nodes[len(concurrent.nodes)-1].ID) == nil {
					_ = store.Close()
					b.Fatal("concurrent-writer sentinel was not durable")
				}
				if err := store.Close(); err != nil {
					b.Fatal(err)
				}
			}

			if insertedRows > 0 {
				b.ReportMetric(float64(insertElapsed.Nanoseconds())/float64(insertedRows), "insert-ns/row")
				b.ReportMetric(float64(insertedRows)/insertElapsed.Seconds(), "insert-rows/s")
			}
			if finalRows > 0 {
				endToEnd := setupElapsed + insertElapsed + sealElapsed
				b.ReportMetric(float64(endToEnd.Nanoseconds())/float64(finalRows), "end-to-end-ns/row")
				b.ReportMetric(float64(finalRows)/float64(b.N), "final-input-rows/op")
			}
			b.ReportMetric(float64(setupElapsed.Nanoseconds())/float64(b.N), "prefill-ns/op")
			b.ReportMetric(float64(sealElapsed.Nanoseconds())/float64(b.N), "seal-ns/op")
			b.ReportMetric(float64(runtime.GOMAXPROCS(0)), "gomaxprocs")
			b.ReportMetric(float64(pragmas.cacheSize), "pragma-cache-size")
			b.ReportMetric(float64(pragmas.mmapSize), "pragma-mmap-bytes")
			b.ReportMetric(float64(pragmas.tempStore), "pragma-temp-store")
			b.ReportMetric(float64(pragmas.synchronous), "pragma-synchronous")
			b.ReportMetric(float64(pragmas.walAutoCheckpoint), "pragma-wal-autocheckpoint")
			b.Logf("effective writer journal_mode=%s", pragmas.journalMode)
		})
	}
}

func indexMaintenanceBenchmarkEnv(b *testing.B, name string, defaultValue, maxValue int) int {
	b.Helper()
	raw := os.Getenv(name)
	if raw == "" {
		return defaultValue
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value <= 0 || value > maxValue {
		b.Fatalf("%s=%q must be an integer in [1,%d]", name, raw, maxValue)
	}
	return value
}

func readIndexMaintenanceBenchmarkPragmas(b *testing.B, store *Store) indexMaintenanceBenchmarkPragmas {
	b.Helper()
	ctx := context.Background()
	conn := store.bulkConn
	owned := false
	if conn == nil {
		var err error
		conn, err = store.writerDB.Conn(ctx)
		if err != nil {
			b.Fatalf("acquire writer connection for PRAGMAs: %v", err)
		}
		owned = true
	}
	if owned {
		defer func() {
			if err := conn.Close(); err != nil {
				b.Fatalf("release writer connection after PRAGMAs: %v", err)
			}
		}()
	}
	read := func(name string) int64 {
		value, err := pragmaInt(ctx, conn, name)
		if err != nil {
			b.Fatalf("read PRAGMA %s: %v", name, err)
		}
		return value
	}
	var journalMode string
	if err := conn.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&journalMode); err != nil {
		b.Fatalf("read PRAGMA journal_mode: %v", err)
	}
	return indexMaintenanceBenchmarkPragmas{
		cacheSize:         read("cache_size"),
		mmapSize:          read("mmap_size"),
		tempStore:         read("temp_store"),
		synchronous:       read("synchronous"),
		walAutoCheckpoint: read("wal_autocheckpoint"),
		journalMode:       journalMode,
	}
}

func makeIndexMaintenanceBenchmarkPayload(prefix string, nodeCount, edgesPerNode int) indexMaintenanceBenchmarkPayload {
	payload := indexMaintenanceBenchmarkPayload{
		nodes: make([]*graph.Node, 0, nodeCount),
		edges: make([]*graph.Edge, 0, nodeCount*edgesPerNode),
	}
	for i := 0; i < nodeCount; i++ {
		id := fmt.Sprintf(
			"%s/repository/internal/package-%04d/file-%05d.go::Receiver%06d.Method%06d",
			prefix, i%1_000, i%10_000, i, i,
		)
		payload.nodes = append(payload.nodes, &graph.Node{
			ID:   id,
			Name: fmt.Sprintf("Receiver%06d.Method%06d", i, i),
			Kind: graph.KindFunction,
		})
	}
	for i := 0; i < nodeCount; i++ {
		from := payload.nodes[i].ID
		for j := 0; j < edgesPerNode; j++ {
			to := payload.nodes[(i*17+j+1)%nodeCount].ID
			payload.edges = append(payload.edges, &graph.Edge{
				From: from,
				To:   to,
				Kind: "calls",
			})
		}
	}
	return payload
}
