package mcp

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/query"
)

// Embed the actual SQLite backend so the registered request retains its real
// checked-reader capabilities. These counters distinguish the discarded
// symbol-adjacency walk from the file-level importers that read.file returns.
type readFileAttributionStore struct {
	*store_sqlite.Store
	target        string
	subgraphs     atomic.Int64
	bulkOut       atomic.Int64
	bulkIn        atomic.Int64
	fileImporters atomic.Int64
	fileNodes     atomic.Int64
}

func (s *readFileAttributionStore) GetFileSubGraph(path string) ([]*graph.Node, []*graph.Edge) {
	if path == s.target {
		s.subgraphs.Add(1)
	}
	return s.Store.GetFileSubGraph(path)
}

func (s *readFileAttributionStore) GetFileNodes(path string) []*graph.Node {
	if path == s.target {
		s.fileNodes.Add(1)
	}
	return s.Store.GetFileNodes(path)
}

func (s *readFileAttributionStore) GetOutEdgesByNodeIDs(ids []string) map[string][]*graph.Edge {
	s.bulkOut.Add(1)
	return s.Store.GetOutEdgesByNodeIDs(ids)
}

func (s *readFileAttributionStore) GetInEdgesByNodeIDs(ids []string) map[string][]*graph.Edge {
	s.bulkIn.Add(1)
	return s.Store.GetInEdgesByNodeIDs(ids)
}

func (s *readFileAttributionStore) GetInEdges(id string) []*graph.Edge {
	if id == s.target {
		s.fileImporters.Add(1)
	}
	return s.Store.GetInEdges(id)
}

func TestRegisteredReadFileDoesNotLoadUnusedSymbolAdjacency(t *testing.T) {
	for _, facade := range []bool{false, true} {
		name := "legacy"
		if facade {
			name = "facade"
		}
		t.Run(name, func(t *testing.T) {
			selectedProjectionRegistry(t)
			t.Setenv("XDG_CACHE_HOME", t.TempDir())
			root := t.TempDir()
			var source strings.Builder
			source.WriteString("package probe\n\n")
			for i := range 16 {
				fmt.Fprintf(&source, "func Target%02d() int { return helper0() + helper1() + helper2() + helper3() }\n", i)
			}
			for i := range 4 {
				fmt.Fprintf(&source, "func helper%d() int { return %d }\n", i, i)
			}
			content := source.String()
			// Keep token accounting below its disk-cache threshold; this fixture
			// measures graph work, not process-global token-cache setup.
			require.Less(t, len(content), 2048)
			require.NoError(t, os.WriteFile(filepath.Join(root, "main.go"), []byte(content), 0600))
			require.NoError(t, os.WriteFile(filepath.Join(root, "caller.go"), []byte("package probe\n\nfunc Consumer() int { return Target00() }\n"), 0600))
			sql, err := store_sqlite.Open(filepath.Join(t.TempDir(), "read-file.sqlite"))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, sql.Close()) })
			idx := newProjectionTestIndexer(t, sql)
			_, err = idx.Index(root)
			require.NoError(t, err)
			// A controlled file-import relation makes the separately required
			// dependents response nonempty. Real extraction supplies the dense
			// per-symbol call edges that the attribution path must not fetch.
			require.NoError(t, sql.AddBatchChecked(nil, []*graph.Edge{{From: "caller.go", To: "main.go", Kind: graph.EdgeImports}}))
			nodes, adjacent := sql.GetFileSubGraph("main.go")
			require.GreaterOrEqual(t, len(nodes), 21)
			require.GreaterOrEqual(t, len(adjacent), 64)
			backend := &readFileAttributionStore{Store: sql, target: "main.go"}
			s := NewServer(query.NewEngine(backend), backend, idx, nil, zap.NewNop(), nil)
			s.frecency = &frecencyTracker{mode: ModeAI, now: func() int64 { return 1000 }}
			const session = "read-file-node-only"
			require.NoError(t, s.ConnectSession(session, func([]byte) error { return nil }))
			t.Cleanup(func() { s.DisconnectSession(session) })
			s.NoteSessionToolPolicy(session, "all", "defer")
			if facade {
				s.NoteSessionToolPolicy(session, FacadeSurfaceVersion, "hide")
			}
			initializeSession(t, s, session)
			ctx := s.WithClientSession(WithSessionID(t.Context(), session), session)
			tool := "read_file"
			args := map[string]any{"path": "main.go", "format": "json", "max_bytes": 0}
			if facade {
				tool = "read"
				args = map[string]any{
					"operation": "file",
					"target":    map[string]any{"file": "main.go"},
					"output":    map[string]any{"format": "json", "max_bytes": 0},
				}
			}
			// This generic helper uses the real JSON-RPC HandleMessage path,
			// including registered middleware and facade lowering; it does not
			// call handleReadFile directly despite its historical helper name.
			result := callEditOperationFrame(t, s, ctx, tool, args)
			out := extractTextResult(t, result)
			require.Equal(t, content, out["content"])
			require.Equal(t, "main.go", out["path"])
			require.Equal(t, "go", out["language"])
			require.Equal(t, float64(len(content)), out["bytes"])
			require.Equal(t, float64(len(content)), out["original_bytes"])
			require.NotContains(t, out, "content_truncated")
			require.Equal(t, computeETag(map[string]any{
				"path": "main.go", "language": "go", "bytes": len(content),
				"original_bytes": len(content), "content": content,
			}), out["etag"])
			require.Equal(t, []any{"caller.go"}, out["dependents"])
			require.Contains(t, out["dependents_header"], "caller.go")
			for _, n := range nodes {
				if n != nil && n.Kind != graph.KindFile {
					require.Equal(t, 1.0, s.frecency.Score(n.ID), n.ID)
				}
			}
			for _, n := range sql.GetFileNodes("caller.go") {
				if n != nil && n.Kind != graph.KindFile {
					require.Zero(t, s.frecency.Score(n.ID), n.ID)
				}
			}
			t.Logf("actual registered %s exact content/language/etag/dependents/frecency passed; subgraphs=%d bulk_out=%d bulk_in=%d allowed_file_importers=%d node_only=%d",
				tool, backend.subgraphs.Load(), backend.bulkOut.Load(), backend.bulkIn.Load(), backend.fileImporters.Load(), backend.fileNodes.Load())
			require.Positive(t, backend.fileImporters.Load(), "the response's file-level dependents lookup remains required")
			require.Zero(t, backend.subgraphs.Load()+backend.bulkOut.Load()+backend.bulkIn.Load(), "read.file only consumes nodes: full symbol adjacency is discarded")
			require.Positive(t, backend.fileNodes.Load(), "read through the selected real SQLite node accessor")
		})
	}
}
