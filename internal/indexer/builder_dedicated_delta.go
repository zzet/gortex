package indexer

import (
	"context"
	"fmt"
	"time"

	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/indexer/source"
)

// ClaimedDedicatedDeltaRequest advances an immutable dedicated base using its
// catalog reservation. Base must be the FULL composed lower view at the claim's
// BaseGenerationID, not just that generation's sparse payload. The caller owns
// and holds its read lease through this call; this builder never closes Base.
// Ready reuse does not read Base and may omit it; parent metadata is still checked.
// Config and workspace/project stamping must match the claim's effective policy
// identity. Policy changes require an explicit full reseed, not this delta path.
type ClaimedDedicatedDeltaRequest struct {
	Claim       store_sqlite.DedicatedBaseBuildClaim
	Base        LayerBase
	BaseTreeOID string
	RepoDir     string
	RootPath    string
	WorkspaceID string
	ProjectID   string
	PrePublish  func(context.Context, int64) error
}

// BuildClaimedDedicatedDelta reuses commit-layer sparse planning without calling
// BuildCommitLayer/Build: those APIs stamp a commit kind and allocate a different
// generation. This method publishes payload only. Every successful caller,
// including followers/ready reuse, still must use guarded catalog adoption.
func (b *SparseGenerationBuilder) BuildClaimedDedicatedDelta(ctx context.Context, request ClaimedDedicatedDeltaRequest) (int64, BuildReport, error) {
	started := time.Now()
	claim := request.Claim
	if ctx == nil || b == nil || b.Store == nil || b.Registry == nil || b.Logger == nil {
		return 0, BuildReport{}, fmt.Errorf("indexer: claimed delta requires context, store, registry and logger")
	}
	if err := ctx.Err(); err != nil {
		return 0, BuildReport{}, err
	}
	if claim.GenerationID <= 0 || claim.BaseGenerationID <= 0 || claim.AttemptToken == "" {
		return 0, BuildReport{}, fmt.Errorf("%w: dedicated delta requires a positive reservation and parent", store_sqlite.ErrDedicatedBaseCandidate)
	}
	catalog := b.Store.Catalog()
	p, found, err := catalog.DedicatedBasePublication(ctx, claim.Desire.Authority.GraphID)
	if err != nil {
		return 0, BuildReport{}, err
	}
	if !found || !dedicatedDeltaClaimMatches(p.Claim, claim) ||
		(p.AttemptState != "building" && p.AttemptState != "ready" && p.AttemptState != "adopted") {
		return 0, BuildReport{}, fmt.Errorf("%w: dedicated delta reservation changed", store_sqlite.ErrCatalogStaleGuard)
	}
	// ExistingGenerationID makes this an atomic validate-only transaction. The
	// preceding read is only an early rejection, never an allocation fence.
	validated, err := catalog.ClaimDedicatedBaseBuild(ctx, store_sqlite.ClaimDedicatedBaseBuildRequest{
		ExistingGenerationID: claim.GenerationID,
		Desire:               claim.Desire, ExpectedActiveGenerationID: claim.ExpectedActiveGenerationID,
		AttemptToken: claim.AttemptToken, BaseGenerationID: claim.BaseGenerationID,
		LayerID: claim.LayerID, LowerViewFingerprint: claim.LowerViewFingerprint,
	})
	if err != nil {
		return 0, BuildReport{}, err
	}
	if !dedicatedDeltaClaimMatches(validated, claim) {
		return 0, BuildReport{}, fmt.Errorf("%w: dedicated delta association was replaced", store_sqlite.ErrCatalogStaleGuard)
	}
	row, found, err := catalog.GetViewGeneration(ctx, claim.GenerationID)
	if err != nil {
		return 0, BuildReport{}, err
	}
	identity := claim.Desire.Identity
	if !found || row.GenerationID <= claim.Desire.Authority.GenerationFloor ||
		row.OwnerKind != "dedicated_graph" || row.GenerationKind != "dedicated" ||
		row.GraphID != claim.Desire.Authority.GraphID || row.CheckoutID != claim.Desire.Authority.Owner.CheckoutID ||
		row.BaseGenerationID != claim.BaseGenerationID || row.LayerID != claim.LayerID || row.LowerViewFingerprint != claim.LowerViewFingerprint ||
		row.TreeOID != identity.TreeOID || row.ConfigHash != identity.ConfigHash ||
		row.ExtractorVersions != identity.ExtractorVersions || row.ResolverVersion != identity.ResolverVersion || row.DependencyRevision != identity.DependencyRevision {
		return 0, BuildReport{}, fmt.Errorf("%w: incompatible dedicated delta identity", store_sqlite.ErrDedicatedBaseCandidate)
	}
	parent, found, err := catalog.GetViewGeneration(ctx, claim.BaseGenerationID)
	if err != nil {
		return 0, BuildReport{}, err
	}
	if !found || parent.GenerationID <= claim.Desire.Authority.GenerationFloor ||
		(parent.State != store_sqlite.ViewGenerationReady && parent.State != store_sqlite.ViewGenerationSuperseded) ||
		parent.OwnerKind != "dedicated_graph" || parent.GenerationKind != "dedicated" ||
		parent.GraphID != row.GraphID || parent.CheckoutID != row.CheckoutID || parent.TreeOID != request.BaseTreeOID ||
		parent.ConfigHash != identity.ConfigHash || parent.ExtractorVersions != identity.ExtractorVersions || parent.ResolverVersion != identity.ResolverVersion {
		return 0, BuildReport{}, fmt.Errorf("%w: dedicated delta parent tree, policy or ownership differs", store_sqlite.ErrDedicatedBaseCandidate)
	}
	if row.State == store_sqlite.ViewGenerationReady || row.State == store_sqlite.ViewGenerationSuperseded {
		return row.GenerationID, BuildReport{GenerationID: row.GenerationID, Coalesced: true, Duration: time.Since(started)}, nil
	}
	if row.State != store_sqlite.ViewGenerationBuilding || request.Base == nil {
		return 0, BuildReport{}, fmt.Errorf("%w: dedicated delta needs a building reservation and full lower reader", store_sqlite.ErrDedicatedBaseCandidate)
	}
	commit := CommitLayerRequest{Base: request.Base, RepoDir: request.RepoDir, RootPath: request.RootPath,
		BaseTreeOID: request.BaseTreeOID, TargetTreeOID: identity.TreeOID}
	if err := b.validateCommitLayer(&commit); err != nil {
		return 0, BuildReport{}, err
	}
	// This metadata diff precedes the flight because the existing preparation
	// callback returns a source/plan/report, not a replacement BuildRequest.
	// Followers may repeat the diff; only the leader opens/plans/parses content.
	changes, err := diffTreeChanges(ctx, commit.RepoDir, commit.BaseTreeOID, commit.TargetTreeOID)
	if err != nil {
		return 0, BuildReport{}, err
	}
	req := BuildRequest{
		Identity: GenerationIdentity{OwnerKind: row.OwnerKind, GraphID: row.GraphID, LayerID: row.LayerID,
			CheckoutID: row.CheckoutID, GenerationKind: row.GenerationKind, BaseGenerationID: row.BaseGenerationID,
			LowerViewFingerprint: row.LowerViewFingerprint, TreeOID: row.TreeOID, ProvenanceCommitOID: row.ProvenanceCommitOID,
			ConfigHash: row.ConfigHash, ExtractorVersions: row.ExtractorVersions, ResolverVersion: row.ResolverVersion, DependencyRevision: row.DependencyRevision, CreatedAt: row.CreatedAt},
		Base: request.Base, Changes: changes, RootPath: commit.RootPath,
		RepoPrefix: claim.Desire.Authority.RepoPrefix, WorkspaceID: request.WorkspaceID,
		ProjectID: request.ProjectID, PrePublish: request.PrePublish,
	}
	prepare := func(ctx context.Context) (source.ContentSource, buildPlan, BuildReport, error) {
		if err := source.VerifyGitTreeObjectsLocal(ctx, commit.RepoDir, commit.TargetTreeOID); err != nil {
			return nil, buildPlan{}, BuildReport{}, fmt.Errorf("indexer: verify dedicated delta tree: %w", err)
		}
		target, err := source.NewGitTreeSource(ctx, commit.RepoDir, commit.TargetTreeOID)
		if err != nil {
			return nil, buildPlan{}, BuildReport{}, fmt.Errorf("indexer: open dedicated delta tree: %w", err)
		}
		return b.prepareOwnedDedicatedDelta(ctx, target, req)
	}
	handle, err := b.Store.AtManagedGeneration(claim.GenerationID)
	if err != nil {
		return 0, BuildReport{}, err
	}
	failed := func(ctx context.Context, cause error) error {
		return b.Store.Catalog().FailDedicatedBaseBuild(ctx, store_sqlite.FailDedicatedBaseBuildRequest{
			Claim: claim, Error: cause.Error(),
		})
	}
	return b.buildReservedGenerationWithCallbacks(ctx, req, buildPlan{}, BuildReport{}, started,
		claim.GenerationID, handle, validated.Status != "allocated", prepare, failed)
}

func dedicatedDeltaClaimMatches(a, b store_sqlite.DedicatedBaseBuildClaim) bool {
	return a.Desire == b.Desire && a.AttemptToken == b.AttemptToken && a.GenerationID == b.GenerationID &&
		a.BaseGenerationID == b.BaseGenerationID && a.LayerID == b.LayerID && a.LowerViewFingerprint == b.LowerViewFingerprint &&
		a.ExpectedActiveGenerationID == b.ExpectedActiveGenerationID
}

func (b *SparseGenerationBuilder) prepareOwnedDedicatedDelta(ctx context.Context, target source.ContentSource, req BuildRequest) (source.ContentSource, buildPlan, BuildReport, error) {
	transferred := false
	defer func() {
		if !transferred {
			_ = target.Close()
		}
	}()
	req.Target = target
	if err := b.validate(ctx, &req); err != nil {
		transferred = true
		return target, buildPlan{}, BuildReport{}, err
	}
	started := time.Now()
	plan, report, err := b.planFileSetContext(ctx, req)
	report.PlanningDuration = time.Since(started)
	transferred = true // The runner closes the source on success and error.
	return target, plan, report, err
}
