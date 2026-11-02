package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/mcp"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"github.com/zzet/gortex/internal/query"
)

// Exercise the real facade and text producer rather than inserting a false
// truncation field into a stub. A complete text response uses count and the
// absence of positive truncation flags; symbol pages explicitly carry false.
func TestSearchReadbackNegativeUsesActualTextProducerCompleteness(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "main.go"), []byte("package main\n\nfunc Existing() {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	registry := parser.NewRegistry()
	registry.Register(languages.NewGoExtractor())
	idx := indexer.New(store, registry, config.Default().Index, zap.NewNop())
	idx.SetRepoPrefix("fixture")
	t.Cleanup(idx.Close)
	if _, err := idx.Index(root); err != nil {
		t.Fatal(err)
	}
	server := mcp.NewServer(query.NewEngine(store), store, idx, nil, zap.NewNop(), nil)
	const sessionID = "search-negative-producer"
	server.NoteSessionToolPolicy(sessionID, mcp.FacadeSurfaceVersion, "full")
	ctx := mcp.WithSessionID(t.Context(), sessionID)
	tool := server.MCPServer().GetTool("search")
	if tool == nil {
		t.Fatal("registered search facade missing")
	}
	step := editProbeStep{ExpectedSearchLine: 3, BeforeSymbol: "main.go::RemovedBefore", Marker: "Removed"}
	repo := privateRepo{Prefix: "fixture", Corpus: Corpus{File: "main.go"}}
	probe := searchReadbackRequest("old_text_absent", repo, step)
	probe.Arguments["output"] = editProbeDispatchOutput(probe)
	request := mcplib.CallToolRequest{}
	request.Params.Name, request.Params.Arguments = probe.Tool, probe.Arguments
	result, err := tool.Handler(ctx, request)
	if err != nil || result == nil || result.IsError {
		t.Fatalf("actual text handler failed: result=%+v err=%v", result, err)
	}
	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	body, err := decodeEditProbeOutput(raw, "json", "search.text")
	if err != nil {
		t.Fatal(err)
	}
	if _, emitted := body["truncated"]; emitted {
		t.Fatal("fixture no longer exercises the actual text producer's absent false flag")
	}
	for _, fault := range []string{"none", "missing_rows", "missing_count", "count_mismatch", "inexact_count", "old_hit", "truncated", "unknown_truncation", "limit", "budget", "continuation"} {
		t.Run(fault, func(t *testing.T) {
			copy := make(map[string]any, len(body))
			for key, value := range body {
				copy[key] = value
			}
			switch fault {
			case "missing_rows":
				delete(copy, "matches")
			case "missing_count":
				delete(copy, "count")
			case "count_mismatch":
				copy["count"] = 1
			case "inexact_count":
				copy["count_is_exact"] = false
			case "old_hit":
				copy["count"] = 1
				copy["matches"] = []any{map[string]any{"path": "main.go", "line": 3, "text": "func RemovedBefore() {}"}}
			case "truncated":
				copy["truncated"] = true
			case "unknown_truncation":
				copy["_truncated_by_limit"] = "unknown"
			case "limit":
				copy["_truncated_by_limit"] = true
			case "budget":
				copy["_truncated_by_budget"] = true
			case "continuation":
				copy["next_cursor"] = "remaining"
			}
			if got := exactSearchReadback("old_text_absent", copy, repo, step, ""); got != (fault == "none") {
				t.Fatalf("actual negative completeness %s: got=%v body=%v", fault, got, copy)
			}
		})
	}
}
