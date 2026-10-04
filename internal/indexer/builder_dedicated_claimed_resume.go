package indexer

import (
	"context"
	"fmt"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/indexer/source"
)

// A claimed full-snapshot base survives a restart.
//
// The initial committed base of a large repository is minutes of work: one bulk
// copy of generation zero (about 290 s and 1.4 GB on a real repository) or a
// full re-parse, then enrichment, context separation, masks and producer states
// before publication. A daemon stopped inside that window used to fail the
// claim — the runner marks an unpublished generation failed and the claim
// failed — so the next start allocated a fresh generation and paid the whole
// build again, and the failed generation kept its rows until a retirement sweep
// reached it (the publication row still named it, so nothing could retire it
// sooner).
//
// Now a claimed build cancelled by its context keeps its reservation: the
// generation stays building and the claim stays live, and the next start's
// claim coalesces onto it (ClaimDedicatedBaseBuild's building branch). When the
// reservation already holds a complete payload the build resumes after the
// payload step instead of producing it again. Complete means the generation's
// own index state names the reserved commit with the reserved extractor
// versions: the copy moves generation zero's repo_index_state row in the same
// transaction as the rows (it is a per-generation sidecar), and a re-parse
// writes it at the end of its pass, so a generation carrying it holds the whole
// payload. A reservation holding a partial payload keeps the established
// recovery (a full re-derivation over it).

// claimedBaseResumeRoute is the source-plan route of a resumed build.
const claimedBaseResumeRoute = "resume_claimed_payload"

// keepReservationOnCancel marks a build context whose reservation must survive
// a cancellation (see above).
type keepReservationOnCancelKey struct{}

func withKeepReservationOnCancel(ctx context.Context) context.Context {
	return context.WithValue(ctx, keepReservationOnCancelKey{}, true)
}

// keepsReservationOnCancel reports whether a failed build over ctx must leave
// its reservation building: the context asks for it and was cancelled.
func keepsReservationOnCancel(ctx context.Context) bool {
	keep, _ := ctx.Value(keepReservationOnCancelKey{}).(bool)
	return keep && ctx.Err() != nil
}

// claimedBaseResumable reports whether the reserved generation already holds
// the complete payload of the reservation (its index state names the reserved
// commit with the reserved extractor versions, over a clean tree, and it holds
// the repository's rows).
func claimedBaseResumable(handle *store_sqlite.Store, repoPrefix string, row store_sqlite.ViewGeneration) (bool, string) {
	state, found, err := handle.GetRepoIndexState(repoPrefix)
	switch {
	case err != nil:
		return false, "the reservation's index state is unreadable"
	case !found:
		return false, "the reservation records no complete payload"
	case state.Dirty || state.IndexedSHA == "" || state.IndexedSHA != row.ProvenanceCommitOID:
		return false, "the reservation's payload describes another commit"
	case state.ExtractorVersions != row.ExtractorVersions:
		return false, "the reservation's payload was produced by other extractor versions"
	}
	if len(handle.GetRepoNodesLight(repoPrefix)) == 0 {
		return false, "the reservation records a payload it does not hold"
	}
	return true, "the reservation holds the complete payload of an interrupted build"
}

// prepareResumedDedicatedBase is the resume route's payload step: the payload
// is already in the generation, so it produces nothing and reports what the
// generation holds, exactly as the copy route reports what it copied.
func (b *SparseGenerationBuilder) prepareResumedDedicatedBase(repoPrefix string, generationID int64, handle *store_sqlite.Store) generationPayloadPreparation {
	return func(ctx context.Context) (source.ContentSource, buildPlan, BuildReport, error) {
		if err := ctx.Err(); err != nil {
			return nil, buildPlan{}, BuildReport{}, err
		}
		report, err := copiedDedicatedBaseReport(handle, repoPrefix)
		if err != nil {
			return nil, buildPlan{}, BuildReport{}, err
		}
		if state, found, err := handle.GetRepoIndexState(repoPrefix); err == nil && found {
			report.NodeCount, report.EdgeCount = state.NodeCount, state.EdgeCount
		}
		return copiedGenerationSource{identity: fmt.Sprintf("resumed-generation:%d", generationID)},
			buildPlan{}, report, nil
	}
}

var _ graph.RepoIndexStateReader = (*store_sqlite.Store)(nil)

// retireReplacedDedicatedClaim retires the generation a failed claim left
// behind, as soon as a new claim no longer names it.
//
// A failed claimed build keeps its generation's rows (the runner marks the
// generation failed; the payload stays), and the publication row keeps naming
// it until the next claim rebinds, so no retirement can take it before then —
// and the periodic sweep reached it much later: 1.4 GB of rows per abandoned
// full snapshot on a real repository. Once the claim transaction has bound a
// different generation, the failed one is referenced by nothing and is retired
// here. A refusal (a lease, a reference that appeared since) leaves it to the
// ordinary sweep, which collects failed generations like superseded ones.
func retireReplacedDedicatedClaim(ctx context.Context, store *store_sqlite.Store, previous store_sqlite.DedicatedBaseBuildClaim, previousState string, current store_sqlite.DedicatedBaseBuildClaim, inUse func(int64) bool) (bool, error) {
	if store == nil || previous.GenerationID <= 0 || previous.GenerationID == current.GenerationID || previousState != "failed" {
		return false, nil
	}
	row, found, err := store.Catalog().GetViewGeneration(ctx, previous.GenerationID)
	if err != nil || !found {
		return false, err
	}
	if row.State != store_sqlite.ViewGenerationFailed {
		return false, nil
	}
	if err := store.RetirePayloadGeneration(ctx, previous.GenerationID, inUse); err != nil {
		return false, err
	}
	return true, nil
}
