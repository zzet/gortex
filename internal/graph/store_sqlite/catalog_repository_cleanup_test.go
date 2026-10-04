package store_sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
)

type repositoryCleanupFixture struct {
	store   *Store
	catalog *Catalog
	path    string
	graph   DedicatedGraph
	owner   Checkout
}

func newRepositoryCleanupFixture(t *testing.T) *repositoryCleanupFixture {
	t.Helper()
	root := t.TempDir()
	f := &repositoryCleanupFixture{path: filepath.Join(root, "catalog.sqlite")}
	var err error
	f.store, err = openPristine(t, f.path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.store.Close() })
	f.catalog = f.store.Catalog()
	ctx := context.Background()
	family := RepositoryFamily{FamilyID: "family", CommonDirIdentity: filepath.Join(root, ".git"), State: "active"}
	if err := f.catalog.UpsertRepositoryFamily(ctx, family); err != nil {
		t.Fatal(err)
	}
	f.owner = Checkout{
		CheckoutID: "owner", Incarnation: "incarnation", FamilyID: family.FamilyID,
		RootPath: root, GitDir: family.CommonDirIdentity, AdminName: "main",
		State: CheckoutStateReady, DesiredMode: CheckoutModeDedicated, EffectiveMode: CheckoutModeDedicated,
	}
	if err := f.catalog.UpsertCheckout(ctx, f.owner); err != nil {
		t.Fatal(err)
	}
	f.graph = DedicatedGraph{GraphID: "graph", OwnerCheckoutID: f.owner.CheckoutID, RepoPrefix: "repo", FamilyID: family.FamilyID, State: "ready"}
	if err := f.catalog.UpsertDedicatedGraph(ctx, f.graph); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *repositoryCleanupFixture) generation(state ViewGenerationState) ViewGeneration {
	return ViewGeneration{GraphID: f.graph.GraphID, OwnerKind: "dedicated_graph", CheckoutID: f.owner.CheckoutID, GenerationKind: "dedicated_base", State: state}
}

func TestCatalogRepositoryCleanupWithdrawsAndRestoresExactOwner(t *testing.T) {
	f := newRepositoryCleanupFixture(t)
	ctx := context.Background()
	id, err := f.catalog.CreateViewGeneration(ctx, f.generation(ViewGenerationReady))
	if err != nil {
		t.Fatal(err)
	}
	f.graph.ActiveGenerationID = id
	if err := f.catalog.UpsertDedicatedGraph(ctx, f.graph); err != nil {
		t.Fatal(err)
	}
	identity, found, err := f.catalog.BeginRepositoryCleanup(ctx, f.graph.GraphID)
	if err != nil || !found {
		t.Fatalf("close: found=%v err=%v", found, err)
	}
	if identity.GraphID != f.graph.GraphID || identity.CheckoutID != f.owner.CheckoutID || identity.Incarnation != f.owner.Incarnation || identity.RepoPrefix != f.graph.RepoPrefix || identity.RootPath != f.owner.RootPath {
		t.Fatalf("identity changed: %+v", identity)
	}
	closed, found, err := f.catalog.GetDedicatedGraph(ctx, f.graph.GraphID)
	if err != nil || !found || closed.State != DedicatedGraphClosing || closed.ActiveGenerationID != 0 || closed.OwnerCheckoutID != f.owner.CheckoutID {
		t.Fatalf("not withdrawn with retained owner: %+v found=%v err=%v", closed, found, err)
	}
	if err := f.store.Close(); err != nil {
		t.Fatal(err)
	}
	f.store, err = Open(f.path)
	if err != nil {
		t.Fatal(err)
	}
	f.catalog = f.store.Catalog()
	owners, err := f.catalog.ListRepositoryCleanupOwners(ctx)
	if err != nil || len(owners) != 1 || owners[0] != identity {
		t.Fatalf("restart lost closure identity: %+v err=%v", owners, err)
	}
	again, found, err := f.catalog.BeginRepositoryCleanup(ctx, f.graph.GraphID)
	if err != nil || !found || again != identity {
		t.Fatalf("close not idempotent: %+v %v %v", again, found, err)
	}
}

func TestCatalogRepositoryCleanupFencesZeroGenerationAdmissions(t *testing.T) {
	f := newRepositoryCleanupFixture(t)
	ctx := context.Background()
	ref := RefView{RefViewID: "ref", GraphID: f.graph.GraphID, SelectorKind: "git_ref", SelectorValue: "refs/heads/main", State: RefViewPending, ExactView: true, EnrichmentProfile: "structural"}
	if err := f.catalog.UpsertRefView(ctx, ref); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.catalog.BeginRepositoryCleanup(ctx, f.graph.GraphID); err != nil {
		t.Fatal(err)
	}
	checks := map[string]func() error{
		"create_generation": func() error {
			_, err := f.catalog.CreateViewGeneration(ctx, f.generation(ViewGenerationBuilding))
			return err
		},
		"adopt_or_create_generation": func() error {
			_, _, err := f.catalog.AdoptOrCreateViewGeneration(ctx, f.generation(ViewGenerationBuilding))
			return err
		},
		"upsert_zero_ref":       func() error { return f.catalog.UpsertRefView(ctx, ref) },
		"get_existing_zero_ref": func() error { _, err := f.catalog.GetOrCreateRefView(ctx, ref); return err },
		"upsert_zero_route": func() error {
			return f.catalog.UpsertCheckoutRoute(ctx, CheckoutRoute{CheckoutID: f.owner.CheckoutID, GraphID: f.graph.GraphID, State: RouteActive})
		},
		"reopen_identity": func() error { return f.catalog.UpsertDedicatedGraph(ctx, f.graph) },
		"new_ref_desire": func() error {
			return f.catalog.UpdateRefViewDesire(ctx, UpdateRefViewDesireRequest{RefViewID: ref.RefViewID, DesiredRef: ref.SelectorValue, DesiredCommit: "commit", DesiredTree: "tree", DesiredBuildFingerprint: "fingerprint", State: RefViewBuilding})
		},
		"new_ref_claim": func() error {
			_, err := f.catalog.ClaimRefViewBuild(ctx, RefViewBuild{BuildID: "build", RefViewID: ref.RefViewID, DesiredRef: ref.SelectorValue, DesiredCommit: "commit", DesiredTree: "tree", BuildFingerprint: "fingerprint", State: ViewGenerationBuilding, BuildToken: "token", CreatedAt: 1, LastProgress: 1}, 0)
			return err
		},
	}
	for name, check := range checks {
		t.Run(name, func(t *testing.T) {
			if err := check(); !errors.Is(err, ErrCatalogGraphClosing) {
				t.Fatalf("closing admission not refused: %v", err)
			}
		})
	}
}

func TestCatalogRepositoryCleanupEnumerationIsCheckedAndUncapped(t *testing.T) {
	f := newRepositoryCleanupFixture(t)
	ctx := context.Background()
	if _, err := f.catalog.ListRepositoryCleanupGenerations(ctx, f.graph.GraphID); !errors.Is(err, ErrCatalogStaleGuard) {
		t.Fatalf("open graph enumerated for deletion: %v", err)
	}
	const count = 1103
	var previous int64
	for n := 0; n < count; n++ {
		id, err := f.catalog.CreateViewGeneration(ctx, f.generation(ViewGenerationBuilding))
		if err != nil {
			t.Fatal(err)
		}
		previous = id
	}
	if _, _, err := f.catalog.BeginRepositoryCleanup(ctx, f.graph.GraphID); err != nil {
		t.Fatal(err)
	}
	ids, err := f.catalog.ListRepositoryCleanupGenerations(ctx, f.graph.GraphID)
	if err != nil || len(ids) != count {
		t.Fatalf("truncated cleanup enumeration: len=%d err=%v", len(ids), err)
	}
	if ids[0] != previous {
		t.Fatal("cleanup is not newest first")
	}
	for n := 1; n < len(ids); n++ {
		if ids[n] >= ids[n-1] {
			t.Fatal("cleanup order not strictly descending")
		}
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if ids, err := f.catalog.ListRepositoryCleanupGenerations(cancelled, f.graph.GraphID); err == nil || ids != nil {
		t.Fatalf("query failure became partial/success: %v %v", ids, err)
	}
}

func TestCatalogRepositoryCleanupFenceRollsBackWithAuthorization(t *testing.T) {
	f := newRepositoryCleanupFixture(t)
	ctx := context.Background()
	rollback := errors.New("authorization write failed")
	err := f.catalog.withTx(ctx, func(tx *sql.Tx) error {
		if err := closeDedicatedGraphAdmissionTx(ctx, tx, f.graph.GraphID); err != nil {
			return err
		}
		return rollback
	})
	if !errors.Is(err, rollback) {
		t.Fatal(err)
	}
	graph, found, err := f.catalog.GetDedicatedGraph(ctx, f.graph.GraphID)
	if err != nil || !found || graph.State != "ready" {
		t.Fatalf("failed authorization closed graph: %+v %v %v", graph, found, err)
	}
	if _, err := f.catalog.CreateViewGeneration(ctx, f.generation(ViewGenerationBuilding)); err != nil {
		t.Fatal(err)
	}
}

func TestCatalogRepositoryCleanupPreservesUncataloguedGenerationContract(t *testing.T) {
	f := newRepositoryCleanupFixture(t)
	for _, graphID := range []string{"", "uncatalogued"} {
		t.Run(fmt.Sprintf("graph=%q", graphID), func(t *testing.T) {
			generation := f.generation(ViewGenerationBuilding)
			generation.GraphID = graphID
			if _, err := f.catalog.CreateViewGeneration(context.Background(), generation); err != nil {
				t.Fatalf("expanded generic allocation contract: %v", err)
			}
		})
	}
}

func TestCatalogRepositoryCleanupFencesChildWithDifferentGraphID(t *testing.T) {
	f := newRepositoryCleanupFixture(t)
	ctx := context.Background()
	baseID, err := f.catalog.CreateViewGeneration(ctx, f.generation(ViewGenerationReady))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.catalog.BeginRepositoryCleanup(ctx, f.graph.GraphID); err != nil {
		t.Fatal(err)
	}
	child := f.generation(ViewGenerationBuilding)
	child.GraphID = "uncatalogued-child"
	child.BaseGenerationID = baseID
	if _, err := f.catalog.CreateViewGeneration(ctx, child); !errors.Is(err, ErrCatalogGraphClosing) {
		t.Fatalf("closing parent admitted through another graph identity: %v", err)
	}
}

func TestCatalogRepositoryCleanupPublicationReleaseRequiresExactClosedOwner(t *testing.T) {
	f := newRepositoryCleanupFixture(t)
	ctx := context.Background()
	expected := RepositoryCleanupIdentity{GraphID: f.graph.GraphID, CheckoutID: f.owner.CheckoutID, Incarnation: f.owner.Incarnation, RepoPrefix: f.graph.RepoPrefix, FamilyID: f.graph.FamilyID, RootPath: f.owner.RootPath}
	if err := f.catalog.ReleaseRepositoryCleanupPublication(ctx, expected); !errors.Is(err, ErrCatalogStaleGuard) {
		t.Fatalf("open publication released: %v", err)
	}
	identity, found, err := f.catalog.BeginRepositoryCleanup(ctx, f.graph.GraphID)
	if err != nil || !found {
		t.Fatalf("close: %v %v", found, err)
	}
	stale := identity
	stale.Incarnation = "old-incarnation"
	if err := f.catalog.ReleaseRepositoryCleanupPublication(ctx, stale); !errors.Is(err, ErrCatalogStaleGuard) {
		t.Fatalf("stale cleanup owner accepted: %v", err)
	}
	for n := 0; n < 2; n++ {
		if err := f.catalog.ReleaseRepositoryCleanupPublication(ctx, identity); err != nil {
			t.Fatal(err)
		}
	}
	graph, found, err := f.catalog.GetDedicatedGraph(ctx, f.graph.GraphID)
	if err != nil || !found || graph.State != DedicatedGraphClosing || graph.OwnerCheckoutID != identity.CheckoutID {
		t.Fatalf("cleanup identity removed before saga completion: %+v %v %v", graph, found, err)
	}
}
