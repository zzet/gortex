package indexer

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"testing"

	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// The durable half of commit-layer reuse.
//
// The process-local cache (checkout_coordinator.go retainCommit/cachedCommit)
// is four identities deep and dies with the daemon. Everything outside that
// window re-indexed a tree whose payload was still sitting in the database and
// still servable. These tests drive the production reconcile path and assert
// that the catalog's equal-identity lookup is reached, that it routes rather
// than rebuilds, and that it never routes a payload built for something else.

// TestCoordinatorAdoptsAStoredCommitLayerAfterRestart is the restart case.
//
// A -> B in one process, then the daemon dies and the checkout goes back to A.
// The replacement coordinator's cache is empty and its inherited route names B,
// so nothing in the process knows about A's layer — but the catalog does, and
// the tree has not changed, so the payload is still exactly what a build would
// produce.
func TestCoordinatorAdoptsAStoredCommitLayerAfterRestart(t *testing.T) {
	f := newCoordinatorFixture(t)
	first := f.inertCoordinator(t, CheckoutCoordinatorConfig{})
	commitA := builderGit(t, f.worktree, "rev-parse", "HEAD")

	initial := coordinatorReconcile(t, first)
	if !initial.CommitBuilt || initial.CommitGenerationID == 0 {
		t.Fatalf("the first cycle did not build A: %+v", initial)
	}
	generationA := initial.CommitGenerationID

	f.commitTreeB()
	onB := coordinatorReconcile(t, first)
	if !onB.CommitBuilt || onB.CommitGenerationID == generationA {
		t.Fatalf("the second cycle did not build and route B: %+v", onB)
	}

	// The daemon restarts. The replacement inherits route B with an empty
	// reuse cache, and only then does the checkout go back to A's tree — so the
	// route-preserving arm of reconcileCommitSlot, which retains whatever the
	// route already names, never sees A at all.
	restarted := f.inertCoordinator(t, CheckoutCoordinatorConfig{})
	if len(restarted.retained) != 0 {
		t.Fatalf("a fresh coordinator started with %d cached layers", len(restarted.retained))
	}
	builderGit(t, f.worktree, "checkout", "--detach", commitA)

	back := coordinatorReconcile(t, restarted)
	if back.CommitBuilt {
		t.Fatalf("the post-restart switch back re-indexed A: %+v", back)
	}
	if !back.CommitReused {
		t.Fatalf("the post-restart switch back did not report a reuse: %+v", back)
	}
	if back.CommitGenerationID != generationA {
		t.Fatalf("the switch back routed generation %d, want the stored %d",
			back.CommitGenerationID, generationA)
	}
	if got := f.route().CommitGenerationID; got != generationA {
		t.Fatalf("the route names generation %d, want %d", got, generationA)
	}

	// Two trees visited three times across a restart is two commit layers.
	commits := 0
	for _, row := range f.generations() {
		if row.GenerationKind == CommitLayerGenerationKind {
			commits++
		}
	}
	if commits != 2 {
		t.Fatalf("%d commit generations exist for two trees across a restart", commits)
	}
}

// TestStoredCommitLayerReuseWritesNothing measures the reuse path itself.
//
// Reuse that costs a write is not reuse. resolveCommitLayer is called directly
// so the measurement covers the lookup and the decision without the route flip
// the caller makes afterwards — a flip writes checkout_routes by design, and
// nothing else may move.
//
// This is the non-evicting case: the restarted coordinator's reuse cache is
// empty, so filing the adopted generation gives nothing up. The evicting case
// is TestStoredCommitLayerReuseWritesOnlyTheCacheEviction — the two together
// are what "zero DML beyond the route flip" actually means.
func TestStoredCommitLayerReuseWritesNothing(t *testing.T) {
	f := newCoordinatorFixture(t)
	first := f.inertCoordinator(t, CheckoutCoordinatorConfig{})
	ctx := context.Background()

	initial := coordinatorReconcile(t, first)
	if !initial.CommitBuilt || initial.CommitGenerationID == 0 {
		t.Fatalf("the first cycle did not build A: %+v", initial)
	}
	generationA := initial.CommitGenerationID
	f.commitTreeB()
	if onB := coordinatorReconcile(t, first); !onB.CommitBuilt {
		t.Fatalf("the second cycle did not build B: %+v", onB)
	}

	restarted := f.inertCoordinator(t, CheckoutCoordinatorConfig{})
	base, err := restarted.primaryBase(ctx)
	if err != nil {
		t.Fatalf("primaryBase: %v", err)
	}

	writes := installCheckoutLayerWriteAudit(t, f.storePath)
	generationID, reused, err := restarted.resolveCommitLayer(ctx, base, f.treeA)
	if err != nil {
		t.Fatalf("resolveCommitLayer: %v", err)
	}
	if !reused || generationID != generationA {
		t.Fatalf("resolveCommitLayer = (%d, reused %v), want the stored %d reused",
			generationID, reused, generationA)
	}
	if counted := writes(t); counted != 0 {
		t.Fatalf("the reuse path made %d payload/generation writes, want none", counted)
	}
}

// TestStoredCommitLayerReuseWritesOnlyTheCacheEviction measures the case the
// zero-write audit cannot: a reuse that fills the last slot of the process
// cache and pushes something out of it.
//
// storedCommit files what it adopted through retainCommit, and retainCommit
// offers every evicted generation for retirement — catalog and payload DML when
// nothing refuses it. So "the reuse path writes nothing" is only true while the
// cache has room, and the honest claim is narrower: the ADOPTION writes
// nothing, and every write the call can reach belongs to the generation the
// cache gave up. This test pins exactly that split, on one coordinator, with
// the same audit.
func TestStoredCommitLayerReuseWritesOnlyTheCacheEviction(t *testing.T) {
	f := newCoordinatorFixture(t)
	first := f.inertCoordinator(t, CheckoutCoordinatorConfig{})
	ctx := context.Background()

	initial := coordinatorReconcile(t, first)
	if !initial.CommitBuilt || initial.CommitGenerationID == 0 {
		t.Fatalf("the first cycle did not build A: %+v", initial)
	}
	generationA := initial.CommitGenerationID
	treeB := f.commitTreeB()
	onB := coordinatorReconcile(t, first)
	if !onB.CommitBuilt || onB.CommitGenerationID == generationA {
		t.Fatalf("the second cycle did not build and route B: %+v", onB)
	}
	generationB := onB.CommitGenerationID

	// A cache exactly one identity wide: the second adoption must evict the
	// first. The route names B, so A is what the cache can give up and A is
	// retirable — nothing routes it, leases it, or sits on it.
	// The first coordinator is still holding the working-tree layers it built
	// over A and over B: a layer the route leaves is retained for an undo now
	// rather than collected (dirty_layer_reuse_test.go). A commit generation
	// something sits on cannot be retired, so those layers would hide the
	// eviction this test measures behind a refusal that has nothing to do with
	// it. Hand them over the way teardown does, newest first — a layer is
	// always offered before the generation it sits on.
	// Only the working-tree layers: the commit layers the drain also hands
	// over are the payload this test is about to adopt from the catalog.
	handed := first.DrainRetirements()
	retireNewestFirst(handed)
	for _, generationID := range handed {
		if generationID <= 0 {
			continue
		}
		row, found := f.generation(generationID)
		if !found || row.GenerationKind != DirtyLayerGenerationKind {
			continue
		}
		_ = f.store.RetirePayloadGeneration(ctx, generationID, nil)
	}

	restarted := f.inertCoordinator(t, CheckoutCoordinatorConfig{Retain: 1})
	if len(restarted.retained) != 0 {
		t.Fatalf("a fresh coordinator started with %d cached layers", len(restarted.retained))
	}
	base, err := restarted.primaryBase(ctx)
	if err != nil {
		t.Fatalf("primaryBase: %v", err)
	}

	writes := installCheckoutLayerWriteAudit(t, f.storePath)

	// One: the adoption that fits. Zero writes, as the sibling test says.
	got, reused, err := restarted.resolveCommitLayer(ctx, base, f.treeA)
	if err != nil {
		t.Fatalf("resolveCommitLayer(A): %v", err)
	}
	if !reused || got != generationA {
		t.Fatalf("resolveCommitLayer(A) = (%d, reused %v), want the stored %d reused",
			got, reused, generationA)
	}
	fitted := writes(t)
	if fitted != 0 {
		t.Fatalf("the adoption that fit the cache made %d writes, want none", fitted)
	}

	// Two: the adoption that evicts. B is adopted, A is pushed out and offered.
	got, reused, err = restarted.resolveCommitLayer(ctx, base, treeB)
	if err != nil {
		t.Fatalf("resolveCommitLayer(B): %v", err)
	}
	if !reused || got != generationB {
		t.Fatalf("resolveCommitLayer(B) = (%d, reused %v), want the stored %d reused",
			got, reused, generationB)
	}

	// The adopted generation is untouched: whatever the audit counted, none of
	// it happened to B. A route may name it the moment this returns.
	adopted, found := f.generation(generationB)
	if !found {
		t.Fatalf("the adopted generation %d is gone", generationB)
	}
	if !servableGeneration(adopted.State) || adopted.State != store_sqlite.ViewGenerationReady {
		t.Fatalf("the adoption moved generation %d to %s", generationB, adopted.State)
	}

	// And every write it DID count is the eviction's: A is collected, which is
	// the whole of what retainCommit gave up, and it is the same collection the
	// build path's own retainCommit would have made at the end of this cycle.
	evicted := writes(t) - fitted
	if evicted == 0 {
		t.Fatalf("the evicting adoption made no writes; the eviction it must pay for did not happen")
	}
	if _, stillThere := f.generation(generationA); stillThere {
		t.Fatalf("generation %d was evicted from the cache but not collected", generationA)
	}
	if held := restarted.retirementBacklog(); len(held) != 0 {
		t.Fatalf("the eviction was refused and deferred: backlog %v", held)
	}
}

// TestCoordinatorNeverAdoptsALayerBuiltOverAnotherBase is the negative half.
//
// A commit layer's identity carries the base it was built over as its
// lower_view_fingerprint, because the layer is a delta and the base is the
// left-hand side of it. Routing A's layer after the family's base has moved
// would serve this checkout's old delta over the new corpus. The stored lookup
// keys on the whole identity, so it must miss.
func TestCoordinatorNeverAdoptsALayerBuiltOverAnotherBase(t *testing.T) {
	f := newCoordinatorFixture(t)
	c := f.inertCoordinator(t, CheckoutCoordinatorConfig{})
	ctx := context.Background()

	initial := coordinatorReconcile(t, c)
	if !initial.CommitBuilt || initial.CommitGenerationID == 0 {
		t.Fatalf("the first cycle did not build A: %+v", initial)
	}
	stored, found := f.generation(initial.CommitGenerationID)
	if !found || stored.LowerViewFingerprint != f.treeA {
		t.Fatalf("the built layer does not name the base it was built over: %+v", stored)
	}

	// The family's committed base moves. The checkout's own tree does not.
	treeB := f.commitTreeB()
	f.movePrimaryHead(t, treeB)
	builderGit(t, f.worktree, "checkout", "--detach", "HEAD~1")

	moved := f.inertCoordinator(t, CheckoutCoordinatorConfig{})
	base, err := moved.primaryBase(ctx)
	if err != nil {
		t.Fatalf("primaryBase: %v", err)
	}
	if base.treeOID != treeB {
		t.Fatalf("the base is at %q, want the advanced %q", base.treeOID, treeB)
	}
	if generationID, ok := moved.storedCommit(ctx, moved.commitIdentity(base, f.treeA)); ok {
		t.Fatalf("generation %d, built over base %q, was offered for base %q",
			generationID, f.treeA, base.treeOID)
	}

	// And the checkout that arrives there builds its own rather than adopting.
	out := coordinatorReconcile(t, moved)
	if out.CommitReused {
		t.Fatalf("a cycle over the advanced base reused a layer: %+v", out)
	}
	row, found := f.generation(out.CommitGenerationID)
	if !found || row.LowerViewFingerprint != treeB {
		t.Fatalf("the routed layer names base %q, want the current %q",
			row.LowerViewFingerprint, treeB)
	}
}

// installCheckoutLayerWriteAudit counts the writes a measured interval makes to
// the payload tables and to view_generations, on a test-owned database.
//
// It instruments the store file directly for the reason installDedicatedWriteAudit
// does: keeping the probe in the test package avoids exporting a production-only
// hook. checkout_routes is deliberately NOT audited — the route flip is the one
// write a reuse is allowed to make, and the caller makes it after this returns.
func installCheckoutLayerWriteAudit(t *testing.T, path string) func(*testing.T) int {
	t.Helper()
	ctx := context.Background()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open the store for instrumentation: %v", err)
	}
	defer db.Close()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin the instrumentation transaction: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	_, err = tx.ExecContext(ctx,
		`CREATE TABLE checkout_layer_write_audit(n INTEGER NOT NULL); INSERT INTO checkout_layer_write_audit VALUES(0)`)
	if err != nil {
		t.Fatalf("create the audit table: %v", err)
	}
	for _, table := range []string{"view_generations", "nodes", "edges"} {
		for _, op := range []string{"INSERT", "UPDATE", "DELETE"} {
			query := fmt.Sprintf(
				`CREATE TRIGGER checkout_layer_audit_%s_%s AFTER %s ON %s BEGIN UPDATE checkout_layer_write_audit SET n=n+1; END`,
				table, op, op, table)
			if _, err := tx.ExecContext(ctx, query); err != nil {
				t.Fatalf("install the %s %s trigger: %v", table, op, err)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit the instrumentation: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close the instrumentation connection: %v", err)
	}
	return func(t *testing.T) int {
		t.Helper()
		uri := (&url.URL{Scheme: "file", Path: path, RawQuery: "mode=ro"}).String()
		probe, err := sql.Open("sqlite", uri)
		if err != nil {
			t.Fatalf("open the audit probe: %v", err)
		}
		defer probe.Close()
		var writes int
		if err := probe.QueryRowContext(ctx, `SELECT n FROM checkout_layer_write_audit`).Scan(&writes); err != nil {
			t.Fatalf("read the audit counter: %v", err)
		}
		return writes
	}
}

// TestStoredCommitLayerIdentityRecheckRefusesAForeignRow pins the Go half of
// the reuse identity check on its own terms.
//
// storedCommit checks the candidates the catalog returned against
// generationRowKey before it routes one, which against a real catalog is
// exactly redundant with the lookup's own SQL — the two filter the same
// thirteen columns, so no database state can separate them and the redundancy
// cannot be pinned end to end. That is precisely why it has to be pinned here:
// the guard exists for the case where the two halves DISAGREE, which is what a
// column added to the identity on one side only would produce, and the only way
// to state it is to hand the selector a row the SQL would never have returned.
//
// The wiring that reaches it is pinned separately, by
// TestCoordinatorAdoptsAStoredCommitLayerAfterRestart.
func TestStoredCommitLayerIdentityRecheckRefusesAForeignRow(t *testing.T) {
	identity := GenerationIdentity{
		OwnerKind: "dedicated_graph", GraphID: "graph-1", LayerID: "commit-wt",
		CheckoutID: "wt", GenerationKind: CommitLayerGenerationKind, BaseGenerationID: 7,
		LowerViewFingerprint: "base-tree", TreeOID: "tree-a", ProvenanceCommitOID: "commit-a",
		ConfigHash: "cfg", ExtractorVersions: "ex", ResolverVersion: "rv",
		DependencyRevision: "dep",
	}
	key := generationIdentityKey(identity)
	match := store_sqlite.ViewGeneration{
		GenerationID: 42, State: store_sqlite.ViewGenerationReady,
		OwnerKind: identity.OwnerKind, GraphID: identity.GraphID, LayerID: identity.LayerID,
		CheckoutID: identity.CheckoutID, GenerationKind: identity.GenerationKind,
		BaseGenerationID: identity.BaseGenerationID, LowerViewFingerprint: identity.LowerViewFingerprint,
		TreeOID: identity.TreeOID, ProvenanceCommitOID: identity.ProvenanceCommitOID,
		ConfigHash: identity.ConfigHash, ExtractorVersions: identity.ExtractorVersions,
		ResolverVersion: identity.ResolverVersion, DependencyRevision: identity.DependencyRevision,
	}
	if got, ok := selectReusableCommitGeneration([]store_sqlite.ViewGeneration{match}, key); !ok || got != 42 {
		t.Fatalf("the matching row was not selected: %d/%v", got, ok)
	}

	// One column at a time, each a different build: every one of the thirteen
	// the key renders must be refused on its own.
	foreign := map[string]func(*store_sqlite.ViewGeneration){
		"owner_kind":             func(r *store_sqlite.ViewGeneration) { r.OwnerKind = "ref_view" },
		"graph_id":               func(r *store_sqlite.ViewGeneration) { r.GraphID = "graph-2" },
		"layer_id":               func(r *store_sqlite.ViewGeneration) { r.LayerID = "commit-wt-2" },
		"checkout_id":            func(r *store_sqlite.ViewGeneration) { r.CheckoutID = "wt-2" },
		"generation_kind":        func(r *store_sqlite.ViewGeneration) { r.GenerationKind = "dirty" },
		"base_generation_id":     func(r *store_sqlite.ViewGeneration) { r.BaseGenerationID = 8 },
		"lower_view_fingerprint": func(r *store_sqlite.ViewGeneration) { r.LowerViewFingerprint = "other-base" },
		"tree_oid":               func(r *store_sqlite.ViewGeneration) { r.TreeOID = "tree-b" },
		"provenance_commit_oid":  func(r *store_sqlite.ViewGeneration) { r.ProvenanceCommitOID = "commit-b" },
		"config_hash":            func(r *store_sqlite.ViewGeneration) { r.ConfigHash = "cfg-2" },
		"extractor_versions":     func(r *store_sqlite.ViewGeneration) { r.ExtractorVersions = "ex-2" },
		"resolver_version":       func(r *store_sqlite.ViewGeneration) { r.ResolverVersion = "rv-2" },
		"dependency_revision":    func(r *store_sqlite.ViewGeneration) { r.DependencyRevision = "dep-2" },
	}
	for column, mutate := range foreign {
		row := match
		row.GenerationID = 43
		mutate(&row)
		if got, ok := selectReusableCommitGeneration([]store_sqlite.ViewGeneration{row}, key); ok {
			t.Fatalf("a row differing only in %s was routed as generation %d", column, got)
		}
		// The same foreign row ahead of a real one must be skipped, not fatal:
		// the guard refuses a candidate, it does not abandon the lookup.
		if got, ok := selectReusableCommitGeneration([]store_sqlite.ViewGeneration{row, match}, key); !ok || got != 42 {
			t.Fatalf("a foreign %s row hid the matching one: %d/%v", column, got, ok)
		}
	}

	// And a row with this exact identity that cannot be served is refused for
	// the other half of the same guard.
	for _, state := range []store_sqlite.ViewGenerationState{
		store_sqlite.ViewGenerationBuilding,
		store_sqlite.ViewGenerationRetiring,
		store_sqlite.ViewGenerationFailed,
	} {
		row := match
		row.State = state
		if got, ok := selectReusableCommitGeneration([]store_sqlite.ViewGeneration{row}, key); ok {
			t.Fatalf("a %s generation was routed as %d", state, got)
		}
	}
	for _, state := range []store_sqlite.ViewGenerationState{
		store_sqlite.ViewGenerationReady,
		store_sqlite.ViewGenerationSuperseded,
	} {
		row := match
		row.State = state
		if _, ok := selectReusableCommitGeneration([]store_sqlite.ViewGeneration{row}, key); !ok {
			t.Fatalf("a %s generation was refused", state)
		}
	}
}
