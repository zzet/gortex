package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/query"
	"github.com/zzet/gortex/internal/search"
)

// noteSearchTestServer ranks every query to the same equally scored hits, so
// the whole-task ranking is flat and the retrieval note fires when it is
// computed at all.
func noteSearchTestServer(t *testing.T) (*Server, *recordingBackend, []*graph.Node) {
	t.Helper()
	g := graph.New()
	nodes := make(map[string]*graph.Node)
	var hits []search.SearchResult
	var pack []*graph.Node
	for i, name := range []string{"parser", "parseConfig", "parseFile", "parserState"} {
		node := &graph.Node{
			ID: fmt.Sprintf("pkg/p%d.go::%s", i, name), Kind: graph.KindFunction, Name: name,
			FilePath: fmt.Sprintf("pkg/p%d.go", i), StartLine: 1, EndLine: 3, Language: "go",
		}
		g.AddNode(node)
		nodes[node.ID] = node
		pack = append(pack, node)
		hits = append(hits, search.SearchResult{ID: node.ID, Score: 1})
	}
	rb := newRecordingBackend(nodes, hits)
	eng := query.NewEngine(g)
	eng.SetSearch(rb)
	srv := NewServer(eng, g, nil, nil, zap.NewNop(), nil)
	srv.RunAnalysis()
	return srv, rb, pack
}

func countQueries(queries []string, want string) int {
	n := 0
	for _, q := range queries {
		if q == want {
			n++
		}
	}
	return n
}

func TestSmartContextSearchesAOneWordTaskOnce(t *testing.T) {
	for _, tc := range []struct {
		task     string
		searched []string
		note     bool
	}{
		// A distinctive identifier never draws a note, so it is searched
		// only as its own keyword.
		{task: "parseConfig", searched: []string{"parseConfig"}},
		// A plain word is its own keyword; the note reuses that ranking.
		{task: "parser", searched: []string{"parser"}, note: true},
		// A sentence still ranks the whole task once for the note.
		{task: "how does parser work", searched: []string{"parser", "work", "how does parser work"}, note: true},
	} {
		t.Run(tc.task, func(t *testing.T) {
			srv, rb, _ := noteSearchTestServer(t)
			req := mcplib.CallToolRequest{}
			req.Params.Name = "smart_context"
			req.Params.Arguments = map[string]any{"task": tc.task}
			res, err := srv.handleSmartContext(context.Background(), req)
			require.NoError(t, err)
			require.False(t, res.IsError, "smart_context errored: %v", res.Content)
			queries := rb.queries()
			require.Len(t, queries, len(tc.searched), "queries=%v", queries)
			for _, q := range tc.searched {
				require.Equal(t, 1, countQueries(queries, q), "query %q ran %d times; queries=%v", q, countQueries(queries, q), queries)
			}
			var resp map[string]any
			require.NoError(t, json.Unmarshal([]byte(res.Content[0].(mcplib.TextContent).Text), &resp))
			_, hasNote := resp["retrieval_note"]
			require.Equal(t, tc.note, hasNote, "retrieval_note = %v", resp["retrieval_note"])
		})
	}
}

func TestRetrievalNoteFromAKeywordRankingEqualsItsOwnSearch(t *testing.T) {
	srv, rb, pack := noteSearchTestServer(t)
	ctx := context.Background()
	ranked := srv.engineFor(ctx).SearchSymbolsRankedContext(ctx, "parser", 10, query.QueryOptions{}, nil)
	before := len(rb.queries())
	reused := srv.lowConfidenceRetrievalNote(ctx, "parser", pack, ranked, true, query.QueryOptions{})
	require.Len(t, rb.queries(), before, "a known task ranking must not search again")
	searched := srv.lowConfidenceRetrievalNote(ctx, "parser", pack, nil, false, query.QueryOptions{})
	require.Len(t, rb.queries(), before+1)
	require.NotNil(t, searched, "a flat ranking must draw the note")
	require.Equal(t, searched, reused)

	// A distinctive identifier draws no note and reaches no search.
	before = len(rb.queries())
	require.Nil(t, srv.lowConfidenceRetrievalNote(ctx, "parseConfig", pack, nil, false, query.QueryOptions{}))
	require.Len(t, rb.queries(), before)
}
