package indexer

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/config"
)

// The view census is what a person reads when a daemon is not serving what
// they expect. Everything in it is a count, and a count is only legible beside
// the reason it has the value it does: "three checkouts, one build loop" states
// a problem and explains nothing.
//
// The reasons exist — every path that starts a coordinator records why it could
// not — and until this they went nowhere a person could read. The daemon log
// carries them, which is not the same thing: a log line is a moment, and the
// question here is about the daemon's present.

// TestAViewCensusStatesWhyACheckoutHasNoBuildLoop is the health surface for a
// checkout whose coordinator could not be started.
//
// Nothing upstream receives that failure — every entry point that starts a
// coordinator is a background reconciliation with no caller to fail — so the
// checkout simply has no view. The census is the one payload that already
// answers "what is this daemon holding", it is assembled on the status path
// (cmd/gortex/daemon_controller.go's collectViewsStatus calls exactly this
// method), and it is where the reason belongs.
func TestAViewCensusStatesWhyACheckoutHasNoBuildLoop(t *testing.T) {
	f := newFamilyFixture(t, "unfreezable-census")
	defer f.close()
	ctx := context.Background()

	health, err := f.lc.ViewsHealth(ctx)
	require.NoError(t, err)
	require.Empty(t, health.CoordinatorStartFailures,
		"a daemon whose checkouts all have build loops still reports a reason for one that has none")

	checkout, found, err := f.catalog.GetCheckout(ctx, f.automatic.CheckoutID)
	require.NoError(t, err)
	require.True(t, found)
	f.lc.dropCoordinator(f.automatic.CheckoutID)
	f.lc.configSnapshot = func(
		config.IndexConfig, string, string, string,
	) (config.IndexConfig, string, error) {
		return config.IndexConfig{}, "", errors.New("encode dedicated base config: unencodable value")
	}
	// The call the janitor pass makes for every ready automatic checkout it
	// finds without a loop, and the selection path makes for a dormant one.
	f.lc.ensureCoordinator(ctx, f.primaryGraph, checkout)

	health, err = f.lc.ViewsHealth(ctx)
	require.NoError(t, err)
	require.Len(t, health.CoordinatorStartFailures, 1,
		"the census counts the build loops this daemon runs and says nothing about the "+
			"checkout that has none; its view is missing and the payload explains nothing")
	require.Equal(t, f.automatic.CheckoutID, health.CoordinatorStartFailures[0].CheckoutID)
	require.Equal(t, checkout.RootPath, health.CoordinatorStartFailures[0].RootPath,
		"the stated reason does not name the working copy that has no view")
	require.Contains(t, health.CoordinatorStartFailures[0].Reason, "freeze the index configuration",
		"the stated reason does not name what failed")

	// And the census is about the daemon's present, not its history: a
	// checkout that got its loop back stops being reported.
	f.lc.configSnapshot = nil
	f.lc.ensureCoordinator(ctx, f.primaryGraph, checkout)
	health, err = f.lc.ViewsHealth(ctx)
	require.NoError(t, err)
	require.Empty(t, health.CoordinatorStartFailures,
		"a checkout that recovered its build loop is still reported as having none")
	require.Positive(t, health.Coordinators,
		"the recovered build loop is not counted, so the reason and the count disagree")
}
