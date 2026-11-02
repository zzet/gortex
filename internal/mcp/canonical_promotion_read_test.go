package mcp

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/query"
)

func canonicalPromotionReadFixture(t *testing.T) (*Server, *store_sqlite.Store, string) {
	t.Helper()
	root := t.TempDir()
	path := filepath.Join(root, "main.go")
	require.NoError(t, os.WriteFile(path, []byte("package probe\n\nfunc Target() int { return 1 }\n\nfunc Neighbor() int { return 9 }\n"), 0600))
	store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "canonical.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	idx := newProjectionTestIndexer(t, store)
	_, err = idx.Index(root)
	require.NoError(t, err)
	s := NewServer(query.NewEngine(store), store, idx, nil, zap.NewNop(), nil)
	s.editAckMode = config.EditAckLocal
	return s, store, path
}

// Hypothesis prepared from the older-pin diagnostic, not a reproduction claim:
// ordinary canonical promotion must be distinguished from a newer physical
// owner or native invalidation. The leaf-return seam deterministically places
// real publication/retirement before the existing final freshness decorator.
func TestSymbolSourceCanonicalPromotionAfterLeafUsesBoundedFreshAttempt(t *testing.T) {
	for _, transition := range []string{"canonical_promotion", "receipt_only", "peer_same_bytes_commit", "same_bytes_event"} {
		t.Run(transition, func(t *testing.T) {
			selectedProjectionRegistry(t)
			s, store, path := canonicalPromotionReadFixture(t)
			pending := make(chan indexer.MutationResult)
			var receipt *mutationReceipt
			t.Cleanup(func() {
				close(pending)
				if receipt == nil {
					return
				}
				select {
				case <-receipt.done:
				case <-time.After(3 * time.Second):
					t.Error("owned held ticket did not join before fixture cleanup")
				}
			})
			s.SetWatcher(mutationTestWatcher{done: pending})
			const body = "func Target() int {\n\tvalue := 7\n\treturn value\n}"
			ack, err := s.runAcknowledgedEdit(t.Context(), makeReq("edit_file", map[string]any{
				"path": path, "old_string": "func Target() int { return 1 }", "new_string": body, "format": "json",
			}), s.handleEditFile)
			require.NoError(t, err)
			require.False(t, ack.IsError, resultText(ack))
			fields := extractTextResult(t, ack)
			require.Equal(t, "local", fields["ack_mode"])
			require.Equal(t, "committed", fields["disk_status"])
			value, found := s.mutationReceipts.Load(fields["reindex_receipt"].(string))
			require.True(t, found)
			receipt = value.(*mutationReceipt)
			entry := liveEntry(pendingFileProjections, path)
			require.NotNil(t, entry)
			require.Positive(t, entry.commitSeq)
			require.True(t, liveProjectionForAcknowledgement(entry))
			require.Equal(t, 3, store.GetNode("main.go::Target").StartLine)
			require.Equal(t, 5, store.GetNode("main.go::Neighbor").StartLine, "canonical graph still precedes the acknowledged line shift")
			after, err := os.ReadFile(path)
			require.NoError(t, err)
			sequence, layer := entry.commitSeq, entry.layer
			retirement := s.canonicalProjectionRetirement(path)
			require.NotNil(t, retirement)

			originalDeadline := time.Now().Add(projectedReadBudget)
			ctx := context.WithValue(t.Context(), projectedSourceDeadlineKey{}, originalDeadline)
			calls := 0
			var first context.Context
			var deadlines []time.Time
			leaf := func(ctx context.Context, req mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {
				calls++
				deadlines = append(deadlines, ctx.Value(projectedSourceDeadlineKey{}).(time.Time))
				result, readErr := s.handleGetSymbolSource(ctx, req)
				require.NoError(t, readErr)
				require.NotNil(t, result)
				require.False(t, result.IsError, resultText(result))
				require.Equal(t, body, extractTextResult(t, result)["source"])
				if calls != 1 {
					return result, nil
				}
				first = ctx
				require.True(t, s.hasRequestProjectionFact(ctx, "main.go"), "the real source handler must borrow the ACK owner before the transition")
				switch transition {
				case "canonical_promotion":
					// Actual parser/store publication, not a fabricated Done result,
					// matching hash alone, peer edit or manual registry deletion.
					require.NoError(t, s.indexer.IndexFile(path))
					require.Equal(t, 8, store.GetNode("main.go::Neighbor").StartLine)
					rows, metaErr := store.FileMetasByPaths(entry.repoPrefix, []string{entry.graphPath})
					require.NoError(t, metaErr)
					require.Equal(t, entry.hash, rows[entry.graphPath].ContentHash)
					retirement.run()
					require.Nil(t, liveEntry(pendingFileProjections, path))
					require.Positive(t, pendingFileProjections.retired.Load())
					require.False(t, entry.suspect.Load(), "canonical promotion is not a native invalidation")
				case "receipt_only":
					retirement.run()
					require.Same(t, entry, liveEntry(pendingFileProjections, path), "an unadvanced canonical store cannot retire the acknowledged bytes")
					require.Zero(t, pendingFileProjections.retired.Load())
				case "peer_same_bytes_commit":
					held, release, lockErr := acquireMutationPath(t.Context(), path)
					require.NoError(t, lockErr)
					commit, writeErr := s.commitFileMutation(held, "write_file", "", "", "main.go", path, after, 0600)
					release()
					require.NoError(t, writeErr)
					require.Greater(t, commit.commitSeq, sequence)
					require.NotSame(t, entry, liveEntry(pendingFileProjections, path))
					require.Equal(t, entry.rawHash, liveEntry(pendingFileProjections, path).rawHash)
				case "same_bytes_event":
					pendingFileProjections.markSuspect(path)
					require.Same(t, entry, liveEntry(pendingFileProjections, path))
					require.True(t, entry.suspect.Load())
				}
				require.Equal(t, sequence, entry.commitSeq)
				require.Same(t, layer, entry.layer)
				current, diskErr := os.ReadFile(path)
				require.NoError(t, diskErr)
				require.Equal(t, after, current, "the retirement/event controls must never introduce another physical edit")
				if transition != "receipt_only" {
					require.ErrorIs(t, validateRequestProjections(first), projectionSuperseded, "the old context must remain unusable; a fresh attempt cannot rebind its facts")
				}
				return result, nil
			}
			result, err := s.wrapToolHandler(leaf)(ctx, makeReq("get_symbol_source", map[string]any{
				"id": "main.go::Target", "context_lines": 0, "format": "json",
			}))
			require.NoError(t, err)
			require.NotNil(t, result)
			t.Logf("transition=%s handler_attempts=%d final_is_error=%v", transition, calls, result.IsError)
			require.LessOrEqual(t, calls, 2, "late validation cannot renew the whole-call allowance")
			for _, deadline := range deadlines {
				require.Equal(t, originalDeadline, deadline)
			}
			facts := first.Value(requestProjectionFactsKey{}).(*requestProjectionFacts)
			facts.mu.RLock()
			borrowed := facts.entries[entry.key]
			facts.mu.RUnlock()
			require.Same(t, entry, borrowed, "another attempt cannot rebind the first selection's facts")
			switch transition {
			case "canonical_promotion":
				require.False(t, result.IsError, "verified canonical promotion should allow one coherent fresh source attempt: %s", resultText(result))
				require.Equal(t, 2, calls, "the old selection stays invalid; the successful response must come from a new whole attempt")
				require.Equal(t, body, extractTextResult(t, result)["source"])
				require.ErrorIs(t, validateRequestProjections(first), projectionSuperseded)
			case "receipt_only":
				require.False(t, result.IsError, resultText(result))
				require.Equal(t, 1, calls)
			default:
				require.True(t, result.IsError, "a peer owner or invalidating event cannot be treated as canonical promotion")
				require.Contains(t, resultText(result), "superseded")
				require.Equal(t, 1, calls)
			}
		})
	}
}
