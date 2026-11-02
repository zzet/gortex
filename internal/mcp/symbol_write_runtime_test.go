package mcp

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
)

func TestSymbolWritersUseCurrentDeclarationAfterLineShift(t *testing.T) {
	for _, tool := range []string{"edit_symbol", "safe_delete_symbol"} {
		t.Run(tool, func(t *testing.T) {
			original := strings.Replace(repeatedFragmentSource, "// second documents itself.\n", "", 1)
			srv, root := setupMoveInlineRepo(t, map[string]string{"sample.go": original})
			srv.SetWatcher(mutationTestWatcher{})
			shifted := strings.Replace(original, "func first", "// a\n// b\n// c\nfunc first", 1)
			path := filepath.Join(root, "sample.go")
			require.NoError(t, os.WriteFile(path, []byte(shifted), 0o644))
			req := editFileRequest(map[string]any{
				"id": "sample.go::second", "old_source": "\treturn nil", "new_source": "\treturn errSecond", "force": true, "dry_run": false, "format": "json",
			})
			req.Params.Name = tool
			var result *mcp.CallToolResult
			var callErr error
			if tool == "edit_symbol" {
				ctx, ack, _ := srv.withEditAcknowledgement(t.Context())
				defer ack.finishActual()
				result, callErr = srv.handleEditSymbol(ctx, req)
				ack.finishReply(result, callErr)
				ack.finishActual()
				observed := ack.snapshot()
				require.EqualValues(t, 1, observed.ParseGate.Started, "actual source relocation extraction must be observed")
				require.EqualValues(t, 1, observed.ParseGate.Completed)
				require.True(t, observed.LapsDisjoint)
			} else {
				result, callErr = srv.handleSafeDeleteSymbol(t.Context(), req)
			}
			require.NoError(t, callErr)
			require.False(t, result.IsError, "%v", result.Content)
			current, err := os.ReadFile(path)
			require.NoError(t, err)
			first, _, found := strings.Cut(string(current), "func second")
			require.Contains(t, first, "func first")
			require.Contains(t, first, "\treturn nil", "the repeated fragment in the other declaration must survive")
			if tool == "edit_symbol" {
				require.True(t, found)
				require.NotContains(t, first, "errSecond")
				require.Contains(t, string(current), "\treturn errSecond")
			} else {
				require.False(t, found, "delete must remove the current second declaration")
			}
		})
	}
}

func TestSafeDeleteRemovedDeclarationRefusesBeforeSourceWrite(t *testing.T) {
	original := strings.Replace(repeatedFragmentSource, "// second documents itself.\n", "", 1)
	srv, root := setupMoveInlineRepo(t, map[string]string{"sample.go": original})
	srv.SetWatcher(mutationTestWatcher{})
	removed := strings.Replace(original, "func second() error {\n\treturn nil\n}", "// declaration removed\n\n\n", 1)
	require.NotEqual(t, original, removed)
	path := filepath.Join(root, "sample.go")
	require.NoError(t, os.WriteFile(path, []byte(removed), 0o644))
	result, callErr := srv.handleSafeDeleteSymbol(t.Context(), editFileRequest(map[string]any{"id": "sample.go::second", "force": true, "dry_run": false, "format": "json"}))
	require.NoError(t, callErr)
	require.True(t, result.IsError, "a removed current declaration cannot donate its old span: %v", result.Content)
	current, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, removed, string(current))
	_, committed := srv.mutationCommits.recentForPath(path)
	require.False(t, committed, "refusal must precede physical source authority")
}
