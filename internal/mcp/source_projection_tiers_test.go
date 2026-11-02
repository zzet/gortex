package mcp

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/pathkey"
)

type controlledTierEvidenceWatcher struct {
	*indexer.Watcher
	invalidate bool
	observed   chan bool
}

func (w *controlledTierEvidenceWatcher) EnqueueToolMutation(ctx context.Context, path string) (*indexer.MutationTicket, error) {
	entry := pendingFileProjections.snap.Load().entries[pathkey.MutationKey(path)]
	current := entry != nil && liveProjectionForAcknowledgement(entry)
	if w.invalidate {
		pendingFileProjections.markSuspect(path)
	}
	w.observed <- current
	return w.Watcher.EnqueueToolMutation(ctx, path)
}

func TestMutationAckIncludesExistingTierProducerEvidence(t *testing.T) {
	for _, invalidate := range []bool{false, true} {
		t.Run(map[bool]string{false: "live_projection", true: "observed_invalidation"}[invalidate], func(t *testing.T) {
			previous := pendingFileProjections
			pendingFileProjections = newPendingProjections()
			t.Cleanup(func() { pendingFileProjections = previous })
			s, root := setupTestServer(t)
			watcher, err := indexer.NewWatcher(s.indexer, config.WatchConfig{Enabled: true, DebounceMs: 60000}, zap.NewNop())
			require.NoError(t, err)
			controlled := &controlledTierEvidenceWatcher{Watcher: watcher, invalidate: invalidate, observed: make(chan bool, 1)}
			s.SetWatcher(controlled)
			// Keep native delivery independent of this manual tool-admission test.
			require.NoError(t, watcher.Start([]string{t.TempDir()}))
			s.graph.ResolveMutex().Lock()
			var receipt string
			t.Cleanup(func() {
				s.graph.ResolveMutex().Unlock()
				if receipt != "" {
					waitCheckoutReceipt(t, s, receipt)
				}
				require.NoError(t, watcher.Stop())
			})
			s.mutationReindexWait = time.Nanosecond
			result := callTool(t, s, "edit_file", map[string]any{"path": filepath.Join(root, "main.go"), "old_string": "func helper() {}", "new_string": "func helper() { _ = 3 }", "format": "json"})
			require.False(t, result.IsError, "%v", result.Content)
			response := extractTextResult(t, result)
			receipt, _ = response["reindex_receipt"].(string)
			require.True(t, <-controlled.observed, "the real committed projection must be live before controlled invalidation")
			require.NotZero(t, response["reindex_generation"], "prove real watcher admission before checking evidence")
			states, ok := response["tier_state"].(map[string]any)
			require.True(t, ok, "the acknowledgement must consume the existing ticket-owned tier producer")
			require.Len(t, states, 4)
			if invalidate {
				require.Equal(t, "pending", states["file"], "observed invalidation cannot receive exact-file credit")
				require.Equal(t, false, response["readback_exact"])
				require.Equal(t, "projection_not_live", response["file_reason"])
			} else {
				require.Equal(t, "fresh", states["file"], "the live physical projection must supply the additional file tier")
				require.Equal(t, true, response["readback_exact"])
				require.Equal(t, "projected", response["file_view"].(map[string]any)["state"])
			}
			ack, ok := response["ack_at"].(string)
			require.True(t, ok)
			at, err := time.Parse(time.RFC3339Nano, ack)
			require.NoError(t, err)
			require.False(t, at.IsZero())
			for tier, field := range map[string]string{"file_graph": "file_graph_at", "relations": "relations_at", "derived": "derived_at"} {
				require.Contains(t, []string{"pending", "fresh", "partial", "failed"}, states[tier])
				if states[tier] == "fresh" {
					value, ok := response[field].(string)
					require.True(t, ok, "fresh %s must have actual publication time", tier)
					stamp, err := time.Parse(time.RFC3339Nano, value)
					require.NoError(t, err)
					require.False(t, stamp.IsZero())
				}
			}
		})
	}
}
