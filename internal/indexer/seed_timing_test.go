package indexer

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
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

	entries := logs.FilterMessage("checkout lifecycle seed stage completed").All()
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
