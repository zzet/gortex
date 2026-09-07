package graph

// evictFileNodesLocked retains the existing node-file eviction frontier. It
// additionally follows ownership targets of doomed source nodes so a canonical
// node whose scalar belongs to another file does not leak after its final owner
// disappears. The caller holds every shard's write lock for the entire call.
//
// Deliberately do not scan edges by their own FilePath: an edge whose source and
// target both lie outside this frontier was untouched by the old file eviction.
// ReplaceContractOwners remains responsible for that file-record lifecycle.
//
// bumpDeletedNodeRevision preserves the existing public-path distinction:
// EvictFile bumps nodeMutGen for deletion; EvictFiles currently does not. A new
// legacy scalar mutation always bumps it, independently of that old difference.
func (g *Graph) evictFileNodesLocked(paths map[string]struct{}, bumpDeletedNodeRevision bool) (nodesRemoved, edgesRemoved int, scalarInvalidated bool) {
	original := make(map[string]*Node)
	for path := range paths {
		for _, shard := range g.shards {
			for _, node := range shard.byFile[path] {
				if node != nil {
					original[node.ID] = node
				}
			}
		}
	}
	if len(original) == 0 {
		return 0, 0, false
	}

	doomed := make(map[string]*Node, len(original))
	touched := make(map[string]*Node)
	liveOwners := make(map[string]int)
	legacySurvives := make(map[string]bool)
	dependents := make(map[string][]string)
	var queue []string
	for id, node := range original {
		if node.Kind != KindContract {
			doomed[id] = node
		}
	}
	touch := func(node *Node) {
		if node == nil || node.Kind != KindContract {
			return
		}
		if _, seen := touched[node.ID]; seen {
			return
		}
		touched[node.ID] = node
		legacySurvives[node.ID] = contractFileLegacyScalarSurvives(node, paths)
		shard := g.shardFor(node.ID)
		for _, edge := range shard.inEdges[node.ID] {
			if edge == nil {
				continue
			}
			sourceID := edge.From
			if g.contractFileOwnerSurvivesLocked(edge, sourceID, paths, doomed) {
				liveOwners[node.ID]++
				dependents[sourceID] = append(dependents[sourceID], node.ID)
			}
		}
		if liveOwners[node.ID] == 0 && !legacySurvives[node.ID] {
			queue = append(queue, node.ID)
		}
	}
	touchOwnerTargets := func(id string) {
		shard := g.shardFor(id)
		for _, edge := range shard.outEdges[id] {
			if edge != nil && contractFileOwnerKind(edge.Kind) {
				targetID := edge.To
				touch(g.shardFor(targetID).nodes[targetID])
			}
		}
	}
	for id, node := range original {
		touch(node)
		// A source record in the removed file is not a surviving owner even
		// when that source happens to be a retained shared canonical node.
		touchOwnerTargets(id)
	}
	for next := 0; next < len(queue); next++ {
		id := queue[next]
		if _, removed := doomed[id]; removed {
			continue
		}
		doomed[id] = touched[id]
		// Each live incoming owner was counted once when its target was
		// discovered. Decrement only those dependent targets, without
		// rescanning their adjacency for every newly doomed source.
		for _, targetID := range dependents[id] {
			liveOwners[targetID]--
			if liveOwners[targetID] == 0 && !legacySurvives[targetID] {
				queue = append(queue, targetID)
			}
		}
		// Follow only newly doomed ownership sources, not the entire graph.
		// Newly discovered targets exclude already-doomed incoming owners.
		touchOwnerTargets(id)
	}

	// A kept canonical may still describe the exact removed legacy scalar.
	// Copy metadata under the held lock; do not overwrite a sibling snapshot or
	// reenter the public invalidator's shard locks.
	for id, node := range touched {
		if _, removed := doomed[id]; removed {
			continue
		}
		if g.invalidateContractOwnerScalarLocked(id, node.RepoPrefix, paths) {
			scalarInvalidated = true
		}
	}

	// Remove only affected owner records incident to kept touched canonicals.
	// Their source can survive (for example, a bound handler in another file).
	// Like existing eviction, endpoints must match their adjacency buckets.
	// Snapshot insertion-time hashes before mutating either bucket; non-endpoint
	// payload changes must not make removal recompute a different hash.
	type ownerRemoval struct {
		edge   *Edge
		key    edgeHash
		source string
		target string
	}
	var ownerRemovals []ownerRemoval
	for id := range touched {
		if _, removed := doomed[id]; removed {
			continue // normal endpoint eviction removes every incident edge
		}
		shard := g.shardFor(id)
		for i, edge := range shard.inEdges[id] {
			if edge == nil || !contractFileOwnerKind(edge.Kind) {
				continue
			}
			key := shard.inEdgeKeys[id][i]
			sourceID := edge.From
			if _, removed := doomed[sourceID]; removed {
				continue // normal source eviction removes this owner
			}
			if _, removed := paths[edge.FilePath]; removed {
				ownerRemovals = append(ownerRemovals, ownerRemoval{edge, key, sourceID, id})
			}
		}
	}
	for _, removal := range ownerRemovals {
		edge := removal.edge
		from := g.shardFor(removal.source)
		to := g.shardFor(removal.target)
		var sourceRepo string
		if source := from.nodes[removal.source]; source != nil {
			sourceRepo = source.RepoPrefix
		}
		removeEdgeFromBucket(from.outEdges, from.outEdgeKeys, from.outEdgeIdx, removal.source, removal.key)
		removeEdgeFromBucket(to.inEdges, to.inEdgeKeys, to.inEdgeIdx, removal.target, removal.key)
		from.repoEdgeRemove(sourceRepo, edge)
	}

	nodes := make([]*Node, 0, len(doomed))
	evictedIDs := make(map[string]string, len(doomed))
	for id, node := range doomed {
		nodes = append(nodes, node)
		evictedIDs[id] = node.RepoPrefix
	}
	g.recordEvictedNodesForReceipts(nodes)
	for _, node := range nodes {
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
	if (bumpDeletedNodeRevision && nodesRemoved > 0) || scalarInvalidated {
		g.nodeMutGen.Add(1)
	}
	edgesRemoved = g.evictEdgesLocked(evictedIDs)
	if edgesRemoved == 0 && len(ownerRemovals) > 0 {
		g.edgeMutGen.Add(1)
	}
	edgesRemoved += len(ownerRemovals)
	return nodesRemoved, edgesRemoved, scalarInvalidated
}

func contractFileOwnerKind(kind EdgeKind) bool {
	return kind == EdgeProvides || kind == EdgeConsumes || kind == EdgeHandlesRoute
}

func contractFileLegacyScalarSurvives(node *Node, paths map[string]struct{}) bool {
	if node == nil || node.Kind != KindContract {
		return false
	}
	if _, removed := paths[node.FilePath]; removed {
		return false
	}
	removed, _ := node.Meta["contract_owner_removed"].(bool)
	backed, _ := node.Meta["contract_owner_record"].(bool)
	return !removed && !backed
}

func (g *Graph) contractFileOwnerSurvivesLocked(edge *Edge, sourceID string, paths map[string]struct{}, doomed map[string]*Node) bool {
	if edge == nil || !contractFileOwnerKind(edge.Kind) {
		return false
	}
	if _, removed := paths[edge.FilePath]; removed {
		return false
	}
	if _, removed := doomed[sourceID]; removed {
		return false
	}
	source := g.shardFor(sourceID).nodes[sourceID]
	if source == nil {
		return false
	}
	if _, removed := paths[source.FilePath]; removed {
		return false
	}
	if ownerRepo, explicit := edge.Meta["contract_owner_repo_prefix"].(string); explicit && ownerRepo != source.RepoPrefix {
		return false
	}
	return true
}
