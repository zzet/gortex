package indexer

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// A dependency-revision change is a complete-frozen-input change, so it must
// root a NEW full chain rather than extend the active one. The unchanged arm is
// the control that proves this does not simply disable sparse advancement.
func TestDedicatedBaseAdvanceRevisionChangeRootsNewChain(t *testing.T) {
	for _, changed := range []bool{false, true} {
		name := "unchanged-revision-still-deltas"
		if changed {
			name = "changed-revision-roots"
		}
		t.Run(name, func(t *testing.T) {
			f := newDedicatedAdvanceFixture(t)
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			f.identity.DependencyRevision = "cohort-v1:a"
			initial := f.ensure(t, ctx)
			if initial.Claim.BaseGenerationID != 0 {
				t.Fatalf("initial publication is not a full root: %+v", initial.Claim)
			}
			initialRow, found, err := f.builder.Store.Catalog().GetViewGeneration(ctx, initial.Claim.GenerationID)
			if err != nil || !found || initialRow.DependencyRevision != "cohort-v1:a" {
				t.Fatalf("fixture did not persist the observed revision: %+v found=%v err=%v", initialRow, found, err)
			}
			oldView := f.view(t, ctx, initial.Claim.GenerationID)
			f.commitFile(t, "package dedicated\nfunc AdvancementMarker() int { return 1 }\n")
			if changed {
				f.identity.DependencyRevision = "cohort-v1:b"
			}
			next := f.ensure(t, ctx)
			if next.Claim.GenerationID == initial.Claim.GenerationID || next.Report.Coalesced {
				t.Fatalf("advancement did not allocate fresh output: %+v", next)
			}
			nextRow, found, err := f.builder.Store.Catalog().GetViewGeneration(ctx, next.Claim.GenerationID)
			if err != nil || !found {
				t.Fatalf("advanced row: found=%v err=%v", found, err)
			}
			if changed {
				if next.Claim.BaseGenerationID != 0 || next.Claim.LayerID != "" || next.Claim.LowerViewFingerprint != "" ||
					nextRow.BaseGenerationID != 0 || nextRow.LayerID != "" || nextRow.LowerViewFingerprint != "" {
					t.Fatalf("revision change extended the chain instead of rooting: claim=%+v row=%+v", next.Claim, nextRow)
				}
				if nextRow.DependencyRevision != "cohort-v1:b" {
					t.Fatalf("new root did not carry the observed revision: %q", nextRow.DependencyRevision)
				}
			} else if next.Claim.BaseGenerationID != initial.Claim.GenerationID || nextRow.BaseGenerationID != initial.Claim.GenerationID {
				t.Fatalf("unchanged revision lost sparse advancement: claim=%+v row=%+v", next.Claim, nextRow)
			}
			// Either way the old committed route is immutable and still readable.
			path := f.request.RepoPrefix + "/advance.go"
			if len(oldView.Reader.GetFileNodes(path)) != 0 {
				t.Fatal("old immutable route changed")
			}
			current := f.view(t, ctx, next.Claim.GenerationID)
			if len(current.Reader.GetFileNodes(path)) == 0 {
				t.Fatal("new committed source missing")
			}
			// Gate 2: replaying the very same observation is a no-op. A rooted
			// revision change must not turn every later poll into a rebuild.
			observation, err := f.observe(t, ctx)
			if err != nil {
				t.Fatal(err)
			}
			check, err := installDedicatedWriteAudit(ctx, f.request.StorePath)
			if err != nil {
				t.Fatal(err)
			}
			replay, err := f.publisher.ensureCurrent(ctx, f.leases, func(context.Context) (dedicatedBaseObservation, error) { return observation, nil })
			if err != nil || replay.Claim.GenerationID != next.Claim.GenerationID || !replay.Report.Coalesced || !replay.Adoption.AlreadyAdopted {
				t.Fatalf("no-op replay after a revision decision: %+v err=%v", replay, err)
			}
			if err := check(); err != nil {
				t.Fatalf("no-op replay wrote catalog rows: %v", err)
			}
		})
	}
}

// The planner is metadata-only. Neither of its full-root verdicts — a changed
// target revision, or a stored chain that is not revision-homogeneous — may
// allocate a generation or touch the publication.
func TestDedicatedBaseAdvanceRevisionPlanningWritesNothing(t *testing.T) {
	f := newDedicatedAdvanceFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	f.identity.DependencyRevision = "cohort-v1:a"
	initial := f.ensure(t, ctx)
	catalog := f.builder.Store.Catalog()
	active, found, err := catalog.GetViewGeneration(ctx, initial.Claim.GenerationID)
	if err != nil || !found {
		t.Fatalf("active row: found=%v err=%v", found, err)
	}
	// A synthetic same-policy child whose stored revision differs from its own
	// parent's is exactly what the catalog's ancestry check tolerates. Planning
	// on top of it is not valid advancement ancestry.
	mixed, err := catalog.CreateViewGeneration(ctx, store_sqlite.ViewGeneration{
		OwnerKind: "dedicated_graph", GraphID: f.publisher.authority.GraphID,
		CheckoutID: f.publisher.authority.Owner.CheckoutID, GenerationKind: "dedicated",
		TreeOID: active.TreeOID, ConfigHash: active.ConfigHash,
		ExtractorVersions: active.ExtractorVersions, ResolverVersion: active.ResolverVersion,
		DependencyRevision: "cohort-v1:b", BaseGenerationID: active.GenerationID,
		LayerID:              "dedicated-delta:mixed",
		LowerViewFingerprint: "dedicated:mixed",
		CreatedAt:            2, State: store_sqlite.ViewGenerationBuilding,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := catalog.PublishViewGeneration(ctx, mixed, 3); err != nil {
		t.Fatal(err)
	}
	mixedRow, found, err := catalog.GetViewGeneration(ctx, mixed)
	if err != nil || !found {
		t.Fatalf("mixed row: found=%v err=%v", found, err)
	}
	check, err := installDedicatedWriteAudit(ctx, f.request.StorePath)
	if err != nil {
		t.Fatal(err)
	}
	sameRevision := f.identity
	sameRevision.TreeOID = "0000000000000000000000000000000000000000"
	changedRevision := sameRevision
	changedRevision.DependencyRevision = "cohort-v1:b"

	selected, err := dedicatedBaseParentForAdvance(ctx, catalog, f.publisher.authority, active, sameRevision)
	if err != nil || selected != active.GenerationID {
		t.Fatalf("same revision did not propose the active parent: selected=%d err=%v", selected, err)
	}
	selected, err = dedicatedBaseParentForAdvance(ctx, catalog, f.publisher.authority, active, changedRevision)
	if err != nil || selected != 0 {
		t.Fatalf("changed revision proposed a parent: selected=%d err=%v", selected, err)
	}
	// A non-homogeneous stored chain is not reusable ancestry, but it must NOT
	// be a hard failure: the verdict is a full root, so publication continues.
	selected, err = dedicatedBaseParentForAdvance(ctx, catalog, f.publisher.authority, mixedRow, changedRevision)
	if err != nil || selected != 0 {
		t.Fatalf("non-homogeneous ancestry did not degrade to a full root: selected=%d err=%v", selected, err)
	}
	// Genuinely invalid ancestry (a policy break below the head) stays an error;
	// the revision degrade must not swallow the ancestry guard as a whole.
	brokenPolicy := active
	brokenPolicy.ConfigHash = "private-config-v2"
	brokenPolicy.GenerationID, brokenPolicy.BaseGenerationID = mixedRow.GenerationID, mixedRow.BaseGenerationID
	brokenPolicy.LayerID, brokenPolicy.LowerViewFingerprint = mixedRow.LayerID, mixedRow.LowerViewFingerprint
	brokenTarget := changedRevision
	brokenTarget.ConfigHash = brokenPolicy.ConfigHash
	if _, err := dedicatedBaseParentForAdvance(ctx, catalog, f.publisher.authority, brokenPolicy, brokenTarget); !errors.Is(err, store_sqlite.ErrDedicatedBaseCandidate) {
		t.Fatalf("policy-broken ancestry admitted: %v", err)
	}
	if err := check(); err != nil {
		t.Fatalf("planning wrote catalog rows: %v", err)
	}
}

// A dependency-revision change must never wedge publication. The catalog can
// legitimately hold a chain whose lower carries an older revision (its ancestry
// validation revision-checks only the output candidate), and the ACTIVE pointer
// can be at the head of exactly such a chain. ensureCurrent must then publish a
// new full root, not return ErrDedicatedBaseCandidate forever.
func TestDedicatedBaseAdvanceNonHomogeneousChainStillPublishesAsRoot(t *testing.T) {
	f := newDedicatedAdvanceFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	catalog := f.builder.Store.Catalog()
	f.identity.DependencyRevision = "cohort-v1:a"
	initial := f.ensure(t, ctx)
	rootID := initial.Claim.GenerationID

	// Build the mixed chain through the catalog's own public API, exactly as a
	// mixed-binary window or a non-indexer claimer would: an output frozen at
	// cohort-v1:b whose parent is the cohort-v1:a root. The catalog accepts the
	// reservation (exactTree=false on the proposed parent) and the adoption
	// (exactTree=true revision-checks depth 0 only), so this shape is reachable
	// without touching store internals.
	f.commitFile(t, "package dedicated\nfunc MixedChainMarker() int { return 1 }\n")
	mixedTree := f.git(t, "rev-parse", "HEAD^{tree}")
	publication, found, err := catalog.DedicatedBasePublication(ctx, f.publisher.authority.GraphID)
	if err != nil || !found {
		t.Fatalf("publication: found=%v err=%v", found, err)
	}
	mixedIdentity := f.identity
	mixedIdentity.TreeOID, mixedIdentity.DependencyRevision = mixedTree, "cohort-v1:b"
	desire, err := catalog.RecordDedicatedBaseDesire(ctx, store_sqlite.RecordDedicatedBaseDesireRequest{
		Authority: f.publisher.authority, ExpectedDesiredEpoch: publication.Desire.Epoch, Identity: mixedIdentity})
	if err != nil {
		t.Fatal(err)
	}
	mixedClaim, err := catalog.ClaimDedicatedBaseBuild(ctx, store_sqlite.ClaimDedicatedBaseBuildRequest{
		Desire: desire, ExpectedActiveGenerationID: rootID, AttemptToken: "mixed-chain-attempt",
		BaseGenerationID: rootID, LayerID: fmt.Sprintf("dedicated-delta:%d", rootID),
		LowerViewFingerprint: fmt.Sprintf("dedicated:%s:%d", f.publisher.authority.GraphID, rootID), CreatedAt: 2,
	})
	if err != nil || mixedClaim.BaseGenerationID != rootID {
		t.Fatalf("catalog refused the mixed reservation, so the wedge is unreachable: claim=%+v err=%v", mixedClaim, err)
	}
	if err := catalog.PublishViewGeneration(ctx, mixedClaim.GenerationID, 3); err != nil {
		t.Fatal(err)
	}
	if _, err := catalog.AdoptDedicatedBaseGeneration(ctx, store_sqlite.AdoptDedicatedBaseGenerationRequest{Claim: mixedClaim}); err != nil {
		t.Fatal(err)
	}
	graph, found, err := catalog.GetDedicatedGraph(ctx, f.publisher.authority.GraphID)
	if err != nil || !found || graph.ActiveGenerationID != mixedClaim.GenerationID {
		t.Fatalf("active pointer is not the mixed head: graph=%+v found=%v err=%v", graph, found, err)
	}

	// The next observation sees the same revision as the mixed head, so the
	// planner walks into the older-revision lower. That must re-root, not error.
	f.commitFile(t, "package dedicated\nfunc MixedChainMarker() int { return 2 }\n")
	f.identity.DependencyRevision = "cohort-v1:b"
	next := f.ensure(t, ctx)
	if next.Claim.BaseGenerationID != 0 || next.Claim.LayerID != "" || next.Claim.LowerViewFingerprint != "" {
		t.Fatalf("non-homogeneous chain was extended instead of re-rooted: %+v", next.Claim)
	}
	nextRow, found, err := catalog.GetViewGeneration(ctx, next.Claim.GenerationID)
	if err != nil || !found || nextRow.BaseGenerationID != 0 || nextRow.LayerID != "" ||
		nextRow.LowerViewFingerprint != "" || nextRow.DependencyRevision != "cohort-v1:b" {
		t.Fatalf("re-rooted row: %+v found=%v err=%v", nextRow, found, err)
	}
	if len(f.view(t, ctx, next.Claim.GenerationID).Reader.GetFileNodes(f.request.RepoPrefix+"/advance.go")) == 0 {
		t.Fatal("re-rooted publication is missing committed source")
	}
}
