package graph

// PathlessNodeBatchEvicter removes a bounded set of nodes that live at no
// source file — resolver stubs, synthesised module and dependency identities —
// and every incident edge in one backend operation. A node that carries a real
// file path is never removed by it: file-owned payload leaves through
// EvictFiles, which keeps the per-file indexes and sidecars consistent.
//
// "Pathless" is decided by the caller, which knows which paths are files of
// the corpus; the backend only refuses the empty-ID and unknown-ID cases.
type PathlessNodeBatchEvicter interface {
	EvictPathlessNodesByIDs(ids []string) (nodesRemoved, edgesRemoved int)
}

// EvictPathlessNodesByIDs implements the bounded in-memory capability while
// holding all shard locks once. Unknown IDs are ignored.
func (g *Graph) EvictPathlessNodesByIDs(ids []string) (nodesRemoved, edgesRemoved int) {
	if g == nil || len(ids) == 0 {
		return 0, 0
	}
	receiptActive := g.beginReceiptMutation()
	if receiptActive {
		defer g.endReceiptMutation()
	}
	g.markMutationReceiptsIncomplete()
	g.lockAllWrite()
	defer g.unlockAllWrite()

	seen := make(map[string]struct{}, len(ids))
	evicted := make(map[string]string)
	for _, id := range ids {
		if id == "" {
			continue
		}
		if _, duplicate := seen[id]; duplicate {
			continue
		}
		seen[id] = struct{}{}
		shard := g.shardFor(id)
		node := shard.nodes[id]
		if node == nil {
			continue
		}
		evicted[id] = node.RepoPrefix
		shard.repoNodeRemove(node)
		delete(shard.nodes, id)
		if node.QualName != "" {
			if current, ok := shard.byQual[node.QualName]; ok && current.ID == id {
				delete(shard.byQual, node.QualName)
			}
		}
		removeNodeFromBucket(shard.byName, shard.byNameIdx, node.Name, id)
		removeNodeFromBucket(shard.byFile, shard.byFileIdx, node.FilePath, id)
		removeNodeFromBucket(shard.byRepo, shard.byRepoIdx, node.RepoPrefix, id)
	}
	if len(evicted) == 0 {
		return 0, 0
	}
	return len(evicted), g.evictEdgesLocked(evicted)
}

var _ PathlessNodeBatchEvicter = (*Graph)(nil)
