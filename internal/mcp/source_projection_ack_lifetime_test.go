package mcp

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// Exercise the original operation's real acknowledgement owner, not a direct
// leaf call which has no original-RPC lifetime. Graph publication and physical
// bytes are real; neither a completed ticket nor cached exact=true is proof.
func TestOriginalFullAcknowledgementSurvivesVerifiedProjectionRetirement(t *testing.T) {
	for _, mode := range []string{"", "full"} {
		t.Run("mode="+mode, func(t *testing.T) {
			previous := pendingFileProjections
			registry := newPendingProjections()
			pendingFileProjections = registry
			t.Cleanup(func() { pendingFileProjections = previous })
			s, root := ackModeServer(t, mode)
			path := filepath.Join(root, "main.go")
			args := map[string]any{"path": path, "old_string": "func helper() {}", "new_string": "func helper() {}\nfunc OriginalFullPublished() {}", "mutation_id": "original-full-exact", "format": "json"}
			result, err := s.boundToolHandler(s.handleEditFile)(t.Context(), editFileRequest(args))
			require.NoError(t, err)
			require.False(t, result.IsError, "%v", result.Content)
			response := extractTextResult(t, result)
			require.Equal(t, "full", response["ack_mode"])
			require.Equal(t, true, response["reindexed"], "full must still wait for actual canonical publication")
			require.Equal(t, "fresh", response["graph_status"])
			raw, err := os.ReadFile(path)
			require.NoError(t, err)
			written := sha256.Sum256(raw)
			require.Equal(t, hex.EncodeToString(written[:]), response["content_sha256"])
			require.Contains(t, string(raw), "func OriginalFullPublished() {}")
			require.NotNil(t, s.graph.GetNode("main.go::OriginalFullPublished"))
			require.Equal(t, true, response["readback_exact"], "verified retirement must not consume the original ACK's still-valid exact file proof")
			requireMutationFileViewState(t, response, "projected")
			require.Nil(t, liveEntry(registry, path), "the original operation must release its bounded hold before returning")
			require.Positive(t, registry.retired.Load(), "verified retirement is deferred, not disabled")
		})
	}
}
