package graph

import (
	"errors"
	"maps"
	"sort"
)

// ContractOwnerReplacement replaces contract ownership rows emitted by an
// exact repository/file frontier. TouchedNodeIDs includes both prior and current
// canonical contract IDs so orphan pruning handles rename and deletion.
type ContractOwnerReplacement struct {
	RepoPrefix     string
	FilePaths      []string
	TouchedNodeIDs []string
	Nodes          []*Node
	Edges          []*Edge
}

// ContractOwnerReplaceResult reports actual set-oriented mutations.
type ContractOwnerReplaceResult struct {
	EdgesRemoved int
	NodesRemoved int
	NodesChanged int
	EdgesAdded   int
}

// ContractOwnerReplacer applies a replacement atomically when the backend can,
// including invalidation of an exact removed legacy scalar record when a
// sibling owner requires the shared canonical node to remain.
// SQLite implements one transaction; the in-memory compatibility path below is
// still set-oriented and never removes another repository's shared-ID edges.
type ContractOwnerReplacer interface {
	ReplaceContractOwners(replacement ContractOwnerReplacement) (ContractOwnerReplaceResult, error)
}

// ReplaceContractOwners selects the native atomic capability or a bounded
// adapter fallback. The fallback performs one repo/kind projection, one target
// node batch, one exact edge removal batch, one AddBatch, and one batched orphan
// check. It never scans all nodes or issues edge-shaped point queries.
func ReplaceContractOwners(store Store, replacement ContractOwnerReplacement) (ContractOwnerReplaceResult, error) {
	if store == nil {
		return ContractOwnerReplaceResult{}, nil
	}
	if replacer, ok := store.(ContractOwnerReplacer); ok {
		return replacer.ReplaceContractOwners(replacement)
	}

	files := make(map[string]struct{}, len(replacement.FilePaths))
	for _, filePath := range replacement.FilePaths {
		if filePath != "" {
			files[filePath] = struct{}{}
		}
	}
	var candidates []*Edge
	var targetIDs []string
	for _, row := range ReadRepoEdgesByKinds(store, []string{replacement.RepoPrefix}, []EdgeKind{
		EdgeProvides, EdgeConsumes, EdgeHandlesRoute,
	}) {
		if row.Edge == nil {
			continue
		}
		if _, ok := files[row.Edge.FilePath]; !ok {
			continue
		}
		candidates = append(candidates, row.Edge)
		targetIDs = append(targetIDs, row.Edge.To)
	}
	targets := store.GetNodesByIDs(targetIDs)
	stale := make([]*Edge, 0, len(candidates))
	for _, edge := range candidates {
		if target := targets[edge.To]; target != nil && target.Kind == KindContract {
			stale = append(stale, edge)
		}
	}

	pruneIDs := contractOwnerPruneIDs(replacement)
	remover, canRemove := store.(ExactEdgeBatchRemover)
	if !canRemove && len(stale) > 0 {
		return ContractOwnerReplaceResult{}, errors.New("exact edge batch removal unsupported")
	}
	if _, canEvict := store.(ContractNodeBatchEvicter); !canEvict && len(pruneIDs) > 0 {
		return ContractOwnerReplaceResult{}, errors.New("contract node batch eviction unsupported")
	}

	result := ContractOwnerReplaceResult{}
	if invalidator, ok := store.(ContractOwnerScalarInvalidator); ok {
		result.NodesChanged = invalidator.InvalidateContractOwnerScalars(replacement)
	}
	if len(stale) > 0 {
		result.EdgesRemoved = remover.RemoveEdgesExact(stale)
	}
	if len(replacement.Nodes) > 0 || len(replacement.Edges) > 0 {
		store.AddBatch(replacement.Nodes, replacement.Edges)
		result.NodesChanged += len(replacement.Nodes)
		result.EdgesAdded = len(replacement.Edges)
	}
	if len(pruneIDs) == 0 {
		return result, nil
	}

	currentNodes := store.GetNodesByIDs(pruneIDs)
	incoming := store.GetInEdgesByNodeIDs(pruneIDs)
	orphanIDs := make([]string, 0, len(pruneIDs))
	for _, id := range pruneIDs {
		node := currentNodes[id]
		if node != nil && node.Kind == KindContract {
			removed, _ := node.Meta["contract_owner_removed"].(bool)
			ownerBacked, _ := node.Meta["contract_owner_record"].(bool)
			_, removedFile := files[node.FilePath]
			if !removed && !ownerBacked && (node.RepoPrefix != replacement.RepoPrefix || !removedFile) {
				continue // This exact legacy scalar belongs to a surviving frontier.
			}
		}
		owned := false
		for _, edge := range incoming[id] {
			if edge != nil && (edge.Kind == EdgeProvides || edge.Kind == EdgeConsumes || edge.Kind == EdgeHandlesRoute) {
				owned = true
				break
			}
		}
		if !owned {
			orphanIDs = append(orphanIDs, id)
		}
	}
	if len(orphanIDs) > 0 {
		nodes, edges, _ := EvictContractNodesByIDs(store, orphanIDs)
		result.NodesRemoved = nodes
		result.EdgesRemoved += edges
	}
	return result, nil
}

// ContractOwnerScalarInvalidator invalidates a removed scalar record only if
// that record still belongs to the exact replacement repository/file frontier.
// It is a conditional node mutation, not an atomic replacement of owner edges.
type ContractOwnerScalarInvalidator interface {
	InvalidateContractOwnerScalars(replacement ContractOwnerReplacement) int
}

// InvalidateContractOwnerScalars uses the same node lock, index accounting and
// receipt boundaries as AddNode, but rechecks ownership under the lock before
// copying metadata. A sibling that replaced the shared canonical ID wins: its
// current record never gets replaced with a stale pre-lock snapshot.
func (g *Graph) InvalidateContractOwnerScalars(replacement ContractOwnerReplacement) int {
	files := make(map[string]struct{}, len(replacement.FilePaths))
	for _, path := range replacement.FilePaths {
		if path != "" {
			files[path] = struct{}{}
		}
	}
	ids := contractOwnerPruneIDs(replacement)
	if len(files) == 0 || len(ids) == 0 {
		return 0
	}
	receiptActive := g.beginReceiptMutation()
	if receiptActive {
		defer g.endReceiptMutation()
	}
	changed := 0
	for _, id := range ids {
		shard := g.shardFor(id)
		shard.mu.Lock()
		if g.invalidateContractOwnerScalarLocked(id, replacement.RepoPrefix, files) {
			changed++
		}
		shard.mu.Unlock()
	}
	if changed > 0 {
		g.nodeMutGen.Add(1)
		// The removed scalar is no longer a usable contract record even though
		// the shared canonical node and sibling edges remain. This is not an
		// add-only frontier; do not leave an observation receipt complete.
		g.markMutationReceiptsIncomplete()
	}
	return changed
}

func (g *Graph) invalidateContractOwnerScalarLocked(id, repo string, files map[string]struct{}) bool {
	shard := g.shardFor(id)
	current := shard.nodes[id]
	if !contractOwnerScalarMatchesRemovedFrontier(current, repo, files) {
		return false
	}
	updated := *current
	updated.Meta = maps.Clone(current.Meta)
	if updated.Meta == nil {
		updated.Meta = make(map[string]any, 1)
	}
	updated.Meta["contract_owner_removed"] = true
	g.addNodeLocked(shard, &updated)
	return true
}

func contractOwnerScalarMatchesRemovedFrontier(node *Node, repo string, files map[string]struct{}) bool {
	if node == nil || node.Kind != KindContract || node.RepoPrefix != repo {
		return false
	}
	if _, removed := files[node.FilePath]; !removed {
		return false
	}
	if removed, _ := node.Meta["contract_owner_removed"].(bool); removed {
		return false
	}
	if ownerBacked, _ := node.Meta["contract_owner_record"].(bool); ownerBacked {
		return false // fallback already requires the corresponding owner row
	}
	return true
}

func contractOwnerPruneIDs(replacement ContractOwnerReplacement) []string {
	current := make(map[string]struct{}, len(replacement.Nodes))
	for _, node := range replacement.Nodes {
		if node != nil && node.ID != "" {
			current[node.ID] = struct{}{}
		}
	}
	seen := make(map[string]struct{}, len(replacement.TouchedNodeIDs))
	for _, id := range replacement.TouchedNodeIDs {
		if id == "" {
			continue
		}
		if _, keep := current[id]; keep {
			continue
		}
		seen[id] = struct{}{}
	}
	out := make([]string, 0, len(seen))
	for id := range seen {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}
