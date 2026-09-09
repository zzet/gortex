package indexer

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

type graphBaseCatalogFixture struct {
	catalog   *store_sqlite.Catalog
	dedicated store_sqlite.DedicatedGraph
	want      primaryBase
}

// Each fixture owns a real SQLite catalog. No daemon, Git checkout, payload
// builder, or live tracking configuration participates in this guard test.
func newGraphBaseCatalogFixture(t testing.TB, scenario string) graphBaseCatalogFixture {
	t.Helper()
	root := t.TempDir()
	store, err := store_sqlite.Open(filepath.Join(root, "catalog.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	catalog := store.Catalog()
	ctx := context.Background()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	family := store_sqlite.RepositoryFamily{
		FamilyID: "guard-family", CommonDirIdentity: filepath.Join(root, "git-common"), State: "active",
	}
	must(catalog.UpsertRepositoryFamily(ctx, family))
	owner := store_sqlite.Checkout{
		CheckoutID: "guard-owner", Incarnation: "guard-owner-incarnation", FamilyID: family.FamilyID,
		RootPath: root, GitDir: family.CommonDirIdentity, AdminName: "main",
		State: store_sqlite.CheckoutStateReady, DesiredMode: store_sqlite.CheckoutModeDedicated,
		EffectiveMode: store_sqlite.CheckoutModeDedicated, HeadTree: strings.Repeat("a", 40),
	}
	must(catalog.UpsertCheckout(ctx, owner))
	dedicated := store_sqlite.DedicatedGraph{
		GraphID: "guard-graph", OwnerCheckoutID: owner.CheckoutID, RepoPrefix: "guard-repo",
		FamilyID: family.FamilyID, IsPrimaryBase: true, State: "ready",
	}
	must(catalog.UpsertDedicatedGraph(ctx, dedicated))
	fixture := graphBaseCatalogFixture{
		catalog: catalog, dedicated: dedicated,
		want: primaryBase{graphID: dedicated.GraphID, treeOID: owner.HeadTree},
	}
	if strings.HasPrefix(scenario, "legacy_zero_") {
		switch scenario {
		case "legacy_zero_missing_owner":
			// A caller may retain the prior DedicatedGraph value after its owner
			// is removed. Delete through normal APIs, not foreign-key bypasses.
			must(catalog.DeleteDedicatedGraph(ctx, dedicated.GraphID))
			must(catalog.DeleteCheckout(ctx, owner.CheckoutID))
		case "legacy_zero_empty_owner_tree":
			owner.HeadTree = ""
			must(catalog.UpsertCheckout(ctx, owner))
		}
		return fixture
	}
	if scenario == "positive_missing_generation_healthy_owner" {
		// graphBase validates the supplied value: model a stale/invalid caller
		// without asking normal catalog admission to persist a missing target.
		fixture.dedicated.ActiveGenerationID = 999999
		return fixture
	}
	row := store_sqlite.ViewGeneration{
		OwnerKind: "dedicated_graph", GraphID: dedicated.GraphID, CheckoutID: owner.CheckoutID,
		GenerationKind: "dedicated", TreeOID: strings.Repeat("b", 40),
		ConfigHash: "guard-config", ExtractorVersions: "guard-extractors", ResolverVersion: "guard-resolver",
		State: store_sqlite.ViewGenerationBuilding,
	}
	switch scenario {
	case "positive_wrong_owner_kind":
		row.OwnerKind = "ref_view"
	case "positive_wrong_generation_kind":
		row.GenerationKind = "commit"
	case "positive_empty_stored_tree":
		row.TreeOID = ""
	case "positive_wrong_graph", "positive_wrong_checkout":
		other := owner
		other.CheckoutID, other.Incarnation, other.AdminName = "other-owner", "other-incarnation", "other"
		other.RootPath = filepath.Join(root, "other")
		must(catalog.UpsertCheckout(ctx, other))
		otherGraph := dedicated
		otherGraph.GraphID, otherGraph.OwnerCheckoutID, otherGraph.RepoPrefix = "other-graph", other.CheckoutID, "other-repo"
		otherGraph.IsPrimaryBase = false
		must(catalog.UpsertDedicatedGraph(ctx, otherGraph))
		if scenario == "positive_wrong_graph" {
			row.GraphID = otherGraph.GraphID
		} else {
			row.CheckoutID = other.CheckoutID
		}
	}
	id, err := catalog.CreateViewGeneration(ctx, row)
	must(err)
	if id <= 0 {
		t.Fatalf("catalog assigned invalid generation id %d", id)
	}
	wantState := store_sqlite.ViewGenerationReady
	switch scenario {
	case "positive_building":
		wantState = store_sqlite.ViewGenerationBuilding
	case "positive_failed":
		wantState = store_sqlite.ViewGenerationFailed
		must(catalog.SetViewGenerationState(ctx, id, wantState, store_sqlite.ViewGenerationBuilding))
	default:
		must(catalog.PublishViewGeneration(ctx, id, 123))
		switch scenario {
		case "positive_after_real_ready_to_superseded_transition":
			wantState = store_sqlite.ViewGenerationSuperseded
			must(catalog.SetViewGenerationState(ctx, id, wantState, store_sqlite.ViewGenerationReady))
		case "positive_retiring":
			wantState = store_sqlite.ViewGenerationRetiring
			must(catalog.SetViewGenerationState(ctx, id, wantState, store_sqlite.ViewGenerationReady))
		}
	}
	stored, found, err := catalog.GetViewGeneration(ctx, id)
	must(err)
	if !found || stored.State != wantState || stored.TreeOID != row.TreeOID {
		t.Fatalf("fixture generation mismatch: found=%v row=%+v", found, stored)
	}
	fixture.dedicated.ActiveGenerationID = id
	if scenario != "positive_retiring" {
		must(catalog.UpsertDedicatedGraph(ctx, fixture.dedicated))
	}
	// The retiring case supplies a stale caller value to graphBase. Its real
	// generation state remains checked above; no invalid pointer is persisted.
	fixture.want = primaryBase{graphID: dedicated.GraphID, generationID: id, treeOID: row.TreeOID}
	return fixture
}

func TestGraphBaseCatalogPositivePointerContract(t *testing.T) {
	for _, tc := range []struct {
		name      string
		wantError string
		cancel    bool
	}{
		{name: "legacy_zero_healthy_owner"},
		{name: "legacy_zero_missing_owner", wantError: "names no committed tree"},
		{name: "legacy_zero_empty_owner_tree", wantError: "names no committed tree"},
		{name: "positive_ready_stored_tree_differs_from_owner_head"},
		{name: "positive_after_real_ready_to_superseded_transition"},
		{name: "positive_missing_generation_healthy_owner", wantError: "does not exist"},
		{name: "positive_wrong_owner_kind", wantError: "incompatible ownership or kind"},
		{name: "positive_wrong_graph", wantError: "incompatible ownership or kind"},
		{name: "positive_wrong_checkout", wantError: "incompatible ownership or kind"},
		{name: "positive_wrong_generation_kind", wantError: "incompatible ownership or kind"},
		{name: "positive_building", wantError: "not a servable committed tree"},
		{name: "positive_failed", wantError: "not a servable committed tree"},
		{name: "positive_retiring", wantError: "not a servable committed tree"},
		{name: "positive_empty_stored_tree", wantError: "not a servable committed tree"},
		{name: "legacy_zero_canceled_catalog_read", cancel: true},
		{name: "positive_canceled_catalog_read", cancel: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newGraphBaseCatalogFixture(t, tc.name)
			ctx := t.Context()
			if tc.cancel {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			got, err := graphBase(ctx, fixture.catalog, fixture.dedicated)
			if tc.wantError != "" || tc.cancel {
				if err == nil || got != (primaryBase{}) {
					t.Fatalf("invalid base was served: got=%+v err=%v", got, err)
				}
				if tc.cancel && !errors.Is(err, context.Canceled) {
					t.Fatalf("catalog cancellation was lost: %v", err)
				}
				if tc.wantError != "" && !strings.Contains(err.Error(), tc.wantError) {
					t.Fatalf("error=%v; want context %q", err, tc.wantError)
				}
				return
			}
			if err != nil || got != fixture.want {
				t.Fatalf("base=%+v err=%v; want %+v", got, err, fixture.want)
			}
		})
	}
}

func BenchmarkGraphBaseCatalogPositivePointer(b *testing.B) {
	for _, scenario := range []string{"legacy_zero_healthy_owner", "positive_ready_stored_tree_differs_from_owner_head"} {
		b.Run(scenario, func(b *testing.B) {
			fixture := newGraphBaseCatalogFixture(b, scenario)
			b.ReportAllocs()
			for b.Loop() {
				got, err := graphBase(b.Context(), fixture.catalog, fixture.dedicated)
				if err != nil || got != fixture.want {
					b.Fatalf("base=%+v err=%v; want %+v", got, err, fixture.want)
				}
			}
		})
	}
}
