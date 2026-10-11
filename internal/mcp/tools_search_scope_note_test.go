package mcp

import (
	"context"
	"encoding/json"
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"github.com/zzet/gortex/internal/search"
)

type scopeNoteCountingBackend struct {
	searchSymbolsContextBackend
	repoCalls [][]string
}

func (b *scopeNoteCountingBackend) SearchSymbolBundlesScopedContext(_ context.Context, _ string, repos []string, _ int) []search.SymbolBundle {
	b.repoCalls = append(b.repoCalls, append([]string(nil), repos...))
	return []search.SymbolBundle{} // Authoritative empty scoped answer.
}

func TestSearchSymbolsScopedMissHintDoesNotRetrieveOutsideScope(t *testing.T) {
	for _, text := range []string{"missing output encoding", "ZZZUNFINDABLE"} {
		t.Run(text, func(t *testing.T) {
			fixture := newSharedWorkspaceServer(t, true)
			backend := &scopeNoteCountingBackend{}
			fixture.srv.engine.SetSearch(backend)
			fixture.srv.engine.SetRerank(nil)
			ctx := sessionCtx("scope-note-miss", fixture.repoA)
			res, err := fixture.srv.handleSearchSymbols(ctx, makeReq("search_symbols", map[string]any{
				"query": text, "expand": "none", "assist": "never",
			}))
			require.NoError(t, err)
			require.False(t, res.IsError)
			var resp map[string]any
			require.NoError(t, json.Unmarshal([]byte(res.Content[0].(mcplib.TextContent).Text), &resp))
			require.Empty(t, resultIDs(resp))
			require.Equal(t, [][]string{{"repo-a"}}, backend.repoCalls, "only the scoped primary search is needed")
			require.Zero(t, backend.contextCalls.Load(), "scope guidance must not retrieve an unscoped bundle")
			require.Zero(t, backend.legacyBundleCalls.Load())
			require.Zero(t, backend.legacySearchCalls.Load())
			require.Equal(t, scopeZeroNote(ResolvedScope{Applied: "repo:repo-a"}, -1), resp["scope_note"])
		})
	}
}

func TestSearchSymbolsScopedMissCancellationBeforePublication(t *testing.T) {
	fixture := newSharedWorkspaceServer(t, true)
	backend := &scopeNoteCountingBackend{}
	fixture.srv.engine.SetSearch(backend)
	fixture.srv.engine.SetRerank(nil)
	ctx, cancel := context.WithCancel(sessionCtx("scope-note-cancel", fixture.repoA))
	defer cancel()
	sess := fixture.srv.sessionFor(ctx)
	sess.recordLastSearch("previous search", []string{"repo-a/previous.go::Previous"})

	// The phases log runs after assembling the scope note and immediately
	// before the final cancellation gate and session/localization publication.
	core, _ := observer.New(zap.DebugLevel)
	canceledAtPublication := false
	fixture.srv.logger = zap.New(core, zap.Hooks(func(entry zapcore.Entry) error {
		if entry.Message == "search_symbols phases" {
			canceledAtPublication = true
			cancel()
		}
		return nil
	}))
	res, err := fixture.srv.handleSearchSymbols(ctx, makeReq("search_symbols", map[string]any{
		"query": "missing output encoding", "expand": "none", "assist": "never",
	}))
	require.True(t, canceledAtPublication)
	require.ErrorIs(t, err, context.Canceled)
	require.Nil(t, res, "a canceled request must not publish an authoritative miss or hint")
	require.Equal(t, [][]string{{"repo-a"}}, backend.repoCalls)
	require.Zero(t, backend.contextCalls.Load())
	require.Zero(t, backend.legacyBundleCalls.Load())
	require.Zero(t, backend.legacySearchCalls.Load())
	sess.mu.Lock()
	defer sess.mu.Unlock()
	require.Equal(t, "previous search", sess.lastSearch.query)
	require.Equal(t, []string{"repo-a/previous.go::Previous"}, sess.lastSearch.returned)
}
