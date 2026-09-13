package indexer

import (
	"context"
	"fmt"

	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
)

// Keep ordinary committed advancement sparse, but stop extending long chains.
// A policy change or a full chain proposes a new full root. A historical ready
// snapshot can still be reused within the catalog's existing hard depth bound;
// this constant is an allocation policy, not a stronger claim about cached
// ancestry. Old routes remain intact; adoption is not retirement permission.
// This is separate from MaxRepoViewLayers, which names commit/dirty/buffer
// content layers, not the persisted ancestry inside a committed base.
//
// The number is the read side's, not this package's: every link this planner
// adds is one more OverlaidView nesting and one more mask read set on every
// lookup by every later reader of the base, so the allocation bound must be
// exactly the depth the materializer is willing to compose. Defining it as
// graphview's constant is what makes "bounded" enforceable instead of a pair
// of numbers two packages are trusted to keep equal
// (graphview.MaxGenerationAncestryDepth adds the one checkout commit layer
// that legitimately stands on a dedicated head).
const maxDedicatedBaseDeltaAncestors = graphview.MaxDedicatedBaseChainDepth

// maxDedicatedBaseActiveAncestryWalk mirrors store_sqlite's hard catalog
// ancestry limit (maxDedicatedBaseAncestry, unexported there). Walking a stored
// chain past it means the chain is not a shape this planner or that catalog can
// have produced, so it is reported rather than re-rooted around: re-rooting
// would quietly adopt a corrupt graph as a valid starting point.
// TestDedicatedBaseAncestryBoundsAreOrdered pins the relation to the literal in
// that file, so raising either number without the other fails a test instead of
// producing chains the catalog refuses to claim on.
const maxDedicatedBaseActiveAncestryWalk = 64

func (p *dedicatedBasePublisher) ensureInitial(ctx context.Context, observe func(context.Context) (dedicatedBaseObservation, error)) (dedicatedBaseResult, error) {
	return p.ensureObserved(ctx, observe, nil)
}

// ensureCurrent shares initial publication's authority and observation ordering.
// The lease domain MUST be the one used by request readers and lifecycle cleanup,
// not a temporary manager created by a watcher or by one publication attempt.
func (p *dedicatedBasePublisher) ensureCurrent(ctx context.Context, leases *graphview.LeaseManager, observe func(context.Context) (dedicatedBaseObservation, error)) (dedicatedBaseResult, error) {
	if leases == nil {
		return dedicatedBaseResult{}, fmt.Errorf("%w: advancement requires shared generation leases", errDedicatedBaseRuntimeInput)
	}
	return p.ensureObserved(ctx, observe, leases)
}

// dedicatedBaseParentForAdvance performs a bounded metadata-only ancestry check.
// The catalog revalidates the selected parent transactionally when claiming it;
// these reads are planning, not publication authority or a payload lease.
func dedicatedBaseParentForAdvance(ctx context.Context, catalog *store_sqlite.Catalog, authority store_sqlite.DedicatedBaseAuthority, active store_sqlite.ViewGeneration, target store_sqlite.DedicatedBaseIdentity) (int64, error) {
	if active.GenerationID > 0 && active.GenerationID <= authority.GenerationFloor {
		// A pre-protocol pointer is an expected-active CAS fence, not a trusted
		// committed lower snapshot. Reconstruct from Git; do not traverse or
		// materialize legacy payload merely because its stored labels match.
		return 0, nil
	}
	// The advancement policy tuple is the COMPLETE frozen-input identity, not
	// just parse policy. A dependency-revision change roots a new chain; it
	// never extends one. The catalog's ancestry validation deliberately admits
	// a same-policy parent whose revision differs (only the output candidate is
	// revision-checked there, so a derived-only refresh may borrow an older
	// lower), which makes this planner the site that enforces the chain rule:
	// a changed revision falls to the full-root branch below, and stored
	// ancestry that is not revision-homogeneous is not reusable advancement
	// ancestry — it re-roots too, it never blocks publication.
	policy := store_sqlite.DedicatedBaseIdentity{
		ConfigHash: active.ConfigHash, ExtractorVersions: active.ExtractorVersions,
		ResolverVersion: active.ResolverVersion, DependencyRevision: active.DependencyRevision,
	}
	seen := make(map[int64]struct{})
	row := active
	for {
		// A cycle and an over-long chain are separate facts with separate
		// diagnoses, so they no longer share one message. Both are refusals:
		// the walk never stops early and reports a shorter chain than it found.
		if _, duplicate := seen[row.GenerationID]; duplicate {
			return 0, fmt.Errorf("%w: cyclic active dedicated ancestry at generation %d",
				store_sqlite.ErrDedicatedBaseCandidate, row.GenerationID)
		}
		if len(seen) >= maxDedicatedBaseActiveAncestryWalk {
			return 0, fmt.Errorf("%w: active dedicated ancestry is at least %d generations deep at generation %d, over the %d-generation catalog limit",
				store_sqlite.ErrDedicatedBaseCandidate, len(seen)+1, row.GenerationID, maxDedicatedBaseActiveAncestryWalk)
		}
		if row.GenerationID <= authority.GenerationFloor || row.OwnerKind != "dedicated_graph" || row.GenerationKind != "dedicated" ||
			row.GraphID != authority.GraphID || row.CheckoutID != authority.Owner.CheckoutID || row.TreeOID == "" || row.BaseGenerationID < 0 ||
			(row.State != store_sqlite.ViewGenerationReady && row.State != store_sqlite.ViewGenerationSuperseded) ||
			row.ConfigHash != policy.ConfigHash || row.ExtractorVersions != policy.ExtractorVersions || row.ResolverVersion != policy.ResolverVersion {
			return 0, fmt.Errorf("%w: invalid active dedicated ancestry at generation %d", store_sqlite.ErrDedicatedBaseCandidate, row.GenerationID)
		}
		// Revision non-homogeneity below the head is NOT corruption, so it is
		// not an error: the catalog's ancestry validation revision-checks only
		// the output candidate (exactTree, depth 0), so a stored chain whose
		// lower carries an older revision is a shape the catalog itself admits
		// — from a mixed-binary window or a non-indexer claimer. Such a chain is
		// simply not reusable advancement ancestry, so re-root. Erroring here
		// would turn a legitimately reusable READY generation into a PERMANENT
		// publication failure: every later ensureCurrent would return the same
		// error with no path back to a full root. The builder keeps its typed
		// refusal as the last line of defence against actually composing one.
		if row.DependencyRevision != policy.DependencyRevision {
			return 0, nil
		}
		seen[row.GenerationID] = struct{}{}
		if row.BaseGenerationID == 0 {
			if row.LayerID != "" || row.LowerViewFingerprint != "" {
				return 0, fmt.Errorf("%w: dedicated root has sparse identity", store_sqlite.ErrDedicatedBaseCandidate)
			}
			break
		}
		var found bool
		var err error
		row, found, err = catalog.GetViewGeneration(ctx, row.BaseGenerationID)
		if err != nil {
			return 0, err
		}
		if !found {
			return 0, fmt.Errorf("%w: active dedicated ancestor missing", store_sqlite.ErrDedicatedBaseCandidate)
		}
	}
	if target.ConfigHash != policy.ConfigHash || target.ExtractorVersions != policy.ExtractorVersions || target.ResolverVersion != policy.ResolverVersion ||
		target.DependencyRevision != policy.DependencyRevision {
		return 0, nil
	}
	// The depth arm, stated on its own because it is a different decision from
	// the policy arms above it. A chain already holding the allocation bound's
	// worth of generations is not extended and is not truncated either: the
	// advance proposes a full root, so the new head stands alone and the old
	// chain keeps serving its own routes until nothing references it. Never
	// return a parent here on the theory that "one more" is cheap — one more is
	// paid by every reader of the resulting base on every lookup.
	if len(seen) >= maxDedicatedBaseDeltaAncestors {
		return 0, nil
	}
	return active.GenerationID, nil
}

// dedicatedBaseAncestryDepth counts the persisted chain at and beneath row,
// stopping as soon as the count passes limit. It is metadata only — the same
// bounded planning read dedicatedBaseParentForAdvance performs — and the
// returned depth is exact when it is at most limit and "more than limit"
// otherwise, which is all a bound check needs.
func dedicatedBaseAncestryDepth(ctx context.Context, catalog *store_sqlite.Catalog, row store_sqlite.ViewGeneration, limit int) (int, error) {
	seen := make(map[int64]struct{}, limit+1)
	depth := 0
	for {
		if _, duplicate := seen[row.GenerationID]; duplicate {
			return 0, fmt.Errorf("%w: cyclic dedicated ancestry at generation %d",
				store_sqlite.ErrDedicatedBaseCandidate, row.GenerationID)
		}
		seen[row.GenerationID] = struct{}{}
		depth++
		if row.BaseGenerationID == 0 || depth > limit {
			return depth, nil
		}
		next, found, err := catalog.GetViewGeneration(ctx, row.BaseGenerationID)
		if err != nil {
			return 0, err
		}
		if !found {
			return 0, fmt.Errorf("%w: dedicated ancestor %d missing", store_sqlite.ErrDedicatedBaseCandidate, row.BaseGenerationID)
		}
		row = next
	}
}

// buildObservedClaim dispatches by the reservation actually returned by the
// catalog. A cached or coalesced claim can name a different valid parent than
// the caller proposed. Never splice the observed active reader under that claim.
func (p *dedicatedBasePublisher) buildObservedClaim(ctx context.Context, observation dedicatedBaseObservation, claim store_sqlite.DedicatedBaseBuildClaim, leases *graphview.LeaseManager) (int64, BuildReport, error) {
	if claim.BaseGenerationID == 0 {
		return observation.Builder.BuildClaimedDedicatedBase(ctx, ClaimedDedicatedBaseRequest{
			Claim: claim, RootPath: observation.RootPath, WorkspaceID: observation.WorkspaceID,
			ProjectID: observation.ProjectID, PrePublish: observation.PrePublish,
		})
	}
	if leases == nil {
		return 0, BuildReport{}, fmt.Errorf("%w: initial runtime cannot consume a claimed delta", store_sqlite.ErrDedicatedBaseCandidate)
	}
	parent, found, err := p.runtime.store.Catalog().GetViewGeneration(ctx, claim.BaseGenerationID)
	if err != nil {
		return 0, BuildReport{}, err
	}
	if !found || parent.TreeOID == "" {
		return 0, BuildReport{}, fmt.Errorf("%w: claimed dedicated parent missing", store_sqlite.ErrDedicatedBaseCandidate)
	}
	// The claim's parent is not necessarily the one the planner proposed — a
	// cached or coalesced reservation names its own valid parent, and the
	// catalog's own ancestry validation admits any chain under its hard
	// 64-generation limit. So the depth bound is re-checked here, on the parent
	// actually claimed, before any payload is written: a delta built over a
	// chain already at the bound would publish a head no reader can compose,
	// which is a far worse outcome than refusing the build. Refusing before the
	// catalog's hard limit also keeps the refusal attributable — the next
	// advance observes no new head and proposes a full root.
	depth, err := dedicatedBaseAncestryDepth(ctx, p.runtime.store.Catalog(), parent, maxDedicatedBaseDeltaAncestors)
	if err != nil {
		return 0, BuildReport{}, err
	}
	if depth >= maxDedicatedBaseDeltaAncestors {
		return 0, BuildReport{}, fmt.Errorf("%w: claimed dedicated parent %d is %d generations deep; a delta over it would pass the %d-generation ancestry bound",
			store_sqlite.ErrDedicatedBaseCandidate, claim.BaseGenerationID, depth, maxDedicatedBaseDeltaAncestors)
	}
	request := ClaimedDedicatedDeltaRequest{
		Claim: claim, BaseTreeOID: parent.TreeOID, RepoDir: observation.RootPath,
		RootPath: observation.RootPath, WorkspaceID: observation.WorkspaceID,
		ProjectID: observation.ProjectID, PrePublish: observation.PrePublish,
	}
	if claim.Status != "ready" && !claim.AlreadyAdopted {
		materializer := graphview.Materializer{Store: p.runtime.store, Catalog: p.runtime.store.Catalog(), Leases: leases, Logger: observation.Builder.Logger}
		view, err := materializer.MaterializeRefView(ctx, claim.Desire.Authority.GraphID, claim.BaseGenerationID)
		if err != nil {
			return 0, BuildReport{}, err
		}
		defer view.Close()
		request.Base = commitLayerBase{Reader: view.Reader, corpus: p.runtime.store.AtGeneration(claim.BaseGenerationID)}
	}
	return observation.Builder.BuildClaimedDedicatedDelta(ctx, request)
}
