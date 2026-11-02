package mcp

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/indexer"
)

func TestFreshnessResponseContractNormalizesFileReadPaths(t *testing.T) {
	for _, tool := range []string{"read_file", "get_file_summary", "get_editing_context"} {
		t.Run(tool, func(t *testing.T) {
			s, root := newRelationFixture(t)
			path := filepath.Join(root, "target.go")
			s.SetWatcher(&watchedTierFixture{paths: map[string]indexer.MutationTierState{
				path: {Generation: 1, AckAt: time.Now()},
			}})
			handlers := map[string]func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error){
				"read_file":           s.handleReadFile,
				"get_file_summary":    s.handleGetFileSummary,
				"get_editing_context": s.handleGetEditingContext,
			}
			for _, target := range []string{"target.go", path} {
				t.Run(target, func(t *testing.T) {
					req := mcp.CallToolRequest{}
					req.Params.Name = tool
					req.Params.Arguments = map[string]any{"path": target, "format": "json"}
					res, err := s.wrapToolHandler(handlers[tool])(t.Context(), req)
					require.NoError(t, err)
					require.False(t, res.IsError, resultText(res))
					payload := resultJSON(t, res)
					freshness, ok := payload["freshness"].(map[string]any)
					require.True(t, ok, "pending file read lacks freshness: %s", resultText(res))
					require.Equal(t, "pending", freshness["graph_freshness"])
					require.Equal(t, "file", freshness["freshness_scope"])
					require.EqualValues(t, 1, freshness["pending_file_count"])
					require.Equal(t, []any{"target.go"}, freshness["pending_files"])
				})
			}
		})
	}
}

func TestFreshnessResponseContractRequireFreshRefusalNamesSelectedTier(t *testing.T) {
	for _, scope := range []string{"file", "relations", "derived"} {
		t.Run(scope, func(t *testing.T) {
			s, root := newRelationFixture(t)
			s.SetWatcher(&watchedTierFixture{paths: map[string]indexer.MutationTierState{
				filepath.Join(root, "target.go"): {Generation: 1, AckAt: time.Now()},
			}})
			req := mcp.CallToolRequest{}
			req.Params.Name = "read_file"
			req.Params.Arguments = map[string]any{
				"path": "target.go", "require_fresh": true, "freshness_scope": scope,
				"wait_deadline": time.Now().Add(50 * time.Millisecond).Format(time.RFC3339Nano),
			}
			res, err := s.wrapToolHandler(s.handleReadFile)(t.Context(), req)
			require.NoError(t, err)
			require.True(t, res.IsError, "pending %s tier must refuse require_fresh", scope)
			require.Contains(t, resultText(res), "require_fresh: "+scope+": pending (1 files)")
		})
	}
}
