package store_sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// catalogTables is every table the checkout-lifecycle control plane owns. The
// migration test drops all of them to recreate a pre-catalog store, and the
// fresh-store test asserts Open creates all of them.
var catalogTables = []string{
	"repository_families",
	"checkouts",
	"checkout_root_moves",
	"tracking_intents",
	"intent_transitions",
	"checkout_path_evidence",
	"dedicated_graphs",
	"view_generations",
	"view_layers",
	"checkout_routes",
	"ref_views",
	"ref_view_builds",
	"cleanup_journal",
}

func openCatalogStore(t testing.TB) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), "catalog.sqlite"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func hasTable(t *testing.T, db *sql.DB, name string) bool {
	t.Helper()
	var present bool
	if err := db.QueryRow(
		`SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type = 'table' AND name = ?)`, name,
	).Scan(&present); err != nil {
		t.Fatalf("probe table %s: %v", name, err)
	}
	return present
}

// seedFamilyAndCheckout installs the minimum control plane a lifecycle test
// needs: one family and one ready checkout inside it.
func seedFamilyAndCheckout(t testing.TB, catalog *Catalog, familyID, checkoutID, incarnation string) {
	t.Helper()
	ctx := context.Background()
	if err := catalog.UpsertRepositoryFamily(ctx, RepositoryFamily{
		FamilyID:          familyID,
		CommonDirIdentity: "identity/" + familyID,
		DisplayRemote:     "git@example.invalid:" + familyID + ".git",
		State:             "family_ready",
		CreatedAt:         100,
		LastSeen:          100,
	}); err != nil {
		t.Fatalf("UpsertRepositoryFamily: %v", err)
	}
	if err := catalog.UpsertCheckout(ctx, Checkout{
		CheckoutID:    checkoutID,
		Incarnation:   incarnation,
		FamilyID:      familyID,
		RootPath:      "/tmp/" + checkoutID,
		GitDir:        "/tmp/" + checkoutID + "/.git",
		AdminName:     checkoutID,
		State:         CheckoutStateReady,
		DesiredMode:   CheckoutModeAutomatic,
		EffectiveMode: CheckoutModeAutomatic,
		HeadRef:       "refs/heads/main",
		HeadCommit:    "c0ffee",
		HeadTree:      "7ee7",
		LastSeen:      101,
	}); err != nil {
		t.Fatalf("UpsertCheckout: %v", err)
	}
}

// TestCatalogRevokeTrackingIntentsIsAtomic proves both refusal and cancellation
// leave the whole intent set active, while an admitted revocation updates the
// set in one transaction.
func TestCatalogRevokeTrackingIntentsIsAtomic(t *testing.T) {
	catalog := openCatalogStore(t).Catalog()
	const checkoutID = "checkout-intents"
	seedFamilyAndCheckout(t, catalog, "family-intents", checkoutID, "incarnation-intents")
	ctx := context.Background()
	intents := []TrackingIntent{
		{IntentID: "intent-cli", CheckoutID: checkoutID, SourceKind: IntentSourceCLITrack, SourceLocator: "cli", Active: true, CreatedAt: 10},
		{IntentID: "intent-mcp", CheckoutID: checkoutID, SourceKind: IntentSourceMCPTrack, SourceLocator: "mcp", Active: true, CreatedAt: 11},
		{IntentID: "intent-project", CheckoutID: checkoutID, SourceKind: IntentSourceProjectMembership, SourceLocator: "project", Active: true, CreatedAt: 12},
	}
	for _, intent := range intents {
		if err := catalog.UpsertTrackingIntent(ctx, intent); err != nil {
			t.Fatalf("UpsertTrackingIntent(%s): %v", intent.IntentID, err)
		}
	}

	revoked, blocked, err := catalog.RevokeTrackingIntents(ctx, checkoutID, 20, []IntentSourceKind{
		IntentSourceCLITrack,
		IntentSourceMCPTrack,
	})
	if err != nil {
		t.Fatalf("blocked RevokeTrackingIntents: %v", err)
	}
	if len(revoked) != 0 || len(blocked) != 1 || blocked[0].SourceKind != IntentSourceProjectMembership {
		t.Fatalf("blocked revocation = revoked %#v, blocked %#v", revoked, blocked)
	}
	assertActive := func(want bool, wantRevokedAt int64) {
		t.Helper()
		got, err := catalog.ListTrackingIntents(context.Background(), checkoutID)
		if err != nil {
			t.Fatalf("ListTrackingIntents: %v", err)
		}
		if len(got) != len(intents) {
			t.Fatalf("got %d intents, want %d", len(got), len(intents))
		}
		for _, intent := range got {
			if intent.Active != want || intent.RevokedAt != wantRevokedAt {
				t.Fatalf("intent %s active/revoked_at = %v/%d, want %v/%d", intent.IntentID, intent.Active, intent.RevokedAt, want, wantRevokedAt)
			}
		}
	}
	assertActive(true, 0)

	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, _, err := catalog.RevokeTrackingIntents(cancelled, checkoutID, 21, []IntentSourceKind{
		IntentSourceCLITrack,
		IntentSourceMCPTrack,
		IntentSourceProjectMembership,
	}); err == nil {
		t.Fatal("cancelled RevokeTrackingIntents unexpectedly succeeded")
	}
	assertActive(true, 0)

	revoked, blocked, err = catalog.RevokeTrackingIntents(ctx, checkoutID, 22, []IntentSourceKind{
		IntentSourceCLITrack,
		IntentSourceMCPTrack,
		IntentSourceProjectMembership,
	})
	if err != nil {
		t.Fatalf("admitted RevokeTrackingIntents: %v", err)
	}
	if len(revoked) != len(intents) || len(blocked) != 0 {
		t.Fatalf("admitted revocation = revoked %#v, blocked %#v", revoked, blocked)
	}
	assertActive(false, 22)
}

// seedBuildingGeneration creates one generation in the building state.
func seedBuildingGeneration(t *testing.T, catalog *Catalog, graphID string) int64 {
	t.Helper()
	id, err := catalog.CreateViewGeneration(context.Background(), ViewGeneration{
		OwnerKind:      "dedicated_graph",
		GraphID:        graphID,
		GenerationKind: "commit",
		TreeOID:        "tree-" + graphID,
		State:          ViewGenerationBuilding,
		CreatedAt:      200,
	})
	if err != nil {
		t.Fatalf("CreateViewGeneration: %v", err)
	}
	return id
}

// TestCatalogSchemaAppliesOnFreshStore proves Open creates the whole control
// plane on a brand-new database, and that a round trip through the accessors
// preserves every column — including the nullable ones whose empty Go value
// must come back as an empty value rather than a scan error.
func TestCatalogSchemaAppliesOnFreshStore(t *testing.T) {
	store := openCatalogStore(t)
	for _, name := range catalogTables {
		if !hasTable(t, store.writerDB, name) {
			t.Fatalf("fresh store is missing catalog table %s", name)
		}
	}

	ctx := context.Background()
	catalog := store.Catalog()
	seedFamilyAndCheckout(t, catalog, "fam", "wt", "inc-1")

	family, ok, err := catalog.GetRepositoryFamily(ctx, "fam")
	if err != nil || !ok {
		t.Fatalf("GetRepositoryFamily = %v, %v, %v", family, ok, err)
	}
	if family.CommonDirIdentity != "identity/fam" || family.PrimaryEpoch != 0 {
		t.Fatalf("family round trip = %+v", family)
	}

	checkout, ok, err := catalog.GetCheckout(ctx, "wt")
	if err != nil || !ok {
		t.Fatalf("GetCheckout = %v, %v, %v", checkout, ok, err)
	}
	if checkout.State != CheckoutStateReady || checkout.EffectiveMode != CheckoutModeAutomatic {
		t.Fatalf("checkout round trip = %+v", checkout)
	}
	if checkout.ActiveIntentTransitionID != "" {
		t.Fatalf("unset transition pointer = %q, want empty", checkout.ActiveIntentTransitionID)
	}

	checkouts, err := catalog.ListCheckouts(ctx, "fam")
	if err != nil {
		t.Fatalf("ListCheckouts: %v", err)
	}
	if len(checkouts) != 1 || checkouts[0].CheckoutID != "wt" {
		t.Fatalf("ListCheckouts = %+v, want the one seeded checkout", checkouts)
	}

	if err := catalog.UpsertCheckoutPathEvidence(ctx, CheckoutPathEvidence{
		CheckoutID:                  "wt",
		RootPathIdentity:            "dev:1,ino:2",
		RootVolumeKind:              "local",
		RootVolumeToken:             "vol-a",
		NearestExistingAncestorPath: "/tmp",
		AncestorVolumeKind:          "local",
		AncestorVolumeToken:         "vol-a",
		CommonDirVolumeKind:         "local",
		CommonDirVolumeToken:        "vol-a",
		SampledAt:                   300,
		SampleGeneration:            4,
	}); err != nil {
		t.Fatalf("UpsertCheckoutPathEvidence: %v", err)
	}
	evidence, ok, err := catalog.GetCheckoutPathEvidence(ctx, "wt")
	if err != nil || !ok {
		t.Fatalf("GetCheckoutPathEvidence = %v, %v, %v", evidence, ok, err)
	}
	if evidence.SampleGeneration != 4 || evidence.RootPathIdentity != "dev:1,ino:2" {
		t.Fatalf("path evidence round trip = %+v", evidence)
	}

	if err := catalog.UpsertViewLayer(ctx, ViewLayer{
		LayerID:      "layer-1",
		Kind:         "commit",
		GraphID:      "graph-1",
		CheckoutID:   "wt",
		TargetRef:    "refs/heads/main",
		TargetCommit: "c0ffee",
		TargetTree:   "7ee7",
	}); err != nil {
		t.Fatalf("UpsertViewLayer: %v", err)
	}
	layer, ok, err := catalog.GetViewLayer(ctx, "layer-1")
	if err != nil || !ok {
		t.Fatalf("GetViewLayer = %v, %v, %v", layer, ok, err)
	}
	if layer.TargetRef != "refs/heads/main" || layer.CheckoutID != "wt" {
		t.Fatalf("layer round trip = %+v", layer)
	}

	generationID := seedBuildingGeneration(t, catalog, "graph-1")
	generation, ok, err := catalog.GetViewGeneration(ctx, generationID)
	if err != nil || !ok {
		t.Fatalf("GetViewGeneration = %v, %v, %v", generation, ok, err)
	}
	if generation.State != ViewGenerationBuilding || generation.BaseGenerationID != 0 {
		t.Fatalf("generation round trip = %+v", generation)
	}
}

// TestCatalogSchemaMigratesExistingStore is the backward-compatibility proof:
// an on-disk store written before the catalog existed gains every table on its
// next Open, keeps its graph rows, and does not signal a rebuild. The catalog
// is additive, so this must be an in-place upgrade rather than a wipe.
func TestCatalogSchemaMigratesExistingStore(t *testing.T) {
	if currentSchemaVersion < 13 {
		t.Fatalf("currentSchemaVersion = %d, want >= 13 for the catalog migration", currentSchemaVersion)
	}
	var step *schemaMigration
	for i := range schemaMigrations {
		if schemaMigrations[i].version == 13 {
			step = &schemaMigrations[i]
			break
		}
	}
	if step == nil || step.rebuild || step.inPlace == nil {
		t.Fatalf("v13 migration = %+v, want a registered in-place step", step)
	}

	path := filepath.Join(t.TempDir(), "pre-catalog.sqlite")
	seed, err := Open(path)
	if err != nil {
		t.Fatalf("create current store: %v", err)
	}
	seed.AddBatch([]*graph.Node{
		{ID: "repo/a.go::Legacy", Kind: graph.KindFunction, Name: "Legacy", FilePath: "repo/a.go", RepoPrefix: "repo"},
	}, nil)
	if err := seed.Close(); err != nil {
		t.Fatalf("close seed store: %v", err)
	}

	// Recreate the exact pre-catalog shape: the graph exists, the catalog does
	// not, and the file is stamped at the version before the catalog shipped.
	withRawDB(t, path, func(db *sql.DB) {
		for _, name := range catalogTables {
			if _, err := db.Exec(`DROP TABLE IF EXISTS ` + name); err != nil {
				t.Fatalf("drop %s: %v", name, err)
			}
		}
		if _, err := db.Exec(`PRAGMA user_version = 12`); err != nil {
			t.Fatalf("stamp v12: %v", err)
		}
	})

	migrated, err := Open(path)
	if err != nil {
		t.Fatalf("reopen pre-catalog store: %v", err)
	}
	t.Cleanup(func() { _ = migrated.Close() })

	if migrated.NeedsRebuild() {
		t.Fatal("an additive catalog upgrade must not signal a wipe/reindex")
	}
	for _, name := range catalogTables {
		if !hasTable(t, migrated.writerDB, name) {
			t.Fatalf("migrated store is missing catalog table %s", name)
		}
	}
	if version, err := readUserVersion(migrated.writerDB); err != nil || version != currentSchemaVersion {
		t.Fatalf("post-migration user_version = %d (err %v), want %d", version, err, currentSchemaVersion)
	}
	if migrated.GetNode("repo/a.go::Legacy") == nil {
		t.Fatal("existing graph rows must survive the in-place catalog upgrade")
	}

	// The upgraded store is fully usable, not merely present.
	seedFamilyAndCheckout(t, migrated.Catalog(), "fam", "wt", "inc-1")
	if _, ok, err := migrated.Catalog().GetCheckout(context.Background(), "wt"); err != nil || !ok {
		t.Fatalf("write to migrated catalog = %v, %v", ok, err)
	}
}

// TestCatalogCheckoutDeleteCascades proves the ON DELETE CASCADE wiring: a
// checkout's intents, in-flight transition and path evidence go with it, while
// the cleanup journal — which deliberately has no foreign keys — survives,
// because its whole purpose is to outlive the rows it names.
func TestCatalogCheckoutDeleteCascades(t *testing.T) {
	store := openCatalogStore(t)
	ctx := context.Background()
	catalog := store.Catalog()
	seedFamilyAndCheckout(t, catalog, "fam", "wt", "inc-1")

	if err := catalog.UpsertTrackingIntent(ctx, TrackingIntent{
		IntentID:      "intent-1",
		CheckoutID:    "wt",
		SourceKind:    IntentSourceCLITrack,
		SourceLocator: "gortex track /tmp/wt",
		Active:        true,
		CreatedAt:     400,
	}); err != nil {
		t.Fatalf("UpsertTrackingIntent: %v", err)
	}
	if err := catalog.BeginIntentTransition(ctx, IntentTransition{
		TransitionID:       "trans-1",
		CheckoutID:         "wt",
		Cause:              "user_requested_dedicated",
		PriorDesiredMode:   CheckoutModeAutomatic,
		PriorEffectiveMode: CheckoutModeAutomatic,
		RequestedMode:      CheckoutModeDedicated,
		PriorCheckoutState: CheckoutStateReady,
		State:              IntentTransitionPending,
		CreatedAt:          401,
	}); err != nil {
		t.Fatalf("BeginIntentTransition: %v", err)
	}
	if err := catalog.UpsertCheckoutPathEvidence(ctx, CheckoutPathEvidence{
		CheckoutID: "wt", RootPathIdentity: "dev:1,ino:2", SampledAt: 402,
	}); err != nil {
		t.Fatalf("UpsertCheckoutPathEvidence: %v", err)
	}
	if err := catalog.UpsertCleanupEntry(ctx, CleanupEntry{
		CleanupID:       "cleanup-1",
		OpaqueTargetIDs: "wt",
		Reason:          "checkout_forgotten",
		Phase:           CleanupPhaseGrace,
		GraceDeadline:   500,
		PrimaryEpoch:    0,
	}); err != nil {
		t.Fatalf("UpsertCleanupEntry: %v", err)
	}

	if err := catalog.DeleteCheckout(ctx, "wt"); err != nil {
		t.Fatalf("DeleteCheckout: %v", err)
	}

	intents, err := catalog.ListTrackingIntents(ctx, "wt")
	if err != nil {
		t.Fatalf("ListTrackingIntents: %v", err)
	}
	if len(intents) != 0 {
		t.Fatalf("tracking intents survived the checkout delete: %+v", intents)
	}
	if transition, ok, err := catalog.GetIntentTransition(ctx, "wt"); err != nil || ok {
		t.Fatalf("intent transition survived the checkout delete: %+v, %v, %v", transition, ok, err)
	}
	if evidence, ok, err := catalog.GetCheckoutPathEvidence(ctx, "wt"); err != nil || ok {
		t.Fatalf("path evidence survived the checkout delete: %+v, %v, %v", evidence, ok, err)
	}

	entry, ok, err := catalog.GetCleanupEntry(ctx, "cleanup-1")
	if err != nil || !ok {
		t.Fatalf("cleanup journal entry must outlive its target: %v, %v, %v", entry, ok, err)
	}
	if entry.Phase != CleanupPhaseGrace || entry.OpaqueTargetIDs != "wt" {
		t.Fatalf("cleanup entry round trip = %+v", entry)
	}

	if err := catalog.DeleteCheckout(ctx, "wt"); !errors.Is(err, ErrCatalogNotFound) {
		t.Fatalf("second DeleteCheckout = %v, want ErrCatalogNotFound", err)
	}
}

// TestCatalogRoutedGenerationCannotBeDeleted proves the RESTRICT-style
// protection on view generations: a generation a route, a ref view, another
// generation's base pointer, or a dedicated graph's active pointer still names
// cannot be deleted, and becomes deletable only once the last reference drops.
func TestCatalogRoutedGenerationCannotBeDeleted(t *testing.T) {
	store := openCatalogStore(t)
	ctx := context.Background()
	catalog := store.Catalog()
	seedFamilyAndCheckout(t, catalog, "fam", "wt", "inc-1")

	base := seedBuildingGeneration(t, catalog, "graph-1")
	if err := catalog.PublishViewGeneration(ctx, base, 600); err != nil {
		t.Fatalf("PublishViewGeneration: %v", err)
	}

	if err := catalog.UpsertCheckoutRoute(ctx, CheckoutRoute{
		CheckoutID:         "wt",
		GraphID:            "graph-1",
		CommitGenerationID: base,
		RouteEpoch:         0,
		State:              RouteActive,
	}); err != nil {
		t.Fatalf("UpsertCheckoutRoute: %v", err)
	}

	err := catalog.DeleteViewGeneration(ctx, base)
	if !errors.Is(err, ErrCatalogGenerationReferenced) {
		t.Fatalf("delete of a routed generation = %v, want ErrCatalogGenerationReferenced", err)
	}

	// A route does not cascade, so the checkout under it is pinned too: routes
	// must be retired deliberately rather than vanishing with their checkout.
	if err := catalog.DeleteCheckout(ctx, "wt"); err == nil {
		t.Fatal("deleting a routed checkout succeeded; the route foreign key does not restrict")
	}

	// The database refuses it too, not just the Go guard: with the store's
	// per-connection foreign_keys(ON), the non-deferred NO ACTION constraint on
	// checkout_routes.commit_generation_id behaves as RESTRICT.
	if _, err := store.writerDB.ExecContext(ctx,
		`DELETE FROM view_generations WHERE generation_id = ?`, base); err == nil {
		t.Fatal("raw delete of a routed generation succeeded; the foreign key is not enforced")
	}

	// Move the route off the generation, then pin it three other ways in turn.
	if err := catalog.FlipCheckoutRoute(ctx, FlipCheckoutRouteRequest{
		CheckoutID: "wt", ExpectedRouteEpoch: 0, GraphID: "graph-1", State: RoutePending,
	}); err != nil {
		t.Fatalf("FlipCheckoutRoute: %v", err)
	}

	overlay, err := catalog.CreateViewGeneration(ctx, ViewGeneration{
		OwnerKind:        "checkout",
		GraphID:          "graph-1",
		CheckoutID:       "wt",
		GenerationKind:   "dirty",
		BaseGenerationID: base,
		State:            ViewGenerationBuilding,
		CreatedAt:        601,
	})
	if err != nil {
		t.Fatalf("CreateViewGeneration overlay: %v", err)
	}
	if err := catalog.DeleteViewGeneration(ctx, base); !errors.Is(err, ErrCatalogGenerationReferenced) {
		t.Fatalf("delete under a base pointer = %v, want ErrCatalogGenerationReferenced", err)
	}
	if err := catalog.DeleteViewGeneration(ctx, overlay); err != nil {
		t.Fatalf("DeleteViewGeneration overlay: %v", err)
	}

	if err := catalog.UpsertRefView(ctx, RefView{
		RefViewID:          "rv-1",
		GraphID:            "graph-1",
		SelectorKind:       "branch",
		SelectorValue:      "main",
		DesiredRef:         "refs/heads/main",
		DesiredTree:        "7ee7",
		ActiveGenerationID: base,
		EnrichmentProfile:  "default",
		State:              RefViewReady,
		ExactView:          true,
	}); err != nil {
		t.Fatalf("UpsertRefView: %v", err)
	}
	if err := catalog.DeleteViewGeneration(ctx, base); !errors.Is(err, ErrCatalogGenerationReferenced) {
		t.Fatalf("delete under a ref view = %v, want ErrCatalogGenerationReferenced", err)
	}
	if _, err := store.writerDB.ExecContext(ctx, `DELETE FROM ref_views WHERE ref_view_id = ?`, "rv-1"); err != nil {
		t.Fatalf("drop ref view: %v", err)
	}

	if err := catalog.UpsertDedicatedGraph(ctx, DedicatedGraph{
		GraphID:            "graph-1",
		OwnerCheckoutID:    "wt",
		RepoPrefix:         "wt-prefix",
		FamilyID:           "fam",
		ActiveGenerationID: base,
		State:              "graph_ready",
	}); err != nil {
		t.Fatalf("UpsertDedicatedGraph: %v", err)
	}
	if err := catalog.DeleteViewGeneration(ctx, base); !errors.Is(err, ErrCatalogGenerationReferenced) {
		t.Fatalf("delete under a dedicated graph pointer = %v, want ErrCatalogGenerationReferenced", err)
	}
	if err := catalog.UpsertDedicatedGraph(ctx, DedicatedGraph{
		GraphID:         "graph-1",
		OwnerCheckoutID: "wt",
		RepoPrefix:      "wt-prefix",
		FamilyID:        "fam",
		State:           "graph_ready",
	}); err != nil {
		t.Fatalf("clear dedicated graph pointer: %v", err)
	}

	if err := catalog.DeleteViewGeneration(ctx, base); err != nil {
		t.Fatalf("DeleteViewGeneration once unreferenced: %v", err)
	}
	if _, ok, err := catalog.GetViewGeneration(ctx, base); err != nil || ok {
		t.Fatalf("generation still present after delete: %v, %v", ok, err)
	}
}

// TestCatalogPrimaryBaseIsUniquePerFamily proves the partial unique index: a
// family may hold at most one primary base, but each family holds its own.
func TestCatalogPrimaryBaseIsUniquePerFamily(t *testing.T) {
	store := openCatalogStore(t)
	ctx := context.Background()
	catalog := store.Catalog()
	seedFamilyAndCheckout(t, catalog, "fam-a", "wt-a", "inc-1")
	seedFamilyAndCheckout(t, catalog, "fam-b", "wt-b", "inc-1")

	primaryA := DedicatedGraph{
		GraphID: "graph-a1", OwnerCheckoutID: "wt-a", RepoPrefix: "prefix-a1",
		FamilyID: "fam-a", IsPrimaryBase: true, State: "graph_ready",
	}
	if err := catalog.UpsertDedicatedGraph(ctx, primaryA); err != nil {
		t.Fatalf("first primary in fam-a: %v", err)
	}

	second := DedicatedGraph{
		GraphID: "graph-a2", RepoPrefix: "prefix-a2",
		FamilyID: "fam-a", IsPrimaryBase: true, State: "graph_ready",
	}
	if err := catalog.UpsertDedicatedGraph(ctx, second); err == nil {
		t.Fatal("a second primary base in one family must be rejected")
	} else if !strings.Contains(strings.ToLower(err.Error()), "unique") {
		t.Fatalf("second primary rejection = %v, want a uniqueness failure", err)
	}

	// A non-primary sibling in the same family is fine — the index is partial.
	second.IsPrimaryBase = false
	if err := catalog.UpsertDedicatedGraph(ctx, second); err != nil {
		t.Fatalf("non-primary sibling in fam-a: %v", err)
	}

	// And each family gets its own primary.
	if err := catalog.UpsertDedicatedGraph(ctx, DedicatedGraph{
		GraphID: "graph-b1", OwnerCheckoutID: "wt-b", RepoPrefix: "prefix-b1",
		FamilyID: "fam-b", IsPrimaryBase: true, State: "graph_ready",
	}); err != nil {
		t.Fatalf("first primary in fam-b: %v", err)
	}
	for _, id := range []string{"graph-a1", "graph-b1"} {
		dedicated, ok, err := catalog.GetDedicatedGraph(ctx, id)
		if err != nil || !ok {
			t.Fatalf("GetDedicatedGraph(%s) = %v, %v, %v", id, dedicated, ok, err)
		}
		if !dedicated.IsPrimaryBase {
			t.Fatalf("%s should be its family's primary base: %+v", id, dedicated)
		}
	}
}

// TestCatalogDedicatedGraphDeleteGuardsCheckoutIncarnation proves graph IDs can
// be reused without letting a stale teardown delete the replacement binding.
// The DELETE must match the graph owner and the owner's current incarnation in
// one SQL statement; a stale token changes no row.
func TestCatalogDedicatedGraphDeleteGuardsCheckoutIncarnation(t *testing.T) {
	store := openCatalogStore(t)
	ctx := context.Background()
	catalog := store.Catalog()
	seedFamilyAndCheckout(t, catalog, "fam", "wt", "inc-1")
	graph := DedicatedGraph{
		GraphID: "graph-1", OwnerCheckoutID: "wt", RepoPrefix: "prefix-1",
		FamilyID: "fam", State: "graph_ready",
	}
	if err := catalog.UpsertDedicatedGraph(ctx, graph); err != nil {
		t.Fatalf("UpsertDedicatedGraph: %v", err)
	}

	deleted, err := catalog.DeleteDedicatedGraphForIncarnation(ctx, "graph-1", "wt", "stale-inc")
	if err != nil {
		t.Fatalf("stale delete: %v", err)
	}
	if deleted {
		t.Fatal("stale incarnation deleted the graph")
	}
	if got, ok, err := catalog.GetDedicatedGraph(ctx, "graph-1"); err != nil || !ok || got != graph {
		t.Fatalf("stale delete changed graph: %+v, %v, %v", got, ok, err)
	}

	deleted, err = catalog.DeleteDedicatedGraphForIncarnation(ctx, "graph-1", "wt", "inc-1")
	if err != nil || !deleted {
		t.Fatalf("current delete = %v, %v", deleted, err)
	}
	if _, ok, err := catalog.GetDedicatedGraph(ctx, "graph-1"); err != nil || ok {
		t.Fatalf("deleted graph remains: %v, %v", ok, err)
	}

	// Recreate the same checkout and deterministic graph IDs under a fresh
	// incarnation. Replaying the old cleanup token must preserve them.
	seedFamilyAndCheckout(t, catalog, "fam", "wt", "inc-2")
	if err := catalog.UpsertDedicatedGraph(ctx, graph); err != nil {
		t.Fatalf("UpsertDedicatedGraph replacement: %v", err)
	}
	deleted, err = catalog.DeleteDedicatedGraphForIncarnation(ctx, "graph-1", "wt", "inc-1")
	if err != nil {
		t.Fatalf("replacement stale delete: %v", err)
	}
	if deleted {
		t.Fatal("old incarnation deleted the replacement graph")
	}
	if got, ok, err := catalog.GetDedicatedGraph(ctx, "graph-1"); err != nil || !ok || got != graph {
		t.Fatalf("replacement graph changed: %+v, %v, %v", got, ok, err)
	}
}

func TestCatalogDedicatedGraphRestoreIsInsertOnlyAndIncarnationGuarded(t *testing.T) {
	store := openCatalogStore(t)
	ctx := context.Background()
	catalog := store.Catalog()
	seedFamilyAndCheckout(t, catalog, "fam", "wt", "inc-1")
	captured := DedicatedGraph{
		GraphID: "graph-1", OwnerCheckoutID: "wt", RepoPrefix: "captured-prefix",
		FamilyID: "fam", State: "graph_ready",
	}
	if err := catalog.UpsertDedicatedGraph(ctx, captured); err != nil {
		t.Fatalf("seed captured graph: %v", err)
	}
	if deleted, err := catalog.DeleteDedicatedGraphForIncarnation(ctx, "graph-1", "wt", "inc-1"); err != nil || !deleted {
		t.Fatalf("delete captured graph = %v, %v", deleted, err)
	}

	present, err := catalog.RestoreDedicatedGraphForIncarnation(ctx, captured, "wt", "inc-1")
	if err != nil || !present {
		t.Fatalf("restore current graph = %v, %v", present, err)
	}
	if got, ok, err := catalog.GetDedicatedGraph(ctx, captured.GraphID); err != nil || !ok || got != captured {
		t.Fatalf("restored graph = %+v, present:%v err:%v", got, ok, err)
	}
	if deleted, err := catalog.DeleteDedicatedGraphForIncarnation(ctx, "graph-1", "wt", "inc-1"); err != nil || !deleted {
		t.Fatalf("delete restored graph = %v, %v", deleted, err)
	}

	seedFamilyAndCheckout(t, catalog, "fam", "wt", "inc-2")
	present, err = catalog.RestoreDedicatedGraphForIncarnation(ctx, captured, "wt", "inc-1")
	if err != nil {
		t.Fatalf("stale restore without graph: %v", err)
	}
	if present {
		t.Fatal("stale incarnation restored an absent graph")
	}
	if _, ok, err := catalog.GetDedicatedGraph(ctx, captured.GraphID); err != nil || ok {
		t.Fatalf("stale restore created graph: present:%v err:%v", ok, err)
	}

	// Even an identical row is stale when its owner checkout ID now names a
	// different incarnation. Identity is checked before row equality.
	if err := catalog.UpsertDedicatedGraph(ctx, captured); err != nil {
		t.Fatalf("seed identical replacement graph: %v", err)
	}
	present, err = catalog.RestoreDedicatedGraphForIncarnation(ctx, captured, "wt", "inc-1")
	if err != nil {
		t.Fatalf("stale restore over identical replacement: %v", err)
	}
	if present {
		t.Fatal("stale restore accepted an identical row owned by a new incarnation")
	}

	replacement := captured
	replacement.RepoPrefix = "replacement-prefix"
	if err := catalog.UpsertDedicatedGraph(ctx, replacement); err != nil {
		t.Fatalf("seed replacement graph: %v", err)
	}
	present, err = catalog.RestoreDedicatedGraphForIncarnation(ctx, captured, "wt", "inc-1")
	if err != nil {
		t.Fatalf("stale restore over replacement: %v", err)
	}
	if present {
		t.Fatal("stale restore reported captured graph present")
	}
	if got, ok, err := catalog.GetDedicatedGraph(ctx, replacement.GraphID); err != nil || !ok || got != replacement {
		t.Fatalf("stale restore overwrote replacement: %+v, present:%v err:%v", got, ok, err)
	}
}

// TestCatalogIncarnationGuardRejectsStaleWrite proves a checkout state write
// aimed at a previous incarnation of a recreated working copy changes nothing.
func TestCatalogIncarnationGuardRejectsStaleWrite(t *testing.T) {
	store := openCatalogStore(t)
	ctx := context.Background()
	catalog := store.Catalog()
	seedFamilyAndCheckout(t, catalog, "fam", "wt", "inc-2")

	stale := UpdateCheckoutStateRequest{
		CheckoutID: "wt", Incarnation: "inc-1",
		State:       CheckoutStateUnavailable,
		DesiredMode: CheckoutModeDedicated, EffectiveMode: CheckoutModeDedicated,
		LastSeen: 700, LastError: "stale writer",
	}
	if err := catalog.UpdateCheckoutState(ctx, stale); !errors.Is(err, ErrCatalogStaleGuard) {
		t.Fatalf("stale incarnation write = %v, want ErrCatalogStaleGuard", err)
	}
	checkout, ok, err := catalog.GetCheckout(ctx, "wt")
	if err != nil || !ok {
		t.Fatalf("GetCheckout = %v, %v, %v", checkout, ok, err)
	}
	if checkout.State != CheckoutStateReady || checkout.LastError != "" || checkout.LastSeen != 101 {
		t.Fatalf("a rejected guard must change nothing: %+v", checkout)
	}

	current := stale
	current.Incarnation = "inc-2"
	current.State = CheckoutStateReconciling
	current.LastError = ""
	if err := catalog.UpdateCheckoutState(ctx, current); err != nil {
		t.Fatalf("current incarnation write: %v", err)
	}
	checkout, _, err = catalog.GetCheckout(ctx, "wt")
	if err != nil {
		t.Fatalf("GetCheckout: %v", err)
	}
	if checkout.State != CheckoutStateReconciling || checkout.EffectiveMode != CheckoutModeDedicated || checkout.LastSeen != 700 {
		t.Fatalf("accepted guard did not apply: %+v", checkout)
	}

	// A value outside the vocabulary never reaches SQL.
	bad := current
	bad.State = CheckoutState("checkout_teleporting")
	if err := catalog.UpdateCheckoutState(ctx, bad); !errors.Is(err, ErrCatalogInvalidValue) {
		t.Fatalf("out-of-vocabulary state = %v, want ErrCatalogInvalidValue", err)
	}
}

// TestCatalogRouteEpochCASRejectsStaleFlip proves the route compare-and-set:
// the first flip wins and bumps the epoch, a second flip replaying the old
// epoch changes nothing.
func TestCatalogRouteEpochCASRejectsStaleFlip(t *testing.T) {
	store := openCatalogStore(t)
	ctx := context.Background()
	catalog := store.Catalog()
	seedFamilyAndCheckout(t, catalog, "fam", "wt", "inc-1")

	first := seedBuildingGeneration(t, catalog, "graph-1")
	second := seedBuildingGeneration(t, catalog, "graph-1")
	if err := catalog.UpsertCheckoutRoute(ctx, CheckoutRoute{
		CheckoutID: "wt", GraphID: "graph-1", CommitGenerationID: first,
		RouteEpoch: 0, State: RouteActive,
	}); err != nil {
		t.Fatalf("UpsertCheckoutRoute: %v", err)
	}

	flip := FlipCheckoutRouteRequest{
		CheckoutID: "wt", ExpectedRouteEpoch: 0, GraphID: "graph-1",
		CommitGenerationID: second, State: RouteActive,
	}
	if err := catalog.FlipCheckoutRoute(ctx, flip); err != nil {
		t.Fatalf("first flip: %v", err)
	}
	route, ok, err := catalog.GetCheckoutRoute(ctx, "wt")
	if err != nil || !ok {
		t.Fatalf("GetCheckoutRoute = %v, %v, %v", route, ok, err)
	}
	if route.RouteEpoch != 1 || route.CommitGenerationID != second {
		t.Fatalf("first flip result = %+v", route)
	}

	// A concurrent reconciler replaying the pre-flip epoch must lose.
	replay := flip
	replay.CommitGenerationID = first
	if err := catalog.FlipCheckoutRoute(ctx, replay); !errors.Is(err, ErrCatalogStaleGuard) {
		t.Fatalf("stale route flip = %v, want ErrCatalogStaleGuard", err)
	}
	route, _, err = catalog.GetCheckoutRoute(ctx, "wt")
	if err != nil {
		t.Fatalf("GetCheckoutRoute: %v", err)
	}
	if route.RouteEpoch != 1 || route.CommitGenerationID != second {
		t.Fatalf("a rejected flip must change nothing: %+v", route)
	}

	// Clearing both pointers is expressible and still bumps the epoch.
	clear := FlipCheckoutRouteRequest{
		CheckoutID: "wt", ExpectedRouteEpoch: 1, GraphID: "graph-1", State: RoutePending,
	}
	if err := catalog.FlipCheckoutRoute(ctx, clear); err != nil {
		t.Fatalf("clearing flip: %v", err)
	}
	route, _, err = catalog.GetCheckoutRoute(ctx, "wt")
	if err != nil {
		t.Fatalf("GetCheckoutRoute: %v", err)
	}
	if route.RouteEpoch != 2 || route.CommitGenerationID != 0 || route.State != RoutePending {
		t.Fatalf("cleared route = %+v", route)
	}
}

// TestCatalogPrimaryEpochCASRejectsStaleFlip proves the primary-base
// compare-and-set on the family row, and that promoting a second graph moves
// the flag rather than colliding with the partial unique index.
func TestCatalogPrimaryEpochCASRejectsStaleFlip(t *testing.T) {
	store := openCatalogStore(t)
	ctx := context.Background()
	catalog := store.Catalog()
	seedFamilyAndCheckout(t, catalog, "fam", "wt", "inc-1")

	for _, spec := range []struct{ graphID, prefix string }{
		{"graph-1", "prefix-1"},
		{"graph-2", "prefix-2"},
	} {
		if err := catalog.UpsertDedicatedGraph(ctx, DedicatedGraph{
			GraphID: spec.graphID, RepoPrefix: spec.prefix,
			FamilyID: "fam", State: "graph_ready",
		}); err != nil {
			t.Fatalf("UpsertDedicatedGraph(%s): %v", spec.graphID, err)
		}
	}

	promote := SetPrimaryDedicatedGraphRequest{
		FamilyID: "fam", GraphID: "graph-1", ExpectedPrimaryEpoch: 0, LastSeen: 800,
	}
	if err := catalog.SetPrimaryDedicatedGraph(ctx, promote); err != nil {
		t.Fatalf("first promotion: %v", err)
	}
	family, _, err := catalog.GetRepositoryFamily(ctx, "fam")
	if err != nil {
		t.Fatalf("GetRepositoryFamily: %v", err)
	}
	if family.PrimaryEpoch != 1 {
		t.Fatalf("primary epoch = %d, want 1", family.PrimaryEpoch)
	}

	// Replaying the pre-promotion epoch must change nothing at all.
	replay := promote
	replay.GraphID = "graph-2"
	if err := catalog.SetPrimaryDedicatedGraph(ctx, replay); !errors.Is(err, ErrCatalogStaleGuard) {
		t.Fatalf("stale primary flip = %v, want ErrCatalogStaleGuard", err)
	}
	first, _, err := catalog.GetDedicatedGraph(ctx, "graph-1")
	if err != nil {
		t.Fatalf("GetDedicatedGraph: %v", err)
	}
	if !first.IsPrimaryBase {
		t.Fatalf("a rejected primary flip must leave the incumbent: %+v", first)
	}
	second, _, err := catalog.GetDedicatedGraph(ctx, "graph-2")
	if err != nil {
		t.Fatalf("GetDedicatedGraph: %v", err)
	}
	if second.IsPrimaryBase {
		t.Fatalf("a rejected primary flip must not promote: %+v", second)
	}

	// With the current epoch the flag moves: the incumbent is cleared inside
	// the same transaction, so the partial unique index is never violated.
	move := SetPrimaryDedicatedGraphRequest{
		FamilyID: "fam", GraphID: "graph-2", ExpectedPrimaryEpoch: 1, LastSeen: 801,
	}
	if err := catalog.SetPrimaryDedicatedGraph(ctx, move); err != nil {
		t.Fatalf("moving the primary base: %v", err)
	}
	first, _, err = catalog.GetDedicatedGraph(ctx, "graph-1")
	if err != nil {
		t.Fatalf("GetDedicatedGraph: %v", err)
	}
	second, _, err = catalog.GetDedicatedGraph(ctx, "graph-2")
	if err != nil {
		t.Fatalf("GetDedicatedGraph: %v", err)
	}
	if first.IsPrimaryBase || !second.IsPrimaryBase {
		t.Fatalf("primary base did not move: %+v / %+v", first, second)
	}

	// Promoting a graph that is not in the family leaves nothing behind.
	if err := catalog.SetPrimaryDedicatedGraph(ctx, SetPrimaryDedicatedGraphRequest{
		FamilyID: "fam", GraphID: "graph-missing", ExpectedPrimaryEpoch: 2, LastSeen: 802,
	}); !errors.Is(err, ErrCatalogNotFound) {
		t.Fatalf("promoting an unknown graph = %v, want ErrCatalogNotFound", err)
	}
	family, _, err = catalog.GetRepositoryFamily(ctx, "fam")
	if err != nil {
		t.Fatalf("GetRepositoryFamily: %v", err)
	}
	if family.PrimaryEpoch != 2 {
		t.Fatalf("a rolled-back promotion must not bump the epoch: %d", family.PrimaryEpoch)
	}
}

// TestCatalogGenerationIsImmutableOnceReady proves publish is a one-way
// building -> ready transition: a second publish, and any publish of a
// generation that never was building, are refused.
func TestCatalogGenerationIsImmutableOnceReady(t *testing.T) {
	store := openCatalogStore(t)
	ctx := context.Background()
	catalog := store.Catalog()

	generationID := seedBuildingGeneration(t, catalog, "graph-1")
	if err := catalog.PublishViewGeneration(ctx, generationID, 900); err != nil {
		t.Fatalf("PublishViewGeneration: %v", err)
	}
	published, ok, err := catalog.GetViewGeneration(ctx, generationID)
	if err != nil || !ok {
		t.Fatalf("GetViewGeneration = %v, %v, %v", published, ok, err)
	}
	if published.State != ViewGenerationReady || published.PublishedAt != 900 {
		t.Fatalf("published generation = %+v", published)
	}

	if err := catalog.PublishViewGeneration(ctx, generationID, 901); !errors.Is(err, ErrCatalogStaleGuard) {
		t.Fatalf("republish = %v, want ErrCatalogStaleGuard", err)
	}
	republished, _, err := catalog.GetViewGeneration(ctx, generationID)
	if err != nil {
		t.Fatalf("GetViewGeneration: %v", err)
	}
	if republished.PublishedAt != 900 {
		t.Fatalf("a ready generation must be immutable: %+v", republished)
	}

	failed, err := catalog.CreateViewGeneration(ctx, ViewGeneration{
		OwnerKind: "dedicated_graph", GraphID: "graph-1", GenerationKind: "commit",
		State: ViewGenerationFailed, CreatedAt: 902, Error: "extractor crashed",
	})
	if err != nil {
		t.Fatalf("CreateViewGeneration failed-state: %v", err)
	}
	if err := catalog.PublishViewGeneration(ctx, failed, 903); !errors.Is(err, ErrCatalogStaleGuard) {
		t.Fatalf("publish of a failed generation = %v, want ErrCatalogStaleGuard", err)
	}
}

// TestCatalogRefViewSelectorAndBuildCoalescing proves the two ref-view
// constraints: one row per (graph, selector, profile), and one in-flight build
// per (ref view, tree, base, fingerprint).
func TestCatalogRefViewSelectorAndBuildCoalescing(t *testing.T) {
	store := openCatalogStore(t)
	ctx := context.Background()
	catalog := store.Catalog()

	base := seedBuildingGeneration(t, catalog, "graph-1")
	view := RefView{
		RefViewID: "rv-1", GraphID: "graph-1", SelectorKind: "branch", SelectorValue: "main",
		DesiredRef: "refs/heads/main", DesiredCommit: "c0ffee", DesiredTree: "7ee7",
		EnrichmentProfile: "default", DesiredBuildFingerprint: "fp-1",
		State: RefViewPending, ExactView: true, LastResolved: 1000,
	}
	if err := catalog.UpsertRefView(ctx, view); err != nil {
		t.Fatalf("UpsertRefView: %v", err)
	}

	duplicate := view
	duplicate.RefViewID = "rv-2"
	if err := catalog.UpsertRefView(ctx, duplicate); err == nil {
		t.Fatal("a second row for the same selector and profile must be rejected")
	} else if !strings.Contains(strings.ToLower(err.Error()), "unique") {
		t.Fatalf("duplicate selector rejection = %v, want a uniqueness failure", err)
	}

	// A different enrichment profile is a different view of the same selector.
	otherProfile := view
	otherProfile.RefViewID = "rv-3"
	otherProfile.EnrichmentProfile = "deep"
	if err := catalog.UpsertRefView(ctx, otherProfile); err != nil {
		t.Fatalf("second profile for the same selector: %v", err)
	}

	build := RefViewBuild{
		BuildID: "build-1", RefViewID: "rv-1", DesiredRef: "refs/heads/main",
		DesiredCommit: "c0ffee", DesiredTree: "7ee7", BaseGenerationID: base,
		EnrichmentProfile: "default", BuildFingerprint: "fp-1",
		CapturedRouteEpoch: 3, State: ViewGenerationBuilding,
		BuildToken: "token-1", CreatedAt: 1001,
	}
	if err := catalog.UpsertRefViewBuild(ctx, build); err != nil {
		t.Fatalf("UpsertRefViewBuild: %v", err)
	}

	racing := build
	racing.BuildID = "build-2"
	racing.BuildToken = "token-2"
	if err := catalog.UpsertRefViewBuild(ctx, racing); err == nil {
		t.Fatal("a second in-flight build for the same work must be rejected")
	} else if !strings.Contains(strings.ToLower(err.Error()), "unique") {
		t.Fatalf("coalescing rejection = %v, want a uniqueness failure", err)
	}

	// Once the first attempt leaves the building state the slot is free again.
	finished := build
	finished.State = ViewGenerationReady
	finished.LastProgress = 1002
	if err := catalog.UpsertRefViewBuild(ctx, finished); err != nil {
		t.Fatalf("finish first build: %v", err)
	}
	if err := catalog.UpsertRefViewBuild(ctx, racing); err != nil {
		t.Fatalf("retry after the first build finished: %v", err)
	}

	stored, ok, err := catalog.GetRefViewBuild(ctx, "build-1")
	if err != nil || !ok {
		t.Fatalf("GetRefViewBuild = %v, %v, %v", stored, ok, err)
	}
	if stored.State != ViewGenerationReady || stored.BaseGenerationID != base || stored.CapturedRouteEpoch != 3 {
		t.Fatalf("build round trip = %+v", stored)
	}

	// Deleting a ref view takes its builds with it.
	if _, err := store.writerDB.ExecContext(ctx, `DELETE FROM ref_views WHERE ref_view_id = ?`, "rv-1"); err != nil {
		t.Fatalf("delete ref view: %v", err)
	}
	if stored, ok, err := catalog.GetRefViewBuild(ctx, "build-1"); err != nil || ok {
		t.Fatalf("builds survived their ref view: %+v, %v, %v", stored, ok, err)
	}
	if stored, ok, err := catalog.GetRefView(ctx, "rv-3"); err != nil || !ok || stored.EnrichmentProfile != "deep" {
		t.Fatalf("unrelated ref view = %+v, %v, %v", stored, ok, err)
	}
}

// seedRefView creates a view through GetOrCreateRefView and returns the row.
func seedRefView(t *testing.T, catalog *Catalog, refViewID, graphID string) RefView {
	t.Helper()
	view, err := catalog.GetOrCreateRefView(context.Background(), RefView{
		RefViewID: refViewID, GraphID: graphID, SelectorKind: "git_ref",
		SelectorValue: "refs/heads/" + refViewID, EnrichmentProfile: "default",
		State: RefViewPending, ExactView: true,
	})
	if err != nil {
		t.Fatalf("GetOrCreateRefView: %v", err)
	}
	return view
}

func readRefView(t *testing.T, catalog *Catalog, refViewID string) RefView {
	t.Helper()
	view, found, err := catalog.GetRefView(context.Background(), refViewID)
	if err != nil || !found {
		t.Fatalf("GetRefView(%s) = %v, %v, %v", refViewID, view, found, err)
	}
	return view
}

// TestCatalogRefViewCreationIsIdempotent proves that a second selection of a
// view does not reset the row the first one advanced: the create declines the
// conflict and hands back what is stored.
func TestCatalogRefViewCreationIsIdempotent(t *testing.T) {
	store := openCatalogStore(t)
	ctx := context.Background()
	catalog := store.Catalog()

	first := seedRefView(t, catalog, "rv-1", "graph-1")
	if first.State != RefViewPending || first.RouteEpoch != 0 {
		t.Fatalf("created view = %+v", first)
	}

	err := catalog.UpdateRefViewDesire(ctx, UpdateRefViewDesireRequest{
		RefViewID: "rv-1", DesiredRef: "refs/heads/rv-1", DesiredCommit: "c1",
		DesiredTree: "t1", DesiredBuildFingerprint: "fp-1",
		State: RefViewBuilding, LastResolved: 10, LastSelected: 10,
	})
	if err != nil {
		t.Fatalf("UpdateRefViewDesire: %v", err)
	}

	// A second creator arrives with a pristine row. It must change nothing.
	second := seedRefView(t, catalog, "rv-1", "graph-1")
	if second.DesiredTree != "t1" || second.State != RefViewBuilding || second.RouteEpoch != 1 {
		t.Fatalf("a second create reset the row: %+v", second)
	}
}

// TestCatalogRefViewDesireMovesTheEpochOnlyOnMovement is what makes two
// concurrent selections of one view able to share a build: writing the same
// desire twice must not invalidate the epoch the first one's build captured,
// while re-targeting the view must.
func TestCatalogRefViewDesireMovesTheEpochOnlyOnMovement(t *testing.T) {
	store := openCatalogStore(t)
	ctx := context.Background()
	catalog := store.Catalog()
	seedRefView(t, catalog, "rv-1", "graph-1")

	desire := UpdateRefViewDesireRequest{
		RefViewID: "rv-1", DesiredRef: "refs/heads/rv-1", DesiredCommit: "c1",
		DesiredTree: "t1", DesiredBuildFingerprint: "fp-1",
		State: RefViewBuilding, LastResolved: 10, LastSelected: 10,
	}
	if err := catalog.UpdateRefViewDesire(ctx, desire); err != nil {
		t.Fatalf("first desire: %v", err)
	}
	if epoch := readRefView(t, catalog, "rv-1").RouteEpoch; epoch != 1 {
		t.Fatalf("first desire left epoch %d, want 1", epoch)
	}

	// The same tree reached by a different commit — a rebase, an amend. The
	// payload is unchanged, so the epoch must not move.
	same := desire
	same.DesiredCommit = "c2"
	same.LastSelected = 20
	if err := catalog.UpdateRefViewDesire(ctx, same); err != nil {
		t.Fatalf("same-tree desire: %v", err)
	}
	view := readRefView(t, catalog, "rv-1")
	if view.RouteEpoch != 1 {
		t.Fatalf("a commit that changed no tree moved the epoch to %d", view.RouteEpoch)
	}
	if view.DesiredCommit != "c2" || view.LastSelected != 20 {
		t.Fatalf("same-tree desire = %+v", view)
	}

	moved := desire
	moved.DesiredTree, moved.DesiredBuildFingerprint = "t2", "fp-2"
	if err := catalog.UpdateRefViewDesire(ctx, moved); err != nil {
		t.Fatalf("moved desire: %v", err)
	}
	if epoch := readRefView(t, catalog, "rv-1").RouteEpoch; epoch != 2 {
		t.Fatalf("re-targeting the view left epoch %d, want 2", epoch)
	}
}

// TestCatalogRefViewAdoptionRevalidates proves the publish-side compare-and-
// set: a generation is adopted only while the epoch, the tree and the
// fingerprint the build captured all still stand.
func TestCatalogRefViewAdoptionRevalidates(t *testing.T) {
	store := openCatalogStore(t)
	ctx := context.Background()
	catalog := store.Catalog()
	seedRefView(t, catalog, "rv-1", "graph-1")
	generation := seedBuildingGeneration(t, catalog, "graph-1")

	desire := UpdateRefViewDesireRequest{
		RefViewID: "rv-1", DesiredRef: "refs/heads/rv-1", DesiredCommit: "c1",
		DesiredTree: "t1", DesiredBuildFingerprint: "fp-1",
		State: RefViewBuilding, LastResolved: 10, LastSelected: 10,
	}
	if err := catalog.UpdateRefViewDesire(ctx, desire); err != nil {
		t.Fatalf("desire: %v", err)
	}
	captured := readRefView(t, catalog, "rv-1").RouteEpoch

	adopt := AdoptRefViewGenerationRequest{
		RefViewID: "rv-1", ExpectedRouteEpoch: captured,
		ExpectedDesiredTree: "t1", ExpectedDesiredBuildFingerprint: "fp-1",
		GenerationID: generation, ActiveRef: "refs/heads/rv-1", ActiveCommit: "c1",
		ActiveTree: "t1", ActiveBuildFingerprint: "fp-1", ExactView: true,
		LastResolved: 20, LastSelected: 20,
	}

	stale := []struct {
		name   string
		mutate func(*AdoptRefViewGenerationRequest)
	}{
		{"a lost epoch", func(r *AdoptRefViewGenerationRequest) { r.ExpectedRouteEpoch = captured + 1 }},
		{"a moved tree", func(r *AdoptRefViewGenerationRequest) { r.ExpectedDesiredTree = "t2" }},
		{"a changed fingerprint", func(r *AdoptRefViewGenerationRequest) { r.ExpectedDesiredBuildFingerprint = "fp-2" }},
	}
	for _, tc := range stale {
		t.Run(tc.name, func(t *testing.T) {
			req := adopt
			tc.mutate(&req)
			if err := catalog.AdoptRefViewGeneration(ctx, req); !errors.Is(err, ErrCatalogStaleGuard) {
				t.Fatalf("adoption under %s = %v, want ErrCatalogStaleGuard", tc.name, err)
			}
			if view := readRefView(t, catalog, "rv-1"); view.ActiveGenerationID != 0 {
				t.Fatalf("a refused adoption flipped the active pointer: %+v", view)
			}
		})
	}

	if err := catalog.AdoptRefViewGeneration(ctx, adopt); err != nil {
		t.Fatalf("adoption: %v", err)
	}
	view := readRefView(t, catalog, "rv-1")
	if view.ActiveGenerationID != generation || view.ActiveCommit != "c1" || view.State != RefViewReady {
		t.Fatalf("adopted view = %+v", view)
	}
	if view.RouteEpoch != captured+1 {
		t.Fatalf("adoption left epoch %d, want %d", view.RouteEpoch, captured+1)
	}

	// A second adoption carrying the epoch the first one consumed is exactly
	// the losing build, and it must not overwrite the winner.
	if err := catalog.AdoptRefViewGeneration(ctx, adopt); !errors.Is(err, ErrCatalogStaleGuard) {
		t.Fatalf("replayed adoption = %v, want ErrCatalogStaleGuard", err)
	}
}

// TestCatalogRefViewAdoptionRefusesAReclaimedClaim pins what the claim buys a
// build: the right to publish. A pass whose slot was reclaimed while it ran no
// longer holds that right, and a late adoption behind the successor would put
// a payload nobody is waiting on in front of the one somebody is.
//
// The two writes are one transaction, so a refused adoption also leaves the
// attempt open — an attempt recorded as finished on a generation the view
// never took would be a build history that lies.
func TestCatalogRefViewAdoptionRefusesAReclaimedClaim(t *testing.T) {
	store := openCatalogStore(t)
	ctx := context.Background()
	catalog := store.Catalog()
	seedRefView(t, catalog, "rv-1", "graph-1")
	generation := seedBuildingGeneration(t, catalog, "graph-1")

	err := catalog.UpdateRefViewDesire(ctx, UpdateRefViewDesireRequest{
		RefViewID: "rv-1", DesiredRef: "refs/heads/rv-1", DesiredCommit: "c1",
		DesiredTree: "t1", DesiredBuildFingerprint: "fp-1",
		State: RefViewBuilding, LastResolved: 10, LastSelected: 10,
	})
	if err != nil {
		t.Fatalf("desire: %v", err)
	}
	captured := readRefView(t, catalog, "rv-1").RouteEpoch

	reaped := RefViewBuild{
		BuildID: "build-reaped", RefViewID: "rv-1", DesiredRef: "refs/heads/rv-1",
		DesiredCommit: "c1", DesiredTree: "t1", BaseGenerationID: 0,
		EnrichmentProfile: "default", BuildFingerprint: "fp-1",
		CapturedRouteEpoch: captured, State: ViewGenerationBuilding,
		BuildToken: "token-reaped", CreatedAt: 100, LastProgress: 100,
	}
	if _, err := catalog.ClaimRefViewBuild(ctx, reaped, 0); err != nil {
		t.Fatalf("seed the attempt that is about to be reclaimed: %v", err)
	}
	successor := reaped
	successor.BuildID, successor.BuildToken = "build-live", "token-live"
	successor.CreatedAt, successor.LastProgress = 900, 900
	if _, err := catalog.ClaimRefViewBuild(ctx, successor, 500); err != nil {
		t.Fatalf("reclaim the slot: %v", err)
	}

	adopt := AdoptRefViewGenerationRequest{
		RefViewID: "rv-1", ExpectedRouteEpoch: captured,
		ExpectedDesiredTree: "t1", ExpectedDesiredBuildFingerprint: "fp-1",
		GenerationID: generation, ActiveRef: "refs/heads/rv-1", ActiveCommit: "c1",
		ActiveTree: "t1", ActiveBuildFingerprint: "fp-1", ExactView: true,
		LastResolved: 20, LastSelected: 20,
		BuildID: "build-reaped", BuildToken: "token-reaped", LastProgress: 950,
	}
	if err := catalog.AdoptRefViewGeneration(ctx, adopt); !errors.Is(err, ErrCatalogStaleGuard) {
		t.Fatalf("adoption behind a reclaimed claim = %v, want ErrCatalogStaleGuard", err)
	}
	if view := readRefView(t, catalog, "rv-1"); view.ActiveGenerationID != 0 {
		t.Fatalf("a reclaimed attempt published its generation: %+v", view)
	}

	// A view that moved refuses the adoption for the other reason, and the
	// live attempt must come out of it exactly as it went in.
	moved := adopt
	moved.BuildID, moved.BuildToken = "build-live", "token-live"
	moved.ExpectedRouteEpoch = captured + 1
	if err := catalog.AdoptRefViewGeneration(ctx, moved); !errors.Is(err, ErrCatalogStaleGuard) {
		t.Fatalf("adoption at a lost epoch = %v, want ErrCatalogStaleGuard", err)
	}
	held, found, err := catalog.GetRefViewBuild(ctx, "build-live")
	if err != nil || !found {
		t.Fatalf("read the live attempt: found=%v err=%v", found, err)
	}
	if held.State != ViewGenerationBuilding || held.GenerationID != 0 {
		t.Fatalf("a refused adoption closed the attempt anyway: %+v", held)
	}

	adopt.BuildID, adopt.BuildToken = "build-live", "token-live"
	if err := catalog.AdoptRefViewGeneration(ctx, adopt); err != nil {
		t.Fatalf("adoption under the live claim: %v", err)
	}
	if view := readRefView(t, catalog, "rv-1"); view.ActiveGenerationID != generation {
		t.Fatalf("adopted view = %+v, want generation %d", view, generation)
	}
	closed, _, err := catalog.GetRefViewBuild(ctx, "build-live")
	if err != nil {
		t.Fatalf("re-read the live attempt: %v", err)
	}
	if closed.State != ViewGenerationReady || closed.GenerationID != generation {
		t.Fatalf("the live attempt = %+v, want it closed on generation %d", closed, generation)
	}
}

// TestCatalogRefViewBuildProgressKeepsAClaimAlive pins the heartbeat's whole
// job. The liveness window reads last_progress and nothing else, so a claim
// that never re-stamps it is indistinguishable from one whose worker died the
// moment it made it — and a build that outlasts the window is reclaimed while
// it is still running.
func TestCatalogRefViewBuildProgressKeepsAClaimAlive(t *testing.T) {
	store := openCatalogStore(t)
	ctx := context.Background()
	catalog := store.Catalog()
	seedRefView(t, catalog, "rv-1", "graph-1")

	build := RefViewBuild{
		BuildID: "build-1", RefViewID: "rv-1", DesiredRef: "refs/heads/rv-1",
		DesiredCommit: "c1", DesiredTree: "t1", BaseGenerationID: 0,
		EnrichmentProfile: "default", BuildFingerprint: "fp-1",
		CapturedRouteEpoch: 1, State: ViewGenerationBuilding,
		BuildToken: "token-1", CreatedAt: 100, LastProgress: 100,
	}
	if _, err := catalog.ClaimRefViewBuild(ctx, build, 0); err != nil {
		t.Fatalf("ClaimRefViewBuild: %v", err)
	}

	// The token is the proof of who is behind the claim, so it guards the
	// stamp exactly as it guards the completion.
	if err := catalog.TouchRefViewBuild(ctx, "build-1", "token-9", 900); !errors.Is(err, ErrCatalogStaleGuard) {
		t.Fatalf("progress under a foreign token = %v, want ErrCatalogStaleGuard", err)
	}
	if err := catalog.TouchRefViewBuild(ctx, "build-1", "token-1", 900); err != nil {
		t.Fatalf("TouchRefViewBuild: %v", err)
	}

	racing := build
	racing.BuildID, racing.BuildToken = "build-2", "token-2"
	racing.CreatedAt, racing.LastProgress = 950, 950
	inFlight, err := catalog.ClaimRefViewBuild(ctx, racing, 500)
	if !errors.Is(err, ErrRefViewBuildInFlight) {
		t.Fatalf("claim against a stamped attempt = %v, want ErrRefViewBuildInFlight", err)
	}
	if inFlight.BuildToken != "token-1" || inFlight.LastProgress != 900 {
		t.Fatalf("claim returned %+v, want the attempt at its stamped progress", inFlight)
	}
	if rows, err := catalog.ListRefViewBuilds(ctx, "rv-1"); err != nil || len(rows) != 1 {
		t.Fatalf("ListRefViewBuilds = %+v, %v, want the one live attempt", rows, err)
	}

	// An attempt out of the building state is finished, not slow. Re-stamping
	// it would resurrect a claim the coalescing index has already released.
	err = catalog.CompleteRefViewBuild(ctx, CompleteRefViewBuildRequest{
		BuildID: "build-1", BuildToken: "token-1", State: ViewGenerationReady,
		GenerationID: 7, LastProgress: 1000,
	})
	if err != nil {
		t.Fatalf("CompleteRefViewBuild: %v", err)
	}
	if err := catalog.TouchRefViewBuild(ctx, "build-1", "token-1", 1100); !errors.Is(err, ErrCatalogStaleGuard) {
		t.Fatalf("progress on a finished attempt = %v, want ErrCatalogStaleGuard", err)
	}
}

// TestCatalogRefViewMetadataAndFailureLeaveTheActivePointer covers the two
// writes that must never move what a view serves: stamping a moved ref, and
// recording a failed selection.
func TestCatalogRefViewMetadataAndFailureLeaveTheActivePointer(t *testing.T) {
	store := openCatalogStore(t)
	ctx := context.Background()
	catalog := store.Catalog()
	seedRefView(t, catalog, "rv-1", "graph-1")
	generation := seedBuildingGeneration(t, catalog, "graph-1")

	err := catalog.UpdateRefViewDesire(ctx, UpdateRefViewDesireRequest{
		RefViewID: "rv-1", DesiredRef: "refs/heads/rv-1", DesiredCommit: "c1",
		DesiredTree: "t1", DesiredBuildFingerprint: "fp-1",
		State: RefViewBuilding, LastResolved: 10, LastSelected: 10,
	})
	if err != nil {
		t.Fatalf("desire: %v", err)
	}
	epoch := readRefView(t, catalog, "rv-1").RouteEpoch
	err = catalog.AdoptRefViewGeneration(ctx, AdoptRefViewGenerationRequest{
		RefViewID: "rv-1", ExpectedRouteEpoch: epoch,
		ExpectedDesiredTree: "t1", ExpectedDesiredBuildFingerprint: "fp-1",
		GenerationID: generation, ActiveRef: "refs/heads/rv-1", ActiveCommit: "c1",
		ActiveTree: "t1", ActiveBuildFingerprint: "fp-1", ExactView: true,
		LastResolved: 20, LastSelected: 20,
	})
	if err != nil {
		t.Fatalf("adoption: %v", err)
	}
	epoch = readRefView(t, catalog, "rv-1").RouteEpoch

	touch := TouchRefViewSelectionRequest{
		RefViewID: "rv-1", ExpectedRouteEpoch: epoch,
		ActiveRef: "refs/heads/rv-1", ActiveCommit: "c2",
		LastResolved: 30, LastSelected: 30,
	}
	if err := catalog.TouchRefViewSelection(ctx, touch); err != nil {
		t.Fatalf("TouchRefViewSelection: %v", err)
	}
	view := readRefView(t, catalog, "rv-1")
	if view.ActiveCommit != "c2" || view.ActiveGenerationID != generation || view.ActiveTree != "t1" {
		t.Fatalf("touched view = %+v", view)
	}
	if view.RouteEpoch != epoch {
		t.Fatalf("a metadata stamp moved the epoch to %d", view.RouteEpoch)
	}

	stale := touch
	stale.ExpectedRouteEpoch = epoch + 1
	stale.ActiveCommit = "c3"
	if err := catalog.TouchRefViewSelection(ctx, stale); !errors.Is(err, ErrCatalogStaleGuard) {
		t.Fatalf("stale touch = %v, want ErrCatalogStaleGuard", err)
	}

	err = catalog.FailRefView(ctx, FailRefViewRequest{
		RefViewID: "rv-1", ExpectedRouteEpoch: epoch,
		LastError: "ref is not available in the local object store", LastResolved: 40,
	})
	if err != nil {
		t.Fatalf("FailRefView: %v", err)
	}
	failed := readRefView(t, catalog, "rv-1")
	if failed.State != RefViewFailed || failed.LastError == "" {
		t.Fatalf("failed view = %+v", failed)
	}
	if failed.ActiveGenerationID != generation || failed.ActiveCommit != "c2" {
		t.Fatalf("a failed selection moved what the view serves: %+v", failed)
	}
}

// TestCatalogRefViewBuildClaimHandsBackTheInFlightAttempt is the coalescing
// contract from the claimant's side: the loser gets the winner's row rather
// than a bare constraint failure, a base of zero coalesces like any other base,
// and finishing the attempt frees the slot for the next one.
func TestCatalogRefViewBuildClaimHandsBackTheInFlightAttempt(t *testing.T) {
	store := openCatalogStore(t)
	ctx := context.Background()
	catalog := store.Catalog()
	seedRefView(t, catalog, "rv-1", "graph-1")

	// Base zero is the base corpus — the case a plainly indexed graph is in,
	// and therefore the one coalescing has to cover.
	build := RefViewBuild{
		BuildID: "build-1", RefViewID: "rv-1", DesiredRef: "refs/heads/rv-1",
		DesiredCommit: "c1", DesiredTree: "t1", BaseGenerationID: 0,
		EnrichmentProfile: "default", BuildFingerprint: "fp-1",
		CapturedRouteEpoch: 1, State: ViewGenerationBuilding,
		BuildToken: "token-1", CreatedAt: 100, LastProgress: 100,
	}
	claimed, err := catalog.ClaimRefViewBuild(ctx, build, 0)
	if err != nil {
		t.Fatalf("ClaimRefViewBuild: %v", err)
	}
	if claimed.BuildToken != "token-1" {
		t.Fatalf("claim = %+v, want the caller's own attempt", claimed)
	}

	racing := build
	racing.BuildID, racing.BuildToken = "build-2", "token-2"
	// A cutoff at the in-flight row's own progress stamp does not make it
	// abandoned, so this collision is coalescing and nothing else.
	inFlight, err := catalog.ClaimRefViewBuild(ctx, racing, build.LastProgress)
	if !errors.Is(err, ErrRefViewBuildInFlight) {
		t.Fatalf("second claim = %v, want ErrRefViewBuildInFlight", err)
	}
	if inFlight.BuildID != "build-1" || inFlight.BuildToken != "token-1" {
		t.Fatalf("second claim returned %+v, want the in-flight attempt", inFlight)
	}
	if rows, err := catalog.ListRefViewBuilds(ctx, "rv-1"); err != nil || len(rows) != 1 {
		t.Fatalf("ListRefViewBuilds = %+v, %v, want the one claimed attempt", rows, err)
	}

	// A build for a different tree is different work and claims its own slot.
	other := build
	other.BuildID, other.BuildToken = "build-3", "token-3"
	other.DesiredTree, other.BuildFingerprint = "t2", "fp-2"
	if _, err := catalog.ClaimRefViewBuild(ctx, other, 0); err != nil {
		t.Fatalf("claim for a different tree: %v", err)
	}

	complete := CompleteRefViewBuildRequest{
		BuildID: "build-1", BuildToken: "token-9", State: ViewGenerationReady,
		GenerationID: 7, LastProgress: 200,
	}
	if err := catalog.CompleteRefViewBuild(ctx, complete); !errors.Is(err, ErrCatalogStaleGuard) {
		t.Fatalf("completion with the wrong token = %v, want ErrCatalogStaleGuard", err)
	}
	complete.BuildToken = "token-1"
	if err := catalog.CompleteRefViewBuild(ctx, complete); err != nil {
		t.Fatalf("CompleteRefViewBuild: %v", err)
	}
	if err := catalog.CompleteRefViewBuild(ctx, complete); !errors.Is(err, ErrCatalogStaleGuard) {
		t.Fatalf("second completion = %v, want ErrCatalogStaleGuard", err)
	}

	// The slot is free again now that the first attempt has left the building
	// state, so the retry claims it outright.
	retry, err := catalog.ClaimRefViewBuild(ctx, racing, 0)
	if err != nil {
		t.Fatalf("retry after the first attempt finished: %v", err)
	}
	if retry.BuildToken != "token-2" {
		t.Fatalf("retry = %+v, want its own attempt", retry)
	}
	rows, err := catalog.ListRefViewBuilds(ctx, "rv-1")
	if err != nil || len(rows) != 3 {
		t.Fatalf("ListRefViewBuilds = %+v, %v, want three recorded attempts", rows, err)
	}
}

// TestCatalogRefViewBuildClaimReclaimsAnAbandonedAttempt pins the liveness
// rule. A claim outlives the worker that made it, so a row that stopped
// reporting progress is wreckage rather than work in flight: the next claimant
// fails it and takes the slot, instead of being handed a token nobody is
// running and waiting on it forever.
func TestCatalogRefViewBuildClaimReclaimsAnAbandonedAttempt(t *testing.T) {
	store := openCatalogStore(t)
	ctx := context.Background()
	catalog := store.Catalog()
	seedRefView(t, catalog, "rv-1", "graph-1")

	abandoned := RefViewBuild{
		BuildID: "build-dead", RefViewID: "rv-1", DesiredRef: "refs/heads/rv-1",
		DesiredCommit: "c1", DesiredTree: "t1", BaseGenerationID: 0,
		EnrichmentProfile: "default", BuildFingerprint: "fp-1",
		CapturedRouteEpoch: 1, State: ViewGenerationBuilding,
		BuildToken: "token-dead", CreatedAt: 100, LastProgress: 100,
	}
	if _, err := catalog.ClaimRefViewBuild(ctx, abandoned, 0); err != nil {
		t.Fatalf("seed the abandoned attempt: %v", err)
	}

	successor := abandoned
	successor.BuildID, successor.BuildToken = "build-live", "token-live"
	successor.CreatedAt, successor.LastProgress = 900, 900
	claimed, err := catalog.ClaimRefViewBuild(ctx, successor, 500)
	if err != nil {
		t.Fatalf("claim over an abandoned attempt: %v", err)
	}
	if claimed.BuildToken != "token-live" {
		t.Fatalf("claim = %+v, want the successor's own attempt", claimed)
	}

	rows, err := catalog.ListRefViewBuilds(ctx, "rv-1")
	if err != nil || len(rows) != 2 {
		t.Fatalf("ListRefViewBuilds = %+v, %v, want both attempts recorded", rows, err)
	}
	byID := map[string]RefViewBuild{}
	for _, row := range rows {
		byID[row.BuildID] = row
	}
	if dead := byID["build-dead"]; dead.State != ViewGenerationFailed || dead.Error == "" {
		t.Fatalf("the abandoned attempt = %+v, want it failed with a recorded cause", dead)
	}
	if dead := byID["build-dead"]; dead.LastProgress != successor.LastProgress {
		t.Fatalf("the abandoned attempt's progress = %d, want the clock that reclaimed it (%d)",
			dead.LastProgress, successor.LastProgress)
	}
	if live := byID["build-live"]; live.State != ViewGenerationBuilding {
		t.Fatalf("the successor = %+v, want it in flight", live)
	}

	// The successor is live, so the next claimant coalesces on it rather than
	// stealing it in turn.
	racing := successor
	racing.BuildID, racing.BuildToken = "build-racing", "token-racing"
	inFlight, err := catalog.ClaimRefViewBuild(ctx, racing, successor.LastProgress)
	if !errors.Is(err, ErrRefViewBuildInFlight) {
		t.Fatalf("claim against a live attempt = %v, want ErrRefViewBuildInFlight", err)
	}
	if inFlight.BuildToken != "token-live" {
		t.Fatalf("claim against a live attempt returned %+v, want the successor", inFlight)
	}
}

// TestCatalogIntentTransitionIsSinglePerCheckout proves the UNIQUE(checkout_id)
// contract: one transition at a time, the checkout points at it while it is in
// flight, completing it frees the slot, and a stale completion is refused.
func TestCatalogIntentTransitionIsSinglePerCheckout(t *testing.T) {
	store := openCatalogStore(t)
	ctx := context.Background()
	catalog := store.Catalog()
	seedFamilyAndCheckout(t, catalog, "fam", "wt", "inc-1")

	first := IntentTransition{
		TransitionID: "trans-1", CheckoutID: "wt", Cause: "user_requested_dedicated",
		PriorDesiredMode: CheckoutModeAutomatic, PriorEffectiveMode: CheckoutModeAutomatic,
		RequestedMode: CheckoutModeDedicated, PriorCheckoutState: CheckoutStateReady,
		SourceSnapshotHash: "hash-1", State: IntentTransitionRunning, CreatedAt: 1100,
	}
	if err := catalog.BeginIntentTransition(ctx, first); err != nil {
		t.Fatalf("BeginIntentTransition: %v", err)
	}
	checkout, _, err := catalog.GetCheckout(ctx, "wt")
	if err != nil {
		t.Fatalf("GetCheckout: %v", err)
	}
	if checkout.ActiveIntentTransitionID != "trans-1" {
		t.Fatalf("checkout does not point at its transition: %+v", checkout)
	}

	second := first
	second.TransitionID = "trans-2"
	if err := catalog.BeginIntentTransition(ctx, second); !errors.Is(err, ErrCatalogIntentTransitionActive) {
		t.Fatalf("second transition = %v, want ErrCatalogIntentTransitionActive", err)
	}
	stored, ok, err := catalog.GetIntentTransition(ctx, "wt")
	if err != nil || !ok {
		t.Fatalf("GetIntentTransition = %v, %v, %v", stored, ok, err)
	}
	if stored.TransitionID != "trans-1" || stored.State != IntentTransitionRunning ||
		stored.RequestedMode != CheckoutModeDedicated || stored.SourceSnapshotHash != "hash-1" {
		t.Fatalf("transition round trip = %+v", stored)
	}

	if err := catalog.CompleteIntentTransition(ctx, "wt", "trans-2"); !errors.Is(err, ErrCatalogStaleGuard) {
		t.Fatalf("completing someone else's transition = %v, want ErrCatalogStaleGuard", err)
	}
	if _, ok, err := catalog.GetIntentTransition(ctx, "wt"); err != nil || !ok {
		t.Fatalf("a rejected completion must leave the transition: %v, %v", ok, err)
	}

	if err := catalog.CompleteIntentTransition(ctx, "wt", "trans-1"); err != nil {
		t.Fatalf("CompleteIntentTransition: %v", err)
	}
	if stored, ok, err := catalog.GetIntentTransition(ctx, "wt"); err != nil || ok {
		t.Fatalf("completed transition still present: %+v, %v, %v", stored, ok, err)
	}
	checkout, _, err = catalog.GetCheckout(ctx, "wt")
	if err != nil {
		t.Fatalf("GetCheckout: %v", err)
	}
	if checkout.ActiveIntentTransitionID != "" {
		t.Fatalf("completion did not clear the checkout pointer: %+v", checkout)
	}

	// The slot is genuinely free again.
	if err := catalog.BeginIntentTransition(ctx, second); err != nil {
		t.Fatalf("transition after completion: %v", err)
	}

	// A transition for a checkout that does not exist rolls back whole.
	orphan := first
	orphan.TransitionID = "trans-3"
	orphan.CheckoutID = "missing"
	if err := catalog.BeginIntentTransition(ctx, orphan); err == nil {
		t.Fatal("a transition for an unknown checkout must be refused")
	}
	if _, ok, err := catalog.GetIntentTransition(ctx, "missing"); err != nil || ok {
		t.Fatalf("rolled-back transition left a row: %v, %v", ok, err)
	}
}

// TestCatalogModeTransitionAdmissionIsAtomicAndGuarded covers the durable
// admission used by asynchronous promotion: the transition and tracking intent
// appear together, compatible retries adopt one journal row, and stale or
// incompatible requests write neither half.
func TestCatalogModeTransitionAdmissionIsAtomicAndGuarded(t *testing.T) {
	store := openCatalogStore(t)
	ctx := context.Background()
	catalog := store.Catalog()
	seedFamilyAndCheckout(t, catalog, "fam", "wt", "inc-1")

	transition := IntentTransition{
		TransitionID: "transition-1", CheckoutID: "wt", Cause: "promote_checkout",
		PriorDesiredMode: CheckoutModeAutomatic, PriorEffectiveMode: CheckoutModeAutomatic,
		RequestedMode: CheckoutModeDedicated, PriorCheckoutState: CheckoutStateReady,
		State: IntentTransitionPending, CreatedAt: 200, LastProgress: 200,
	}
	intent := TrackingIntent{
		IntentID: "intent-1", CheckoutID: "wt", SourceKind: IntentSourceCLITrack,
		SourceLocator: "/tmp/wt", Active: true, CreatedAt: 200,
	}
	standing, adopted, err := catalog.BeginIntentTransitionWithTrackingIntent(ctx,
		BeginIntentTransitionRequest{Transition: transition, Incarnation: "inc-1", TrackingIntent: &intent})
	if err != nil || adopted || standing.TransitionID != transition.TransitionID {
		t.Fatalf("first admission = %+v, adopted=%v, err=%v", standing, adopted, err)
	}

	retry := transition
	retry.TransitionID = "transition-2"
	standing, adopted, err = catalog.BeginIntentTransitionWithTrackingIntent(ctx,
		BeginIntentTransitionRequest{Transition: retry, Incarnation: "inc-1", TrackingIntent: &intent})
	if err != nil || !adopted || standing.TransitionID != transition.TransitionID {
		t.Fatalf("compatible retry = %+v, adopted=%v, err=%v", standing, adopted, err)
	}
	transitions, err := catalog.ListIntentTransitions(ctx)
	if err != nil || len(transitions) != 1 {
		t.Fatalf("transition journal = %+v, err=%v", transitions, err)
	}
	intents, err := catalog.ListTrackingIntents(ctx, "wt")
	if err != nil || len(intents) != 1 || !intents[0].Active {
		t.Fatalf("tracking intents = %+v, err=%v", intents, err)
	}

	incompatible := retry
	incompatible.Cause = "different_operation"
	otherIntent := intent
	otherIntent.IntentID = "intent-2"
	otherIntent.SourceLocator = "/tmp/other"
	if _, _, err := catalog.BeginIntentTransitionWithTrackingIntent(ctx,
		BeginIntentTransitionRequest{Transition: incompatible, Incarnation: "inc-1", TrackingIntent: &otherIntent}); !errors.Is(err, ErrCatalogStaleGuard) {
		t.Fatalf("incompatible retry = %v, want ErrCatalogStaleGuard", err)
	}
	intents, err = catalog.ListTrackingIntents(ctx, "wt")
	if err != nil || len(intents) != 1 {
		t.Fatalf("incompatible admission wrote intent: %+v, err=%v", intents, err)
	}

	seedFamilyAndCheckout(t, catalog, "fam", "stale", "inc-2")
	stale := transition
	stale.TransitionID = "transition-stale"
	stale.CheckoutID = "stale"
	stale.PriorEffectiveMode = CheckoutModeDedicated
	staleIntent := intent
	staleIntent.IntentID = "intent-stale"
	staleIntent.CheckoutID = "stale"
	staleIntent.SourceLocator = "/tmp/stale"
	if _, _, err := catalog.BeginIntentTransitionWithTrackingIntent(ctx,
		BeginIntentTransitionRequest{Transition: stale, Incarnation: "inc-2", TrackingIntent: &staleIntent}); !errors.Is(err, ErrCatalogStaleGuard) {
		t.Fatalf("stale admission = %v, want ErrCatalogStaleGuard", err)
	}
	if _, found, err := catalog.GetIntentTransition(ctx, "stale"); err != nil || found {
		t.Fatalf("stale admission wrote transition: found=%v err=%v", found, err)
	}
	if intents, err := catalog.ListTrackingIntents(ctx, "stale"); err != nil || len(intents) != 0 {
		t.Fatalf("stale admission wrote intent: %+v, err=%v", intents, err)
	}
}

func TestCatalogAuthorizedDemotionPreservesTransitionUntilCleanupCompletes(t *testing.T) {
	store := openCatalogStore(t)
	ctx := context.Background()
	catalog := store.Catalog()
	if err := catalog.UpsertRepositoryFamily(ctx, RepositoryFamily{
		FamilyID: "fam-demote", CommonDirIdentity: "fam-demote", State: "family_ready",
		PrimaryEpoch: 7, CreatedAt: 1, LastSeen: 1,
	}); err != nil {
		t.Fatal(err)
	}
	for _, checkout := range []Checkout{
		{CheckoutID: "primary-wt", Incarnation: "primary-inc", FamilyID: "fam-demote",
			RootPath: "/tmp/primary-wt", GitDir: "/tmp/primary-wt/.git", AdminName: "primary-wt",
			State: CheckoutStateReady, DesiredMode: CheckoutModeDedicated,
			EffectiveMode: CheckoutModeDedicated, HeadCommit: "c0ffee", HeadTree: "7ee7", LastSeen: 1},
		{CheckoutID: "demoted-wt", Incarnation: "demoted-inc", FamilyID: "fam-demote",
			RootPath: "/tmp/demoted-wt", GitDir: "/tmp/demoted-wt/.git", AdminName: "demoted-wt",
			State: CheckoutStateReady, DesiredMode: CheckoutModeDedicated,
			EffectiveMode: CheckoutModeDedicated, HeadCommit: "c0ffee", HeadTree: "7ee7", LastSeen: 1},
	} {
		if err := catalog.UpsertCheckout(ctx, checkout); err != nil {
			t.Fatal(err)
		}
	}
	for _, graph := range []DedicatedGraph{
		{GraphID: "primary-graph", OwnerCheckoutID: "primary-wt", RepoPrefix: "/tmp/primary-wt",
			FamilyID: "fam-demote", IsPrimaryBase: true, State: DedicatedGraphStateReady},
		{GraphID: "demoted-graph", OwnerCheckoutID: "demoted-wt", RepoPrefix: "/tmp/demoted-wt",
			FamilyID: "fam-demote", State: DedicatedGraphStateReady},
	} {
		if err := catalog.UpsertDedicatedGraph(ctx, graph); err != nil {
			t.Fatal(err)
		}
	}
	baseGenerationID, err := catalog.CreateViewGeneration(ctx, ViewGeneration{
		OwnerKind: checkoutGenerationOwnerKind, GraphID: "primary-graph",
		LayerID: "demotion-primary-base", CheckoutID: "primary-wt",
		GenerationKind: "dedicated_base", TreeOID: "7ee7", State: ViewGenerationReady,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := catalog.UpsertDedicatedGraph(ctx, DedicatedGraph{
		GraphID: "primary-graph", OwnerCheckoutID: "primary-wt", RepoPrefix: "/tmp/primary-wt",
		FamilyID: "fam-demote", IsPrimaryBase: true, ActiveGenerationID: baseGenerationID,
		State: DedicatedGraphStateReady,
	}); err != nil {
		t.Fatal(err)
	}
	commitGenerationID, err := catalog.CreateViewGeneration(ctx, ViewGeneration{
		OwnerKind: checkoutGenerationOwnerKind, GraphID: "primary-graph",
		LayerID: "demotion-commit", CheckoutID: "demoted-wt",
		GenerationKind: "checkout_commit", BaseGenerationID: baseGenerationID,
		TreeOID: "7ee7", State: ViewGenerationReady,
	})
	if err != nil {
		t.Fatal(err)
	}
	dirtyGenerationID, err := catalog.CreateViewGeneration(ctx, ViewGeneration{
		OwnerKind: checkoutGenerationOwnerKind, GraphID: "primary-graph",
		LayerID: "demotion-dirty", CheckoutID: "demoted-wt",
		GenerationKind: "checkout_dirty", BaseGenerationID: commitGenerationID,
		State: ViewGenerationReady,
	})
	if err != nil {
		t.Fatal(err)
	}
	expectedRoute := CheckoutRoute{
		CheckoutID: "demoted-wt", GraphID: "demoted-graph", RouteEpoch: 4, State: RouteActive,
	}
	if err := catalog.UpsertCheckoutRoute(ctx, expectedRoute); err != nil {
		t.Fatal(err)
	}
	transition := IntentTransition{
		TransitionID: "demotion-transition", CheckoutID: "demoted-wt",
		Cause: "explicit_untrack_demote", PriorDesiredMode: CheckoutModeDedicated,
		PriorEffectiveMode: CheckoutModeDedicated, RequestedMode: CheckoutModeAutomatic,
		PriorCheckoutState: CheckoutStateReady, SourceSnapshotHash: "demoted-graph:primary-graph:7",
		State: IntentTransitionPending, CreatedAt: 2, LastProgress: 2,
	}
	if err := catalog.BeginIntentTransition(ctx, transition); err != nil {
		t.Fatal(err)
	}
	if err := catalog.CommitAuthorizedDemotion(ctx, CommitAuthorizedDemotionRequest{
		CheckoutID: "demoted-wt", Incarnation: "demoted-inc", FamilyID: "fam-demote",
		TransitionID: transition.TransitionID, OwnedGraphID: "demoted-graph",
		PrimaryGraphID: "primary-graph", ExpectedPrimaryEpoch: 7,
		RequiredPrimaryState: DedicatedGraphStateReady,
		ExpectedRoute:        expectedRoute, RouteExists: true,
		CommitGenerationID: commitGenerationID, DirtyGenerationID: dirtyGenerationID,
		State: CheckoutStateReady, LastSeen: 3,
	}); err != nil {
		t.Fatal(err)
	}
	checkout, found, err := catalog.GetCheckout(ctx, "demoted-wt")
	if err != nil || !found {
		t.Fatalf("GetCheckout after commit: found=%v err=%v", found, err)
	}
	if checkout.EffectiveMode != CheckoutModeAutomatic ||
		checkout.ActiveIntentTransitionID != transition.TransitionID {
		t.Fatalf("post-commit checkout = %+v, want automatic with cleanup-pending transition", checkout)
	}
	standing, found, err := catalog.GetIntentTransition(ctx, "demoted-wt")
	if err != nil || !found || standing.TransitionID != transition.TransitionID {
		t.Fatalf("post-commit transition = %+v, found=%v err=%v", standing, found, err)
	}
	if err := catalog.CompleteIntentTransition(ctx, "demoted-wt", transition.TransitionID); err != nil {
		t.Fatal(err)
	}
	checkout, found, err = catalog.GetCheckout(ctx, "demoted-wt")
	if err != nil || !found || checkout.ActiveIntentTransitionID != "" {
		t.Fatalf("checkout after completion = %+v, found=%v err=%v", checkout, found, err)
	}
	if _, found, err := catalog.GetIntentTransition(ctx, "demoted-wt"); err != nil || found {
		t.Fatalf("transition after completion: found=%v err=%v", found, err)
	}
}

func BenchmarkGetDedicatedGraphByOwnerReadyHit(b *testing.B) {
	store := openCatalogStore(b)
	ctx := context.Background()
	catalog := store.Catalog()
	const (
		familyID   = "bench-owner-family"
		checkoutID = "bench-owner-checkout"
		graphID    = "bench-owner-graph"
	)
	seedFamilyAndCheckout(b, catalog, familyID, checkoutID, "bench-owner-incarnation")
	if err := catalog.UpsertDedicatedGraph(ctx, DedicatedGraph{
		GraphID: graphID, OwnerCheckoutID: checkoutID, RepoPrefix: "bench-owner",
		FamilyID: familyID, IsPrimaryBase: true, State: "ready",
	}); err != nil {
		b.Fatal(err)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		graph, found, err := catalog.GetDedicatedGraphByOwner(ctx, checkoutID)
		if err != nil || !found || graph.GraphID != graphID {
			b.Fatalf("GetDedicatedGraphByOwner = %+v, found=%v, err=%v", graph, found, err)
		}
	}
}

func BenchmarkListIntentTransitions256(b *testing.B) {
	store, err := Open(filepath.Join(b.TempDir(), "transitions.sqlite"))
	if err != nil {
		b.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	catalog := store.Catalog()
	if err := catalog.UpsertRepositoryFamily(ctx, RepositoryFamily{
		FamilyID: "bench-family", CommonDirIdentity: "bench-family", State: "family_ready",
		CreatedAt: 1, LastSeen: 1,
	}); err != nil {
		b.Fatal(err)
	}
	for i := 0; i < 256; i++ {
		checkoutID := fmt.Sprintf("bench-wt-%03d", i)
		if err := catalog.UpsertCheckout(ctx, Checkout{
			CheckoutID: checkoutID, Incarnation: "inc-1", FamilyID: "bench-family",
			RootPath: "/tmp/" + checkoutID, GitDir: "/tmp/" + checkoutID + "/.git",
			AdminName: checkoutID, State: CheckoutStateReady,
			DesiredMode: CheckoutModeAutomatic, EffectiveMode: CheckoutModeAutomatic,
			HeadCommit: "c0ffee", HeadTree: "7ee7", LastSeen: 1,
		}); err != nil {
			b.Fatal(err)
		}
		if err := catalog.BeginIntentTransition(ctx, IntentTransition{
			TransitionID: fmt.Sprintf("bench-transition-%03d", i), CheckoutID: checkoutID,
			Cause: "promote_checkout", PriorDesiredMode: CheckoutModeAutomatic,
			PriorEffectiveMode: CheckoutModeAutomatic, RequestedMode: CheckoutModeDedicated,
			PriorCheckoutState: CheckoutStateReady, State: IntentTransitionPending,
			CreatedAt: int64(i + 1), LastProgress: int64(i + 1),
		}); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		transitions, err := catalog.ListIntentTransitions(ctx)
		if err != nil || len(transitions) != 256 {
			b.Fatalf("ListIntentTransitions = %d rows, err=%v", len(transitions), err)
		}
	}
}

// TestCatalogWriteValidationRejectsUnknownVocabulary proves every typed state
// column is checked in Go before it reaches SQL, and that the required
// identifiers cannot be empty.
func TestCatalogWriteValidationRejectsUnknownVocabulary(t *testing.T) {
	store := openCatalogStore(t)
	ctx := context.Background()
	catalog := store.Catalog()
	seedFamilyAndCheckout(t, catalog, "fam", "wt", "inc-1")

	cases := []struct {
		name string
		call func() error
	}{
		{"family without id", func() error {
			return catalog.UpsertRepositoryFamily(ctx, RepositoryFamily{CommonDirIdentity: "x", State: "s"})
		}},
		{"checkout state", func() error {
			return catalog.UpsertCheckout(ctx, Checkout{
				CheckoutID: "x", Incarnation: "i", FamilyID: "fam", State: CheckoutState("nope"),
				DesiredMode: CheckoutModeAutomatic, EffectiveMode: CheckoutModeAutomatic,
			})
		}},
		{"checkout mode", func() error {
			return catalog.UpsertCheckout(ctx, Checkout{
				CheckoutID: "x", Incarnation: "i", FamilyID: "fam", State: CheckoutStateDemoting,
				DesiredMode: CheckoutMode("hybrid"), EffectiveMode: CheckoutModeAutomatic,
			})
		}},
		{"intent source kind", func() error {
			return catalog.UpsertTrackingIntent(ctx, TrackingIntent{
				IntentID: "i", CheckoutID: "wt", SourceKind: IntentSourceKind("telepathy"), SourceLocator: "l",
			})
		}},
		{"transition state", func() error {
			return catalog.BeginIntentTransition(ctx, IntentTransition{
				TransitionID: "t", CheckoutID: "wt", Cause: "c", State: IntentTransitionState("done"),
			})
		}},
		{"transition prior mode", func() error {
			return catalog.BeginIntentTransition(ctx, IntentTransition{
				TransitionID: "t", CheckoutID: "wt", Cause: "c", State: IntentTransitionPending,
				PriorDesiredMode: CheckoutMode("hybrid"),
			})
		}},
		{"generation state", func() error {
			_, err := catalog.CreateViewGeneration(ctx, ViewGeneration{
				OwnerKind: "o", GenerationKind: "k", State: ViewGenerationState("published"),
			})
			return err
		}},
		{"route state", func() error {
			return catalog.UpsertCheckoutRoute(ctx, CheckoutRoute{
				CheckoutID: "wt", GraphID: "g", State: RouteState("live"),
			})
		}},
		{"ref view state", func() error {
			return catalog.UpsertRefView(ctx, RefView{
				RefViewID: "rv", GraphID: "g", SelectorKind: "branch", SelectorValue: "main",
				EnrichmentProfile: "default", State: RefViewState("warm"),
			})
		}},
		{"build state", func() error {
			return catalog.UpsertRefViewBuild(ctx, RefViewBuild{
				BuildID: "b", RefViewID: "rv", BuildFingerprint: "fp", BuildToken: "tok",
				State: ViewGenerationState("queued"),
			})
		}},
		{"cleanup phase", func() error {
			return catalog.UpsertCleanupEntry(ctx, CleanupEntry{
				CleanupID: "c", OpaqueTargetIDs: "t", Reason: "r", Phase: CleanupPhase("soon"),
			})
		}},
		{"layer without kind", func() error {
			return catalog.UpsertViewLayer(ctx, ViewLayer{LayerID: "l", GraphID: "g"})
		}},
		{"dedicated graph without prefix", func() error {
			return catalog.UpsertDedicatedGraph(ctx, DedicatedGraph{
				GraphID: "g", FamilyID: "fam", State: "graph_ready",
			})
		}},
		{"publish of a non-generation", func() error {
			return catalog.PublishViewGeneration(ctx, 0, 1)
		}},
		{"delete of a non-generation", func() error {
			return catalog.DeleteViewGeneration(ctx, -1)
		}},
	}
	for _, tc := range cases {
		if err := tc.call(); !errors.Is(err, ErrCatalogInvalidValue) {
			t.Errorf("%s: err = %v, want ErrCatalogInvalidValue", tc.name, err)
		}
	}

	// Nothing above should have written a row.
	for _, table := range []string{"checkouts", "tracking_intents", "intent_transitions",
		"view_generations", "checkout_routes", "ref_views", "ref_view_builds",
		"cleanup_journal", "view_layers", "dedicated_graphs"} {
		var count int
		if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+table).Scan(&count); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		want := 0
		if table == "checkouts" {
			want = 1 // the seeded checkout
		}
		if count != want {
			t.Errorf("%s holds %d rows after rejected writes, want %d", table, count, want)
		}
	}

	// The valid vocabulary values all round-trip, so the validators are not
	// simply refusing everything.
	for _, state := range []CheckoutState{
		CheckoutStateReady, CheckoutStateAvailabilityGrace, CheckoutStateUnavailable,
		CheckoutStateReconciling, CheckoutStateDemoting, CheckoutStateForgetting,
		CheckoutStatePrimaryClosureRetiring,
	} {
		if err := catalog.UpdateCheckoutState(ctx, UpdateCheckoutStateRequest{
			CheckoutID: "wt", Incarnation: "inc-1", State: state,
			DesiredMode: CheckoutModeDedicated, EffectiveMode: CheckoutModeAutomatic,
			LastSeen: 1200,
		}); err != nil {
			t.Errorf("state %q rejected: %v", state, err)
		}
	}
	for _, phase := range []CleanupPhase{
		CleanupPhasePending, CleanupPhaseGrace, CleanupPhaseDeleting,
		CleanupPhaseDone, CleanupPhaseFailed,
	} {
		if err := catalog.UpsertCleanupEntry(ctx, CleanupEntry{
			CleanupID: "c-" + string(phase), OpaqueTargetIDs: "t", Reason: "r", Phase: phase,
		}); err != nil {
			t.Errorf("phase %q rejected: %v", phase, err)
		}
	}
	for _, state := range []RefViewState{
		RefViewPending, RefViewBuilding, RefViewReady, RefViewStale, RefViewFailed,
	} {
		if err := catalog.UpsertRefView(ctx, RefView{
			RefViewID: "rv-" + string(state), GraphID: "g", SelectorKind: "branch",
			SelectorValue: string(state), EnrichmentProfile: "default", State: state,
		}); err != nil {
			t.Errorf("ref view state %q rejected: %v", state, err)
		}
	}
	for _, state := range []RouteState{RoutePending, RouteActive, RouteRetired} {
		if err := catalog.UpsertCheckoutRoute(ctx, CheckoutRoute{
			CheckoutID: "wt", GraphID: "g", State: state,
		}); err != nil {
			t.Errorf("route state %q rejected: %v", state, err)
		}
	}
	for _, state := range []ViewGenerationState{
		ViewGenerationBuilding, ViewGenerationReady, ViewGenerationSuperseded,
		ViewGenerationRetiring, ViewGenerationFailed,
	} {
		if _, err := catalog.CreateViewGeneration(ctx, ViewGeneration{
			OwnerKind: "dedicated_graph", GraphID: "g", GenerationKind: "commit", State: state,
		}); err != nil {
			t.Errorf("generation state %q rejected: %v", state, err)
		}
	}
	for _, kind := range []IntentSourceKind{
		IntentSourceCLITrack, IntentSourceMCPTrack, IntentSourceManualConfig,
		IntentSourceProjectMembership,
	} {
		if err := catalog.UpsertTrackingIntent(ctx, TrackingIntent{
			IntentID: "i-" + string(kind), CheckoutID: "wt", SourceKind: kind,
			SourceLocator: string(kind), Active: true,
		}); err != nil {
			t.Errorf("intent source %q rejected: %v", kind, err)
		}
	}
	for _, state := range []IntentTransitionState{
		IntentTransitionPending, IntentTransitionRunning, IntentTransitionFailed,
	} {
		transition := IntentTransition{
			TransitionID: "t-" + string(state), CheckoutID: "wt", Cause: "c", State: state,
		}
		if err := catalog.BeginIntentTransition(ctx, transition); err != nil {
			t.Errorf("transition state %q rejected: %v", state, err)
			continue
		}
		if err := catalog.CompleteIntentTransition(ctx, "wt", transition.TransitionID); err != nil {
			t.Errorf("completing %q: %v", state, err)
		}
	}
}

// TestCatalogObservationWriteMovesBothClocks covers the guarded write a
// reconciliation pass makes. The two clock axes are the point: they are stored
// columns, so they have to survive a restart, and they have to move
// independently of one another.
func TestCatalogObservationWriteMovesBothClocks(t *testing.T) {
	store := openCatalogStore(t)
	ctx := context.Background()
	catalog := store.Catalog()
	seedFamilyAndCheckout(t, catalog, "fam", "wt", "inc-1")

	req := UpdateCheckoutObservationRequest{
		CheckoutID:           "wt",
		Incarnation:          "inc-1",
		ExpectedRootPath:     "/tmp/wt",
		State:                CheckoutStateAvailabilityGrace,
		RootPath:             "/moved/wt",
		GitDir:               "/moved/wt/.git",
		Locked:               true,
		Prunable:             true,
		HeadRef:              "refs/heads/feature",
		HeadCommit:           "beef",
		HeadTree:             "cafe",
		LastAccessible:       10,
		UnavailableSince:     20,
		AvailabilityDeadline: 30,
		RemovalDetectedAt:    40,
		RemovalDeadline:      50,
		RemovalEvidence:      "evidence_prunable_confirmed",
		LastSeen:             60,
		LastError:            "volume detached",
	}
	if err := catalog.UpdateCheckoutObservation(ctx, req); err != nil {
		t.Fatalf("UpdateCheckoutObservation: %v", err)
	}
	checkout, ok, err := catalog.GetCheckout(ctx, "wt")
	if err != nil || !ok {
		t.Fatalf("GetCheckout = %v %v", ok, err)
	}
	if checkout.State != CheckoutStateAvailabilityGrace || checkout.RootPath != "/moved/wt" {
		t.Fatalf("state columns = %+v", checkout)
	}
	if !checkout.Locked || !checkout.Prunable || checkout.HeadTree != "cafe" {
		t.Fatalf("observed facts = %+v", checkout)
	}
	if checkout.UnavailableSince != 20 || checkout.AvailabilityDeadline != 30 {
		t.Fatalf("availability clock = (%d, %d)", checkout.UnavailableSince, checkout.AvailabilityDeadline)
	}
	if checkout.RemovalDetectedAt != 40 || checkout.RemovalDeadline != 50 {
		t.Fatalf("removal clock = (%d, %d)", checkout.RemovalDetectedAt, checkout.RemovalDeadline)
	}
	if checkout.RemovalEvidence != "evidence_prunable_confirmed" || checkout.LastError != "volume detached" {
		t.Fatalf("evidence columns = %+v", checkout)
	}
	// The identity columns are not the observation's to change.
	if checkout.Incarnation != "inc-1" || checkout.AdminName != "wt" || checkout.FamilyID != "fam" {
		t.Fatalf("an observation re-keyed the row: %+v", checkout)
	}

	stale := req
	stale.Incarnation = "inc-0"
	stale.State = CheckoutStateUnavailable
	if err := catalog.UpdateCheckoutObservation(ctx, stale); !errors.Is(err, ErrCatalogStaleGuard) {
		t.Fatalf("stale observation = %v, want ErrCatalogStaleGuard", err)
	}
	if again, _, _ := catalog.GetCheckout(ctx, "wt"); again.State != CheckoutStateAvailabilityGrace {
		t.Fatalf("a stale observation advanced the state to %q", again.State)
	}

	for name, bad := range map[string]UpdateCheckoutObservationRequest{
		"no checkout id": {Incarnation: "inc-1", State: CheckoutStateReady},
		"no incarnation": {CheckoutID: "wt", State: CheckoutStateReady},
		"unknown state": {CheckoutID: "wt", Incarnation: "inc-1", ExpectedRootPath: "/tmp/wt",
			RootPath: "/tmp/wt", State: "invented"},
	} {
		if err := catalog.UpdateCheckoutObservation(ctx, bad); !errors.Is(err, ErrCatalogInvalidValue) {
			t.Errorf("%s = %v, want ErrCatalogInvalidValue", name, err)
		}
	}
}

// TestCatalogObservationLeavesTheModeAxisAlone pins the column split between
// the two writers on a checkout row. A mode transition that commits between an
// observation's read and its write must survive: the incarnation guard does
// not move on a promotion, so the observation write is only safe as long as it
// names no mode column.
func TestCatalogObservationLeavesTheModeAxisAlone(t *testing.T) {
	store := openCatalogStore(t)
	ctx := context.Background()
	catalog := store.Catalog()
	seedFamilyAndCheckout(t, catalog, "fam", "wt", "inc-1")

	// What a pass reads at its start: the checkout is served automatically.
	observed, ok, err := catalog.GetCheckout(ctx, "wt")
	if err != nil || !ok {
		t.Fatalf("GetCheckout = %v %v", ok, err)
	}
	if observed.EffectiveMode != CheckoutModeAutomatic {
		t.Fatalf("seeded mode = %q", observed.EffectiveMode)
	}

	// A promotion commits while the pass is still deciding.
	err = catalog.UpdateCheckoutState(ctx, UpdateCheckoutStateRequest{
		CheckoutID:    "wt",
		Incarnation:   "inc-1",
		State:         CheckoutStateReconciling,
		DesiredMode:   CheckoutModeDedicated,
		EffectiveMode: CheckoutModeDedicated,
	})
	if err != nil {
		t.Fatalf("UpdateCheckoutState: %v", err)
	}

	// The pass writes what it observed, under an incarnation that is still
	// current, so the write lands.
	err = catalog.UpdateCheckoutObservation(ctx, UpdateCheckoutObservationRequest{
		CheckoutID:       "wt",
		Incarnation:      "inc-1",
		ExpectedRootPath: observed.RootPath,
		State:            CheckoutStateReady,
		RootPath:         observed.RootPath,
		GitDir:           observed.GitDir,
		LastAccessible:   77,
		LastSeen:         77,
	})
	if err != nil {
		t.Fatalf("UpdateCheckoutObservation: %v", err)
	}

	after, _, err := catalog.GetCheckout(ctx, "wt")
	if err != nil {
		t.Fatalf("GetCheckout: %v", err)
	}
	if after.DesiredMode != CheckoutModeDedicated || after.EffectiveMode != CheckoutModeDedicated {
		t.Fatalf("an observation reverted the promotion: %q/%q", after.DesiredMode, after.EffectiveMode)
	}
	if after.State != CheckoutStateReady || after.LastAccessible != 77 {
		t.Fatalf("the observation itself did not land: %+v", after)
	}
}

// TestCatalogAllocateCheckoutRefusesASecondIdentity covers the allocation-time
// backstop. The table's UNIQUE key spans the incarnation, so it happily takes
// a second live row for one (family_id, admin_name); the guarded insert is
// what does not, and that is the only thing standing between two racing
// allocators and a family with two identities for one working copy.
func TestCatalogAllocateCheckoutRefusesASecondIdentity(t *testing.T) {
	store := openCatalogStore(t)
	ctx := context.Background()
	catalog := store.Catalog()
	seedFamilyAndCheckout(t, catalog, "fam", "wt", "inc-1")

	rival := Checkout{
		CheckoutID:    "wt-2",
		Incarnation:   "inc-2",
		FamilyID:      "fam",
		RootPath:      "/tmp/wt",
		GitDir:        "/tmp/wt/.git",
		AdminName:     "wt",
		State:         CheckoutStateReady,
		DesiredMode:   CheckoutModeAutomatic,
		EffectiveMode: CheckoutModeAutomatic,
	}
	if err := catalog.AllocateCheckout(ctx, rival); !errors.Is(err, ErrCatalogStaleGuard) {
		t.Fatalf("second allocation = %v, want ErrCatalogStaleGuard", err)
	}
	rows, err := catalog.ListCheckouts(ctx, "fam")
	if err != nil {
		t.Fatalf("ListCheckouts: %v", err)
	}
	if len(rows) != 1 || rows[0].CheckoutID != "wt" {
		t.Fatalf("family holds %+v, want only the first identity", rows)
	}

	// Another administrative name is another working copy, and the same name
	// in another family is not taken here.
	sibling := rival
	sibling.CheckoutID, sibling.AdminName = "wt-sibling", "sibling"
	if err := catalog.AllocateCheckout(ctx, sibling); err != nil {
		t.Fatalf("allocating a second admin name = %v", err)
	}
	seedFamilyAndCheckout(t, catalog, "other", "other-wt", "inc-1")
	elsewhere := rival
	elsewhere.CheckoutID, elsewhere.FamilyID = "wt-elsewhere", "other"
	if err := catalog.AllocateCheckout(ctx, elsewhere); err != nil {
		t.Fatalf("allocating the same name in another family = %v", err)
	}

	// A name freed by a teardown may be allocated again — that is a path that
	// was removed and recreated.
	if err := catalog.DeleteCheckout(ctx, "wt"); err != nil {
		t.Fatalf("DeleteCheckout: %v", err)
	}
	if err := catalog.AllocateCheckout(ctx, rival); err != nil {
		t.Fatalf("re-allocating a freed name = %v", err)
	}

	nameless := rival
	nameless.CheckoutID, nameless.AdminName = "wt-nameless", ""
	if err := catalog.AllocateCheckout(ctx, nameless); !errors.Is(err, ErrCatalogInvalidValue) {
		t.Fatalf("allocation without an admin name = %v, want ErrCatalogInvalidValue", err)
	}
	invalid := rival
	invalid.CheckoutID, invalid.State = "wt-invalid", "invented"
	if err := catalog.AllocateCheckout(ctx, invalid); !errors.Is(err, ErrCatalogInvalidValue) {
		t.Fatalf("allocation with an unknown state = %v, want ErrCatalogInvalidValue", err)
	}
}

// TestCatalogListsAreScopedAndOrdered covers the three listings a lifecycle
// caller needs to find rows it does not already know the ids of.
func TestCatalogListsAreScopedAndOrdered(t *testing.T) {
	store := openCatalogStore(t)
	ctx := context.Background()
	catalog := store.Catalog()
	seedFamilyAndCheckout(t, catalog, "fam", "wt", "inc-1")
	seedFamilyAndCheckout(t, catalog, "other", "other-wt", "inc-1")

	for _, dedicated := range []DedicatedGraph{
		{GraphID: "g-b", RepoPrefix: "p-b", FamilyID: "fam", State: "graph_ready"},
		{GraphID: "g-a", RepoPrefix: "p-a", FamilyID: "fam", IsPrimaryBase: true,
			OwnerCheckoutID: "wt", ActiveGenerationID: 0, State: "graph_ready"},
		{GraphID: "g-z", RepoPrefix: "p-z", FamilyID: "other", State: "graph_ready"},
	} {
		if err := catalog.UpsertDedicatedGraph(ctx, dedicated); err != nil {
			t.Fatalf("UpsertDedicatedGraph %s: %v", dedicated.GraphID, err)
		}
	}
	graphs, err := catalog.ListDedicatedGraphs(ctx, "fam")
	if err != nil {
		t.Fatalf("ListDedicatedGraphs: %v", err)
	}
	if len(graphs) != 2 || graphs[0].GraphID != "g-a" || graphs[1].GraphID != "g-b" {
		t.Fatalf("ListDedicatedGraphs = %+v", graphs)
	}
	if !graphs[0].IsPrimaryBase || graphs[0].OwnerCheckoutID != "wt" || graphs[0].FamilyID != "fam" {
		t.Fatalf("primary row round trip = %+v", graphs[0])
	}
	if graphs[1].IsPrimaryBase || graphs[1].OwnerCheckoutID != "" {
		t.Fatalf("an unowned non-primary row read back as %+v", graphs[1])
	}
	if empty, err := catalog.ListDedicatedGraphs(ctx, "no-such-family"); err != nil || len(empty) != 0 {
		t.Fatalf("ListDedicatedGraphs on an unknown family = %v, %v", empty, err)
	}

	for _, view := range []RefView{
		{RefViewID: "v-b", GraphID: "g-a", SelectorKind: "branch", SelectorValue: "b",
			EnrichmentProfile: "default", State: RefViewReady, ExactView: true, ActiveRef: "refs/heads/b"},
		{RefViewID: "v-a", GraphID: "g-a", SelectorKind: "branch", SelectorValue: "a",
			EnrichmentProfile: "default", State: RefViewPending},
		{RefViewID: "v-other", GraphID: "g-b", SelectorKind: "branch", SelectorValue: "a",
			EnrichmentProfile: "default", State: RefViewReady},
	} {
		if err := catalog.UpsertRefView(ctx, view); err != nil {
			t.Fatalf("UpsertRefView %s: %v", view.RefViewID, err)
		}
	}
	views, err := catalog.ListRefViews(ctx, "g-a")
	if err != nil {
		t.Fatalf("ListRefViews: %v", err)
	}
	if len(views) != 2 || views[0].RefViewID != "v-a" || views[1].RefViewID != "v-b" {
		t.Fatalf("ListRefViews = %+v", views)
	}
	if views[1].ActiveRef != "refs/heads/b" || !views[1].ExactView || views[1].State != RefViewReady {
		t.Fatalf("ref view round trip through the listing = %+v", views[1])
	}
	// The listing and the single read must agree column for column.
	single, ok, err := catalog.GetRefView(ctx, "v-b")
	if err != nil || !ok {
		t.Fatalf("GetRefView = %v %v", ok, err)
	}
	if single != views[1] {
		t.Fatalf("GetRefView = %+v, listing = %+v", single, views[1])
	}

	for _, entry := range []CleanupEntry{
		{CleanupID: "c-b", OpaqueTargetIDs: "b", Reason: "forget_checkout", Phase: CleanupPhaseDeleting, PrimaryEpoch: 3},
		{CleanupID: "c-a", OpaqueTargetIDs: "a", Reason: "purge_checkout_layers", Phase: CleanupPhaseDone},
	} {
		if err := catalog.UpsertCleanupEntry(ctx, entry); err != nil {
			t.Fatalf("UpsertCleanupEntry %s: %v", entry.CleanupID, err)
		}
	}
	entries, err := catalog.ListCleanupEntries(ctx)
	if err != nil {
		t.Fatalf("ListCleanupEntries: %v", err)
	}
	if len(entries) != 2 || entries[0].CleanupID != "c-a" || entries[1].CleanupID != "c-b" {
		t.Fatalf("ListCleanupEntries = %+v", entries)
	}
	if entries[0].Phase != CleanupPhaseDone || entries[1].PrimaryEpoch != 3 {
		t.Fatalf("cleanup round trip = %+v", entries)
	}
}

// TestCatalogDeletesAreAddressedAndGuarded covers the row-at-a-time deletes a
// cleanup saga walks through, including the refusal that keeps a family from
// being deleted out from under its checkouts.
func TestCatalogDeletesAreAddressedAndGuarded(t *testing.T) {
	store := openCatalogStore(t)
	ctx := context.Background()
	catalog := store.Catalog()
	seedFamilyAndCheckout(t, catalog, "fam", "wt", "inc-1")

	if err := catalog.UpsertDedicatedGraph(ctx, DedicatedGraph{
		GraphID: "g-a", RepoPrefix: "p-a", FamilyID: "fam", State: "graph_ready",
	}); err != nil {
		t.Fatalf("UpsertDedicatedGraph: %v", err)
	}
	if err := catalog.UpsertRefView(ctx, RefView{
		RefViewID: "v-a", GraphID: "g-a", SelectorKind: "branch", SelectorValue: "a",
		EnrichmentProfile: "default", State: RefViewReady,
	}); err != nil {
		t.Fatalf("UpsertRefView: %v", err)
	}
	if err := catalog.UpsertCheckoutRoute(ctx, CheckoutRoute{
		CheckoutID: "wt", GraphID: "g-a", State: RouteActive,
	}); err != nil {
		t.Fatalf("UpsertCheckoutRoute: %v", err)
	}
	if err := catalog.UpsertCleanupEntry(ctx, CleanupEntry{
		CleanupID: "c-a", OpaqueTargetIDs: "a", Reason: "forget_checkout", Phase: CleanupPhasePending,
	}); err != nil {
		t.Fatalf("UpsertCleanupEntry: %v", err)
	}

	// A family cannot go while its checkouts and graphs still reference it.
	if err := catalog.DeleteRepositoryFamily(ctx, "fam"); err == nil {
		t.Fatal("deleting a populated family succeeded")
	}
	if _, ok, _ := catalog.GetRepositoryFamily(ctx, "fam"); !ok {
		t.Fatal("the refused delete removed the family anyway")
	}

	// A route blocks its checkout until it is withdrawn.
	if err := catalog.DeleteCheckout(ctx, "wt"); err == nil {
		t.Fatal("deleting a routed checkout succeeded")
	}

	for _, step := range []struct {
		name   string
		delete func() error
		gone   func() bool
	}{
		{
			name:   "route",
			delete: func() error { return catalog.DeleteCheckoutRoute(ctx, "wt") },
			gone:   func() bool { _, ok, _ := catalog.GetCheckoutRoute(ctx, "wt"); return !ok },
		},
		{
			name:   "ref view",
			delete: func() error { return catalog.DeleteRefView(ctx, "v-a") },
			gone:   func() bool { _, ok, _ := catalog.GetRefView(ctx, "v-a"); return !ok },
		},
		{
			name:   "dedicated graph",
			delete: func() error { return catalog.DeleteDedicatedGraph(ctx, "g-a") },
			gone:   func() bool { _, ok, _ := catalog.GetDedicatedGraph(ctx, "g-a"); return !ok },
		},
		{
			name:   "cleanup entry",
			delete: func() error { return catalog.DeleteCleanupEntry(ctx, "c-a") },
			gone:   func() bool { _, ok, _ := catalog.GetCleanupEntry(ctx, "c-a"); return !ok },
		},
		{
			name:   "checkout",
			delete: func() error { return catalog.DeleteCheckout(ctx, "wt") },
			gone:   func() bool { _, ok, _ := catalog.GetCheckout(ctx, "wt"); return !ok },
		},
		{
			name:   "family",
			delete: func() error { return catalog.DeleteRepositoryFamily(ctx, "fam") },
			gone:   func() bool { _, ok, _ := catalog.GetRepositoryFamily(ctx, "fam"); return !ok },
		},
	} {
		if err := step.delete(); err != nil {
			t.Fatalf("delete %s: %v", step.name, err)
		}
		if !step.gone() {
			t.Fatalf("%s survived its delete", step.name)
		}
		// A second delete reports the row as missing rather than succeeding
		// silently, so a caller can tell the two apart.
		if err := step.delete(); !errors.Is(err, ErrCatalogNotFound) {
			t.Errorf("second delete of %s = %v, want ErrCatalogNotFound", step.name, err)
		}
	}

	for name, delete := range map[string]func() error{
		"route":           func() error { return catalog.DeleteCheckoutRoute(ctx, "") },
		"ref view":        func() error { return catalog.DeleteRefView(ctx, "") },
		"dedicated graph": func() error { return catalog.DeleteDedicatedGraph(ctx, "") },
		"cleanup entry":   func() error { return catalog.DeleteCleanupEntry(ctx, "") },
		"family":          func() error { return catalog.DeleteRepositoryFamily(ctx, "") },
	} {
		if err := delete(); !errors.Is(err, ErrCatalogInvalidValue) {
			t.Errorf("empty %s id = %v, want ErrCatalogInvalidValue", name, err)
		}
	}
}

// TestCatalogListViewGenerationsFilters covers the enumeration retirement
// recovers its work list from: each filter axis on its own, the axes combined,
// the ordering, and the empty answers.
func TestCatalogListViewGenerationsFilters(t *testing.T) {
	store := openCatalogStore(t)
	ctx := context.Background()
	catalog := store.Catalog()

	// Seeded oldest first, so the expected orderings below are these layer
	// ids reversed: generation ids ascend and the listing is newest first.
	for _, generation := range []ViewGeneration{
		{LayerID: "a-ready", OwnerKind: "dedicated_graph", GraphID: "graph-a", CheckoutID: "wt-a",
			GenerationKind: "commit", State: ViewGenerationReady, CreatedAt: 10},
		{LayerID: "a-super", OwnerKind: "dedicated_graph", GraphID: "graph-a", CheckoutID: "wt-a",
			GenerationKind: "dirty", State: ViewGenerationSuperseded, CreatedAt: 11},
		{LayerID: "a-retiring", OwnerKind: "dedicated_graph", GraphID: "graph-a", CheckoutID: "wt-b",
			GenerationKind: "commit", State: ViewGenerationRetiring, CreatedAt: 12},
		{LayerID: "b-ready", OwnerKind: "ref_view", GraphID: "graph-b",
			GenerationKind: "commit", State: ViewGenerationReady, CreatedAt: 13},
		{LayerID: "b-building", OwnerKind: "dedicated_graph", GraphID: "graph-b", CheckoutID: "wt-a",
			GenerationKind: "dirty", State: ViewGenerationBuilding, CreatedAt: 14},
	} {
		if _, err := catalog.CreateViewGeneration(ctx, generation); err != nil {
			t.Fatalf("CreateViewGeneration %s: %v", generation.LayerID, err)
		}
	}

	listed := func(filter ViewGenerationFilter) []string {
		t.Helper()
		rows, err := catalog.ListViewGenerations(ctx, filter)
		if err != nil {
			t.Fatalf("ListViewGenerations %+v: %v", filter, err)
		}
		out := make([]string, 0, len(rows))
		for _, row := range rows {
			out = append(out, row.LayerID)
		}
		return out
	}

	for _, tc := range []struct {
		name   string
		filter ViewGenerationFilter
		want   []string
	}{
		{"unfiltered is newest first", ViewGenerationFilter{},
			[]string{"b-building", "b-ready", "a-retiring", "a-super", "a-ready"}},
		{"the states retirement asks for", ViewGenerationFilter{
			States: []ViewGenerationState{ViewGenerationSuperseded, ViewGenerationRetiring}},
			[]string{"a-retiring", "a-super"}},
		{"one graph", ViewGenerationFilter{GraphID: "graph-a"},
			[]string{"a-retiring", "a-super", "a-ready"}},
		{"one owner kind", ViewGenerationFilter{OwnerKind: "ref_view"},
			[]string{"b-ready"}},
		// wt-a spans both graphs, and the generation that names no checkout at
		// all is stored as NULL rather than as the empty string — it must not
		// answer a filter for a checkout.
		{"one checkout", ViewGenerationFilter{CheckoutID: "wt-a"},
			[]string{"b-building", "a-super", "a-ready"}},
		{"every axis at once", ViewGenerationFilter{
			States:     []ViewGenerationState{ViewGenerationReady},
			GraphID:    "graph-a",
			OwnerKind:  "dedicated_graph",
			CheckoutID: "wt-a",
		}, []string{"a-ready"}},
		{"the limit takes the newest", ViewGenerationFilter{Limit: 2},
			[]string{"b-building", "b-ready"}},
		{"an unknown graph", ViewGenerationFilter{GraphID: "graph-nowhere"}, nil},
		{"an unknown checkout", ViewGenerationFilter{CheckoutID: "wt-nowhere"}, nil},
		{"a state nothing is in", ViewGenerationFilter{
			States: []ViewGenerationState{ViewGenerationFailed}}, nil},
		{"a combination no row satisfies", ViewGenerationFilter{
			GraphID: "graph-b", CheckoutID: "wt-b"}, nil},
	} {
		if got := listed(tc.filter); !slices.Equal(got, tc.want) {
			t.Errorf("%s = %v, want %v", tc.name, got, tc.want)
		}
	}

	// The listing and the single read must agree column for column, the
	// nullable ones included.
	rows, err := catalog.ListViewGenerations(ctx, ViewGenerationFilter{OwnerKind: "ref_view"})
	if err != nil || len(rows) != 1 {
		t.Fatalf("ListViewGenerations = %+v, %v", rows, err)
	}
	if rows[0].CheckoutID != "" || rows[0].BaseGenerationID != 0 {
		t.Fatalf("an unset checkout read back as %+v", rows[0])
	}
	single, ok, err := catalog.GetViewGeneration(ctx, rows[0].GenerationID)
	if err != nil || !ok {
		t.Fatalf("GetViewGeneration = %v %v", ok, err)
	}
	if single != rows[0] {
		t.Fatalf("GetViewGeneration = %+v, listing = %+v", single, rows[0])
	}

	if _, err := catalog.ListViewGenerations(ctx, ViewGenerationFilter{
		States: []ViewGenerationState{"reticent"},
	}); !errors.Is(err, ErrCatalogInvalidValue) {
		t.Errorf("an unknown state = %v, want ErrCatalogInvalidValue", err)
	}
	if _, err := catalog.ListViewGenerations(ctx, ViewGenerationFilter{Limit: -1}); !errors.Is(err, ErrCatalogInvalidValue) {
		t.Errorf("a negative limit = %v, want ErrCatalogInvalidValue", err)
	}
}

// TestCatalogListViewGenerationsCapsOneScan pins the bound. A janitor pass
// reads this listing every time it runs, so an unset — or an over-ambitious —
// limit must still cost one bounded read.
func TestCatalogListViewGenerationsCapsOneScan(t *testing.T) {
	store := openCatalogStore(t)
	ctx := context.Background()
	catalog := store.Catalog()

	var newest int64
	for i := 0; i < maxViewGenerationListing+3; i++ {
		id, err := catalog.CreateViewGeneration(ctx, ViewGeneration{
			OwnerKind: "dedicated_graph", GraphID: "graph-cap", GenerationKind: "commit",
			State: ViewGenerationReady, CreatedAt: int64(i),
		})
		if err != nil {
			t.Fatalf("CreateViewGeneration %d: %v", i, err)
		}
		newest = id
	}

	for _, limit := range []int{0, maxViewGenerationListing, maxViewGenerationListing + 100} {
		rows, err := catalog.ListViewGenerations(ctx, ViewGenerationFilter{Limit: limit})
		if err != nil {
			t.Fatalf("ListViewGenerations limit %d: %v", limit, err)
		}
		if len(rows) != maxViewGenerationListing {
			t.Fatalf("limit %d returned %d rows, want the cap %d", limit, len(rows), maxViewGenerationListing)
		}
		// The cap must take the newest page rather than an arbitrary one: a
		// layer has to be offered before the generation it sits on, and
		// descending id is that order.
		if rows[0].GenerationID != newest {
			t.Fatalf("limit %d starts at generation %d, want the newest %d", limit, rows[0].GenerationID, newest)
		}
	}

	under, err := catalog.ListViewGenerations(ctx, ViewGenerationFilter{Limit: 7})
	if err != nil || len(under) != 7 {
		t.Fatalf("ListViewGenerations limit 7 = %d rows, %v", len(under), err)
	}
}

// TestCatalogWithdrawProducer pins the withdrawal write: one producer of a
// PUBLISHED generation moves to unavailable, its neighbours do not, and the
// payload seal that refuses every payload write to such a generation does not
// stand in the way — the row is control plane, not payload.
func TestCatalogWithdrawProducer(t *testing.T) {
	store := openCatalogStore(t)
	ctx := context.Background()
	catalog := store.Catalog()

	generationID, handle, err := store.BeginPayloadGeneration(ctx, PayloadGenerationRequest{
		OwnerKind: "ref_view", GraphID: "graph-withdraw", LayerID: "layer-withdraw",
		GenerationKind: "commit", TreeOID: "tree-withdraw", CreatedAt: 10,
	})
	if err != nil {
		t.Fatalf("BeginPayloadGeneration: %v", err)
	}
	for _, row := range []ProducerCompleteness{
		{Producer: "source.snapshot", State: ProducerStateComplete},
		{Producer: "graph.syntax", State: ProducerStateComplete},
	} {
		if err := handle.SetProducerState(row); err != nil {
			t.Fatalf("SetProducerState %s: %v", row.Producer, err)
		}
	}
	if err := store.PublishPayloadGeneration(ctx, generationID, 20); err != nil {
		t.Fatalf("PublishPayloadGeneration: %v", err)
	}

	// The payload write path is sealed on a published generation, which is
	// exactly why the withdrawal is a catalog write.
	if err := handle.SetProducerState(ProducerCompleteness{
		Producer: "source.snapshot", State: ProducerStateUnavailable,
	}); !errors.Is(err, ErrPayloadGenerationSealed) {
		t.Fatalf("SetProducerState on a published generation = %v, want it sealed", err)
	}

	if err := catalog.WithdrawProducer(ctx, generationID, "source.snapshot", "the blobs are gone"); err != nil {
		t.Fatalf("WithdrawProducer: %v", err)
	}
	states := map[string]ProducerState{}
	rows, err := handle.ProducerStates()
	if err != nil {
		t.Fatalf("ProducerStates: %v", err)
	}
	for _, row := range rows {
		states[row.Producer] = row.State
	}
	if states["source.snapshot"] != ProducerStateUnavailable {
		t.Errorf("source.snapshot = %q, want %q", states["source.snapshot"], ProducerStateUnavailable)
	}
	if states["graph.syntax"] != ProducerStateComplete {
		t.Errorf("the withdrawal disturbed graph.syntax: %q", states["graph.syntax"])
	}

	// A second withdrawal changes nothing and says so, so a caller cannot read
	// it as a fresh loss.
	if err := catalog.WithdrawProducer(ctx, generationID, "source.snapshot", "again"); !errors.Is(err, ErrCatalogStaleGuard) {
		t.Errorf("a repeat withdrawal = %v, want %v", err, ErrCatalogStaleGuard)
	}
	// A producer the generation never declared is the same no-op.
	if err := catalog.WithdrawProducer(ctx, generationID, "search.vector", "never declared"); !errors.Is(err, ErrCatalogStaleGuard) {
		t.Errorf("withdrawing an undeclared producer = %v, want %v", err, ErrCatalogStaleGuard)
	}
	if err := catalog.WithdrawProducer(ctx, 0, "source.snapshot", ""); !errors.Is(err, ErrCatalogInvalidValue) {
		t.Errorf("withdrawing on generation 0 = %v, want %v", err, ErrCatalogInvalidValue)
	}
}
