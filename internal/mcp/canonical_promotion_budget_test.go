package mcp

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer"
)

func TestRepeatedCheckedRetirementCannotRenewSourceAllowance(t *testing.T) {
	selectedProjectionRegistry(t)
	s, store, path := canonicalPromotionReadFixture(t)
	pending := make(chan indexer.MutationResult)
	s.SetWatcher(mutationTestWatcher{done: pending})
	var receipt *mutationReceipt
	t.Cleanup(func() {
		close(pending)
		if receipt != nil {
			select {
			case <-receipt.done:
			case <-time.After(3 * time.Second):
				t.Error("owned held ticket did not join")
			}
		}
	})
	ack, err := s.runAcknowledgedEdit(t.Context(), makeReq("edit_file", map[string]any{
		"path": path, "old_string": "return 1", "new_string": "return 7", "format": "json",
	}), s.handleEditFile)
	require.NoError(t, err)
	require.False(t, ack.IsError, resultText(ack))
	value, found := s.mutationReceipts.Load(extractTextResult(t, ack)["reindex_receipt"].(string))
	require.True(t, found)
	receipt = value.(*mutationReceipt)
	prepared := liveEntry(pendingFileProjections, path)
	require.NotNil(t, prepared)
	require.Positive(t, prepared.commitSeq)
	before, err := os.ReadFile(path)
	require.NoError(t, err)
	current := string(before)
	originalDeadline := time.Now().Add(projectedReadBudget)
	ctx := context.WithValue(t.Context(), projectedSourceDeadlineKey{}, originalDeadline)
	var entries []*projectionEntry
	var contexts []context.Context
	calls := 0
	leaf := func(ctx context.Context, req mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
		calls++
		t.Logf("attempt=%d before_leaf_remaining=%s", calls, time.Until(originalDeadline))
		require.Equal(t, originalDeadline, ctx.Value(projectedSourceDeadlineKey{}))
		result, err := s.handleGetSymbolSource(ctx, req)
		require.NoError(t, err)
		require.False(t, result.IsError, resultText(result))
		t.Logf("attempt=%d after_leaf_remaining=%s", calls, time.Until(originalDeadline))
		entry := liveEntry(pendingFileProjections, path)
		require.NotNil(t, entry)
		require.True(t, s.hasRequestProjectionFact(ctx, "main.go"))
		entries = append(entries, entry)
		contexts = append(contexts, ctx)
		retirement := s.canonicalProjectionRetirement(path)
		require.NotNil(t, retirement)
		// Publish the already checked full rows in the real canonical SQLite
		// backend. The original interleaving test covers IndexFile orchestration;
		// this control isolates two retirements from its unrelated cold work.
		nodes := entry.layer.FileNodes(entry.graphPath)
		store.AddBatch(nodes, nil)
		require.NoError(t, store.SetFileMetas(entry.repoPrefix, []graph.FileMetaRow{{
			FilePath: entry.graphPath, ContentHash: entry.hash, NodeCount: len(nodes),
		}}))
		require.Equal(t, nodes[0].ID, store.GetNode(nodes[0].ID).ID)
		retirement.run()
		t.Logf("attempt=%d after_retirement_remaining=%s", calls, time.Until(originalDeadline))
		require.True(t, entry.checkedRetired.Load())
		require.Nil(t, liveEntry(pendingFileProjections, path))
		if calls == 1 {
			// A second real source version forces the fresh attempt to select
			// another projection, which can independently catch up and retire.
			current = strings.Replace(current, "return 7", "return 8", 1)
			require.NoError(t, os.WriteFile(path, []byte(current), 0600))
		}
		return result, nil
	}
	result, err := s.wrapToolHandler(leaf)(ctx, makeReq("get_symbol_source", map[string]any{
		"id": "main.go::Target", "context_lines": 0, "format": "json",
	}))
	require.NoError(t, err)
	require.NotNil(t, result)
	t.Logf("handler_attempts=%d final=%s remaining_original_budget=%s", calls, resultText(result), time.Until(originalDeadline))
	require.True(t, result.IsError, "two retirements cannot authorize a third fresh attempt")
	require.Equal(t, 2, calls)
	require.NotSame(t, entries[0], entries[1])
	for index, observed := range contexts {
		if observed.Err() != nil {
			require.ErrorIs(t, observed.Err(), context.Canceled)
			require.ErrorIs(t, validateRequestProjections(observed), context.Canceled)
		} else {
			require.ErrorIs(t, validateRequestProjections(observed), projectionSuperseded)
		}
		facts := observed.Value(requestProjectionFactsKey{}).(*requestProjectionFacts)
		facts.mu.RLock()
		borrowed := facts.entries[entries[index].key]
		facts.mu.RUnlock()
		require.Same(t, entries[index], borrowed)
	}
}
