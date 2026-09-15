package store_sqlite

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// A self-contained catalog fixture, derived from the reviewed publication
// fixture. No runtime, real checkout, configuration or parser is involved.
type dependencyPublicationFixture struct {
	store   *Store
	catalog *Catalog
	path    string
	desire  DedicatedBaseDesire
}

func newDependencyPublicationFixture(t *testing.T, revision string) *dependencyPublicationFixture {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "catalog.sqlite")
	s, err := openPristine(t, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	f := &dependencyPublicationFixture{store: s, catalog: s.Catalog(), path: path}
	ctx := context.Background()
	family := RepositoryFamily{FamilyID: "dependency-family", CommonDirIdentity: filepath.Join(dir, "git"), State: "active"}
	if err := f.catalog.UpsertRepositoryFamily(ctx, family); err != nil {
		t.Fatal(err)
	}
	owner := Checkout{CheckoutID: "dependency-owner", Incarnation: "incarnation", FamilyID: family.FamilyID,
		RootPath: dir, GitDir: family.CommonDirIdentity, AdminName: "main", State: CheckoutStateReady,
		DesiredMode: CheckoutModeDedicated, EffectiveMode: CheckoutModeDedicated}
	if err := f.catalog.UpsertCheckout(ctx, owner); err != nil {
		t.Fatal(err)
	}
	g := DedicatedGraph{GraphID: "dependency-graph", OwnerCheckoutID: owner.CheckoutID, RepoPrefix: "repo", FamilyID: family.FamilyID, IsPrimaryBase: true, State: DedicatedGraphReady}
	if err := f.catalog.UpsertDedicatedGraph(ctx, g); err != nil {
		t.Fatal(err)
	}
	authority, err := f.catalog.AcquireDedicatedBaseAuthority(ctx, AcquireDedicatedBaseAuthorityRequest{
		GraphID: g.GraphID, Owner: DedicatedBaseOwner{CheckoutID: owner.CheckoutID, Incarnation: owner.Incarnation}, Token: "authority"})
	if err != nil {
		t.Fatal(err)
	}
	f.desire, err = f.catalog.RecordDedicatedBaseDesire(ctx, RecordDedicatedBaseDesireRequest{Authority: authority,
		Identity: DedicatedBaseIdentity{TreeOID: "same-local-tree", ConfigHash: "config", ExtractorVersions: "extractors", ResolverVersion: "resolver", DependencyRevision: revision}})
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *dependencyPublicationFixture) observe(t *testing.T, revision string) {
	t.Helper()
	identity := f.desire.Identity
	identity.DependencyRevision = revision
	desire, err := f.catalog.RecordDedicatedBaseDesire(context.Background(), RecordDedicatedBaseDesireRequest{
		Authority: f.desire.Authority, ExpectedDesiredEpoch: f.desire.Epoch, Identity: identity})
	if err != nil {
		t.Fatal(err)
	}
	f.desire = desire
}

func (f *dependencyPublicationFixture) claim(t *testing.T, token string, active, base int64) DedicatedBaseBuildClaim {
	t.Helper()
	req := ClaimDedicatedBaseBuildRequest{Desire: f.desire, AttemptToken: token, ExpectedActiveGenerationID: active, BaseGenerationID: base, CreatedAt: 1}
	if base > 0 {
		req.LayerID = fmt.Sprintf("dependency-layer:%d", base)
		req.LowerViewFingerprint = fmt.Sprintf("dependency-lower:%d", base)
	}
	claim, err := f.catalog.ClaimDedicatedBaseBuild(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	return claim
}

func (f *dependencyPublicationFixture) publishAdopt(t *testing.T, claim DedicatedBaseBuildClaim) {
	t.Helper()
	if err := f.catalog.PublishViewGeneration(context.Background(), claim.GenerationID, 2); err != nil {
		t.Fatal(err)
	}
	if _, err := f.catalog.AdoptDedicatedBaseGeneration(context.Background(), AdoptDedicatedBaseGenerationRequest{Claim: claim}); err != nil {
		t.Fatal(err)
	}
}

// readyGeneration creates and publishes one generation directly, outside the
// publication protocol. It is the only way to manufacture a chain the protocol
// itself refuses to certify — the shape a pre-fix binary could have left in a
// store — without reaching into store internals.
func (f *dependencyPublicationFixture) readyGeneration(t *testing.T, revision string, base int64, layer, lower string) int64 {
	t.Helper()
	ctx := context.Background()
	identity := f.desire.Identity
	id, err := f.catalog.CreateViewGeneration(ctx, ViewGeneration{
		OwnerKind: "dedicated_graph", GraphID: f.desire.Authority.GraphID,
		CheckoutID: f.desire.Authority.Owner.CheckoutID, GenerationKind: "dedicated",
		TreeOID: identity.TreeOID, ConfigHash: identity.ConfigHash,
		ExtractorVersions: identity.ExtractorVersions, ResolverVersion: identity.ResolverVersion,
		DependencyRevision: revision, BaseGenerationID: base, LayerID: layer, LowerViewFingerprint: lower,
		CreatedAt: 2, State: ViewGenerationBuilding,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.catalog.PublishViewGeneration(ctx, id, 3); err != nil {
		t.Fatal(err)
	}
	return id
}

func (f *dependencyPublicationFixture) row(t *testing.T, id int64) ViewGeneration {
	t.Helper()
	g, found, err := f.catalog.GetViewGeneration(context.Background(), id)
	if err != nil || !found {
		t.Fatalf("generation %d found=%v err=%v", id, found, err)
	}
	return g
}

// Rejecting triggers catch forbidden DML; WAL length catches committed
// metadata traffic not covered by a single update counter. Neither proves the
// whole runtime or parser is idle; these are catalog-only assertions.
func (f *dependencyPublicationFixture) noWriteOracle(t *testing.T) func() {
	t.Helper()
	exec := func(query string) {
		if _, err := f.catalog.exec(context.Background(), query); err != nil {
			t.Fatal(err)
		}
	}
	exec(`CREATE TABLE dependency_revision_write_audit(n INTEGER NOT NULL)`)
	exec(`INSERT INTO dependency_revision_write_audit VALUES(0)`)
	for _, table := range []string{"dedicated_base_publications", "view_generations", "dedicated_graphs"} {
		for _, event := range []string{"INSERT", "UPDATE", "DELETE"} {
			exec(fmt.Sprintf("CREATE TRIGGER dependency_reject_%s_%s BEFORE %s ON %s BEGIN SELECT RAISE(ABORT,'dependency revision forbidden DML'); END", table, event, event, table))
			exec(fmt.Sprintf("CREATE TRIGGER dependency_audit_%s_%s AFTER %s ON %s BEGIN UPDATE dependency_revision_write_audit SET n=n+1; END", table, event, event, table))
		}
	}
	walSize := func() int64 {
		info, err := os.Stat(f.path + "-wal")
		if errors.Is(err, os.ErrNotExist) {
			return 0
		}
		if err != nil {
			t.Fatal(err)
		}
		return info.Size()
	}
	before := walSize()
	return func() {
		t.Helper()
		var writes int
		if err := f.store.db.QueryRow(`SELECT n FROM dependency_revision_write_audit`).Scan(&writes); err != nil {
			t.Fatal(err)
		}
		if writes != 0 || walSize() != before {
			t.Fatalf("unexpected catalog writes=%d WAL=%d->%d", writes, before, walSize())
		}
	}
}

// A dependency-only change still advances the desire by exactly one epoch and
// still produces a distinct output identity, but the new output ROOTS: the
// catalog no longer certifies a chain whose lower was frozen under a different
// dependency revision, so the caller must claim it with no proposed parent.
// The historical same-revision root stays reusable with its own metadata.
func TestDedicatedDependencyRevisionParentAndHistoricalReuse(t *testing.T) {
	f := newDependencyPublicationFixture(t, "cohort-v1:a")
	aDesire := f.desire
	a := f.claim(t, "a", 0, 0)
	f.publishAdopt(t, a)
	f.observe(t, "cohort-v1:b")
	if f.desire.Epoch != aDesire.Epoch+1 || f.desire.Identity.TreeOID != aDesire.Identity.TreeOID {
		t.Fatal("dependency-only desire did not advance exactly one epoch")
	}
	b := f.claim(t, "b", a.GenerationID, 0)
	if b.Status != "allocated" || b.GenerationID == a.GenerationID || b.BaseGenerationID != 0 ||
		b.LayerID != "" || b.LowerViewFingerprint != "" {
		t.Fatalf("dependency-only re-root=%+v", b)
	}
	row := f.row(t, b.GenerationID)
	if row.DependencyRevision != "cohort-v1:b" || row.ConfigHash != aDesire.Identity.ConfigHash || row.TreeOID != aDesire.Identity.TreeOID {
		t.Fatalf("output identity roundtrip=%+v", row)
	}
	f.publishAdopt(t, b)
	f.observe(t, "cohort-v1:a")
	if _, err := f.catalog.AdoptDedicatedBaseGeneration(context.Background(), AdoptDedicatedBaseGenerationRequest{Claim: a}); !errors.Is(err, ErrCatalogStaleGuard) {
		t.Fatalf("old A epoch adopted after ABA: %v", err)
	}
	// Proposed B lower must not overwrite the actual historical A metadata.
	reused := f.claim(t, "a-again", b.GenerationID, b.GenerationID)
	if reused.Status != "ready" || reused.GenerationID != a.GenerationID || reused.BaseGenerationID != 0 || reused.LayerID != "" || reused.LowerViewFingerprint != "" {
		t.Fatalf("historical reuse=%+v", reused)
	}
}

// Ready reuse hands the publisher a complete output to adopt as-is, so every
// layer it composes must have been frozen under the current dependency inputs.
// A head that matches over an older lower is a mixed composition: reusing it
// returns base_generation_id = that lower, which routes the publisher into a
// delta its builder must refuse — a publication wedge that lasts for as long
// as the tree is unchanged. The homogeneous arm is the control that proves
// this does not simply disable ready reuse.
func TestDedicatedDependencyRevisionReadyReuseRequiresHomogeneousAncestry(t *testing.T) {
	for _, homogeneous := range []bool{true, false} {
		name := "mixed-chain-not-reused"
		if homogeneous {
			name = "homogeneous-chain-reused"
		}
		t.Run(name, func(t *testing.T) {
			f := newDependencyPublicationFixture(t, "cohort-v1:a")
			a := f.claim(t, "a", 0, 0)
			f.publishAdopt(t, a)
			f.observe(t, "cohort-v1:b")
			lowerRevision := "cohort-v1:a"
			if homogeneous {
				lowerRevision = "cohort-v1:b"
			}
			lower := f.readyGeneration(t, lowerRevision, 0, "", "")
			head := f.readyGeneration(t, "cohort-v1:b", lower, "dependency-head", "dependency-head-lower")
			claim := f.claim(t, "scan", a.GenerationID, 0)
			if homogeneous {
				if claim.Status != "ready" || claim.GenerationID != head || claim.BaseGenerationID != lower ||
					claim.LayerID != "dependency-head" || claim.LowerViewFingerprint != "dependency-head-lower" {
					t.Fatalf("same-revision chain was not reused: %+v (head=%d lower=%d)", claim, head, lower)
				}
				return
			}
			if claim.Status != "allocated" || claim.GenerationID == head || claim.GenerationID == lower ||
				claim.BaseGenerationID != 0 || claim.LayerID != "" || claim.LowerViewFingerprint != "" {
				t.Fatalf("mixed-revision chain was reused as ready output: %+v (head=%d lower=%d)", claim, head, lower)
			}
			if row := f.row(t, head); row.State != ViewGenerationReady || row.DependencyRevision != "cohort-v1:b" {
				t.Fatalf("refused candidate was mutated: %+v", row)
			}
		})
	}
}

// The proposed-parent arm deliberately keeps its latitude — the indexer's
// builder is the guard that refuses to extend a chain across a revision change,
// and that guard must stay reachable. Certification is the harder fence:
// adoption validates the whole composition, so a mixed chain can be reserved
// and built but never becomes the active committed output.
func TestDedicatedDependencyRevisionMixedCompositionReservesButCannotAdopt(t *testing.T) {
	ctx := context.Background()
	f := newDependencyPublicationFixture(t, "cohort-v1:a")
	a := f.claim(t, "a", 0, 0)
	f.publishAdopt(t, a)
	f.observe(t, "cohort-v1:b")
	mixed := f.claim(t, "mixed", a.GenerationID, a.GenerationID)
	if mixed.Status != "allocated" || mixed.BaseGenerationID != a.GenerationID {
		t.Fatalf("proposed-parent reservation is no longer handed out, so the builder guard is dead code: %+v", mixed)
	}
	// The builder re-validates its own still-building reservation through the
	// validation-only claim mode before writing payload. Refusing the mixed
	// parent there would take the decision away from the builder's typed guard.
	validated, err := f.catalog.ClaimDedicatedBaseBuild(ctx, ClaimDedicatedBaseBuildRequest{
		ExistingGenerationID: mixed.GenerationID, Desire: f.desire, ExpectedActiveGenerationID: mixed.ExpectedActiveGenerationID,
		AttemptToken: mixed.AttemptToken, BaseGenerationID: mixed.BaseGenerationID,
		LayerID: mixed.LayerID, LowerViewFingerprint: mixed.LowerViewFingerprint})
	if err != nil || validated.GenerationID != mixed.GenerationID || validated.BaseGenerationID != a.GenerationID {
		t.Fatalf("building reservation was refused before its builder saw it: %+v err=%v", validated, err)
	}
	if err := f.catalog.PublishViewGeneration(ctx, mixed.GenerationID, 4); err != nil {
		t.Fatal(err)
	}
	if _, err := f.catalog.AdoptDedicatedBaseGeneration(ctx, AdoptDedicatedBaseGenerationRequest{Claim: mixed}); !errors.Is(err, ErrDedicatedBaseCandidate) {
		t.Fatalf("mixed composition was adopted: %v", err)
	}
	graph, found, err := f.catalog.GetDedicatedGraph(ctx, f.desire.Authority.GraphID)
	if err != nil || !found || graph.ActiveGenerationID != a.GenerationID {
		t.Fatalf("refused adoption moved the active pointer: graph=%+v found=%v err=%v", graph, found, err)
	}
}

func TestDedicatedDependencyRevisionLegacyNotCertified(t *testing.T) {
	f := newDependencyPublicationFixture(t, "")
	a := f.claim(t, "legacy", 0, 0)
	f.publishAdopt(t, a)
	f.observe(t, "cohort-v1:empty-roster")
	b := f.claim(t, "certified", a.GenerationID, a.GenerationID)
	if b.Status != "allocated" || b.GenerationID == a.GenerationID || f.row(t, a.GenerationID).DependencyRevision != "" {
		t.Fatalf("legacy output was certified/reused: %+v", b)
	}
}

func TestDedicatedDependencyRevisionUnchangedReplayWritesNothing(t *testing.T) {
	f := newDependencyPublicationFixture(t, "cohort-v1:stable")
	a := f.claim(t, "stable", 0, 0)
	f.publishAdopt(t, a)
	epoch := f.desire.Epoch
	assertUnchanged := f.noWriteOracle(t)
	for range 3 {
		f.observe(t, "cohort-v1:stable")
		replayed := f.claim(t, "ignored", a.GenerationID, a.GenerationID)
		if f.desire.Epoch != epoch || replayed.GenerationID != a.GenerationID || !replayed.AlreadyAdopted || replayed.AttemptToken != a.AttemptToken {
			t.Fatalf("unchanged replay=%+v", replayed)
		}
		if _, err := f.catalog.AdoptDedicatedBaseGeneration(context.Background(), AdoptDedicatedBaseGenerationRequest{Claim: replayed}); err != nil {
			t.Fatal(err)
		}
	}
	assertUnchanged()
}

func TestDedicatedDependencyRevisionExistingClaimMismatchIsReadOnly(t *testing.T) {
	f := newDependencyPublicationFixture(t, "cohort-v1:a")
	a := f.claim(t, "a", 0, 0)
	wrong := f.desire
	wrong.Identity.DependencyRevision = "cohort-v1:wrong"
	assertUnchanged := f.noWriteOracle(t)
	_, err := f.catalog.ClaimDedicatedBaseBuild(context.Background(), ClaimDedicatedBaseBuildRequest{
		Desire: wrong, ExistingGenerationID: a.GenerationID, AttemptToken: a.AttemptToken})
	if !errors.Is(err, ErrCatalogStaleGuard) {
		t.Fatalf("wrong revision accepted as existing claim: %v", err)
	}
	assertUnchanged()
}

func TestDedicatedDependencyRevisionTamperedOutputCannotValidateOrAdopt(t *testing.T) {
	f := newDependencyPublicationFixture(t, "cohort-v1:a")
	a := f.claim(t, "a", 0, 0)
	if err := f.catalog.PublishViewGeneration(context.Background(), a.GenerationID, 2); err != nil {
		t.Fatal(err)
	}
	// Deliberately corrupt PRIVATE metadata to exercise the candidate row guard.
	if _, err := f.catalog.exec(context.Background(), `UPDATE view_generations SET dependency_revision=? WHERE generation_id=?`, "cohort-v1:wrong", a.GenerationID); err != nil {
		t.Fatal(err)
	}
	assertUnchanged := f.noWriteOracle(t)
	_, err := f.catalog.ClaimDedicatedBaseBuild(context.Background(), ClaimDedicatedBaseBuildRequest{
		Desire: f.desire, ExistingGenerationID: a.GenerationID, AttemptToken: a.AttemptToken})
	if !errors.Is(err, ErrDedicatedBaseCandidate) {
		t.Fatalf("tampered row validated: %v", err)
	}
	if _, err := f.catalog.AdoptDedicatedBaseGeneration(context.Background(), AdoptDedicatedBaseGenerationRequest{Claim: a}); !errors.Is(err, ErrDedicatedBaseCandidate) {
		t.Fatalf("tampered row adopted: %v", err)
	}
	assertUnchanged()
}

// The building-candidate exception covers the candidate's ANCESTRY only. The
// candidate's own stored revision is still checked at depth 0, so a builder
// cannot be told its reservation is live after that row was retagged.
func TestDedicatedDependencyRevisionTamperedBuildingCandidateCannotValidate(t *testing.T) {
	ctx := context.Background()
	f := newDependencyPublicationFixture(t, "cohort-v1:a")
	a := f.claim(t, "a", 0, 0)
	if a.Status != "allocated" {
		t.Fatalf("fixture candidate is not building: %+v", a)
	}
	// Deliberately corrupt PRIVATE metadata of a still-building reservation.
	if _, err := f.catalog.exec(ctx, `UPDATE view_generations SET dependency_revision=? WHERE generation_id=?`, "cohort-v1:wrong", a.GenerationID); err != nil {
		t.Fatal(err)
	}
	assertUnchanged := f.noWriteOracle(t)
	_, err := f.catalog.ClaimDedicatedBaseBuild(ctx, ClaimDedicatedBaseBuildRequest{
		Desire: f.desire, ExistingGenerationID: a.GenerationID, AttemptToken: a.AttemptToken})
	if !errors.Is(err, ErrDedicatedBaseCandidate) {
		t.Fatalf("tampered building candidate validated: %v", err)
	}
	assertUnchanged()
}

func TestDedicatedDependencyRevisionWarmReopenRoundtrip(t *testing.T) {
	f := newDependencyPublicationFixture(t, "cohort-v1:persisted")
	a := f.claim(t, "persisted", 0, 0)
	f.publishAdopt(t, a)
	before, found, err := f.catalog.DedicatedBasePublication(context.Background(), f.desire.Authority.GraphID)
	if err != nil || !found {
		t.Fatalf("publication found=%v err=%v", found, err)
	}
	if err := f.store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(f.path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	after, found, err := reopened.Catalog().DedicatedBasePublication(context.Background(), f.desire.Authority.GraphID)
	if err != nil || !found || before != after {
		t.Fatalf("publication changed on reopen: before=%+v after=%+v found=%v err=%v", before, after, found, err)
	}
	row, found, err := reopened.Catalog().GetViewGeneration(context.Background(), a.GenerationID)
	if err != nil || !found || row.DependencyRevision != "cohort-v1:persisted" {
		t.Fatalf("generation revision lost on reopen: row=%+v found=%v err=%v", row, found, err)
	}
}

func TestDedicatedDependencyRevisionDoesNotRelaxParentParsePolicy(t *testing.T) {
	f := newDependencyPublicationFixture(t, "cohort-v1:a")
	a := f.claim(t, "a", 0, 0)
	f.publishAdopt(t, a)
	identity := f.desire.Identity
	identity.DependencyRevision = "cohort-v1:b"
	identity.ConfigHash = "changed-parse-policy"
	desire, err := f.catalog.RecordDedicatedBaseDesire(context.Background(), RecordDedicatedBaseDesireRequest{
		Authority: f.desire.Authority, ExpectedDesiredEpoch: f.desire.Epoch, Identity: identity})
	if err != nil {
		t.Fatal(err)
	}
	assertUnchanged := f.noWriteOracle(t)
	_, err = f.catalog.ClaimDedicatedBaseBuild(context.Background(), ClaimDedicatedBaseBuildRequest{
		Desire: desire, AttemptToken: "b", ExpectedActiveGenerationID: a.GenerationID, BaseGenerationID: a.GenerationID})
	if !errors.Is(err, ErrDedicatedBaseCandidate) {
		t.Fatalf("different parent parse policy accepted: %v", err)
	}
	assertUnchanged()
}

func TestDedicatedDependencyRevisionFailedTopRecoveryGuards(t *testing.T) {
	t.Run("wrong-revision-refuses", func(t *testing.T) {
		f := newDependencyPublicationFixture(t, "cohort-v1:a")
		a := f.claim(t, "a", 0, 0)
		// Model the existing crash boundary in PRIVATE catalog metadata, then
		// corrupt only top output provenance. This is not a physical-failure test.
		if _, err := f.catalog.exec(context.Background(), `UPDATE view_generations SET state='failed',dependency_revision='cohort-v1:wrong' WHERE generation_id=?`, a.GenerationID); err != nil {
			t.Fatal(err)
		}
		assertUnchanged := f.noWriteOracle(t)
		_, err := f.catalog.ClaimDedicatedBaseBuild(context.Background(), ClaimDedicatedBaseBuildRequest{Desire: f.desire, AttemptToken: "replacement"})
		if !errors.Is(err, ErrDedicatedBaseCandidate) {
			t.Fatalf("wrong-revision failed output recovered: %v", err)
		}
		assertUnchanged()
	})
	t.Run("existing-mode-cannot-replace", func(t *testing.T) {
		f := newDependencyPublicationFixture(t, "cohort-v1:a")
		a := f.claim(t, "a", 0, 0)
		if _, err := f.catalog.exec(context.Background(), `UPDATE view_generations SET state='failed' WHERE generation_id=?`, a.GenerationID); err != nil {
			t.Fatal(err)
		}
		assertUnchanged := f.noWriteOracle(t)
		_, err := f.catalog.ClaimDedicatedBaseBuild(context.Background(), ClaimDedicatedBaseBuildRequest{Desire: f.desire, AttemptToken: a.AttemptToken, ExistingGenerationID: a.GenerationID})
		if err == nil {
			t.Fatal("validation-only request replaced failed output")
		}
		assertUnchanged()
	})
	t.Run("same-revision-ordinary-recovery", func(t *testing.T) {
		f := newDependencyPublicationFixture(t, "cohort-v1:a")
		a := f.claim(t, "a", 0, 0)
		if _, err := f.catalog.exec(context.Background(), `UPDATE view_generations SET state='failed' WHERE generation_id=?`, a.GenerationID); err != nil {
			t.Fatal(err)
		}
		epoch := f.desire.Epoch
		b := f.claim(t, "replacement", 0, 0)
		if b.GenerationID == a.GenerationID || b.Status != "allocated" || b.Desire.Epoch != epoch || b.Desire.Identity.DependencyRevision != "cohort-v1:a" {
			t.Fatalf("same-desire recovery=%+v", b)
		}
		old := f.row(t, a.GenerationID)
		if old.State != ViewGenerationFailed || old.DependencyRevision != "cohort-v1:a" {
			t.Fatalf("recovery altered failed payload identity: %+v", old)
		}
	})
}
