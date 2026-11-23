package mcp

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/pathkey"
)

// The second sorted pending file provides an observation after the first
// file's facts were retained and before the whole selection is validated.
func TestRegisteredFreshCallersFinalBindRetirementAfterWatchedReady(t *testing.T) {
	for _, checked := range []bool{true, false} {
		name := "unchecked_eviction"
		if checked {
			name = "checked_retirement"
		}
		t.Run(name, func(t *testing.T) {
			selectedProjectionRegistry(t)
			t.Setenv("GORTEX_SEARCH_REFUSAL_EVIDENCE", "1")
			t.Setenv("GORTEX_QUERY_LOG", filepath.Join(t.TempDir(), "query-log.jsonl"))
			t.Setenv(batchTransactionDirEnv, filepath.Join(t.TempDir(), "transactions"))
			f, store, watched := newSharedReceiptFreshContractFixtureWithRetirement(t, false)
			path := filepath.Join(f.root, "main.go")
			retirement := &sourceProjectionRetirement{registry: pendingFileProjections, backend: store, key: pathkey.MutationKey(path)}
			releaseSourceEvents := holdSharedReceiptFixtureSourceEvents(t, watched.sharedReceiptWatcher, retirement.registry, path)

			const auxiliary = "zz_auxiliary.go"
			aux := &sharedReceiptOffer{agent: 1, ticket: make(chan *indexer.MutationTicket, 1)}
			setup, cancelSetup := context.WithTimeout(context.WithValue(f.contexts[1], sharedReceiptOfferKey{}, aux), 5*time.Second)
			defer cancelSetup()
			written, err := sharedReceiptJSON(f.server, setup, 50, "write_file", map[string]any{
				"path": auxiliary, "content": "package main\nfunc AuxiliaryOnly() {}\n", "format": "json",
			})
			require.NoError(t, err)
			require.NoError(t, json.Unmarshal(written["mutation_receipt"], &aux.receipt))
			auxTicket, auxReceipt := sharedReceiptFreshContractPublication(t, setup, f, aux)
			select {
			case <-auxReceipt.done:
			case <-setup.Done():
				t.Fatal("unrelated file did not finish before the observed caller")
			}
			require.False(t, auxTicket.TierState().RelationsAt.IsZero())
			require.NotNil(t, liveEntry(pendingFileProjections, filepath.Join(f.root, auxiliary)))

			a := &sharedReceiptOffer{agent: 0, ordinal: 0, ticket: make(chan *indexer.MutationTicket, 1)}
			f.edit(a)
			ctx, cancel := context.WithTimeout(f.contexts[0], 5*time.Second)
			defer cancel()
			deadline, _ := ctx.Deadline()
			ticketA, receiptA := sharedReceiptFreshContractPublication(t, ctx, f, a)
			select {
			case <-receiptA.done:
			case <-ctx.Done():
				t.Fatal("A did not reach the original relations bound")
			}
			require.False(t, ticketA.TierState().RelationsAt.IsZero())
			retirement.run()
			require.Nil(t, liveEntry(pendingFileProjections, path))

			store.armed.Store(true)
			b := &sharedReceiptOffer{agent: 1, ordinal: 0, ticket: make(chan *indexer.MutationTicket, 1)}
			bctx, cancelB := context.WithDeadline(f.contexts[1], deadline)
			defer cancelB()
			f.contexts[1] = bctx
			f.edit(b)
			ticketB, _ := sharedReceiptFreshContractPublication(t, ctx, f, b)
			select {
			case <-store.applied:
			case <-ctx.Done():
				t.Fatal("B did not reach the real pending resolver boundary")
			}
			require.True(t, ticketB.TierState().RelationsAt.IsZero())
			watched.generation.Store(ticketB.Generation)
			watched.armed.Store(true)
			barrierWorker := make(chan struct{})
			go func() {
				defer close(barrierWorker)
				defer store.unblock()
				select {
				case <-watched.pending:
				case <-ctx.Done():
				}
			}()
			t.Cleanup(func() {
				store.unblock()
				cancel()
				<-barrierWorker
			})

			var onOwner func(func()) bool
			var allowance *projectedSourceAllowance
			var receiptSequence uint64
			var originalProjectionDeadline time.Time
			observer := checkedProjectionReadObserver(func(build context.Context, selected *projectionEntry) {
				if selected.graphPath != auxiliary {
					return
				}
				if !onOwner(func() {
					if allowance != nil {
						return
					}
					select {
					case <-watched.pending:
						require.True(t, watched.pendingState.RelationsAt.IsZero())
					default:
						t.Fatal("handler was reached without the real watched wait")
					}
					require.False(t, ticketB.TierState().RelationsAt.IsZero(), "actual B relations completed before binding")
					allowance, _ = build.Value(projectedSourceAllowanceKey{}).(*projectedSourceAllowance)
					require.NotNil(t, allowance)
					require.True(t, allowance.afterFreshness)
					require.EqualValues(t, 1, allowance.attempts.Load())
					originalProjectionDeadline = build.Value(projectedSourceDeadlineKey{}).(time.Time)
					require.Equal(t, originalProjectionDeadline, allowance.deadline)
					require.True(t, originalProjectionDeadline.Before(deadline))
					require.True(t, time.Now().Before(originalProjectionDeadline))
					facts := build.Value(requestProjectionFactsKey{}).(*requestProjectionFacts)
					facts.mu.RLock()
					prior := facts.entries[retirement.key]
					facts.mu.RUnlock()
					require.NotNil(t, prior, "the first sorted file must already be retained")
					require.Same(t, prior, liveEntry(pendingFileProjections, path))
					require.False(t, prior.suspect.Load())
					var ack sharedReceiptStatus
					require.True(t, sharedReceiptDecode(b.content["edit_file"], &ack))
					require.Equal(t, b.receipt, ack.MutationReceipt)
					require.Equal(t, ack.Commit, prior.sourceCommitSeq)
					require.NotEmpty(t, ack.SHA)
					require.Equal(t, ack.SHA, prior.contentSHA256)
					receiptSequence = prior.sourceCommitSeq
					if checked {
						retirement.run()
					} else {
						pendingFileProjections.drop(prior)
					}
					require.Nil(t, liveEntry(pendingFileProjections, path))
					require.Equal(t, checked, prior.checkedRetired.Load())
					require.False(t, prior.suspect.Load())
					require.True(t, time.Now().Before(originalProjectionDeadline))
				}) {
					cancel()
				}
			})
			caller := context.WithValue(ctx, checkedProjectionReadObserverKey{}, observer)
			first, err := canonicalReadWithOwner(t, func(dispatch func(func()) bool) (*mcplib.CallToolResult, error) {
				onOwner = dispatch
				return externalVersionRegisteredCall(f.server, caller, 300, "get_callers", sharedReceiptCallersArguments())
			})
			require.NoError(t, err)
			require.NotNil(t, first)
			require.NotNil(t, allowance, "the final-validation transition must have occurred")
			raw, err := json.Marshal(first)
			require.NoError(t, err)
			t.Logf("FINAL_BIND_WATCHED_FIRST_REPLY checked=%t attempts=%d receipt_seq=%d %s", checked, allowance.attempts.Load(), receiptSequence, raw)
			releaseSourceEvents()
			require.NotNil(t, first.Meta)
			trace := first.Meta.AdditionalFields["gortex_projection_predicate"].(map[string]any)
			predicate := trace["first_refusal"].(map[string]any)
			require.Equal(t, "bind.final_validation", predicate["site"])
			require.Equal(t, "nil", predicate["current_id"])
			require.Equal(t, float64(receiptSequence), predicate["selected_source_seq"])
			wait := first.Meta.AdditionalFields["freshness"].(map[string]any)
			require.Equal(t, true, wait["fresh"], "the earlier watcher wait really finished")
			require.Positive(t, wait["waited_ms"].(float64))
			stillOriginal, _ := caller.Deadline()
			require.Equal(t, deadline, stillOriginal)
			require.Equal(t, originalProjectionDeadline, allowance.deadline)
			if !checked {
				require.True(t, first.IsError, "eviction without proof must preserve the refusal")
				require.EqualValues(t, 1, allowance.attempts.Load())
				return
			}
			count, _ := sharedReceiptFreshContractOwnEdges(t, first, "main.go::Stream0Edit00")
			require.Equal(t, 1, count)
			count, _ = sharedReceiptFreshContractOwnEdges(t, first, "main.go::Stream1Edit00")
			require.Equal(t, 1, count)
			require.EqualValues(t, 2, allowance.attempts.Load(), "checked retirement permits the one existing rebound")
		})
	}
}
