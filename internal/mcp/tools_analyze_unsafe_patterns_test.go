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

// callAnalyzeUnsafe is the shared harness: build a request with the
// given args, invoke the analyze handler, decode the JSON. Any tool-
// level error fails the test.
func callAnalyzeUnsafe(t *testing.T, srv *Server, extra map[string]any) map[string]any {
	t.Helper()
	args := map[string]any{"kind": "unsafe_patterns"}
	for k, v := range extra {
		args[k] = v
	}
	req := mcplib.CallToolRequest{}
	req.Params.Name = "analyze"
	req.Params.Arguments = args
	res, err := srv.handleAnalyze(context.Background(), req)
	require.NoError(t, err)
	require.False(t, res.IsError, "analyze unsafe_patterns must not error: %+v", res.Content)
	text := res.Content[0].(mcplib.TextContent).Text
	var out map[string]any
	require.NoError(t, json.Unmarshal([]byte(text), &out), "json: %s", text)
	return out
}

// writeUnsafeFixture drops a fixture file into the indexed test repository and
// registers a KindFile node for it so buildASTTargets exercises production
// path confinement rather than an unregistered absolute temp path.
func writeUnsafeFixture(t *testing.T, srv *Server, name, lang, src string) string {
	t.Helper()
	dir := srv.indexer.RootPath()
	require.NotEmpty(t, dir, "test server has no indexed repository root")
	abs := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(abs, []byte(src), 0o644))
	srv.graph.AddNode(&graph.Node{
		ID: abs, Kind: graph.KindFile, Name: abs,
		FilePath: abs, Language: lang, StartLine: 1, EndLine: 100,
	})
	return abs
}

// TestAnalyzeDispatcher_RoutesUnsafePatterns regression-protects
// the dispatcher wiring: passing kind=unsafe_patterns must not
// return an unknown-kind error.
func TestAnalyzeDispatcher_RoutesUnsafePatterns(t *testing.T) {
	srv, _ := setupTestServer(t)
	req := mcplib.CallToolRequest{}
	req.Params.Name = "analyze"
	req.Params.Arguments = map[string]any{"kind": "unsafe_patterns"}
	res, err := srv.handleAnalyze(context.Background(), req)
	require.NoError(t, err)
	require.False(t, res.IsError,
		"dispatcher must route kind=unsafe_patterns without error; got %v", res)
}

func TestAnalyzeUnsafePatterns_RustUnwrapAndPanic(t *testing.T) {
	srv, _ := setupTestServer(t)
	writeUnsafeFixture(t, srv, "lib.rs", "rust", `fn run(s: &str) {
    let _n: i32 = s.parse().unwrap();
    panic!("boom");
    todo!("later");
}
`)

	out := callAnalyzeUnsafe(t, srv, nil)

	total, _ := out["total"].(float64)
	require.GreaterOrEqual(t, total, float64(3),
		"expected at least 3 matches (unwrap + panic! + todo!) — got %v\n%v", total, out)

	matches := out["matches"].([]any)
	detectors := map[string]int{}
	for _, m := range matches {
		row := m.(map[string]any)
		detectors[row["detector"].(string)]++
		assert.Equal(t, "rust", row["language"])
	}
	assert.Equal(t, 1, detectors["unsafe-rust-unwrap"])
	assert.Equal(t, 2, detectors["unsafe-rust-panic-macro"])

	// Severity is set per detector.
	summary := out["summary"].([]any)
	require.NotEmpty(t, summary)
}

func TestAnalyzeUnsafePatterns_FilterByDetector(t *testing.T) {
	srv, _ := setupTestServer(t)
	writeUnsafeFixture(t, srv, "lib.rs", "rust", `fn run(s: &str) {
    let _ = s.parse::<i32>().unwrap();
    panic!("boom");
}
`)

	out := callAnalyzeUnsafe(t, srv, map[string]any{"detector": "unsafe-rust-unwrap"})

	total, _ := out["total"].(float64)
	assert.Equal(t, float64(1), total, "detector filter must restrict to unsafe-rust-unwrap only")

	matches := out["matches"].([]any)
	for _, m := range matches {
		row := m.(map[string]any)
		assert.Equal(t, "unsafe-rust-unwrap", row["detector"])
	}
}

func TestAnalyzeUnsafePatterns_FilterByLanguage(t *testing.T) {
	srv, _ := setupTestServer(t)
	writeUnsafeFixture(t, srv, "lib.rs", "rust", `fn run() { panic!("x"); }
`)
	writeUnsafeFixture(t, srv, "lib.py", "python", `def run(x):
    assert x > 0
`)

	out := callAnalyzeUnsafe(t, srv, map[string]any{"language": "python"})

	total, _ := out["total"].(float64)
	require.Equal(t, float64(1), total, "language=python must drop the Rust match")
	matches := out["matches"].([]any)
	row := matches[0].(map[string]any)
	assert.Equal(t, "python", row["language"])
	assert.Equal(t, "unsafe-python-assert", row["detector"])
}

func TestAnalyzeUnsafePatterns_FilterBySeverity(t *testing.T) {
	srv, _ := setupTestServer(t)
	writeUnsafeFixture(t, srv, "lib.rs", "rust", `fn run() {
    panic!("warn");
    assert!(1 == 1);
}
`)

	// severity=warning keeps panic!; severity=info keeps assert!.
	outWarn := callAnalyzeUnsafe(t, srv, map[string]any{"severity": "warning"})
	for _, m := range outWarn["matches"].([]any) {
		assert.Equal(t, "warning", m.(map[string]any)["severity"])
	}

	outInfo := callAnalyzeUnsafe(t, srv, map[string]any{"severity": "info"})
	for _, m := range outInfo["matches"].([]any) {
		assert.Equal(t, "info", m.(map[string]any)["severity"])
	}
}

func TestAnalyzeUnsafePatterns_JSThrow(t *testing.T) {
	srv, _ := setupTestServer(t)
	writeUnsafeFixture(t, srv, "lib.ts", "typescript", `function run(x: number): number {
  if (x < 0) {
    throw new Error("neg");
  }
  return x;
}
`)

	out := callAnalyzeUnsafe(t, srv, map[string]any{"language": "typescript"})
	total, _ := out["total"].(float64)
	require.Equal(t, float64(1), total)

	row := out["matches"].([]any)[0].(map[string]any)
	assert.Equal(t, "unsafe-js-throw", row["detector"])
	assert.Equal(t, "typescript", row["language"])
}

func TestAnalyzeUnsafePatterns_UnknownDetectorErrors(t *testing.T) {
	srv, _ := setupTestServer(t)
	req := mcplib.CallToolRequest{}
	req.Params.Name = "analyze"
	req.Params.Arguments = map[string]any{
		"kind":     "unsafe_patterns",
		"detector": "no-such-detector",
	}
	res, err := srv.handleAnalyze(context.Background(), req)
	require.NoError(t, err)
	require.True(t, res.IsError, "unknown detector name must surface as an error result")
}

func TestAnalyzeUnsafePatterns_PathPrefixScopes(t *testing.T) {
	srv, _ := setupTestServer(t)
	keep := writeUnsafeFixture(t, srv, "in.rs", "rust", `fn run() { panic!("x"); }`)
	writeUnsafeFixture(t, srv, "out.rs", "rust", `fn run() { panic!("y"); }`)

	out := callAnalyzeUnsafe(t, srv, map[string]any{"path_prefix": keep})

	total, _ := out["total"].(float64)
	require.Equal(t, float64(1), total, "path_prefix must scope to the named file")
	row := out["matches"].([]any)[0].(map[string]any)
	assert.Equal(t, keep, row["file"])
}

func TestAnalyzeUnsafePatterns_LimitTruncates(t *testing.T) {
	srv, _ := setupTestServer(t)
	writeUnsafeFixture(t, srv, "lib.rs", "rust", `fn run() {
    panic!("a");
    panic!("b");
    panic!("c");
}
`)

	out := callAnalyzeUnsafe(t, srv, map[string]any{"limit": 2.0})

	total, _ := out["total"].(float64)
	assert.Equal(t, float64(3), total, "total must report the pre-truncation count")
	truncated, _ := out["truncated"].(bool)
	assert.True(t, truncated)
	matches := out["matches"].([]any)
	assert.Equal(t, 2, len(matches))
}

func TestAnalyzeUnsafePatterns_GCXEncodesHeader(t *testing.T) {
	srv, _ := setupTestServer(t)
	writeUnsafeFixture(t, srv, "lib.rs", "rust", `fn run() { panic!("x"); }`)

	req := mcplib.CallToolRequest{}
	req.Params.Name = "analyze"
	req.Params.Arguments = map[string]any{
		"kind":   "unsafe_patterns",
		"format": "gcx",
	}
	res, err := srv.handleAnalyze(context.Background(), req)
	require.NoError(t, err)
	require.False(t, res.IsError)

	text := res.Content[0].(mcplib.TextContent).Text
	assert.Contains(t, text, "analyze.unsafe_patterns")
	for _, col := range []string{"detector", "severity", "language", "file", "line", "symbol", "text"} {
		assert.Contains(t, text, col, "GCX header must list column %q", col)
	}
}

func TestAnalyzeUnsafePatterns_CompactOutput(t *testing.T) {
	srv, _ := setupTestServer(t)
	abs := writeUnsafeFixture(t, srv, "lib.rs", "rust", `fn run() { panic!("x"); }`)

	req := mcplib.CallToolRequest{}
	req.Params.Name = "analyze"
	req.Params.Arguments = map[string]any{
		"kind":    "unsafe_patterns",
		"compact": true,
	}
	res, err := srv.handleAnalyze(context.Background(), req)
	require.NoError(t, err)
	require.False(t, res.IsError)
	text := res.Content[0].(mcplib.TextContent).Text

	assert.Contains(t, text, "unsafe-rust-panic-macro")
	assert.Contains(t, text, abs)
}
