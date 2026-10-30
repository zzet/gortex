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
		for _, operation := range []string{"sibling", "rename", "delete"} {
			t.Run(language+"/"+operation, func(t *testing.T) {
				s, root := setupTestServer(t)
				t.Cleanup(s.DrainBackground)
				var file, header, declaration, survivor, added, renamed string
				switch language {
				case "go":
					file = "symbol-cell.go"
					header = "package main\n"
					declaration = "func Target() int { return 1 }"
					survivor = "func Sentinel() int { return 2 }"
					added = "func Added() int { return Target() }"
					renamed = "func Renamed() int { return 1 }"
				case "typescript":
					file = "symbol-cell.ts"
					declaration = "export function Target(): number { return 1; }"
					survivor = "export function Sentinel(): number { return 2; }"
					added = "export function Added(): number { return Target(); }"
					renamed = "export function Renamed(): number { return 1; }"
				case "python":
					file = "symbol-cell.py"
					declaration = "def Target():\n    return 1"
					survivor = "def Sentinel():\n    return 2"
					added = "def Added():\n    return Target()"
					renamed = "def Renamed():\n    return 1"
				}
				source := header + declaration + "\n\n" + survivor + "\n"
				path := filepath.Join(root, file)
				require.NoError(t, os.WriteFile(path, []byte(source), 0600))
				require.NoError(t, s.indexer.IndexFile(path))
				id := file + "::Target"
				require.NotNil(t, s.engineFor(t.Context()).GetSymbol(id), "fixture must start with actual producer-owned symbol")
				replacement := declaration + "\n" + added
				if operation == "rename" {
					replacement = renamed
				}
				if operation == "delete" {
					replacement = ""
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
				if operation == "sibling" {
					require.NotNil(t, s.engineFor(t.Context()).GetSymbol(id))
					require.NotNil(t, s.engineFor(t.Context()).GetSymbol(file+"::Added"))
				} else {
					require.Nil(t, s.engineFor(t.Context()).GetSymbol(id), "old identity must really be absent, not an empty successful source")
					missing := callTool(t, s, "get_symbol_source", map[string]any{"id": id})
					require.True(t, missing.IsError, "expected absence is an explicit producer refusal")
					require.Contains(t, mustErrText(t, missing), id)
					if operation == "rename" {
						require.NotNil(t, s.engineFor(t.Context()).GetSymbol(file+"::Renamed"))
					}
				}
			})
		}
	}
}
