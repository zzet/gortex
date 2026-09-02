package indexer

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

type incrementalBatchCountingStore struct {
	graph.Store

	getFileNodes         atomic.Int64
	getFileNodesByPaths  atomic.Int64
	getNode              atomic.Int64
	getNodesByIDs        atomic.Int64
	getInEdges           atomic.Int64
	getInEdgesByNodeIDs  atomic.Int64
	getOutEdges          atomic.Int64
	getOutEdgesByNodeIDs atomic.Int64
	addNode              atomic.Int64
	addEdge              atomic.Int64
	addBatch             atomic.Int64
	reindexEdge          atomic.Int64
	reindexEdges         atomic.Int64
	evictFile            atomic.Int64
	evictFiles           atomic.Int64
	removeEdge           atomic.Int64
}

func (s *incrementalBatchCountingStore) GetFileNodes(path string) []*graph.Node {
	s.getFileNodes.Add(1)
	return s.Store.GetFileNodes(path)
}

func (s *incrementalBatchCountingStore) GetFileNodesByPaths(paths []string) map[string][]*graph.Node {
	s.getFileNodesByPaths.Add(1)
	return s.Store.GetFileNodesByPaths(paths)
}

func (s *incrementalBatchCountingStore) GetNode(id string) *graph.Node {
	s.getNode.Add(1)
	return s.Store.GetNode(id)
}

func (s *incrementalBatchCountingStore) GetNodesByIDs(ids []string) map[string]*graph.Node {
	s.getNodesByIDs.Add(1)
	return s.Store.GetNodesByIDs(ids)
}

func (s *incrementalBatchCountingStore) GetInEdges(id string) []*graph.Edge {
	s.getInEdges.Add(1)
	return s.Store.GetInEdges(id)
}

func (s *incrementalBatchCountingStore) GetInEdgesByNodeIDs(ids []string) map[string][]*graph.Edge {
	s.getInEdgesByNodeIDs.Add(1)
	return s.Store.GetInEdgesByNodeIDs(ids)
}

func (s *incrementalBatchCountingStore) GetOutEdges(id string) []*graph.Edge {
	s.getOutEdges.Add(1)
	return s.Store.GetOutEdges(id)
}

func (s *incrementalBatchCountingStore) GetOutEdgesByNodeIDs(ids []string) map[string][]*graph.Edge {
	s.getOutEdgesByNodeIDs.Add(1)
	return s.Store.GetOutEdgesByNodeIDs(ids)
}

func (s *incrementalBatchCountingStore) AddNode(node *graph.Node) {
	s.addNode.Add(1)
	s.Store.AddNode(node)
}

func (s *incrementalBatchCountingStore) AddEdge(edge *graph.Edge) {
	s.addEdge.Add(1)
	s.Store.AddEdge(edge)
}

func (s *incrementalBatchCountingStore) AddBatch(nodes []*graph.Node, edges []*graph.Edge) {
	s.addBatch.Add(1)
	s.Store.AddBatch(nodes, edges)
}

func (s *incrementalBatchCountingStore) ReindexEdge(edge *graph.Edge, oldTo string) {
	s.reindexEdge.Add(1)
	s.Store.ReindexEdge(edge, oldTo)
}

func (s *incrementalBatchCountingStore) ReindexEdges(batch []graph.EdgeReindex) {
	s.reindexEdges.Add(1)
	s.Store.ReindexEdges(batch)
}

func (s *incrementalBatchCountingStore) RemoveEdge(from, to string, kind graph.EdgeKind) bool {
	s.removeEdge.Add(1)
	return s.Store.RemoveEdge(from, to, kind)
}

func (s *incrementalBatchCountingStore) EvictFile(path string) (int, int) {
	s.evictFile.Add(1)
	return s.Store.EvictFile(path)
}

func (s *incrementalBatchCountingStore) EvictFiles(paths []string) (int, int) {
	s.evictFiles.Add(1)
	if batch, ok := s.Store.(graph.FileBatchEvicter); ok {
		return batch.EvictFiles(paths)
	}
	nodes, edges := 0, 0
	for _, path := range paths {
		n, e := s.Store.EvictFile(path)
		nodes += n
		edges += e
	}
	return nodes, edges
}

type incrementalBatchCounts struct {
	batchReads  int64
	pointReads  int64
	batchWrites int64
	pointWrites int64
	evictFiles  int64
	evictFile   int64
}

func (s *incrementalBatchCountingStore) resetCounts() {
	for _, counter := range []*atomic.Int64{
		&s.getFileNodes, &s.getFileNodesByPaths, &s.getNode, &s.getNodesByIDs,
		&s.getInEdges, &s.getInEdgesByNodeIDs, &s.getOutEdges, &s.getOutEdgesByNodeIDs,
		&s.addNode, &s.addEdge, &s.addBatch, &s.reindexEdge, &s.reindexEdges,
		&s.evictFile, &s.evictFiles, &s.removeEdge,
	} {
		counter.Store(0)
	}
}

func (s *incrementalBatchCountingStore) counts() incrementalBatchCounts {
	return incrementalBatchCounts{
		batchReads: s.getFileNodesByPaths.Load() + s.getNodesByIDs.Load() +
			s.getInEdgesByNodeIDs.Load() + s.getOutEdgesByNodeIDs.Load(),
		pointReads: s.getFileNodes.Load() + s.getNode.Load() +
			s.getInEdges.Load() + s.getOutEdges.Load(),
		batchWrites: s.addBatch.Load() + s.reindexEdges.Load() + s.evictFiles.Load(),
		pointWrites: s.addNode.Load() + s.addEdge.Load() + s.reindexEdge.Load() +
			s.evictFile.Load() + s.removeEdge.Load(),
		evictFiles: s.evictFiles.Load(),
		evictFile:  s.evictFile.Load(),
	}
}

func runIncrementalBatchScale(t *testing.T, fileCount int) incrementalBatchCounts {
	t.Helper()
	dir := t.TempDir()
	paths := make([]string, 0, fileCount)
	for i := 0; i < fileCount; i++ {
		path := filepath.Join(dir, fmt.Sprintf("f%03d.go", i))
		writeFile(t, path, fmt.Sprintf("package batch\n\nfunc F%03d() int { return %d }\n", i, i))
		paths = append(paths, path)
	}

	base := graph.New()
	counting := &incrementalBatchCountingStore{Store: base}
	idx := newTestIndexer(counting)
	_, err := idx.Index(dir)
	require.NoError(t, err)
	idx.deferGlobalPasses.Store(true)
	counting.resetCounts()

	future := time.Now().Add(3 * time.Second)
	for i, path := range paths {
		content := fmt.Sprintf(
			"package batch\n\nfunc F%03d() int { return %d }\nfunc Added%03d() {}\n",
			i, i+1, i,
		)
		require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
		require.NoError(t, os.Chtimes(path, future, future))
	}

	result, err := idx.IncrementalReindexPaths(dir, paths)
	require.NoError(t, err)
	require.Equal(t, fileCount, result.StaleFileCount)
	require.Empty(t, result.FailedFiles)
	return counting.counts()
}

func TestIncrementalMultiFileBatchQueriesAndCommitsScaleByChunks(t *testing.T) {
	small := runIncrementalBatchScale(t, 1)
	medium := runIncrementalBatchScale(t, 100)
	large := runIncrementalBatchScale(t, 1000)

	for count, got := range map[int]incrementalBatchCounts{
		1: small, 100: medium, 1000: large,
	} {
		require.Zero(t, got.pointReads, "%d-file batch used point queries: %+v", count, got)
		require.Zero(t, got.pointWrites, "%d-file batch used point commits: %+v", count, got)
		require.Zero(t, got.evictFile, "%d-file batch used point eviction: %+v", count, got)
		chunks := int64((count + incrementalBatchFiles - 1) / incrementalBatchFiles)
		require.Equal(t, chunks, got.evictFiles,
			"%d files should be evicted in ceil(n/%d) bounded commits", count, incrementalBatchFiles)
		// Data chunks plus one fixed post-pass allowance. A per-file loop
		// would grow by n; the bounded pipeline must grow only by chunk count.
		maxGrowth := chunks + 1
		require.LessOrEqual(t, got.batchReads, small.batchReads*maxGrowth,
			"batch queries grew faster than chunk count: 1=%+v %d=%+v", small, count, got)
		require.LessOrEqual(t, got.batchWrites, small.batchWrites*maxGrowth,
			"batch commits grew faster than chunk count: 1=%+v %d=%+v", small, count, got)
	}
}

func boundedGraphProjection(g graph.Store) string {
	nodes := g.GetRepoNodes("")
	ids := make([]string, 0, len(nodes))
	lines := make([]string, 0, len(nodes)*2)
	for _, node := range nodes {
		if node == nil {
			continue
		}
		ids = append(ids, node.ID)
		lines = append(lines, fmt.Sprintf("N|%s|%s|%s|%s|%d|%d|%s",
			node.ID, node.Kind, node.Name, node.FilePath,
			node.StartLine, node.EndLine, node.Language))
	}
	out := g.GetOutEdgesByNodeIDs(ids)
	for _, id := range ids {
		for _, edge := range out[id] {
			if edge != nil {
				lines = append(lines, fmt.Sprintf("E|%s|%s|%s|%d", edge.From, edge.To, edge.Kind, edge.Line))
			}
		}
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}

func TestIncrementalMultiFileBatchMatchesSingleFileAPI(t *testing.T) {
	build := func(dir string) []string {
		files := map[string]string{
			"a.go": "package parity\n\nfunc A() { B() }\n",
			"b.go": "package parity\n\nfunc B() {}\n",
			"c.go": "package parity\n\ntype C struct{ Value int }\n",
			"d.go": "package parity\n\nfunc D(c C) int { return c.Value }\n",
		}
		paths := make([]string, 0, len(files))
		for name, content := range files {
			path := filepath.Join(dir, name)
			writeFile(t, path, content)
			paths = append(paths, path)
		}
		sort.Strings(paths)
		return paths
	}
	mutate := func(paths []string) {
		content := []string{
			"package parity\n\nfunc A() { B(); AddedA() }\nfunc AddedA() {}\n",
			"package parity\n\nfunc B() int { return 1 }\n",
			"package parity\n\ntype C struct{ Value int; Name string }\n",
			"package parity\n\nfunc D(c C) int { return c.Value + B() }\n",
		}
		future := time.Now().Add(3 * time.Second)
		for i, path := range paths {
			require.NoError(t, os.WriteFile(path, []byte(content[i]), 0o644))
			require.NoError(t, os.Chtimes(path, future, future))
		}
	}

	dirBatch := t.TempDir()
	batchPaths := build(dirBatch)
	batchGraph := graph.New()
	batchIndexer := newTestIndexer(batchGraph)
	_, err := batchIndexer.Index(dirBatch)
	require.NoError(t, err)
	mutate(batchPaths)
	result, err := batchIndexer.IncrementalReindexPaths(dirBatch, batchPaths)
	require.NoError(t, err)
	require.Empty(t, result.FailedFiles)

	dirSingle := t.TempDir()
	singlePaths := build(dirSingle)
	singleGraph := graph.New()
	singleIndexer := newTestIndexer(singleGraph)
	_, err = singleIndexer.Index(dirSingle)
	require.NoError(t, err)
	mutate(singlePaths)
	for _, path := range singlePaths {
		require.NoError(t, singleIndexer.IndexFile(path))
	}

	require.Equal(t, boundedGraphProjection(singleGraph), boundedGraphProjection(batchGraph))
}

func TestIncrementalMultiFileBatchKeepsFailedFileAndCommitsSiblings(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("an unreadable-file test is meaningless as root")
	}
	dir := t.TempDir()
	good := filepath.Join(dir, "good.go")
	bad := filepath.Join(dir, "bad.go")
	writeFile(t, good, "package isolation\n\nfunc Good() {}\n")
	writeFile(t, bad, "package isolation\n\nfunc Bad() {}\n")

	g := graph.New()
	idx := newTestIndexer(g)
	_, err := idx.Index(dir)
	require.NoError(t, err)
	oldBadMtime := idx.FileMtimes()[idx.relKey(bad)]

	bumpMtime(t, good, "package isolation\n\nfunc Good() {}\nfunc GoodCommitted() {}\n")
	require.NoError(t, os.Chmod(bad, 0o000))
	t.Cleanup(func() { _ = os.Chmod(bad, 0o644) })
	future := time.Now().Add(4 * time.Second)
	require.NoError(t, os.Chtimes(bad, future, future))

	result, err := idx.IncrementalReindexPaths(dir, []string{good, bad})
	require.NoError(t, err)
	require.Contains(t, result.FailedFiles, bad)
	require.NotEmpty(t, g.FindNodesByName("GoodCommitted"),
		"a sibling parse must commit even when another file fails")
	require.NotEmpty(t, g.FindNodesByName("Bad"),
		"failed parse must retain the prior graph state")
	require.Equal(t, oldBadMtime, idx.FileMtimes()[idx.relKey(bad)],
		"failed file must not advance its durable retry watermark")

	require.NoError(t, os.Chmod(bad, 0o644))
	recovered, err := idx.IncrementalReindexPaths(dir, []string{bad})
	require.NoError(t, err)
	require.Empty(t, recovered.FailedFiles)
	require.Greater(t, idx.FileMtimes()[idx.relKey(bad)], oldBadMtime)
}

var _ graph.FileBatchEvicter = (*incrementalBatchCountingStore)(nil)

func TestIncrementalReindexKeepsMutationReceiptExact(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{
		"a.go": "package receipt\n\nfunc A() { B() }\n",
		"b.go": "package receipt\n\nfunc B() {}\n",
	}
	paths := make([]string, 0, len(files))
	for name, content := range files {
		path := filepath.Join(dir, name)
		writeFile(t, path, content)
		paths = append(paths, path)
	}
	sort.Strings(paths)

	g := graph.New()
	idx := newTestIndexer(g)
	_, err := idx.Index(dir)
	require.NoError(t, err)

	future := time.Now().Add(3 * time.Second)
	for _, path := range paths {
		content := "package receipt\n\nfunc A() { B(); Added() }\nfunc Added() {}\n"
		if filepath.Base(path) == "b.go" {
			content = "package receipt\n\nfunc B() int { return 1 }\n"
		}
		require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
		require.NoError(t, os.Chtimes(path, future, future))
	}

	result, receipt, _, err := idx.incrementalReindexPathsWithReceiptMode(dir, paths, incrementalPathMode{})
	require.NoError(t, err)
	require.Empty(t, result.FailedFiles)
	require.NotNil(t, receipt)
	require.Truef(t, receipt.Complete,
		"structural incremental reindex voided the mutation receipt (%s) — forces the whole-graph fallback resolve", receipt.IncompleteReason)
	require.True(t, receipt.ResolutionRelevant)
	for _, name := range []string{"a.go", "b.go"} {
		require.Contains(t, receipt.ResolutionFiles(), name)
	}
}

// The production shape of the receipt-exact eviction fix, on the production
// backend: ONE file edited while an UNCHANGED file holds a resolved incoming
// reference to it. The pre-evict restub reindexes that reference, the evict
// describes its doomed nodes, the re-add records the successors - only the
// SQLite backend keeps all three receipt-exact (the in-memory Graph's
// reindexEdge still fails receipts closed, a documented asymmetry), so this
// is the composition the perf claim actually rides on.
func TestSQLiteIncrementalSingleFileEditKeepsMutationReceiptExact(t *testing.T) {
	dir := t.TempDir()
	defPath := filepath.Join(dir, "def.go")
	callerPath := filepath.Join(dir, "caller.go")
	writeFile(t, defPath, "package p\n\nfunc Foo() {}\n")
	writeFile(t, callerPath, "package p\n\nfunc Bar() { Foo() }\n")

	g := newSqliteGraph(t)
	idx := newTestIndexer(g)
	_, err := idx.Index(dir)
	require.NoError(t, err)

	future := time.Now().Add(3 * time.Second)
	require.NoError(t, os.WriteFile(defPath, []byte("package p\n\nfunc Foo() int { return 1 }\n"), 0o644))
	require.NoError(t, os.Chtimes(defPath, future, future))

	result, receipt, _, err := idx.incrementalReindexPathsWithReceiptMode(dir, []string{defPath}, incrementalPathMode{})
	require.NoError(t, err)
	require.Empty(t, result.FailedFiles)
	require.NotNil(t, receipt)
	require.Truef(t, receipt.Complete,
		"single-file edit with an external incoming reference voided the receipt (%s) on the SQLite backend", receipt.IncompleteReason)
	require.True(t, receipt.ResolutionRelevant)
	require.Contains(t, receipt.ResolutionFiles(), "def.go")
}
