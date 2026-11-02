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
	"github.com/zzet/gortex/internal/tokens"
)

func canonicalPromotionReadFixture(t *testing.T) (*Server, *store_sqlite.Store, string) {
	t.Helper()
	// Daemon startup warms token counting before it serves source requests.
	// Keep that one-time encoder initialization outside the read allowance.
	require.True(t, tokens.EncoderReady())
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
	for _, transition := range []string{"canonical_promotion", "receipt_only", "unchecked_eviction", "peer_same_bytes_commit", "peer_retired", "same_bytes_event", "event_then_retire", "canonical_cancelled"} {
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
			caller, cancel := context.WithCancel(t.Context())
			defer cancel()
			ctx := context.WithValue(caller, projectedSourceDeadlineKey{}, originalDeadline)
			calls := 0
			var first context.Context
			var deadlines []time.Time
			var onOwner func(func()) bool
			leaf := func(ctx context.Context, req mcplib.CallToolRequest) (*mcplib.CallToolResult, error) {

				var result *mcplib.CallToolResult
				var err error
				if !onOwner(func() {
					result, err = func() (*mcplib.CallToolResult, error) {
						t.Logf("tool=%s stage=callback_entry remaining_original_budget=%s", "get_symbol_source", time.Until(originalDeadline))
						leafStarted := time.Now()
						calls++
						deadlines = append(deadlines, ctx.Value(projectedSourceDeadlineKey{}).(time.Time))
						result, readErr := s.handleGetSymbolSource(ctx, req)
						t.Logf("tool=%s stage=leaf_return elapsed=%s remaining_original_budget=%s", "get_symbol_source", time.Since(leafStarted), time.Until(originalDeadline))
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
						case "canonical_promotion", "canonical_cancelled":
							// Actual parser/store publication, not a fabricated Done result,
							// matching hash alone, peer edit or manual registry deletion.
							publicationStarted := time.Now()
							publicationErr := s.indexer.IndexFile(path)
							t.Logf("publication elapsed=%s remaining_original_budget=%s", time.Since(publicationStarted), time.Until(originalDeadline))
							require.NoError(t, publicationErr)
							require.Equal(t, 8, store.GetNode("main.go::Neighbor").StartLine)
							rows, metaErr := store.FileMetasByPaths(entry.repoPrefix, []string{entry.graphPath})
							require.NoError(t, metaErr)
							require.Equal(t, entry.hash, rows[entry.graphPath].ContentHash)
							retirement.run()
							require.Nil(t, liveEntry(pendingFileProjections, path))
							require.Positive(t, pendingFileProjections.retired.Load())
							require.False(t, entry.suspect.Load(), "canonical promotion is not a native invalidation")
							require.True(t, time.Now().Before(originalDeadline), "canonical retirement must fit the original read allowance")
							require.True(t, entry.checkedRetired.Load())
							if transition == "canonical_cancelled" {
								cancel()
							}
						case "receipt_only":
							retirement.run()
							require.Same(t, entry, liveEntry(pendingFileProjections, path), "an unadvanced canonical store cannot retire the acknowledged bytes")
							require.Zero(t, pendingFileProjections.retired.Load())
						case "unchecked_eviction":
							pendingFileProjections.drop(entry)
							require.Nil(t, liveEntry(pendingFileProjections, path))
							require.False(t, entry.checkedRetired.Load())
						case "peer_same_bytes_commit", "peer_retired":
							held, release, lockErr := acquireMutationPath(t.Context(), path)
							require.NoError(t, lockErr)
							commit, writeErr := s.commitFileMutation(held, "write_file", "", "", "main.go", path, after, 0600)
							release()
							require.NoError(t, writeErr)
							require.Greater(t, commit.commitSeq, sequence)
							require.NotSame(t, entry, liveEntry(pendingFileProjections, path))
							require.Equal(t, entry.rawHash, liveEntry(pendingFileProjections, path).rawHash)
							if transition == "peer_retired" {
								newer := liveEntry(pendingFileProjections, path)
								publicationStarted := time.Now()
								publicationErr := s.indexer.IndexFile(path)
								t.Logf("publication elapsed=%s remaining_original_budget=%s", time.Since(publicationStarted), time.Until(originalDeadline))
								require.NoError(t, publicationErr)
								retirement.run()
								require.Nil(t, liveEntry(pendingFileProjections, path))
								require.True(t, newer.checkedRetired.Load())
								require.False(t, entry.checkedRetired.Load(), "another owner's retirement cannot authorize this old selection")
							}
						case "same_bytes_event", "event_then_retire":
							pendingFileProjections.markSuspect(path)
							require.Same(t, entry, liveEntry(pendingFileProjections, path))
							require.True(t, entry.suspect.Load())
							if transition == "event_then_retire" {
								publicationStarted := time.Now()
								publicationErr := s.indexer.IndexFile(path)
								t.Logf("publication elapsed=%s remaining_original_budget=%s", time.Since(publicationStarted), time.Until(originalDeadline))
								require.NoError(t, publicationErr)
								retirement.run()
								require.Nil(t, liveEntry(pendingFileProjections, path))
								require.False(t, entry.checkedRetired.Load())
							}
						}
						require.Equal(t, sequence, entry.commitSeq)
						require.Same(t, layer, entry.layer)
						current, diskErr := os.ReadFile(path)
						require.NoError(t, diskErr)
						require.Equal(t, after, current, "the retirement/event controls must never introduce another physical edit")
						if transition != "receipt_only" && transition != "canonical_cancelled" {
							require.ErrorIs(t, validateRequestProjections(first), projectionSuperseded, "the old context must remain unusable; a fresh attempt cannot rebind its facts")
						}
						return result, nil
					}()
				}) {
					return mcplib.NewToolResultError("fixture callback aborted"), nil
				}
				return result, err
			}
			t.Logf("tool=%s stage=dispatch remaining_original_budget=%s", "get_symbol_source", time.Until(originalDeadline))
			result, err := canonicalReadWithOwner(t, func(dispatch func(func()) bool) (*mcplib.CallToolResult, error) {
				onOwner = dispatch
				return s.wrapToolHandler(leaf)(ctx, makeReq("get_symbol_source", map[string]any{
					"id": "main.go::Target", "context_lines": 0, "format": "json",
				}))
			})
			if transition == "canonical_cancelled" {
				require.True(t, err != nil || result != nil && result.IsError, "cancellation cannot become an accepted result")
				require.Equal(t, 1, calls)
				require.ErrorIs(t, caller.Err(), context.Canceled)
				return
			}
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
				if first.Err() != nil {
					require.ErrorIs(t, first.Err(), context.Canceled, "the discarded attempt's lease was released")
					require.ErrorIs(t, validateRequestProjections(first), context.Canceled)
				} else {
					require.ErrorIs(t, validateRequestProjections(first), projectionSuperseded)
				}
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
