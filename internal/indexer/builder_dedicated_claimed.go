package indexer

import (
	"context"
	"fmt"
	"time"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/indexer/source"
)

// ClaimedDedicatedBaseRequest is an initial full build reserved by the catalog.
// The effective configuration identity must include output-affecting workspace
// and project context. A caller must still adopt through the catalog guard after
// this method returns, including for ready reuse and flight followers.
type ClaimedDedicatedBaseRequest struct {
	Claim       store_sqlite.DedicatedBaseBuildClaim
	RootPath    string
	WorkspaceID string
	ProjectID   string
	PrePublish  func(context.Context, int64) error
}

// BuildClaimedDedicatedBase consumes the positive reserved generation rather
// than allocating a second one. It is an initial-full-snapshot primitive, not
// the later incremental committed-base advancement path.
func (b *SparseGenerationBuilder) BuildClaimedDedicatedBase(ctx context.Context, request ClaimedDedicatedBaseRequest) (int64, BuildReport, error) {
	started := time.Now()
	claim := request.Claim
	if ctx == nil || b == nil || b.Store == nil || b.Registry == nil || b.Logger == nil {
		return 0, BuildReport{}, fmt.Errorf("indexer: claimed base requires context, store, registry and logger")
	}
	if err := ctx.Err(); err != nil {
		return 0, BuildReport{}, err
	}
	if claim.GenerationID <= 0 || claim.AttemptToken == "" || claim.BaseGenerationID != 0 || claim.LayerID != "" || claim.LowerViewFingerprint != "" {
		return 0, BuildReport{}, fmt.Errorf("indexer: initial base requires a positive full-snapshot reservation")
	}
	p, found, err := b.Store.Catalog().DedicatedBasePublication(ctx, claim.Desire.Authority.GraphID)
	if err != nil {
		return 0, BuildReport{}, err
	}
	if !found || p.Desire != claim.Desire || p.Claim.AttemptToken != claim.AttemptToken ||
		p.Claim.GenerationID != claim.GenerationID || p.Claim.BaseGenerationID != claim.BaseGenerationID ||
		p.Claim.LayerID != claim.LayerID || p.Claim.LowerViewFingerprint != claim.LowerViewFingerprint ||
		p.Claim.ExpectedActiveGenerationID != claim.ExpectedActiveGenerationID ||
		(p.AttemptState != "building" && p.AttemptState != "ready" && p.AttemptState != "adopted") {
		return 0, BuildReport{}, fmt.Errorf("%w: initial base reservation changed", store_sqlite.ErrCatalogStaleGuard)
	}
	row, found, err := b.Store.Catalog().GetViewGeneration(ctx, claim.GenerationID)
	if err != nil {
		return 0, BuildReport{}, err
	}
	identity := claim.Desire.Identity
	if !found || row.GenerationID <= claim.Desire.Authority.GenerationFloor ||
		row.OwnerKind != "dedicated_graph" || row.GenerationKind != "dedicated" ||
		row.GraphID != claim.Desire.Authority.GraphID || row.CheckoutID != claim.Desire.Authority.Owner.CheckoutID ||
		row.BaseGenerationID != 0 || row.LayerID != "" || row.LowerViewFingerprint != "" ||
		row.TreeOID != identity.TreeOID || row.ConfigHash != identity.ConfigHash ||
		row.ExtractorVersions != identity.ExtractorVersions || row.ResolverVersion != identity.ResolverVersion {
		return 0, BuildReport{}, fmt.Errorf("%w: initial base reservation has incompatible payload identity", store_sqlite.ErrDedicatedBaseCandidate)
	}
	if row.State == store_sqlite.ViewGenerationReady || row.State == store_sqlite.ViewGenerationSuperseded {
		// Never call BeginPayloadGeneration or reopen a payload seal on this path.
		// A ready snapshot must not need a Git subprocess, parse, or payload write.
		// As with the existing sparse ready/follower path, this reports reuse,
		// not freshly measured physical work. Do not scan payload to fill counts.
		return row.GenerationID, BuildReport{GenerationID: row.GenerationID, Coalesced: true,
			Duration: time.Since(started)}, nil
	}
	if row.State != store_sqlite.ViewGenerationBuilding || request.RootPath == "" {
		return 0, BuildReport{}, fmt.Errorf("%w: initial base is not a buildable reservation", store_sqlite.ErrDedicatedBaseCandidate)
	}
	// Revalidate current availability and namespace before joining expensive
	// work. This reuses the existing claim without allocating or rewriting it.
	validated, err := b.Store.Catalog().ClaimDedicatedBaseBuild(ctx, store_sqlite.ClaimDedicatedBaseBuildRequest{
		ExistingGenerationID: claim.GenerationID,
		Desire:               claim.Desire, ExpectedActiveGenerationID: claim.ExpectedActiveGenerationID,
		AttemptToken: claim.AttemptToken, BaseGenerationID: claim.BaseGenerationID,
		LayerID: claim.LayerID, LowerViewFingerprint: claim.LowerViewFingerprint,
	})
	if err != nil {
		return 0, BuildReport{}, err
	}
	if validated.GenerationID != claim.GenerationID || validated.AttemptToken != claim.AttemptToken {
		return 0, BuildReport{}, fmt.Errorf("%w: initial base reservation was replaced", store_sqlite.ErrCatalogStaleGuard)
	}
	req := BuildRequest{
		Identity: GenerationIdentity{OwnerKind: "dedicated_graph", GenerationKind: "dedicated",
			GraphID: row.GraphID, CheckoutID: row.CheckoutID, TreeOID: row.TreeOID,
			ProvenanceCommitOID: row.ProvenanceCommitOID, ConfigHash: row.ConfigHash,
			ExtractorVersions: row.ExtractorVersions, ResolverVersion: row.ResolverVersion, CreatedAt: row.CreatedAt},
		Base: graph.New(), RootPath: request.RootPath,
		RepoPrefix: claim.Desire.Authority.RepoPrefix, WorkspaceID: request.WorkspaceID,
		ProjectID: request.ProjectID, PrePublish: request.PrePublish,
	}
	prepare := func(ctx context.Context) (source.ContentSource, buildPlan, BuildReport, error) {
		target, err := source.NewGitTreeSource(ctx, request.RootPath, identity.TreeOID)
		if err != nil {
			return nil, buildPlan{}, BuildReport{}, fmt.Errorf("indexer: open claimed base snapshot: %w", err)
		}
		validation := req
		validation.Target = target
		return prepareOwnedDedicatedSnapshot(ctx, target, func() error { return b.validate(ctx, &validation) })
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
	return b.buildReservedGenerationWithCallbacks(ctx, req, buildPlan{}, BuildReport{}, started, claim.GenerationID,
		handle, validated.Status != "allocated", prepare, failed)
}

// Until this function returns the source, the physical runner cannot own it.
// Close locally on panic; transfer both successful and error returns to the
// runner for deferred closure.
func prepareOwnedDedicatedSnapshot(ctx context.Context, target source.ContentSource, validate func() error) (source.ContentSource, buildPlan, BuildReport, error) {
	transferred := false
	defer func() {
		if !transferred {
			_ = target.Close()
		}
	}()
	if err := validate(); err != nil {
		transferred = true
		return target, buildPlan{}, BuildReport{}, err
	}
	started := time.Now()
	plan, report, err := planDedicatedSnapshot(ctx, target)
	report.PlanningDuration = time.Since(started)
	transferred = true
	return target, plan, report, err
}

// planDedicatedSnapshot visits the source inventory once. It does not open
// blobs or query an existing graph. Ordinary index admission remains in the
// index pass, so this cannot accidentally omit a supported source language.
func planDedicatedSnapshot(ctx context.Context, target source.ContentSource) (buildPlan, BuildReport, error) {
	var plan buildPlan
	var report BuildReport
	err := target.Walk(ctx, func(meta source.FileMeta) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		plan.indexed = append(plan.indexed, meta.Path)
		if meta.Size > 0 {
			report.SourceBytes += meta.Size
		}
		return nil
	})
	if err != nil {
		return buildPlan{}, BuildReport{}, fmt.Errorf("indexer: enumerate dedicated base snapshot: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return buildPlan{}, BuildReport{}, err
	}
	report.AddedFiles = len(plan.indexed)
	report.IndexedPaths = plan.indexed
	return plan, report, nil
}
