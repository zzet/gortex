package mcp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer"
	"go.uber.org/zap"
)

// newSafeDeleteTestServer builds a server with one indexed file
// containing two symbols: Live (called by Caller) and DeadCode
// (unused). The absolute graph-path fixture has a real parser-owning indexer
// so current-byte declaration resolution does not trust its manual spans.
func newSafeDeleteTestServer(t *testing.T) (*Server, string) {
	t.Helper()

	dir := t.TempDir()
	src := `package foo

// DeadCode is an unused helper.
func DeadCode() int {
	return 42
}

// Live is used by Caller.
func Live() int {
	return 1
}

func Caller() int {
	return Live()
}
`
	require.NoError(t, os.WriteFile(filepath.Join(dir, "foo.go"), []byte(src), 0o644))

	g := graph.New()
	// We use absolute paths in FilePath so resolveFilePath takes the
	// absolute branch and skips indexer dependency.
	abs := filepath.Join(dir, "foo.go")
	g.AddNode(&graph.Node{
		ID: abs + "::DeadCode", Name: "DeadCode", Kind: graph.KindFunction,
		FilePath: abs, StartLine: 4, EndLine: 6,
	})
	g.AddNode(&graph.Node{
		ID: abs + "::Live", Name: "Live", Kind: graph.KindFunction,
		FilePath: abs, StartLine: 9, EndLine: 11,
	})
	g.AddNode(&graph.Node{
		ID: abs + "::Caller", Name: "Caller", Kind: graph.KindFunction,
		FilePath: abs, StartLine: 13, EndLine: 15,
	})
	g.AddEdge(&graph.Edge{From: abs + "::Caller", To: abs + "::Live", Kind: graph.EdgeCalls})

	idx := indexer.New(g, testRegistry(), config.Default().Index, zap.NewNop())
	idx.SetRootPath(dir)
	s := &Server{
		indexer:    idx,
		graph:      g,
		session:    newSessionState(),
		tokenStats: &tokenStats{},
		symHistory: &symbolHistory{entries: make(map[string][]SymbolModification)},
		sessions:   newSessionMap(),
		toolScopes: newScopeRegistry(),
	}
	return s, dir
}

func callSafeDelete(t *testing.T, s *Server, args map[string]any) map[string]any {
	t.Helper()
	req := mcp.CallToolRequest{}
	req.Params.Arguments = args
	res, err := s.handleSafeDeleteSymbol(context.Background(), req)
	require.NoError(t, err)
	require.NotNil(t, res)
	if res.IsError {
		return map[string]any{"is_error": true, "content": res.Content}
	}
	tc, ok := res.Content[0].(mcp.TextContent)
	require.True(t, ok)
	var m map[string]any
	require.NoError(t, json.Unmarshal([]byte(tc.Text), &m))
	return m
}

func TestSafeDelete_DryRunReturnsPreview(t *testing.T) {
	s, dir := newSafeDeleteTestServer(t)
	abs := filepath.Join(dir, "foo.go")

	out := callSafeDelete(t, s, map[string]any{
		"id":      abs + "::DeadCode",
		"dry_run": true,
	})

	assert.Equal(t, "preview", out["status"])
	assert.EqualValues(t, 0, out["reference_count"].(float64))
	preview, _ := out["preview"].(string)
	assert.Contains(t, preview, "DeadCode")

	// Disk untouched — DeadCode still in file.
	body, _ := os.ReadFile(abs)
	assert.Contains(t, string(body), "func DeadCode()")
}

func TestSafeDelete_CommitRemovesSymbolAndDocComment(t *testing.T) {
	s, dir := newSafeDeleteTestServer(t)
	abs := filepath.Join(dir, "foo.go")

	out := callSafeDelete(t, s, map[string]any{
		"id":      abs + "::DeadCode",
		"dry_run": false,
	})
	assert.Equal(t, "deleted", out["status"])

	body, _ := os.ReadFile(abs)
	assert.NotContains(t, string(body), "func DeadCode()", "function removed")
	assert.NotContains(t, string(body), "DeadCode is an unused helper", "leading doc comment removed")
	assert.Contains(t, string(body), "func Live()", "Live untouched")
}

func TestSafeDelete_RejectsWhenReferenced(t *testing.T) {
	s, dir := newSafeDeleteTestServer(t)
	abs := filepath.Join(dir, "foo.go")

	out := callSafeDelete(t, s, map[string]any{
		"id":      abs + "::Live",
		"dry_run": false,
	})

	assert.Equal(t, "rejected_has_references", out["status"])
	assert.EqualValues(t, 1, out["reference_count"].(float64))
	refs, _ := out["references"].([]any)
	require.Len(t, refs, 1)
	row := refs[0].(map[string]any)
	assert.Equal(t, abs+"::Caller", row["from_id"])
	assert.Equal(t, "calls", row["kind"])

	// Disk untouched.
	body, _ := os.ReadFile(abs)
	assert.Contains(t, string(body), "func Live()")
}

func TestSafeDelete_ForceBypassesReferences(t *testing.T) {
	s, dir := newSafeDeleteTestServer(t)
	abs := filepath.Join(dir, "foo.go")

	out := callSafeDelete(t, s, map[string]any{
		"id":      abs + "::Live",
		"dry_run": false,
		"force":   true,
	})

	assert.Equal(t, "deleted", out["status"])
	body, _ := os.ReadFile(abs)
	assert.NotContains(t, string(body), "func Live()")
}

func TestSafeDelete_RejectsUnknownSymbol(t *testing.T) {
	s, _ := newSafeDeleteTestServer(t)
	out := callSafeDelete(t, s, map[string]any{
		"id": "nonexistent::Foo",
	})
	assert.True(t, out["is_error"] == true)
}

func TestSafeDelete_RejectsMissingID(t *testing.T) {
	s, _ := newSafeDeleteTestServer(t)
	out := callSafeDelete(t, s, map[string]any{})
	assert.True(t, out["is_error"] == true)
}

func TestSafeDelete_DryRunWithReferencesPreservesRefList(t *testing.T) {
	s, dir := newSafeDeleteTestServer(t)
	abs := filepath.Join(dir, "foo.go")

	out := callSafeDelete(t, s, map[string]any{
		"id":      abs + "::Live",
		"dry_run": true,
	})

	// References cause an immediate reject, regardless of dry_run.
	assert.Equal(t, "rejected_has_references", out["status"])
}

func TestSafeDelete_RecordsSessionEdit(t *testing.T) {
	s, dir := newSafeDeleteTestServer(t)
	abs := filepath.Join(dir, "foo.go")

	out := callSafeDelete(t, s, map[string]any{
		"id":      abs + "::DeadCode",
		"dry_run": false,
	})
	require.Equal(t, "deleted", out["status"])

	mods := s.symHistory.Get(abs + "::DeadCode")
	require.Len(t, mods, 1)
	assert.True(t, mods[0].SignatureChanged, "delete is a signature change")
}

func TestSafeDelete_LinesDeletedAccurate(t *testing.T) {
	s, dir := newSafeDeleteTestServer(t)
	abs := filepath.Join(dir, "foo.go")

	out := callSafeDelete(t, s, map[string]any{
		"id":      abs + "::DeadCode",
		"dry_run": true,
	})
	// Lines 3-7 (doc comment line 3, func lines 4-6, blank line 7).
	assert.EqualValues(t, 3, out["start_line"].(float64))
	assert.GreaterOrEqual(t, out["lines_deleted"].(float64), 4.0)
	preview, _ := out["preview"].(string)
	assert.Equal(t, strings.Count(preview, "\n")+1, int(out["lines_deleted"].(float64)))
}

func TestCollectReferencingEdges_FiltersStructuralEdges(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "a", Name: "a", Kind: graph.KindFunction})
	g.AddNode(&graph.Node{ID: "b", Name: "b", Kind: graph.KindFunction})
	g.AddNode(&graph.Node{ID: "file", Name: "file", Kind: graph.KindFile})

	// One real reference (calls) and one structural (defines).
	g.AddEdge(&graph.Edge{From: "b", To: "a", Kind: graph.EdgeCalls})
	g.AddEdge(&graph.Edge{From: "file", To: "a", Kind: graph.EdgeDefines})

	refs := collectReferencingEdges(g, "a")
	require.Len(t, refs, 1)
	assert.Equal(t, "calls", refs[0].Kind)
	assert.Equal(t, "b", refs[0].FromID)
}
