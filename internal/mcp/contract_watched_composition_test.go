package mcp

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/indexer"
)

func TestContractConsumerWatchedFreshnessComposition(t *testing.T) {
	t.Run("required_and_optional_core", func(t *testing.T) {
		f := newContractConsumerFixture(t)
		f.publish(t, nil, "", "/after")
		fake := installForbiddenContractRuntime(t, f.srv)
		f.srv.SetWatcher(&watchedTierFixture{paths: map[string]indexer.MutationTierState{
			filepath.Join(f.root, "repo", "handler.go"): {Generation: 1, AckAt: time.Now()},
		}})
		result, err := f.request(t.Context(), "graph_query", map[string]any{
			"query": "nodes name~Handle | traverse provides,calls both", "format": "json",
			requiredCapabilitiesArgName: string(graphview.CapContracts),
		}, f.srv.handleGraphQuery)
		body := requireContractBody(t, result, err)
		require.Contains(t, body, "InvokeCaller", "required analysis preserves canonical caller edges")
		require.Contains(t, body, "http::GET::/after")
		require.NotContains(t, body, "/before")
		captures := fake.captures.Load()
		result, err = f.request(t.Context(), "search_symbols", map[string]any{"query": "Handle", "format": "json"}, f.srv.handleSearchSymbols)
		body = requireContractBody(t, result, err)
		require.Contains(t, body, "repo/handler.go::Handle")
		require.NotContains(t, body, "contract_analysis")
		require.Equal(t, captures, fake.captures.Load(), "ordinary core search must not capture optional analysis")
		require.Zero(t, fake.schedules.Load())
		require.Zero(t, fake.waits.Load())
	})
	for _, canceled := range []bool{false, true} {
		t.Run(fmt.Sprintf("relation_canceled_%t", canceled), func(t *testing.T) {
			s, root := newRelationFixture(t)
			fake := installForbiddenContractRuntime(t, s)
			s.SetWatcher(&watchedTierFixture{paths: map[string]indexer.MutationTierState{
				filepath.Join(root, "caller.go"): {Generation: 1, AckAt: time.Now()},
			}})
			req := mcplib.CallToolRequest{}
			req.Params.Name = "verify_change"
			req.Params.Arguments = map[string]any{
				"changes":       fmt.Sprintf(`[{"symbol_id":%q,"new_signature":"func Target()"}]`, targetID(t, s)),
				"require_fresh": true, "wait_deadline": time.Now().Add(250 * time.Millisecond).Format(time.RFC3339Nano), "format": "json",
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if canceled {
				cancel()
			}
			result, err := s.wrapToolHandler(s.handleVerifyChange)(ctx, req)
			if canceled {
				require.ErrorIs(t, err, context.Canceled)
				require.Nil(t, result)
			} else {
				require.NoError(t, err)
				require.False(t, result.IsError)
				body := freshnessToolResultText(result)
				require.Contains(t, body, "pending")
				require.Contains(t, body, "checked_callers", "scoped deadline still runs the verdict handler")
			}
			require.Zero(t, fake.captures.Load())
			require.Zero(t, fake.schedules.Load())
			require.Zero(t, fake.waits.Load())
		})
	}
}
