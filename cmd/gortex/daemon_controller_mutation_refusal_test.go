package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/daemon"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/indexer"
)

func TestFileCoveragePreRouteFailureIsNotAnObservedMutationRefusal(t *testing.T) {
	f := newProbeFixture(t)
	f.routeWorktree(t)
	route, found, err := f.catalog.GetCheckoutRoute(t.Context(), probeWorktreeID)
	if err != nil || !found {
		t.Fatalf("route: %v/%v", found, err)
	}
	advance := func() {
		t.Helper()
		if err := f.catalog.FlipCheckoutRoute(t.Context(), store_sqlite.FlipCheckoutRouteRequest{
			CheckoutID: route.CheckoutID, ExpectedRouteEpoch: route.RouteEpoch,
			GraphID: route.GraphID, CommitGenerationID: route.CommitGenerationID,
			DirtyGenerationID: route.DirtyGenerationID, State: store_sqlite.RouteActive,
		}); err != nil {
			t.Fatal(err)
		}
		route.RouteEpoch++
	}
	advance()
	// This controller fixture has a catalog route with no live checkout
	// directory. Admission fails BEFORE observing the route: this is unknown
	// refusal evidence and must not be attached to the later coverage route.
	// The independently resolved/pinned coverage still proves a healthy
	// graph route with no applicable refusal mark (ready, not read_only).
	lease, refusal := f.controller.lifecycle.BeginCheckoutMutation(t.Context(), probeWorktreeID, f.worktreeRoot, route.RouteEpoch)
	if lease != nil {
		lease.Close()
	}
	if !errors.Is(refusal, indexer.ErrCheckoutMutationStale) {
		t.Fatalf("admission=%v", refusal)
	}
	f.controller.lifecycle.RecordCheckoutMutationRefusal(t.Context(), refusal)
	probe := func(root string) daemon.FileCoverageResult {
		t.Helper()
		coverage, err := f.controller.FileCoverage(t.Context(), daemon.FileCoverageParams{Path: filepath.Join(root, probeFile)})
		if err != nil {
			t.Fatal(err)
		}
		return coverage
	}
	coverage := probe(f.worktreeRoot)
	if !coverage.Covered || coverage.MutationState != "ready" || coverage.MutationReason != "" {
		t.Fatalf("failure before observing a route cannot arm a refusal mark: %+v", coverage)
	}
	if primary := probe(f.primaryRoot); !primary.Covered || primary.Symbols != 2 || primary.View == nil || !primary.View.Exact || primary.View.CheckoutID != probePrimaryID || primary.MutationState != "ready" || primary.MutationReason != "" {
		t.Fatalf("worktree refusal affected primary coverage/identity/readiness: %+v", primary)
	}
	advance()
	f.controller.lifecycle.RecordCheckoutMutationRefusal(t.Context(), refusal)
	if fresh := probe(f.worktreeRoot); !fresh.Covered || fresh.MutationState != "ready" || fresh.MutationReason != "" {
		t.Fatalf("late refusal survived fresh route: %+v", fresh)
	}
	f.controller.lifecycle.RecordCheckoutMutationRefusal(t.Context(), indexer.ErrCheckoutMutationBusy)
	if busy := probe(f.worktreeRoot); busy.MutationState != "ready" {
		t.Fatal("Busy became read_only")
	}
}

// Exercise the production controller and lifecycle together over tiny owned
// Git checkouts. The held physical build lane deterministically leaves the
// old route serving reads while changed source makes real admission stale.
func TestFileCoverageObservedStaleRefusalAndSuccessfulAdmission(t *testing.T) {
	c, _, catalog, dir := buildCatalogController(t)
	t.Cleanup(func() { _ = c.lifecycle.Close() })
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	primary := filepath.Join(dir, "primary-live")
	worktree := filepath.Join(dir, "worktree-live")
	wtGit(t, dir, "init", "-q", "-b", "main", "--", "primary-live")
	if err := os.WriteFile(filepath.Join(primary, "edit.go"), []byte("package fixture\nfunc Original() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	wtGit(t, primary, "add", ".")
	wtGit(t, primary, "commit", "-q", "-m", "initial owned fixture")
	wtGit(t, primary, "worktree", "add", "-b", "fixture-edit", worktree)
	gate := indexer.NewViewBuildGate()
	gate.Open()
	c.lifecycle.SetBuildGate(gate)
	registered, err := c.lifecycle.Register(ctx, config.RepoEntry{Path: primary, Name: "fixture"}, indexer.TrackSourceCLI)
	require.NoError(t, err)
	require.NoError(t, registered.CatalogErr)
	binding, err := c.lifecycle.ExplainView(ctx, worktree)
	require.NoError(t, err)
	require.True(t, binding.Matched)
	require.NotEmpty(t, binding.CheckoutID)
	require.True(t, c.lifecycle.ActivateCheckout(binding.CheckoutID, "coverage refusal test"))
	var route store_sqlite.CheckoutRoute
	require.Eventually(t, func() bool {
		var found bool
		var routeErr error
		route, found, routeErr = catalog.GetCheckoutRoute(ctx, binding.CheckoutID)
		return routeErr == nil && found && route.State == store_sqlite.RouteActive && route.CommitGenerationID > 0 && route.DirtyGenerationID > 0
	}, 20*time.Second, 10*time.Millisecond)
	store := c.graph.(*store_sqlite.Store)
	c.viewMaterializer = &graphview.Materializer{Store: store, Catalog: catalog, Leases: c.lifecycle.ViewLeases()}
	release, err := gate.Acquire(ctx, indexer.ViewBuildInteractive)
	require.NoError(t, err)
	release = sync.OnceFunc(release)
	t.Cleanup(release)
	require.NoError(t, os.WriteFile(filepath.Join(worktree, "edit.go"), []byte("package fixture\nfunc ChangedOutsideGraph() {}\n"), 0o644))
	ticket, err := c.lifecycle.RequestCheckoutRefresh(ctx, binding.CheckoutID, worktree)
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		stats := gate.Stats()
		return stats.InteractiveQueued+stats.BackgroundQueued > 0
	}, 10*time.Second, time.Millisecond, "coordinator must hold its cycle lock waiting on the owned lane")
	lease, refusal := c.lifecycle.BeginCheckoutMutation(ctx, binding.CheckoutID, worktree, route.RouteEpoch)
	if lease != nil {
		lease.Close()
	}
	require.ErrorIs(t, refusal, indexer.ErrCheckoutMutationStale)
	c.lifecycle.RecordCheckoutMutationRefusal(ctx, refusal)
	coverage, err := c.FileCoverage(ctx, daemon.FileCoverageParams{Path: filepath.Join(worktree, "edit.go")})
	require.NoError(t, err)
	require.True(t, coverage.Covered)
	require.Equal(t, "read_only", coverage.MutationState)
	require.NotEmpty(t, coverage.MutationReason)
	primaryCoverage, err := c.FileCoverage(ctx, daemon.FileCoverageParams{Path: filepath.Join(primary, "edit.go")})
	require.NoError(t, err)
	require.NotEqual(t, "read_only", primaryCoverage.MutationState)
	release()
	select {
	case result := <-ticket.Ticket.Done:
		require.NoError(t, result.Err)
	case <-ctx.Done():
		t.Fatal("fresh route did not publish")
	}
	route, found, err := catalog.GetCheckoutRoute(ctx, binding.CheckoutID)
	require.NoError(t, err)
	require.True(t, found)
	lease, err = c.lifecycle.BeginCheckoutMutation(ctx, binding.CheckoutID, worktree, route.RouteEpoch)
	require.NoError(t, err)
	lease.Close()
	c.lifecycle.RecordCheckoutMutationRefusal(ctx, refusal)
	fresh, err := c.FileCoverage(ctx, daemon.FileCoverageParams{Path: filepath.Join(worktree, "edit.go")})
	require.NoError(t, err)
	require.Equal(t, "ready", fresh.MutationState)
	require.Empty(t, fresh.MutationReason)
}
