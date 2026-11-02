package mcp

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPendingSearchCachedSelectionRefusesNewPhysicalOwner(t *testing.T) {
	for _, sameBytes := range []bool{false, true} {
		name := "new names"
		if sameBytes {
			name = "same bytes new physical owner"
		}
		t.Run(name, func(t *testing.T) {
			previous := pendingFileProjections
			pendingFileProjections = newPendingProjections()
			t.Cleanup(func() { pendingFileProjections = previous })
			server, root := ackModeServer(t, "local")
			searchProjectedAck(t, server, root, "BorrowedSearchOne")
			pinned, err := server.bindPendingSearch(t.Context())
			require.NoError(t, err)
			path := filepath.Join(root, "main.go")
			if sameBytes {
				raw, readErr := os.ReadFile(path)
				require.NoError(t, readErr)
				held, release, lockErr := acquireMutationPath(t.Context(), path)
				require.NoError(t, lockErr)
				commit, writeErr := server.commitFileMutation(held, "write_file", "", "", "main.go", path, raw, 0o600)
				release()
				require.NoError(t, writeErr)
				require.Positive(t, commit.commitSeq)
			} else {
				result := callToolByName(t, server, t.Context(), "edit_file", map[string]any{"path": path, "old_string": "BorrowedSearchOne", "new_string": "BorrowedSearchTwo", "format": "json"})
				require.False(t, result.IsError, "%v", result.Content)
			}
			// An already selected request may not silently mix its old candidates with
			// a newer physical owner's rows, even when every source byte is unchanged.
			result := callToolByName(t, server, pinned, "search_symbols", map[string]any{"query": "BorrowedSearchOne", "query_class": "symbol", "expand": "off", "limit": 1000, "max_bytes": 0, "format": "json"})
			require.True(t, result.IsError, "superseded selection must refuse the whole public response: %v", result.Content)
			require.Contains(t, toolResultText(result), "superseded")
			currentName := "BorrowedSearchTwo"
			if sameBytes {
				currentName = "BorrowedSearchOne"
			}
			current := callToolByName(t, server, t.Context(), "search_symbols", map[string]any{"query": currentName, "query_class": "symbol", "expand": "off", "limit": 1000, "max_bytes": 0, "format": "json"})
			require.False(t, current.IsError, "%v", current.Content)
			rows, _ := extractTextResult(t, current)["results"].([]any)
			found := false
			for _, raw := range rows {
				found = found || raw.(map[string]any)["id"] == "main.go::"+currentName
			}
			require.True(t, found)
		})
	}
}
