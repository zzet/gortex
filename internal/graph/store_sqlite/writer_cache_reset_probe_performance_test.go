//go:build performance

package store_sqlite

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

// awaitStoreStartupWork waits for the store's one-time startup work on the
// writer (row counters seeded, the lazy edges_by_file_generation index built;
// 30 s after the open with the defaults), so a measurement of the writer's
// page cache sees the steady state the daemon is in after its first minute.
func awaitStoreStartupWork(t *testing.T, s *Store) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Minute)
	for {
		counters := !rowCountersEnabled() || s.rowCountersReady.Load()
		index := !lazyGraphIndexesEnabled() || s.fileGenerationIndexPresent()
		if counters && index {
			return
		}
		require.True(t, time.Now().Before(deadline), "startup work not done: row counters=%v lazy index=%v", counters, index)
		time.Sleep(200 * time.Millisecond)
	}
}

// Builds alternating with resets, with the daemon's defaults (the store's
// default settings and the reclaim configuration the daemon resolves). The
// resets are driven directly between builds rather than by the loop's 256 MiB
// trigger (the only departure: without it each build would have to write
// 256 MiB). Each build is an edit of a few files (evict, re-add) on a graph
// larger than a few files, cycling over a few file sets; per build it reports the writer's page-cache
// misses and its reads of the database file, first for builds with no reset
// between them, then for builds each preceded by a reset.
func TestPerformanceBuildsAlternatingWithResetsKeepTheWritersCacheWithTheDaemonDefaults(t *testing.T) {
	s, path := openWALReclaimStore(t)
	defer func() { _ = s.Close() }()
	// Builds cycle over `sets` file sets, so a build after a build finds
	// its pages cached; what a reset adds is visible against that.
	const files, perFile, builds, sets = 400, 60, 8, 4
	node := func(f, i, rev int) *graph.Node {
		file := fmt.Sprintf("pkg%d/f%d.go", f%20, f)
		return &graph.Node{
			ID: fmt.Sprintf("repo/%s::S%d", file, i), Kind: graph.KindFunction,
			Name: fmt.Sprintf("S%d_%d", f, i), FilePath: file, RepoPrefix: "repo", Language: "go",
			StartLine: i * 10, EndLine: i*10 + 8, Meta: map[string]any{"rev": rev},
		}
	}
	fileBatch := func(f, rev int) ([]*graph.Node, []*graph.Edge) {
		nodes := make([]*graph.Node, perFile)
		for i := range nodes {
			nodes[i] = node(f, i, rev)
		}
		edges := make([]*graph.Edge, 0, 2*perFile)
		for i := range nodes {
			edges = append(edges,
				&graph.Edge{From: nodes[i].ID, To: nodes[(i+1)%perFile].ID, Kind: graph.EdgeCalls, FilePath: nodes[i].FilePath, Line: i*10 + 1},
				&graph.Edge{From: nodes[i].ID, To: node((f+1)%files, i, 0).ID, Kind: graph.EdgeCalls, FilePath: nodes[i].FilePath, Line: i*10 + 2})
		}
		return nodes, edges
	}
	for f := 0; f < files; f += 20 {
		var nodes []*graph.Node
		var edges []*graph.Edge
		for g := f; g < f+20; g++ {
			n, e := fileBatch(g, 0)
			nodes, edges = append(nodes, n...), append(edges, e...)
		}
		s.AddBatch(nodes, edges)
	}
	cfg := resolveWALReclaimConfig()
	ckpt, err := openWALReclaimCheckpointDB(s.dbPath)
	require.NoError(t, err)
	defer func() { _ = ckpt.Close() }()
	awaitStoreStartupWork(t, s)
	reset := func() {
		// The store's own loop (the defaults) may have an attempt in flight;
		// the daemon's loop retries, so does this.
		for {
			res := s.reclaimWALOnce(cfg, ckpt, path+"-wal")
			if res.reason == "checkpoint_in_flight" {
				time.Sleep(10 * time.Millisecond)
				continue
			}
			require.Equal(t, walReclaimReset, res.outcome, res.reason)
			return
		}
	}
	rev := 0
	build := func(k int) ReaderWaitSplit {
		rev++
		before := s.ReaderWaitMark()
		for j := 0; j < 5; j++ {
			f := ((k%sets)*37 + j*53) % files
			n, e := fileBatch(f, rev)
			s.EvictFile(n[0].FilePath)
			s.AddBatch(n, e)
		}
		return s.ReaderWaitMark().Split(before)
	}
	reset()
	for k := 0; k < 2*sets; k++ {
		build(k) // warm the writer on the working set
	}
	var noReset, withReset struct{ misses, mainReads int64 }
	for k := 0; k < builds; k++ {
		d := build(k)
		t.Logf("build %d, no reset before it: writer misses=%d hits=%d WriterMainReads=%d", k, d.WriterCacheMisses, d.WriterCacheHits, d.VFS.WriterMainReads)
		noReset.misses += d.WriterCacheMisses
		noReset.mainReads += d.VFS.WriterMainReads
	}
	for k := 0; k < builds; k++ {
		reset()
		d := build(k)
		t.Logf("build %d, a reset before it: writer misses=%d hits=%d WriterMainReads=%d", k, d.WriterCacheMisses, d.WriterCacheHits, d.VFS.WriterMainReads)
		withReset.misses += d.WriterCacheMisses
		withReset.mainReads += d.VFS.WriterMainReads
	}
	t.Logf("per build: no reset misses=%.0f WriterMainReads=%.0f; after a reset misses=%.0f WriterMainReads=%.0f",
		float64(noReset.misses)/builds, float64(noReset.mainReads)/builds, float64(withReset.misses)/builds, float64(withReset.mainReads)/builds)
	// A build after a reset misses no more than a build after another build.
	slack := int64(20 * builds)
	require.LessOrEqual(t, withReset.misses, noReset.misses+slack, "the resets cold the writer's cache")
}
