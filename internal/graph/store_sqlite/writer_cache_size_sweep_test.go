package store_sqlite

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	modernsqlite "modernc.org/sqlite"
)

// The writer's page-cache size against the pages an edit touches. With the
// daemon's defaults (the reclaim loop running with its default configuration,
// the store's pragmas), builds alternate an edit of five files and its undo on
// a graph far larger than the cache; per build the test reports the writer's
// page-cache misses, its reads of the database file and their time, and the
// memory its cache holds. The one override is the writer's cache_size, named
// per case: 8,192 pages is the daemon's (cache_size -32768 KiB at 4 KiB pages)
// and is the twin of the two larger ones.
//
// Gated: the graph is ~1 GiB on disk (GORTEX_STORE_WRITER_CACHE_SWEEP=1;
// GORTEX_STORE_WRITER_CACHE_SWEEP_FILES sets its size, default 4,000 files).
func TestWriterCacheSizeAgainstFiveFileEditsWithTheDaemonDefaults(t *testing.T) {
	if os.Getenv("GORTEX_STORE_WRITER_CACHE_SWEEP") != "1" {
		t.Skip("set GORTEX_STORE_WRITER_CACHE_SWEEP=1 (builds a ~1 GiB graph)")
	}
	files := 4000
	if v, err := strconv.Atoi(os.Getenv("GORTEX_STORE_WRITER_CACHE_SWEEP_FILES")); err == nil && v > 0 {
		files = v
	}
	const perFile, editFiles, builds = 60, 5, 24
	node := func(f, i, rev int) *graph.Node {
		file := fmt.Sprintf("pkg%d/f%d.go", f%97, f)
		return &graph.Node{
			ID: fmt.Sprintf("repo/%s::S%d", file, i), Kind: graph.KindFunction,
			// Names spread over the name indexes as real symbols do.
			Name: fmt.Sprintf("S%x_%d", (f*2654435761)%1000003, i), FilePath: file,
			RepoPrefix: "repo", Language: "go", StartLine: i * 10, EndLine: i*10 + 8,
			Meta: map[string]any{"rev": rev, "doc": fmt.Sprintf("symbol %d of file %d, revision %d", i, f, rev)},
		}
	}
	fileBatch := func(f, rev int) ([]*graph.Node, []*graph.Edge) {
		nodes := make([]*graph.Node, perFile)
		for i := range nodes {
			nodes[i] = node(f, i, rev)
		}
		edges := make([]*graph.Edge, 0, 3*perFile)
		for i := range nodes {
			edges = append(edges,
				&graph.Edge{From: nodes[i].ID, To: nodes[(i+1)%perFile].ID, Kind: graph.EdgeCalls, FilePath: nodes[i].FilePath, Line: i*10 + 1},
				&graph.Edge{From: nodes[i].ID, To: node((f*31+i)%files, i, 0).ID, Kind: graph.EdgeCalls, FilePath: nodes[i].FilePath, Line: i*10 + 2},
				&graph.Edge{From: nodes[i].ID, To: node((f*17+7*i)%files, (i+3)%perFile, 0).ID, Kind: graph.EdgeReferences, FilePath: nodes[i].FilePath, Line: i*10 + 3})
		}
		return nodes, edges
	}

	s, _ := openWALReclaimStore(t)
	defer func() { _ = s.Close() }()
	for f := 0; f < files; f += 50 {
		var nodes []*graph.Node
		var edges []*graph.Edge
		for g := f; g < min(f+50, files); g++ {
			n, e := fileBatch(g, 0)
			nodes, edges = append(nodes, n...), append(edges, e...)
		}
		s.AddBatch(nodes, edges)
	}
	awaitStoreStartupWork(t, s)
	ctx := context.Background()
	var pageSize int64
	require.NoError(t, s.writerDB.QueryRowContext(ctx, `PRAGMA page_size`).Scan(&pageSize))
	fi, err := os.Stat(s.dbPath)
	require.NoError(t, err)
	t.Logf("graph: files=%d nodes=%d edges=%d db=%.0fMiB page_size=%d", files, s.NodeCount(), s.EdgeCount(), float64(fi.Size())/(1<<20), pageSize)

	// The writer pool holds one connection that is never closed, so a
	// cache_size set on it stays for the case.
	setWriterCachePages := func(pages int64) {
		s.writeMu.Lock()
		defer s.writeMu.Unlock()
		_, err := s.writerDB.ExecContext(ctx, fmt.Sprintf(`PRAGMA cache_size = %d`, -pages*pageSize/1024))
		require.NoError(t, err)
	}
	writerCacheBytes := func() int64 {
		conn, err := s.writerDB.Conn(ctx)
		require.NoError(t, err)
		defer func() { _ = conn.Close() }()
		var used int64
		_ = conn.Raw(func(dc any) error {
			if st, ok := dc.(modernsqlite.DBStatus); ok {
				n, _, _ := st.Status(modernsqlite.DBStatusCacheUsed, false)
				used = int64(n)
			}
			return nil
		})
		return used
	}

	rng := rand.New(rand.NewSource(1))
	rev := 0
	// One build: five files edited (evict, re-add at a new revision), the
	// next build their undo (the previous revision back), as an edit and its
	// undo.
	var editSet []int
	build := func(undo bool) ReaderWaitSplit {
		if !undo {
			editSet = editSet[:0]
			for j := 0; j < editFiles; j++ {
				editSet = append(editSet, rng.Intn(files))
			}
			rev++
		}
		r := rev
		if undo {
			r = rev - 1
		}
		before := s.ReaderWaitMark()
		for _, f := range editSet {
			n, e := fileBatch(f, r)
			s.EvictFile(n[0].FilePath)
			s.AddBatch(n, e)
		}
		return s.ReaderWaitMark().Split(before)
	}

	for _, pages := range []int64{8192, 16384, 65536} {
		setWriterCachePages(pages)
		for k := 0; k < 4; k++ {
			build(k%2 == 1) // fill the cache at this size
		}
		var misses, reads, walReads int64
		var editMisses, undoMisses int64
		var readTime, took time.Duration
		for k := 0; k < builds; k++ {
			start := time.Now()
			d := build(k%2 == 1)
			took += time.Since(start)
			misses += d.WriterCacheMisses
			reads += d.VFS.WriterMainReads
			walReads += d.VFS.WriterWALReads
			readTime += d.VFS.WriterMainReadTime
			if k%2 == 1 {
				undoMisses += d.WriterCacheMisses
			} else {
				editMisses += d.WriterCacheMisses
			}
		}
		used := writerCacheBytes()
		t.Logf("cache_size=%d pages (%.0f MiB of pages): per build misses=%.0f (edit %.0f, its undo %.0f) WriterMainReads=%.0f WriterWALReads=%.0f read_time=%s build=%s; writer cache holds %.1f MiB (%.0f B per configured page; the cache fills lazily)",
			pages, float64(pages*pageSize)/(1<<20), float64(misses)/builds, float64(editMisses)/(builds/2), float64(undoMisses)/(builds/2), float64(reads)/builds, float64(walReads)/builds,
			(readTime / builds).Round(time.Microsecond), (took / builds).Round(time.Millisecond),
			float64(used)/(1<<20), float64(used)/float64(pages))
	}
	setWriterCachePages(8192)
}
