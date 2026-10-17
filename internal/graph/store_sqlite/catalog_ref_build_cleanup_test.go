package store_sqlite

import (
	"context"
	"errors"
	"testing"
)

func TestCatalogClosingRefBuildUpsertCannotCreateZeroGenerationAttempt(t *testing.T) {
	f := newRepositoryCleanupFixture(t)
	ctx := context.Background()
	ref := RefView{RefViewID: "closing-ref", GraphID: f.graph.GraphID, SelectorKind: "git_ref", SelectorValue: "refs/heads/main", State: RefViewPending, ExactView: true, EnrichmentProfile: "structural"}
	if err := f.catalog.UpsertRefView(ctx, ref); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.catalog.BeginRepositoryCleanup(ctx, f.graph.GraphID); err != nil {
		t.Fatal(err)
	}
	build := RefViewBuild{BuildID: "late", RefViewID: ref.RefViewID, DesiredRef: ref.SelectorValue, DesiredCommit: "commit", DesiredTree: "tree", BuildFingerprint: "policy", State: ViewGenerationBuilding, BuildToken: "late-token", CreatedAt: 1, LastProgress: 1}
	if err := f.catalog.UpsertRefViewBuild(ctx, build); !errors.Is(err, ErrCatalogGraphClosing) {
		t.Fatalf("zero-generation attempt crossed closing fence: %v", err)
	}
	if _, found, err := f.catalog.GetRefViewBuild(ctx, build.BuildID); err != nil || found {
		t.Fatalf("refused attempt persisted: found=%v err=%v", found, err)
	}
}

func TestCatalogClosingRefBuildStillAllowsTokenGuardedTerminalDrain(t *testing.T) {
	f := newRepositoryCleanupFixture(t)
	ctx := context.Background()
	ref := RefView{RefViewID: "ref", GraphID: f.graph.GraphID, SelectorKind: "git_ref", SelectorValue: "refs/heads/main", State: RefViewPending, ExactView: true, EnrichmentProfile: "structural"}
	if err := f.catalog.UpsertRefView(ctx, ref); err != nil {
		t.Fatal(err)
	}
	build := RefViewBuild{BuildID: "admitted", RefViewID: ref.RefViewID, DesiredRef: ref.SelectorValue, DesiredCommit: "commit", DesiredTree: "tree", BuildFingerprint: "policy", State: ViewGenerationBuilding, BuildToken: "owned-token", CreatedAt: 1, LastProgress: 1}
	if err := f.catalog.UpsertRefViewBuild(ctx, build); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.catalog.BeginRepositoryCleanup(ctx, f.graph.GraphID); err != nil {
		t.Fatal(err)
	}
	if err := f.catalog.TouchRefViewBuild(ctx, build.BuildID, build.BuildToken, 2); err != nil {
		t.Fatal(err)
	}
	wrong := CompleteRefViewBuildRequest{BuildID: build.BuildID, BuildToken: "foreign-token", State: ViewGenerationFailed, LastProgress: 3, Error: "closing"}
	if err := f.catalog.CompleteRefViewBuild(ctx, wrong); !errors.Is(err, ErrCatalogStaleGuard) {
		t.Fatalf("foreign completion allowed: %v", err)
	}
	owned := wrong
	owned.BuildToken = build.BuildToken
	if err := f.catalog.CompleteRefViewBuild(ctx, owned); err != nil {
		t.Fatalf("closing fence prevented admitted terminal cleanup: %v", err)
	}
	row, found, err := f.catalog.GetRefViewBuild(ctx, build.BuildID)
	if err != nil || !found || row.State != ViewGenerationFailed || row.Error != "closing" {
		t.Fatalf("terminal row=%+v found=%v err=%v", row, found, err)
	}
	if err := f.catalog.UpsertRefViewBuild(ctx, build); !errors.Is(err, ErrCatalogGraphClosing) {
		t.Fatalf("completed claim resurrected: %v", err)
	}
}

func TestCatalogClosingRefBuildCannotBeRetargetedToOpenGraph(t *testing.T) {
	f := newRepositoryCleanupFixture(t)
	ctx := context.Background()
	closedRef := RefView{RefViewID: "closed-ref", GraphID: f.graph.GraphID, SelectorKind: "git_ref", SelectorValue: "refs/heads/main", State: RefViewPending, ExactView: true, EnrichmentProfile: "structural"}
	healthyOwner := f.owner
	healthyOwner.CheckoutID, healthyOwner.Incarnation, healthyOwner.AdminName = "healthy-owner", "healthy-incarnation", "healthy-admin"
	healthyOwner.RootPath = t.TempDir()
	if err := f.catalog.UpsertCheckout(ctx, healthyOwner); err != nil {
		t.Fatal(err)
	}
	openGraph := DedicatedGraph{GraphID: "healthy", OwnerCheckoutID: healthyOwner.CheckoutID, RepoPrefix: "healthy", FamilyID: f.graph.FamilyID, State: "ready"}
	if err := f.catalog.UpsertDedicatedGraph(ctx, openGraph); err != nil {
		t.Fatal(err)
	}
	openRef := closedRef
	openRef.RefViewID, openRef.GraphID = "open-ref", openGraph.GraphID
	for _, ref := range []RefView{closedRef, openRef} {
		if err := f.catalog.UpsertRefView(ctx, ref); err != nil {
			t.Fatal(err)
		}
	}
	build := RefViewBuild{BuildID: "owned", RefViewID: closedRef.RefViewID, DesiredRef: closedRef.SelectorValue, DesiredCommit: "commit", DesiredTree: "tree", BuildFingerprint: "policy", State: ViewGenerationBuilding, BuildToken: "token", CreatedAt: 1, LastProgress: 1}
	if err := f.catalog.UpsertRefViewBuild(ctx, build); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.catalog.BeginRepositoryCleanup(ctx, f.graph.GraphID); err != nil {
		t.Fatal(err)
	}
	build.RefViewID = openRef.RefViewID
	if err := f.catalog.UpsertRefViewBuild(ctx, build); !errors.Is(err, ErrCatalogGraphClosing) {
		t.Fatalf("closing build escaped via identity replacement: %v", err)
	}
	build.BuildID, build.BuildToken = "independent", "independent-token"
	if err := f.catalog.UpsertRefViewBuild(ctx, build); err != nil {
		t.Fatalf("independent healthy graph harmed: %v", err)
	}
}
