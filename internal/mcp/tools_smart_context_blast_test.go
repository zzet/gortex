package mcp

import (
	"bytes"
	"context"
	"fmt"
	"testing"

	"github.com/zzet/gortex/internal/graph"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSmartContext_BlastRadiusAlwaysPresent verifies the fused
// blast-radius block is emitted on every call (no entry_point given)
// and carries the caller/test sub-lists.
func TestSmartContext_BlastRadiusAlwaysPresent(t *testing.T) {
	srv, _ := setupCompressTestServer(t)
	m := extractTextResult(t, callTool(t, srv, "smart_context", map[string]any{
		"task": "validate token and parse claims",
	}))
	br, ok := m["blast_radius"].(map[string]any)
	require.True(t, ok, "blast_radius must be present on every smart_context call, keys: %v", keysOf(m))
	_, hasCallers := br["callers_by_file"]
	assert.True(t, hasCallers, "blast_radius must carry callers_by_file")
	_, hasTests := br["covering_tests"]
	assert.True(t, hasTests, "blast_radius must carry covering_tests")
}

// TestSmartContext_BlastRadiusNoTestsWarning verifies that when no
// working-set symbol has a covering test the block carries the warning.
func TestSmartContext_BlastRadiusNoTestsWarning(t *testing.T) {
	srv, _ := setupCompressTestServer(t)
	m := extractTextResult(t, callTool(t, srv, "smart_context", map[string]any{
		"task": "validate token and parse claims",
	}))
	br, _ := m["blast_radius"].(map[string]any)
	require.NotNil(t, br)
	// The single-file fixture ships no tests, so the warning must fire.
	assert.Equal(t, "no covering tests found", br["warning"],
		"a working set with no EdgeTests coverage must warn")
}

// TestSmartContext_BlastRadiusStable verifies the blast-radius block
// keeps the pack root byte-stable within a session.
func TestSmartContext_BlastRadiusStable(t *testing.T) {
	srv, _ := setupCompressTestServer(t)
	args := map[string]any{"task": "validate token and parse claims"}
	a := extractTextResult(t, callTool(t, srv, "smart_context", args))
	b := extractTextResult(t, callTool(t, srv, "smart_context", args))
	require.NotEmpty(t, a["etag"])
	assert.Equal(t, a["etag"], b["etag"],
		"the blast-radius block must not perturb within-session pack-root stability")
}

// TestSmartContext_WorkingSetClustered verifies the clustered-by-file
// working set is present alongside files_to_edit, with per-file symbol
// lists and an is_test flag.
func TestSmartContext_WorkingSetClustered(t *testing.T) {
	srv, _ := setupCompressTestServer(t)
	m := extractTextResult(t, callTool(t, srv, "smart_context", map[string]any{
		"task": "validate token and parse claims",
	}))
	// files_to_edit is kept for back-compat.
	_, hasFiles := m["files_to_edit"]
	assert.True(t, hasFiles, "files_to_edit must remain for back-compat")

	rawWS, ok := m["working_set"].([]any)
	require.True(t, ok, "working_set must be present, keys: %v", keysOf(m))
	require.NotEmpty(t, rawWS, "working_set must group the relevant symbols by file")
	for _, raw := range rawWS {
		c, ok := raw.(map[string]any)
		require.True(t, ok, "each working_set entry must be an object")
		assert.NotEmpty(t, c["file"], "each cluster names its file")
		_, hasIsTest := c["is_test"]
		assert.True(t, hasIsTest, "each cluster carries an is_test flag")
		syms, ok := c["symbols"].([]any)
		require.True(t, ok, "each cluster carries a symbols list")
		assert.NotEmpty(t, syms, "a cluster must carry at least one symbol id")
	}
}

// TestBuildBlastRadiusCapsCoveringTests verifies a symbol covered by more
// tests than the cap keeps the first blastRadiusTestLimit of the sorted list,
// reports the full count, and marks the gcx header as truncated.
func TestBuildBlastRadiusCapsCoveringTests(t *testing.T) {
	srv, _ := setupTestServer(t)
	const helperID = "pkg/helper.go::Helper"
	helper := &graph.Node{ID: helperID, Name: "Helper", Kind: graph.KindFunction, FilePath: "pkg/helper.go", StartLine: 1}
	srv.graph.AddNode(helper)
	total := blastRadiusTestLimit + 5
	for i := 0; i < total; i++ {
		testID := fmt.Sprintf("pkg/helper_test.go::TestHelper%02d", i)
		srv.graph.AddNode(&graph.Node{ID: testID, Name: fmt.Sprintf("TestHelper%02d", i), Kind: graph.KindFunction, FilePath: "pkg/helper_test.go", StartLine: 10 + i})
		srv.graph.AddEdge(&graph.Edge{From: testID, To: helperID, Kind: graph.EdgeTests, FilePath: "pkg/helper_test.go", Line: 11 + i})
	}

	br := srv.buildBlastRadius(context.Background(), []*graph.Node{helper})
	require.NotNil(t, br)
	tests, _ := br["covering_tests"].([]map[string]any)
	require.Len(t, tests, blastRadiusTestLimit)
	assert.Equal(t, "TestHelper00", tests[0]["function"])
	assert.Equal(t, total, br["covering_tests_total"])
	assert.Equal(t, true, br["covering_tests_truncated"])

	encoded, err := encodeSmartContext(map[string]any{"blast_radius": br})
	require.NoError(t, err)
	assert.True(t, bytes.Contains(encoded, []byte(fmt.Sprintf("total=%d", total))), "gcx header must carry the full count: %s", encoded)
}
