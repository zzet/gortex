package mcp

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
)

func TestSymbolFirstPageReusesOnlyAnIdenticalLiveSequence(t *testing.T) {
	srv, backend, req := symbolLifecycleServer(t)
	t.Cleanup(srv.DrainBackground)
	ctx, cancel := context.WithTimeout(WithSessionID(t.Context(), "first-page-reuse"), 5*time.Second)
	defer cancel()
	call := func(request mcplib.CallToolRequest) (*mcplib.CallToolResult, string) {
		t.Helper()
		result, err := srv.handleSearchSymbols(ctx, request)
		require.NoError(t, err)
		require.False(t, result.IsError)
		raw, err := json.Marshal(result)
		require.NoError(t, err)
		return result, string(raw)
	}
	first, firstWire := call(req)
	cursor := first.StructuredContent.(map[string]any)["next_cursor"].(string)
	continuation := req
	continuation.Params.Arguments = map[string]any{"query": "Needle", "limit": 4, "max_bytes": 0, "cursor": cursor}
	_, secondWire := call(continuation)
	before := len(backend.searchLimits())
	_, repeated := call(req)
	require.Equal(t, firstWire, repeated, "first-page replay includes exactly the same opaque cursor")
	require.Greater(t, len(backend.searchLimits()), before, "first-page reuse must still execute actual retrieval")
	_, secondReplay := call(continuation)
	require.Equal(t, secondWire, secondReplay, "restarting a query must not replace continuation progress")
	cache := srv.symbolPages(ctx)
	cache.mu.Lock()
	count := len(cache.entries)
	cache.mu.Unlock()
	require.Equal(t, 1, count)

	otherCtx := WithSessionID(ctx, "first-page-other-session")
	other, err := srv.handleSearchSymbols(otherCtx, req)
	require.NoError(t, err)
	require.False(t, other.IsError)
	require.NotEqual(t, cursor, other.StructuredContent.(map[string]any)["next_cursor"], "identical seeds must not share continuation ownership across sessions")
	require.NotSame(t, cache, srv.symbolPages(otherCtx))

	// Alter deeper retrieval without changing the graph's mutation identity.
	// An identical visible first page cannot authorize reuse of different tails.
	backend.mu.Lock()
	ids := append([]string(nil), backend.byToken["needle"]...)
	backend.mu.Unlock()
	replacement := newOrderedBackend()
	ids[8], ids[9] = ids[9], ids[8]
	replacement.put("needle", ids...)
	srv.engineFor(ctx).SetSearch(replacement)
	changed, changedWire := call(req)
	require.NotEqual(t, firstWire, changedWire, "the full candidate seed, including unread rows, determines reuse")
	require.NotEqual(t, cursor, changed.StructuredContent.(map[string]any)["next_cursor"])
	_, oldContinuation := call(continuation)
	require.Equal(t, secondWire, oldContinuation, "the earlier immutable sequence remains independently replayable")
}

func TestSymbolFirstPageConcurrentPublicationHasOneWinner(t *testing.T) {
	srv, _, req := symbolLifecycleServer(t)
	t.Cleanup(srv.DrainBackground)
	ctx, cancel := context.WithTimeout(WithSessionID(t.Context(), "first-page-concurrent"), 5*time.Second)
	defer cancel()
	type answer struct {
		result *mcplib.CallToolResult
		err    error
	}
	answers := make(chan answer, 8)
	start := make(chan struct{})
	var joined sync.WaitGroup
	for range cap(answers) {
		joined.Add(1)
		go func() {
			defer joined.Done()
			<-start
			result, err := srv.handleSearchSymbols(ctx, req)
			answers <- answer{result, err}
		}()
	}
	close(start)
	joined.Wait()
	close(answers)
	var expected []byte
	for answer := range answers {
		require.NoError(t, answer.err)
		require.NotNil(t, answer.result)
		require.False(t, answer.result.IsError)
		wire, err := json.Marshal(answer.result)
		require.NoError(t, err)
		if expected == nil {
			expected = wire
		}
		require.Equal(t, expected, wire)
	}
	cache := srv.symbolPages(ctx)
	cache.mu.Lock()
	count, ordered := len(cache.entries), len(cache.order)
	cache.mu.Unlock()
	require.Equal(t, 1, count)
	require.Equal(t, 1, ordered)
}

func TestSymbolFirstPageDoesNotReviveExpiredOrRetiredOwnership(t *testing.T) {
	for _, retirement := range []string{"expired", "session_release"} {
		t.Run(retirement, func(t *testing.T) {
			srv, _, req := symbolLifecycleServer(t)
			t.Cleanup(srv.DrainBackground)
			ctx := WithSessionID(t.Context(), "first-page-retirement")
			first, err := srv.handleSearchSymbols(ctx, req)
			require.NoError(t, err)
			require.False(t, first.IsError)
			cursor := first.StructuredContent.(map[string]any)["next_cursor"].(string)
			parsed, ok := parseSymbolCursor(cursor)
			require.True(t, ok)
			cache := srv.symbolPages(ctx)
			entry := cache.lookup(parsed.Sequence)
			require.NotNil(t, entry)
			if retirement == "expired" {
				cache.mu.Lock()
				entry.created = time.Now().Add(-symbolPageTTL - time.Second)
				cache.mu.Unlock()
			} else {
				cache.beginClose()()
				// A new session owner must not inherit the terminal cache.
				ctx = WithSessionID(t.Context(), "first-page-replacement")
			}
			second, err := srv.handleSearchSymbols(ctx, req)
			require.NoError(t, err)
			require.False(t, second.IsError)
			require.NotEqual(t, cursor, second.StructuredContent.(map[string]any)["next_cursor"])
			old := req
			old.Params.Arguments = map[string]any{"query": "Needle", "limit": 4, "max_bytes": 0, "cursor": cursor}
			refusal, err := srv.handleSearchSymbols(ctx, old)
			require.NoError(t, err)
			require.True(t, refusal.IsError, "fresh ownership cannot revive an expired or retired cursor")
		})
	}
}
