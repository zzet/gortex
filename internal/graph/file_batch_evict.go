package graph

// EvictFiles removes all nodes owned by the supplied file paths and every
// edge touching those nodes under one graph-wide write section. It preserves
// EvictFile's exact indexes/counters/receipt semantics while amortising the
// lock and edge cleanup across a partial-reconcile chunk.
func (g *Graph) EvictFiles(filePaths []string) (nodesRemoved, edgesRemoved int) {
	pathSet := make(map[string]struct{}, len(filePaths))
	for _, filePath := range filePaths {
		if filePath != "" {
			pathSet[filePath] = struct{}{}
		}
	}
	if len(pathSet) == 0 {
		return 0, 0
	}
	receiptActive := g.beginReceiptMutation()
	if receiptActive {
		defer g.endReceiptMutation()
	}
	g.lockAllWrite()
	var scalarInvalidated bool
	defer func() {
		g.unlockAllWrite()
		if scalarInvalidated {
			g.markMutationReceiptsIncomplete()
		}
	}()
	nodesRemoved, edgesRemoved, scalarInvalidated = g.evictFileNodesLocked(pathSet, false)
	return nodesRemoved, edgesRemoved
}

var _ FileBatchEvicter = (*Graph)(nil)
