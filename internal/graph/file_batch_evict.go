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
	defer g.unlockAllWrite()

	var nodes []*Node
	for filePath := range pathSet {
		for _, shard := range g.shards {
			nodes = append(nodes, shard.byFile[filePath]...)
		}
	}
	if len(nodes) == 0 {
		return 0, 0
	}
	evictedIDs := make(map[string]string, len(nodes))
	for _, node := range nodes {
		if node != nil {
			evictedIDs[node.ID] = node.RepoPrefix
		}
	}
	// This eviction deletes every edge touching a doomed node, including
	// edges whose SOURCE survives. restubIncomingRefs parks the
	// IsResolvableRefEdge kinds under a stub first, so those stay described
	// by the name and file frontiers; the rest — accesses_field, arg_of,
	// tests, imports, contains — are destroyed outright.
	//
	// An earlier revision failed the receipt closed over exactly that, which
	// forces the whole-graph fallback resolve. The fallback cannot repair it:
	// it retargets edges that still exist and are parked under a stub, and a
	// deleted edge is neither, so both paths reach the same graph and the
	// gate bought only the cost of the larger pass. On a real package it
	// fired on nearly every reindex, a capability or structural edge from a
	// neighbouring file being the common shape rather than the exceptional
	// one. TestResolveAllDoesNotRestoreAnEvictionDestroyedCapabilityEdge in
	// internal/resolver pins the premise.
	//
	// The eviction therefore still describes its RESOLUTION delta exactly,
	// which is the only question a receipt answers. The destruction is a real
	// pre-existing defect — those edges vanish when a neighbour is reindexed
	// and nothing recreates them until the SOURCE file is reparsed — but it
	// is not a receipt-exactness problem and is tracked separately.
	g.recordEvictedNodesForReceipts(nodes)
	for _, node := range nodes {
		if node == nil {
			continue
		}
		shard := g.shardFor(node.ID)
		shard.repoNodeRemove(node)
		delete(shard.nodes, node.ID)
		if node.QualName != "" {
			if current, ok := shard.byQual[node.QualName]; ok && current.ID == node.ID {
				delete(shard.byQual, node.QualName)
			}
		}
		removeNodeFromBucket(shard.byName, shard.byNameIdx, node.Name, node.ID)
		removeNodeFromBucket(shard.byFile, shard.byFileIdx, node.FilePath, node.ID)
		removeNodeFromBucket(shard.byRepo, shard.byRepoIdx, node.RepoPrefix, node.ID)
	}
	nodesRemoved = len(evictedIDs)
	edgesRemoved = g.evictEdgesLocked(evictedIDs)
	return nodesRemoved, edgesRemoved
}

var _ FileBatchEvicter = (*Graph)(nil)
