package mcp

import (
	"context"
	"fmt"
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/query"
	"github.com/zzet/gortex/internal/respbudget"
	"github.com/zzet/gortex/internal/search"
)

// symbolFullBudgetServer builds a graph whose seed symbol carries
// enough direct edges that a full-detail response dwarfs a small
// budget. The payload shape matters: typed []*graph.Edge slices
// inside a map[string]any, which the trimmer only sees after JSON
// normalization.
func symbolFullBudgetServer(t *testing.T) (*Server, string) {
	t.Helper()
	g := graph.New()
	seed := &graph.Node{ID: "pkg/svc.go::Handler", Kind: graph.KindFunction, Name: "Handler", FilePath: "pkg/svc.go", StartLine: 1}
	g.AddNode(seed)
	for i := 0; i < 40; i++ {
		file := fmt.Sprintf("pkg/peer%02d.go", i)
		peer := &graph.Node{ID: fmt.Sprintf("%s::Peer%02d", file, i), Kind: graph.KindFunction, Name: fmt.Sprintf("Peer%02d", i), FilePath: file, StartLine: 3}
		g.AddNode(peer)
		g.AddEdge(&graph.Edge{From: peer.ID, To: seed.ID, Kind: graph.EdgeCalls, FilePath: file, Line: 5, Origin: graph.OriginASTResolved, Confidence: 0.9})
		g.AddEdge(&graph.Edge{From: seed.ID, To: peer.ID, Kind: graph.EdgeReferences, FilePath: "pkg/svc.go", Line: 7 + i, Origin: graph.OriginASTResolved, Confidence: 0.9})
	}
	eng := query.NewEngine(g)
	eng.SetSearch(search.NewNull())
	return NewServer(eng, g, nil, nil, zap.NewNop(), nil), seed.ID
}

func getSymbolText(t *testing.T, srv *Server, args map[string]any) string {
	t.Helper()
	req := mcplib.CallToolRequest{}
	req.Params.Name = "get_symbol"
	req.Params.Arguments = args
	res, err := srv.handleGetSymbol(context.Background(), req)
	require.NoError(t, err)
	require.False(t, res.IsError)
	return res.Content[0].(mcplib.TextContent).Text
}

// TestGetSymbolFullHonorsBudget pins the byte/token budget on the
// get_symbol detail:"full" response — the payload whose edge lists are
// typed slices, so the trim must normalize before it can cut.
func TestGetSymbolFullHonorsBudget(t *testing.T) {
	srv, seedID := symbolFullBudgetServer(t)

	full := getSymbolText(t, srv, map[string]any{"id": seedID, "detail": "full"})
	require.Greater(t, len(full), 600, "fixture must overflow the test cap unbudgeted")

	cases := []struct {
		name string
		args map[string]any
		cap  int
	}{
		{"json_max_bytes", map[string]any{"max_bytes": 600}, 600},
		{"json_max_tokens", map[string]any{"max_tokens": 200}, tokensToBytes(200)},
		{"toon_max_bytes", map[string]any{"format": "toon", "max_bytes": 600}, 600},
		{"toon_max_tokens", map[string]any{"format": "toon", "max_tokens": 200}, tokensToBytes(200)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			args := map[string]any{"id": seedID, "detail": "full"}
			for k, v := range tc.args {
				args[k] = v
			}
			out := getSymbolText(t, srv, args)
			require.LessOrEqual(t, len(out), tc.cap,
				"get_symbol full must honor the cap %d, got %d bytes", tc.cap, len(out))
			require.NotEmpty(t, out)
			require.Contains(t, out, respbudget.TruncatedKey,
				"a trimmed response carries the truncation marker in both JSON and TOON")
		})
	}
}
