package mcp

import (
	"context"
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
				if operation == "sibling" || operation == "append_leaf" {
					require.NotNil(t, s.engineFor(t.Context()).GetSymbol(id))
					require.NotNil(t, s.engineFor(t.Context()).GetSymbol(file+"::Added"))
				} else if operation == "modify_body" || operation == "change_signature" {
					afterNode := s.engineFor(t.Context()).GetSymbol(id)
					require.NotNil(t, afterNode)
					require.NotNil(t, s.engineFor(t.Context()).GetSymbol(file+"::Caller"), "unchanged zero-argument caller must survive")
					sourceResult := callTool(t, s, "get_symbol_source", map[string]any{"id": id})
					require.False(t, sourceResult.IsError)
					sourceBody := unmarshalResult(t, sourceResult)
					require.Equal(t, strings.TrimSpace(replacement), strings.TrimSpace(sourceBody["source"].(string)))
					if operation == "modify_body" {
						require.Greater(t, afterNode.EndLine-afterNode.StartLine, beforeSpan)
					}
					if operation == "change_signature" {
						require.Contains(t, afterNode.Meta["signature"], "optional")
					}
				} else {
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
						require.Nil(t, chainBody["nodes"])
						require.Nil(t, chainBody["edges"])
						require.Zero(t, chainBody["total_nodes"])
						require.Zero(t, chainBody["total_edges"])
						require.Equal(t, false, chainBody["truncated"])
						search := callTool(t, s, "search_symbols", map[string]any{"query": "GortexDeletedMeasurementNoSuchSymbol"})
						require.False(t, search.IsError)
						searchBody := unmarshalResult(t, search)
						require.Contains(t, searchBody, "results")
						require.Nil(t, searchBody["results"])
						require.Zero(t, searchBody["total"])
						require.Equal(t, false, searchBody["truncated"])
					}

				}
			})
		}
	}
}
