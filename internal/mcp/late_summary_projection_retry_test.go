package mcp

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/pathkey"
)

func TestLateSummaryProjectionProofKeepsRefusalFences(t *testing.T) {
	for _, change := range []string{"newer_same_bytes_commit", "different_bytes_same_stamp", "event_during_check", "cancelled", "replay_unsafe", "deadline_expired", "attempts_spent", "foreign_checkout", "unproven_version", "other_tool"} {
		t.Run(change, func(t *testing.T) {
			selectedProjectionRegistry(t)
			s, _, path := canonicalPromotionReadFixture(t)
			pending := make(chan indexer.MutationResult)
			s.SetWatcher(mutationTestWatcher{done: pending})
			var receipt *mutationReceipt
			t.Cleanup(func() {
				close(pending)
				if receipt != nil {
					select {
					case <-receipt.done:
					case <-time.After(3 * time.Second):
						t.Error("owned held mutation did not join")
					}
				}
			})
			const body = "func Target() int {\n\tvalue := 7\n\treturn value\n}"
			ack, err := s.runAcknowledgedEdit(t.Context(), makeReq("edit_symbol", map[string]any{
				"id": "main.go::Target", "old_source": "func Target() int { return 1 }", "new_source": body, "dry_run": false,
			}), s.handleEditSymbol)
			require.NoError(t, err)
			require.False(t, ack.IsError, resultText(ack))
			fields := extractTextResult(t, ack)
			require.Equal(t, "local", fields["ack_mode"])
			require.Equal(t, true, fields["readback_exact"])
			value, found := s.mutationReceipts.Load(fields["reindex_receipt"].(string))
			require.True(t, found)
			receipt = value.(*mutationReceipt)
			entry := liveEntry(pendingFileProjections, path)
			require.NotNil(t, entry)
			if change == "foreign_checkout" || change == "unproven_version" {
				copy := entry.committedCopy(0, entry.disk)
				if change == "foreign_checkout" {
					other := filepath.Join(t.TempDir(), "main.go")
					raw, readErr := os.ReadFile(path)
					require.NoError(t, readErr)
					require.NoError(t, os.WriteFile(other, raw, 0600))
					copy.absPath, copy.key = other, pathkey.MutationKey(other)
					copy.disk, err = statDiskStamp(other)
					require.NoError(t, err)
				} else {
					copy.sourceCommitSeq = 0
					// Construct an unproven current version, rather than asking a
					// zero sequence to overwrite a live physical execution.
					pendingFileProjections.drop(entry)
				}
				require.NoError(t, pendingFileProjections.publish(copy))
				entry = copy
			}
			caller, cancel := context.WithCancel(t.Context())
			defer cancel()
			ctx, err := retainRequestProjection(caller, entry)
			require.NoError(t, err)
			allowance := &projectedSourceAllowance{tool: "get_file_summary"}
			allowance.attempts.Store(1)
			ctx = context.WithValue(ctx, projectedSourceAllowanceKey{}, allowance)
			deadline := time.Now().Add(projectedReadBudget)
			state := &freshSymbolAttempt{}
			ctx = context.WithValue(ctx, freshSymbolAttemptKey{}, state)
			pendingFileProjections.markSuspect(entry.absPath)
			current := entry
			checks := 0
			req := makeReq("get_file_summary", map[string]any{"path": "main.go", "format": "json"})
			switch change {
			case "newer_same_bytes_commit":
				current = entry.committedCopy(entry.sourceCommitSeq+1, entry.disk)
				require.NoError(t, pendingFileProjections.publish(current))
			case "different_bytes_same_stamp":
				raw, readErr := os.ReadFile(path)
				require.NoError(t, readErr)
				changed := strings.Replace(string(raw), "value := 7", "value := 8", 1)
				require.NotEqual(t, string(raw), changed)
				require.NoError(t, os.WriteFile(path, []byte(changed), 0600))
				require.NoError(t, os.Chtimes(path, entry.disk.info.ModTime(), entry.disk.info.ModTime()))
				stamp, statErr := statDiskStamp(path)
				require.NoError(t, statErr)
				require.True(t, entry.disk.matches(stamp), "only the required byte check distinguishes this write")
			case "event_during_check":
				ctx = context.WithValue(ctx, checkedProjectionReadObserverKey{}, checkedProjectionReadObserver(func(context.Context, *projectionEntry) {
					checks++
					pendingFileProjections.markSuspect(path)
				}))
			case "cancelled":
				cancel()
			case "replay_unsafe":
				state.replayUnsafe = true
			case "deadline_expired":
				deadline = time.Now().Add(-time.Nanosecond)
			case "attempts_spent":
				allowance.attempts.Store(2)
			case "other_tool":
				req = makeReq("get_symbol", map[string]any{"id": "main.go::Target", "detail": "full", "format": "json"})
			}
			ctx = context.WithValue(ctx, projectedSourceDeadlineKey{}, deadline)
			require.False(t, s.retryAfterLateSummaryProjectionChange(ctx, req, projectionSuperseded))
			require.Same(t, current, liveEntry(pendingFileProjections, entry.absPath), "failed proof cannot publish changed bytes or replace the current owner")
			require.True(t, entry.suspect.Load(), "never clear the old invalidation")
			require.Equal(t, deadline, ctx.Value(projectedSourceDeadlineKey{}))
			if change == "event_during_check" {
				pendingFileProjections.mu.Lock()
				observed := entry.sourceEvents
				pendingFileProjections.mu.Unlock()
				require.Equal(t, 1, checks, "the second event must occur inside the actual byte-checked proof")
				require.EqualValues(t, 2, observed)
			}
		})
	}
}
