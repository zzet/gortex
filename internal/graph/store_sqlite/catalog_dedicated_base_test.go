package store_sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

type dedicatedPublicationFixture struct {
	c         *Catalog
	store     *Store
	path      string
	owner     Checkout
	graph     DedicatedGraph
	authority DedicatedBaseAuthority
	desire    DedicatedBaseDesire
}

func newDedicatedPublicationFixture(t testing.TB) *dedicatedPublicationFixture {
	t.Helper()
	path := filepath.Join(t.TempDir(), "catalog.sqlite")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	f := &dedicatedPublicationFixture{c: s.Catalog(), store: s, path: path}
	ctx := context.Background()
	family := RepositoryFamily{FamilyID: "family", CommonDirIdentity: filepath.Join(filepath.Dir(path), "git"), State: "active"}
	if err = f.c.UpsertRepositoryFamily(ctx, family); err != nil {
		t.Fatal(err)
	}
	f.owner = Checkout{CheckoutID: "owner", Incarnation: "incarnation", FamilyID: family.FamilyID, RootPath: filepath.Dir(path), GitDir: family.CommonDirIdentity, AdminName: "main", State: CheckoutStateReady, DesiredMode: CheckoutModeDedicated, EffectiveMode: CheckoutModeDedicated, HeadTree: "live-dirty-observation-is-not-the-snapshot"}
	if err = f.c.UpsertCheckout(ctx, f.owner); err != nil {
		t.Fatal(err)
	}
	f.graph = DedicatedGraph{GraphID: "graph", OwnerCheckoutID: f.owner.CheckoutID, RepoPrefix: "repo", FamilyID: family.FamilyID, IsPrimaryBase: true, State: "ready"}
	if err = f.c.UpsertDedicatedGraph(ctx, f.graph); err != nil {
		t.Fatal(err)
	}
	f.authority, err = f.c.AcquireDedicatedBaseAuthority(ctx, AcquireDedicatedBaseAuthorityRequest{GraphID: f.graph.GraphID, Owner: DedicatedBaseOwner{f.owner.CheckoutID, f.owner.Incarnation}, Token: "authority-1"})
	if err != nil {
		t.Fatal(err)
	}
	f.desire, err = f.c.RecordDedicatedBaseDesire(ctx, RecordDedicatedBaseDesireRequest{Authority: f.authority, Identity: DedicatedBaseIdentity{"tree-a", "config", "extractors", "resolver"}})
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *dedicatedPublicationFixture) claim(t testing.TB, token string, active, base int64) DedicatedBaseBuildClaim {
	t.Helper()
	claim, err := f.c.ClaimDedicatedBaseBuild(context.Background(), ClaimDedicatedBaseBuildRequest{Desire: f.desire, AttemptToken: token, ExpectedActiveGenerationID: active, BaseGenerationID: base, CreatedAt: 1})
	if err != nil {
		t.Fatal(err)
	}
	return claim
}
func (f *dedicatedPublicationFixture) publish(t testing.TB, claim DedicatedBaseBuildClaim) {
	t.Helper()
	if err := f.c.PublishViewGeneration(context.Background(), claim.GenerationID, 2); err != nil {
		t.Fatal(err)
	}
}
func (f *dedicatedPublicationFixture) adopt(t testing.TB, claim DedicatedBaseBuildClaim) DedicatedBaseAdoption {
	t.Helper()
	out, err := f.c.AdoptDedicatedBaseGeneration(context.Background(), AdoptDedicatedBaseGenerationRequest{claim})
	if err != nil {
		t.Fatal(err)
	}
	return out
}
func (f *dedicatedPublicationFixture) observe(t testing.TB, identity DedicatedBaseIdentity) {
	t.Helper()
	var err error
	f.desire, err = f.c.RecordDedicatedBaseDesire(context.Background(), RecordDedicatedBaseDesireRequest{Authority: f.authority, ExpectedDesiredEpoch: f.desire.Epoch, Identity: identity})
	if err != nil {
		t.Fatal(err)
	}
}
func (f *dedicatedPublicationFixture) count(t testing.TB) int {
	t.Helper()
	var n int
	if err := f.store.db.QueryRow(`SELECT count(*) FROM view_generations`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
func (f *dedicatedPublicationFixture) exec(t testing.TB, query string, args ...any) {
	t.Helper()
	if _, err := f.c.exec(context.Background(), query, args...); err != nil {
		t.Fatal(err)
	}
}
func (f *dedicatedPublicationFixture) active(t testing.TB) int64 {
	t.Helper()
	g, found, err := f.c.GetDedicatedGraph(context.Background(), f.graph.GraphID)
	if err != nil || !found {
		t.Fatalf("graph found=%v err=%v", found, err)
	}
	return g.ActiveGenerationID
}

func TestDedicatedPublicationInitialAndReadyReuse(t *testing.T) {
	f := newDedicatedPublicationFixture(t)
	a := f.claim(t, "attempt-a", 0, 0)
	if a.GenerationID <= 0 || a.Status != "allocated" || f.count(t) != 1 {
		t.Fatalf("allocation=%+v count=%d", a, f.count(t))
	}
	b := f.claim(t, "unused-follower-token", 0, 0)
	if b.GenerationID != a.GenerationID || b.AttemptToken != a.AttemptToken || b.Status != "building" || f.count(t) != 1 {
		t.Fatalf("follower=%+v first=%+v", b, a)
	}
	f.publish(t, a)
	r := f.claim(t, "unused-ready-token", 0, 0)
	if r.Status != "ready" || r.GenerationID != a.GenerationID || f.count(t) != 1 {
		t.Fatalf("ready=%+v", r)
	}
	if got := f.adopt(t, r); got.AlreadyAdopted || got.GenerationID != a.GenerationID {
		t.Fatalf("adoption=%+v", got)
	}
	if got := f.adopt(t, a); !got.AlreadyAdopted {
		t.Fatalf("retry=%+v", got)
	}
	if f.active(t) != a.GenerationID {
		t.Fatal("active pointer not atomically installed")
	}
}

func TestDedicatedPublicationConcurrentSingleAllocation(t *testing.T) {
	f := newDedicatedPublicationFixture(t)
	const n = 12
	claims := make([]DedicatedBaseBuildClaim, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			claims[i], errs[i] = f.c.ClaimDedicatedBaseBuild(context.Background(), ClaimDedicatedBaseBuildRequest{Desire: f.desire, AttemptToken: fmt.Sprint("attempt-", i)})
		}()
	}
	wg.Wait()
	allocated := 0
	for i, c := range claims {
		if errs[i] != nil {
			t.Fatal(errs[i])
		}
		if c.GenerationID != claims[0].GenerationID || c.AttemptToken != claims[0].AttemptToken {
			t.Fatalf("uncoalesced: %+v", claims)
		}
		if c.Status == "allocated" {
			allocated++
		}
	}
	if allocated != 1 || f.count(t) != 1 {
		t.Fatalf("allocated=%d generation_count=%d", allocated, f.count(t))
	}
}

func TestDedicatedPublicationUnchangedCycleNoWrites(t *testing.T) {
	f := newDedicatedPublicationFixture(t)
	claim := f.claim(t, "first", 0, 0)
	f.publish(t, claim)
	f.adopt(t, claim)
	f.exec(t, `CREATE TABLE publication_write_audit (n INTEGER NOT NULL)`)
	f.exec(t, `INSERT INTO publication_write_audit VALUES(0)`)
	f.exec(t, `CREATE TRIGGER publication_write_audit_update AFTER UPDATE ON dedicated_base_publications BEGIN UPDATE publication_write_audit SET n=n+1; END`)
	f.exec(t, `CREATE TRIGGER publication_write_audit_active AFTER UPDATE OF active_generation_id ON dedicated_graphs BEGIN UPDATE publication_write_audit SET n=n+1; END`)
	walSize := func() int64 {
		s, e := os.Stat(f.path + "-wal")
		if errors.Is(e, os.ErrNotExist) {
			return 0
		}
		if e != nil {
			t.Fatal(e)
		}
		return s.Size()
	}
	before := walSize()
	for range 10 {
		f.observe(t, f.desire.Identity)
		again := f.claim(t, "ignored-new-token", claim.GenerationID, claim.GenerationID)
		if !again.AlreadyAdopted || again.AttemptToken != claim.AttemptToken {
			t.Fatalf("did not preserve adopted attempt: %+v", again)
		}
		if !f.adopt(t, again).AlreadyAdopted {
			t.Fatal("lost adoption idempotence")
		}
	}
	var writes int
	if err := f.store.db.QueryRow(`SELECT n FROM publication_write_audit`).Scan(&writes); err != nil {
		t.Fatal(err)
	}
	if writes != 0 || f.count(t) != 1 || walSize() != before {
		t.Fatalf("idle writes=%d count=%d WAL=%d→%d", writes, f.count(t), before, walSize())
	}
}

func TestDedicatedPublicationDesireABAAndPolicyChange(t *testing.T) {
	f := newDedicatedPublicationFixture(t)
	aIdentity := f.desire.Identity
	a := f.claim(t, "a", 0, 0)
	f.publish(t, a)
	f.adopt(t, a)
	bIdentity := aIdentity
	bIdentity.TreeOID = "tree-b"
	f.observe(t, bIdentity)
	b := f.claim(t, "b", a.GenerationID, a.GenerationID)
	f.publish(t, b)
	f.adopt(t, b)
	f.observe(t, aIdentity)
	if _, err := f.c.AdoptDedicatedBaseGeneration(context.Background(), AdoptDedicatedBaseGenerationRequest{a}); !errors.Is(err, ErrCatalogStaleGuard) {
		t.Fatalf("old A epoch published: %v", err)
	}
	reused := f.claim(t, "a-new-epoch", b.GenerationID, b.GenerationID)
	if reused.GenerationID != a.GenerationID || reused.BaseGenerationID != 0 || reused.ExpectedActiveGenerationID != b.GenerationID || reused.Status != "ready" {
		t.Fatalf("historical A reuse=%+v", reused)
	}
	f.adopt(t, reused)
	for _, field := range []string{"config", "extractors", "resolver"} {
		t.Run(field, func(t *testing.T) {
			identity := f.desire.Identity
			switch field {
			case "config":
				identity.ConfigHash += "-new"
			case "extractors":
				identity.ExtractorVersions += "-new"
			case "resolver":
				identity.ResolverVersion += "-new"
			}
			f.observe(t, identity)
			_, err := f.c.ClaimDedicatedBaseBuild(context.Background(), ClaimDedicatedBaseBuildRequest{Desire: f.desire, AttemptToken: field, ExpectedActiveGenerationID: f.active(t), BaseGenerationID: f.active(t)})
			if !errors.Is(err, ErrDedicatedBaseCandidate) {
				t.Fatalf("incompatible inherited policy admitted: %v", err)
			}
			seed := f.claim(t, field+"-seed", f.active(t), 0)
			f.publish(t, seed)
			f.adopt(t, seed)
		})
	}
}

func TestDedicatedPublicationTakeoverCannotJoinOldWriter(t *testing.T) {
	f := newDedicatedPublicationFixture(t)
	old := f.claim(t, "old-attempt", 0, 0)
	oldAuthority := f.authority
	a, err := f.c.AcquireDedicatedBaseAuthority(context.Background(), AcquireDedicatedBaseAuthorityRequest{GraphID: f.graph.GraphID, Owner: oldAuthority.Owner, ExpectedEpoch: oldAuthority.Epoch, ExpectedToken: oldAuthority.Token, Token: "authority-2"})
	if err != nil {
		t.Fatal(err)
	}
	f.authority = a
	f.desire.Authority = a
	if _, err = f.c.RecordDedicatedBaseDesire(context.Background(), RecordDedicatedBaseDesireRequest{Authority: oldAuthority, ExpectedDesiredEpoch: f.desire.Epoch, Identity: f.desire.Identity}); !errors.Is(err, ErrCatalogStaleGuard) {
		t.Fatalf("old observer accepted: %v", err)
	}
	newClaim := f.claim(t, "new-attempt", 0, 0)
	if newClaim.GenerationID == old.GenerationID || newClaim.Status != "allocated" {
		t.Fatalf("unverified old writer reused: %+v", newClaim)
	}
	f.publish(t, old)
	if _, err = f.c.AdoptDedicatedBaseGeneration(context.Background(), AdoptDedicatedBaseGenerationRequest{old}); !errors.Is(err, ErrCatalogStaleGuard) {
		t.Fatalf("old writer adopted: %v", err)
	}
	f.publish(t, newClaim)
	f.adopt(t, newClaim)
}

func TestDedicatedPublicationAuthorityTakeoverReusesReadyCandidate(t *testing.T) {
	f := newDedicatedPublicationFixture(t)
	old := f.claim(t, "old", 0, 0)
	f.publish(t, old)
	a, err := f.c.AcquireDedicatedBaseAuthority(context.Background(), AcquireDedicatedBaseAuthorityRequest{GraphID: f.graph.GraphID, Owner: f.authority.Owner, ExpectedEpoch: f.authority.Epoch, ExpectedToken: f.authority.Token, Token: "new-authority"})
	if err != nil {
		t.Fatal(err)
	}
	f.authority = a
	f.desire.Authority = a
	r := f.claim(t, "new-attempt", 0, 0)
	if r.GenerationID != old.GenerationID || r.Status != "ready" || r.AttemptToken == old.AttemptToken {
		t.Fatalf("ready reuse after takeover=%+v", r)
	}
	f.adopt(t, r)
}

func TestDedicatedPublicationFailureAndStaleGuardRollback(t *testing.T) {
	f := newDedicatedPublicationFixture(t)
	a := f.claim(t, "attempt-a", 0, 0)
	if err := f.c.FailDedicatedBaseBuild(context.Background(), FailDedicatedBaseBuildRequest{a, "interrupted"}); err != nil {
		t.Fatal(err)
	}
	b := f.claim(t, "attempt-b", 0, 0)
	if b.GenerationID == a.GenerationID {
		t.Fatal("failed writer reused")
	}
	f.publish(t, a)
	f.publish(t, b)
	if _, err := f.c.AdoptDedicatedBaseGeneration(context.Background(), AdoptDedicatedBaseGenerationRequest{a}); !errors.Is(err, ErrCatalogStaleGuard) {
		t.Fatal(err)
	}
	f.exec(t, `UPDATE dedicated_graphs SET active_generation_id=? WHERE graph_id=?`, a.GenerationID, f.graph.GraphID)
	if _, err := f.c.AdoptDedicatedBaseGeneration(context.Background(), AdoptDedicatedBaseGenerationRequest{b}); !errors.Is(err, ErrCatalogStaleGuard) {
		t.Fatalf("stale pointer accepted: %v", err)
	}
	p, _, err := f.c.DedicatedBasePublication(context.Background(), f.graph.GraphID)
	if err != nil {
		t.Fatal(err)
	}
	if p.AttemptState != "building" || f.active(t) != a.GenerationID {
		t.Fatalf("failed CAS partially changed state: %+v", p)
	}
}

func TestDedicatedPublicationAllocationAssociationRollback(t *testing.T) {
	f := newDedicatedPublicationFixture(t)
	f.exec(t, `CREATE TRIGGER reject_candidate_association BEFORE UPDATE OF generation_id ON dedicated_base_publications WHEN NEW.generation_id>0 BEGIN SELECT RAISE(ABORT,'test association failure'); END`)
	_, err := f.c.ClaimDedicatedBaseBuild(context.Background(), ClaimDedicatedBaseBuildRequest{Desire: f.desire, AttemptToken: "cannot-bind"})
	if err == nil || f.count(t) != 0 {
		t.Fatalf("allocation escaped association rollback: err=%v count=%d", err, f.count(t))
	}
}

func TestDedicatedPublicationAuthorizationRejectsUnsupportedOwners(t *testing.T) {
	for _, tc := range []struct {
		name, query string
		args        []any
	}{
		{"wrong_incarnation", `UPDATE checkouts SET incarnation='replacement' WHERE checkout_id='owner'`, nil},
		{"unavailable", `UPDATE checkouts SET state='unavailable' WHERE checkout_id='owner'`, nil},
		{"promotion_effective_overlay", `UPDATE checkouts SET effective_mode='automatic_overlay' WHERE checkout_id='owner'`, nil},
		{"demotion_desired_overlay", `UPDATE checkouts SET desired_mode='automatic_overlay' WHERE checkout_id='owner'`, nil},
		{"active_transition", `UPDATE checkouts SET active_intent_transition_id='transition' WHERE checkout_id='owner'`, nil},
		{"graph_not_ready", `UPDATE dedicated_graphs SET state='building' WHERE graph_id='graph'`, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newDedicatedPublicationFixture(t)
			c := f.claim(t, "a", 0, 0)
			f.publish(t, c)
			f.exec(t, tc.query, tc.args...)
			if _, err := f.c.AdoptDedicatedBaseGeneration(context.Background(), AdoptDedicatedBaseGenerationRequest{c}); !errors.Is(err, ErrCatalogStaleGuard) {
				t.Fatalf("unsupported owner authorized: %v", err)
			}
		})
	}
}

func TestDedicatedPublicationCandidateRejectionMatrix(t *testing.T) {
	for _, tc := range []struct{ name, query string }{
		{"owner_kind", `UPDATE view_generations SET owner_kind='ref_view' WHERE generation_id=?`},
		{"generation_kind", `UPDATE view_generations SET generation_kind='commit' WHERE generation_id=?`},
		{"graph", `UPDATE view_generations SET graph_id='foreign' WHERE generation_id=?`},
		{"checkout", `UPDATE view_generations SET checkout_id='foreign' WHERE generation_id=?`},
		{"tree", `UPDATE view_generations SET tree_oid='foreign-tree' WHERE generation_id=?`},
		{"empty_tree", `UPDATE view_generations SET tree_oid='' WHERE generation_id=?`},
		{"config", `UPDATE view_generations SET config_hash='foreign' WHERE generation_id=?`},
		{"extractors", `UPDATE view_generations SET extractor_versions='foreign' WHERE generation_id=?`},
		{"resolver", `UPDATE view_generations SET resolver_version='foreign' WHERE generation_id=?`},
		{"building", `UPDATE view_generations SET state='building' WHERE generation_id=?`},
		{"failed", `UPDATE view_generations SET state='failed' WHERE generation_id=?`},
		{"retiring", `UPDATE view_generations SET state='retiring' WHERE generation_id=?`},
		{"physical_layer", `UPDATE view_generations SET layer_id='wrong' WHERE generation_id=?`},
		{"physical_lower_fingerprint", `UPDATE view_generations SET lower_view_fingerprint='wrong' WHERE generation_id=?`},
		{"self_cycle", `UPDATE view_generations SET base_generation_id=generation_id WHERE generation_id=?`},
		{"missing", `DELETE FROM view_generations WHERE generation_id=?`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newDedicatedPublicationFixture(t)
			c := f.claim(t, "a", 0, 0)
			f.publish(t, c)
			f.exec(t, tc.query, c.GenerationID)
			if _, err := f.c.AdoptDedicatedBaseGeneration(context.Background(), AdoptDedicatedBaseGenerationRequest{c}); !errors.Is(err, ErrDedicatedBaseCandidate) {
				t.Fatalf("bad candidate accepted: %v", err)
			}
			if f.active(t) != 0 {
				t.Fatal("bad candidate changed active pointer")
			}
		})
	}
}

func TestDedicatedPublicationAncestryAndCascade(t *testing.T) {
	f := newDedicatedPublicationFixture(t)
	a := f.claim(t, "a", 0, 0)
	f.publish(t, a)
	f.adopt(t, a)
	bIdentity := f.desire.Identity
	bIdentity.TreeOID = "tree-b"
	f.observe(t, bIdentity)
	b := f.claim(t, "b", a.GenerationID, a.GenerationID)
	f.publish(t, b)
	f.exec(t, `UPDATE view_generations SET resolver_version='wrong-parent-policy' WHERE generation_id=?`, a.GenerationID)
	if _, err := f.c.AdoptDedicatedBaseGeneration(context.Background(), AdoptDedicatedBaseGenerationRequest{b}); !errors.Is(err, ErrDedicatedBaseCandidate) {
		t.Fatalf("incompatible ancestor accepted: %v", err)
	}
	if err := f.c.DeleteDedicatedGraph(context.Background(), f.graph.GraphID); err != nil {
		t.Fatal(err)
	}
	if _, found, err := f.c.DedicatedBasePublication(context.Background(), f.graph.GraphID); err != nil || found {
		t.Fatalf("publication not cascaded: found=%v err=%v", found, err)
	}
	if _, err := f.c.AdoptDedicatedBaseGeneration(context.Background(), AdoptDedicatedBaseGenerationRequest{b}); !errors.Is(err, ErrCatalogStaleGuard) {
		t.Fatalf("deleted graph resurrected: %v", err)
	}
}

func TestDedicatedPublicationCancellation(t *testing.T) {
	f := newDedicatedPublicationFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := f.c.ClaimDedicatedBaseBuild(ctx, ClaimDedicatedBaseBuildRequest{Desire: f.desire, AttemptToken: "canceled"})
	if !errors.Is(err, context.Canceled) || f.count(t) != 0 {
		t.Fatalf("cancel lost: %v count=%d", err, f.count(t))
	}
}

func TestDedicatedPublicationRejectsIncompleteIdentity(t *testing.T) {
	for _, field := range []string{"tree", "config", "extractors", "resolver"} {
		t.Run(field, func(t *testing.T) {
			f := newDedicatedPublicationFixture(t)
			identity := f.desire.Identity
			switch field {
			case "tree":
				identity.TreeOID = ""
			case "config":
				identity.ConfigHash = ""
			case "extractors":
				identity.ExtractorVersions = ""
			case "resolver":
				identity.ResolverVersion = ""
			}
			_, err := f.c.RecordDedicatedBaseDesire(context.Background(), RecordDedicatedBaseDesireRequest{Authority: f.authority, ExpectedDesiredEpoch: f.desire.Epoch, Identity: identity})
			if err == nil {
				t.Fatal("incomplete policy accepted")
			}
			p, _, err := f.c.DedicatedBasePublication(context.Background(), f.graph.GraphID)
			if err != nil || p.Desire != f.desire {
				t.Fatalf("incomplete observation changed desire: %+v err=%v", p, err)
			}
		})
	}
}

func TestDedicatedPublicationBuildingCandidateValidation(t *testing.T) {
	for _, field := range []string{"owner_kind", "generation_kind", "graph_id", "checkout_id", "tree_oid", "config_hash", "extractor_versions", "resolver_version", "layer_id", "lower_view_fingerprint"} {
		t.Run(field, func(t *testing.T) {
			f := newDedicatedPublicationFixture(t)
			claim := f.claim(t, "first", 0, 0)
			// Column names are the static test table above, never user input.
			f.exec(t, "UPDATE view_generations SET "+field+"='incompatible' WHERE generation_id=?", claim.GenerationID)
			_, err := f.c.ClaimDedicatedBaseBuild(context.Background(), ClaimDedicatedBaseBuildRequest{Desire: f.desire, AttemptToken: "follower"})
			if !errors.Is(err, ErrDedicatedBaseCandidate) {
				t.Fatalf("incompatible building %s returned to writer: %v", field, err)
			}
			if f.count(t) != 1 {
				t.Fatal("mismatch allocated another candidate")
			}
		})
	}
	t.Run("unservable_ancestor", func(t *testing.T) {
		f := newDedicatedPublicationFixture(t)
		a := f.claim(t, "a", 0, 0)
		f.publish(t, a)
		f.adopt(t, a)
		next := f.desire.Identity
		next.TreeOID = "next"
		f.observe(t, next)
		f.claim(t, "child", a.GenerationID, a.GenerationID)
		f.exec(t, `UPDATE view_generations SET state='retiring' WHERE generation_id=?`, a.GenerationID)
		_, err := f.c.ClaimDedicatedBaseBuild(context.Background(), ClaimDedicatedBaseBuildRequest{Desire: f.desire, AttemptToken: "follower", ExpectedActiveGenerationID: a.GenerationID})
		if !errors.Is(err, ErrDedicatedBaseCandidate) {
			t.Fatalf("building child with retiring ancestor returned: %v", err)
		}
	})
}

func TestDedicatedPublicationDepthBoundBeforeAllocation(t *testing.T) {
	f := newDedicatedPublicationFixture(t)
	var parent int64
	for range maxDedicatedBaseAncestry {
		g := ViewGeneration{OwnerKind: "dedicated_graph", GraphID: f.graph.GraphID, CheckoutID: f.owner.CheckoutID, GenerationKind: "dedicated", BaseGenerationID: parent, TreeOID: "ancestor", ConfigHash: f.desire.Identity.ConfigHash, ExtractorVersions: f.desire.Identity.ExtractorVersions, ResolverVersion: f.desire.Identity.ResolverVersion, State: ViewGenerationBuilding}
		id, err := f.c.CreateViewGeneration(context.Background(), g)
		if err != nil {
			t.Fatal(err)
		}
		if err = f.c.PublishViewGeneration(context.Background(), id, 1); err != nil {
			t.Fatal(err)
		}
		parent = id
	}
	before := f.count(t)
	_, err := f.c.ClaimDedicatedBaseBuild(context.Background(), ClaimDedicatedBaseBuildRequest{Desire: f.desire, AttemptToken: "too-deep", BaseGenerationID: parent})
	if !errors.Is(err, ErrDedicatedBaseCandidate) || f.count(t) != before {
		t.Fatalf("over-deep child allocated: err=%v count=%d→%d", err, before, f.count(t))
	}
}

func TestDedicatedPublicationDetectsExternalPointerClobber(t *testing.T) {
	f := newDedicatedPublicationFixture(t)
	claim := f.claim(t, "a", 0, 0)
	f.publish(t, claim)
	f.adopt(t, claim)
	// Model external catalog corruption independently of the guarded public
	// identity-upsert path, which must preserve an adopted pointer.
	f.exec(t, `UPDATE dedicated_graphs SET active_generation_id=NULL WHERE graph_id=?`, f.graph.GraphID)
	if _, err := f.c.ClaimDedicatedBaseBuild(context.Background(), ClaimDedicatedBaseBuildRequest{Desire: f.desire, AttemptToken: "retry"}); !errors.Is(err, ErrCatalogStaleGuard) {
		t.Fatalf("clobber disguised by new claim: %v", err)
	}
	if _, err := f.c.AdoptDedicatedBaseGeneration(context.Background(), AdoptDedicatedBaseGenerationRequest{claim}); !errors.Is(err, ErrCatalogStaleGuard) {
		t.Fatalf("clobber disguised by old adoption: %v", err)
	}
}

func TestDedicatedPublicationRecreatedOwnerFloor(t *testing.T) {
	f := newDedicatedPublicationFixture(t)
	old := f.claim(t, "old", 0, 0)
	f.publish(t, old)
	f.adopt(t, old)
	if err := f.c.DeleteDedicatedGraph(context.Background(), f.graph.GraphID); err != nil {
		t.Fatal(err)
	}
	f.owner.Incarnation = "new-incarnation"
	if err := f.c.UpsertCheckout(context.Background(), f.owner); err != nil {
		t.Fatal(err)
	}
	if err := f.c.UpsertDedicatedGraph(context.Background(), f.graph); err != nil {
		t.Fatal(err)
	}
	var err error
	f.authority, err = f.c.AcquireDedicatedBaseAuthority(context.Background(), AcquireDedicatedBaseAuthorityRequest{GraphID: f.graph.GraphID, Owner: DedicatedBaseOwner{f.owner.CheckoutID, f.owner.Incarnation}, Token: "new-owner-authority"})
	if err != nil {
		t.Fatal(err)
	}
	if f.authority.GenerationFloor < old.GenerationID {
		t.Fatalf("floor=%d did not exclude old generation %d", f.authority.GenerationFloor, old.GenerationID)
	}
	f.desire, err = f.c.RecordDedicatedBaseDesire(context.Background(), RecordDedicatedBaseDesireRequest{Authority: f.authority, Identity: old.Desire.Identity})
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.c.ClaimDedicatedBaseBuild(context.Background(), ClaimDedicatedBaseBuildRequest{Desire: f.desire, AttemptToken: "unsafe-parent", BaseGenerationID: old.GenerationID})
	if !errors.Is(err, ErrDedicatedBaseCandidate) {
		t.Fatalf("old incarnation ancestor admitted: %v", err)
	}
	newClaim := f.claim(t, "new-seed", 0, 0)
	if newClaim.GenerationID <= f.authority.GenerationFloor || newClaim.GenerationID == old.GenerationID || newClaim.Status != "allocated" {
		t.Fatalf("old incarnation candidate reused: %+v", newClaim)
	}
	// A newly allocated ID is not sufficient if its ancestry reaches below the
	// owner fence. The test mutates only its private DB to model bad metadata.
	f.exec(t, `UPDATE view_generations SET base_generation_id=? WHERE generation_id=?`, old.GenerationID, newClaim.GenerationID)
	_, err = f.c.ClaimDedicatedBaseBuild(context.Background(), ClaimDedicatedBaseBuildRequest{Desire: f.desire, AttemptToken: "follower"})
	if !errors.Is(err, ErrDedicatedBaseCandidate) {
		t.Fatalf("new candidate inherited pre-owner payload: %v", err)
	}
}

func TestDedicatedPublicationGenerationIDsRemainMonotonicAfterDeleteAndReopen(t *testing.T) {
	f := newDedicatedPublicationFixture(t)
	var schema string
	if err := f.store.db.QueryRow(`SELECT sql FROM sqlite_schema WHERE type='table' AND name='view_generations'`).Scan(&schema); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.ToUpper(schema), "AUTOINCREMENT") {
		t.Fatal("high-water provenance requires durable AUTOINCREMENT IDs")
	}
	first := f.claim(t, "first", 0, 0)
	if err := f.c.FailDedicatedBaseBuild(context.Background(), FailDedicatedBaseBuildRequest{first, "remove private candidate"}); err != nil {
		t.Fatal(err)
	}
	f.exec(t, `DELETE FROM view_generations`)
	if err := f.store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(f.path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	f.store, f.c = reopened, reopened.Catalog()
	second := f.claim(t, "second", 0, 0)
	if second.GenerationID <= first.GenerationID {
		t.Fatalf("generation ID reused after restart: %d→%d", first.GenerationID, second.GenerationID)
	}
}

func TestDedicatedPublicationRejectsGraphBindingChanges(t *testing.T) {
	for _, field := range []string{"namespace", "family"} {
		t.Run(field, func(t *testing.T) {
			f := newDedicatedPublicationFixture(t)
			claim := f.claim(t, "a", 0, 0)
			f.publish(t, claim)
			if field == "namespace" {
				f.exec(t, `UPDATE dedicated_graphs SET repo_prefix='rebound-repo' WHERE graph_id=?`, f.graph.GraphID)
			} else {
				if err := f.c.UpsertRepositoryFamily(context.Background(), RepositoryFamily{FamilyID: "new-family", CommonDirIdentity: filepath.Join(filepath.Dir(f.path), "other-git"), State: "active"}); err != nil {
					t.Fatal(err)
				}
				f.exec(t, `UPDATE checkouts SET family_id='new-family' WHERE checkout_id=?`, f.owner.CheckoutID)
				f.exec(t, `UPDATE dedicated_graphs SET family_id='new-family' WHERE graph_id=?`, f.graph.GraphID)
			}
			if _, err := f.c.AdoptDedicatedBaseGeneration(context.Background(), AdoptDedicatedBaseGenerationRequest{claim}); !errors.Is(err, ErrCatalogStaleGuard) {
				t.Fatalf("old binding adopted: %v", err)
			}
			if _, err := f.c.AcquireDedicatedBaseAuthority(context.Background(), AcquireDedicatedBaseAuthorityRequest{GraphID: f.graph.GraphID, Owner: f.authority.Owner, ExpectedEpoch: f.authority.Epoch, ExpectedToken: f.authority.Token, Token: "takeover"}); !errors.Is(err, ErrCatalogStaleGuard) {
				t.Fatalf("silent binding takeover accepted: %v", err)
			}
		})
	}
}

func BenchmarkDedicatedPublicationUnchangedCycle(b *testing.B) {
	f := newDedicatedPublicationFixture(b)
	claim := f.claim(b, "initial", 0, 0)
	f.publish(b, claim)
	f.adopt(b, claim)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		f.observe(b, f.desire.Identity)
		c := f.claim(b, "ignored", claim.GenerationID, claim.GenerationID)
		f.adopt(b, c)
	}
	b.StopTimer()
	if f.count(b) != 1 {
		b.Fatal("idle cycle allocated generations")
	}
}

func dedicatedIdentityGraph(t testing.TB, f *dedicatedPublicationFixture) DedicatedGraph {
	t.Helper()
	g, found, err := f.c.GetDedicatedGraph(context.Background(), f.graph.GraphID)
	if err != nil || !found {
		t.Fatalf("dedicated graph found=%v err=%v", found, err)
	}
	return g
}

func TestDedicatedIdentityUpsertPreservesPublishedPointer(t *testing.T) {
	f := newDedicatedPublicationFixture(t)
	claim := f.claim(t, "initial", 0, 0)
	f.publish(t, claim)
	f.adopt(t, claim)
	beforePublication, found, err := f.c.DedicatedBasePublication(context.Background(), f.graph.GraphID)
	if err != nil || !found {
		t.Fatal(err)
	}
	for _, active := range []int64{0, claim.GenerationID, claim.GenerationID + 1000} {
		t.Run(fmt.Sprint(active), func(t *testing.T) {
			incoming := f.graph
			incoming.ActiveGenerationID = active
			// Conflicting active ID is intentionally ignored by identity upsert;
			// it is not a failed attempt to adopt and must return nil.
			if err := f.c.UpsertDedicatedGraph(context.Background(), incoming); err != nil {
				t.Fatal(err)
			}
			if got := dedicatedIdentityGraph(t, f); got.ActiveGenerationID != claim.GenerationID {
				t.Fatalf("supplied pointer %d clobbered adopted %d: %+v", active, claim.GenerationID, got)
			}
		})
	}
	afterPublication, _, err := f.c.DedicatedBasePublication(context.Background(), f.graph.GraphID)
	if err != nil || afterPublication != beforePublication {
		t.Fatalf("identity upsert changed publication: before=%+v after=%+v err=%v", beforePublication, afterPublication, err)
	}
	// Legitimate state changes still apply without gaining pointer ownership.
	incoming := f.graph
	incoming.State = "offline"
	incoming.ActiveGenerationID = 0
	if err := f.c.UpsertDedicatedGraph(context.Background(), incoming); err != nil {
		t.Fatal(err)
	}
	if got := dedicatedIdentityGraph(t, f); got.State != "offline" || got.ActiveGenerationID != claim.GenerationID {
		t.Fatalf("state update lost pointer protection: %+v", got)
	}
}

func TestDedicatedIdentityUpsertUnchangedNoWrites(t *testing.T) {
	for _, guarded := range []bool{false, true} {
		t.Run(fmt.Sprint("guarded=", guarded), func(t *testing.T) {
			f := newDedicatedPublicationFixture(t)
			if guarded {
				claim := f.claim(t, "initial", 0, 0)
				f.publish(t, claim)
				f.adopt(t, claim)
			} else {
				if err := f.c.DeleteDedicatedGraph(context.Background(), f.graph.GraphID); err != nil {
					t.Fatal(err)
				}
				if err := f.c.UpsertDedicatedGraph(context.Background(), f.graph); err != nil {
					t.Fatal(err)
				}
			}
			f.exec(t, `CREATE TABLE identity_write_audit(n INTEGER NOT NULL)`)
			f.exec(t, `INSERT INTO identity_write_audit VALUES(0)`)
			f.exec(t, `CREATE TRIGGER identity_write_update AFTER UPDATE ON dedicated_graphs BEGIN UPDATE identity_write_audit SET n=n+1; END`)
			f.exec(t, `CREATE TRIGGER identity_write_insert AFTER INSERT ON dedicated_graphs BEGIN UPDATE identity_write_audit SET n=n+1; END`)
			f.exec(t, `CREATE TRIGGER identity_publication_update AFTER UPDATE ON dedicated_base_publications BEGIN UPDATE identity_write_audit SET n=n+1; END`)
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
			for range 20 {
				if err := f.c.UpsertDedicatedGraph(context.Background(), f.graph); err != nil {
					t.Fatal(err)
				}
			}
			var writes int
			if err := f.store.db.QueryRow(`SELECT n FROM identity_write_audit`).Scan(&writes); err != nil {
				t.Fatal(err)
			}
			if writes != 0 || walSize() != before {
				t.Fatalf("unchanged identity wrote metadata: writes=%d WAL=%d→%d", writes, before, walSize())
			}
		})
	}
}

func TestDedicatedIdentityUpsertCannotActivateUnpublishedReservation(t *testing.T) {
	f := newDedicatedPublicationFixture(t)
	claim := f.claim(t, "building", 0, 0)
	incoming := f.graph
	incoming.ActiveGenerationID = claim.GenerationID
	if err := f.c.UpsertDedicatedGraph(context.Background(), incoming); err != nil {
		t.Fatal(err)
	}
	if got := dedicatedIdentityGraph(t, f); got.ActiveGenerationID != 0 {
		t.Fatalf("identity upsert activated building payload: %+v", got)
	}
	p, found, err := f.c.DedicatedBasePublication(context.Background(), f.graph.GraphID)
	if err != nil || !found || p.AttemptState != "building" || p.Claim.GenerationID != claim.GenerationID {
		t.Fatalf("identity upsert changed reservation: %+v %v", p, err)
	}
}

func TestDedicatedIdentityUpsertRejectsRebindingAtomically(t *testing.T) {
	for _, field := range []string{"owner", "prefix", "family"} {
		t.Run(field, func(t *testing.T) {
			f := newDedicatedPublicationFixture(t)
			claim := f.claim(t, "initial", 0, 0)
			f.publish(t, claim)
			f.adopt(t, claim)
			before := dedicatedIdentityGraph(t, f)
			publication, _, err := f.c.DedicatedBasePublication(context.Background(), f.graph.GraphID)
			if err != nil {
				t.Fatal(err)
			}
			incoming := before
			incoming.State = "offline"
			incoming.ActiveGenerationID = 0
			switch field {
			case "owner":
				incoming.OwnerCheckoutID = "other-owner"
			case "prefix":
				incoming.RepoPrefix = "other-prefix"
			case "family":
				incoming.FamilyID = "other-family"
			}
			err = f.c.UpsertDedicatedGraph(context.Background(), incoming)
			if !errors.Is(err, ErrCatalogStaleGuard) {
				t.Fatalf("rebind error=%v; want stale guard (not foreign-key failure)", err)
			}
			if after := dedicatedIdentityGraph(t, f); after != before {
				t.Fatalf("rejected rebind partly changed graph: before=%+v after=%+v", before, after)
			}
			afterPublication, _, err := f.c.DedicatedBasePublication(context.Background(), f.graph.GraphID)
			if err != nil || afterPublication != publication {
				t.Fatalf("rejected rebind changed publication: %+v %v", afterPublication, err)
			}
		})
	}
}

func TestDedicatedIdentityUpsertLegacyCompatibility(t *testing.T) {
	f := newDedicatedPublicationFixture(t)
	// Removing the graph through the real catalog cascades its authority. Its
	// explicit recreation is a fresh pre-protocol graph, not an implicit rebind.
	if err := f.c.DeleteDedicatedGraph(context.Background(), f.graph.GraphID); err != nil {
		t.Fatal(err)
	}
	// Keep real Ready payloads on an independent non-primary seed graph. The
	// generic identity API admits healthy pointers without adopting authority.
	seedOwner := f.owner
	seedOwner.CheckoutID = "legacy-payload-owner"
	seedOwner.Incarnation = "legacy-payload-incarnation"
	seedOwner.AdminName = "legacy-payload"
	seedOwner.RootPath = filepath.Join(filepath.Dir(f.path), "legacy-payload-root")
	seedOwner.GitDir = filepath.Join(f.owner.GitDir, "worktrees", "legacy-payload")
	if err := f.c.UpsertCheckout(context.Background(), seedOwner); err != nil {
		t.Fatal(err)
	}
	seedGraph := f.graph
	seedGraph.GraphID = "legacy-payload-graph"
	seedGraph.OwnerCheckoutID = seedOwner.CheckoutID
	seedGraph.RepoPrefix = "legacy-payload"
	seedGraph.IsPrimaryBase = false
	if err := f.c.UpsertDedicatedGraph(context.Background(), seedGraph); err != nil {
		t.Fatal(err)
	}
	newReadyGeneration := func(layer string) int64 {
		generationID, _, err := f.store.BeginPayloadGeneration(context.Background(), PayloadGenerationRequest{
			OwnerKind: "dedicated_graph", GraphID: seedGraph.GraphID, CheckoutID: seedOwner.CheckoutID,
			GenerationKind: "commit", LayerID: layer, TreeOID: layer,
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := f.store.PublishPayloadGeneration(context.Background(), generationID, 2); err != nil {
			t.Fatal(err)
		}
		return generationID
	}
	incoming := f.graph
	incoming.ActiveGenerationID = newReadyGeneration("legacy-insert")
	if err := f.c.UpsertDedicatedGraph(context.Background(), incoming); err != nil {
		t.Fatal(err)
	}
	if got := dedicatedIdentityGraph(t, f); got != incoming {
		t.Fatalf("legacy insert=%+v want=%+v", got, incoming)
	}
	if _, found, err := f.c.DedicatedBasePublication(context.Background(), f.graph.GraphID); err != nil || found {
		t.Fatalf("identity insert created publication authority: found=%v err=%v", found, err)
	}
	incoming.ActiveGenerationID = 0
	if err := f.c.UpsertDedicatedGraph(context.Background(), incoming); err != nil {
		t.Fatal(err)
	}
	if got := dedicatedIdentityGraph(t, f); got.ActiveGenerationID != 0 {
		t.Fatalf("legacy active reset no longer works: %+v", got)
	}
	otherFamily := RepositoryFamily{FamilyID: "legacy-family", CommonDirIdentity: filepath.Join(filepath.Dir(f.path), "legacy-git"), State: "active"}
	if err := f.c.UpsertRepositoryFamily(context.Background(), otherFamily); err != nil {
		t.Fatal(err)
	}
	otherOwner := f.owner
	otherOwner.CheckoutID = "legacy-owner"
	otherOwner.Incarnation = "legacy-incarnation"
	otherOwner.FamilyID = otherFamily.FamilyID
	otherOwner.AdminName = "other"
	otherOwner.RootPath = filepath.Join(filepath.Dir(f.path), "legacy-root")
	if err := f.c.UpsertCheckout(context.Background(), otherOwner); err != nil {
		t.Fatal(err)
	}
	incoming.OwnerCheckoutID = otherOwner.CheckoutID
	incoming.FamilyID = otherFamily.FamilyID
	incoming.RepoPrefix = "legacy-prefix"
	incoming.ActiveGenerationID = newReadyGeneration("legacy-update")
	if err := f.c.UpsertDedicatedGraph(context.Background(), incoming); err != nil {
		t.Fatal(err)
	}
	if got := dedicatedIdentityGraph(t, f); got != incoming {
		t.Fatalf("legacy identity upsert changed behavior: %+v want=%+v", got, incoming)
	}
}

func TestDedicatedIdentityUpsertConcurrentAdoption(t *testing.T) {
	f := newDedicatedPublicationFixture(t)
	claim := f.claim(t, "initial", 0, 0)
	f.publish(t, claim)
	const n = 20
	start := make(chan struct{})
	errs := make(chan error, n+1)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		_, err := f.c.AdoptDedicatedBaseGeneration(context.Background(), AdoptDedicatedBaseGenerationRequest{claim})
		errs <- err
	}()
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			errs <- f.c.UpsertDedicatedGraph(context.Background(), f.graph)
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Error(err)
		}
	}
	if got := dedicatedIdentityGraph(t, f); got.ActiveGenerationID != claim.GenerationID {
		t.Fatalf("concurrent upsert clobbered adoption: %+v", got)
	}
	p, found, err := f.c.DedicatedBasePublication(context.Background(), f.graph.GraphID)
	if err != nil || !found || p.AttemptState != "adopted" || p.Claim.GenerationID != claim.GenerationID {
		t.Fatalf("adoption not coherent: %+v err=%v", p, err)
	}
}

func TestDedicatedIdentityUpsertInputAndCancellation(t *testing.T) {
	f := newDedicatedPublicationFixture(t)
	before := dedicatedIdentityGraph(t, f)
	invalid := f.graph
	invalid.GraphID = ""
	if err := f.c.UpsertDedicatedGraph(context.Background(), invalid); err == nil {
		t.Fatal("invalid graph accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := f.c.UpsertDedicatedGraph(ctx, f.graph); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation lost: %v", err)
	}
	if got := dedicatedIdentityGraph(t, f); got != before {
		t.Fatalf("failed request changed graph: %+v", got)
	}
}

func BenchmarkDedicatedIdentityUpsertUnchanged(b *testing.B) {
	for _, guarded := range []bool{false, true} {
		b.Run(fmt.Sprint("guarded=", guarded), func(b *testing.B) {
			f := newDedicatedPublicationFixture(b)
			if guarded {
				claim := f.claim(b, "initial", 0, 0)
				f.publish(b, claim)
				f.adopt(b, claim)
			} else {
				if err := f.c.DeleteDedicatedGraph(context.Background(), f.graph.GraphID); err != nil {
					b.Fatal(err)
				}
				if err := f.c.UpsertDedicatedGraph(context.Background(), f.graph); err != nil {
					b.Fatal(err)
				}
			}
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				if err := f.c.UpsertDedicatedGraph(b.Context(), f.graph); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func TestDedicatedBasePublicationMigrationRegistration(t *testing.T) {
	if currentSchemaVersion < 22 {
		t.Fatalf("schema=%d, want dedicated-publication schema22", currentSchemaVersion)
	}
	found := false
	for _, migration := range schemaMigrations {
		if migration.version == 22 {
			found = true
			if migration.inPlace == nil || migration.rebuild || migration.name != "add dedicated base publication intent" {
				t.Fatalf("v22 must be the registered additive publication migration: %+v", migration)
			}
		}
	}
	if !found || !strings.Contains(schemaSQL, dedicatedBasePublicationsSchemaSQL) {
		t.Fatal("publication migration must be registered and share canonical cold DDL")
	}
	parent := strings.Index(schemaSQL, "dedicated_graphs")
	child := strings.Index(schemaSQL, "dedicated_base_publications")
	if parent < 0 || child <= parent {
		t.Fatalf("publication DDL must follow its dedicated_graphs parent: parent=%d child=%d", parent, child)
	}
}

func TestDedicatedBasePublicationSchemaFreshOpen(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "fresh.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	assertDedicatedPublicationSchemaVersion(t, s.db)
	if _, found, err := s.Catalog().DedicatedBasePublication(context.Background(), "absent"); err != nil || found {
		t.Fatalf("fresh catalog lookup found=%v err=%v", found, err)
	}
	var foreignKeys int
	if err := s.db.QueryRow(`SELECT count(*) FROM pragma_foreign_key_list('dedicated_base_publications') WHERE "table"='dedicated_graphs' AND "from"='graph_id' AND "to"='graph_id' AND on_delete='CASCADE'`).Scan(&foreignKeys); err != nil || foreignKeys != 1 {
		t.Fatalf("dedicated graph cascade FK count=%d err=%v", foreignKeys, err)
	}
	var generations int
	if err := s.db.QueryRow(`SELECT count(*) FROM view_generations`).Scan(&generations); err != nil || generations != 0 {
		t.Fatalf("migration must not allocate payload generations: count=%d err=%v", generations, err)
	}
}

func assertDedicatedPublicationSchemaVersion(t testing.TB, db *sql.DB) {
	t.Helper()
	var version int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != currentSchemaVersion {
		t.Fatalf("user_version=%d want%d err=%v", version, currentSchemaVersion, err)
	}
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM dedicated_base_publications`).Scan(&count); err != nil {
		t.Fatal(err)
	}
}

func dedicatedPublicationV21Fixture(t *testing.T) (*dedicatedPublicationFixture, DedicatedBaseBuildClaim, *graph.Node) {
	t.Helper()
	f := newDedicatedPublicationFixture(t)
	claim := f.claim(t, "before-migration", 0, 0)
	f.publish(t, claim)
	f.adopt(t, claim)
	node := &graph.Node{ID: "repo/keep.go::Keep", Name: "Keep", Kind: graph.KindFunction, FilePath: "repo/keep.go", RepoPrefix: "repo", Language: "go", StartLine: 7}
	f.store.AddBatch([]*graph.Node{node}, nil)
	// Remove only the new companion from this disposable fixture to model a
	// populated pre-protocol v21 store. Existing graph/payload pointers remain.
	f.exec(t, `DROP TABLE dedicated_base_publications`)
	f.exec(t, `PRAGMA user_version=21`)
	if err := f.store.Close(); err != nil {
		t.Fatal(err)
	}
	return f, claim, node
}

func TestDedicatedBasePublicationSchemaV21UpgradePreservesRows(t *testing.T) {
	f, claim, node := dedicatedPublicationV21Fixture(t)
	reopened, err := Open(f.path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	assertDedicatedPublicationSchemaVersion(t, reopened.db)
	g, found, err := reopened.Catalog().GetDedicatedGraph(context.Background(), f.graph.GraphID)
	if err != nil || !found || g.ActiveGenerationID != claim.GenerationID || g.OwnerCheckoutID != f.owner.CheckoutID {
		t.Fatalf("upgrade changed graph/active pointer: %+v found=%v err=%v", g, found, err)
	}
	if got := reopened.GetNode(node.ID); got == nil || got.StartLine != node.StartLine || got.Name != node.Name {
		t.Fatalf("upgrade changed payload node: %+v", got)
	}
	var generations, publications int
	if err := reopened.db.QueryRow(`SELECT count(*) FROM view_generations`).Scan(&generations); err != nil {
		t.Fatal(err)
	}
	if err := reopened.db.QueryRow(`SELECT count(*) FROM dedicated_base_publications`).Scan(&publications); err != nil || publications != 0 || generations != 1 {
		t.Fatalf("upgrade allocated protocol or payload state: publications=%d generations=%d err=%v", publications, generations, err)
	}
}

func TestDedicatedBasePublicationMigrationTransactionRollbackAndReplay(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "migration.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`PRAGMA foreign_keys=ON; CREATE TABLE dedicated_graphs(graph_id TEXT PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}
	step := schemaMigration{version: 22, name: "add dedicated base publication intent", inPlace: createDedicatedBasePublicationsTable}
	injected := errors.New("injected migration failure")
	err = applyInPlaceMigrations(db, []schemaMigration{step, {version: 23, name: "test failure", inPlace: func(*sql.Tx) error { return injected }}})
	if !errors.Is(err, injected) {
		t.Fatalf("migration failure lost: %v", err)
	}
	var exists int
	if err := db.QueryRow(`SELECT count(*) FROM sqlite_schema WHERE name='dedicated_base_publications'`).Scan(&exists); err != nil || exists != 0 {
		t.Fatalf("failed transactional migration retained table: exists=%d err=%v", exists, err)
	}
	if err := applyInPlaceMigrations(db, []schemaMigration{step}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO dedicated_graphs VALUES('graph'); INSERT INTO dedicated_base_publications(graph_id,owner_checkout_id,owner_incarnation,owner_generation_floor,repo_prefix,family_id,authority_epoch,authority_token) VALUES('graph','owner','incarnation',0,'repo','family',1,'authority')`); err != nil {
		t.Fatal(err)
	}
	if err := applyInPlaceMigrations(db, []schemaMigration{step}); err != nil {
		t.Fatal(err)
	}
	var token string
	if err := db.QueryRow(`SELECT authority_token FROM dedicated_base_publications WHERE graph_id='graph'`).Scan(&token); err != nil || token != "authority" {
		t.Fatalf("idempotent migration changed authority: token=%q err=%v", token, err)
	}
	if _, err := db.Exec(`DELETE FROM dedicated_graphs WHERE graph_id='graph'`); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM dedicated_base_publications`).Scan(&exists); err != nil || exists != 0 {
		t.Fatalf("graph deletion did not cascade: count=%d err=%v", exists, err)
	}
}

func TestDedicatedBasePublicationSchemaWarmReopenPreservesAuthority(t *testing.T) {
	f := newDedicatedPublicationFixture(t)
	claim := f.claim(t, "before-reopen", 0, 0)
	f.publish(t, claim)
	f.adopt(t, claim)
	before, found, err := f.c.DedicatedBasePublication(context.Background(), f.graph.GraphID)
	if err != nil || !found {
		t.Fatalf("publication found=%v err=%v", found, err)
	}
	f.exec(t, `CREATE TABLE reopen_publication_audit(n INTEGER NOT NULL); INSERT INTO reopen_publication_audit VALUES(0)`)
	f.exec(t, `CREATE TRIGGER reopen_publication_update AFTER UPDATE ON dedicated_base_publications BEGIN UPDATE reopen_publication_audit SET n=n+1; END`)
	f.exec(t, `CREATE TRIGGER reopen_graph_update AFTER UPDATE OF active_generation_id ON dedicated_graphs BEGIN UPDATE reopen_publication_audit SET n=n+1; END`)
	if err := f.store.Close(); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		reopened, err := Open(f.path)
		if err != nil {
			t.Fatal(err)
		}
		assertDedicatedPublicationSchemaVersion(t, reopened.db)
		after, found, err := reopened.Catalog().DedicatedBasePublication(context.Background(), f.graph.GraphID)
		if err != nil || !found || after != before {
			_ = reopened.Close()
			t.Fatalf("warm reopen changed publication: before=%+v after=%+v found=%v err=%v", before, after, found, err)
		}
		var writes int
		err = reopened.db.QueryRow(`SELECT n FROM reopen_publication_audit`).Scan(&writes)
		closeErr := reopened.Close()
		if err != nil || closeErr != nil || writes != 0 {
			t.Fatalf("warm reopen publication writes=%d err=%v close=%v", writes, err, closeErr)
		}
	}
}

func TestDedicatedBasePublicationMigrationOpenRetryBeforeStamp(t *testing.T) {
	f, claim, node := dedicatedPublicationV21Fixture(t)
	injected := errors.New("injected publication migration failure")
	steps := make([]schemaMigration, 0, len(schemaMigrations)+1)
	for _, step := range schemaMigrations {
		if step.version < 22 {
			steps = append(steps, step)
		}
	}
	steps = append(steps, schemaMigration{version: 22, name: "test publication failure", inPlace: func(tx *sql.Tx) error {
		if err := createDedicatedBasePublicationsTable(tx); err != nil {
			return err
		}
		return injected
	}})
	opened, err := openWithObserver(f.path, 22, steps, false, nil)
	if opened != nil {
		_ = opened.Close()
		t.Fatal("failed migration returned a store")
	}
	if !errors.Is(err, injected) {
		t.Fatalf("wrong startup failure: %v", err)
	}
	probe, err := sql.Open("sqlite", f.path)
	if err != nil {
		t.Fatal(err)
	}
	var version, companion int
	if err := probe.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != 21 {
		_ = probe.Close()
		t.Fatalf("failed open stamped version=%d err=%v", version, err)
	}
	err = probe.QueryRow(`SELECT count(*) FROM sqlite_schema WHERE name='dedicated_base_publications'`).Scan(&companion)
	_ = probe.Close()
	if err != nil || companion != 1 {
		t.Fatalf("canonical cold DDL must already have created the empty companion: count=%d err=%v", companion, err)
	}
	reopened, err := Open(f.path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	assertDedicatedPublicationSchemaVersion(t, reopened.db)
	g, _, err := reopened.Catalog().GetDedicatedGraph(context.Background(), f.graph.GraphID)
	if err != nil || g.ActiveGenerationID != claim.GenerationID || reopened.GetNode(node.ID) == nil {
		t.Fatalf("retry lost existing state: graph=%+v err=%v", g, err)
	}
}

func TestDedicatedBasePublicationMigrationFutureStoreRefusedBeforeDDL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "future.sqlite")
	futureVersion := max(23, currentSchemaVersion+1)
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(fmt.Sprintf(`CREATE TABLE future_sentinel(value TEXT); INSERT INTO future_sentinel VALUES('preserve'); PRAGMA user_version=%d`, futureVersion)); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	_ = db.Close()
	opened, err := Open(path)
	if opened != nil {
		_ = opened.Close()
		t.Fatal("future store was opened")
	}
	if err == nil {
		t.Fatal("future schema was not rejected")
	}
	db, err = sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	var value string
	var version, companion int
	if err := db.QueryRow(`SELECT value FROM future_sentinel`).Scan(&value); err != nil || value != "preserve" {
		t.Fatalf("future sentinel changed: value=%q err=%v", value, err)
	}
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != futureVersion {
		t.Fatalf("future version changed: version=%d err=%v", version, err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM sqlite_schema WHERE name='dedicated_base_publications'`).Scan(&companion); err != nil || companion != 0 {
		t.Fatalf("future refusal ran publication DDL: table=%d err=%v", companion, err)
	}
}
