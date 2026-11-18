package mcp

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
)

func TestRegisteredRenameOriginalFullAcknowledgementKeepsProjection(t *testing.T) {
	for _, tool := range []string{"rename_symbol", "refactor"} {
		t.Run(tool, func(t *testing.T) {
			previous := pendingFileProjections
			registry := newPendingProjections()
			pendingFileProjections = registry
			t.Cleanup(func() { pendingFileProjections = previous })
			s, root := setupRenameServer(t, renameTargetSrc, renameCallerSrc)
			t.Cleanup(s.indexer.Close)
			session := "original-full-rename-" + tool
			require.NoError(t, s.ConnectSession(session, func([]byte) error { return nil }))
			t.Cleanup(func() { s.DisconnectSession(session) })
			ctx := s.WithClientSession(WithSessionID(t.Context(), session), session)
			if tool == "refactor" {
				s.NoteSessionToolPolicy(session, FacadeSurfaceVersion, "hide")
			} else {
				s.NoteSessionToolPolicy(session, "all", "defer")
			}
			require.NotNil(t, s.MCPServer().HandleMessage(ctx, []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"original-full-rename","version":"1"}}}`)))
			args := map[string]any{"id": "target.go::Target", "new_name": "RenamedFull", "dry_run": false, "format": "json"}
			if tool == "refactor" {
				args = map[string]any{"operation": "rename", "target": map[string]any{"symbol": "target.go::Target"}, "new_name": "RenamedFull", "dry_run": false}
			}
			shape := mcplib.CallToolRequest{}
			shape.Params.Name, shape.Params.Arguments = tool, args
			require.False(t, measuredSourceEdit(shape), "rename must not broaden the five measured edit surfaces")
			result := preselectionRegisteredCall(t, s, ctx, 2, tool, args)
			require.False(t, result.IsError, resultText(result))
			response := extractTextResult(t, result)
			require.Equal(t, "applied", response["status"])
			files, ok := response["files"].([]any)
			require.True(t, ok)
			require.Len(t, files, 2)
			for _, value := range files {
				file := value.(map[string]any)
				require.Equal(t, "full", file["ack_mode"])
				require.Equal(t, "fresh", file["graph_status"])
				raw, err := os.ReadFile(filepath.Join(root, file["file"].(string)))
				require.NoError(t, err)
				sum := sha256.Sum256(raw)
				require.Equal(t, hex.EncodeToString(sum[:]), file["content_sha256"])
				require.Contains(t, string(raw), "RenamedFull")
				require.Equal(t, true, file["readback_exact"], "registered raw and facade rename need the same original-operation projection lifetime")
				requireMutationFileViewState(t, file, "projected")
			}
			require.Empty(t, registry.snap.Load().entries)
			require.EqualValues(t, 2, registry.retired.Load())
			require.Zero(t, s.editOperations.completed.Load(), "projection ownership is independent of measured-edit counters")
		})
	}
}
