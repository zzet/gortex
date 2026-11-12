package indexer

import (
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/reconcile"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// Replay old journal payloads through the lifecycle's actual cleanup hooks and
// YAML finalizer. The fixture restarts its real store and ConfigManager; this
// is journal compatibility on a current-schema database, not a schema upgrade.
func TestLegacyPrimaryClosureReplayPreservesOrRemovesPersistedConfig(t *testing.T) {
	for _, tc := range []struct {
		name    string
		revoked bool
	}{
		{name: "active-intent"},
		{name: "revoked-intent", revoked: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			core, logs := observer.New(zap.InfoLevel)
			f := newLifecycleFixture(t, zap.New(core))
			defer f.close()
			root := f.gitRepo("legacy-primary")
			tracked, err := f.lc.Register(ctx, config.RepoEntry{Path: root, Name: "legacy-primary"}, TrackSourceCLI)
			require.NoError(t, err)
			sentinelRoot := f.gitRepo("retained-primary")
			_, err = f.lc.Register(ctx, config.RepoEntry{Path: sentinelRoot, Name: "retained-primary"}, TrackSourceCLI)
			require.NoError(t, err)
			require.ElementsMatch(t, []string{root, sentinelRoot}, f.configPaths())
			beforeBytes, err := os.ReadFile(f.cfgPath)
			require.NoError(t, err)
			beforeConfig, err := config.LoadGlobal(f.cfgPath)
			require.NoError(t, err)
			var sentinel config.RepoEntry
			for _, entry := range beforeConfig.Repos {
				if entry.Path == sentinelRoot {
					sentinel = entry
				}
			}
			require.Equal(t, sentinelRoot, sentinel.Path)

			graph := f.familyOf(tracked.Prefix)
			require.True(t, graph.IsPrimaryBase)
			family, found, err := f.catalog.GetRepositoryFamily(ctx, tracked.FamilyID)
			require.NoError(t, err)
			require.True(t, found)
			intents, err := f.catalog.ListTrackingIntents(ctx, tracked.CheckoutID)
			require.NoError(t, err)
			require.Len(t, intents, 1)
			require.True(t, intents[0].Active)
			if tc.revoked {
				intent := intents[0]
				intent.Active = false
				intent.RevokedAt = f.clock.Now().Unix()
				require.NoError(t, f.catalog.UpsertTrackingIntent(ctx, intent))
			}
			// No explicit untrack API runs: only the old journal and persisted
			// intent may authorize replay to remove the owned YAML entry.
			payload, err := json.Marshal(map[string]any{
				"kind": "retire_primary_closure", "graph_id": graph.GraphID,
				"family_id": tracked.FamilyID, "checkout_id": tracked.CheckoutID,
				"primary_epoch": family.PrimaryEpoch,
			})
			require.NoError(t, err)
			var raw map[string]json.RawMessage
			require.NoError(t, json.Unmarshal(payload, &raw))
			require.NotContains(t, raw, "cause", "the payload must exercise legacy cause inference")
			cleanupID := "retire-primary-closure:" + graph.GraphID
			require.NoError(t, f.catalog.UpsertCleanupEntry(ctx, store_sqlite.CleanupEntry{
				CleanupID: cleanupID, OpaqueTargetIDs: string(payload),
				Reason: "retire_primary_closure", Phase: store_sqlite.CleanupPhasePending,
			}))

			previousStore, previousConfig := f.store, f.cm
			f.restart()
			require.NotSame(t, previousStore, f.store)
			require.NotSame(t, previousConfig, f.cm)
			persisted, found, err := f.catalog.GetCleanupEntry(ctx, cleanupID)
			require.NoError(t, err)
			require.True(t, found, "close/reopen must retain the actual old journal")
			require.Equal(t, string(payload), persisted.OpaqueTargetIDs)
			require.Equal(t, store_sqlite.CleanupPhasePending, persisted.Phase)
			reopenedBytes, err := os.ReadFile(f.cfgPath)
			require.NoError(t, err)
			require.Equal(t, beforeBytes, reopenedBytes, "reopening must not perform the replay's config mutation")
			logs.TakeAll()

			// Seed resumes pending teardown before reading configured roots,
			// exactly the startup order that prevents a revoked entry returning.
			require.NoError(t, f.lc.Seed(ctx))
			require.Eventually(t, func() bool {
				_, present, err := f.catalog.GetCleanupEntry(ctx, cleanupID)
				return err == nil && !present
			}, 10*time.Second, 10*time.Millisecond, "the real replay did not finish")
			afterBytes, err := os.ReadFile(f.cfgPath)
			require.NoError(t, err)
			afterConfig, err := config.LoadGlobal(f.cfgPath)
			require.NoError(t, err)
			var roots []string
			var retainedSentinel config.RepoEntry
			for _, entry := range afterConfig.Repos {
				roots = append(roots, entry.Path)
				if entry.Path == sentinelRoot {
					retainedSentinel = entry
				}
			}
			require.Equal(t, sentinel, retainedSentinel, "replaying one family must preserve the other persisted entry")
			_, checkoutPresent, err := f.catalog.GetCheckout(ctx, tracked.CheckoutID)
			require.NoError(t, err)
			_, graphPresent, err := f.catalog.GetDedicatedGraph(ctx, graph.GraphID)
			require.NoError(t, err)
			removals := logs.FilterMessage("config: removed tracked repo").All()
			if tc.revoked {
				require.Equal(t, []string{sentinelRoot}, roots)
				require.False(t, checkoutPresent)
				require.False(t, graphPresent)
				require.Len(t, removals, 1, "real YAML finalization removes the legacy revoked entry once")
				require.Equal(t, root, removals[0].ContextMap()["root"])
				require.Equal(t, reconcile.CauseUntrack, removals[0].ContextMap()["cause"])
				require.Equal(t, "repository_cleanup", removals[0].ContextMap()["caller"])
			} else {
				require.ElementsMatch(t, []string{root, sentinelRoot}, roots)
				require.Equal(t, beforeBytes, afterBytes, "the owned stable fixture stays byte-identical for an active intent")
				require.True(t, checkoutPresent)
				require.True(t, graphPresent)
				require.Empty(t, removals)
			}

			// A second true restart and replay cannot resurrect a revoked
			// entry, remove an active one, or repeat a removal event.
			removalCount := len(removals)
			f.restart()
			require.NoError(t, f.lc.Seed(ctx))
			require.NoError(t, f.lc.Reconciler().Resume(ctx))
			finalBytes, err := os.ReadFile(f.cfgPath)
			require.NoError(t, err)
			require.Equal(t, afterBytes, finalBytes, "completed replay must be durable and idempotent")
			require.Len(t, logs.FilterMessage("config: removed tracked repo").All(), removalCount)
		})
	}
}
