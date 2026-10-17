package mcp

import (
	"context"
	"encoding/json"
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

type danglingSurfacerSpy struct {
	graph.Store
	calls int
}

func (s *danglingSurfacerSpy) ThrowerErrorSurface(path string) []graph.ThrowerErrorRow {
	s.calls++
	return s.Store.(graph.ThrowerErrorSurfacer).ThrowerErrorSurface(path)
}

func danglingAnalyzeBody(t *testing.T, s *Server, ctx context.Context, kind string) map[string]any {
	t.Helper()
	req := mcplib.CallToolRequest{}
	req.Params.Arguments = map[string]any{"kind": kind, "format": "json"}
	res, err := s.handleAnalyze(ctx, req)
	require.NoError(t, err)
	require.False(t, res.IsError)
	var body map[string]any
	require.NoError(t, json.Unmarshal([]byte(toolResultText(res)), &body))
	return body
}

func TestAnalyzeDanglingScope_RealGoReferences(t *testing.T) {
	body := `package main
func SenderA(ch chan int) { ch <- 1 }
func SenderB(ch chan int) { ch <- 2 }
func Receiver(ch chan int) { _ = <-ch }
func ErrorFunction() error { return nil }
`
	srv, paths := newAnalyzeServer(t, true, analyzeRepoSpec{name: "repo-a", workspace: "a", body: body}, analyzeRepoSpec{name: "repo-b", workspace: "b", body: body})
	ctx := sessionCtx("dangling-a", paths["repo-a"])
	channels := danglingAnalyzeBody(t, srv, ctx, "channel_ops")["channels"].([]any)
	require.Len(t, channels, 1)
	row := channels[0].(map[string]any)
	require.Equal(t, "unresolved::ch", row["channel"])
	require.Equal(t, float64(2), row["sends"])
	require.Equal(t, float64(1), row["recvs"])
	require.Len(t, row["senders"].([]any), 2)
	unclosed := danglingAnalyzeBody(t, srv, ctx, "unclosed_channels")["unclosed_channels"].([]any)
	require.Len(t, unclosed, 1)
	require.Equal(t, float64(2), unclosed[0].(map[string]any)["senders"])
	errors := danglingAnalyzeBody(t, srv, ctx, "error_surface")["throwers"].([]any)
	require.Len(t, errors, 1)
	require.Equal(t, []any{"repo-a/external::error"}, errors[0].(map[string]any)["errors"])
}

func TestAnalyzeDanglingScope_RejectsForeignOwnersSitesTargetsAndClose(t *testing.T) {
	srv, _ := setupTestServer(t)
	g := srv.graph
	for _, repo := range []string{"repo-a", "repo-b"} {
		for _, name := range []string{"A", "B", "C", "ForeignTarget"} {
			g.AddNode(&graph.Node{ID: repo + "/f.go::" + name, RepoPrefix: repo, Kind: graph.KindFunction, Name: name, FilePath: repo + "/f.go"})
		}
	}
	spy := &danglingSurfacerSpy{Store: g}
	srv.graph = spy
	ctx := withRepoAllow(context.Background(), map[string]bool{"repo-a": true})
	for i, name := range []string{"A", "B"} {
		g.AddEdge(&graph.Edge{From: "repo-a/f.go::" + name, To: "unresolved::ch", Kind: graph.EdgeSends, FilePath: "repo-a/f.go", Line: i + 1})
	}
	g.AddEdge(&graph.Edge{From: "repo-a/f.go::C", To: "unresolved::ch", Kind: graph.EdgeRecvs, FilePath: "repo-a/f.go", Line: 3})
	// Same unresolved name in another repo must not alter counts or risk.
	g.AddEdge(&graph.Edge{From: "repo-b/f.go::A", To: "unresolved::ch", Kind: graph.EdgeSends, FilePath: "repo-b/f.go", Line: 4})
	g.AddEdge(&graph.Edge{From: "repo-b/f.go::A", To: "unresolved::close", Kind: graph.EdgeCalls, FilePath: "repo-b/f.go", Line: 5})
	// A visible owner is insufficient when the reference site is foreign.
	g.AddEdge(&graph.Edge{From: "repo-a/f.go::A", To: "unresolved::ch", Kind: graph.EdgeSends, FilePath: "repo-b/f.go", Line: 6})
	g.AddEdge(&graph.Edge{From: "repo-a/f.go::A", To: "repo-b/f.go::ForeignTarget", Kind: graph.EdgeSends, FilePath: "repo-a/f.go", Line: 7})
	for i, to := range []string{"external::error", "unresolved::LocalError", "repo-b/f.go::ForeignTarget", "repo-b/external::foreign_error"} {
		g.AddEdge(&graph.Edge{From: "repo-a/f.go::A", To: to, Kind: graph.EdgeThrows, FilePath: "repo-a/f.go", Line: 10 + i})
	}
	g.AddEdge(&graph.Edge{From: "repo-b/f.go::A", To: "external::secret", Kind: graph.EdgeThrows, FilePath: "repo-b/f.go", Line: 13})
	g.AddEdge(&graph.Edge{From: "repo-a/f.go::A", To: "external::secret", Kind: graph.EdgeThrows, FilePath: "repo-b/f.go", Line: 14})
	g.AddNode(&graph.Node{ID: "repo-a/msg::local", RepoPrefix: "repo-a", Kind: graph.KindString, Name: "local error message", FilePath: "repo-a/f.go", Meta: map[string]any{"context": "error_msg"}})
	g.AddNode(&graph.Node{ID: "repo-b/msg::secret", RepoPrefix: "repo-b", Kind: graph.KindString, Name: "secret error message", FilePath: "repo-b/f.go", Meta: map[string]any{"context": "error_msg"}})
	g.AddEdge(&graph.Edge{From: "repo-a/f.go::A", To: "repo-a/msg::local", Kind: graph.EdgeEmits, FilePath: "repo-a/f.go"})
	g.AddEdge(&graph.Edge{From: "repo-a/f.go::A", To: "repo-b/msg::secret", Kind: graph.EdgeEmits, FilePath: "repo-a/f.go"})
	g.AddEdge(&graph.Edge{From: "repo-a/f.go::B", To: "unresolved::close", Kind: graph.EdgeCalls, FilePath: "repo-b/f.go", Line: 20})
	channels := danglingAnalyzeBody(t, srv, ctx, "channel_ops")["channels"].([]any)
	require.Len(t, channels, 1)
	row := channels[0].(map[string]any)
	require.Equal(t, float64(2), row["sends"])
	require.Equal(t, float64(1), row["recvs"])
	unclosed := danglingAnalyzeBody(t, srv, ctx, "unclosed_channels")["unclosed_channels"].([]any)
	require.Len(t, unclosed, 1)
	require.Equal(t, "high", unclosed[0].(map[string]any)["risk"])
	errors := danglingAnalyzeBody(t, srv, ctx, "error_surface")["throwers"].([]any)
	require.Len(t, errors, 1)
	row = errors[0].(map[string]any)
	require.Equal(t, float64(2), row["throws"])
	require.Equal(t, []any{"external::error", "unresolved::LocalError"}, row["errors"])
	require.Equal(t, []any{"local error message"}, row["error_msgs"])
	require.Equal(t, 0, spy.calls, "scoped analysis must not consume whole-store aggregates")
	_ = danglingAnalyzeBody(t, srv, context.Background(), "error_surface")
	require.Equal(t, 1, spy.calls, "unscoped analysis retains aggregate fast path")
}
