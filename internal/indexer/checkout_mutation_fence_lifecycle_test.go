package indexer

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/reconcile"
)

func mutationFenceRegistrySizes(l *CheckoutLifecycle) (families, checkouts, graphs int) {
	fences := l.ensureMutationFences()
	fences.mu.Lock()
	defer fences.mu.Unlock()
	return len(fences.families), len(fences.checkouts), len(fences.graphs)
}

func TestCleanupHooksReclaimTerminalMutationFences(t *testing.T) {
	f := newLifecycleFixture(t)
	defer f.close()
	ctx := context.Background()

	family, err := f.lc.AcquireCheckoutFamilyTopology(ctx, "family-terminal")
	require.NoError(t, err)
	family.Release()
	checkout, err := f.lc.AcquireCheckoutTopology(ctx, "checkout-terminal")
	require.NoError(t, err)
	checkout.Release()
	graph, err := f.lc.AcquireCheckoutGraphTopology(ctx, "graph-terminal")
	require.NoError(t, err)
	graph.Release()
	require.Equal(t, []int{1, 1, 1}, func() []int {
		families, checkouts, graphs := mutationFenceRegistrySizes(f.lc)
		return []int{families, checkouts, graphs}
	}())

	hooks := cleanupHooks{l: f.lc}
	hooks.GraphRemovalCompleted(reconcile.GraphRemovalTarget{GraphID: "graph-terminal"})
	hooks.CheckoutRemovalCompleted(reconcile.CheckoutRemovalTarget{
		CheckoutID: "checkout-terminal", Incarnation: "incarnation-terminal",
	})
	hooks.FamilyRemovalCompleted(reconcile.FamilyRemovalTarget{FamilyID: "family-terminal"})

	families, checkouts, graphs := mutationFenceRegistrySizes(f.lc)
	require.Zero(t, families)
	require.Zero(t, checkouts)
	require.Zero(t, graphs)
}

func TestGraphMutationFenceRetirementWaitsForDurableGenerationCleanup(t *testing.T) {
	f := newLifecycleFixture(t)
	defer f.close()
	ctx := context.Background()
	const graphID = "graph-retained-generation"

	graph, err := f.lc.AcquireCheckoutGraphTopology(ctx, graphID)
	require.NoError(t, err)
	graph.Release()
	generationID, err := f.catalog.CreateViewGeneration(ctx, store_sqlite.ViewGeneration{
		OwnerKind:         dedicatedBaseGenerationKind,
		GraphID:           graphID,
		LayerID:           graphID + ":base",
		GenerationKind:    dedicatedBaseGenerationKind,
		TreeOID:           "tree-retained-generation",
		ConfigHash:        "config-retained-generation",
		ExtractorVersions: "extractors-retained-generation",
		ResolverVersion:   checkoutResolverVersion,
		State:             store_sqlite.ViewGenerationSuperseded,
		CreatedAt:         time.Now().Unix(),
		PublishedAt:       time.Now().Unix(),
	})
	require.NoError(t, err)

	cleanupHooks{l: f.lc}.GraphRemovalCompleted(reconcile.GraphRemovalTarget{GraphID: graphID})
	f.lc.topologyFenceRetireMu.Lock()
	_, pending := f.lc.pendingGraphFences[graphID]
	f.lc.topologyFenceRetireMu.Unlock()
	require.True(t, pending, "durable generation must keep graph fence retirement retryable")
	_, _, graphs := mutationFenceRegistrySizes(f.lc)
	require.Equal(t, 1, graphs)

	require.NoError(t, f.catalog.DeleteViewGeneration(ctx, generationID))
	f.lc.sweepTopologyFenceRetirements(ctx)
	f.lc.topologyFenceRetireMu.Lock()
	_, pending = f.lc.pendingGraphFences[graphID]
	f.lc.topologyFenceRetireMu.Unlock()
	require.False(t, pending)
	_, _, graphs = mutationFenceRegistrySizes(f.lc)
	require.Zero(t, graphs)
}

func TestCheckoutFenceRetirementNeverWaitsUnderTopologyPublicationLock(t *testing.T) {
	f := newLifecycleFixture(t)
	defer f.close()
	ctx := context.Background()
	const checkoutID = "checkout-terminal-lock-order"

	held, err := f.lc.AcquireCheckoutTopology(ctx, checkoutID)
	require.NoError(t, err)
	done := make(chan struct{})
	go func() {
		cleanupHooks{l: f.lc}.CheckoutRemovalCompleted(reconcile.CheckoutRemovalTarget{
			CheckoutID:  checkoutID,
			Incarnation: "incarnation-terminal-lock-order",
		})
		close(done)
	}()
	waitForCheckoutGateState(t, f.lc.ensureMutationFences(), checkoutID, 1, true)

	require.True(t, f.lc.topologyPublishMu.TryLock(),
		"fence retirement waited while retaining topology publication ownership")
	f.lc.topologyPublishMu.Unlock()
	held.Release()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("terminal checkout callback did not finish after its fence drained")
	}
}
