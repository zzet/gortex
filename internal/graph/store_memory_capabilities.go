package graph

import "context"

// The memory backend's optional write capabilities (PR #901 review R1).
//
// The indexer reaches the live store through capability assertions, not
// through the base Store interface: FileBatchEvicter on the incremental
// reindex path, EdgeKindEvicter and ScopedEdgeKindEvicter for derived-edge
// reconciliation, PathlessNodeBatchEvicter for resolver stubs,
// ContractNodeBatchEvicter / ConfigNodeBatchEvicter for synthetic identity
// retirement, and the repo-generation evicters for administrative deletion.
//
// Before the composition refactor the embedded *Graph answered every one of
// those assertions with its own unjournaled mutator: the eviction skipped the
// journal and applyMu, so a crash before the next snapshot replayed the
// evicted state back and a concurrent journaled write raced it. Each method
// here journals the intent first and then delegates to the backing *Graph
// under the same applyMu critical section every base write uses, and replay
// completes the eviction through applyEvictionOp.
//
// The capabilities NOT exposed here (churn/coverage/blame/release enrichment
// writers and FileIndexFailureWriter) are intentionally absent: they write
// derived side-table data that is not journaled, so exposing them through
// composition would recreate exactly the silent unjournaled write this
// refactor removes. Their callers take the documented capability fallbacks
// and the memory backend simply does not offer those enrichments.

// EvictFiles journals one batch file eviction, then delegates.
func (m *MemoryStore) EvictFiles(filePaths []string) (nodesRemoved, edgesRemoved int) {
	m.journaled(&journalOp{Op: "evict_files", Paths: filePaths},
		func() {
			nodesRemoved, edgesRemoved = m.graph.EvictFiles(filePaths)
		})
	return nodesRemoved, edgesRemoved
}

// EvictFilesWithNodeIDs journals the legacy-file-identity batch eviction.
func (m *MemoryStore) EvictFilesWithNodeIDs(filePaths, nodeIDs []string) (nodesRemoved, edgesRemoved int) {
	m.journaled(&journalOp{Op: "evict_files_with_node_ids", Paths: filePaths, NodeIDs: nodeIDs},
		func() {
			nodesRemoved, edgesRemoved = m.graph.EvictFilesWithNodeIDs(filePaths, nodeIDs)
		})
	return nodesRemoved, edgesRemoved
}

// EvictEdgesByKinds journals the set-oriented edge-kind eviction.
func (m *MemoryStore) EvictEdgesByKinds(kinds []EdgeKind) int {
	removed := 0
	m.journaled(&journalOp{Op: "evict_edges_by_kinds", Kinds: kinds},
		func() { removed = m.graph.EvictEdgesByKinds(kinds) })
	return removed
}

// EvictPathlessNodesByIDs journals the bounded pathless-node eviction.
func (m *MemoryStore) EvictPathlessNodesByIDs(ids []string) (nodesRemoved, edgesRemoved int) {
	m.journaled(&journalOp{Op: "evict_pathless_nodes", IDs: ids},
		func() {
			nodesRemoved, edgesRemoved = m.graph.EvictPathlessNodesByIDs(ids)
		})
	return nodesRemoved, edgesRemoved
}

// EvictContractNodesByIDs journals the bounded contract-node eviction.
func (m *MemoryStore) EvictContractNodesByIDs(ids []string) (nodesRemoved, edgesRemoved int) {
	m.journaled(&journalOp{Op: "evict_contract_nodes", IDs: ids},
		func() {
			nodesRemoved, edgesRemoved = m.graph.EvictContractNodesByIDs(ids)
		})
	return nodesRemoved, edgesRemoved
}

// EvictConfigNodesByIDs journals the bounded config-key node eviction.
func (m *MemoryStore) EvictConfigNodesByIDs(ids []string) (nodesRemoved, edgesRemoved int) {
	m.journaled(&journalOp{Op: "evict_config_nodes", IDs: ids},
		func() {
			nodesRemoved, edgesRemoved = m.graph.EvictConfigNodesByIDs(ids)
		})
	return nodesRemoved, edgesRemoved
}

// EvictEdgesFromSourcesByKinds journals the scoped derived-edge eviction.
//
// Journal-first means the record describes the whole intent: a caller that
// cancels ctx after the record was written leaves the live graph only
// partially evicted while the journal holds the full set. Replay therefore
// completes the eviction. That over-application is the same lost-window the
// derived-cache contract already accepts for a crash between snapshots — one
// re-reconcile regenerates the edges — and it never resurrects state the
// caller asked to destroy.
func (m *MemoryStore) EvictEdgesFromSourcesByKinds(
	ctx context.Context,
	sourceIDs []string,
	kinds []EdgeKind,
) (removed int, err error) {
	m.journaled(&journalOp{Op: "evict_edges_from_sources", Sources: sourceIDs, Kinds: kinds},
		func() { removed, err = m.graph.EvictEdgesFromSourcesByKinds(ctx, sourceIDs, kinds) })
	return removed, err
}

// EvictRepoCurrentGeneration reuses the journaled EvictRepo: the memory
// backend holds one logical generation, mirroring Graph.EvictRepoCurrentGeneration.
func (m *MemoryStore) EvictRepoCurrentGeneration(repoPrefix string) (nodesRemoved, edgesRemoved int) {
	return m.EvictRepo(repoPrefix)
}

// EvictRepoAllGenerations reuses the journaled EvictRepo: one logical
// generation, mirroring Graph.EvictRepoAllGenerations.
func (m *MemoryStore) EvictRepoAllGenerations(repoPrefix string) (nodesRemoved, edgesRemoved int) {
	return m.EvictRepo(repoPrefix)
}

// RemoveEdgesExact journals the exact-identity batch removal. The coarse
// fallback (one RemoveEdge per from/to/kind) would also drop sibling call
// sites that share endpoints and kind but differ by file or line, so this
// capability must stay exposed rather than degrading through the fallback.
func (m *MemoryStore) RemoveEdgesExact(edges []*Edge) int {
	removed := 0
	m.journaled(&journalOp{Op: "remove_edges_exact", RemoveEdges: journalEdgesOf(edges)},
		func() { removed = m.graph.RemoveEdgesExact(edges) })
	return removed
}

// ReplaceDerivedContracts journals one derived-contract frontier replacement
// as a single record: edge removals, bridge/topic node retirement and the
// replacement vector replay together, so a crash never leaves half a frontier.
func (m *MemoryStore) ReplaceDerivedContracts(replacement DerivedContractReplacement) (DerivedContractReplaceResult, error) {
	var result DerivedContractReplaceResult
	var err error
	m.journaled(&journalOp{
		Op:          "replace_derived_contracts",
		RemoveEdges: journalEdgesOf(replacement.RemoveEdges),
		BridgeIDs:   replacement.RemoveBridgeNodeIDs,
		Nodes:       journalNodesOf(replacement.Nodes),
		Edges:       journalEdgesOf(replacement.Edges),
		TopicIDs:    replacement.TouchedTopicNodeIDs,
	}, func() { result, err = m.graph.ReplaceDerivedContracts(replacement) })
	return result, err
}

// applyEvictionOp replays one journaled capability eviction (see MemoryStore's
// applyOp). The *Graph primitives are the same ones the live call used, so
// the replayed state matches the applied state.
func (m *MemoryStore) applyEvictionOp(op *journalOp) {
	switch op.Op {
	case "evict_files":
		m.graph.EvictFiles(op.Paths)
	case "evict_files_with_node_ids":
		m.graph.EvictFilesWithNodeIDs(op.Paths, op.NodeIDs)
	case "evict_edges_by_kinds":
		m.graph.EvictEdgesByKinds(op.Kinds)
	case "evict_pathless_nodes":
		m.graph.EvictPathlessNodesByIDs(op.IDs)
	case "evict_contract_nodes":
		m.graph.EvictContractNodesByIDs(op.IDs)
	case "evict_config_nodes":
		m.graph.EvictConfigNodesByIDs(op.IDs)
	case "evict_edges_from_sources":
		// The cancellation context is not journalable; replay runs to
		// completion (see EvictEdgesFromSourcesByKinds).
		_, _ = m.graph.EvictEdgesFromSourcesByKinds(context.Background(), op.Sources, op.Kinds)
	case "remove_edges_exact":
		m.graph.RemoveEdgesExact(journalEdgesFrom(op.RemoveEdges))
	case "replace_derived_contracts":
		_, _ = m.graph.ReplaceDerivedContracts(DerivedContractReplacement{
			RemoveEdges:         journalEdgesFrom(op.RemoveEdges),
			RemoveBridgeNodeIDs: op.BridgeIDs,
			Nodes:               journalNodesFrom(op.Nodes),
			Edges:               journalEdgesFrom(op.Edges),
			TouchedTopicNodeIDs: op.TopicIDs,
		})
	}
}

// journalEdgesOf / journalNodesOf project slices into their journal forms;
// the *From twins rebuild them during replay.
func journalEdgesOf(edges []*Edge) []journalEdge {
	if len(edges) == 0 {
		return nil
	}
	out := make([]journalEdge, 0, len(edges))
	for _, e := range edges {
		if je := journalEdgeOf(e); je != nil {
			out = append(out, *je)
		}
	}
	return out
}

func journalEdgesFrom(edges []journalEdge) []*Edge {
	if len(edges) == 0 {
		return nil
	}
	out := make([]*Edge, 0, len(edges))
	for i := range edges {
		if e := edges[i].toEdge(); e != nil {
			out = append(out, e)
		}
	}
	return out
}

func journalNodesOf(nodes []*Node) []journalNode {
	if len(nodes) == 0 {
		return nil
	}
	out := make([]journalNode, 0, len(nodes))
	for _, n := range nodes {
		if jn := journalNodeOf(n); jn != nil {
			out = append(out, *jn)
		}
	}
	return out
}

func journalNodesFrom(nodes []journalNode) []*Node {
	if len(nodes) == 0 {
		return nil
	}
	out := make([]*Node, 0, len(nodes))
	for i := range nodes {
		if n := nodes[i].toNode(); n != nil {
			out = append(out, n)
		}
	}
	return out
}

// Compile-time proof that the only capability writes the memory backend
// exposes are the journaled ones above. A capability missing here is a
// deliberate choice documented in the file header, not an embedding accident.
var (
	_ FileBatchEvicter             = (*MemoryStore)(nil)
	_ FileIdentityBatchEvicter     = (*MemoryStore)(nil)
	_ EdgeKindEvicter              = (*MemoryStore)(nil)
	_ PathlessNodeBatchEvicter     = (*MemoryStore)(nil)
	_ ContractNodeBatchEvicter     = (*MemoryStore)(nil)
	_ ConfigNodeBatchEvicter       = (*MemoryStore)(nil)
	_ ScopedEdgeKindEvicter        = (*MemoryStore)(nil)
	_ CurrentGenerationRepoEvicter = (*MemoryStore)(nil)
	_ AllGenerationsRepoEvicter    = (*MemoryStore)(nil)
	_ ExactEdgeBatchRemover        = (*MemoryStore)(nil)
	_ DerivedContractReplacer      = (*MemoryStore)(nil)
)
