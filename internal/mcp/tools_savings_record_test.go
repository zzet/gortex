package mcp

import (
	"context"
	"encoding/json"
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/savings"
)

// TestReadFamilyToolsRecordSavings pins the savings recording surface on a
// single-repo server (the issue-67 shape: one tracked repo, unprefixed
// nodes). Every read-family tool — read_file, get_file_summary,
// get_editing_context, get_symbol_source, batch_symbols, smart_context —
// must book an observation on a transferring call, and must book nothing
// when if_none_match hits. Before the lone-repo resolution fix none of
// the original four could, because the record sites sit behind
// resolveNodePath/resolveFilePath.
func TestReadFamilyToolsRecordSavings(t *testing.T) {
	srv, _, _ := newSingleRepoServer(t)
	ctx := context.Background()

	store, err := savings.Open("")
	require.NoError(t, err)
	srv.InitSavings(store, "")

	calls := func() int64 {
		return srv.tokenStats.snapshot()["calls_counted"].(int64)
	}
	require.Equal(t, int64(0), calls())

	etagOf := func(raw string) string {
		var parsed struct {
			ETag string `json:"etag"`
		}
		require.NoError(t, json.Unmarshal([]byte(raw), &parsed))
		require.NotEmpty(t, parsed.ETag)
		return parsed.ETag
	}

	recordThenWarmPoll := func(name string, args map[string]any) {
		t.Helper()
		before := calls()
		res := callToolByName(t, srv, ctx, name, args)
		require.False(t, res.IsError, "%s must succeed: %s", name, textOfResult(t, res))
		afterTransfer := calls()
		require.Greater(t, afterTransfer, before, "%s must record a savings observation", name)

		replay := make(map[string]any, len(args)+1)
		for k, v := range args {
			replay[k] = v
		}
		replay["if_none_match"] = etagOf(textOfResult(t, res))
		res = callToolByName(t, srv, ctx, name, replay)
		require.False(t, res.IsError, "not-modified %s must succeed: %s", name, textOfResult(t, res))
		require.Equal(t, afterTransfer, calls(), "not-modified %s must not record", name)
	}

	recordThenWarmPoll("read_file", map[string]any{"path": "main.go"})
	recordThenWarmPoll("get_file_summary", map[string]any{"path": "main.go"})

	res := callToolByName(t, srv, ctx, "get_editing_context", map[string]any{"path": "main.go"})
	require.False(t, res.IsError)
	require.Equal(t, int64(3), calls(), "get_editing_context must record a savings observation")

	recordThenWarmPoll("get_symbol_source", map[string]any{"id": "main.go::Hello"})

	batchArgs := map[string]any{
		"ids":            []any{"myrepo/main.go::Hello"},
		"include_source": true,
	}
	// batch_symbols walks callers/callees with Detail:"brief", which
	// ends in query.stripMeta. On graph.New() that nils Meta on the
	// stored nodes, so the first payload includes Hello's signature and
	// the next does not. SQLite GetNode returns a copy, so a production
	// daemon is unaffected. Prime once so the baseline ETag is stable.
	prime := callToolByName(t, srv, ctx, "batch_symbols", batchArgs)
	require.False(t, prime.IsError, "batch_symbols prime must succeed: %s", textOfResult(t, prime))
	recordThenWarmPoll("batch_symbols", batchArgs)
	recordThenWarmPoll("smart_context", map[string]any{"task": "Hello"})

	snap := srv.tokenStats.snapshot()
	require.Greater(t, snap["tokens_returned"].(int64), int64(0))

	// Single-repo events attribute to the lone repo's prefix — the same
	// bucket key multi-repo mode would use — for every recording tool.
	ledger, lerr := store.Snapshot()
	require.NoError(t, lerr)
	require.NotNil(t, ledger.PerRepo["myrepo"], "events must land in the lone repo's per-repo bucket, got %v", ledger.PerRepo)
	require.Equal(t, calls(), ledger.PerRepo["myrepo"].CallsCounted)
}

// textOfResult extracts the first text content of a tool result.
func textOfResult(t *testing.T, res *mcplib.CallToolResult) string {
	t.Helper()
	for _, c := range res.Content {
		if tc, ok := c.(mcplib.TextContent); ok {
			return tc.Text
		}
	}
	t.Fatal("tool result has no text content")
	return ""
}
