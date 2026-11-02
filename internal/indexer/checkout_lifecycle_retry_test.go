package indexer

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/reconcile"
)

func TestCheckoutLifecycleReconcileFamilyRetainsExpiredIntendedCheckout(t *testing.T) {
	f := newLifecycleFixture(t)
	defer f.close()
	ctx := context.Background()
	root := f.gitRepo("event-offline")
	tracked, err := f.lc.Register(ctx, config.RepoEntry{Path: root, Name: "event-offline"}, TrackSourceCLI)
	require.NoError(t, err)
	require.NoError(t, tracked.CatalogErr)
	evidence, _, err := f.catalog.GetCheckoutPathEvidence(ctx, tracked.CheckoutID)
	require.NoError(t, err)
	proven := evidence.CommonDirVolumeToken != ""
	require.NoError(t, os.RemoveAll(root))
	graced, err := f.lc.ReconcileFamily(ctx, tracked.FamilyID)
	require.NoError(t, err)
	entry := checkoutReportByID(t, graced, tracked.CheckoutID)
	if proven {
		require.Equal(t, reconcile.ActionRemovalGraceStarted, entry.Action)
	} else {
		require.Equal(t, reconcile.ActionInventoryUnknown, entry.Action)
	}
	f.lc.retryMu.Lock()
	retry, armed := f.lc.familyRetries[tracked.FamilyID]
	f.lc.retryMu.Unlock()
	require.True(t, armed, "unknown inventories and grace windows both need a retry")
	require.Greater(t, retry.deadline, f.clock.Now().Unix())
	f.clock.advance(lifecycleGrace.AvailabilityGrace + time.Second)
	expired, err := f.lc.ReconcileFamily(ctx, tracked.FamilyID)
	require.NoError(t, err)
	entry = checkoutReportByID(t, expired, tracked.CheckoutID)
	if proven {
		require.Equal(t, reconcile.ActionPrimaryRetirementRefused, entry.Action)
	} else {
		require.Equal(t, reconcile.ActionInventoryUnknown, entry.Action)
	}
	require.NotNil(t, f.mi.GetMetadata("event-offline"))
	require.Contains(t, f.configPaths(), root)
}

func TestCheckoutLifecycleFamilyRetryTimerFires(t *testing.T) {
	lifecycle := &CheckoutLifecycle{
		now:           time.Now,
		logger:        zap.NewNop(),
		familyRetries: map[string]familyRetry{},
		coordinators:  map[string]*CheckoutCoordinator{},
	}
	t.Cleanup(func() { require.NoError(t, lifecycle.Close()) })

	initial := time.Now().Unix()
	lifecycle.scheduleFamilyRetryAt("family", initial)
	require.Eventually(t, func() bool {
		lifecycle.retryMu.Lock()
		defer lifecycle.retryMu.Unlock()
		retry, ok := lifecycle.familyRetries["family"]
		return ok && retry.deadline > initial
	}, time.Second, 5*time.Millisecond,
		"the expired timer should run reconciliation and schedule a bounded retry after failure")
}

func checkoutReportByID(t *testing.T, report reconcile.FamilyReport, checkoutID string) reconcile.CheckoutReport {
	t.Helper()
	for _, checkout := range report.Checkouts {
		if checkout.CheckoutID == checkoutID {
			return checkout
		}
	}
	t.Fatalf("checkout %s missing from report %+v", checkoutID, report)
	return reconcile.CheckoutReport{}
}
