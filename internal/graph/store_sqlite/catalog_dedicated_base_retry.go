package store_sqlite

import (
	"context"
	"database/sql"
	"fmt"
)

// failedDedicatedBaseClaimTx recognizes the narrow crash boundary where payload
// abandonment committed but publication failure notification did not. The caller
// must already have checked the current owner, desire and expected active pointer
// in this transaction. It never changes the failed payload or grants permission
// to recover a missing, retiring, foreign, or still-building generation.
func failedDedicatedBaseClaimTx(ctx context.Context, tx *sql.Tx, claim DedicatedBaseBuildClaim) (ViewGeneration, bool, error) {
	g, err := dedicatedBaseGenerationTx(ctx, tx, claim.GenerationID)
	if err != nil {
		return ViewGeneration{}, false, err
	}
	if g.State != ViewGenerationFailed {
		return g, false, nil
	}
	desire := claim.Desire
	if g.GenerationID <= desire.Authority.GenerationFloor || g.OwnerKind != "dedicated_graph" || g.GenerationKind != "dedicated" ||
		g.GraphID != desire.Authority.GraphID || g.CheckoutID != desire.Authority.Owner.CheckoutID ||
		g.TreeOID == "" || g.TreeOID != desire.Identity.TreeOID || g.ConfigHash != desire.Identity.ConfigHash ||
		g.ExtractorVersions != desire.Identity.ExtractorVersions || g.ResolverVersion != desire.Identity.ResolverVersion ||
		g.DependencyRevision != desire.Identity.DependencyRevision ||
		g.BaseGenerationID < 0 || g.BaseGenerationID != claim.BaseGenerationID || g.LayerID != claim.LayerID ||
		g.LowerViewFingerprint != claim.LowerViewFingerprint {
		return ViewGeneration{}, false, fmt.Errorf("%w: failed payload no longer matches its publication claim", ErrDedicatedBaseCandidate)
	}
	if g.BaseGenerationID > 0 {
		if _, err := validateDedicatedBaseAncestryTx(ctx, tx, desire, g.BaseGenerationID, false); err != nil {
			return ViewGeneration{}, false, err
		}
	}
	return g, true, nil
}
