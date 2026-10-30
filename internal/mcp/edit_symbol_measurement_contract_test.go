package mcp

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
)

// The benchmark adapters use existing arbitrary text splicing within an actual
// indexed declaration. Empty replacement and sibling declarations are product
// semantics, not invented operation enums or fake benchmark success.
func TestEditSymbolMeasurementFixtureUsesActualSpliceAndExplicitAbsence(t *testing.T) {
	for _, language := range []string{"go", "typescript", "python"} {
		for _, operation := range []string{"sibling", "rename", "delete", "append_leaf", "modify_body", "change_signature"} {
			t.Run(language+"/"+operation, func(t *testing.T) {
				s, root := setupTestServer(t)
				t.Cleanup(s.DrainBackground)
				var file, header, declaration, survivor, added, renamed, leaf, modified, signatureChanged, caller string
				switch language {
				case "go":
					file = "symbol-cell.go"
					header = "package main\n"
					declaration = "func Target() int { return 1 }"
					survivor = "func Sentinel() int { return 2 }"
					added = "func Added() int { return Target() }"
					renamed = "func Renamed() int { return 1 }"
					leaf = "func Added() int { return 7 }"
					modified = "func Target() int {\n value := 7\n return value\n}"
					signatureChanged = "func Target(optional ...int) int { return 1 }"
					caller = "func Caller() int { return Target() }"
				case "typescript":
					file = "symbol-cell.ts"
					declaration = "export function Target(): number { return 1; }"
					survivor = "export function Sentinel(): number { return 2; }"
					added = "export function Added(): number { return Target(); }"
					renamed = "export function Renamed(): number { return 1; }"
					leaf = "export function Added(): number { return 7; }"
					modified = "export function Target(): number {\n const value = 7;\n return value;\n}"
					signatureChanged = "export function Target(optional?: number): number { return 1; }"
					caller = "export function Caller(): number { return Target(); }"
				case "python":
					file = "symbol-cell.py"
					declaration = "def Target():\n    return 1"
					survivor = "def Sentinel():\n    return 2"
					added = "def Added():\n    return Target()"
					renamed = "def Renamed():\n    return 1"
					leaf = "def Added():\n    return 7"
					modified = "def Target():\n    value = 7\n    return value"
					signatureChanged = "def Target(optional=0):\n    return 1"
					caller = "def Caller():\n    return Target()"
				}
				source := header + declaration + "\n\n" + survivor + "\n"
				if operation == "modify_body" || operation == "change_signature" {
					source += "\n" + caller + "\n"
				}
				path := filepath.Join(root, file)
				require.NoError(t, os.WriteFile(path, []byte(source), 0600))
				require.NoError(t, s.indexer.IndexFile(path))
				id := file + "::Target"
				beforeNode := s.engineFor(t.Context()).GetSymbol(id)
				require.NotNil(t, beforeNode, "fixture must start with actual producer-owned symbol")
				beforeSpan := beforeNode.EndLine - beforeNode.StartLine
				replacement := declaration + "\n" + added
				if operation == "rename" {
					replacement = renamed
				}
				if operation == "delete" {
					replacement = ""
				}
				if operation == "append_leaf" {
					replacement = declaration + "\n" + leaf
				}
				if operation == "modify_body" {
					replacement = modified
				}
				if operation == "change_signature" {
					replacement = signatureChanged
				}
				s.facades.capture(mcpgo.NewTool("edit_symbol"), s.handleEditSymbol)
				req := editOperationRequest("edit", "symbol")
				req.Params.Arguments = map[string]any{"operation": "symbol", "target": map[string]any{"symbol": id}, "match": declaration, "replacement": replacement, "options": map[string]any{"mutation_id": "symbol-cell-" + language + "-" + operation}, "output": map[string]any{"format": "json"}}
				result, err := s.boundToolHandler(func(ctx context.Context, r mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
					return s.handleFacade(ctx, "edit", r)
				})(t.Context(), req)
				require.NoError(t, err)
				require.False(t, result.IsError, "%v", result.Content)
				response := unmarshalResult(t, result)
				require.Equal(t, "applied", response["status"])
				require.NotEmpty(t, response["mutation_receipt"])
				record, found := s.mutationCommits.byReceipt(response["mutation_receipt"].(string))
				require.True(t, found)
				status := s.mutationStatusPayload(record)
				require.Equal(t, "edit_symbol", status["tool"])
				ack := status["acknowledgement"].(acknowledgementPayload)
				require.Equal(t, "not_applicable_text_splice", ack.ParserScope)
				require.Equal(t, "not_applicable", ack.PhaseStates["parse"])
				parse, present := ack.ObservedExecutionMS["parse"]
				require.True(t, present)
				require.Nil(t, parse)
				actual, err := os.ReadFile(path)
				require.NoError(t, err)
				require.Equal(t, strings.Replace(source, declaration, replacement, 1), string(actual))
				require.NotNil(t, s.engineFor(t.Context()).GetSymbol(file+"::Sentinel"), "neighbor must survive every splice")
				switch operation {
    case "sibling", "append_leaf":
					require.NotNil(t, s.engineFor(t.Context()).GetSymbol(id))
					require.NotNil(t, s.engineFor(t.Context()).GetSymbol(file+"::Added"))
    case "modify_body", "change_signature":
					afterNode := s.engineFor(t.Context()).GetSymbol(id)
					require.NotNil(t, afterNode)
					require.NotNil(t, s.engineFor(t.Context()).GetSymbol(file+"::Caller"), "unchanged zero-argument caller must survive")
					sourceResult := callTool(t, s, "get_symbol_source", map[string]any{"id": id, "context_lines": 0})
					require.False(t, sourceResult.IsError)
					sourceBody := unmarshalResult(t, sourceResult)
					require.Equal(t, strings.TrimSpace(replacement), strings.TrimSpace(sourceBody["source"].(string)))
					if operation == "modify_body" {
						require.Greater(t, afterNode.EndLine-afterNode.StartLine, beforeSpan)
					}
					if operation == "change_signature" {
						switch language {
						case "go":
							require.Contains(t, afterNode.Meta["signature"], "optional")
						case "typescript":
							require.Equal(t, "function Target()", afterNode.Meta["signature"], "producer parameter detail is proved by exact source, not invented metadata")
						case "python":
							require.Equal(t, "def Target(...)", afterNode.Meta["signature"], "producer parameter detail is proved by exact source, not invented metadata")
						}
					}
    default:
					require.Nil(t, s.engineFor(t.Context()).GetSymbol(id), "old identity must really be absent, not an empty successful source")
					missing := callTool(t, s, "get_symbol_source", map[string]any{"id": id})
					require.True(t, missing.IsError, "expected absence is an explicit producer refusal")
					require.Contains(t, mustErrText(t, missing), id)
					if operation == "rename" {
						require.NotNil(t, s.engineFor(t.Context()).GetSymbol(file+"::Renamed"))
					}
					if operation == "delete" {
						chain := callTool(t, s, "get_call_chain", map[string]any{"id": id, "depth": 1, "limit": 1000})
						require.False(t, chain.IsError)
						chainBody := unmarshalResult(t, chain)
						require.Contains(t, chainBody, "nodes")
						require.Contains(t, chainBody, "edges")
						require.Empty(t, chainBody["nodes"])
						require.Empty(t, chainBody["edges"])
						require.Zero(t, chainBody["total_nodes"])
						require.Zero(t, chainBody["total_edges"])
						require.Equal(t, false, chainBody["truncated"])
						search := callTool(t, s, "search_symbols", map[string]any{"query": "GortexDeletedMeasurementNoSuchSymbol", "query_class": "symbol", "expand": "off"})
						require.False(t, search.IsError)
						searchBody := unmarshalResult(t, search)
						require.Contains(t, searchBody, "results")
						results, _ := searchBody["results"].([]any)
						require.EqualValues(t, len(results), searchBody["total"])
						for _, raw := range results {
							require.NotEqual(t, id, raw.(map[string]any)["id"], "deleted identity must remain absent even in complete fallback output")
						}
						require.Equal(t, "symbol", searchBody["query_class"])
						require.Equal(t, false, searchBody["truncated"])
					}

				}
			})
		}
	}
}

func TestDeleteMeasurementSearchCoversHundredDeclarationFixture(t *testing.T) {
	for _, language := range []string{"go", "typescript", "python"} {
		t.Run(language, func(t *testing.T) {
			s, root := setupTestServer(t)
			t.Cleanup(s.DrainBackground)
			file, source := "fixture.go", "package main\n"
			declaration := func(name string) string { return "func " + name + "() int { return 1 }" }
			if language == "typescript" {
				file = "fixture.ts"
				source = ""
				declaration = func(name string) string { return "export function " + name + "(): number { return 1; }" }
			}
			if language == "python" {
				file = "fixture.py"
				source = ""
				declaration = func(name string) string { return "def " + name + "():\n    return 1" }
			}
			for i := 0; i < 100; i++ {
				source += declaration(fmt.Sprintf("GortexEditProbe000000000000002aN%03dBefore", i)) + "\n\n"
			}
			path := filepath.Join(root, file)
			require.NoError(t, os.WriteFile(path, []byte(source), 0600))
			require.NoError(t, s.indexer.IndexFile(path))
			name := "GortexEditProbe000000000000002aN099Before"
			id := file + "::" + name
			args := map[string]any{"query": name, "query_class": "symbol", "expand": "off", "limit": 1000, "max_per_file": 0}
			before := callTool(t, s, "search_symbols", args)
			require.False(t, before.IsError)
			beforeBody := unmarshalResult(t, before)
			found := false
			for _, raw := range beforeBody["results"].([]any) {
				if raw.(map[string]any)["id"] == id {
					found = true
				}
			}
			require.True(t, found, "fixture must prove actual old ID was searchable before deletion")
			s.facades.capture(mcpgo.NewTool("edit_symbol"), s.handleEditSymbol)
			req := editOperationRequest("edit", "symbol")
			req.Params.Arguments = map[string]any{"operation": "symbol", "target": map[string]any{"symbol": id}, "match": declaration(name), "replacement": "", "output": map[string]any{"format": "json"}}
			result, err := s.boundToolHandler(func(ctx context.Context, r mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
				return s.handleFacade(ctx, "edit", r)
			})(t.Context(), req)
			require.NoError(t, err)
			require.False(t, result.IsError)
			after := callTool(t, s, "search_symbols", args)
			require.False(t, after.IsError)
			body := unmarshalResult(t, after)
			require.Contains(t, body, "results")
			results, _ := body["results"].([]any)
			require.EqualValues(t, len(results), body["total"])
			require.Equal(t, false, body["truncated"])
			require.Empty(t, body["next_cursor"])
			for _, raw := range results {
				require.NotEqual(t, id, raw.(map[string]any)["id"], "complete fallback population retained deleted exact ID")
			}
		})
	}
}

func TestEditSymbolCrossFileMeasurementUsesActualResolvedCallee(t *testing.T) {
	for _, language := range []string{"go", "typescript", "python"} {
		t.Run(language, func(t *testing.T) {
			s, root := setupTestServer(t)
			t.Cleanup(s.DrainBackground)
			file, hub, header, declaration, added, hubSource := "caller.go", "hub.go", "package main\n", "func CrossAnchor() int { return 1 }", "func CrossMarker() int { return Hub() }", "package main\nfunc Hub() int { return 2 }\n"
			if language == "typescript" {
				file = "caller.ts"
				hub = "hub.ts"
				header = "import { Hub } from \"./hub\";\n"
				declaration = "export function CrossAnchor(): number { return 1; }"
				added = "export function CrossMarker(): number { return Hub(); }"
				hubSource = "export function Hub(): number { return 2; }\n"
			}
			if language == "python" {
				file = "caller.py"
				hub = "hub.py"
				header = "from hub import Hub\n"
				declaration = "def CrossAnchor():\n    return 1"
				added = "def CrossMarker():\n    return Hub()"
				hubSource = "def Hub():\n    return 2\n"
			}
			require.NoError(t, os.WriteFile(filepath.Join(root, hub), []byte(hubSource), 0600))
			require.NoError(t, s.indexer.IndexFile(filepath.Join(root, hub)))
			require.NoError(t, os.WriteFile(filepath.Join(root, file), []byte(header+declaration+"\n"), 0600))
			require.NoError(t, s.indexer.IndexFile(filepath.Join(root, file)))
			s.indexer.ResolveAll()
			s.facades.capture(mcpgo.NewTool("edit_symbol"), s.handleEditSymbol)
			req := editOperationRequest("edit", "symbol")
			req.Params.Arguments = map[string]any{"operation": "symbol", "target": map[string]any{"symbol": file + "::CrossAnchor"}, "match": declaration, "replacement": declaration + "\n" + added, "output": map[string]any{"format": "json"}}
			result, err := s.boundToolHandler(func(ctx context.Context, r mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
				return s.handleFacade(ctx, "edit", r)
			})(t.Context(), req)
			require.NoError(t, err)
			require.False(t, result.IsError)
			chain := callTool(t, s, "get_call_chain", map[string]any{"id": file + "::CrossMarker", "depth": 1, "limit": 1000})
			require.False(t, chain.IsError)
			body := unmarshalResult(t, chain)
			edges, _ := body["edges"].([]any)
			found := false
			for _, raw := range edges {
				edge := raw.(map[string]any)
				if edge["from"] == file+"::CrossMarker" && edge["to"] == hub+"::Hub" && edge["kind"] == "calls" {
					found = true
				}
			}
			require.True(t, found, "actual public edit must publish exact crossfile Hub edge")
		})
	}
}
