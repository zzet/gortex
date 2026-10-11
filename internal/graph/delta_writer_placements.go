package graph

var _ NodePlacementBatchReader = (*DeltaWriter)(nil)

// NodePlacementsByIDs implements NodePlacementBatchReader: each identity's
// kind, file and repository as the delta serves it. With a projection cache,
// an identity no layer above the part of the stack the cache is kept for
// speaks for (neither the dirty chain's nor the delta's own) is answered from
// the stack's kept placement, and only the rest are read through the
// composition.
func (dw *DeltaWriter) NodePlacementsByIDs(ids []string) map[string]NodePlacement {
	out := make(map[string]NodePlacement, len(ids))
	var own, stable []string
	s := dw.stack()
	k, split := dw.cacheSplit(s)
	for _, id := range UniqueRecordingPaths(ids) {
		if id == "" {
			continue
		}
		// An identity no layer above the cached part speaks for is placed
		// where that part places it, so the composed view's answer for it is
		// the stack's.
		if split && !s.hiddenAbove(id, k) {
			stable = append(stable, id)
		} else {
			own = append(own, id)
		}
	}
	if len(stable) > 0 {
		view := dw.viewBelowLevel(k)
		if view == nil {
			view = dw.view
		}
		for id, placement := range dw.baseCache.stackPlacements(stable, func(missing []string) map[string]NodePlacement {
			return placementsOf(view.GetNodesByIDs(missing))
		}) {
			out[id] = placement
		}
	}
	if len(own) > 0 {
		for id, placement := range placementsOf(dw.view.GetNodesByIDs(own)) {
			out[id] = placement
		}
	}
	return out
}

func placementsOf(nodes map[string]*Node) map[string]NodePlacement {
	out := make(map[string]NodePlacement, len(nodes))
	for id, node := range nodes {
		if node != nil {
			out[id] = NodePlacement{Kind: node.Kind, FilePath: node.FilePath, RepoPrefix: node.RepoPrefix}
		}
	}
	return out
}

// Untouched reports whether the delta has written nothing yet: its reads are
// then the view below's answers.
func (dw *DeltaWriter) Untouched() bool {
	l := dw.layer
	l.mu.RLock()
	empty := len(l.covered) == 0 && len(l.claimed) == 0 && len(l.removed) == 0 && len(l.edgeClaims) == 0 && len(l.importSources) == 0
	l.mu.RUnlock()
	return empty && dw.work.NodeCount() == 0 && dw.work.EdgeCount() == 0
}
