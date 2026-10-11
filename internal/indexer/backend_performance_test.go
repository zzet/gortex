//go:build performance

package indexer_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
)

// TestPerformanceBackendBench cold-indexes GORTEX_BENCH_ROOT through the full indexer
// pipeline into the sink named by GORTEX_BENCH_BACKEND, then runs a fixed
// query workload. Reports cold-index time, graph size, process RSS, and query
// throughput. "sqlite" is the shipping store; "memory" is not a backend any
// more — that arm drains into the indexer's cold-index staging shadow and
// stands in as the ceiling the sqlite path is measured against.
//
//	GORTEX_BENCH_ROOT=/path/to/gortex \
//	GORTEX_BENCH_BACKEND=sqlite \
//	  go test -tags performance ./internal/indexer/ -run TestPerformanceBackendBench -timeout 40m -v
func TestPerformanceBackendBench(t *testing.T) {
	root := os.Getenv("GORTEX_BENCH_ROOT")
	if root == "" {
		t.Skip("bench harness; set GORTEX_BENCH_ROOT=<repo> and GORTEX_BENCH_BACKEND=memory|sqlite")
	}
	if _, err := os.Stat(root); err != nil {
		t.Skipf("bench root not available: %v", err)
	}
	backendName := os.Getenv("GORTEX_BENCH_BACKEND")
	if backendName == "" {
		backendName = "memory"
	}

	store, cleanup := openBenchStore(t, backendName)
	defer cleanup()

	reg := parser.NewRegistry()
	languages.RegisterAll(reg)
	workers := runtime.NumCPU()
	idx := indexer.New(store, reg, config.IndexConfig{Workers: workers}, zap.NewNop())

	var m0 runtime.MemStats
	runtime.ReadMemStats(&m0)

	start := time.Now()
	res, err := idx.IndexCtx(context.Background(), root)
	indexDur := time.Since(start)
	if err != nil {
		t.Fatalf("index: %v", err)
	}
	rssAfterIndex := processRSSMB()
	var m1 runtime.MemStats
	runtime.ReadMemStats(&m1)
	fmt.Fprintf(os.Stderr, ">>> %s INDEX DONE in %s (files=%d nodes=%d edges=%d) — querying\n",
		backendName, indexDur.Round(time.Millisecond), res.FileCount, res.NodeCount, res.EdgeCount)

	qStart := time.Now()
	q := runQueryWorkload(store)
	fmt.Fprintf(os.Stderr, ">>> %s QUERY WORKLOAD DONE in %s\n", backendName, time.Since(qStart).Round(time.Millisecond))

	mb := func(b uint64) float64 { return float64(b) / (1024 * 1024) }
	t.Logf("================ BACKEND BENCH ================")
	t.Logf("backend=%s root=%s workers=%d", backendName, root, workers)
	t.Logf("cold index : %s  files=%d nodes=%d edges=%d errors=%d",
		indexDur.Round(time.Millisecond), res.FileCount, res.NodeCount, res.EdgeCount, len(res.Errors))
	if indexDur.Seconds() > 0 {
		t.Logf("throughput : %.0f files/s  %.0f nodes/s",
			float64(res.FileCount)/indexDur.Seconds(), float64(res.NodeCount)/indexDur.Seconds())
	}
	t.Logf("memory     : processRSS=%.0fMB  goHeapAlloc=%.0fMB  goTotalAlloc=%.0fMB",
		rssAfterIndex, mb(m1.HeapAlloc), mb(m1.TotalAlloc-m0.TotalAlloc))
	t.Logf("queries    : %s", q)
	t.Logf("==============================================")
	runtime.KeepAlive(store)
}

func openBenchStore(t *testing.T, name string) (graph.Store, func()) {
	t.Helper()
	switch strings.ToLower(name) {
	case "", "memory", "mem":
		return graph.New(), func() {}
	case "sqlite", "sqlite3":
		s, err := store_sqlite.Open(filepath.Join(t.TempDir(), "bench.sqlite"))
		if err != nil {
			t.Fatalf("open sqlite: %v", err)
		}
		return s, func() { _ = s.Close() }
	default:
		t.Fatalf("unknown GORTEX_BENCH_BACKEND %q (memory|sqlite)", name)
		return nil, func() {}
	}
}

// runQueryWorkload times a fixed, deterministic read mix against the freshly
// indexed store: point lookups + adjacency over a node sample, exact-name
// lookups, substring search, Stats, and a full AllEdges scan.
func runQueryWorkload(store graph.Store) string {
	nodes := store.AllNodes()
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].ID < nodes[j].ID })
	sample := sampleNodes(nodes, 2000)

	ptStart := time.Now()
	ptOps := 0
	for _, n := range sample {
		store.GetNode(n.ID)
		store.GetOutEdges(n.ID)
		store.GetInEdges(n.ID)
		ptOps += 3
	}
	ptDur := time.Since(ptStart)

	// Query DISTINCT names once each — real lookup traffic asks for a name
	// once, not N times. (A naive per-sample loop re-queries hyper-common
	// names like markdown "json code block", which match ~25k rows, hundreds
	// of times and measures result-set serialization, not lookup latency.)
	seenName := make(map[string]struct{}, len(sample))
	var names []string
	for _, n := range sample {
		if n.Name == "" {
			continue
		}
		if _, ok := seenName[n.Name]; ok {
			continue
		}
		seenName[n.Name] = struct{}{}
		names = append(names, n.Name)
	}
	nameStart := time.Now()
	nameRows := 0
	for _, nm := range names {
		nameRows += len(store.FindNodesByName(nm))
	}
	nameDur := time.Since(nameStart)
	nameOps := len(names)

	subStart := time.Now()
	for _, frag := range []string{"Index", "resolve", "Store", "config", "handler"} {
		store.FindNodesByNameContaining(frag, 50)
	}
	subDur := time.Since(subStart)

	statsStart := time.Now()
	st := store.Stats()
	statsDur := time.Since(statsStart)

	allStart := time.Now()
	allEdges := store.AllEdges()
	allDur := time.Since(allStart)

	opsPerSec := func(ops int, d time.Duration) float64 {
		if d <= 0 {
			return 0
		}
		return float64(ops) / d.Seconds()
	}
	return fmt.Sprintf(
		"sample=%d | point %d ops %s (%.0f op/s) | name %d distinct %s (%.0f op/s, %d rows) | substr 5q %s | Stats(%dn/%de) %s | AllEdges %d %s",
		len(sample),
		ptOps, ptDur.Round(time.Millisecond), opsPerSec(ptOps, ptDur),
		nameOps, nameDur.Round(time.Millisecond), opsPerSec(nameOps, nameDur), nameRows,
		subDur.Round(time.Millisecond),
		st.TotalNodes, st.TotalEdges, statsDur.Round(time.Millisecond),
		len(allEdges), allDur.Round(time.Millisecond),
	)
}

func sampleNodes(nodes []*graph.Node, n int) []*graph.Node {
	if len(nodes) <= n {
		return nodes
	}
	step := len(nodes) / n
	out := make([]*graph.Node, 0, n)
	for i := 0; i < len(nodes) && len(out) < n; i += step {
		out = append(out, nodes[i])
	}
	return out
}

// processRSSMB returns the current process RSS in MiB (reads /proc on Linux,
// falls back to `ps` on macOS).
func processRSSMB() float64 {
	if b, err := os.ReadFile("/proc/self/statm"); err == nil {
		if f := strings.Fields(string(b)); len(f) >= 2 {
			if pages, err := strconv.ParseInt(f[1], 10, 64); err == nil {
				return float64(pages*int64(os.Getpagesize())) / (1024 * 1024)
			}
		}
	}
	out, err := exec.Command("ps", "-o", "rss=", "-p", strconv.Itoa(os.Getpid())).Output()
	if err == nil {
		if kb, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64); err == nil {
			return float64(kb) / 1024
		}
	}
	return 0
}
