package store_sqlite

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

// The large synthetic store: the clone's size (2.2 million nodes, 11 million
// edges; files of 30 functions with five out-edges each, two repositories,
// three languages, names shared across files). Built once at the path in
// GORTEX_STORE_LARGE_STORE and kept, so every measurement that needs a store
// of realistic size reads the same one (GORTEX_STORE_LARGE_FILES sets the
// size, default 73,400 files).
const largeStorePerFile = 30

func largeStoreFiles() int {
	if v, err := strconv.Atoi(os.Getenv("GORTEX_STORE_LARGE_FILES")); err == nil && v > 0 {
		return v
	}
	return 73400
}

func largeStoreNode(files, g, i int) *graph.Node {
	repo := []string{"repo", "other"}[g%2]
	file := fmt.Sprintf("%s/pkg%d/f%d.go", repo, g%400, g)
	return &graph.Node{ID: fmt.Sprintf("%s::S%d_%d", file, g, i), Kind: graph.KindFunction,
		Name:     fmt.Sprintf("N%d", (g*7+i)%50000),
		FilePath: file, RepoPrefix: repo, Language: []string{"go", "python", "typescript"}[g%3],
		StartLine: i * 10, EndLine: i*10 + 8,
		Meta: map[string]any{"doc": fmt.Sprintf("function %d of file %d", i, g)}}
}

func largeStoreFileBatch(files, g, rev int) ([]*graph.Node, []*graph.Edge) {
	nodes := make([]*graph.Node, largeStorePerFile)
	for i := range nodes {
		nodes[i] = largeStoreNode(files, g, i)
		if rev > 0 {
			nodes[i].Meta["rev"] = rev
		}
	}
	edges := make([]*graph.Edge, 0, 5*largeStorePerFile)
	for i, n := range nodes {
		for e := 0; e < 5; e++ {
			to := largeStoreNode(files, (g+e*131+1)%files, (i+e)%largeStorePerFile).ID
			edges = append(edges, &graph.Edge{From: n.ID, To: to, Kind: graph.EdgeCalls, FilePath: n.FilePath, Line: i*10 + e})
		}
	}
	return nodes, edges
}

// openLargeStore opens the kept store, building it first when absent.
func openLargeStore(t *testing.T) (*Store, string) {
	t.Helper()
	path := os.Getenv("GORTEX_STORE_LARGE_STORE")
	if path == "" {
		t.Skip("set GORTEX_STORE_LARGE_STORE=<path> (builds, or reuses, a store of 2.2M nodes and 11M edges)")
	}
	files := largeStoreFiles()
	if _, err := os.Stat(path); os.IsNotExist(err) {
		started := time.Now()
		s, err := Open(path)
		require.NoError(t, err)
		for f := 0; f < files; f += 200 {
			var nodes []*graph.Node
			var edges []*graph.Edge
			for g := f; g < min(f+200, files); g++ {
				n, e := largeStoreFileBatch(files, g, 0)
				nodes, edges = append(nodes, n...), append(edges, e...)
			}
			require.NoError(t, s.AddBatchChecked(nodes, edges))
			if f%(files/10+1) < 200 {
				t.Logf("large store: %d/%d files after %s wal=%.0fMiB", f+200, files, time.Since(started).Round(time.Second), float64(walFileSize(path+"-wal"))/(1<<20))
			}
		}
		require.NoError(t, s.Close())
		t.Logf("large store built: files=%d in %s", files, time.Since(started).Round(time.Second))
	}
	s, err := Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	return s, path
}

// The writer hold of each planner index's ANALYZE at the clone's size, with
// the daemon's analysis_limit (plannerStatsAnalysisLimit): the time an edit
// beside an insistent refresh can wait for one index. Twice: the first pass
// after the store's open (its pages as the OS holds them), then again.
func TestPlannerStatsIndexHoldAtTheClonesSize(t *testing.T) {
	s, path := openLargeStore(t)
	fi, err := os.Stat(path)
	require.NoError(t, err)
	t.Logf("store: %s db=%.1fGiB nodes=%d edges=%d", path, float64(fi.Size())/(1<<30), s.NodeCount(), s.EdgeCount())
	present, err := s.plannerStatsPresentIndexList(context.Background())
	require.NoError(t, err)
	for pass := 1; pass <= 2; pass++ {
		type hold struct {
			name string
			took time.Duration
		}
		var holds []hold
		var sum time.Duration
		for _, name := range present {
			s.writeMu.Lock()
			at := time.Now()
			_, err := s.analyzePlannerStatsIndexLocked(context.Background(), name)
			took := time.Since(at)
			s.writeMu.Unlock()
			require.NoError(t, err)
			holds = append(holds, hold{name, took})
			sum += took
		}
		sort.Slice(holds, func(i, j int) bool { return holds[i].took > holds[j].took })
		for _, h := range holds {
			t.Logf("pass %d: ANALYZE %-30s writer held %s", pass, h.name, h.took.Round(time.Microsecond))
		}
		t.Logf("pass %d: indexes=%d longest=%s (%s) sum=%s", pass, len(holds), holds[0].took.Round(time.Microsecond), holds[0].name, sum.Round(time.Millisecond))
	}
	// And the whole owed refresh, from no statistics, through the runtime
	// path (probes included).
	_, _ = s.writerDB.Exec(`ANALYZE sqlite_schema`)
	_, err = s.writerDB.Exec(`DELETE FROM sqlite_stat1`)
	require.NoError(t, err)
	_, _ = s.writerDB.Exec(`ANALYZE sqlite_schema`)
	started := time.Now()
	h, err := s.EnsurePlannerStatsFresh(context.Background())
	require.NoError(t, err)
	t.Logf("owed refresh from no statistics: refreshed=%v reason=%q took=%s", h.Refreshed, h.Reason, time.Since(started).Round(time.Millisecond))
}

// What today's cooperative pass costs an edit at the clone's size: one
// index's ANALYZE under the writer, interrupted when an edit cycle takes the
// build lane (cancelOnEditCycle) or at the per-index limit. The edit's wait is
// the time from the lane being taken to the writer being free.
func TestCooperativeAnalyzeGivesWayToAnEditAtTheClonesSize(t *testing.T) {
	s, _ := openLargeStore(t)
	lane := &fakeBuildLane{}
	lane.install(s)
	for _, name := range []string{"edges_by_from_line", "edges_by_kind", "nodes_by_file"} {
		s.writeMu.Lock()
		indexCtx, cancel := context.WithTimeout(context.Background(), plannerStatsIndexTimeout)
		stop, yielded := s.cancelOnEditCycle(cancel)
		done := make(chan time.Duration, 1)
		started := time.Now()
		go func() {
			_, _ = s.analyzePlannerStatsIndexLocked(indexCtx, name)
			done <- time.Since(started)
		}()
		time.Sleep(time.Second)
		lane.held.Store(true) // an edit cycle takes the lane
		taken := time.Now()
		held := <-done
		freed := time.Since(taken)
		stop()
		cancel()
		s.writeMu.Unlock()
		lane.held.Store(false)
		t.Logf("ANALYZE %-22s interrupted=%v writer held %s; the edit waited %s from taking the lane", name, yielded.Load(), held.Round(time.Millisecond), freed.Round(time.Millisecond))
	}
}
