package mcp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"

	"github.com/zzet/gortex/internal/graph"
)

// callSearchAST is the test harness: build a request with the given
// args, invoke the MCP handler, decode the JSON result. Any tool
// error fails the test.
func callSearchAST(t *testing.T, srv *Server, args map[string]any) map[string]any {
	t.Helper()
	req := mcplib.CallToolRequest{}
	req.Params.Name = "search_ast"
	req.Params.Arguments = args
	res, err := srv.handleSearchAST(context.Background(), req)
	if err != nil {
		t.Fatalf("handleSearchAST: %v", err)
	}
	if res.IsError {
		t.Fatalf("error result: %+v", res.Content)
	}
	textBlock := res.Content[0].(mcplib.TextContent)
	var out map[string]any
	if err := json.Unmarshal([]byte(textBlock.Text), &out); err != nil {
		t.Fatalf("json: %v\n%s", err, textBlock.Text)
	}
	return out
}

// writeTempGoFile drops fixture content into the indexed test repository and
// returns the absolute path. Keeping fixtures beneath root exercises the same
// path-confinement contract as production instead of relying on unregistered
// absolute graph paths.
func writeTempGoFile(t *testing.T, root, name, src string) string {
	t.Helper()
	abs := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatalf("create fixture directory: %v", err)
	}
	if err := os.WriteFile(abs, []byte(src), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return abs
}

func TestSearchAST_RawPattern_GoPanic(t *testing.T) {
	srv, root := setupTestServer(t)
	abs := writeTempGoFile(t, root, "lib.go", `package x

func F() {
	panic("boom")
}
`)
	srv.graph.AddNode(&graph.Node{
		ID: abs, Kind: graph.KindFile, Name: abs,
		FilePath: abs, Language: "go", StartLine: 1, EndLine: 5,
	})
	srv.graph.AddNode(&graph.Node{
		ID:        abs + "::F",
		Kind:      graph.KindFunction,
		Name:      "F",
		FilePath:  abs,
		StartLine: 3, EndLine: 5,
	})

	out := callSearchAST(t, srv, map[string]any{
		"pattern":  `((call_expression function: (identifier) @fn) @match (#eq? @fn "panic"))`,
		"language": "go",
	})
	if got, _ := out["total"].(float64); got != 1 {
		t.Fatalf("expected 1 match, got %v\n%v", got, out)
	}
	matches := out["matches"].([]any)
	first := matches[0].(map[string]any)
	if first["symbol_id"] != abs+"::F" {
		t.Errorf("expected enclosing symbol_id %q, got %q", abs+"::F", first["symbol_id"])
	}
	if first["symbol_name"] != "F" {
		t.Errorf("expected symbol_name F, got %v", first["symbol_name"])
	}
}

func TestSearchASTUsesBoundedEnclosingProjection(t *testing.T) {
	srv, root := setupTestServer(t)
	abs := writeTempGoFile(t, root, "bounded.go", `package x

func Bounded() {
	panic("boom")
}
`)
	quietAbs := writeTempGoFile(t, root, "quiet.go", `package x

func Quiet() {}
`)
	backing := graph.New()
	backing.AddNode(&graph.Node{
		ID: abs, Kind: graph.KindFile, Name: abs,
		FilePath: abs, Language: "go", StartLine: 1, EndLine: 5,
	})
	backing.AddNode(&graph.Node{
		ID: quietAbs, Kind: graph.KindFile, Name: quietAbs,
		FilePath: quietAbs, Language: "go", StartLine: 1, EndLine: 3,
	})
	owner := &graph.Node{
		ID: abs + "::Bounded", Kind: graph.KindFunction, Name: "Bounded",
		FilePath: abs, StartLine: 3, EndLine: 5,
	}
	backing.AddNode(owner)
	probe := &astTargetBoundedStore{Store: backing}
	probe.read = func(ctx context.Context, path string, scope graph.LocalizationNodeScope, limit int) (graph.BoundedNodeProjection, error) {
		return backing.FindFileNodesBounded(ctx, path, scope, limit)
	}
	srv.graph = probe

	out := callSearchAST(t, srv, map[string]any{
		"pattern":  `((call_expression function: (identifier) @fn) @match (#eq? @fn "panic"))`,
		"language": "go",
	})

	if got, _ := out["total"].(float64); got != 1 {
		t.Fatalf("expected 1 bounded match, got %v\n%v", got, out)
	}
	match := out["matches"].([]any)[0].(map[string]any)
	if match["symbol_id"] != owner.ID || match["symbol_name"] != owner.Name {
		t.Fatalf("bounded handler enrichment = %#v, want %s/%s", match, owner.ID, owner.Name)
	}
	if len(probe.calls) != 1 || probe.calls[0].path != abs {
		t.Fatalf("bounded handler calls = %#v, want one read of %q", probe.calls, abs)
	}
}

func TestSearchAST_BundledDetector_HardcodedSecret(t *testing.T) {
	srv, root := setupTestServer(t)
	abs := writeTempGoFile(t, root, "creds.go", `package x

func F() {
	password := "hunter2hunter2hunter"
	emptyDefault := ""
	_ = password
	_ = emptyDefault
}
`)
	srv.graph.AddNode(&graph.Node{
		ID: abs, Kind: graph.KindFile, Name: abs,
		FilePath: abs, Language: "go", StartLine: 1, EndLine: 8,
	})

	out := callSearchAST(t, srv, map[string]any{
		"detector": "hardcoded-secret",
	})
	total, _ := out["total"].(float64)
	if total != 1 {
		t.Fatalf("expected 1 match, got %v\n%v", total, out)
	}
	first := out["matches"].([]any)[0].(map[string]any)
	if first["detector"] != "hardcoded-secret" {
		t.Errorf("expected detector enrichment, got %v", first["detector"])
	}
}

func TestSearchAST_PathPrefixFilter(t *testing.T) {
	srv, root := setupTestServer(t)
	matched := writeTempGoFile(t, root, "matched/match.go", `package x
func F() { panic("hit") }
`)
	excluded := writeTempGoFile(t, root, "excluded/skip.go", `package x
func G() { panic("skip") }
`)
	for _, p := range []string{matched, excluded} {
		srv.graph.AddNode(&graph.Node{
			ID: p, Kind: graph.KindFile, Name: p,
			FilePath: p, Language: "go", StartLine: 1, EndLine: 2,
		})
	}

	out := callSearchAST(t, srv, map[string]any{
		"pattern":     `((call_expression function: (identifier) @fn) @match (#eq? @fn "panic"))`,
		"language":    "go",
		"path_prefix": filepath.Dir(matched),
	})
	total, _ := out["total"].(float64)
	if total != 1 {
		t.Fatalf("expected 1 match (path_prefix scopes the file set), got %v", total)
	}
	first := out["matches"].([]any)[0].(map[string]any)
	if first["file"] != matched {
		t.Errorf("expected match in %q, got %q", matched, first["file"])
	}
}

func TestSearchAST_RejectsBadInputs(t *testing.T) {
	srv, _ := setupTestServer(t)

	// No pattern, no detector.
	{
		req := mcplib.CallToolRequest{}
		req.Params.Name = "search_ast"
		req.Params.Arguments = map[string]any{}
		res, err := srv.handleSearchAST(context.Background(), req)
		if err != nil {
			t.Fatalf("handleSearchAST: %v", err)
		}
		if !res.IsError {
			t.Fatal("expected error when neither pattern nor detector is set")
		}
	}
	// Pattern without language.
	{
		req := mcplib.CallToolRequest{}
		req.Params.Name = "search_ast"
		req.Params.Arguments = map[string]any{"pattern": "(identifier) @match"}
		res, err := srv.handleSearchAST(context.Background(), req)
		if err != nil {
			t.Fatalf("handleSearchAST: %v", err)
		}
		if !res.IsError {
			t.Fatal("expected error for pattern without language")
		}
	}
	// Both pattern and detector.
	{
		req := mcplib.CallToolRequest{}
		req.Params.Name = "search_ast"
		req.Params.Arguments = map[string]any{
			"pattern":  "(identifier) @match",
			"detector": "panic-in-library",
			"language": "go",
		}
		res, err := srv.handleSearchAST(context.Background(), req)
		if err != nil {
			t.Fatalf("handleSearchAST: %v", err)
		}
		if !res.IsError {
			t.Fatal("expected error for mutually-exclusive args")
		}
	}
	// Unknown detector.
	{
		req := mcplib.CallToolRequest{}
		req.Params.Name = "search_ast"
		req.Params.Arguments = map[string]any{"detector": "no-such-rule"}
		res, err := srv.handleSearchAST(context.Background(), req)
		if err != nil {
			t.Fatalf("handleSearchAST: %v", err)
		}
		if !res.IsError {
			t.Fatal("expected error for unknown detector")
		}
	}
}

func TestSearchAST_MinFanInOfEnclosingFunc(t *testing.T) {
	srv, root := setupTestServer(t)
	abs := writeTempGoFile(t, root, "two.go", `package x

func Hot() { panic("a") }
func Cold() { panic("b") }
`)
	srv.graph.AddNode(&graph.Node{
		ID: abs, Kind: graph.KindFile, Name: abs,
		FilePath: abs, Language: "go", StartLine: 1, EndLine: 4,
	})
	hot := &graph.Node{ID: abs + "::Hot", Kind: graph.KindFunction, Name: "Hot",
		FilePath: abs, StartLine: 3, EndLine: 3}
	cold := &graph.Node{ID: abs + "::Cold", Kind: graph.KindFunction, Name: "Cold",
		FilePath: abs, StartLine: 4, EndLine: 4}
	srv.graph.AddNode(hot)
	srv.graph.AddNode(cold)
	// Three callers on Hot, zero on Cold.
	for i := 0; i < 3; i++ {
		caller := &graph.Node{
			ID:       abs + "::caller" + string(rune('A'+i)),
			Kind:     graph.KindFunction,
			Name:     "caller",
			FilePath: abs, StartLine: 10 + i, EndLine: 10 + i,
		}
		srv.graph.AddNode(caller)
		srv.graph.AddEdge(&graph.Edge{From: caller.ID, To: hot.ID, Kind: graph.EdgeCalls})
	}

	out := callSearchAST(t, srv, map[string]any{
		"detector":                     "panic-in-library",
		"min_fan_in_of_enclosing_func": float64(2),
	})
	total, _ := out["total"].(float64)
	if total != 1 {
		t.Fatalf("expected only Hot's panic to survive min_fan_in=2 filter, got total=%v", total)
	}
	first := out["matches"].([]any)[0].(map[string]any)
	if first["symbol_name"] != "Hot" {
		t.Errorf("expected enclosing symbol Hot, got %v", first["symbol_name"])
	}
}
