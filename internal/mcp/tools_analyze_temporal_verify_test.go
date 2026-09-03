package mcp

import (
	"context"
	"strings"
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/zzet/gortex/internal/graph"
)

// TestAnalyzeTemporalVerify_RoutesAndRequiresLLM asserts that
// `analyze kind=temporal_verify` is wired into the dispatcher and, with no LLM
// provider configured (the default test server), returns a clear actionable
// error result instead of panicking or silently no-op'ing.
func TestAnalyzeTemporalVerify_RoutesAndRequiresLLM(t *testing.T) {
	srv, _ := setupTestServer(t)
	if srv.llmService != nil && srv.llmService.Enabled() {
		t.Fatal("test precondition: setupTestServer must leave the LLM disabled")
	}

	req := mcplib.CallToolRequest{}
	req.Params.Name = "analyze"
	req.Params.Arguments = map[string]any{"kind": "temporal_verify"}

	res, err := srv.handleAnalyze(context.Background(), req)
	if err != nil {
		t.Fatalf("handleAnalyze returned a transport error: %v", err)
	}
	if res == nil || !res.IsError {
		t.Fatalf("expected an error result when no LLM is configured, got %+v", res)
	}
	text := res.Content[0].(mcplib.TextContent).Text
	if !strings.Contains(text, "LLM provider") {
		t.Errorf("error result should name the missing LLM provider, got: %q", text)
	}
}

// TestAnalyzeTemporalVerify_UnknownKindStillRejected guards that adding the new
// kind didn't break the dispatcher's unknown-kind fallthrough.
func TestAnalyzeTemporalVerify_UnknownKindStillRejected(t *testing.T) {
	srv, _ := setupTestServer(t)
	req := mcplib.CallToolRequest{}
	req.Params.Name = "analyze"
	req.Params.Arguments = map[string]any{"kind": "temporal_verify_does_not_exist"}

	res, err := srv.handleAnalyze(context.Background(), req)
	if err != nil {
		t.Fatalf("handleAnalyze: %v", err)
	}
	if res == nil || !res.IsError {
		t.Fatalf("unknown kind must yield an error result, got %+v", res)
	}
}

// TestServerSourceProvider_ReadsNodeSource exercises the MCP-side source
// provider that the handler injects into resolver.VerifyTemporalEdges: it must
// resolve a graph node's on-disk path (via the server's resolveNodePath) and
// return the node's source slice. This is the handler-specific seam — the LLM
// verification core itself is covered in the resolver / analyzer packages.
func TestServerSourceProvider_ReadsNodeSource(t *testing.T) {
	srv, _ := setupTestServer(t)

	// setupTestServer indexes a fixture containing `func helper() {}`.
	var helper *graph.Node
	for _, n := range srv.graph.FindNodesByName("helper") {
		if n != nil && n.Kind == graph.KindFunction {
			helper = n
			break
		}
	}
	if helper == nil {
		t.Fatal("fixture node `helper` not found in the indexed graph")
	}

	src := newServerSourceProvider(context.Background(), srv)
	body, ok := src.NodeSource(helper)
	if !ok {
		t.Fatalf("NodeSource(helper) returned ok=false (path=%q)", helper.FilePath)
	}
	if !strings.Contains(body, "helper") {
		t.Errorf("source slice should contain the function name, got: %q", body)
	}

	// A nil node degrades to ("", false) — never a panic.
	if got, ok := src.NodeSource(nil); ok || got != "" {
		t.Errorf("NodeSource(nil) = (%q, %v), want (\"\", false)", got, ok)
	}
}
