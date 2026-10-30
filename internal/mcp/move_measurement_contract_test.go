package mcp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
)

func TestMoveMeasurementUsesActualGoRelocationAndExplicitLanguageRefusal(t *testing.T) {
	for _, language := range []string{"go", "typescript", "python"} {
		t.Run(language, func(t *testing.T) {
			s, root := setupTestServer(t)
			t.Cleanup(s.DrainBackground)
			file, destination, source, target := "move_source.go", "move_target.go", "package main\nfunc Relocated() int { return 7 }\nfunc StableCaller() int { return Relocated() }\n", "package main\nfunc DestinationAnchor() int { return 2 }\n"
			if language == "typescript" {
				file = "move_source.ts"
				destination = "move_target.ts"
				source = "export function Relocated(): number { return 7; }\n"
				target = "export function DestinationAnchor(): number { return 2; }\n"
			}
			if language == "python" {
				file = "move_source.py"
				destination = "move_target.py"
				source = "def Relocated():\n    return 7\n"
				target = "def DestinationAnchor():\n    return 2\n"
			}
			sourcePath, targetPath := filepath.Join(root, file), filepath.Join(root, destination)
			require.NoError(t, os.WriteFile(sourcePath, []byte(source), 0600))
			require.NoError(t, os.WriteFile(targetPath, []byte(target), 0600))
			require.NoError(t, s.indexer.IndexFile(sourcePath))
			require.NoError(t, s.indexer.IndexFile(targetPath))
			s.indexer.ResolveAll()
			oldID, newID := file+"::Relocated", destination+"::Relocated"
			require.NotNil(t, s.engineFor(t.Context()).GetSymbol(oldID), "language refusal must operate on an actual indexed declaration")
			s.facades.capture(mcpgo.NewTool("move_symbol"), s.handleMoveSymbol)
			req := editOperationRequest("refactor", "move")
			req.Params.Arguments = map[string]any{"operation": "move", "target": map[string]any{"symbol": oldID}, "destination": destination, "output": map[string]any{"format": "json"}}
			result, err := s.boundToolHandler(func(ctx context.Context, r mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
				return s.handleFacade(ctx, "refactor", r)
			})(t.Context(), req)
			require.NoError(t, err)
			if language != "go" {
				require.True(t, result.IsError)
				require.Contains(t, mustErrText(t, result), "unsupported language: move_symbol only supports Go files")
				actualSource, err := os.ReadFile(sourcePath)
				require.NoError(t, err)
				require.Equal(t, source, string(actualSource))
				actualTarget, err := os.ReadFile(targetPath)
				require.NoError(t, err)
				require.Equal(t, target, string(actualTarget))
				require.NotNil(t, s.engineFor(t.Context()).GetSymbol(oldID))
				require.Nil(t, s.engineFor(t.Context()).GetSymbol(newID))
				return
			}
			require.False(t, result.IsError)
			body := unmarshalResult(t, result)
			require.Equal(t, oldID, body["source_id"])
			require.Equal(t, newID, body["target_id"])
			require.NotContains(t, body, "mutation_receipt", "multi-file relocation must not be represented as one physical edit receipt")
			require.Nil(t, s.engineFor(t.Context()).GetSymbol(oldID))
			require.NotNil(t, s.engineFor(t.Context()).GetSymbol(newID))
			oldSource := callTool(t, s, "get_symbol_source", map[string]any{"id": oldID})
			require.True(t, oldSource.IsError)
			newSource := callTool(t, s, "get_symbol_source", map[string]any{"id": newID, "context_lines": 0})
			require.False(t, newSource.IsError)
			require.Contains(t, unmarshalResult(t, newSource)["source"], "return 7")
			chain := callTool(t, s, "get_call_chain", map[string]any{"id": file + "::StableCaller", "depth": 1, "limit": 1000})
			require.False(t, chain.IsError)
			found := false
			for _, raw := range unmarshalResult(t, chain)["edges"].([]any) {
				edge := raw.(map[string]any)
				if edge["from"] == file+"::StableCaller" && edge["to"] == newID && edge["kind"] == "calls" {
					found = true
				}
				require.NotEqual(t, oldID, edge["to"], "caller retained old location")
			}
			rawOut, _ := json.Marshal(s.graph.GetOutEdges(file + "::StableCaller"))
			require.True(t, found, "unchanged Go caller must resolve new location; chain=%v raw_out=%s", unmarshalResult(t, chain), string(rawOut))
		})
	}
}

func TestMoveMeasurementActualMultiIndexerPreservesUnchangedCaller(t *testing.T) {
	s, _ := setupContractMultiRepoServer(t, "fixture")
	t.Cleanup(func() {
		s.DrainBackground()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		require.NoError(t, s.multiIndexer.Close(ctx))
	})
	root, ok := s.multiIndexer.RepoRoot("fixture")
	require.True(t, ok)
	sourcePath, targetPath := filepath.Join(root, "move_source.go"), filepath.Join(root, "move_target.go")
	require.NoError(t, os.WriteFile(sourcePath, []byte("package main\nfunc Relocated() int { return 7 }\nfunc StableCaller() int { return Relocated() }\n"), 0600))
	require.NoError(t, os.WriteFile(targetPath, []byte("package main\nfunc DestinationAnchor() int { return 2 }\n"), 0600))
	_, err := s.multiIndexer.IncrementalReindexRepo("fixture", []string{sourcePath, targetPath})
	require.NoError(t, err)
	oldID, newID, callerID := "fixture/move_source.go::Relocated", "fixture/move_target.go::Relocated", "fixture/move_source.go::StableCaller"
	require.NotNil(t, s.engineFor(t.Context()).GetSymbol(callerID), "actual caller must exist before move")
	s.facades.capture(mcpgo.NewTool("move_symbol"), s.handleMoveSymbol)
	req := editOperationRequest("refactor", "move")
	req.Params.Arguments = map[string]any{"operation": "move", "target": map[string]any{"symbol": oldID}, "destination": "fixture/move_target.go", "output": map[string]any{"format": "json"}}
	result, err := s.boundToolHandler(func(ctx context.Context, r mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		return s.handleFacade(ctx, "refactor", r)
	})(t.Context(), req)
	require.NoError(t, err)
	require.False(t, result.IsError)
	source, err := os.ReadFile(sourcePath)
	require.NoError(t, err)
	require.Contains(t, string(source), "func StableCaller()")
	require.Nil(t, s.engineFor(t.Context()).GetSymbol(oldID))
	require.NotNil(t, s.engineFor(t.Context()).GetSymbol(newID))
	chain := callTool(t, s, "get_call_chain", map[string]any{"id": callerID, "depth": 1, "limit": 1000})
	require.False(t, chain.IsError)
	body := unmarshalResult(t, chain)
	edges, _ := body["edges"].([]any)
	found := false
	for _, raw := range edges {
		edge := raw.(map[string]any)
		if edge["from"] == callerID && edge["to"] == newID && edge["kind"] == "calls" {
			found = true
		}
	}
	caller, _ := json.Marshal(s.graph.GetNode(callerID))
	rawOut, _ := json.Marshal(s.graph.GetOutEdges(callerID))
	require.True(t, found, "multi move chain=%v caller=%s raw_out=%s source=%s", body, string(caller), string(rawOut), string(source))
}
