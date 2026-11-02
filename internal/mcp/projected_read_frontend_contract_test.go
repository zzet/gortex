package mcp

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
	"github.com/zeebo/blake3"

	"github.com/zzet/gortex/internal/pathkey"
)

func TestReadFileCompressionKeepsCurrentProjectedID(t *testing.T) {
	selectedProjectionRegistry(t)
	s, root := setupTestServer(t)
	s.SetWatcher(mutationTestWatcher{})
	path := filepath.Join(root, "main.go")
	before, err := os.ReadFile(path)
	require.NoError(t, err)
	current := string(before) + "\nfunc FreshKeep() {\n println(\"KEEP_CURRENT_BODY\")\n println(1)\n println(2)\n println(3)\n}\n\nfunc FreshElide() {\n println(\"DROP_OTHER_BODY\")\n println(4)\n println(5)\n println(6)\n}\n"
	require.NoError(t, os.WriteFile(path, []byte(current), 0600))
	req := mcplib.CallToolRequest{}
	req.Params.Name = "read_file"
	req.Params.Arguments = map[string]any{"path": "main.go", "compress_bodies": true, "keep": "main.go::FreshKeep", "format": "json"}
	result, err := s.wrapToolHandler(s.handleReadFile)(t.Context(), req)
	require.NoError(t, err)
	require.False(t, result.IsError, "%s", resultText(result))
	fields := extractTextResult(t, result)
	require.Contains(t, fields["content"], "KEEP_CURRENT_BODY", "an ID must resolve against the current file projection before compression")
	require.NotContains(t, fields["content"], "DROP_OTHER_BODY")
	require.Contains(t, fields["kept_symbols"], "FreshKeep")
	require.Nil(t, s.graph.GetNode("main.go::FreshKeep"), "read-through cannot publish current definitions into the base graph")
}

func TestSymbolSourceRechecksSelectedHashAfterSameStampWrite(t *testing.T) {
	selectedProjectionRegistry(t)
	s, root := setupMoveInlineRepo(t, map[string]string{"sample.go": "package sample\nfunc Probe() int { return 0 }\n"})
	s.SetWatcher(mutationTestWatcher{})
	path := filepath.Join(root, "sample.go")
	first := []byte("package sample\nfunc Probe() int { return 1 }\n")
	second := []byte("package sample\nfunc Probe() int { return 2 }\n")
	require.Equal(t, len(first), len(second))
	require.NoError(t, os.WriteFile(path, first, 0600))
	var changed atomic.Bool
	var observed *projectionEntry
	var originalContext context.Context
	actual := func(ctx context.Context, req mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
		var err error
		ctx, err = s.ensureProjected(ctx, []string{"sample.go"})
		if err != nil {
			return nil, err
		}
		if changed.CompareAndSwap(false, true) {
			originalContext = ctx
			info, err := os.Stat(path)
			if err != nil {
				return nil, err
			}
			if err := os.WriteFile(path, second, 0600); err != nil {
				return nil, err
			}
			if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
				return nil, err
			}
		}
		result, err := s.handleGetSymbolSource(ctx, req)
		if facts, ok := ctx.Value(requestProjectionFactsKey{}).(*requestProjectionFacts); ok {
			facts.mu.RLock()
			observed = facts.entries[pathkey.MutationKey(path)]
			facts.mu.RUnlock()
		}
		return result, err
	}
	req := mcplib.CallToolRequest{}
	req.Params.Name = "get_symbol_source"
	req.Params.Arguments = map[string]any{"id": "sample.go::Probe", "context_lines": 0, "format": "json"}
	result, err := s.wrapToolHandler(actual)(t.Context(), req)
	require.NoError(t, err)
	require.False(t, result.IsError, "one native same-stamp movement can be retried coherently: %s", resultText(result))
	fields := extractTextResult(t, result)
	require.Equal(t, "func Probe() int { return 2 }", fields["source"])
	require.NotNil(t, observed)
	require.Equal(t, blake3.Sum256(second), observed.rawHash, "returned bytes and selected graph/freshness must share one actual parsed-file identity")
	require.Error(t, validateRequestProjections(originalContext), "released or superseded original contexts remain unusable")
	originalFacts := originalContext.Value(requestProjectionFactsKey{}).(*requestProjectionFacts)
	originalFacts.mu.RLock()
	originalEntry := originalFacts.entries[pathkey.MutationKey(path)]
	originalFacts.mu.RUnlock()
	require.NotSame(t, observed, originalEntry, "whole-attempt retry cannot rebind the original facts")
	require.Equal(t, blake3.Sum256(first), originalEntry.rawHash)
	require.False(t, liveProjectionForAcknowledgement(originalEntry))
}

func TestRemovedProjectedSymbolHasSpecificReason(t *testing.T) {
	for _, tool := range []string{"get_symbol", "get_symbol_source"} {
		t.Run(tool, func(t *testing.T) {
			selectedProjectionRegistry(t)
			s, root := setupTestServer(t)
			s.SetWatcher(mutationTestWatcher{})
			ack := callTool(t, s, "edit_file", map[string]any{"path": filepath.Join(root, "main.go"), "old_string": "func helper() {}", "new_string": "func Survivor() {}", "format": "json"})
			require.False(t, ack.IsError, "%s", resultText(ack))
			require.NotNil(t, s.graph.GetNode("main.go::helper"), "prove base still contains the actually removed declaration")
			handler := s.handleGetSymbol
			if tool == "get_symbol_source" {
				handler = s.handleGetSymbolSource
			}
			invoke := func(id string) *mcplib.CallToolResult {
				req := mcplib.CallToolRequest{}
				req.Params.Name = tool
				req.Params.Arguments = map[string]any{"id": id, "format": "json"}
				result, err := s.wrapToolHandler(handler)(t.Context(), req)
				require.NoError(t, err)
				return result
			}
			unknown := invoke("main.go::NeverExisted")
			require.NotContains(t, resultText(unknown), "removed by pending edit", "absence alone is not evidence of removal")
			removed := invoke("main.go::helper")
			require.Contains(t, resultText(removed), "removed by pending edit", "the current parsed projection must explain the known removed declaration")
		})
	}
}

func TestSymbolSourceRetryIsBoundedAndCancellationDoesNotReplay(t *testing.T) {
	for _, cancelFirst := range []bool{false, true} {
		t.Run(fmt.Sprintf("cancel_%v", cancelFirst), func(t *testing.T) {
			selectedProjectionRegistry(t)
			s, root := setupMoveInlineRepo(t, map[string]string{"sample.go": "package sample\nfunc Probe() int { return 0 }\n"})
			path := filepath.Join(root, "sample.go")
			require.NoError(t, os.WriteFile(path, []byte("package sample\nfunc Probe() int { return 1 }\n"), 0600))
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var calls atomic.Int64
			actual := func(ctx context.Context, req mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
				var err error
				ctx, err = s.ensureProjected(ctx, []string{"sample.go"})
				if err != nil {
					return nil, err
				}
				ordinal := calls.Add(1)
				info, err := os.Stat(path)
				if err != nil {
					return nil, err
				}
				next := []byte(fmt.Sprintf("package sample\nfunc Probe() int { return %d }\n", ordinal+1))
				if err := os.WriteFile(path, next, 0600); err != nil {
					return nil, err
				}
				if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
					return nil, err
				}
				result, err := s.handleGetSymbolSource(ctx, req)
				if cancelFirst {
					cancel()
				}
				return result, err
			}
			req := mcplib.CallToolRequest{}
			req.Params.Name = "get_symbol_source"
			req.Params.Arguments = map[string]any{"id": "sample.go::Probe", "context_lines": 0, "format": "json"}
			// Audit the retry controller over the actual source handler synchronously;
			// the public middleware race is covered by the positive test above.
			result, err := s.withProjectedSourceReadBudget(s.retryProjectedSymbolSource(actual))(ctx, req)
			if cancelFirst {
				require.True(t, errors.Is(err, context.Canceled) || result != nil && result.IsError)
				require.EqualValues(t, 1, calls.Load())
			} else {
				require.NoError(t, err)
				require.NotNil(t, result)
				require.True(t, result.IsError, "a second actual movement must refuse, not retry forever")
				require.EqualValues(t, 2, calls.Load())
				require.NotContains(t, resultText(result), "readback_exact")
			}
		})
	}
}
