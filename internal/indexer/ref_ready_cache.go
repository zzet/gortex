package indexer

import (
	"context"

	"github.com/zzet/gortex/internal/gitstate"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/viewmetrics"
	"go.uber.org/zap"
)

func (m *RefViewManager) refReadyGenerationKey(base primaryBase, treeOID string) store_sqlite.ReadyGenerationCacheKey {
	return commitLayerReadyGenerationKey(base.graphID, base.generationID, treeOID, m.configHash, m.extractors)
}

func (m *RefViewManager) releaseRefReadyLease(ctx context.Context, leaseToken string) {
	if leaseToken == "" {
		return
	}
	if err := m.catalog.ReleaseReadyGenerationLease(closingContext(ctx), leaseToken); err != nil {
		m.logger.Debug("ref view manager: could not release ready-generation lease", zap.String("lease_token", leaseToken), zap.Error(err))
	}
}

func (m *RefViewManager) retireRefReadyCandidate(ctx context.Context, candidate, winner int64) {
	if candidate <= 0 || candidate == winner {
		return
	}
	if err := m.store.RetirePayloadGeneration(closingContext(ctx), candidate, nil); err != nil {
		m.logger.Debug("ref view manager: could not retire losing ready-generation candidate", zap.Int64("candidate", candidate), zap.Int64("winner", winner), zap.Error(err))
	}
}

func (m *RefViewManager) retryRefReadyBuild(ctx context.Context, build store_sqlite.RefViewBuild, view store_sqlite.RefView, published gitstate.ResolvedSelector, built bool) RefViewResult {
	m.completeBuild(ctx, build, store_sqlite.ViewGenerationSuperseded, 0, "")
	viewmetrics.Count(viewmetrics.RefViewSelectionTotal, viewmetrics.RefViewBuilding)
	return RefViewResult{RefViewID: view.RefViewID, Resolved: published, State: store_sqlite.RefViewBuilding, Built: built}
}
