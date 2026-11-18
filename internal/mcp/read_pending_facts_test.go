package mcp

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestStoreBusyFactsRemainScopedAndKeepFileReason(t *testing.T) {
	s, _ := setupTestServer(t)
	ctx := markRequestStoreBusy(t.Context(), "main.go")
	ctx = markRequestStoreBusy(ctx, "main.go")
	outside := s.freshnessFor(ctx, "relations", nil, []string{"other.go"})
	require.Empty(t, outside.state, "a failed store section is not workspace-wide freshness evidence")
	require.False(t, outside.storeBusy)
	observed := s.freshnessFor(ctx, "relations", nil, []string{"main.go"})
	fields := map[string]any{}
	observed.apply(fields)
	require.Equal(t, "pending", fields["graph_freshness"])
	require.Equal(t, "store_busy", fields["reason"])
	require.Equal(t, false, fields["complete"])
	require.Equal(t, 1, fields["pending_file_count"], "duplicate section failures retain one graph-keyed pending file")
	require.NotContains(t, fields, "readback_exact")
	markRequestProjectionStorm(ctx, "main.go")
	observed = s.freshnessFor(ctx, "relations", nil, []string{"main.go"})
	fields = map[string]any{}
	observed.apply(fields)
	require.Equal(t, "storm", fields["file_reason"])
	require.Equal(t, "store_busy", fields["reason"], "a missing store section must not erase the independently observed file deferral")
}
