package mcp

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/analysis"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// The whole-store analysis reads the analyze kinds use are bound to the
// request: once the request's context has ended (the MCP deadline abandoned
// the handler), coverage_gaps' kind scan and dead_code's candidate read stop
// instead of reading the whole store; a live request reads everything.
func TestAnalyzeWholeStoreReadsStopWhenTheRequestEnds(t *testing.T) {
	s, err := store_sqlite.Open(filepath.Join(t.TempDir(), "analyze.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	var nodes []*graph.Node
	for i := 0; i < 9000; i++ {
		file := fmt.Sprintf("repo/f%02d.go", i%20)
		nodes = append(nodes, &graph.Node{ID: fmt.Sprintf("%s::F%05d", file, i), Kind: graph.KindFunction, Name: fmt.Sprintf("F%d", i), FilePath: file, RepoPrefix: "repo", Language: "go"})
	}
	require.NoError(t, s.AddBatchChecked(nodes, nil))
	srv := &Server{graph: s}

	live := srv.scopedNodesByKinds(context.Background(), []graph.NodeKind{graph.KindFunction})
	require.Len(t, live, 9000, "a live request reads every page")

	ended, cancel := context.WithCancel(context.Background())
	cancel()
	require.Empty(t, srv.scopedNodesByKinds(ended, []graph.NodeKind{graph.KindFunction}),
		"an ended request must not read the store's pages")

	bound := graph.BindReadContext(srv.readerFor(ended), ended)
	scan, ok := bound.(graph.DeadCodeCandidator)
	require.True(t, ok, "the bound SQLite reader keeps the dead-code capability")
	require.Empty(t, scan.DeadCodeCandidates([]graph.NodeKind{graph.KindFunction}, nil),
		"an ended request must not read dead-code candidates")
	require.Equal(t, analysis.FindDeadCode(s, nil, nil), analysis.FindDeadCode(graph.BindReadContext(s, context.Background()), nil, nil),
		"a live bound reader answers dead_code exactly as the unbound store")
	liveScan := graph.BindReadContext(s, context.Background()).(graph.DeadCodeCandidator)
	require.Len(t, liveScan.DeadCodeCandidates([]graph.NodeKind{graph.KindFunction}, nil), 9000,
		"a live bound reader reads every page of candidates")
}
