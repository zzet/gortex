package mcp

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/query"
)

type handlerBudgetStore struct {
	graph.Store
	budgetRead  bool
	legacyReads int
}

func (s *handlerBudgetStore) WithFileEditingContextReader(ctx context.Context, _ func(context.Context, graph.FileEditingContextRows) error) error {
	deadline, ok := ctx.Deadline()
	s.budgetRead = ok && time.Until(deadline) <= 30*time.Millisecond
	<-ctx.Done()
	return ctx.Err()
}

func (s *handlerBudgetStore) GetFileNodes(path string) []*graph.Node {
	s.legacyReads++
	return s.Store.GetFileNodes(path)
}

func TestEditingContextStoreDeadlineKeepsProjectedLocalRowsPending(t *testing.T) {
	for _, fileReason := range []string{"", "storm", "external_write"} {
		t.Run("file_reason_"+fileReason, func(t *testing.T) {
			selectedProjectionRegistry(t)
			s, root := setupTestServer(t)
			path := filepath.Join(root, "main.go")
			raw, err := os.ReadFile(path)
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(path, append(raw, []byte("\nfunc StoreBusyLocal() {}\n")...), 0600))
			ctx, err := s.ensureProjected(t.Context(), []string{"main.go"})
			require.NoError(t, err)
			require.Len(t, requestProjectionLayers(ctx), 1)
			if fileReason != "" {
				markRequestProjectionDeferred(ctx, "main.go", fileReason)
			}
			store := &handlerBudgetStore{Store: s.graph}
			s.graph = store
			s.engine = query.NewEngine(store)
			req := mcplib.CallToolRequest{}
			req.Params.Name = "get_editing_context"
			req.Params.Arguments = map[string]any{"path": "main.go", "format": "json"}
			result, err := s.handleGetEditingContext(ctx, req)
			require.NoError(t, err)
			require.False(t, result.IsError, "a store deadline must retain checked local rows: %s", resultText(result))
			fields := extractTextResult(t, result)
			found := false
			for _, value := range fields["defines"].([]any) {
				if value.(map[string]any)["name"] == "StoreBusyLocal" {
					found = true
				}
			}
			require.True(t, found, "the actual handler lost its already selected file-local projection")
			require.True(t, store.budgetRead, "the selected store must receive the existing 30ms budget")
			require.Zero(t, store.legacyReads, "timeout must not fall back to contextless store reads")
			// Apply the SAME existing producer result to this actual leaf response.
			// The test invents no alternate freshness fields or facts producer.
			s.freshnessFor(ctx, "relations", nil, []string{"main.go"}).apply(fields)
			require.Equal(t, "pending", fields["graph_freshness"])
			require.Equal(t, "store_busy", fields["reason"])
			require.Equal(t, false, fields["complete"])
			if fileReason != "" {
				require.Equal(t, fileReason, fields["file_reason"])
			}
		})
	}
}
