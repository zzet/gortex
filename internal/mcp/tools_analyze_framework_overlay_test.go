package mcp

import (
	"context"
	"encoding/json"
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"

	"github.com/zzet/gortex/internal/graph"
)

// callAnalyzeForOverlayRequest runs the analyze dispatcher with a caller-
// supplied context so a test can hand the handler an overlay-active request
// (see overlayCtx) instead of a bare background one.
func callAnalyzeForOverlayRequest(t *testing.T, srv *Server, ctx context.Context, kind string, args map[string]any) map[string]any {
	t.Helper()
	if args == nil {
		args = map[string]any{}
	}
	args["kind"] = kind
	req := mcplib.CallToolRequest{}
	req.Params.Name = "analyze"
	req.Params.Arguments = args
	res, err := srv.handleAnalyze(ctx, req)
	if err != nil {
		t.Fatalf("handleAnalyze %s: %v", kind, err)
	}
	if res.IsError {
		t.Fatalf("analyze %s errored: %+v", kind, res.Content)
	}
	text := res.Content[0].(mcplib.TextContent)
	var out map[string]any
	if err := json.Unmarshal([]byte(text.Text), &out); err != nil {
		t.Fatalf("analyze %s json: %v\n%s", kind, err, text.Text)
	}
	return out
}

// TestAnalyzeTestsAsEdgesReadsThroughRequestReader pins tests_as_edges to the
// request reader on both of its graph reads: the EdgeTests scan and the bulk
// node fetch. Under an overlay the row carries the buffer's line for the test
// function (replaced payload) and loses the symbol the buffer deleted.
func TestAnalyzeTestsAsEdgesReadsThroughRequestReader(t *testing.T) {
	srv, _ := setupTestServer(t)
	const (
		testFile = "pkg/x_test.go"
		srcFile  = "pkg/x.go"
		testID   = testFile + "::TestFoo"
		fooID    = srcFile + "::Foo"
		goneID   = srcFile + "::Gone"
	)
	srv.graph.AddNode(&graph.Node{ID: testID, Name: "TestFoo", Kind: graph.KindFunction, FilePath: testFile, StartLine: 10})
	srv.graph.AddNode(&graph.Node{ID: fooID, Name: "Foo", Kind: graph.KindFunction, FilePath: srcFile, StartLine: 3})
	srv.graph.AddNode(&graph.Node{ID: goneID, Name: "Gone", Kind: graph.KindFunction, FilePath: srcFile, StartLine: 7})
	srv.graph.AddEdge(&graph.Edge{From: testID, To: fooID, Kind: graph.EdgeTests, FilePath: testFile, Line: 11})
	srv.graph.AddEdge(&graph.Edge{From: testID, To: goneID, Kind: graph.EdgeTests, FilePath: testFile, Line: 12})

	layer := graph.NewOverlayLayer()
	layer.MarkFile(testFile, false)
	layer.AddNode(testFile, &graph.Node{
		ID: testID, Name: "TestFoo", Kind: graph.KindFunction,
		FilePath: testFile, StartLine: 40,
	})
	layer.AddEdge(&graph.Edge{From: testID, To: fooID, Kind: graph.EdgeTests, FilePath: testFile, Line: 41})
	layer.MarkRemoved("Gone", goneID)

	args := func() map[string]any { return map[string]any{"group_by": "test"} }

	baseOut := callAnalyzeForOverlayRequest(t, srv, context.Background(), "tests_as_edges", args())
	baseLine, baseCovers := testEdgeRowFor(t, baseOut, testID)
	if baseLine != 10 {
		t.Fatalf("base line for %s = %d, want 10", testID, baseLine)
	}
	if len(baseCovers) != 2 || !baseCovers[fooID] || !baseCovers[goneID] {
		t.Fatalf("base covers = %v, want both %s and %s", baseCovers, fooID, goneID)
	}

	out := callAnalyzeForOverlayRequest(t, srv, overlayCtx(t, srv, layer), "tests_as_edges", args())
	line, covers := testEdgeRowFor(t, out, testID)
	if line != 40 {
		t.Fatalf("overlay line for %s = %d, want the buffer's 40", testID, line)
	}
	if len(covers) != 1 || !covers[fooID] {
		t.Fatalf("overlay covers = %v, want only %s", covers, fooID)
	}
	if covers[goneID] {
		t.Fatalf("overlay kept %s, which the buffer deleted", goneID)
	}
}

// testEdgeRowFor pulls one tests_as_edges row (group_by=test) out of the
// response, returning its line and the set of IDs it covers.
func testEdgeRowFor(t *testing.T, out map[string]any, id string) (int, map[string]bool) {
	t.Helper()
	rows, _ := out["rows"].([]any)
	for _, raw := range rows {
		row, _ := raw.(map[string]any)
		if row == nil || row["id"] != id {
			continue
		}
		line, _ := row["line"].(float64)
		covers := map[string]bool{}
		refs, _ := row["covers"].([]any)
		for _, r := range refs {
			ref, _ := r.(map[string]any)
			if ref == nil {
				continue
			}
			if s, ok := ref["id"].(string); ok {
				covers[s] = true
			}
		}
		return int(line), covers
	}
	t.Fatalf("no tests_as_edges row for %s in %v", id, out["rows"])
	return 0, nil
}

// TestAnalyzeComponentsReadsThroughRequestReader pins the per-component
// EdgeRendersChild walk to the request reader: under an overlay the parent's
// children come from the buffer (the replacement call site's line), and the
// child the buffer deleted is gone.
func TestAnalyzeComponentsReadsThroughRequestReader(t *testing.T) {
	srv, _ := setupTestServer(t)
	const (
		appFile  = "ui/app.jsx"
		listFile = "ui/list.jsx"
		appID    = appFile + "::App"
		listID   = listFile + "::List"
		legacyID = listFile + "::Legacy"
	)
	srv.graph.AddNode(&graph.Node{ID: appID, Name: "App", Kind: graph.KindFunction, FilePath: appFile})
	srv.graph.AddNode(&graph.Node{ID: listID, Name: "List", Kind: graph.KindFunction, FilePath: listFile})
	srv.graph.AddNode(&graph.Node{ID: legacyID, Name: "Legacy", Kind: graph.KindFunction, FilePath: listFile})
	srv.graph.AddEdge(&graph.Edge{From: appID, To: listID, Kind: graph.EdgeRendersChild, FilePath: appFile, Line: 5})
	srv.graph.AddEdge(&graph.Edge{From: appID, To: legacyID, Kind: graph.EdgeRendersChild, FilePath: appFile, Line: 6})

	layer := graph.NewOverlayLayer()
	layer.MarkFile(appFile, false)
	layer.AddNode(appFile, &graph.Node{ID: appID, Name: "App", Kind: graph.KindFunction, FilePath: appFile})
	layer.AddEdge(&graph.Edge{From: appID, To: listID, Kind: graph.EdgeRendersChild, FilePath: appFile, Line: 9})
	layer.MarkRemoved("Legacy", legacyID)

	baseOut := callAnalyzeForOverlayRequest(t, srv, context.Background(), "components", map[string]any{"id": appID})
	if got := componentChildLines(baseOut); len(got) != 2 || got[listID] != 5 || got[legacyID] != 6 {
		t.Fatalf("base children = %v, want %s:5 and %s:6", got, listID, legacyID)
	}

	out := callAnalyzeForOverlayRequest(t, srv, overlayCtx(t, srv, layer), "components", map[string]any{"id": appID})
	got := componentChildLines(out)
	if len(got) != 1 {
		t.Fatalf("overlay children = %v, want just the buffer's one child", got)
	}
	if got[listID] != 9 {
		t.Fatalf("overlay child line for %s = %d, want the buffer's 9", listID, got[listID])
	}
	if _, ok := got[legacyID]; ok {
		t.Fatalf("overlay kept %s, which the buffer deleted", legacyID)
	}
}

// componentChildLines maps a components(id=…) response to child ID -> line.
func componentChildLines(out map[string]any) map[string]int {
	children, _ := out["children"].([]any)
	got := map[string]int{}
	for _, raw := range children {
		child, _ := raw.(map[string]any)
		if child == nil {
			continue
		}
		to, _ := child["to"].(string)
		line, _ := child["line"].(float64)
		got[to] = int(line)
	}
	return got
}

// TestAnalyzeSynthesizersReadsThroughRequestReader covers the widened
// analyzer.AnalyzeSynthesizers signature end to end: handed the request
// reader, the rollup counts the buffer's synthesized edge and drops the base
// edge the buffer replaced.
func TestAnalyzeSynthesizersReadsThroughRequestReader(t *testing.T) {
	srv, _ := setupTestServer(t)
	const (
		callerFile = "svc/caller.go"
		callerID   = callerFile + "::Caller"
		targetID   = "svc/target.go::Handle"
	)
	srv.graph.AddNode(&graph.Node{ID: callerID, Name: "Caller", Kind: graph.KindFunction, FilePath: callerFile})
	srv.graph.AddNode(&graph.Node{ID: targetID, Name: "Handle", Kind: graph.KindFunction, FilePath: "svc/target.go"})
	addSynthEdge(srv.graph, callerID, targetID, "grpc-stub", "grpc.stub")

	layer := graph.NewOverlayLayer()
	layer.MarkFile(callerFile, false)
	layer.AddNode(callerFile, &graph.Node{ID: callerID, Name: "Caller", Kind: graph.KindFunction, FilePath: callerFile})
	layer.AddEdge(&graph.Edge{
		From: callerID, To: targetID, Kind: graph.EdgeCalls,
		Meta: map[string]any{
			"synthesized_by": "event-channel",
			"provenance":     "heuristic",
			"via":            "event.channel",
		},
	})

	if got := synthesizerNames(callAnalyzeForOverlayRequest(t, srv, context.Background(), "synthesizers", nil)); len(got) != 1 || got["grpc-stub"] != 1 {
		t.Fatalf("base synthesizers = %v, want one grpc-stub edge", got)
	}

	got := synthesizerNames(callAnalyzeForOverlayRequest(t, srv, overlayCtx(t, srv, layer), "synthesizers", nil))
	if len(got) != 1 || got["event-channel"] != 1 {
		t.Fatalf("overlay synthesizers = %v, want one event-channel edge", got)
	}
	if _, ok := got["grpc-stub"]; ok {
		t.Fatalf("overlay kept the base grpc-stub edge the buffer replaced: %v", got)
	}
}

// synthesizerNames maps a synthesizers response to synthesizer -> edge count.
func synthesizerNames(out map[string]any) map[string]int {
	rows, _ := out["synthesizers"].([]any)
	got := map[string]int{}
	for _, raw := range rows {
		row, _ := raw.(map[string]any)
		if row == nil {
			continue
		}
		name, _ := row["synthesizer"].(string)
		edges, _ := row["edges"].(float64)
		got[name] = int(edges)
	}
	return got
}
