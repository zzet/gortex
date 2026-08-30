package store_sqlite

import (
	"context"
	"errors"
	"testing"
	"time"
)

func seedReadyCacheRefView(
	t *testing.T,
	catalog *Catalog,
	key ReadyGenerationCacheKey,
	suffix string,
) (RefView, RefViewBuild) {
	t.Helper()
	ctx := context.Background()
	checkoutID := "ready-ref-owner-" + suffix
	upsertReadyCacheCheckout(t, catalog, checkoutID)
	if err := catalog.UpsertDedicatedGraph(ctx, DedicatedGraph{
		GraphID:         key.GraphID,
		OwnerCheckoutID: checkoutID,
		RepoPrefix:      "ready-ref-repo",
		FamilyID:        "ready-cache-family",
		IsPrimaryBase:   true,
		State:           "graph_ready",
	}); err != nil {
		t.Fatalf("upsert dedicated graph: %v", err)
	}
	viewID := "ready-ref-view-" + suffix
	view, err := catalog.GetOrCreateRefView(ctx, RefView{
		RefViewID:         viewID,
		GraphID:           key.GraphID,
		SelectorKind:      "git_ref",
		SelectorValue:     "refs/heads/feature-" + suffix,
		EnrichmentProfile: "default",
		State:             RefViewPending,
		ExactView:         true,
	})
	if err != nil {
		t.Fatalf("create ref view: %v", err)
	}
	fingerprint := "ready-ref-fingerprint-" + suffix
	if err := catalog.UpdateRefViewDesire(ctx, UpdateRefViewDesireRequest{
		RefViewID:               view.RefViewID,
		DesiredRef:              view.SelectorValue,
		DesiredCommit:           "ready-ref-commit-" + suffix,
		DesiredTree:             key.TreeOID,
		DesiredBuildFingerprint: fingerprint,
		State:                   RefViewBuilding,
		LastResolved:            10,
		LastSelected:            10,
	}); err != nil {
		t.Fatalf("desire ref view: %v", err)
	}
	view, found, err := catalog.GetRefView(ctx, view.RefViewID)
	if err != nil || !found {
		t.Fatalf("read desired ref view: found=%v err=%v", found, err)
	}
	build := RefViewBuild{
		BuildID:            "ready-ref-build-" + suffix,
		RefViewID:          view.RefViewID,
		DesiredRef:         view.DesiredRef,
		DesiredCommit:      view.DesiredCommit,
		DesiredTree:        view.DesiredTree,
		BaseGenerationID:   key.BaseGenerationID,
		EnrichmentProfile:  view.EnrichmentProfile,
		BuildFingerprint:   view.DesiredBuildFingerprint,
		CapturedRouteEpoch: view.RouteEpoch,
		State:              ViewGenerationBuilding,
		BuildToken:         "ready-ref-token-" + suffix,
		CreatedAt:          10,
		LastProgress:       10,
	}
	claimed, err := catalog.ClaimRefViewBuild(ctx, build, 0)
	if err != nil {
		t.Fatalf("claim ref-view build: %v", err)
	}
	return view, claimed
}

func claimReadyCacheSourceGeneration(
	t *testing.T,
	catalog *Catalog,
	key ReadyGenerationCacheKey,
	generationID int64,
) ReadyGenerationClaim {
	t.Helper()
	claim, found, err := catalog.ClaimReadyGeneration(context.Background(), ClaimReadyGenerationRequest{
		Key:                   key,
		CandidateGenerationID: generationID,
		RequiredCapabilities:  []string{readyGenerationSourceSnapshotCapability},
	})
	if err != nil || !found {
		t.Fatalf("claim source-complete generation: found=%v claim=%+v err=%v", found, claim, err)
	}
	return claim
}

func bindReadyCacheRefViewRequest(
	view RefView,
	build RefViewBuild,
	key ReadyGenerationCacheKey,
	claim ReadyGenerationClaim,
) BindReadyGenerationLeaseToRefViewRequest {
	return BindReadyGenerationLeaseToRefViewRequest{
		Key:                             key,
		LeaseToken:                      claim.LeaseToken,
		GenerationID:                    claim.WinnerGenerationID,
		RefViewID:                       view.RefViewID,
		ExpectedRouteEpoch:              build.CapturedRouteEpoch,
		ExpectedDesiredTree:             build.DesiredTree,
		ExpectedDesiredBuildFingerprint: build.BuildFingerprint,
		BuildID:                         build.BuildID,
		BuildToken:                      build.BuildToken,
		ActiveRef:                       build.DesiredRef,
		ActiveCommit:                    build.DesiredCommit,
		ActiveTree:                      build.DesiredTree,
		ActiveBuildFingerprint:          build.BuildFingerprint,
		ExactView:                       true,
	}
}

func TestReadyGenerationClaimRequiresCompleteCapabilities(t *testing.T) {
	store := openCatalogStore(t)
	catalog := store.Catalog()
	key := readyCacheTestKey("ready-capability-graph", 0)
	generationID := createReadyCacheGeneration(t, catalog, key,
		"checkout-layer", "ready-capability-checkout", "ready-capability-layer", "")

	claim := claimReadyCacheSourceGeneration(t, catalog, key, generationID)
	if err := catalog.ReleaseReadyGenerationLease(context.Background(), claim.LeaseToken); err != nil {
		t.Fatalf("release source-complete claim: %v", err)
	}
	if err := catalog.WithdrawProducer(context.Background(), generationID,
		readyGenerationSourceSnapshotCapability, "test withdrawal"); err != nil {
		t.Fatalf("withdraw source snapshot: %v", err)
	}

	miss, found, err := catalog.ClaimReadyGeneration(context.Background(), ClaimReadyGenerationRequest{
		Key:                  key,
		RequiredCapabilities: []string{readyGenerationSourceSnapshotCapability},
	})
	if err != nil {
		t.Fatalf("capability-aware miss: %v", err)
	}
	if found || !miss.CapabilityMiss {
		t.Fatalf("claim = %+v found=%v, want a labeled capability miss", miss, found)
	}
	structural, found, err := catalog.ClaimReadyGeneration(context.Background(), ClaimReadyGenerationRequest{Key: key})
	if err != nil || !found || structural.WinnerGenerationID != generationID {
		t.Fatalf("structural claim = %+v found=%v err=%v, want generation %d", structural, found, err, generationID)
	}
	if err := catalog.ReleaseReadyGenerationLease(context.Background(), structural.LeaseToken); err != nil {
		t.Fatalf("release structural claim: %v", err)
	}
}

func TestBindReadyGenerationLeaseToRefViewPublishesAttemptAtomically(t *testing.T) {
	store := openCatalogStore(t)
	catalog := store.Catalog()
	key := readyCacheTestKey("ready-ref-bind-graph", 0)
	view, build := seedReadyCacheRefView(t, catalog, key, "publish")
	generationID := createReadyCacheGeneration(t, catalog, key,
		"checkout-layer", "checkout-publisher", "commit:publisher", "")
	claim := claimReadyCacheSourceGeneration(t, catalog, key, generationID)

	if err := catalog.BindReadyGenerationLeaseToRefView(context.Background(),
		bindReadyCacheRefViewRequest(view, build, key, claim)); err != nil {
		t.Fatalf("bind ready generation to ref view: %v", err)
	}
	stored, found, err := catalog.GetRefView(context.Background(), view.RefViewID)
	if err != nil || !found {
		t.Fatalf("read bound ref view: found=%v err=%v", found, err)
	}
	if stored.ActiveGenerationID != generationID || stored.State != RefViewReady ||
		stored.RouteEpoch != view.RouteEpoch+1 || stored.ActiveTree != key.TreeOID {
		t.Fatalf("bound ref view = %+v, want generation %d at epoch %d", stored, generationID, view.RouteEpoch+1)
	}
	attempt, found, err := catalog.GetRefViewBuild(context.Background(), build.BuildID)
	if err != nil || !found {
		t.Fatalf("read completed attempt: found=%v err=%v", found, err)
	}
	if attempt.State != ViewGenerationReady || attempt.GenerationID != generationID {
		t.Fatalf("completed attempt = %+v, want ready on generation %d", attempt, generationID)
	}
	if err := catalog.ReleaseReadyGenerationLease(context.Background(), claim.LeaseToken); err != nil {
		t.Fatalf("consumed lease release was not idempotent: %v", err)
	}
}

func TestBindReadyGenerationLeaseToRefViewRejectsStaleHandoffs(t *testing.T) {
	tests := []struct {
		name  string
		stale func(*testing.T, *Catalog, RefView, RefViewBuild, ReadyGenerationClaim)
	}{
		{
			name: "build token",
			stale: func(t *testing.T, _ *Catalog, _ RefView, _ RefViewBuild, claim ReadyGenerationClaim) {
				_ = claim
			},
		},
		{
			name: "expired lease",
			stale: func(t *testing.T, catalog *Catalog, _ RefView, _ RefViewBuild, claim ReadyGenerationClaim) {
				if _, err := catalog.store.writerDB.ExecContext(context.Background(), `
					UPDATE ready_generation_leases SET expires_at = unixepoch() - 1 WHERE lease_token = ?
				`, claim.LeaseToken); err != nil {
					t.Fatalf("expire lease: %v", err)
				}
			},
		},
		{
			name: "source withdrawal",
			stale: func(t *testing.T, catalog *Catalog, _ RefView, _ RefViewBuild, claim ReadyGenerationClaim) {
				if err := catalog.WithdrawProducer(context.Background(), claim.WinnerGenerationID,
					readyGenerationSourceSnapshotCapability, "test withdrawal"); err != nil {
					t.Fatalf("withdraw source: %v", err)
				}
			},
		},
		{
			name: "moved route",
			stale: func(t *testing.T, catalog *Catalog, view RefView, _ RefViewBuild, _ ReadyGenerationClaim) {
				if err := catalog.UpdateRefViewDesire(context.Background(), UpdateRefViewDesireRequest{
					RefViewID:               view.RefViewID,
					DesiredRef:              view.DesiredRef,
					DesiredCommit:           "moved-commit",
					DesiredTree:             "moved-tree",
					DesiredBuildFingerprint: "moved-fingerprint",
					State:                   RefViewBuilding,
					LastResolved:            time.Now().Unix(),
					LastSelected:            time.Now().Unix(),
				}); err != nil {
					t.Fatalf("move ref route: %v", err)
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := openCatalogStore(t)
			catalog := store.Catalog()
			key := readyCacheTestKey("ready-ref-stale-graph-"+test.name, 0)
			view, build := seedReadyCacheRefView(t, catalog, key, test.name)
			generationID := createReadyCacheGeneration(t, catalog, key,
				"checkout-layer", "stale-checkout-"+test.name, "stale-layer-"+test.name, "")
			claim := claimReadyCacheSourceGeneration(t, catalog, key, generationID)
			req := bindReadyCacheRefViewRequest(view, build, key, claim)
			if test.name == "build token" {
				req.BuildToken = "wrong-build-token"
			}
			test.stale(t, catalog, view, build, claim)
			err := catalog.BindReadyGenerationLeaseToRefView(context.Background(), req)
			if !errors.Is(err, ErrCatalogStaleGuard) {
				t.Fatalf("bind error = %v, want ErrCatalogStaleGuard", err)
			}
			stored, found, readErr := catalog.GetRefView(context.Background(), view.RefViewID)
			if readErr != nil || !found {
				t.Fatalf("read stale ref view: found=%v err=%v", found, readErr)
			}
			if stored.ActiveGenerationID != 0 {
				t.Fatalf("stale bind published generation %d: %+v", stored.ActiveGenerationID, stored)
			}
			attempt, found, readErr := catalog.GetRefViewBuild(context.Background(), build.BuildID)
			if readErr != nil || !found {
				t.Fatalf("read stale attempt: found=%v err=%v", found, readErr)
			}
			if attempt.State != ViewGenerationBuilding {
				t.Fatalf("stale bind completed attempt: %+v", attempt)
			}
			_ = catalog.ReleaseReadyGenerationLease(context.Background(), claim.LeaseToken)
		})
	}
}
