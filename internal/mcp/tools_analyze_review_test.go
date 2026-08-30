package mcp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

// callAnalyzeReview runs analyze kind=review and decodes the JSON
// payload. A tool-level error fails the test.
func callAnalyzeReview(t *testing.T, srv *Server, extra map[string]any) map[string]any {
	t.Helper()
	args := map[string]any{"kind": "review"}
	for k, v := range extra {
		args[k] = v
	}
	req := mcplib.CallToolRequest{}
	req.Params.Name = "analyze"
	req.Params.Arguments = args
	res, err := srv.handleAnalyze(context.Background(), req)
	require.NoError(t, err)
	require.False(t, res.IsError, "analyze review must not error: %+v", res.Content)
	text := res.Content[0].(mcplib.TextContent).Text
	var out map[string]any
	require.NoError(t, json.Unmarshal([]byte(text), &out), "json: %s", text)
	return out
}

// writeReviewFixture drops a fixture file under the indexed test repository
// and adds a KindFile node so production path confinement can resolve it.
// fnNodes lets the caller register enclosing function nodes (with optional
// loop_depth metadata) for graph-grounded post-processing.
func writeReviewFixture(t *testing.T, srv *Server, name, lang, src string) string {
	t.Helper()
	dir := srv.indexer.RootPath()
	require.NotEmpty(t, dir, "test server has no indexed repository root")
	abs := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(abs, []byte(src), 0o644))
	srv.graph.AddNode(&graph.Node{
		ID: abs, Kind: graph.KindFile, Name: abs,
		FilePath: abs, Language: lang, StartLine: 1, EndLine: 500,
	})
	return abs
}

func addFnNode(srv *Server, file, name string, start, end int, meta map[string]any) string {
	id := file + "::" + name
	srv.graph.AddNode(&graph.Node{
		ID: id, Kind: graph.KindFunction, Name: name,
		FilePath: file, Language: "go", StartLine: start, EndLine: end, Meta: meta,
	})
	return id
}

// TestAnalyzeDispatcher_RoutesReview regression-protects the switch
// wiring: kind=review must route without an unknown-kind error.
func TestAnalyzeDispatcher_RoutesReview(t *testing.T) {
	srv, _ := setupTestServer(t)
	req := mcplib.CallToolRequest{}
	req.Params.Name = "analyze"
	req.Params.Arguments = map[string]any{"kind": "review"}
	res, err := srv.handleAnalyze(context.Background(), req)
	require.NoError(t, err)
	require.False(t, res.IsError,
		"dispatcher must route kind=review without error; got %v", res)
}

// TestAnalyzeReview_ReturnsFindings asserts a decidable Go review rule
// fires through the MCP path and carries Category "review".
func TestAnalyzeReview_ReturnsFindings(t *testing.T) {
	srv, _ := setupTestServer(t)
	writeReviewFixture(t, srv, "lib.go", "go", `package x

func F() error {
	err := do()
	if err == nil {
		return err
	}
	return nil
}
`)

	out := callAnalyzeReview(t, srv, nil)
	total, _ := out["total"].(float64)
	require.GreaterOrEqual(t, total, float64(1),
		"expected the inverted-err-check review rule to fire; got %v\n%v", total, out)

	matches := out["matches"].([]any)
	found := false
	for _, m := range matches {
		row := m.(map[string]any)
		if row["detector"] == "go-inverted-err-check" {
			found = true
			assert.Equal(t, "review", row["category"])
			assert.Equal(t, "error", row["severity"])
		}
	}
	assert.True(t, found, "go-inverted-err-check must appear in review findings: %v", matches)
}

// TestAnalyzeReview_PythonRuleFires asserts the Python review rulepack
// also routes through the MCP path.
func TestAnalyzeReview_PythonRuleFires(t *testing.T) {
	srv, _ := setupTestServer(t)
	writeReviewFixture(t, srv, "lib.py", "python", `def f(x=[]):
    return x
`)
	out := callAnalyzeReview(t, srv, map[string]any{"language": "python"})
	total, _ := out["total"].(float64)
	require.GreaterOrEqual(t, total, float64(1), "expected py-mutable-default-arg to fire; got %v", out)
	row := out["matches"].([]any)[0].(map[string]any)
	assert.Equal(t, "py-mutable-default-arg", row["detector"])
	assert.Equal(t, "review", row["category"])
}

// TestAnalyzeReview_GroundingDropsRefutedN1 proves the graph-grounding
// post-pass is wired into the review path: the N+1 detector fires on
// BOTH functions (both contain a for-loop with a query call in the
// source) but the enclosing symbol of the first carries loop_depth=0 in
// the graph (planted refutation), so its row is dropped while the
// genuine one — loop_depth=2 — survives.
func TestAnalyzeReview_GroundingDropsRefutedN1(t *testing.T) {
	srv, _ := setupTestServer(t)
	abs := writeReviewFixture(t, srv, "n1.go", "go", `package x

func Refuted(db *DB, ids []int) {
	for _, id := range ids {
		db.Query(id)
	}
}

func Genuine(db *DB, ids []int) {
	for _, id := range ids {
		db.Query(id)
	}
}
`)
	// Refuted spans lines 3-7, planted with no loop (loop_depth absent → 0).
	addFnNode(srv, abs, "Refuted", 3, 7, nil)
	// Genuine spans lines 9-13, planted with a real loop.
	addFnNode(srv, abs, "Genuine", 9, 13, map[string]any{"loop_depth": 2})

	out := callAnalyzeReview(t, srv, map[string]any{"detector": "go-loop-query-call"})

	matches := out["matches"].([]any)
	require.Len(t, matches, 1, "grounding must drop the loop_depth=0 N+1 and keep the loop_depth=2 one: %v", matches)
	row := matches[0].(map[string]any)
	assert.Equal(t, "go-loop-query-call", row["detector"])
	assert.Equal(t, abs+"::Genuine", row["symbol"],
		"the surviving N+1 must be the graph-confirmed one (Genuine)")
}
