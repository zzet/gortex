package indexer

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/gitstate"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/reconcile"
)

func TestCheckoutLifecycleSeedLogsStages(t *testing.T) {
	f := newLifecycleFixture(t)
	defer f.close()
	core, logs := observer.New(zap.DebugLevel)
	f.lc.logger = zap.New(core)
	root := f.gitRepo("seed-timing")
	require.NoError(t, f.cm.Global().AddRepo(config.RepoEntry{Path: root, Name: "seed-timing"}))
	ctx := context.Background()
	identity, err := f.lc.recordCheckout(ctx, "seed-timing", root, TrackSourceConfig, true)
	require.NoError(t, err)
	generationID, err := f.catalog.CreateViewGeneration(ctx, store_sqlite.ViewGeneration{
		OwnerKind: "dedicated_graph", GraphID: GraphIDFor("seed-timing"),
		CheckoutID: identity.checkoutID, GenerationKind: "commit", TreeOID: "old-tree",
		State: store_sqlite.ViewGenerationFailed, CreatedAt: f.clock.Now().Unix(),
	})
	require.NoError(t, err)
	require.NoError(t, f.lc.Seed(ctx))
	retirements := logs.FilterMessage("checkout lifecycle generation retirement completed").All()
	require.Len(t, retirements, 1)
	assert.Equal(t, zap.DebugLevel, retirements[0].Level)
	fields := retirements[0].ContextMap()
	assert.Equal(t, "seed-timing", fields["repo"])
	assert.Equal(t, generationID, fields["generation_id"])
	assert.IsType(t, float64(0), fields["elapsed_seconds"])

	// Seed also emits per-checkout/per-family detail. Select the five
	// aggregate stage records whose counters this upstream contract checks.
	var entries []observer.LoggedEntry
	for _, entry := range logs.FilterMessage("checkout lifecycle seed stage completed").All() {
		fields := entry.ContextMap()
		switch fields["stage"] {
		case "resume_reconciler", "sweep_retirements", "resume_mode_transitions":
			entries = append(entries, entry)
		case "record_checkout":
			if _, ok := fields["recorded_checkouts"]; ok {
				entries = append(entries, entry)
			}
		case "reconcile_family":
			if _, ok := fields["families"]; ok {
				entries = append(entries, entry)
			}
		}
	}
	require.Len(t, entries, 5)
	stages := []struct {
		name  string
		count string
		want  int64
	}{
		{"resume_reconciler", "", 0},
		{"sweep_retirements", "retired_generations", 1},
		{"record_checkout", "recorded_checkouts", 1},
		{"resume_mode_transitions", "resumed_transitions", 0},
		{"reconcile_family", "reconciled_families", 1},
	}
	for i, stage := range stages {
		fields := entries[i].ContextMap()
		assert.Equal(t, stage.name, fields["stage"])
		assert.Equal(t, "success", fields["outcome"])
		elapsed, ok := fields["elapsed_seconds"].(float64)
		require.True(t, ok, "%s must log elapsed seconds", stage.name)
		assert.GreaterOrEqual(t, elapsed, float64(0))
		if stage.count != "" {
			assert.Equal(t, stage.want, fields[stage.count])
		}
	}
}

func TestCheckoutLifecycleSeedLogsFirstReconcileFailure(t *testing.T) {
	f := newLifecycleFixture(t)
	defer f.close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	families := make(map[string]string)
	for _, name := range []string{"failure-one", "failure-two"} {
		root := f.gitRepo(name)
		require.NoError(t, f.cm.Global().AddRepo(config.RepoEntry{Path: root, Name: name}))
		identity, err := f.lc.recordCheckout(ctx, name, root, TrackSourceConfig, true)
		require.NoError(t, err)
		canonical, err := filepath.EvalSymlinks(root)
		require.NoError(t, err)
		families[canonical] = identity.familyID
	}
	firstFamily := ""
	var err error
	f.lc.rec, err = reconcile.New(f.catalog, cleanupHooks{l: f.lc}, lifecycleGrace,
		reconcile.WithClock(f.clock.Now),
		reconcile.WithInventory(func(ctx context.Context, root string) (*gitstate.FamilyInventory, error) {
			// Cancel only after both families have been recorded by Seed.
			if firstFamily == "" {
				firstFamily = families[root]
			}
			cancel()
			return nil, ctx.Err()
		}))
	require.NoError(t, err)
	core, logs := observer.New(zap.InfoLevel)
	f.lc.logger = zap.New(core)

	require.NoError(t, f.lc.Seed(ctx), "reconciliation remains best-effort")
	require.NotEmpty(t, firstFamily)
	var entries []observer.LoggedEntry
	for _, entry := range logs.FilterMessage("checkout lifecycle seed stage completed").FilterField(zap.String("stage", "reconcile_family")).All() {
		if _, summary := entry.ContextMap()["families"]; summary {
			entries = append(entries, entry)
		}
	}
	require.Len(t, entries, 1)
	fields := entries[0].ContextMap()
	assert.Equal(t, "error", fields["outcome"])
	failure, ok := fields["error"].(string)
	require.True(t, ok, "reconciliation error must be a string")
	assert.True(t, strings.HasPrefix(failure, "family "+firstFamily+": "), "error = %q", failure)
	assert.Contains(t, failure, "context canceled")
	assert.Equal(t, int64(2), fields["families"])
	assert.Equal(t, int64(0), fields["reconciled_families"])
	assert.Equal(t, int64(2), fields["failed_families"])
}
