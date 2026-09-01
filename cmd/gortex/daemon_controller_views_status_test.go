package main

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/daemon"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/viewmetrics"
)

// The views block on `daemon status`, and the probe counter beside it.
//
// The block is the health surface's answer to "what does the view lifecycle
// currently hold": counts by state and nothing else. What it must never do is
// carry an identity — a status payload that grew a key per worktree would be
// unbounded in exactly the dimension the metric registry is bounded in.

func probeAnswerDelta(before, after viewmetrics.Snapshot, kind, exact string) int64 {
	key := viewmetrics.ProbeAnswerTotal + "{kind=" + kind + ",exact=" + exact + "}"
	return after.Counters[key] - before.Counters[key]
}

// TestViewsStatusCountsTheCatalogByState pins the block's shape: the six
// levels, keyed by the catalog's own state vocabulary.
func TestViewsStatusCountsTheCatalogByState(t *testing.T) {
	f := newProbeFixture(t)
	f.routeWorktree(t)

	views := f.controller.collectViewsStatus(context.Background())
	require.NotNil(t, views, "a controller with a lifecycle serves no views block")

	assert.Equal(t, 1, views.Families)
	assert.Equal(t, 2, views.Checkouts[string(store_sqlite.CheckoutStateReady)],
		"the primary and the worktree are both ready: %v", views.Checkouts)
	assert.Equal(t, 3, views.Generations[string(store_sqlite.ViewGenerationReady)],
		"the active base plus route commit and dirty generations are published: %v", views.Generations)
	assert.Equal(t, 0, views.Leases, "no view is materialized")
	require.NotNil(t, views.BuildQueue)
	assert.True(t, views.BuildQueue.Open)
	assert.False(t, views.BuildQueue.Active)
	assert.Empty(t, views.RefViews, "this family has no ref views")
	assert.NotEmpty(t, views.Counters, "the metric registry is not reaching the status block")
}

// TestViewsStatusCarriesNoIdentities is the cardinality law applied to the
// payload: every key in the block is a state or a series name, so a daemon
// serving a hundred worktrees renders the same keys as one serving two.
func TestViewsStatusCarriesNoIdentities(t *testing.T) {
	f := newProbeFixture(t)
	f.routeWorktree(t)

	views := f.controller.collectViewsStatus(context.Background())
	require.NotNil(t, views)

	body, err := json.Marshal(views)
	require.NoError(t, err)
	rendered := string(body)
	for _, identity := range []string{
		probeWorktreeID, probePrimaryID, probeGraphID, probeFamily, f.worktreeRoot,
	} {
		assert.NotContains(t, rendered, identity,
			"the views block names an identity; counts belong here and ids belong in `gortex checkouts`")
	}
}

// TestViewsStatusIsOmittedWithoutALifecycle pins the degradation: a daemon
// with no view catalog omits the block rather than rendering a zeroed one,
// because zero families and "we never looked" are different answers.
func TestViewsStatusIsOmittedWithoutALifecycle(t *testing.T) {
	f := newProbeFixture(t)
	f.controller.lifecycle = nil
	assert.Nil(t, f.controller.collectViewsStatus(context.Background()))
}

// TestViewsStatusIsSkippedOnceTheBudgetIsGone pins the other degradation: the
// block is a report, and a status call whose caller has already given up must
// not spend its last milliseconds listing the catalog for it.
func TestViewsStatusIsSkippedOnceTheBudgetIsGone(t *testing.T) {
	f := newProbeFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	assert.Nil(t, f.controller.collectViewsStatus(ctx))
}

// TestRoutedProbeIsCountedAsAnExactWorktreeAnswer pins the probe seam's happy
// path: a file inside a routed worktree is answered from that working copy's
// own composed view, and the counter says so.
func TestRoutedProbeIsCountedAsAnExactWorktreeAnswer(t *testing.T) {
	f := newProbeFixture(t)
	f.routeWorktree(t)
	probed := filepath.Join(f.worktreeRoot, probeFile)

	before := viewmetrics.Read()
	_, err := f.controller.FileCoverage(context.Background(), daemon.FileCoverageParams{Path: probed})
	require.NoError(t, err)
	after := viewmetrics.Read()

	assert.Equal(t, int64(1),
		probeAnswerDelta(before, after, daemon.ProbeViewWorktree, viewmetrics.AnswerExact),
		"a routed worktree is answered exactly from its own composed view")
	assert.Equal(t, int64(0),
		probeAnswerDelta(before, after, daemon.ProbeViewUnrouted, viewmetrics.AnswerFallback))
}

// TestUnroutedProbeIsCountedAsItsOwnFallback pins the degradation the probe
// exists to make legible: a registered worktree with no composed view yet is
// answered by nothing at all, and that is a different fact from an answer that
// came from the base corpus.
func TestUnroutedProbeIsCountedAsItsOwnFallback(t *testing.T) {
	f := newProbeFixture(t)
	// An unrouted answer also asks for a reconciliation, on a goroutine the
	// probe deliberately does not wait for. Left live, that pass is still
	// reading and writing the catalog when the test body ends and the fixture
	// closes the store out from under it. What the nudge itself does is pinned
	// by TestUnroutedProbeBurstReconcilesOncePerWindow; here it is stubbed so
	// the counter is measured against nothing else running.
	f.controller.probeReconcile = func(string) {}
	probed := filepath.Join(f.worktreeRoot, probeFile)

	before := viewmetrics.Read()
	_, err := f.controller.FileCoverage(context.Background(), daemon.FileCoverageParams{Path: probed})
	require.NoError(t, err)
	after := viewmetrics.Read()

	assert.Equal(t, int64(1),
		probeAnswerDelta(before, after, daemon.ProbeViewUnrouted, viewmetrics.AnswerFallback),
		"an unrouted worktree is a fallback answer of its own kind")
	assert.Equal(t, int64(0),
		probeAnswerDelta(before, after, daemon.ProbeViewWorktree, viewmetrics.AnswerExact))
	assert.Equal(t, int64(0),
		probeAnswerDelta(before, after, daemon.ProbeViewBase, viewmetrics.AnswerFallback),
		"an unrouted worktree must not be reported as answered from the base")
}
