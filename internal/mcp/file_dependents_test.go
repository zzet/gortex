package mcp

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/query"
)

func depGraph() *graph.Graph {
	g := graph.New()
	for _, id := range []string{"a.go", "b.go", "c.go"} {
		g.AddNode(&graph.Node{ID: id, Kind: graph.KindFile, Name: id, FilePath: id})
	}
	// b.go imports a.go.
	g.AddEdge(&graph.Edge{From: "b.go", To: "a.go", Kind: graph.EdgeImports})
	return g
}

func TestFileDependentsHeader(t *testing.T) {
	s := &Server{graph: depGraph()}
	deps := s.fileDependents(context.Background(), "a.go")
	require.Equal(t, []string{"b.go"}, deps)

	header := fileDependentsNote(deps)
	if !strings.Contains(header, "1 file imports this file") || !strings.Contains(header, "b.go") {
		t.Errorf("header = %q", header)
	}
	// No importers → empty header.
	require.Empty(t, s.fileDependents(context.Background(), "c.go"))
	require.Equal(t, "", fileDependentsNote(nil))
}

func TestFileDependentsHeaderImportsOnly(t *testing.T) {
	g := depGraph()
	// c.go references a.go via a non-import edge — must NOT count as a dependent.
	g.AddEdge(&graph.Edge{From: "c.go", To: "a.go", Kind: graph.EdgeReferences})
	s := &Server{graph: g}
	deps := s.fileDependents(context.Background(), "a.go")
	require.Equal(t, []string{"b.go"}, deps, "only import edges should count")
}

func TestGetFileSummaryDependentsHeader(t *testing.T) {
	s := newTestServer(t) // pkg/foo.go::Bar, ::Baz (FilePath pkg/foo.go)
	s.engine = query.NewEngine(s.graph)
	// A file node + an importer of pkg/foo.go.
	s.graph.AddNode(&graph.Node{ID: "pkg/foo.go", Kind: graph.KindFile, Name: "pkg/foo.go", FilePath: "pkg/foo.go"})
	s.graph.AddNode(&graph.Node{ID: "pkg/user.go", Kind: graph.KindFile, Name: "pkg/user.go", FilePath: "pkg/user.go"})
	s.graph.AddEdge(&graph.Edge{From: "pkg/user.go", To: "pkg/foo.go", Kind: graph.EdgeImports})

	res := callHandler(t, s.handleGetFileSummary, map[string]any{"path": "pkg/foo.go"})
	m := unmarshalResult(t, res)
	deps, ok := m["dependents"].([]any)
	require.True(t, ok, "get_file_summary should emit dependents, got %T", m["dependents"])
	require.Len(t, deps, 1)
	require.Equal(t, "pkg/user.go", deps[0])
	require.Contains(t, m["dependents_header"], "pkg/user.go")
}

func TestReadFileEmitsDependents(t *testing.T) {
	srv, _ := setupTestServer(t) // indexes a temp repo containing main.go
	// Inject a file that imports main.go.
	srv.graph.AddNode(&graph.Node{ID: "main.go", Kind: graph.KindFile, Name: "main.go", FilePath: "main.go"})
	srv.graph.AddNode(&graph.Node{ID: "consumer.go", Kind: graph.KindFile, Name: "consumer.go", FilePath: "consumer.go"})
	srv.graph.AddEdge(&graph.Edge{From: "consumer.go", To: "main.go", Kind: graph.EdgeImports})

	result := callTool(t, srv, "read_file", map[string]any{"path": "main.go"})
	require.False(t, result.IsError, "read_file errored: %+v", result.Content)
	resp := decodeFileOpsResult(t, result)
	deps, ok := resp["dependents"].([]any)
	require.True(t, ok, "read_file should emit dependents, got %T (%+v)", resp["dependents"], resp)
	require.Contains(t, deps, "consumer.go")
	require.Contains(t, resp["dependents_header"], "consumer.go")
}
