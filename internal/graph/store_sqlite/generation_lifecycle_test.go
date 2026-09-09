package store_sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/graph"
)

// This is an additive test of actual public APIs. No implementation is replaced.
// Healthy insertion and both slot values are proved before negative admission
// outcomes count as evidence. Test-owned SQL observes rows and logical writes.
type catalogAdmissionFixture struct {
	s                       *Store
	c                       *Catalog
	ctx                     context.Context
	commit, dirty, retiring int64
}

type catalogAdmissionRouteSnapshot struct {
	commit, dirty, epoch int64
	graph                string
	state                string
}

func newCatalogAdmissionFixture(t *testing.T) *catalogAdmissionFixture {
	t.Helper()
	root := t.TempDir()
	s, err := Open(filepath.Join(root, "routes.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	c := s.Catalog()
	family := RepositoryFamily{FamilyID: "family", CommonDirIdentity: filepath.Join(root, "git"), State: "active"}
	if err := c.UpsertRepositoryFamily(ctx, family); err != nil {
		t.Fatal(err)
	}
	owner := Checkout{CheckoutID: "owner", Incarnation: "incarnation", FamilyID: family.FamilyID, RootPath: root, GitDir: family.CommonDirIdentity, AdminName: "main", State: CheckoutStateReady, DesiredMode: CheckoutModeDedicated, EffectiveMode: CheckoutModeDedicated, HeadTree: "tree"}
	if err := c.UpsertCheckout(ctx, owner); err != nil {
		t.Fatal(err)
	}
	if err := c.UpsertDedicatedGraph(ctx, DedicatedGraph{GraphID: "graph", OwnerCheckoutID: owner.CheckoutID, RepoPrefix: "repo", FamilyID: family.FamilyID, IsPrimaryBase: true, State: "ready"}); err != nil {
		t.Fatal(err)
	}
	f := &catalogAdmissionFixture{s: s, c: c, ctx: ctx}
	ids := make([]int64, 3)
	for i := range ids {
		id, _, err := s.BeginPayloadGeneration(ctx, PayloadGenerationRequest{OwnerKind: "dedicated_graph", GraphID: "graph", CheckoutID: "owner", GenerationKind: "commit", LayerID: fmt.Sprintf("layer-%d", i), TreeOID: fmt.Sprintf("tree-%d", i)})
		if err != nil {
			t.Fatal(err)
		}
		if err := s.PublishPayloadGeneration(ctx, id, 2); err != nil {
			t.Fatal(err)
		}
		ids[i] = id
	}
	f.commit, f.dirty, f.retiring = ids[0], ids[1], ids[2]
	if err := c.SetViewGenerationState(ctx, f.retiring, ViewGenerationRetiring, ViewGenerationReady); err != nil {
		t.Fatal(err)
	}
	row, found, err := c.GetViewGeneration(ctx, f.retiring)
	if err != nil || !found || row.State != ViewGenerationRetiring {
		t.Fatalf("retiring fixture invalid: found=%v state=%s err=%v", found, row.State, err)
	}
	// Positive insertion is an independent fixture gate, not merely a nil error
	// from the failing operation. Persisted pointer/epoch/state must agree.
	if err := c.UpsertCheckoutRoute(ctx, CheckoutRoute{CheckoutID: "owner", GraphID: "graph", CommitGenerationID: f.commit, DirtyGenerationID: f.dirty, RouteEpoch: 4, State: RouteActive}); err != nil {
		t.Fatalf("SETUP healthy route insertion failed: %v", err)
	}
	got, found := f.snapshot(t)
	if !found || got != (catalogAdmissionRouteSnapshot{commit: f.commit, dirty: f.dirty, epoch: 4, graph: "graph", state: string(RouteActive)}) {
		t.Fatalf("SETUP healthy route not persisted: found=%v got=%+v", found, got)
	}
	// Validate both candidate slot literals and actual success semantics before
	// relying on either slot in a retirement refusal regression.
	for _, slot := range []string{"commit", "dirty"} {
		before, _ := f.snapshot(t)
		req := FlipCheckoutRouteSlotRequest{CheckoutID: "owner", GenerationID: f.commit, ExpectedRouteEpoch: before.epoch, State: RouteActive}
		if slot == "commit" {
			req.Slot = "commit"
		} else {
			req.Slot = "dirty"
			req.GenerationID = f.dirty
		}
		if err := c.FlipCheckoutRouteSlot(ctx, req); err != nil {
			t.Fatalf("SETUP healthy %s flip failed: %v", slot, err)
		}
		after, found := f.snapshot(t)
		if !found || after.commit != f.commit || after.dirty != f.dirty || after.epoch != before.epoch+1 || after.state != string(RouteActive) {
			t.Fatalf("SETUP healthy %s flip not persisted correctly: before=%+v after=%+v found=%v", slot, before, after, found)
		}
	}
	t.Logf("healthy route and both slots verified; retiring=%d", f.retiring)
	return f
}

func (f *catalogAdmissionFixture) snapshot(t *testing.T) (catalogAdmissionRouteSnapshot, bool) {
	t.Helper()
	var row catalogAdmissionRouteSnapshot
	err := f.s.db.QueryRowContext(f.ctx, `SELECT graph_id,COALESCE(commit_generation_id,0),COALESCE(dirty_generation_id,0),route_epoch,state FROM checkout_routes WHERE checkout_id=?`, "owner").Scan(&row.graph, &row.commit, &row.dirty, &row.epoch, &row.state)
	if err == sql.ErrNoRows {
		return row, false
	}
	if err != nil {
		t.Fatal(err)
	}
	return row, true
}

func (f *catalogAdmissionFixture) noWriteAudit(t *testing.T) func() {
	t.Helper()
	err := f.c.withTx(f.ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(f.ctx, `CREATE TABLE private_route_write_audit(n INTEGER NOT NULL); INSERT INTO private_route_write_audit VALUES(0)`); err != nil {
			return err
		}
		for _, operation := range []string{"INSERT", "UPDATE", "DELETE"} {
			query := fmt.Sprintf(`CREATE TRIGGER private_route_audit_%s AFTER %s ON checkout_routes BEGIN UPDATE private_route_write_audit SET n=n+1; END`, operation, operation)
			if _, err := tx.ExecContext(f.ctx, query); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return func() {
		var writes int
		if err := f.s.db.QueryRowContext(f.ctx, `SELECT n FROM private_route_write_audit`).Scan(&writes); err != nil {
			t.Fatal(err)
		}
		t.Logf("audited route logical writes=%d", writes)
		if writes != 0 {
			t.Errorf("refused/no-op operation wrote %d route row mutations", writes)
		}
	}
}

func TestCatalogRouteAdmissionHealthyAndZeroClearControls(t *testing.T) {
	for _, slot := range []string{"commit", "dirty"} {
		t.Run(slot, func(t *testing.T) {
			f := newCatalogAdmissionFixture(t)
			before, _ := f.snapshot(t)
			req := FlipCheckoutRouteSlotRequest{CheckoutID: "owner", GenerationID: 0, ExpectedRouteEpoch: before.epoch, State: RouteActive}
			if slot == "commit" {
				req.Slot = "commit"
			} else {
				req.Slot = "dirty"
			}
			if err := f.c.FlipCheckoutRouteSlot(f.ctx, req); err != nil {
				t.Fatalf("zero clearing rejected: %v", err)
			}
			after, found := f.snapshot(t)
			want := before
			want.epoch++
			if slot == "commit" {
				want.commit = 0
			} else {
				want.dirty = 0
			}
			if !found || after != want {
				t.Errorf("zero clearing changed other state: want=%+v got=%+v found=%v", want, after, found)
			}
		})
	}
}

func TestCatalogRouteAdmissionStaleEpochControl(t *testing.T) {
	f := newCatalogAdmissionFixture(t)
	before, _ := f.snapshot(t)
	check := f.noWriteAudit(t)
	err := f.c.FlipCheckoutRouteSlot(f.ctx, FlipCheckoutRouteSlotRequest{CheckoutID: "owner", Slot: "commit", GenerationID: f.dirty, ExpectedRouteEpoch: before.epoch - 1, State: RouteActive})
	t.Logf("stale epoch result=%v", err)
	if !errors.Is(err, ErrCatalogStaleGuard) {
		t.Errorf("stale epoch error changed: %v", err)
	}
	after, found := f.snapshot(t)
	if !found || after != before {
		t.Errorf("stale epoch changed route: before=%+v after=%+v found=%v", before, after, found)
	}
	check()
}

func TestCatalogRouteAdmissionUpsertRejectsRetiring(t *testing.T) {
	for _, existing := range []bool{false, true} {
		for _, slot := range []string{"commit", "dirty"} {
			t.Run(fmt.Sprintf("existing=%v/%s", existing, slot), func(t *testing.T) {
				f := newCatalogAdmissionFixture(t)
				before, _ := f.snapshot(t)
				if !existing {
					if err := f.c.DeleteCheckoutRoute(f.ctx, "owner"); err != nil {
						t.Fatal(err)
					}
				}
				check := f.noWriteAudit(t)
				proposal := CheckoutRoute{CheckoutID: "owner", GraphID: "graph", CommitGenerationID: f.commit, DirtyGenerationID: f.dirty, RouteEpoch: before.epoch + 1, State: RouteActive}
				if slot == "commit" {
					proposal.CommitGenerationID = f.retiring
				} else {
					proposal.DirtyGenerationID = f.retiring
				}
				err := f.c.UpsertCheckoutRoute(f.ctx, proposal)
				t.Logf("retiring upsert existing=%v slot=%s result=%v", existing, slot, err)
				if err == nil {
					t.Error("already-retiring target accepted by actual UpsertCheckoutRoute")
				}
				after, found := f.snapshot(t)
				if existing {
					if !found || after != before {
						t.Errorf("retiring proposal changed existing route: before=%+v after=%+v found=%v", before, after, found)
					}
				} else if found {
					t.Errorf("retiring proposal inserted route: %+v", after)
				}
				check()
			})
		}
	}
}

func TestCatalogRouteAdmissionFlipRejectsRetiring(t *testing.T) {
	for _, slot := range []string{"commit", "dirty"} {
		t.Run(slot, func(t *testing.T) {
			f := newCatalogAdmissionFixture(t)
			before, _ := f.snapshot(t)
			check := f.noWriteAudit(t)
			req := FlipCheckoutRouteSlotRequest{CheckoutID: "owner", GenerationID: f.retiring, ExpectedRouteEpoch: before.epoch, State: RouteActive}
			if slot == "commit" {
				req.Slot = "commit"
			} else {
				req.Slot = "dirty"
			}
			err := f.c.FlipCheckoutRouteSlot(f.ctx, req)
			t.Logf("retiring %s flip result=%v", slot, err)
			if err == nil {
				t.Error("already-retiring target accepted by actual FlipCheckoutRouteSlot")
			}
			after, found := f.snapshot(t)
			if !found || after != before {
				t.Errorf("retiring proposal changed route: before=%+v after=%+v found=%v", before, after, found)
			}
			check()
		})
	}
}

func newCatalogAdmissionRefFixture(t *testing.T) (*catalogAdmissionFixture, RefView) {
	t.Helper()
	f := newCatalogAdmissionFixture(t)
	view := RefView{RefViewID: "ref", GraphID: "graph", SelectorKind: "git_ref", SelectorValue: "refs/heads/topic", EnrichmentProfile: "structural", State: RefViewReady, ActiveGenerationID: f.commit, DesiredTree: "tree-0", ActiveTree: "tree-0", DesiredBuildFingerprint: "fingerprint", ActiveBuildFingerprint: "fingerprint", RouteEpoch: 8, ExactView: true}
	got, err := f.c.GetOrCreateRefView(f.ctx, view)
	if err != nil {
		t.Fatalf("SETUP healthy GetOrCreateRefView rejected: %v", err)
	}
	if !reflect.DeepEqual(got, view) {
		t.Fatalf("SETUP healthy returned view differs: got=%+v want=%+v", got, view)
	}
	stored, found, err := f.c.GetRefView(f.ctx, view.RefViewID)
	if err != nil || !found || !reflect.DeepEqual(stored, view) {
		t.Fatalf("SETUP healthy stored view invalid: found=%v got=%+v err=%v", found, stored, err)
	}
	t.Logf("healthy ref selector=%s profile=%s accepted and persisted", view.SelectorKind, view.EnrichmentProfile)
	return f, view
}

func catalogAdmissionRefNoWrites(t *testing.T, f *catalogAdmissionFixture) func() {
	t.Helper()
	err := f.c.withTx(f.ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(f.ctx, `CREATE TABLE catalog_ref_write_audit(n INTEGER NOT NULL); INSERT INTO catalog_ref_write_audit VALUES(0)`); err != nil {
			return err
		}
		for _, table := range []string{"ref_views", "ref_view_builds"} {
			for _, op := range []string{"INSERT", "UPDATE", "DELETE"} {
				q := fmt.Sprintf(`CREATE TRIGGER catalog_ref_audit_%s_%s AFTER %s ON %s BEGIN UPDATE catalog_ref_write_audit SET n=n+1; END`, table, op, op, table)
				if _, err := tx.ExecContext(f.ctx, q); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return func() {
		var writes int
		if err := f.s.db.QueryRowContext(f.ctx, `SELECT n FROM catalog_ref_write_audit`).Scan(&writes); err != nil {
			t.Fatal(err)
		}
		t.Logf("audited ref/build logical writes=%d", writes)
		if writes != 0 {
			t.Errorf("refused/ignored ref operation wrote %d rows", writes)
		}
	}
}

func TestCatalogRefAdmissionHealthyControl(t *testing.T) {
	newCatalogAdmissionRefFixture(t)
}

func TestCatalogRefAdmissionExistingIgnoresInvalidTargetControl(t *testing.T) {
	for _, target := range []string{"retiring", "missing"} {
		t.Run(target, func(t *testing.T) {
			f, view := newCatalogAdmissionRefFixture(t)
			proposed := view
			proposed.ActiveGenerationID = f.retiring
			if target == "missing" {
				proposed.ActiveGenerationID = f.retiring + 1000
			}
			proposed.DesiredTree = "ignored-new-desire"
			proposed.RouteEpoch++
			check := catalogAdmissionRefNoWrites(t, f)
			for range 20 {
				got, err := f.c.GetOrCreateRefView(f.ctx, proposed)
				if err != nil {
					t.Fatalf("ignored %s proposal rejected existing row: %v", target, err)
				}
				if !reflect.DeepEqual(got, view) {
					t.Fatalf("ignored proposal changed returned row: got=%+v want=%+v", got, view)
				}
			}
			stored, found, err := f.c.GetRefView(f.ctx, view.RefViewID)
			if err != nil || !found || !reflect.DeepEqual(stored, view) {
				t.Errorf("ignored proposal changed persisted row: found=%v got=%+v err=%v", found, stored, err)
			}
			check()
		})
	}
}

func TestCatalogRefAdmissionSelectorConflictControl(t *testing.T) {
	f, view := newCatalogAdmissionRefFixture(t)
	proposed := view
	proposed.RefViewID = "second-id"
	proposed.ActiveGenerationID = f.retiring
	check := catalogAdmissionRefNoWrites(t, f)
	_, err := f.c.GetOrCreateRefView(f.ctx, proposed)
	t.Logf("different-ID selector conflict result=%v", err)
	if !errors.Is(err, ErrCatalogInvalidValue) {
		t.Errorf("selector conflict changed error contract: %v", err)
	}
	stored, found, err := f.c.GetRefView(f.ctx, view.RefViewID)
	if err != nil || !found || !reflect.DeepEqual(stored, view) {
		t.Errorf("selector conflict changed winner: found=%v got=%+v err=%v", found, stored, err)
	}
	check()
}

func TestCatalogRefAdmissionNewRejectsRetiring(t *testing.T) {
	f, view := newCatalogAdmissionRefFixture(t)
	view.RefViewID = "new-ref"
	view.SelectorValue = "refs/heads/new-topic"
	view.ActiveGenerationID = f.retiring
	check := catalogAdmissionRefNoWrites(t, f)
	got, err := f.c.GetOrCreateRefView(f.ctx, view)
	t.Logf("new retiring ref result=%v returned=%+v", err, got)
	if err == nil {
		t.Error("actual GetOrCreateRefView accepted a new retiring target")
	}
	stored, found, readErr := f.c.GetRefView(f.ctx, view.RefViewID)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if found {
		t.Errorf("retiring proposal persisted new ref: %+v", stored)
	}
	check()
}

func TestCatalogRouteAdmissionFlipBothHealthyAndStaleControls(t *testing.T) {
	f := newCatalogAdmissionFixture(t)
	before, _ := f.snapshot(t)
	request := FlipCheckoutRouteRequest{CheckoutID: "owner", GraphID: "graph", CommitGenerationID: f.dirty, DirtyGenerationID: f.commit, ExpectedRouteEpoch: before.epoch, State: RouteActive}
	if err := f.c.FlipCheckoutRoute(f.ctx, request); err != nil {
		t.Fatalf("SETUP healthy FlipCheckoutRoute rejected: %v", err)
	}
	after, found := f.snapshot(t)
	want := before
	want.commit = f.dirty
	want.dirty = f.commit
	want.epoch++
	if !found || after != want {
		t.Fatalf("SETUP healthy both-slot flip not persisted: got=%+v want=%+v found=%v", after, want, found)
	}
	check := f.noWriteAudit(t)
	request.CommitGenerationID = f.retiring
	request.DirtyGenerationID = f.retiring
	// The request retains the now-stale epoch. The retiring target must not
	// mask the existing stale guard or cause a partial update.
	err := f.c.FlipCheckoutRoute(f.ctx, request)
	if !errors.Is(err, ErrCatalogStaleGuard) {
		t.Errorf("stale both-slot error changed: %v", err)
	}
	stored, found := f.snapshot(t)
	if !found || stored != after {
		t.Errorf("stale both-slot request mutated row: got=%+v want=%+v", stored, after)
	}
	check()
}

func TestCatalogRouteAdmissionFlipBothRejectsRetiring(t *testing.T) {
	for _, slot := range []string{"commit", "dirty", "both"} {
		t.Run(slot, func(t *testing.T) {
			f := newCatalogAdmissionFixture(t)
			before, _ := f.snapshot(t)
			// Independent healthy control for the API under test.
			healthy := FlipCheckoutRouteRequest{CheckoutID: "owner", GraphID: "graph", CommitGenerationID: f.commit, DirtyGenerationID: f.dirty, ExpectedRouteEpoch: before.epoch, State: RouteActive}
			if err := f.c.FlipCheckoutRoute(f.ctx, healthy); err != nil {
				t.Fatalf("SETUP healthy FlipCheckoutRoute rejected: %v", err)
			}
			before, found := f.snapshot(t)
			if !found || before.commit != f.commit || before.dirty != f.dirty {
				t.Fatal("SETUP healthy both-slot route invalid")
			}
			proposal := healthy
			proposal.ExpectedRouteEpoch = before.epoch
			if slot == "commit" || slot == "both" {
				proposal.CommitGenerationID = f.retiring
			}
			if slot == "dirty" || slot == "both" {
				proposal.DirtyGenerationID = f.retiring
			}
			check := f.noWriteAudit(t)
			err := f.c.FlipCheckoutRoute(f.ctx, proposal)
			t.Logf("retiring both-slot flip target=%s result=%v", slot, err)
			if err == nil {
				t.Error("actual FlipCheckoutRoute accepted retiring target")
			}
			after, found := f.snapshot(t)
			if !found || after != before {
				t.Errorf("retiring both-slot proposal changed route: before=%+v after=%+v", before, after)
			}
			check()
		})
	}
}

func TestCatalogRouteAdmissionSlotStaleRetiringControls(t *testing.T) {
	for _, slot := range []RouteSlot{RouteSlotCommit, RouteSlotDirty} {
		t.Run(string(slot), func(t *testing.T) {
			f := newCatalogAdmissionFixture(t)
			before, _ := f.snapshot(t)
			check := f.noWriteAudit(t)
			err := f.c.FlipCheckoutRouteSlot(f.ctx, FlipCheckoutRouteSlotRequest{CheckoutID: "owner", Slot: slot, GenerationID: f.retiring, ExpectedRouteEpoch: before.epoch - 1, State: RouteActive})
			if !errors.Is(err, ErrCatalogStaleGuard) {
				t.Errorf("stale slot error masked by retiring target: %v", err)
			}
			after, found := f.snapshot(t)
			if !found || after != before {
				t.Errorf("stale slot changed route: before=%+v after=%+v", before, after)
			}
			check()
		})
	}
}

func TestCatalogRefAdmissionUpsertHealthyControl(t *testing.T) {
	f, view := newCatalogAdmissionRefFixture(t)
	view.RefViewID = "healthy-upsert"
	view.SelectorValue = "refs/heads/healthy-upsert"
	if err := f.c.UpsertRefView(f.ctx, view); err != nil {
		t.Fatalf("SETUP healthy UpsertRefView insert rejected: %v", err)
	}
	stored, found, err := f.c.GetRefView(f.ctx, view.RefViewID)
	if err != nil || !found || !reflect.DeepEqual(stored, view) {
		t.Fatalf("SETUP healthy UpsertRefView insert invalid: found=%v got=%+v err=%v", found, stored, err)
	}
	view.RouteEpoch++
	if err := f.c.UpsertRefView(f.ctx, view); err != nil {
		t.Fatalf("SETUP healthy UpsertRefView replacement rejected: %v", err)
	}
	stored, found, err = f.c.GetRefView(f.ctx, view.RefViewID)
	if err != nil || !found || !reflect.DeepEqual(stored, view) {
		t.Fatalf("SETUP healthy UpsertRefView replacement invalid: found=%v got=%+v err=%v", found, stored, err)
	}
}

func TestCatalogRefAdmissionUpsertRejectsRetiring(t *testing.T) {
	for _, existing := range []bool{false, true} {
		t.Run(fmt.Sprintf("existing=%v", existing), func(t *testing.T) {
			f, view := newCatalogAdmissionRefFixture(t)
			// Prove the exact writer accepts this valid shape before changing only
			// the active pointer and optional new unique selector/id.
			if err := f.c.UpsertRefView(f.ctx, view); err != nil {
				t.Fatalf("SETUP healthy UpsertRefView rejected: %v", err)
			}
			proposal := view
			if !existing {
				proposal.RefViewID = "new-upsert"
				proposal.SelectorValue = "refs/heads/new-upsert"
			}
			proposal.ActiveGenerationID = f.retiring
			proposal.RouteEpoch++
			check := catalogAdmissionRefNoWrites(t, f)
			err := f.c.UpsertRefView(f.ctx, proposal)
			t.Logf("retiring ref upsert existing=%v result=%v", existing, err)
			if err == nil {
				t.Error("actual UpsertRefView accepted retiring target")
			}
			stored, found, readErr := f.c.GetRefView(f.ctx, proposal.RefViewID)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if existing {
				if !found || !reflect.DeepEqual(stored, view) {
					t.Errorf("retiring upsert changed existing ref: found=%v got=%+v want=%+v", found, stored, view)
				}
			} else if found {
				t.Errorf("retiring upsert inserted ref: %+v", stored)
			}
			check()
		})
	}
}

type catalogAdmissionBuildSnapshot struct {
	refID, tree, profile, fingerprint, state, token, lastError string
	base, generation, epoch, progress                          int64
}

func catalogAdmissionBuildSnapshotAt(t *testing.T, f *catalogAdmissionFixture, id string) catalogAdmissionBuildSnapshot {
	t.Helper()
	var got catalogAdmissionBuildSnapshot
	err := f.s.db.QueryRowContext(f.ctx, `SELECT ref_view_id,desired_tree,base_generation_id,enrichment_profile,build_fingerprint,COALESCE(generation_id,0),captured_route_epoch,state,build_token,last_progress,error FROM ref_view_builds WHERE build_id=?`, id).Scan(&got.refID, &got.tree, &got.base, &got.profile, &got.fingerprint, &got.generation, &got.epoch, &got.state, &got.token, &got.progress, &got.lastError)
	if err != nil {
		t.Fatalf("SETUP/read build %s failed: %v", id, err)
	}
	return got
}

func seedCatalogAdmissionBuild(t *testing.T, f *catalogAdmissionFixture, view RefView, id string) {
	t.Helper()
	build := RefViewBuild{BuildID: id, RefViewID: view.RefViewID, DesiredTree: view.DesiredTree, BaseGenerationID: 0, EnrichmentProfile: view.EnrichmentProfile, BuildFingerprint: view.DesiredBuildFingerprint, CapturedRouteEpoch: view.RouteEpoch, State: ViewGenerationBuilding, BuildToken: id + "-token", CreatedAt: 1, LastProgress: 1}
	if err := f.c.UpsertRefViewBuild(f.ctx, build); err != nil {
		t.Fatalf("SETUP healthy ref build insert rejected: %v", err)
	}
	got := catalogAdmissionBuildSnapshotAt(t, f, id)
	want := catalogAdmissionBuildSnapshot{refID: view.RefViewID, tree: view.DesiredTree, profile: view.EnrichmentProfile, fingerprint: view.DesiredBuildFingerprint, state: string(ViewGenerationBuilding), token: id + "-token", epoch: view.RouteEpoch, progress: 1}
	if got != want {
		t.Fatalf("SETUP healthy build did not persist: got=%+v want=%+v", got, want)
	}
}

func catalogAdmissionAdoptionRequest(view RefView, generation int64) AdoptRefViewGenerationRequest {
	return AdoptRefViewGenerationRequest{RefViewID: view.RefViewID, ExpectedRouteEpoch: view.RouteEpoch, ExpectedDesiredTree: view.DesiredTree, ExpectedDesiredBuildFingerprint: view.DesiredBuildFingerprint, GenerationID: generation, ActiveRef: view.ActiveRef, ActiveCommit: view.ActiveCommit, ActiveTree: view.DesiredTree, ActiveBuildFingerprint: view.DesiredBuildFingerprint, ExactView: view.ExactView, LastProgress: 2}
}

func newCatalogAdmissionAdoptionFixture(t *testing.T, named bool) (*catalogAdmissionFixture, RefView, AdoptRefViewGenerationRequest) {
	t.Helper()
	f, view := newCatalogAdmissionRefFixture(t)
	request := catalogAdmissionAdoptionRequest(view, f.commit)
	if named {
		seedCatalogAdmissionBuild(t, f, view, "control-build")
		request.BuildID = "control-build"
		request.BuildToken = "control-build-token"
	}
	if err := f.c.AdoptRefViewGeneration(f.ctx, request); err != nil {
		t.Fatalf("SETUP healthy adoption named=%v rejected: %v", named, err)
	}
	want := view
	want.RouteEpoch++
	want.ActiveGenerationID = f.commit
	got, found, err := f.c.GetRefView(f.ctx, view.RefViewID)
	if err != nil || !found || !reflect.DeepEqual(got, want) {
		t.Fatalf("SETUP healthy adoption did not persist named=%v: found=%v got=%+v want=%+v err=%v", named, found, got, want, err)
	}
	if named {
		stored := catalogAdmissionBuildSnapshotAt(t, f, "control-build")
		if stored.state != string(ViewGenerationReady) || stored.generation != f.commit || stored.progress != 2 {
			t.Fatalf("SETUP healthy named adoption did not close build: %+v", stored)
		}
	}
	view = got
	request = catalogAdmissionAdoptionRequest(view, f.commit)
	if named {
		seedCatalogAdmissionBuild(t, f, view, "attempt-build")
		request.BuildID = "attempt-build"
		request.BuildToken = "attempt-build-token"
	}
	t.Logf("healthy adoption named=%v proved before negative request", named)
	return f, view, request
}

func TestCatalogRefAdmissionAdoptHealthyControls(t *testing.T) {
	for _, named := range []bool{false, true} {
		t.Run(fmt.Sprintf("named=%v", named), func(t *testing.T) { newCatalogAdmissionAdoptionFixture(t, named) })
	}
}

func TestCatalogRefAdmissionAdoptRejectsRetiring(t *testing.T) {
	for _, named := range []bool{false, true} {
		t.Run(fmt.Sprintf("named=%v", named), func(t *testing.T) {
			f, view, request := newCatalogAdmissionAdoptionFixture(t, named)
			var before catalogAdmissionBuildSnapshot
			if named {
				before = catalogAdmissionBuildSnapshotAt(t, f, request.BuildID)
			}
			request.GenerationID = f.retiring
			check := catalogAdmissionRefNoWrites(t, f)
			err := f.c.AdoptRefViewGeneration(f.ctx, request)
			t.Logf("retiring adoption named=%v result=%v", named, err)
			if err == nil {
				t.Error("actual AdoptRefViewGeneration accepted retiring target")
			}
			got, found, readErr := f.c.GetRefView(f.ctx, view.RefViewID)
			if readErr != nil || !found || !reflect.DeepEqual(got, view) {
				t.Errorf("retiring adoption changed ref: found=%v got=%+v want=%+v err=%v", found, got, view, readErr)
			}
			if named {
				after := catalogAdmissionBuildSnapshotAt(t, f, request.BuildID)
				if after != before {
					t.Errorf("rejected adoption changed named build: before=%+v after=%+v", before, after)
				}
			}
			check()
		})
	}
}

func TestCatalogRefAdmissionAdoptStaleControls(t *testing.T) {
	for _, named := range []bool{false, true} {
		reasons := []string{"epoch", "tree", "fingerprint"}
		if named {
			reasons = append(reasons, "token", "token-and-epoch")
		}
		for _, reason := range reasons {
			t.Run(fmt.Sprintf("named=%v/%s", named, reason), func(t *testing.T) {
				f, view, request := newCatalogAdmissionAdoptionFixture(t, named)
				var before catalogAdmissionBuildSnapshot
				if named {
					before = catalogAdmissionBuildSnapshotAt(t, f, request.BuildID)
				}
				request.GenerationID = f.retiring
				switch reason {
				case "epoch":
					request.ExpectedRouteEpoch--
				case "tree":
					request.ExpectedDesiredTree = "stale-tree"
				case "fingerprint":
					request.ExpectedDesiredBuildFingerprint = "stale-fingerprint"
				case "token":
					request.BuildToken = "stale-token"
				case "token-and-epoch":
					request.BuildToken = "stale-token"
					request.ExpectedRouteEpoch--
				}
				check := catalogAdmissionRefNoWrites(t, f)
				err := f.c.AdoptRefViewGeneration(f.ctx, request)
				t.Logf("stale adoption named=%v reason=%s result=%v", named, reason, err)
				if !errors.Is(err, ErrCatalogStaleGuard) {
					t.Errorf("stale adoption error masked by retiring target: %v", err)
				}
				if strings.HasPrefix(reason, "token") && err != nil && !strings.Contains(err.Error(), "ref view build") {
					t.Errorf("token guard did not retain precedence: %v", err)
				}
				got, found, readErr := f.c.GetRefView(f.ctx, view.RefViewID)
				if readErr != nil || !found || !reflect.DeepEqual(got, view) {
					t.Errorf("stale adoption changed ref: found=%v got=%+v err=%v", found, got, readErr)
				}
				if named {
					after := catalogAdmissionBuildSnapshotAt(t, f, request.BuildID)
					if after != before {
						t.Errorf("stale adoption changed named build: before=%+v after=%+v", before, after)
					}
				}
				check()
			})
		}
	}
}

type dedicatedIdentityAdmissionFixture struct {
	s                 *Store
	c                 *Catalog
	ctx               context.Context
	graph             DedicatedGraph
	healthy, retiring int64
}

func newDedicatedIdentityAdmissionFixture(t *testing.T) *dedicatedIdentityAdmissionFixture {
	t.Helper()
	root := t.TempDir()
	s, err := Open(filepath.Join(root, "identity-admission.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	f := &dedicatedIdentityAdmissionFixture{s: s, c: s.Catalog(), ctx: ctx}
	family := RepositoryFamily{FamilyID: "family", CommonDirIdentity: filepath.Join(root, "git"), State: "active"}
	if err := f.c.UpsertRepositoryFamily(ctx, family); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"owner", "insert-owner"} {
		owner := Checkout{CheckoutID: name, Incarnation: name + "-incarnation", FamilyID: family.FamilyID, RootPath: filepath.Join(root, name), GitDir: filepath.Join(root, "git", name), AdminName: name, State: CheckoutStateReady, DesiredMode: CheckoutModeDedicated, EffectiveMode: CheckoutModeDedicated, HeadTree: "tree"}
		if err := f.c.UpsertCheckout(ctx, owner); err != nil {
			t.Fatal(err)
		}
	}
	f.graph = DedicatedGraph{GraphID: "graph", OwnerCheckoutID: "owner", RepoPrefix: "repo", FamilyID: family.FamilyID, IsPrimaryBase: true, State: "ready"}
	if err := f.c.UpsertDedicatedGraph(ctx, f.graph); err != nil {
		t.Fatal(err)
	}
	for i := range 2 {
		id, _, err := s.BeginPayloadGeneration(ctx, PayloadGenerationRequest{OwnerKind: "dedicated_graph", GraphID: f.graph.GraphID, CheckoutID: "owner", GenerationKind: "commit", LayerID: fmt.Sprintf("layer-%d", i), TreeOID: fmt.Sprintf("tree-%d", i)})
		if err != nil {
			t.Fatal(err)
		}
		if err := s.PublishPayloadGeneration(ctx, id, 2); err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			f.healthy = id
		} else {
			f.retiring = id
		}
	}
	if err := f.c.SetViewGenerationState(ctx, f.retiring, ViewGenerationRetiring, ViewGenerationReady); err != nil {
		t.Fatal(err)
	}
	// Independent healthy legacy update and its persisted value are prerequisites
	// to counting any retirement-refusal result from this fixture.
	f.graph.ActiveGenerationID = f.healthy
	if err := f.c.UpsertDedicatedGraph(ctx, f.graph); err != nil {
		t.Fatalf("SETUP healthy legacy update: %v", err)
	}
	if got, found := f.row(t, f.graph.GraphID); !found || got != f.graph {
		t.Fatalf("SETUP healthy pointer not stored: %+v %v", got, found)
	}
	return f
}

func (f *dedicatedIdentityAdmissionFixture) row(t *testing.T, graphID string) (DedicatedGraph, bool) {
	t.Helper()
	got, found, err := f.c.GetDedicatedGraph(f.ctx, graphID)
	if err != nil {
		t.Fatal(err)
	}
	return got, found
}

func (f *dedicatedIdentityAdmissionFixture) freshProposal() DedicatedGraph {
	return DedicatedGraph{GraphID: "fresh-graph", OwnerCheckoutID: "insert-owner", RepoPrefix: "fresh-repo", FamilyID: f.graph.FamilyID, ActiveGenerationID: f.healthy, State: "ready"}
}

func (f *dedicatedIdentityAdmissionFixture) healthyInsertControl(t *testing.T) DedicatedGraph {
	t.Helper()
	proposal := f.freshProposal()
	// This exercises the existing generic pointer API's state contract, not the
	// stricter publication protocol's graph-ownership/ancestry validation.
	if err := f.c.UpsertDedicatedGraph(f.ctx, proposal); err != nil {
		t.Fatalf("SETUP healthy positive insert: %v", err)
	}
	if got, found := f.row(t, proposal.GraphID); !found || got != proposal {
		t.Fatalf("SETUP positive insert not stored: %+v %v", got, found)
	}
	if err := f.c.DeleteDedicatedGraph(f.ctx, proposal.GraphID); err != nil {
		t.Fatal(err)
	}
	if _, found := f.row(t, proposal.GraphID); found {
		t.Fatal("SETUP positive-control row remains")
	}
	return proposal
}

func (f *dedicatedIdentityAdmissionFixture) audit(t *testing.T) func() {
	t.Helper()
	err := f.c.withTx(f.ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(f.ctx, `CREATE TABLE identity_admission_audit(n INTEGER NOT NULL); INSERT INTO identity_admission_audit VALUES(0)`); err != nil {
			return err
		}
		for _, table := range []string{"dedicated_graphs", "dedicated_base_publications"} {
			for _, op := range []string{"INSERT", "UPDATE", "DELETE"} {
				query := fmt.Sprintf(`CREATE TRIGGER identity_admission_%s_%s AFTER %s ON %s BEGIN UPDATE identity_admission_audit SET n=n+1; END`, table, op, op, table)
				if _, err := tx.ExecContext(f.ctx, query); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return func() {
		var writes int
		if err := f.s.db.QueryRowContext(f.ctx, `SELECT n FROM identity_admission_audit`).Scan(&writes); err != nil {
			t.Fatal(err)
		}
		if writes != 0 {
			t.Errorf("refused/no-op identity request committed %d logical row writes", writes)
		}
	}
}

func TestDedicatedIdentityAdmissionHealthyInsertUpdateAndZeroClear(t *testing.T) {
	f := newDedicatedIdentityAdmissionFixture(t)
	f.healthyInsertControl(t)
	proposal := f.graph
	proposal.ActiveGenerationID = 0
	if err := f.c.UpsertDedicatedGraph(f.ctx, proposal); err != nil {
		t.Fatal(err)
	}
	if got, found := f.row(t, proposal.GraphID); !found || got != proposal {
		t.Fatalf("zero clear: %+v %v", got, found)
	}
	if err := f.c.UpsertDedicatedGraph(f.ctx, f.graph); err != nil {
		t.Fatal(err)
	}
	check := f.audit(t)
	for range 20 {
		if err := f.c.UpsertDedicatedGraph(f.ctx, f.graph); err != nil {
			t.Fatal(err)
		}
	}
	check()
}

func TestDedicatedIdentityAdmissionRejectsRetiring(t *testing.T) {
	for _, insert := range []bool{false, true} {
		t.Run(fmt.Sprint("insert=", insert), func(t *testing.T) {
			f := newDedicatedIdentityAdmissionFixture(t)
			proposal := f.graph
			if insert {
				proposal = f.healthyInsertControl(t)
			}
			before, foundBefore := f.row(t, proposal.GraphID)
			check := f.audit(t)
			proposal.ActiveGenerationID = f.retiring
			proposal.State = "offline"
			err := f.c.UpsertDedicatedGraph(f.ctx, proposal)
			if !errors.Is(err, ErrCatalogGenerationRetiring) {
				t.Errorf("Retiring target not refused by state guard: %v", err)
			}
			after, foundAfter := f.row(t, proposal.GraphID)
			if foundAfter != foundBefore || after != before {
				t.Errorf("refusal did not roll back all identity fields: before=%+v/%v after=%+v/%v", before, foundBefore, after, foundAfter)
			}
			check()
		})
	}
}

func TestDedicatedIdentityAdmissionMissingAndSQLFailureControls(t *testing.T) {
	for _, insert := range []bool{false, true} {
		t.Run(fmt.Sprint("insert=", insert), func(t *testing.T) {
			f := newDedicatedIdentityAdmissionFixture(t)
			proposal := f.graph
			if insert {
				proposal = f.healthyInsertControl(t)
			}
			before, foundBefore := f.row(t, proposal.GraphID)
			check := f.audit(t)
			proposal.ActiveGenerationID = f.retiring + 100000
			if err := f.c.UpsertDedicatedGraph(f.ctx, proposal); err == nil {
				t.Error("missing positive target accepted")
			}
			after, foundAfter := f.row(t, proposal.GraphID)
			if foundAfter != foundBefore || after != before {
				t.Errorf("missing-target request committed state: %+v %v", after, foundAfter)
			}
			// Original SQL identity/foreign-key errors precede target admission.
			// Do not demand a universal missing-target sentinel from earlier case.
			proposal.OwnerCheckoutID = "missing-owner"
			proposal.ActiveGenerationID = f.retiring
			err := f.c.UpsertDedicatedGraph(f.ctx, proposal)
			if err == nil || errors.Is(err, ErrCatalogGenerationRetiring) {
				t.Errorf("original invalid-owner SQL error precedence lost: %v", err)
			}
			check()
		})
	}
}

func TestDedicatedIdentityAdmissionGuardedIgnoredTargetsRemainNoop(t *testing.T) {
	f := newDedicatedIdentityAdmissionFixture(t)
	if _, err := f.c.AcquireDedicatedBaseAuthority(f.ctx, AcquireDedicatedBaseAuthorityRequest{GraphID: f.graph.GraphID, Owner: DedicatedBaseOwner{CheckoutID: "owner", Incarnation: "owner-incarnation"}, Token: "authority"}); err != nil {
		t.Fatal(err)
	}
	check := f.audit(t)
	for _, target := range []int64{0, f.healthy, f.retiring, f.retiring + 100000} {
		proposal := f.graph
		proposal.ActiveGenerationID = target
		for range 20 {
			if err := f.c.UpsertDedicatedGraph(f.ctx, proposal); err != nil {
				t.Errorf("ignored proposal %d rejected: %v", target, err)
				break
			}
		}
		if got, found := f.row(t, f.graph.GraphID); !found || got != f.graph {
			t.Errorf("ignored proposal changed graph: %+v %v", got, found)
		}
	}
	proposal := f.graph
	proposal.OwnerCheckoutID = "missing-owner"
	proposal.ActiveGenerationID = f.retiring
	if err := f.c.UpsertDedicatedGraph(f.ctx, proposal); !errors.Is(err, ErrCatalogStaleGuard) {
		t.Errorf("authority identity guard must precede SQL/target checks: %v", err)
	}
	check()
}

// This fixture is intentionally self-contained. It uses the same verified public
// catalog shapes as the publication regressions, without relying on a copied or
// changed existing test helper. No production SQL or method is replaced.
type publicationRetirementFixture struct {
	s         *Store
	c         *Catalog
	ctx       context.Context
	authority DedicatedBaseAuthority
	desire    DedicatedBaseDesire
}

func newPublicationRetirementFixture(t *testing.T) *publicationRetirementFixture {
	t.Helper()
	root := t.TempDir()
	s, err := Open(filepath.Join(root, "publication-retirement.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	f := &publicationRetirementFixture{s: s, c: s.Catalog(), ctx: ctx}
	family := RepositoryFamily{FamilyID: "family", CommonDirIdentity: filepath.Join(root, "git"), State: "active"}
	if err := f.c.UpsertRepositoryFamily(ctx, family); err != nil {
		t.Fatal(err)
	}
	owner := Checkout{CheckoutID: "owner", Incarnation: "incarnation", FamilyID: family.FamilyID, RootPath: root, GitDir: family.CommonDirIdentity, AdminName: "main", State: CheckoutStateReady, DesiredMode: CheckoutModeDedicated, EffectiveMode: CheckoutModeDedicated, HeadTree: "live-is-not-snapshot"}
	if err := f.c.UpsertCheckout(ctx, owner); err != nil {
		t.Fatal(err)
	}
	if err := f.c.UpsertDedicatedGraph(ctx, DedicatedGraph{GraphID: "graph", OwnerCheckoutID: owner.CheckoutID, RepoPrefix: "repo", FamilyID: family.FamilyID, IsPrimaryBase: true, State: "ready"}); err != nil {
		t.Fatal(err)
	}
	f.authority, err = f.c.AcquireDedicatedBaseAuthority(ctx, AcquireDedicatedBaseAuthorityRequest{GraphID: "graph", Owner: DedicatedBaseOwner{CheckoutID: owner.CheckoutID, Incarnation: owner.Incarnation}, Token: "authority"})
	if err != nil {
		t.Fatal(err)
	}
	f.desire, err = f.c.RecordDedicatedBaseDesire(ctx, RecordDedicatedBaseDesireRequest{Authority: f.authority, Identity: DedicatedBaseIdentity{TreeOID: "tree-a", ConfigHash: "config", ExtractorVersions: "extractors", ResolverVersion: "resolver"}})
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *publicationRetirementFixture) claim(t *testing.T, token string, active, base int64) DedicatedBaseBuildClaim {
	t.Helper()
	claim, err := f.c.ClaimDedicatedBaseBuild(f.ctx, ClaimDedicatedBaseBuildRequest{Desire: f.desire, ExpectedActiveGenerationID: active, BaseGenerationID: base, AttemptToken: token, CreatedAt: 1})
	if err != nil || claim.GenerationID <= 0 || claim.BaseGenerationID != base {
		t.Fatalf("SETUP claim=%+v err=%v", claim, err)
	}
	return claim
}

func (f *publicationRetirementFixture) publish(t *testing.T, claim DedicatedBaseBuildClaim) {
	t.Helper()
	if err := f.c.PublishViewGeneration(f.ctx, claim.GenerationID, 2); err != nil {
		t.Fatal(err)
	}
}

func (f *publicationRetirementFixture) adopt(t *testing.T, claim DedicatedBaseBuildClaim) {
	t.Helper()
	got, err := f.c.AdoptDedicatedBaseGeneration(f.ctx, AdoptDedicatedBaseGenerationRequest{Claim: claim})
	if err != nil || got.GenerationID != claim.GenerationID {
		t.Fatalf("SETUP adoption=%+v err=%v", got, err)
	}
}

func (f *publicationRetirementFixture) observe(t *testing.T, tree string) {
	t.Helper()
	identity := f.desire.Identity
	identity.TreeOID = tree
	var err error
	f.desire, err = f.c.RecordDedicatedBaseDesire(f.ctx, RecordDedicatedBaseDesireRequest{Authority: f.authority, ExpectedDesiredEpoch: f.desire.Epoch, Identity: identity})
	if err != nil {
		t.Fatal(err)
	}
}

func (f *publicationRetirementFixture) association(t *testing.T, generationID int64, state string) {
	t.Helper()
	var count int
	var gotID int64
	var gotState string
	err := f.s.db.QueryRowContext(f.ctx, `SELECT count(*),COALESCE(MAX(generation_id),0),COALESCE(MAX(attempt_state),'') FROM dedicated_base_publications WHERE graph_id=?`, "graph").Scan(&count, &gotID, &gotState)
	if err != nil || count != 1 || gotID != generationID || gotState != state {
		t.Fatalf("current association: count=%d generation=%d state=%s want=%d/%s err=%v", count, gotID, gotState, generationID, state, err)
	}
}

func (f *publicationRetirementFixture) noOtherRoots(t *testing.T, generationID int64) {
	t.Helper()
	var checkout, ref, child, dedicated bool
	err := f.s.db.QueryRowContext(f.ctx, `SELECT
EXISTS(SELECT 1 FROM checkout_routes WHERE commit_generation_id=? OR dirty_generation_id=?),
EXISTS(SELECT 1 FROM ref_views WHERE active_generation_id=?),
EXISTS(SELECT 1 FROM view_generations WHERE base_generation_id=?),
EXISTS(SELECT 1 FROM dedicated_graphs WHERE active_generation_id=?)`,
		generationID, generationID, generationID, generationID, generationID).Scan(&checkout, &ref, &child, &dedicated)
	if err != nil || checkout || ref || child || dedicated {
		t.Fatalf("SETUP other root prevents isolation: checkout=%v ref=%v child=%v dedicated=%v err=%v", checkout, ref, child, dedicated, err)
	}
}

func (f *publicationRetirementFixture) held(t *testing.T, generationID int64) {
	t.Helper()
	f.noOtherRoots(t, generationID)
	before, found, err := f.c.GetViewGeneration(f.ctx, generationID)
	if err != nil || !found {
		t.Fatalf("SETUP generation: found=%v err=%v", found, err)
	}
	refs, err := f.c.ViewGenerationReferences(f.ctx, generationID)
	if err != nil || !refs.Any() {
		t.Errorf("diagnostic omitted current publication root: refs=%+v err=%v", refs, err)
	}
	if err := f.c.BeginViewGenerationRetirement(f.ctx, generationID); !errors.Is(err, ErrCatalogGenerationReferenced) {
		t.Errorf("atomic fence did not retain current publication: %v", err)
	}
	if err := f.c.DeleteViewGeneration(f.ctx, generationID); !errors.Is(err, ErrCatalogGenerationReferenced) {
		t.Errorf("final deletion did not retain current publication: %v", err)
	}
	after, found, err := f.c.GetViewGeneration(f.ctx, generationID)
	if err != nil || !found || after.State != before.State {
		t.Errorf("held generation changed: before=%s after=%s found=%v err=%v", before.State, after.State, found, err)
	}
}

func (f *publicationRetirementFixture) unheldAndDeletable(t *testing.T, generationID int64) {
	t.Helper()
	f.noOtherRoots(t, generationID)
	refs, err := f.c.ViewGenerationReferences(f.ctx, generationID)
	if err != nil || refs.Any() {
		t.Fatalf("old publication remained a historical root: refs=%+v err=%v", refs, err)
	}
	if err := f.c.BeginViewGenerationRetirement(f.ctx, generationID); err != nil {
		t.Fatalf("unheld generation could not enter retirement: %v", err)
	}
	if err := f.c.DeleteViewGeneration(f.ctx, generationID); err != nil {
		t.Fatalf("unheld catalog row could not be deleted: %v", err)
	}
	if _, found, err := f.c.GetViewGeneration(f.ctx, generationID); err != nil || found {
		t.Fatalf("retired row remains: found=%v err=%v", found, err)
	}
}

func TestCatalogPublicationCurrentAssociationRetirementRoots(t *testing.T) {
	for _, state := range []string{"building", "ready", "adopted"} {
		t.Run(state, func(t *testing.T) {
			f := newPublicationRetirementFixture(t)
			claim := f.claim(t, "first", 0, 0)
			if state != "building" {
				f.publish(t, claim)
			}
			if state == "ready" {
				// Recover an already-published candidate through the real protocol:
				// retry binds that reusable candidate as this graph's current Ready
				// association, rather than fabricating an attempt_state value.
				if err := f.c.FailDedicatedBaseBuild(f.ctx, FailDedicatedBaseBuildRequest{Claim: claim, Error: "test recovery after physical publication"}); err != nil {
					t.Fatal(err)
				}
				claim = f.claim(t, "ready-retry", 0, 0)
			}
			if state == "adopted" {
				f.adopt(t, claim)
				// Test-only isolation: deliberately clear the separate active pointer
				// AFTER a real adoption. This is not represented as a normal runtime
				// transition; it proves the publication predicate independently.
				if err := f.c.withTx(f.ctx, func(tx *sql.Tx) error {
					_, err := tx.ExecContext(f.ctx, `UPDATE dedicated_graphs SET active_generation_id=NULL WHERE graph_id=?`, "graph")
					return err
				}); err != nil {
					t.Fatal(err)
				}
			}
			f.association(t, claim.GenerationID, state)
			f.held(t, claim.GenerationID)
			f.observe(t, "tree-b")
			f.association(t, 0, "idle")
			f.unheldAndDeletable(t, claim.GenerationID)
		})
	}
}

func TestCatalogPublicationReplacementDoesNotPinHistoricalBase(t *testing.T) {
	f := newPublicationRetirementFixture(t)
	first := f.claim(t, "first", 0, 0)
	f.publish(t, first)
	f.adopt(t, first)
	f.association(t, first.GenerationID, "adopted")
	f.observe(t, "tree-b")
	f.association(t, 0, "idle")
	// The former active pointer is a legitimate root until its replacement
	// adopts. Clearing the publication association alone must not retire it.
	if err := f.c.BeginViewGenerationRetirement(f.ctx, first.GenerationID); !errors.Is(err, ErrCatalogGenerationReferenced) {
		t.Fatalf("previous active base lost its independent root: %v", err)
	}
	// A full seed intentionally has no parent: this isolates the history-pin
	// invariant from legitimate inherited-delta ancestry retention.
	second := f.claim(t, "second", first.GenerationID, 0)
	if second.GenerationID == first.GenerationID {
		t.Fatal("SETUP changed tree incorrectly reused the old generation")
	}
	f.publish(t, second)
	f.adopt(t, second)
	f.association(t, second.GenerationID, "adopted")
	f.unheldAndDeletable(t, first.GenerationID)
	// Removing historical metadata must not remove/rewrite the current claim.
	f.association(t, second.GenerationID, "adopted")
}

type privateManagedTxFixture struct {
	store *Store
	id    int64
}

func privateNewManagedTxFixture(t testing.TB, mode string) privateManagedTxFixture {
	t.Helper()
	root := t.TempDir()
	dbPath := filepath.Join(root, "managed-tx.sqlite")
	if mode == "memory" {
		dbPath = ":memory:"
	}
	s, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	if mode == "bulk" || mode == "memory" {
		s.BeginBulkLoad()
	}
	if mode == "bulk" && s.bulkConn == nil {
		t.Fatal("bulk fixture did not pin its connection")
	}
	if mode != "bulk" && s.bulkConn != nil {
		t.Fatal("non-bulk fixture unexpectedly pinned a connection")
	}
	if mode == "memory" && (s.db != s.writerDB || s.db.Stats().MaxOpenConnections != 1) {
		t.Fatal("memory fixture does not share the maximum-one connection pool")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	c := s.Catalog()
	family := RepositoryFamily{FamilyID: "family", CommonDirIdentity: filepath.Join(root, "git"), State: "active"}
	if err := c.UpsertRepositoryFamily(ctx, family); err != nil {
		t.Fatal(err)
	}
	owner := Checkout{
		CheckoutID: "owner", Incarnation: "incarnation", FamilyID: family.FamilyID,
		RootPath: root, GitDir: family.CommonDirIdentity, AdminName: "main",
		State: CheckoutStateReady, DesiredMode: CheckoutModeDedicated,
		EffectiveMode: CheckoutModeDedicated, HeadTree: "tree",
	}
	if err := c.UpsertCheckout(ctx, owner); err != nil {
		t.Fatal(err)
	}
	if err := c.UpsertDedicatedGraph(ctx, DedicatedGraph{
		GraphID: "graph", OwnerCheckoutID: owner.CheckoutID, RepoPrefix: "repo",
		FamilyID: family.FamilyID, IsPrimaryBase: true, State: "ready",
	}); err != nil {
		t.Fatal(err)
	}
	id, _, err := s.BeginPayloadGeneration(ctx, PayloadGenerationRequest{
		OwnerKind: "dedicated_graph", GraphID: "graph", CheckoutID: "owner",
		GenerationKind: "commit", LayerID: "layer", TreeOID: "tree",
	})
	if err != nil {
		t.Fatal(err)
	}
	return privateManagedTxFixture{store: s, id: id}
}

// This exercises the predicate using an actual selected writer transaction,
// not a pool precheck. The owning base handle avoids preempting this component
// assertion through the process seal. Public write admission is tested below.
func privateWithManagedCheckTx(ctx context.Context, s *Store, fn func(*sql.Tx) error) error {
	if err := s.writeMu.LockContext(ctx); err != nil {
		return err
	}
	defer s.writeMu.Unlock()
	tx, err := s.beginWriteContext(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

func TestPrivateManagedPayloadTxStatesAndConnections(t *testing.T) {
	for _, mode := range []string{"file", "bulk", "memory"} {
		t.Run(mode, func(t *testing.T) {
			f := privateNewManagedTxFixture(t, mode)
			for _, state := range []ViewGenerationState{
				ViewGenerationBuilding, ViewGenerationReady, ViewGenerationSuperseded,
				ViewGenerationFailed, ViewGenerationRetiring,
			} {
				t.Run(string(state), func(t *testing.T) {
					ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
					defer cancel()
					if err := f.store.Catalog().SetViewGenerationState(ctx, f.id, state); err != nil {
						t.Fatal(err)
					}
					err := privateWithManagedCheckTx(ctx, f.store, func(tx *sql.Tx) error {
						return checkManagedPayloadWriteTx(ctx, tx, f.id)
					})
					if state == ViewGenerationBuilding {
						if err != nil {
							t.Fatalf("building refused: %v", err)
						}
					} else if !errors.Is(err, ErrPayloadGenerationSealed) {
						t.Fatalf("state %s admission: %v", state, err)
					}
				})
			}
		})
	}
}

func TestPrivateManagedPayloadTxSeesItsOwnUncommittedFence(t *testing.T) {
	for _, mode := range []string{"file", "bulk", "memory"} {
		t.Run(mode, func(t *testing.T) {
			f := privateNewManagedTxFixture(t, mode)
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			err := privateWithManagedCheckTx(ctx, f.store, func(tx *sql.Tx) error {
				if _, err := tx.ExecContext(ctx,
					`UPDATE view_generations SET state = ? WHERE generation_id = ?`, ViewGenerationRetiring, f.id); err != nil {
					return err
				}
				// A separate pool read cannot see this transaction's uncommitted
				// state; on supported memory it would also await the held sole
				// connection. This is a predicate/connection test, not a GC race.
				return checkManagedPayloadWriteTx(ctx, tx, f.id)
			})
			if !errors.Is(err, ErrPayloadGenerationSealed) {
				t.Fatalf("predicate did not observe its transaction: %v", err)
			}
			row, found, err := f.store.Catalog().GetViewGeneration(ctx, f.id)
			if err != nil || !found || row.State != ViewGenerationBuilding {
				t.Fatalf("refused transaction was not rolled back: row=%+v found=%v err=%v", row, found, err)
			}
		})
	}
}

func TestPrivateManagedPayloadTxMissingDoesNotChangeGenericCompatibility(t *testing.T) {
	for _, mode := range []string{"file", "bulk", "memory"} {
		t.Run(mode, func(t *testing.T) {
			f := privateNewManagedTxFixture(t, mode)
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			id := f.id + 1000
			if err := privateWithManagedCheckTx(ctx, f.store, func(tx *sql.Tx) error {
				return checkManagedPayloadWriteTx(ctx, tx, id)
			}); !errors.Is(err, ErrPayloadGenerationSealed) {
				t.Fatalf("missing generation admitted: %v", err)
			} else if errors.Is(err, sql.ErrNoRows) {
				t.Fatal("missing managed refusal retained the generic no-rows success sentinel")
			}
			h := f.store.AtGeneration(id)
			node := &graph.Node{ID: "repo/a.go::Generic", Kind: graph.KindFunction,
				Name: "Generic", FilePath: "repo/a.go", RepoPrefix: "repo"}
			h.AddBatch([]*graph.Node{node}, nil)
			if h.GetNode(node.ID) == nil {
				t.Fatal("predicate changed generic unmanaged compatibility")
			}
		})
	}
}

func TestPrivateManagedPayloadTxContextAndSQLCauses(t *testing.T) {
	if err := checkManagedPayloadWriteTx(nil, nil, 1); !errors.Is(err, ErrCatalogInvalidValue) { //nolint:staticcheck // Verify explicit nil-context rejection.
		t.Fatalf("nil context: %v", err)
	}
	ctx := context.Background()
	if err := checkManagedPayloadWriteTx(ctx, nil, 1); !errors.Is(err, ErrCatalogInvalidValue) {
		t.Fatalf("nil transaction: %v", err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if err := checkManagedPayloadWriteTx(canceled, nil, 1); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled context cause: %v", err)
	}
	f := privateNewManagedTxFixture(t, "file")
	deadline, stop := context.WithTimeout(ctx, 2*time.Second)
	defer stop()
	if err := f.store.writeMu.LockContext(deadline); err != nil {
		t.Fatal(err)
	}
	defer f.store.writeMu.Unlock()
	tx, err := f.store.beginWriteContext(deadline)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := checkManagedPayloadWriteTx(deadline, tx, 0); !errors.Is(err, ErrCatalogInvalidValue) {
		t.Fatalf("base generation in qualified predicate: %v", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if err := checkManagedPayloadWriteTx(deadline, tx, f.id); !errors.Is(err, sql.ErrTxDone) {
		t.Fatalf("SQL failure cause lost: %v", err)
	}
}

// These are constructor-only controls. Public write admission and
// same-generation forwarding are exercised separately below.
func TestPrivateManagedFactoryRestrictionOnly(t *testing.T) {
	for _, receiver := range []*Store{nil, {}} {
		if h, err := receiver.AtManagedGeneration(1); h != nil || !errors.Is(err, ErrCatalogInvalidValue) {
			t.Fatalf("uninitialized receiver: handle=%v err=%v", h != nil, err)
		}
	}
	f := privateNewManagedTxFixture(t, "file")
	for _, id := range []int64{-1, 0} {
		if h, err := f.store.AtManagedGeneration(id); h != nil || !errors.Is(err, ErrCatalogInvalidValue) {
			t.Fatalf("nonpositive id%d: handle=%v err=%v", id, h != nil, err)
		}
	}
	// No catalog row is required or created by constructing a restricted handle.
	id := f.id + 1000
	h, err := f.store.AtManagedGeneration(id)
	if err != nil || h == nil {
		t.Fatalf("restriction-only construction: %v", err)
	}
	if h.storeCore != f.store.storeCore || h.viewGen != id || !h.managedPayloadGeneration || h.ownsCore {
		t.Fatal("constructor did not return the expected restricted borrowed handle")
	}
	if _, found, err := f.store.Catalog().GetViewGeneration(context.Background(), id); err != nil || found {
		t.Fatalf("constructor created lifecycle authority: found=%v err=%v", found, err)
	}
	if base := h.atBase(); base.viewGen != 0 || base.managedPayloadGeneration || base.ownsCore {
		t.Fatal("base control derivation retained payload qualification or ownership")
	}
}

func TestPrivateManagedGenerationForwarding(t *testing.T) {
	f := privateNewManagedTxFixture(t, "file")
	h, err := f.store.AtManagedGeneration(f.id)
	if err != nil {
		t.Fatal(err)
	}
	for _, same := range []*Store{h.AtGeneration(f.id), h.AtGeneration(f.id).AtGeneration(f.id)} {
		if !same.managedPayloadGeneration || same.viewGen != f.id || same.storeCore != f.store.storeCore || same.ownsCore {
			t.Fatal("same-generation derivation lost its restriction or borrowed identity")
		}
	}
	for _, unqualified := range []*Store{h.AtGeneration(0), h.atBase(), h.AtGeneration(f.id + 1), f.store.AtGeneration(f.id)} {
		if unqualified.managedPayloadGeneration || unqualified.ownsCore {
			t.Fatal("restriction escaped its explicit generation qualification")
		}
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	if _, found, err := f.store.Catalog().GetViewGeneration(context.Background(), f.id); err != nil || !found {
		t.Fatalf("borrowed Close closed the core: found=%v err=%v", found, err)
	}
}

func TestPrivateBeginPayloadReturnsManagedGeneration(t *testing.T) {
	for _, mode := range []string{"file", "bulk", "memory"} {
		t.Run(mode, func(t *testing.T) {
			f := privateNewManagedTxFixture(t, mode)
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			id, h, adopted, err := f.store.BeginPayloadGenerationWithStatus(ctx, PayloadGenerationRequest{
				OwnerKind: "dedicated_graph", GraphID: "graph", CheckoutID: "owner",
				GenerationKind: "commit", LayerID: "another-layer", TreeOID: "another-tree",
			})
			if err != nil {
				t.Fatal(err)
			}
			if adopted || id <= 0 || h == nil || h.viewGen != id || !h.managedPayloadGeneration || h.ownsCore {
				t.Fatalf("Begin did not return a newly restricted borrowed handle: id=%d adopted=%v handle=%+v", id, adopted, h)
			}
		})
	}
}

func privateManagedPanic(fn func()) (caught any) {
	defer func() { caught = recover() }()
	fn()
	return nil
}

func TestPrivateManagedAddBatchAdmission(t *testing.T) {
	for _, mode := range []string{"file", "bulk", "memory"} {
		for _, state := range []string{"building", "ready", "superseded", "failed", "retiring", "missing"} {
			t.Run(mode+"/"+state, func(t *testing.T) {
				f := privateNewManagedTxFixture(t, mode)
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				base := &graph.Node{ID: "repo/base.go::Sentinel", Kind: graph.KindFunction, Name: "Sentinel", FilePath: "repo/base.go", RepoPrefix: "repo"}
				f.store.AddBatch([]*graph.Node{base}, nil)
				id := f.id
				if state == "missing" {
					id += 1000
				} else if err := f.store.Catalog().SetViewGenerationState(ctx, id, ViewGenerationState(state)); err != nil {
					t.Fatal(err)
				}
				h, err := f.store.AtManagedGeneration(id)
				if err != nil {
					t.Fatal(err)
				}
				// Derive once more so actual public payload writes exercise the
				// same-generation provenance seam as well as the factory.
				h = h.AtGeneration(id)
				node := &graph.Node{ID: "repo/a.go::Managed", Kind: graph.KindFunction, Name: "Managed", FilePath: "repo/a.go", RepoPrefix: "repo"}
				caught := privateManagedPanic(func() { h.AddBatch([]*graph.Node{node}, nil) })
				if state == "building" {
					if caught != nil || h.GetNode(node.ID) == nil {
						t.Fatalf("building payload not committed: panic=%v", caught)
					}
				} else {
					storageErr, ok := StorageErrorFromPanic(caught)
					if !ok || !errors.Is(storageErr, ErrPayloadGenerationSealed) {
						t.Fatalf("non-building write was not a typed sealed refusal: panic=%T %v", caught, caught)
					}
					if errors.Is(storageErr, sql.ErrNoRows) {
						t.Fatal("sealed refusal retains the generic no-rows success sentinel")
					}
					if h.GetNode(node.ID) != nil {
						t.Fatal("refused payload escaped transaction rollback")
					}
				}
				if f.store.GetNode(base.ID) == nil || f.store.GetNode(node.ID) != nil {
					t.Fatal("managed payload changed the generation-zero corpus")
				}
			})
		}
	}
}

func privateManagedExec(ctx context.Context, s *Store, query string) error {
	if err := s.writeMu.LockContext(ctx); err != nil {
		return err
	}
	defer s.writeMu.Unlock()
	_, err := s.execActiveWriteLocked(ctx, query)
	return err
}

func TestPrivateManagedRetainedHandleCannotReuseOpenAdmission(t *testing.T) {
	for _, mode := range []string{"file", "bulk", "memory"} {
		t.Run(mode, func(t *testing.T) {
			f := privateNewManagedTxFixture(t, mode)
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			h, err := f.store.AtManagedGeneration(f.id)
			if err != nil {
				t.Fatal(err)
			}
			h = h.AtGeneration(f.id)
			before := &graph.Node{ID: "repo/before.go::Before", Kind: graph.KindFunction, Name: "Before", FilePath: "repo/before.go", RepoPrefix: "repo"}
			h.AddBatch([]*graph.Node{before}, nil)
			if h.GetNode(before.ID) == nil {
				t.Fatal("healthy retained-handle write did not persist")
			}
			// Mutating catalog state alone deliberately leaves the shared process
			// seal untouched. The next payload transaction must observe the fence,
			// even after this exact handle has successfully written Building data.
			if err := f.store.Catalog().SetViewGenerationState(ctx, f.id, ViewGenerationRetiring); err != nil {
				t.Fatal(err)
			}
			after := &graph.Node{ID: "repo/after.go::After", Kind: graph.KindFunction, Name: "After", FilePath: "repo/after.go", RepoPrefix: "repo"}
			caught := privateManagedPanic(func() { h.AddBatch([]*graph.Node{after}, nil) })
			storageErr, ok := StorageErrorFromPanic(caught)
			if !ok || !errors.Is(storageErr, ErrPayloadGenerationSealed) || errors.Is(storageErr, sql.ErrNoRows) {
				t.Fatalf("retained handle reused old admission: panic=%T %v", caught, caught)
			}
			if h.GetNode(before.ID) == nil || h.GetNode(after.ID) != nil {
				t.Fatal("refusal erased earlier data or persisted the late write")
			}
			row, found, err := f.store.Catalog().GetViewGeneration(ctx, f.id)
			if err != nil || !found || row.State != ViewGenerationRetiring {
				t.Fatalf("late payload write changed terminal metadata: row=%+v found=%v err=%v", row, found, err)
			}
		})
	}
}

func TestPrivateManagedExecAdmission(t *testing.T) {
	for _, mode := range []string{"file", "bulk", "memory"} {
		t.Run(mode, func(t *testing.T) {
			f := privateNewManagedTxFixture(t, mode)
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			if err := privateManagedExec(ctx, f.store, `CREATE TABLE managed_exec_probe (n INTEGER NOT NULL)`); err != nil {
				t.Fatal(err)
			}
			for i, state := range []string{"building", "ready", "superseded", "failed", "retiring", "missing"} {
				id := f.id
				if state == "missing" {
					id += 1000
				} else if err := f.store.Catalog().SetViewGenerationState(ctx, id, ViewGenerationState(state)); err != nil {
					t.Fatal(err)
				}
				h, err := f.store.AtManagedGeneration(id)
				if err != nil {
					t.Fatal(err)
				}
				err = privateManagedExec(ctx, h, fmt.Sprintf(`INSERT INTO managed_exec_probe (n) VALUES (%d)`, i))
				if state == "building" {
					if err != nil {
						t.Fatalf("building exec refused: %v", err)
					}
				} else if !errors.Is(err, ErrPayloadGenerationSealed) || errors.Is(err, sql.ErrNoRows) {
					t.Fatalf("%s exec admission: %v", state, err)
				}
			}
			var count int
			if err := f.store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM managed_exec_probe`).Scan(&count); err != nil {
				t.Fatal(err)
			}
			if count != 1 {
				t.Fatalf("refused execs persisted rows: count=%d", count)
			}
		})
	}
}

// Begin's returned positive handle carries the managed restriction.
// Explicit-generation control APIs must retain their own state guards,
// not accidentally run a Building-only payload predicate after sealing/fencing.
func TestPrivatePayloadControlReceiver(t *testing.T) {
	for _, mode := range []string{"file", "bulk", "memory"} {
		t.Run(mode, func(t *testing.T) {
			for _, positive := range []bool{false, true} {
				name := "base_receiver"
				if positive {
					name = "positive_receiver"
				}
				t.Run(name, func(t *testing.T) {
					for _, operation := range []string{"publish", "retire_building", "retire_ready"} {
						t.Run(operation, func(t *testing.T) {
							f := privateNewManagedTxFixture(t, mode)
							ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
							defer cancel()
							id, handle, err := f.store.BeginPayloadGeneration(ctx, PayloadGenerationRequest{
								OwnerKind: "dedicated_graph", GraphID: "graph", CheckoutID: "owner",
								GenerationKind: "commit", LayerID: "layer", TreeOID: "tree",
							})
							if err != nil || id != f.id || handle == nil {
								t.Fatalf("positive handle: id=%d want=%d nil=%v err=%v", id, f.id, handle == nil, err)
							}
							baseNode := &graph.Node{ID: "repo/base.go::Untouched", Kind: graph.KindFunction,
								Name: "Untouched", FilePath: "repo/base.go", RepoPrefix: "repo"}
							f.store.AddBatch([]*graph.Node{baseNode}, nil)
							if operation == "retire_building" {
								handle.AddBatch([]*graph.Node{{ID: "repo/a.go::Payload", Kind: graph.KindFunction,
									Name: "Payload", FilePath: "repo/a.go", RepoPrefix: "repo"}}, nil)
							}
							if operation == "retire_ready" {
								if err := f.store.PublishPayloadGeneration(ctx, id, 1); err != nil {
									t.Fatalf("valid empty publication setup: %v", err)
								}
							}
							receiver := f.store
							if positive {
								receiver = handle
							}
							if operation == "publish" {
								if err := receiver.PublishPayloadGeneration(ctx, id, 1); err != nil {
									t.Errorf("control publication refused: %v", err)
								}
								row, found, err := f.store.Catalog().GetViewGeneration(ctx, id)
								if err != nil || !found || row.State != ViewGenerationReady {
									t.Errorf("publication did not reach ready: state=%s found=%v err=%v", row.State, found, err)
								}
							} else {
								if err := receiver.RetirePayloadGeneration(ctx, id, nil); err != nil {
									t.Errorf("control retirement refused: %v", err)
								}
								row, found, err := f.store.Catalog().GetViewGeneration(ctx, id)
								if err != nil || found {
									t.Errorf("retirement retained metadata: state=%s found=%v err=%v", row.State, found, err)
								}
								if f.store.AtGeneration(id).GetNode("repo/a.go::Payload") != nil {
									t.Error("retirement retained payload")
								}
							}
							if f.store.GetNode(baseNode.ID) == nil {
								t.Error("explicit-generation control changed unrelated generation-zero payload")
							}
						})
					}
				})
			}
		})
	}
}

// This exercises public Join against the actual catalog-backed Store. In particular
// adopted=false must not permit a stale allocation hint to bypass current state.
// The shared private fixture only creates a real catalog generation and Store.
func TestPrivatePayloadBuildJoinFreshState(t *testing.T) {
	for _, hint := range []bool{false, true} {
		name := "fresh_hint"
		if hint {
			name = "adopted_hint"
		}
		t.Run(name, func(t *testing.T) {
			for _, state := range []ViewGenerationState{
				ViewGenerationBuilding, ViewGenerationReady, ViewGenerationSuperseded,
				ViewGenerationFailed, ViewGenerationRetiring, "missing",
			} {
				t.Run(string(state), func(t *testing.T) {
					f := privateNewManagedTxFixture(t, "file")
					ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
					defer cancel()
					id := f.id
					if state == "missing" {
						id += 1000
					} else if err := f.store.Catalog().SetViewGenerationState(ctx, id, state); err != nil {
						t.Fatal(err)
					}
					flight, leader, ready, err := f.store.JoinPayloadBuildFlight(ctx, id, hint)
					active := f.store.PayloadBuildFlightActive(id)
					// Clean up even the old implementation's wrongly admitted flight,
					// so a failed assertion does not retain a fixture owner indefinitely.
					if flight != nil {
						defer flight.Complete(nil)
					}
					switch state {
					case ViewGenerationBuilding:
						if err != nil || flight == nil || !leader || ready || !active {
							t.Fatalf("building: flight=%v leader=%v ready=%v active=%v err=%v", flight != nil, leader, ready, active, err)
						}
					case ViewGenerationReady:
						if err != nil || flight != nil || leader || !ready || active {
							t.Fatalf("ready: flight=%v leader=%v ready=%v active=%v err=%v", flight != nil, leader, ready, active, err)
						}
					default:
						if !errors.Is(err, ErrCatalogInvalidValue) || flight != nil || leader || ready || active {
							t.Fatalf("state %s admitted or leaked: flight=%v leader=%v ready=%v active=%v err=%v", state, flight != nil, leader, ready, active, err)
						}
					}
				})
			}
		})
	}
}

// Store-owned physical flights must be protected even when the external reader
// callback is nil or incomplete. The late callback is a deterministic scheduling
// point after the central fast check, not a fabricated private state mutation.
func TestPrivateRetirementAlwaysHonorsStoreFlight(t *testing.T) {
	for _, late := range []bool{false, true} {
		name := "already_owned_nil_callback"
		if late {
			name = "late_owner_incomplete_callback"
		}
		t.Run(name, func(t *testing.T) {
			f := privateNewManagedTxFixture(t, "file")
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			id, handle, err := f.store.BeginPayloadGeneration(ctx, PayloadGenerationRequest{
				OwnerKind: "dedicated_graph", GraphID: "graph", CheckoutID: "owner",
				GenerationKind: "commit", LayerID: "layer", TreeOID: "tree",
			})
			if err != nil || id != f.id || handle == nil {
				t.Fatalf("fixture handle: id=%d want=%d nil=%v err=%v", id, f.id, handle == nil, err)
			}
			unread := &graph.Node{ID: "repo/a.go::UnreadFlight", Kind: graph.KindFunction,
				Name: "UnreadFlight", FilePath: "repo/a.go", RepoPrefix: "repo"}
			handle.AddBatch([]*graph.Node{unread}, nil)
			var flight *PayloadBuildFlight
			acquire := func() {
				t.Helper()
				var leader, ready bool
				flight, leader, ready, err = f.store.JoinPayloadBuildFlight(ctx, id, false)
				if err != nil || flight == nil || !leader || ready {
					t.Fatalf("physical leader: flight=%v leader=%v ready=%v err=%v", flight != nil, leader, ready, err)
				}
			}
			defer func() {
				if flight != nil {
					flight.Complete(nil)
				}
			}()
			var inUse func(int64) bool
			if late {
				inUse = func(generationID int64) bool {
					if generationID != id {
						t.Fatalf("wrong callback generation %d", generationID)
					}
					if flight == nil {
						acquire()
					}
					return false // Deliberately omits the Store's intrinsic flight.
				}
			} else {
				acquire()
			}
			retireErr := f.store.RetirePayloadGeneration(ctx, id, inUse)
			if !errors.Is(retireErr, ErrPayloadGenerationInUse) {
				t.Errorf("retirement ignored owned flight: %v", retireErr)
			}
			row, found, readErr := f.store.Catalog().GetViewGeneration(ctx, id)
			wantState := ViewGenerationBuilding
			if late {
				wantState = ViewGenerationRetiring
			}
			if readErr != nil || !found || row.State != wantState {
				t.Errorf("owned generation lost or fence reopened: state=%s want=%s found=%v err=%v", row.State, wantState, found, readErr)
			}
			if f.store.AtGeneration(id).GetNode(unread.ID) == nil {
				t.Error("retirement deleted unread payload under an owned flight")
			}
			if late {
				lateNode := &graph.Node{ID: "repo/a.go::AfterFence", Kind: graph.KindFunction,
					Name: "AfterFence", FilePath: "repo/a.go", RepoPrefix: "repo"}
				panicValue := func() (value any) {
					defer func() { value = recover() }()
					handle.AddBatch([]*graph.Node{lateNode}, nil)
					return nil
				}()
				storageErr, classified := StorageErrorFromPanic(panicValue)
				if !classified || !errors.Is(storageErr, ErrPayloadGenerationSealed) {
					t.Errorf("post-fence payload loss was not a typed refusal: panic=%T %v classified=%v err=%v", panicValue, panicValue, classified, storageErr)
				}
				if f.store.AtGeneration(id).GetNode(lateNode.ID) != nil {
					t.Error("post-fence write recreated payload")
				}
			}
			flight.Complete(nil)
			if f.store.PayloadBuildFlightActive(id) {
				t.Fatal("completed owner leaked its physical flight")
			}
			if found && errors.Is(retireErr, ErrPayloadGenerationInUse) {
				if err := f.store.RetirePayloadGeneration(ctx, id, nil); err != nil {
					t.Fatalf("retirement did not resume after owner release: %v", err)
				}
				if f.store.AtGeneration(id).GetNode(unread.ID) != nil {
					t.Error("resumed retirement retained payload")
				}
			}
		})
	}
}

// Reclamation deletes both the catalog row and the process seal registry entry.
// A delayed same-generation derivation must not acquire a new owner's identity
// or treat a newly created process seal as permission to recreate old payload.
func TestPrivateManagedDeletedGenerationCannotAliasReplacement(t *testing.T) {
	for _, mode := range []string{"file", "bulk", "memory"} {
		t.Run(mode, func(t *testing.T) {
			f := privateNewManagedTxFixture(t, mode)
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			old, err := f.store.AtManagedGeneration(f.id)
			if err != nil {
				t.Fatal(err)
			}
			base := &graph.Node{ID: "repo/base.go::KeepBase", Kind: graph.KindFunction, Name: "KeepBase", FilePath: "repo/base.go", RepoPrefix: "repo"}
			f.store.AddBatch([]*graph.Node{base}, nil)
			before := &graph.Node{ID: "repo/old.go::BeforeDelete", Kind: graph.KindFunction, Name: "BeforeDelete", FilePath: "repo/old.go", RepoPrefix: "repo"}
			old.AddBatch([]*graph.Node{before}, nil)
			if old.GetNode(before.ID) == nil {
				t.Fatal("healthy old payload did not persist")
			}
			if err := f.store.RetirePayloadGeneration(ctx, f.id, nil); err != nil {
				t.Fatalf("retire unreferenced old generation: %v", err)
			}
			if _, found, err := f.store.Catalog().GetViewGeneration(ctx, f.id); err != nil || found {
				t.Fatalf("retired catalog row remains: found=%v err=%v", found, err)
			}
			if old.GetNode(before.ID) != nil {
				t.Fatal("retired payload remains")
			}
			newID, replacement, err := f.store.BeginPayloadGeneration(ctx, PayloadGenerationRequest{
				OwnerKind: "dedicated_graph", GraphID: "graph", CheckoutID: "owner",
				GenerationKind: "commit", LayerID: "replacement-layer", TreeOID: "replacement-tree",
			})
			if err != nil || replacement == nil || newID <= f.id {
				t.Fatalf("deleted maximum generation identity reused: old=%d new=%d handle=%v err=%v", f.id, newID, replacement != nil, err)
			}
			keep := &graph.Node{ID: "repo/new.go::KeepReplacement", Kind: graph.KindFunction, Name: "KeepReplacement", FilePath: "repo/new.go", RepoPrefix: "repo"}
			replacement.AddBatch([]*graph.Node{keep}, nil)
			for _, delayed := range []*Store{old, old.AtGeneration(f.id)} {
				late := &graph.Node{ID: "repo/old.go::AfterDelete", Kind: graph.KindFunction, Name: "AfterDelete", FilePath: "repo/old.go", RepoPrefix: "repo"}
				caught := privateManagedPanic(func() { delayed.AddBatch([]*graph.Node{late}, nil) })
				storageErr, ok := StorageErrorFromPanic(caught)
				if !ok || !errors.Is(storageErr, ErrPayloadGenerationSealed) || errors.Is(storageErr, sql.ErrNoRows) {
					t.Fatalf("deleted handle was not a typed refusal: panic=%T %v", caught, caught)
				}
				if delayed.GetNode(late.ID) != nil || replacement.GetNode(late.ID) != nil || f.store.GetNode(late.ID) != nil {
					t.Fatal("delayed write recreated old payload or escaped its generation")
				}
			}
			if replacement.GetNode(keep.ID) == nil || f.store.GetNode(base.ID) == nil {
				t.Fatal("delayed refusal damaged healthy replacement or generation-zero data")
			}
		})
	}
}
