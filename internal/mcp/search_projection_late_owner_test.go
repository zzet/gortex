package mcp

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
)

func TestPendingSearchPublicResponseRefusesLatePhysicalSupersession(t *testing.T) {
	for _, facade := range []bool{false, true} {
		name := "legacy"
		if facade {
			name = "facade"
		}
		t.Run(name, func(t *testing.T) {
			previous := pendingFileProjections
			pendingFileProjections = newPendingProjections()
			t.Cleanup(func() { pendingFileProjections = previous })
			server, root := ackModeServer(t, "local")
			path := filepath.Join(root, "main.go")
			raw, err := os.ReadFile(path)
			require.NoError(t, err)
			raw = append(raw, []byte("\nfunc LateSearchOwner() {}\n")...)
			require.NoError(t, os.WriteFile(path, raw, 0o600))
			_, err = server.ensureProjected(t.Context(), []string{"main.go"})
			require.NoError(t, err)
			require.NotNil(t, liveEntry(pendingFileProjections, path))
			require.Empty(t, server.laggingMutationReceipts(t.Context()), "quiet read-built projections have no pending publication receipt")
			require.Nil(t, server.captureToolFreshness(t.Context()).Value(requestProjectionFactsKey{}), "prove the ordinary freshness capture does not already allocate shared projection facts")
			leafRan := false
			leaf := func(ctx context.Context, request mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
				result, readErr := server.handleSearchSymbols(ctx, request)
				require.NoError(t, readErr)
				require.False(t, result.IsError, "%v", result.Content)
				rows, _ := extractTextResult(t, result)["results"].([]any)
				found := false
				for _, row := range rows {
					found = found || row.(map[string]any)["id"] == "main.go::LateSearchOwner"
				}
				require.True(t, found, "the actual leaf must have selected the pending symbol before supersession")
				leafRan = true
				held, release, lockErr := acquireMutationPath(t.Context(), path)
				require.NoError(t, lockErr)
				commit, writeErr := server.commitFileMutation(held, "write_file", "", "", "main.go", path, raw, 0o600)
				release()
				require.NoError(t, writeErr)
				require.Positive(t, commit.commitSeq, "actual same-byte newer physical execution")
				return result, nil
			}
			var result *mcplib.CallToolResult
			if facade {
				captured, found := server.facades.legacy("search_symbols")
				require.True(t, found)
				server.facades.capture(captured.tool, leaf)
				sessionID := "late-facade-source-owner"
				server.NoteSessionToolPolicy(sessionID, FacadeSurfaceVersion, "full")
				ctx := WithSessionID(t.Context(), sessionID)
				result = callToolByName(t, server, ctx, "search", map[string]any{"operation": "symbols", "query": "LateSearchOwner", "format": "json"})
			} else {
				request := mcplib.CallToolRequest{}
				request.Params.Name = "search_symbols"
				request.Params.Arguments = map[string]any{"query": "LateSearchOwner", "query_class": "symbol", "expand": "off", "max_bytes": 0, "format": "json"}
				result, err = server.wrapToolHandler(leaf)(t.Context(), request)
				require.NoError(t, err)
			}
			require.True(t, leafRan, "the actual dispatcher must reach its leaf: %v", result.Content)
			require.True(t, result.IsError, "the entire public response must refuse ownership superseded between leaf and decoration: %v", result.Content)
			require.Contains(t, toolResultText(result), "superseded")
		})
	}
}
