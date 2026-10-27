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
// writers, FileIndexFailureWriter, ExactEdgeBatchRemover,
// DerivedContractReplacer) are intentionally absent: they are not journaled,
// so exposing them through composition would recreate exactly the silent
// unjournaled write this refactor removes. Their callers take the documented
// capability fallbacks.

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
	}
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
)
