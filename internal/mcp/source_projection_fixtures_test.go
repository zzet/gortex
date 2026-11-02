package mcp

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"github.com/zzet/gortex/internal/pathkey"
)

type projectionFixture struct {
	t   *testing.T
	dir string
	g   *graph.Graph
	idx *indexer.Indexer
	seq uint64
}

// newProjectionFixture indexes a one-file Go repository into an in-memory
// store, the state before a tool edit.
func newProjectionFixture(t *testing.T) *projectionFixture {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.go"), []byte("package p\n\nfunc Old() {}\n"), 0o644))
	g := graph.New()
	idx := newProjectionTestIndexer(t, g)
	_, err := idx.Index(dir)
	require.NoError(t, err)
	return &projectionFixture{t: t, dir: dir, g: g, idx: idx}
}

func newProjectionTestIndexer(t testing.TB, g graph.Store) *indexer.Indexer {
	t.Helper()
	registry := parser.NewRegistry()
	languages.RegisterAll(registry)
	cfg := config.Default().Index
	cfg.Workers = 1
	idx := indexer.New(g, registry, cfg, zap.NewNop())
	t.Cleanup(idx.Close)
	return idx
}

// edit writes content to rel as a tool would and projects it.
func (f *projectionFixture) edit(rel, content string) *projectionEntry {
	f.t.Helper()
	path := filepath.Join(f.dir, rel)
	require.NoError(f.t, os.WriteFile(path, []byte(content), 0o644))
	stamp, err := statDiskStamp(path)
	require.NoError(f.t, err)
	f.seq++
	e, err := projectFile(context.Background(), f.idx, path, []byte(content), f.seq, stamp)
	require.NoError(f.t, err)
	return e
}

func liveEntry(p *pendingProjections, path string) *projectionEntry {
	return p.snap.Load().entries[pathkey.MutationKey(path)]
}

type projectionRetirementSpy struct {
	*graph.Graph
	metadataReads int
	nodeReads     int
}

func (s *projectionRetirementSpy) FileMetasByPaths(prefix string, paths []string) (map[string]graph.FileMetaRow, error) {
	s.metadataReads++
	return s.Graph.FileMetasByPaths(prefix, paths)
}

func (s *projectionRetirementSpy) GetFileNodes(path string) []*graph.Node {
	s.nodeReads++
	return s.Graph.GetFileNodes(path)
}

func syntheticProjectionEntry(path, graphPath, hash string, seq uint64, stamp diskStamp, raw []byte) *projectionEntry {
	node := &graph.Node{ID: graphPath, Kind: graph.KindFile, FilePath: graphPath, StartLine: 1, EndLine: 1}
	projection := &indexer.Projection{GraphPath: graphPath, Language: "go", ContentHash: hash, Nodes: []*graph.Node{node}}
	return newProjectionEntry(path, "", projection, raw, seq, stamp)
}
