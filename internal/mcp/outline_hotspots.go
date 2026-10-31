package mcp

import (
	"context"
	"time"

	"github.com/zzet/gortex/internal/analysis"
)

// warmOutlineHotspots reuses the one existing global scorer only after an
// analysis publication succeeded and still matches the graph. The duration is
// reported separately: preparing steady-state outlines moves this work into
// the explicit analysis pass, it does not make the work disappear.
func (s *Server) warmOutlineHotspots() time.Duration {
	return s.warmOutlineHotspotsContext(context.Background())
}

func (s *Server) warmOutlineHotspotsContext(ctx context.Context) time.Duration {
	if ctx.Err() != nil || !s.analysisSnapshotStillCurrent() {
		return 0
	}
	started := time.Now()
	_ = s.getHotspotsContext(ctx)
	return time.Since(started)
}

// outlineHotspots reuses a published ranking without whole-corpus COUNTs.
// The complete analysis mutation clock and edge-identity token are the same
// proof already used by communityTokenForAnswer. Unsupported sources retain
// the existing producer/currentness path and its conservative counting.
func (s *Server) outlineHotspots() []analysis.HotspotEntry {
	writer, _ := s.analysisGenerationBackends()
	if writer != nil {
		s.analysisMu.RLock()
		if s.analysisEpoch > 0 && s.hotspotsReady && s.communitiesToken.analysisRevision == writer.AnalysisMutationRevision() && s.communitiesToken.edgeIdentity == s.graph.EdgeIdentityRevisions() {
			result := s.hotspots
			s.analysisMu.RUnlock()
			return result
		}
		s.analysisMu.RUnlock()
	}
	return s.getHotspots()
}
