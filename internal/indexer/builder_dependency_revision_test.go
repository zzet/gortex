package indexer

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/indexer/source"
	"go.uber.org/zap"
)

func TestDependencyRevisionGenerationKeysPreserveLegacyAndDifferentiateOutput(t *testing.T) {
	identity := GenerationIdentity{OwnerKind: "owner", GraphID: "graph", LayerID: "layer", CheckoutID: "checkout", GenerationKind: "kind", BaseGenerationID: 7,
		LowerViewFingerprint: "lower", TreeOID: "tree", ProvenanceCommitOID: "commit", ConfigHash: "config", ExtractorVersions: "extractors", ResolverVersion: "resolver", CreatedAt: 11}
	const legacy = "owner\x00graph\x00layer\x00checkout\x00kind\x007\x00lower\x00tree\x00commit\x00config\x00extractors\x00resolver\x00"
	if got := generationIdentityKey(identity); got != legacy {
		t.Fatalf("legacy fingerprint changed: %q", got)
	}
	identity.DependencyRevision = "cohort-v1:a"
	first := generationIdentityKey(identity)
	if first == legacy {
		t.Fatal("dependency revision absent from key")
	}
	identity.DependencyRevision = "cohort-v1:b"
	second := generationIdentityKey(identity)
	if first == second {
		t.Fatal("different revisions share key")
	}
	row := store_sqlite.ViewGeneration{OwnerKind: identity.OwnerKind, GraphID: identity.GraphID, LayerID: identity.LayerID, CheckoutID: identity.CheckoutID, GenerationKind: identity.GenerationKind,
		BaseGenerationID: identity.BaseGenerationID, LowerViewFingerprint: identity.LowerViewFingerprint, TreeOID: identity.TreeOID, ProvenanceCommitOID: identity.ProvenanceCommitOID,
		ConfigHash: identity.ConfigHash, ExtractorVersions: identity.ExtractorVersions, ResolverVersion: identity.ResolverVersion, DependencyRevision: identity.DependencyRevision}
	if got := generationRowKey(row); got != second {
		t.Fatalf("row/request key drift: %q != %q", got, second)
	}
	row.DependencyRevision = ""
	if got := generationRowKey(row); got != legacy {
		t.Fatalf("legacy row key changed: %q", got)
	}
	identity.CreatedAt++
	if generationIdentityKey(identity) != second {
		t.Fatal("nonsemantic creation time entered identity")
	}
}

func TestDependencyRevisionOrdinaryBuilderCarriesMetadata(t *testing.T) {
	builder, fixture, _ := privateDedicatedBuilderFixture(t)
	ctx := context.Background()
	target, err := source.NewGitTreeSource(ctx, fixture.RootPath, fixture.Identity.TreeOID)
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	identity := fixture.Identity
	identity.DependencyRevision = "cohort-v1:ordinary"
	id, report, err := builder.Build(ctx, BuildRequest{Identity: identity, Base: graph.New(), Target: target,
		Changes:  []LayerPathChange{{Path: "base.go", Kind: LayerPathAdded}, {Path: "go.mod", Kind: LayerPathAdded}},
		RootPath: fixture.RootPath, RepoPrefix: fixture.RepoPrefix, WorkspaceID: fixture.WorkspaceID, ProjectID: fixture.ProjectID})
	if err != nil || id <= 0 || report.NodeCount == 0 {
		t.Fatalf("actual build id=%d report=%+v err=%v", id, report, err)
	}
	row, found, err := builder.Store.Catalog().GetViewGeneration(ctx, id)
	if err != nil || !found || row.DependencyRevision != identity.DependencyRevision {
		t.Fatalf("build metadata=%+v found=%v err=%v", row, found, err)
	}
	if builder.Store.AtGeneration(id).GetNode(fixture.RepoPrefix+"/base.go::Committed") == nil {
		t.Fatal("actual parser payload missing")
	}
	if len(builder.Store.AllNodes()) != 0 {
		t.Fatal("positive builder wrote generation zero")
	}
}

// This uses a REAL local source change as well as D1->D2. It tests initial and
// delta output propagation/parent compatibility, not a dependency-only stage.
//
// D2: a dependency-revision change roots a NEW chain and never extends one. The
// three arms are the whole contract: a delta over a MATCHING-revision parent
// still builds and still coalesces on ready replay; a CHANGED revision is
// published as a full root; and the composition D2 forbids (an output frozen at
// one revision over a parent frozen at another) is refused by the builder with
// the typed sentinel, writing nothing.
func TestDependencyRevisionClaimedFullAndDeltaOutput(t *testing.T) {
	builder, fixture, git := privateDedicatedBuilderFixture(t)
	ctx := context.Background()
	catalog := builder.Store.Catalog()
	commit := func(body, message string) string {
		t.Helper()
		if err := os.WriteFile(filepath.Join(fixture.RootPath, "base.go"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		git("add", "base.go")
		git("-c", "user.name=Private Test", "-c", "user.email=private@example.invalid", "-c", "commit.gpgsign=false", "commit", "-qm", message)
		return git("rev-parse", "HEAD^{tree}")
	}
	authority, err := catalog.AcquireDedicatedBaseAuthority(ctx, store_sqlite.AcquireDedicatedBaseAuthorityRequest{
		GraphID: fixture.Identity.GraphID, Owner: store_sqlite.DedicatedBaseOwner{CheckoutID: fixture.Identity.CheckoutID, Incarnation: "private-incarnation"}, Token: "dependency-authority"})
	if err != nil {
		t.Fatal(err)
	}
	identity := store_sqlite.DedicatedBaseIdentity{TreeOID: fixture.Identity.TreeOID, ConfigHash: fixture.Identity.ConfigHash,
		ExtractorVersions: fixture.Identity.ExtractorVersions, ResolverVersion: fixture.Identity.ResolverVersion, DependencyRevision: "cohort-v1:a"}
	desire, err := catalog.RecordDedicatedBaseDesire(ctx, store_sqlite.RecordDedicatedBaseDesireRequest{Authority: authority, Identity: identity})
	if err != nil {
		t.Fatal(err)
	}
	first, err := catalog.ClaimDedicatedBaseBuild(ctx, store_sqlite.ClaimDedicatedBaseBuildRequest{Desire: desire, AttemptToken: "dependency-a"})
	if err != nil {
		t.Fatal(err)
	}
	id, report, err := builder.BuildClaimedDedicatedBase(ctx, ClaimedDedicatedBaseRequest{Claim: first, RootPath: fixture.RootPath, WorkspaceID: fixture.WorkspaceID, ProjectID: fixture.ProjectID})
	if err != nil || id != first.GenerationID || report.NodeCount == 0 {
		t.Fatalf("full id=%d report=%+v err=%v", id, report, err)
	}
	if _, err := catalog.AdoptDedicatedBaseGeneration(ctx, store_sqlite.AdoptDedicatedBaseGenerationRequest{Claim: first}); err != nil {
		t.Fatal(err)
	}
	materializer := graphview.Materializer{Store: builder.Store, Catalog: catalog, Leases: graphview.NewLeaseManager(), Logger: zap.NewNop()}
	lower, err := materializer.MaterializeRefView(ctx, fixture.Identity.GraphID, id)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(lower.Close)
	if lower.Reader.GetNode(fixture.RepoPrefix+"/base.go::Committed") == nil {
		t.Fatal("full lower payload missing")
	}

	// Arm 1 — a MATCHING-revision parent still supports a real sparse delta.
	identity.TreeOID = commit("package dedicated\n\nfunc Committed() string { return \"changed\" }\nfunc AddedByDelta() {}\n", "dependency revision delta")
	desire, err = catalog.RecordDedicatedBaseDesire(ctx, store_sqlite.RecordDedicatedBaseDesireRequest{Authority: authority, ExpectedDesiredEpoch: desire.Epoch, Identity: identity})
	if err != nil {
		t.Fatal(err)
	}
	second, err := catalog.ClaimDedicatedBaseBuild(ctx, store_sqlite.ClaimDedicatedBaseBuildRequest{Desire: desire, AttemptToken: "dependency-b", ExpectedActiveGenerationID: id, BaseGenerationID: id,
		LayerID: "dependency-delta", LowerViewFingerprint: "dependency-lower-a"})
	if err != nil {
		t.Fatal(err)
	}
	deltaID, deltaReport, err := builder.BuildClaimedDedicatedDelta(ctx, ClaimedDedicatedDeltaRequest{Claim: second,
		Base: commitLayerBase{Reader: lower.Reader, corpus: builder.Store.AtGeneration(id)}, BaseTreeOID: fixture.Identity.TreeOID,
		RepoDir: fixture.RootPath, RootPath: fixture.RootPath, WorkspaceID: fixture.WorkspaceID, ProjectID: fixture.ProjectID})
	if err != nil || deltaID != second.GenerationID || deltaReport.NodeCount == 0 {
		t.Fatalf("delta id=%d report=%+v err=%v", deltaID, deltaReport, err)
	}
	row, found, err := catalog.GetViewGeneration(ctx, deltaID)
	if err != nil || !found || row.DependencyRevision != "cohort-v1:a" || row.BaseGenerationID != id {
		t.Fatalf("delta metadata=%+v found=%v err=%v", row, found, err)
	}
	if builder.Store.AtGeneration(deltaID).GetNode(fixture.RepoPrefix+"/base.go::AddedByDelta") == nil {
		t.Fatal("delta parser payload missing")
	}
	if _, err := catalog.AdoptDedicatedBaseGeneration(ctx, store_sqlite.AdoptDedicatedBaseGenerationRequest{Claim: second}); err != nil {
		t.Fatal(err)
	}
	ready, err := catalog.ClaimDedicatedBaseBuild(ctx, store_sqlite.ClaimDedicatedBaseBuildRequest{Desire: desire, AttemptToken: "ignored-ready", ExpectedActiveGenerationID: deltaID})
	if err != nil {
		t.Fatal(err)
	}
	reused, readyReport, err := builder.BuildClaimedDedicatedDelta(ctx, ClaimedDedicatedDeltaRequest{Claim: ready, BaseTreeOID: fixture.Identity.TreeOID})
	if err != nil || reused != deltaID || !readyReport.Coalesced {
		t.Fatalf("ready delta required source: id=%d report=%+v err=%v", reused, readyReport, err)
	}

	// Arm 2 — a CHANGED revision is published as a full root, not as a delta.
	identity.TreeOID = commit("package dedicated\n\nfunc Committed() string { return \"rerooted\" }\nfunc AddedByRoot() {}\n", "dependency revision reroot")
	identity.DependencyRevision = "cohort-v1:b"
	desire, err = catalog.RecordDedicatedBaseDesire(ctx, store_sqlite.RecordDedicatedBaseDesireRequest{Authority: authority, ExpectedDesiredEpoch: desire.Epoch, Identity: identity})
	if err != nil {
		t.Fatal(err)
	}
	third, err := catalog.ClaimDedicatedBaseBuild(ctx, store_sqlite.ClaimDedicatedBaseBuildRequest{Desire: desire, AttemptToken: "dependency-c", ExpectedActiveGenerationID: deltaID})
	if err != nil || third.BaseGenerationID != 0 {
		t.Fatalf("changed revision did not claim a full root: claim=%+v err=%v", third, err)
	}
	rootID, rootReport, err := builder.BuildClaimedDedicatedBase(ctx, ClaimedDedicatedBaseRequest{Claim: third, RootPath: fixture.RootPath, WorkspaceID: fixture.WorkspaceID, ProjectID: fixture.ProjectID})
	if err != nil || rootID != third.GenerationID || rootReport.NodeCount == 0 {
		t.Fatalf("reroot id=%d report=%+v err=%v", rootID, rootReport, err)
	}
	rootRow, found, err := catalog.GetViewGeneration(ctx, rootID)
	if err != nil || !found || rootRow.BaseGenerationID != 0 || rootRow.LayerID != "" ||
		rootRow.LowerViewFingerprint != "" || rootRow.DependencyRevision != "cohort-v1:b" {
		t.Fatalf("reroot metadata=%+v found=%v err=%v", rootRow, found, err)
	}
	if builder.Store.AtGeneration(rootID).GetNode(fixture.RepoPrefix+"/base.go::AddedByRoot") == nil {
		t.Fatal("rerooted parser payload missing")
	}
	if _, err := catalog.AdoptDedicatedBaseGeneration(ctx, store_sqlite.AdoptDedicatedBaseGenerationRequest{Claim: third}); err != nil {
		t.Fatal(err)
	}

	// Arm 3 — the composition D2 forbids. The catalog hands the reservation out
	// (its ancestry validation revision-checks only the output candidate), so
	// this guard is reachable production code and the builder must refuse it.
	identity.TreeOID = commit("package dedicated\n\nfunc Committed() string { return \"forbidden\" }\nfunc AddedByForbiddenDelta() {}\n", "dependency revision forbidden composition")
	desire, err = catalog.RecordDedicatedBaseDesire(ctx, store_sqlite.RecordDedicatedBaseDesireRequest{Authority: authority, ExpectedDesiredEpoch: desire.Epoch, Identity: identity})
	if err != nil {
		t.Fatal(err)
	}
	mismatched, err := catalog.ClaimDedicatedBaseBuild(ctx, store_sqlite.ClaimDedicatedBaseBuildRequest{Desire: desire, AttemptToken: "dependency-d", ExpectedActiveGenerationID: rootID,
		BaseGenerationID: id, LayerID: "dependency-mismatch", LowerViewFingerprint: "dependency-lower-mismatch"})
	if err != nil || mismatched.BaseGenerationID != id {
		t.Fatalf("catalog refused the mismatched reservation, so the guard would be dead code: claim=%+v err=%v", mismatched, err)
	}
	before := len(builder.Store.AllNodes())
	refusedID, refusedReport, err := builder.BuildClaimedDedicatedDelta(ctx, ClaimedDedicatedDeltaRequest{Claim: mismatched,
		Base: commitLayerBase{Reader: lower.Reader, corpus: builder.Store.AtGeneration(id)}, BaseTreeOID: fixture.Identity.TreeOID,
		RepoDir: fixture.RootPath, RootPath: fixture.RootPath, WorkspaceID: fixture.WorkspaceID, ProjectID: fixture.ProjectID})
	if !errors.Is(err, errDedicatedDeltaParentRevision) || !errors.Is(err, store_sqlite.ErrDedicatedBaseCandidate) {
		t.Fatalf("mismatched-revision parent extended the chain: id=%d report=%+v err=%v", refusedID, refusedReport, err)
	}
	if refusedID != 0 || refusedReport.Coalesced || refusedReport.NodeCount != 0 {
		t.Fatalf("refusal still produced output: id=%d report=%+v", refusedID, refusedReport)
	}
	refusedRow, found, err := catalog.GetViewGeneration(ctx, mismatched.GenerationID)
	if err != nil || !found || refusedRow.State != store_sqlite.ViewGenerationBuilding {
		t.Fatalf("refused reservation was published: row=%+v found=%v err=%v", refusedRow, found, err)
	}
	if len(builder.Store.AtGeneration(mismatched.GenerationID).AllNodes()) != 0 {
		t.Fatal("refused delta wrote payload")
	}
	if len(builder.Store.AllNodes()) != before || before != 0 {
		t.Fatalf("claimed builders wrote generation zero: before=%d after=%d", before, len(builder.Store.AllNodes()))
	}
}
