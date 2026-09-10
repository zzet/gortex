package indexer

import (
	"context"
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
func TestDependencyRevisionClaimedFullAndDeltaOutput(t *testing.T) {
	builder, fixture, git := privateDedicatedBuilderFixture(t)
	ctx := context.Background()
	catalog := builder.Store.Catalog()
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
	if err := os.WriteFile(filepath.Join(fixture.RootPath, "base.go"), []byte("package dedicated\n\nfunc Committed() string { return \"changed\" }\nfunc AddedByDelta() {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git("add", "base.go")
	git("-c", "user.name=Private Test", "-c", "user.email=private@example.invalid", "-c", "commit.gpgsign=false", "commit", "-qm", "dependency revision delta")
	identity.TreeOID, identity.DependencyRevision = git("rev-parse", "HEAD^{tree}"), "cohort-v1:b"
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
	if err != nil || !found || row.DependencyRevision != "cohort-v1:b" || row.BaseGenerationID != id {
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
	if len(builder.Store.AllNodes()) != 0 {
		t.Fatal("claimed builders wrote generation zero")
	}
}
