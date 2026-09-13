package store_sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
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
	f.graph = DedicatedGraph{GraphID: "graph", OwnerCheckoutID: f.owner.CheckoutID, RepoPrefix: "repo", FamilyID: family.FamilyID, IsPrimaryBase: true, State: DedicatedGraphReady}
	if err = f.c.UpsertDedicatedGraph(ctx, f.graph); err != nil {
		t.Fatal(err)
	}
	f.authority, err = f.c.AcquireDedicatedBaseAuthority(ctx, AcquireDedicatedBaseAuthorityRequest{GraphID: f.graph.GraphID, Owner: DedicatedBaseOwner{f.owner.CheckoutID, f.owner.Incarnation}, Token: "authority-1"})
	if err != nil {
		t.Fatal(err)
	}
	f.desire, err = f.c.RecordDedicatedBaseDesire(ctx, RecordDedicatedBaseDesireRequest{Authority: f.authority, Identity: DedicatedBaseIdentity{TreeOID: "tree-a", ConfigHash: "config", ExtractorVersions: "extractors", ResolverVersion: "resolver"}})
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
		{"generation_ready_is_not_graph_ready", `UPDATE dedicated_graphs SET state='ready' WHERE graph_id='graph'`, nil},
		{"graph_closing", `UPDATE dedicated_graphs SET state='closing' WHERE graph_id='graph'`, nil},
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

// ownerHead reads the owner checkout's recorded committed head.
func (f *dedicatedPublicationFixture) ownerHead(t testing.TB) Checkout {
	t.Helper()
	row, found, err := f.c.GetCheckout(context.Background(), f.owner.CheckoutID)
	if err != nil || !found {
		t.Fatalf("owner checkout found=%v err=%v", found, err)
	}
	return row
}

// claimAt is the fixture's claim with a stated commit provenance and
// observation clock — what a live publisher supplies, and what the head
// advance is fenced on.
func (f *dedicatedPublicationFixture) claimAt(t testing.TB, token, commitOID string, createdAt, active int64) DedicatedBaseBuildClaim {
	t.Helper()
	claim, err := f.c.ClaimDedicatedBaseBuild(context.Background(), ClaimDedicatedBaseBuildRequest{
		Desire: f.desire, AttemptToken: token, ProvenanceCommitOID: commitOID, CreatedAt: createdAt,
		ExpectedActiveGenerationID: active,
	})
	if err != nil {
		t.Fatal(err)
	}
	return claim
}

// TestDedicatedBaseAdoptionAdvancesTheOwnerHead pins the two identities of one
// base moving together.
//
// dedicated_graphs.active_generation_id and checkouts.head_tree both name the
// committed state a family's base is at, and until the adoption wrote the
// second one only a reconciliation pass did — up to an hour later by default.
// In that window graphBase's unpublished fallback, which reads the checkout row,
// named a tree the base had already left, and every layer built over it carried
// that stale identity. They now stand or fall in one transaction.
func TestDedicatedBaseAdoptionAdvancesTheOwnerHead(t *testing.T) {
	f := newDedicatedPublicationFixture(t)
	before := f.ownerHead(t)
	if before.HeadTree == "tree-a" {
		t.Fatal("the fixture must start with an owner head that is not the published tree")
	}

	claim := f.claimAt(t, "attempt-a", "commit-a", 1000, 0)
	f.publish(t, claim)
	adoption := f.adopt(t, claim)

	if !adoption.HeadAdvanced || adoption.TreeOID != "tree-a" || adoption.CommitOID != "commit-a" {
		t.Fatalf("adoption = %+v, want the owner head advanced to the adopted tree", adoption)
	}
	if adoption.PreviousGenerationID != 0 || adoption.GenerationID != claim.GenerationID {
		t.Fatalf("adoption pointers = %+v", adoption)
	}
	owner := f.ownerHead(t)
	if owner.HeadTree != "tree-a" || owner.HeadCommit != "commit-a" {
		t.Fatalf("owner head = %q/%q, want tree-a/commit-a", owner.HeadTree, owner.HeadCommit)
	}
	if owner.LastSeen != 1000 {
		t.Fatalf("owner observation clock = %d, want the adoption's 1000", owner.LastSeen)
	}
	// The identity columns are as untouched by an adoption as they are by an
	// observation: it states what the base is at, it never re-keys the row.
	if owner.Incarnation != before.Incarnation || owner.AdminName != before.AdminName ||
		owner.State != before.State || owner.EffectiveMode != before.EffectiveMode {
		t.Fatalf("the adoption changed the owner's identity or mode: %+v", owner)
	}

	// A replay of the pointer that is already installed changes nothing.
	replay := f.adopt(t, claim)
	if !replay.AlreadyAdopted || replay.HeadAdvanced {
		t.Fatalf("replay = %+v, want an already-adopted no-op", replay)
	}
}

// TestDedicatedBaseAdoptionHeadAdvanceIsAtomicWithThePointer proves the two
// writes are one transaction: an adoption whose active-pointer compare-and-set
// is refused leaves the owner's head exactly where it was, so no checkout row
// ever advertises a base that was never installed.
func TestDedicatedBaseAdoptionHeadAdvanceIsAtomicWithThePointer(t *testing.T) {
	f := newDedicatedPublicationFixture(t)
	before := f.ownerHead(t)
	claim := f.claimAt(t, "attempt-a", "commit-a", 1000, 0)
	f.publish(t, claim)

	// Another actor moved the pointer between this attempt's claim and its
	// adoption. The adoption is refused as stale.
	f.exec(t, `UPDATE dedicated_graphs SET active_generation_id=987654 WHERE graph_id=?`, f.graph.GraphID)
	_, err := f.c.AdoptDedicatedBaseGeneration(context.Background(), AdoptDedicatedBaseGenerationRequest{claim})
	if err == nil {
		t.Fatal("a clobbered pointer adopted anyway")
	}
	if owner := f.ownerHead(t); owner.HeadTree != before.HeadTree || owner.HeadCommit != before.HeadCommit {
		t.Fatalf("a refused adoption still advanced the owner head to %q/%q", owner.HeadTree, owner.HeadCommit)
	}
}

// TestDedicatedBaseAdoptionCannotRewindALaterObservation is the fence in the
// other direction.
//
// The publisher observes a tree, builds it, and adopts minutes later. If the
// family committed again in between, a reconciliation pass has already recorded
// the newer head, and the adoption must not restore the one it was built for —
// the base is still published and still adopted, the checkout row is simply
// ahead of it, which is the truth about a family that kept moving.
func TestDedicatedBaseAdoptionCannotRewindALaterObservation(t *testing.T) {
	f := newDedicatedPublicationFixture(t)
	claim := f.claimAt(t, "attempt-a", "commit-a", 1000, 0)
	f.publish(t, claim)

	err := f.c.UpdateCheckoutObservation(context.Background(), UpdateCheckoutObservationRequest{
		CheckoutID: f.owner.CheckoutID, Incarnation: f.owner.Incarnation, State: CheckoutStateReady,
		RootPath: f.owner.RootPath, GitDir: f.owner.GitDir,
		HeadRef: "refs/heads/main", HeadCommit: "commit-b", HeadTree: "tree-b",
		LastAccessible: 2000, LastSeen: 2000,
	})
	if err != nil {
		t.Fatalf("record the later observation: %v", err)
	}

	adoption := f.adopt(t, claim)
	if adoption.GenerationID != claim.GenerationID || adoption.AlreadyAdopted {
		t.Fatalf("adoption = %+v, want the generation adopted", adoption)
	}
	if adoption.HeadAdvanced {
		t.Fatal("the adoption rewound a head a later observation had already moved")
	}
	if f.active(t) != claim.GenerationID {
		t.Fatal("the active pointer did not move")
	}
	owner := f.ownerHead(t)
	if owner.HeadTree != "tree-b" || owner.HeadCommit != "commit-b" || owner.LastSeen != 2000 {
		t.Fatalf("owner head = %q/%q@%d, want the later observation intact", owner.HeadTree, owner.HeadCommit, owner.LastSeen)
	}
}

// TestDedicatedBaseAdoptionAnnouncesToObservers pins the reach a published base
// has to the layer above it.
//
// The publisher that adopts and the lifecycle that runs the dependents' build
// loops share nothing but this database, and a dependent that is not told its
// base moved finds out on its own poll at best. The announcement carries the
// fact and nothing else, it is delivered after the transaction commits, and it
// reaches an observer registered through any handle over the same store.
func TestDedicatedBaseAdoptionAnnouncesToObservers(t *testing.T) {
	f := newDedicatedPublicationFixture(t)
	var mu sync.Mutex
	var events []DedicatedBaseAdoptionEvent
	// Registered through a SECOND catalog handle: Catalog() mints a new one per
	// call, so an observer bound to the handle rather than to the database would
	// never see the publisher's adoptions.
	release := f.store.Catalog().ObserveDedicatedBaseAdoptions(func(event DedicatedBaseAdoptionEvent) {
		mu.Lock()
		events = append(events, event)
		mu.Unlock()
	})

	claim := f.claimAt(t, "attempt-a", "commit-a", 1000, 0)
	f.publish(t, claim)
	f.adopt(t, claim)

	mu.Lock()
	seen := slices.Clone(events)
	mu.Unlock()
	if len(seen) != 1 {
		t.Fatalf("adoption announced %d events, want 1", len(seen))
	}
	event := seen[0]
	if !event.Advanced() {
		t.Fatalf("event = %+v, want an advance", event)
	}
	if event.GraphID != f.graph.GraphID || event.FamilyID != f.graph.FamilyID ||
		event.RepoPrefix != f.graph.RepoPrefix || event.Owner.CheckoutID != f.owner.CheckoutID {
		t.Fatalf("event names %+v, want the adopted graph's family and owner", event)
	}
	if event.Adoption.GenerationID != claim.GenerationID || event.Adoption.TreeOID != "tree-a" ||
		event.Adoption.PreviousGenerationID != 0 {
		t.Fatalf("event adoption = %+v", event.Adoption)
	}
	// The observer sees a committed state: the pointer it is told about is
	// already readable when it is told.
	if f.active(t) != event.Adoption.GenerationID {
		t.Fatal("the announcement preceded the commit it describes")
	}

	// A replay is announced as what it is, and nothing takes it for an advance.
	f.adopt(t, claim)
	mu.Lock()
	seen = slices.Clone(events)
	mu.Unlock()
	if len(seen) != 2 || seen[1].Advanced() {
		t.Fatalf("replay announced %d events, last advanced=%v", len(seen), seen[len(seen)-1].Advanced())
	}

	release()
	release() // idempotent

	// Releasing the last observer takes the database's registry out of the map
	// rather than leaving an empty one behind for the life of the process, so
	// the next registration has to work over a registry that was detached.
	var laterMu sync.Mutex
	var later []DedicatedBaseAdoptionEvent
	defer f.store.Catalog().ObserveDedicatedBaseAdoptions(func(event DedicatedBaseAdoptionEvent) {
		laterMu.Lock()
		later = append(later, event)
		laterMu.Unlock()
	})()

	f.observe(t, DedicatedBaseIdentity{TreeOID: "tree-c", ConfigHash: "config", ExtractorVersions: "extractors", ResolverVersion: "resolver"})
	next := f.claimAt(t, "attempt-b", "commit-c", 3000, claim.GenerationID)
	f.publish(t, next)
	f.adopt(t, next)
	mu.Lock()
	after := len(events)
	mu.Unlock()
	if after != 2 {
		t.Fatalf("a released observer received %d events, want 2", after)
	}
	laterMu.Lock()
	defer laterMu.Unlock()
	if len(later) != 1 || later[0].Adoption.GenerationID != next.GenerationID || !later[0].Advanced() {
		t.Fatalf("the observer registered after the last release saw %+v", later)
	}
}

func (f *dedicatedPublicationFixture) state(t testing.TB, generationID int64) ViewGenerationState {
	t.Helper()
	row, found, err := f.c.GetViewGeneration(context.Background(), generationID)
	if err != nil || !found {
		t.Fatalf("generation %d found=%v err=%v", generationID, found, err)
	}
	return row.State
}

// TestDedicatedBaseAdoptionSupersedesTheChainItReplaced is the label the
// retirement enumeration reads. Before it existed, adoption repointed
// active_generation_id and left the head it displaced saying "ready", so a
// chain replaced by a new full root was reachable by no sweep and stayed in the
// database for the life of the installation.
func TestDedicatedBaseAdoptionSupersedesTheChainItReplaced(t *testing.T) {
	f := newDedicatedPublicationFixture(t)
	a := f.claim(t, "attempt-a", 0, 0)
	f.publish(t, a)
	if got := f.adopt(t, a); got.PreviousSuperseded {
		t.Fatalf("a first adoption replaced nothing: %+v", got)
	}

	next := f.desire.Identity
	next.TreeOID = "tree-b"
	f.observe(t, next)
	// A new full root: nothing under the adopted generation, so the head it
	// displaces is genuinely replaced rather than extended.
	b := f.claim(t, "attempt-b", a.GenerationID, 0)
	if b.BaseGenerationID != 0 {
		t.Fatalf("claim = %+v, want a full root", b)
	}
	f.publish(t, b)
	adoption := f.adopt(t, b)
	if !adoption.PreviousSuperseded || adoption.PreviousGenerationID != a.GenerationID {
		t.Fatalf("adoption = %+v, want the replaced head labelled", adoption)
	}
	if got := f.state(t, a.GenerationID); got != ViewGenerationSuperseded {
		t.Fatalf("replaced head state = %s, want superseded", got)
	}
	if got := f.state(t, b.GenerationID); got != ViewGenerationReady {
		t.Fatalf("adopted head state = %s, want ready", got)
	}
	// The label is not retirement: a superseded base is still a servable chain
	// member and still a reuse candidate, which is what makes a revert cheap.
	f.observe(t, DedicatedBaseIdentity{TreeOID: "tree-a", ConfigHash: "config", ExtractorVersions: "extractors", ResolverVersion: "resolver"})
	if reused := f.claim(t, "attempt-a-again", b.GenerationID, b.GenerationID); reused.GenerationID != a.GenerationID || reused.Status != "ready" {
		t.Fatalf("superseded reuse = %+v, want the replaced head offered back", reused)
	}
}

// TestDedicatedBaseAdoptionRetainsTheAncestorItExtends is the other half. The
// ordinary advance is a delta whose base IS the previous head: that generation
// is composed into every view the new head serves, so calling it superseded
// would be a false statement about a generation that is still being read, and
// would put the whole live ancestry in front of the sweep on every pass.
func TestDedicatedBaseAdoptionRetainsTheAncestorItExtends(t *testing.T) {
	f := newDedicatedPublicationFixture(t)
	a := f.claim(t, "attempt-a", 0, 0)
	f.publish(t, a)
	f.adopt(t, a)

	next := f.desire.Identity
	next.TreeOID = "tree-b"
	f.observe(t, next)
	b := f.claim(t, "attempt-b", a.GenerationID, a.GenerationID)
	if b.BaseGenerationID != a.GenerationID {
		t.Fatalf("claim = %+v, want a delta over the live head", b)
	}
	f.publish(t, b)
	adoption := f.adopt(t, b)
	if adoption.PreviousSuperseded {
		t.Fatalf("adoption = %+v, want the extended ancestor left alone", adoption)
	}
	if got := f.state(t, a.GenerationID); got != ViewGenerationReady {
		t.Fatalf("extended ancestor state = %s, want ready", got)
	}
}

// TestDedicatedBaseAdoptionSupersedesAHeadARevertMovedOff covers the second way
// a chain is replaced: the tree goes back to one an older candidate already
// holds, adoption reuses it, and the head the family moved off is no longer on
// any chain the active pointer names — even though the generation under it
// still is.
func TestDedicatedBaseAdoptionSupersedesAHeadARevertMovedOff(t *testing.T) {
	f := newDedicatedPublicationFixture(t)
	aIdentity := f.desire.Identity
	a := f.claim(t, "attempt-a", 0, 0)
	f.publish(t, a)
	f.adopt(t, a)

	bIdentity := aIdentity
	bIdentity.TreeOID = "tree-b"
	f.observe(t, bIdentity)
	b := f.claim(t, "attempt-b", a.GenerationID, a.GenerationID)
	f.publish(t, b)
	f.adopt(t, b)

	f.observe(t, aIdentity)
	reused := f.claim(t, "attempt-a-again", b.GenerationID, b.GenerationID)
	if reused.GenerationID != a.GenerationID || reused.Status != "ready" {
		t.Fatalf("revert reuse = %+v, want the historical root", reused)
	}
	adoption := f.adopt(t, reused)
	if !adoption.PreviousSuperseded || adoption.PreviousGenerationID != b.GenerationID {
		t.Fatalf("adoption = %+v, want the head the revert moved off labelled", adoption)
	}
	if got := f.state(t, b.GenerationID); got != ViewGenerationSuperseded {
		t.Fatalf("abandoned head state = %s, want superseded", got)
	}
	if got := f.state(t, a.GenerationID); got != ViewGenerationReady {
		t.Fatalf("re-adopted head state = %s, want ready", got)
	}
}

// TestDedicatedBaseAdoptionCannotRelabelAGenerationItDoesNotOwn pins the
// guarded compare-and-set. The label is written on the full dedicated identity,
// so a row this graph and owner do not own is left exactly as it is and the
// adoption reports that it relabelled nothing rather than failing.
func TestDedicatedBaseAdoptionCannotRelabelAGenerationItDoesNotOwn(t *testing.T) {
	f := newDedicatedPublicationFixture(t)
	a := f.claim(t, "attempt-a", 0, 0)
	f.publish(t, a)
	f.adopt(t, a)

	next := f.desire.Identity
	next.TreeOID = "tree-b"
	f.observe(t, next)
	b := f.claim(t, "attempt-b", a.GenerationID, 0)
	f.publish(t, b)
	f.exec(t, `UPDATE view_generations SET graph_id=? WHERE generation_id=?`, "foreign-graph", a.GenerationID)

	adoption := f.adopt(t, b)
	if adoption.PreviousSuperseded {
		t.Fatalf("adoption = %+v, want a foreign row left alone", adoption)
	}
	if got := f.state(t, a.GenerationID); got != ViewGenerationReady {
		t.Fatalf("foreign row state = %s, want ready", got)
	}
	if f.active(t) != b.GenerationID {
		t.Fatal("the adoption itself must still have installed the active pointer")
	}
}

// TestDedicatedBaseAdoptionWalksAChainThroughAFullRoot pins the ancestry walk
// against the column it reads. base_generation_id is nullable and a full root
// stores NULL rather than 0, so a walk that reached a root through a delta —
// which is exactly what a revert onto a published-but-not-yet-adopted delta
// does — has to read it the way every other reader does.
func TestDedicatedBaseAdoptionWalksAChainThroughAFullRoot(t *testing.T) {
	f := newDedicatedPublicationFixture(t)
	aIdentity := f.desire.Identity
	a := f.claim(t, "attempt-a", 0, 0)
	f.publish(t, a)
	f.adopt(t, a)

	bIdentity := aIdentity
	bIdentity.TreeOID = "tree-b"
	f.observe(t, bIdentity)
	b := f.claim(t, "attempt-b", a.GenerationID, a.GenerationID)
	if b.BaseGenerationID != a.GenerationID {
		t.Fatalf("claim = %+v, want a delta over the root", b)
	}
	f.publish(t, b)
	f.adopt(t, b)

	// A new full root replaces the b-over-a chain; b is labelled, a stays on
	// b's ancestry and keeps its label.
	cIdentity := aIdentity
	cIdentity.TreeOID = "tree-c"
	f.observe(t, cIdentity)
	c := f.claim(t, "attempt-c", b.GenerationID, 0)
	if c.BaseGenerationID != 0 {
		t.Fatalf("claim = %+v, want a full root", c)
	}
	f.publish(t, c)
	if adoption := f.adopt(t, c); !adoption.PreviousSuperseded {
		t.Fatalf("adoption = %+v, want the replaced delta labelled", adoption)
	}

	// Back to tree-b: the reuse lookup returns the superseded delta, whose own
	// base is the root. The previous head (c) is not on that chain, so the walk
	// has to traverse a delta into a NULL-based root before it can say so.
	f.observe(t, bIdentity)
	reused := f.claim(t, "attempt-b-again", c.GenerationID, c.GenerationID)
	if reused.GenerationID != b.GenerationID || reused.BaseGenerationID != a.GenerationID {
		t.Fatalf("revert reuse = %+v, want the superseded delta over the root", reused)
	}
	adoption := f.adopt(t, reused)
	if !adoption.PreviousSuperseded || adoption.PreviousGenerationID != c.GenerationID {
		t.Fatalf("adoption = %+v, want the abandoned root labelled", adoption)
	}
	if got := f.state(t, c.GenerationID); got != ViewGenerationSuperseded {
		t.Fatalf("abandoned root state = %s, want superseded", got)
	}
	if got := f.state(t, a.GenerationID); got != ViewGenerationReady {
		t.Fatalf("live ancestry state = %s, want ready", got)
	}
	if !adoption.AdoptedRestored {
		t.Fatalf("adoption = %+v, want the re-adopted delta's own label cleared", adoption)
	}
	if got := f.state(t, b.GenerationID); got != ViewGenerationReady {
		t.Fatalf("re-adopted delta state = %s, want ready: a live head must not say it was replaced", got)
	}
}

// TestDedicatedBaseAdoptionClearsTheLabelOffTheHeadItReinstates is the mirror
// write. A revert re-adopts a generation an earlier adoption labelled
// superseded — the reuse lookup keeps offering those on purpose — and without
// this the catalog would say "superseded" about the row
// dedicated_graphs.active_generation_id names: a false statement every status
// surface renders, and one that also hides a live head from the retirement
// sweep's ready-only deleted-graph cohort.
func TestDedicatedBaseAdoptionClearsTheLabelOffTheHeadItReinstates(t *testing.T) {
	f := newDedicatedPublicationFixture(t)
	aIdentity := f.desire.Identity
	a := f.claim(t, "attempt-a", 0, 0)
	f.publish(t, a)
	if got := f.adopt(t, a); got.AdoptedRestored {
		t.Fatalf("adoption = %+v, want no relabel of an already-ready head", got)
	}

	bIdentity := aIdentity
	bIdentity.TreeOID = "tree-b"
	f.observe(t, bIdentity)
	b := f.claim(t, "attempt-b", a.GenerationID, 0)
	f.publish(t, b)
	if got := f.adopt(t, b); !got.PreviousSuperseded {
		t.Fatalf("adoption = %+v, want the replaced root labelled", got)
	}
	if got := f.state(t, a.GenerationID); got != ViewGenerationSuperseded {
		t.Fatalf("replaced root state = %s, want superseded", got)
	}

	// Revert: the reuse lookup hands back the superseded root and the adoption
	// reinstates it as the live head.
	f.observe(t, aIdentity)
	reused := f.claim(t, "attempt-a-again", b.GenerationID, b.GenerationID)
	if reused.GenerationID != a.GenerationID {
		t.Fatalf("revert reuse = %+v, want the superseded root offered back", reused)
	}
	adoption := f.adopt(t, reused)
	if !adoption.AdoptedRestored || !adoption.PreviousSuperseded {
		t.Fatalf("adoption = %+v, want the reinstated head cleared and the abandoned head labelled", adoption)
	}
	if f.active(t) != a.GenerationID {
		t.Fatalf("active pointer = %d, want the reinstated head %d", f.active(t), a.GenerationID)
	}
	if got := f.state(t, a.GenerationID); got != ViewGenerationReady {
		t.Fatalf("reinstated head state = %s, want ready", got)
	}
	if got := f.state(t, b.GenerationID); got != ViewGenerationSuperseded {
		t.Fatalf("abandoned head state = %s, want superseded", got)
	}
}

// TestDedicatedBaseAdoptionCannotRelabelBelowThePublicationFloor pins the fence
// both relabel writes carry. A pointer at or below the owner's publication
// floor names a generation this authority never published — a legacy pointer a
// pre-authority installation left behind, or generation 0's sentinel — so it is
// outside the scope that authorized the write and must be left exactly as it
// is, adoption included.
func TestDedicatedBaseAdoptionCannotRelabelBelowThePublicationFloor(t *testing.T) {
	f := newDedicatedPublicationFixture(t)
	ctx := context.Background()
	legacy := f.claim(t, "legacy", 0, 0)
	f.publish(t, legacy)
	f.adopt(t, legacy)

	// Re-acquire the authority from scratch: the floor is captured once, when
	// the publication row is created, so the legacy generation only falls below
	// it after the owner is re-established.
	if err := f.c.DeleteDedicatedGraph(ctx, f.graph.GraphID); err != nil {
		t.Fatal(err)
	}
	if err := f.c.UpsertDedicatedGraph(ctx, f.graph); err != nil {
		t.Fatal(err)
	}
	// The recreated graph starts with no pointer; the legacy one is what a
	// pre-authority installation leaves behind.
	f.exec(t, `UPDATE dedicated_graphs SET active_generation_id=? WHERE graph_id=?`, legacy.GenerationID, f.graph.GraphID)
	var err error
	f.authority, err = f.c.AcquireDedicatedBaseAuthority(ctx, AcquireDedicatedBaseAuthorityRequest{
		GraphID: f.graph.GraphID, Owner: DedicatedBaseOwner{f.owner.CheckoutID, f.owner.Incarnation}, Token: "authority-2"})
	if err != nil {
		t.Fatal(err)
	}
	if f.authority.GenerationFloor < legacy.GenerationID {
		t.Fatalf("floor=%d did not cover legacy generation %d", f.authority.GenerationFloor, legacy.GenerationID)
	}
	f.desire, err = f.c.RecordDedicatedBaseDesire(ctx, RecordDedicatedBaseDesireRequest{
		Authority: f.authority,
		Identity:  DedicatedBaseIdentity{TreeOID: "tree-b", ConfigHash: "config", ExtractorVersions: "extractors", ResolverVersion: "resolver"}})
	if err != nil {
		t.Fatal(err)
	}
	next := f.claim(t, "post-floor", legacy.GenerationID, 0)
	f.publish(t, next)

	adoption := f.adopt(t, next)
	if adoption.PreviousGenerationID != legacy.GenerationID {
		t.Fatalf("adoption = %+v, want the legacy pointer reported as replaced", adoption)
	}
	if adoption.PreviousSuperseded {
		t.Fatalf("adoption = %+v, want a generation below the floor left alone", adoption)
	}
	if got := f.state(t, legacy.GenerationID); got != ViewGenerationReady {
		t.Fatalf("legacy generation state = %s, want ready: it is outside this authority's scope", got)
	}
	if f.active(t) != next.GenerationID {
		t.Fatal("the adoption itself must still have installed the active pointer")
	}
}

// TestDedicatedBaseRestoreRefusesBelowThePublicationFloor pins the same fence
// on the mirror write, at the level it can be reached at: the claim allocator
// already refuses any candidate at or below the floor, so a below-floor adopted
// id cannot be produced through the public path, and the guard is only
// observable by calling the write directly. It is not decoration — it is what
// keeps a legacy pointer a pre-authority installation left behind from being
// relabelled by an authority that never published it.
func TestDedicatedBaseRestoreRefusesBelowThePublicationFloor(t *testing.T) {
	f := newDedicatedPublicationFixture(t)
	ctx := context.Background()
	a := f.claim(t, "attempt-a", 0, 0)
	f.publish(t, a)
	f.adopt(t, a)
	f.exec(t, `UPDATE view_generations SET state=? WHERE generation_id=?`, string(ViewGenerationSuperseded), a.GenerationID)

	restore := func(floor int64) bool {
		t.Helper()
		authority := f.authority
		authority.GenerationFloor = floor
		var restored bool
		if err := f.c.withTx(ctx, func(tx *sql.Tx) error {
			var err error
			restored, err = restoreAdoptedDedicatedHeadTx(ctx, tx, authority, a.GenerationID)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return restored
	}

	if restore(a.GenerationID) {
		t.Fatal("a generation at the publication floor was relabelled by an authority that did not publish it")
	}
	if got := f.state(t, a.GenerationID); got != ViewGenerationSuperseded {
		t.Fatalf("below-floor generation state = %s, want superseded", got)
	}
	// The same call above the floor fires, so the refusal above is the fence
	// and not some other guard swallowing the write.
	if !restore(a.GenerationID - 1) {
		t.Fatal("an in-scope superseded head was not restored")
	}
	if got := f.state(t, a.GenerationID); got != ViewGenerationReady {
		t.Fatalf("restored head state = %s, want ready", got)
	}
}

// observationPass is a reconciliation pass over the fixture's owner checkout:
// the shape internal/reconcile writes, stating a head and a removal grace it
// sampled at one clock.
func (f *dedicatedPublicationFixture) observationPass(clock int64, tree string) UpdateCheckoutObservationRequest {
	return UpdateCheckoutObservationRequest{
		CheckoutID: f.owner.CheckoutID, Incarnation: f.owner.Incarnation,
		State:    CheckoutStateRemovalGrace,
		RootPath: f.owner.RootPath, GitDir: f.owner.GitDir,
		HeadRef: "refs/heads/main", HeadCommit: "commit-" + tree, HeadTree: tree,
		LastAccessible: clock, RemovalDetectedAt: clock, RemovalDeadline: clock + 300,
		RemovalEvidence: "authoritative_omission",
		LastSeen:        clock, LastError: "gone",
	}
}

// fenceTwice runs the two passes the fence refuses outright, so the next one
// reaches the quorum rebase. It asserts the refusal rather than assuming it.
func (f *dedicatedPublicationFixture) fenceTwice(t testing.TB, tree string) {
	t.Helper()
	for _, clock := range []int64{1400, 1401} {
		report, err := f.c.UpdateCheckoutObservationWithReport(context.Background(), f.observationPass(clock, tree))
		if !errors.Is(err, ErrCatalogObservationFenced) || !report.Fenced() {
			t.Fatalf("the pass at %d = %+v / %v, want it fenced", clock, report, err)
		}
	}
}

// TestObservationRebaseNeverRegressesAnAdoptedHead is the head axis's half of
// the observation fence.
//
// The clock fence rebases after observationRegressionQuorum refusals, because
// refusing a stepped-back clock forever wedges the row. That rebase is safe on
// the state and grace axes and is not safe on the head: the pass that triggers
// it is one the row's own clock says was sampled BEFORE the adoption published
// the head, and head_tree is what every dependent worktree's layer identity
// keys on, so writing it would republish a base the family has moved past —
// invisibly, since last_seen is MAX(stored, observed) and would still name the
// newer sample.
//
// So the head keeps a floor of its own: the sequence its adoption published it
// at. A stale sample keeps its other axes and is refused on the head alone,
// however many times it comes back; a sample from at or past the publication
// moves it as always.
func TestObservationRebaseNeverRegressesAnAdoptedHead(t *testing.T) {
	f := newDedicatedPublicationFixture(t)
	ctx := context.Background()
	claim := f.claimAt(t, "attempt-a", "commit-a", 5000, 0)
	f.publish(t, claim)
	if adoption := f.adopt(t, claim); !adoption.HeadAdvanced {
		t.Fatalf("the fixture did not publish a head to defend: %+v", adoption)
	}

	f.fenceTwice(t, "tree-stale")

	// The third pass from behind is the rebase: the clock domain moved, so the
	// state and grace axes follow it. The head does not.
	rebased, err := f.c.UpdateCheckoutObservationWithReport(ctx, f.observationPass(1402, "tree-stale"))
	if err != nil {
		t.Fatalf("the pass after the skew = %v", err)
	}
	if rebased.Verdict != CheckoutObservationRebased {
		t.Fatalf("the third pass behind the clock = %+v, want it rebased", rebased)
	}
	if !rebased.HeadRefused || rebased.HeadEpoch != 5000 || rebased.HeadTree != "tree-a" {
		t.Fatalf("rebase report = %+v, want the head refused at the adoption's clock 5000", rebased)
	}
	owner := f.ownerHead(t)
	if owner.HeadTree != "tree-a" || owner.HeadCommit != "commit-a" {
		t.Fatalf("the rebase regressed the adopted head to %q/%q", owner.HeadTree, owner.HeadCommit)
	}
	if owner.State != CheckoutStateRemovalGrace || owner.RemovalDeadline != 1702 {
		t.Fatalf("the rebase did not move the axes it may move: %+v", owner)
	}
	if owner.LastSeen != 5000 {
		t.Fatalf("last_seen = %d, want the stored 5000 kept", owner.LastSeen)
	}
	if !strings.Contains(owner.LastError, CheckoutHeadRefusedMarker) || !strings.Contains(owner.LastError, "gone") {
		t.Fatalf("last_error = %q, want the refusal traced beside the pass's own diagnosis", owner.LastError)
	}

	// The clock domain has been adopted, so this pass is an ordinary applied
	// write — and it still cannot have the head. A refusal that only held for
	// the pass that triggered the rebase would be no guarantee at all.
	steady, err := f.c.UpdateCheckoutObservationWithReport(ctx, f.observationPass(1403, "tree-steady"))
	if err != nil {
		t.Fatalf("the pass after the rebase = %v", err)
	}
	if steady.Verdict != CheckoutObservationApplied || !steady.HeadRefused {
		t.Fatalf("the pass after the rebase = %+v, want an applied write with the head still held", steady)
	}
	if got := f.ownerHead(t).HeadTree; got != "tree-a" {
		t.Fatalf("head_tree = %q after a fourth stale sample, want the adopted tree-a", got)
	}

	// A genuine newer observation still advances it: the fence orders the head,
	// it does not freeze it.
	newer, err := f.c.UpdateCheckoutObservationWithReport(ctx, f.observationPass(6000, "tree-live"))
	if err != nil {
		t.Fatalf("the newer observation = %v", err)
	}
	if newer.Verdict != CheckoutObservationApplied || newer.HeadRefused {
		t.Fatalf("the newer observation = %+v, want the head advanced", newer)
	}
	after := f.ownerHead(t)
	if after.HeadTree != "tree-live" || after.HeadCommit != "commit-tree-live" {
		t.Fatalf("head = %q/%q after an observation past the publication", after.HeadTree, after.HeadCommit)
	}
	if strings.Contains(after.LastError, CheckoutHeadRefusedMarker) {
		t.Fatalf("last_error = %q, want the trace gone once the head moved", after.LastError)
	}

	// And the floor is the ADOPTION's, not the row's: the head standing now was
	// written by an ordinary observation and is no longer the tree the active
	// generation carries, so nothing published it and the next rebase may move
	// it. A fence that held every head would wedge the axis it protects.
	f.fenceTwice(t, "tree-ordinary")
	ordinary, err := f.c.UpdateCheckoutObservationWithReport(ctx, f.observationPass(1402, "tree-ordinary"))
	if err != nil {
		t.Fatalf("the rebase over an unpublished head = %v", err)
	}
	if ordinary.Verdict != CheckoutObservationRebased || ordinary.HeadRefused || ordinary.HeadEpoch != 0 {
		t.Fatalf("the rebase over an unpublished head = %+v, want the head admitted with no floor", ordinary)
	}
	if got := f.ownerHead(t).HeadTree; got != "tree-ordinary" {
		t.Fatalf("head_tree = %q, want the rebase to move a head no adoption published", got)
	}
}

// TestObservationAtHeadEpochMovesAnAdoptedHead is the other side of the same
// fence: the head is ordered, not owned. An observer that can say which
// publication its sample is from — the Git watcher's HEAD-change path — states
// the adoption sequence and is believed, even from behind the row's clock.
func TestObservationAtHeadEpochMovesAnAdoptedHead(t *testing.T) {
	f := newDedicatedPublicationFixture(t)
	ctx := context.Background()
	claim := f.claimAt(t, "attempt-a", "commit-a", 5000, 0)
	f.publish(t, claim)
	f.adopt(t, claim)

	f.fenceTwice(t, "tree-after-head-change")

	// The same third pass as the test above, with the one thing that test's
	// pass could not state.
	rebased, err := f.c.UpdateCheckoutObservationAtHeadEpoch(ctx,
		f.observationPass(1402, "tree-after-head-change"), 5000)
	if err != nil {
		t.Fatalf("the head-change pass = %v", err)
	}
	if rebased.Verdict != CheckoutObservationRebased || rebased.HeadRefused {
		t.Fatalf("the head-change pass = %+v, want the head admitted", rebased)
	}
	owner := f.ownerHead(t)
	if owner.HeadTree != "tree-after-head-change" {
		t.Fatalf("head_tree = %q, want the head-change sample's tree", owner.HeadTree)
	}
	if strings.Contains(owner.LastError, CheckoutHeadRefusedMarker) {
		t.Fatalf("last_error = %q, want no refusal traced for an admitted head", owner.LastError)
	}

	// And an epoch older than the publication buys nothing: stating a sequence
	// is not the same as stating this one.
	f.fenceTwice(t, "tree-older-epoch")
	stale, err := f.c.UpdateCheckoutObservationAtHeadEpoch(ctx,
		f.observationPass(1402, "tree-older-epoch"), 1)
	if err != nil {
		t.Fatalf("the pass stating an older epoch = %v", err)
	}
	if !stale.HeadRefused {
		t.Fatalf("the pass stating an older epoch = %+v, want the head refused", stale)
	}
}

// TestAdoptedHeadEpochSurvivesTheProcessThatAdopted pins the durable half.
//
// The ledger is process-local, so a head floor that lived only in it would be
// forgotten by exactly the restart layer reuse exists for — and the first three
// stale passes after that restart would regress the head the previous process
// published. The floor is therefore read back from the adoption itself: the
// active generation's tree and created_at.
func TestAdoptedHeadEpochSurvivesTheProcessThatAdopted(t *testing.T) {
	f := newDedicatedPublicationFixture(t)
	ctx := context.Background()

	// An observation FIRST, so the ledger already holds a fence for this row
	// when the adoption moves the head under it: the floor has to be re-read
	// then too, not only when the row is new to the process.
	first := f.observationPass(100, "tree-before")
	// Ready, because the adoption below refuses an owner that is not steady
	// dedicated — the fence, not the checkout state, is what this test is about.
	first.State, first.RemovalEvidence = CheckoutStateReady, ""
	first.RemovalDetectedAt, first.RemovalDeadline = 0, 0
	if err := f.c.UpdateCheckoutObservation(ctx, first); err != nil {
		t.Fatalf("the first observation = %v", err)
	}
	claim := f.claimAt(t, "attempt-a", "commit-a", 5000, 0)
	f.publish(t, claim)
	if adoption := f.adopt(t, claim); !adoption.HeadAdvanced {
		t.Fatalf("the adoption did not move the head: %+v", adoption)
	}

	f.fenceTwice(t, "tree-stale")
	rebased, err := f.c.UpdateCheckoutObservationWithReport(ctx, f.observationPass(1402, "tree-stale"))
	if err != nil || !rebased.HeadRefused {
		t.Fatalf("the rebase under a known fence = %+v / %v, want the head refused", rebased, err)
	}

	// Now the process dies. A fresh store over the same file has a fresh
	// ledger, which remembers no adoption at all.
	if err := f.store.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	reopened, err := Open(f.path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	f.store, f.c = reopened, reopened.Catalog()

	f.fenceTwice(t, "tree-stale-after-restart")
	restarted, err := f.c.UpdateCheckoutObservationWithReport(ctx,
		f.observationPass(1402, "tree-stale-after-restart"))
	if err != nil {
		t.Fatalf("the rebase after the restart = %v", err)
	}
	if restarted.Verdict != CheckoutObservationRebased {
		t.Fatalf("the pass after the restart = %+v, want it rebased", restarted)
	}
	if !restarted.HeadRefused || restarted.HeadEpoch != 5000 {
		t.Fatalf("the pass after the restart = %+v, want the head refused at the adoption's 5000", restarted)
	}
	if got := f.ownerHead(t).HeadTree; got != "tree-a" {
		t.Fatalf("head_tree = %q after a restart, want the adopted tree-a", got)
	}
}

// TestAdoptedHeadEpochTxNamesOnlyTheOwnersPublication pins the durable probe
// itself: which head it will call published, and which it will not.
//
// It answers for ONE checkout and ONE tree. Another owner's active base is
// another family's business — treating it as this row's publication would fence
// a head nothing published — and a tree the active generation does not carry
// was put there by an ordinary observation, which the head fence has no claim
// over.
func TestAdoptedHeadEpochTxNamesOnlyTheOwnersPublication(t *testing.T) {
	f := newDedicatedPublicationFixture(t)
	ctx := context.Background()
	claim := f.claimAt(t, "attempt-a", "commit-a", 5000, 0)
	f.publish(t, claim)
	f.adopt(t, claim)

	tx, err := f.store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback() }()

	epoch := func(checkoutID, tree string) int64 {
		t.Helper()
		got, err := adoptedHeadEpochTx(ctx, tx, checkoutID, tree)
		if err != nil {
			t.Fatalf("adoptedHeadEpochTx(%q, %q): %v", checkoutID, tree, err)
		}
		return got
	}
	if got := epoch(f.owner.CheckoutID, "tree-a"); got != 5000 {
		t.Fatalf("the owner's own published head = %d, want the adoption's 5000", got)
	}
	if got := epoch("another-checkout", "tree-a"); got != 0 {
		t.Fatalf("another checkout's head = %d, want 0: this publication is not its own", got)
	}
	if got := epoch(f.owner.CheckoutID, "tree-somewhere-else"); got != 0 {
		t.Fatalf("a head the active generation does not carry = %d, want 0", got)
	}
	if got := epoch(f.owner.CheckoutID, ""); got != 0 {
		t.Fatalf("an unsampled head = %d, want 0", got)
	}
}
