package store_sqlite

import (
	"context"

	"github.com/zzet/gortex/internal/graph"
)

const (
	evictFilePredicate         = `file_path = ?`
	evictFilesPredicate        = `file_path IN (SELECT CAST(value AS TEXT) FROM json_each(?))`
	evictRepoPredicate         = `repo_prefix = ?`
	evictNonEmptyRepoPredicate = `repo_prefix = ? AND repo_prefix <> ''`
)

// evictScope selects whether an eviction is bound to the handle's payload view
// generation. File replacement and ordinary repository reindexing retire only
// rows written through the calling handle. Authoritative untrack is the sole
// repository path that selects every generation, matching store_purge.go.
type evictScope bool

const (
	evictThisGeneration evictScope = true
	evictAllGenerations evictScope = false
)

// evictOptions keeps the two independent eviction decisions explicit. Scope
// selects which physical payload generation is mutated; exactReceipt selects
// whether the bounded doomed-node set is described to active resolver
// receipts. Keeping them as named fields avoids conflating two boolean-shaped
// contracts at call sites.
type evictOptions struct {
	scope        evictScope
	exactReceipt bool
}

// EvictFiles removes a bounded file replacement set in one transaction. The
// two edge deletes deliberately use indexed from_id/to_id predicates instead
// of an OR expression, and keep the candidate node set inside SQLite rather
// than materialising every node ID in Go.
func (s *Store) EvictFiles(filePaths []string) (nodesRemoved, edgesRemoved int) {
	paths := make([]string, 0, len(filePaths))
	seen := make(map[string]struct{}, len(filePaths))
	for _, filePath := range filePaths {
		if filePath == "" {
			continue
		}
		if _, duplicate := seen[filePath]; duplicate {
			continue
		}
		seen[filePath] = struct{}{}
		paths = append(paths, filePath)
	}
	if len(paths) == 0 {
		return 0, 0
	}
	pathsJSON, ok := projectionJSON(paths)
	if !ok {
		return 0, 0
	}
	return s.evictByPredicate(evictFilesPredicate, pathsJSON, evictOptions{
		scope:        evictThisGeneration,
		exactReceipt: true,
	})
}

// evictByPredicate is the common SQLite-native scope eviction path. The
// predicate is always one of the package constants above, never caller SQL.
// exactReceipt marks predicates whose evicted-node set is bounded enough to
// describe exactly to active mutation receipts (the file predicates); scope
// evictions without it fail the receipt closed as before.
func (s *Store) evictByPredicate(predicate string, arg any, opts evictOptions) (nodesRemoved, edgesRemoved int) {
	nodesRemoved, edgesRemoved, err := s.evictByPredicateResult(predicate, arg, opts)
	if err != nil {
		panicOnFatal(err)
		return 0, 0
	}
	return nodesRemoved, edgesRemoved
}

// evictByPredicateResult keeps the entire binding/edge/node change in one
// IMMEDIATE transaction. Candidate node IDs remain in SQLite: the two indexed
// edge deletes consume the same predicate subquery directly, so scope size
// never creates a Go ID frontier or a DELETE-per-node loop. The one exception
// is the exact-receipt path: while a mutation receipt is active, a bounded
// file-scoped eviction reads the doomed nodes' identities first (same
// pattern as mutationNodeIdentitiesTx — paid only while receipts observe) so
// the receipt can stay complete instead of forcing the whole-graph fallback
// resolve.
func (s *Store) evictByPredicateResult(predicate string, arg any, opts evictOptions) (nodesRemoved, edgesRemoved int, retErr error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	tx, err := s.beginWrite()
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback() //nolint:errcheck // rollback after Commit is a no-op

	ctx := context.Background()
	// A generation-scoped eviction appends the residual conjunct to every
	// predicate and binds the handle's generation once per placeholder. A
	// repo-administration eviction leaves all three statements byte-identical
	// to what they were before generations existed, so it still reaches every
	// generation's rows in one pass.
	// The edge delete carries the conjunct twice: once inside the candidate-node
	// subquery and once on the edge row itself, so an edge cannot be dragged out
	// of another generation by a node id this one also uses.
	scoped, scopeArgs := predicate, []any{arg}
	edgeScope, edgeArgs := "", []any{arg}
	if opts.scope == evictThisGeneration {
		scoped += ` AND view_gen = ?`
		scopeArgs = append(scopeArgs, s.viewGen)
		edgeScope = ` AND view_gen = ?`
		edgeArgs = append(edgeArgs, s.viewGen, s.viewGen)
	}
	// A failure in this receipt-only read degrades the receipt to incomplete
	// (receiptDelta stays nil, the post-commit branch marks the fallback)
	// rather than blocking the eviction itself - the same choice
	// prepareSQLiteReindexReceiptTx makes for its identity read.
	var receiptDelta *sqliteMutationReceiptAccumulator
	if opts.exactReceipt && opts.scope == evictThisGeneration && s.hasActiveMutationReceiptsLocked() {
		// The DELETEs below remove every edge touching a doomed node,
		// including edges whose SOURCE survives. restubIncomingRefs parks the
		// IsResolvableRefEdge kinds under a stub first, so those stay
		// described by the name and file frontiers; the rest -
		// accesses_field, arg_of, tests, imports, contains - are destroyed.
		//
		// An earlier revision probed for exactly that and failed the receipt
		// closed, forcing the whole-graph fallback resolve. The fallback
		// cannot repair it: it retargets edges that still exist and are
		// parked under a stub, and a deleted edge is neither. Both paths
		// reach the same graph, so the probe bought only the cost of the
		// larger pass, and on a real package it fired on nearly every
		// reindex. The eviction still describes its RESOLUTION delta
		// exactly, which is the only question a receipt answers; the
		// destruction is a real pre-existing defect tracked separately.
		delta := newSQLiteMutationReceiptAccumulator()
		rows, err := tx.QueryContext(ctx, `SELECT id, kind, name, qual_name, file_path FROM nodes WHERE `+scoped, scopeArgs...)
		if err == nil {
			for rows.Next() {
				var id, kind, name, qualName, filePath string
				if err = rows.Scan(&id, &kind, &name, &qualName, &filePath); err != nil {
					break
				}
				recordSQLiteEvictedNode(delta, id, kind, name, qualName, filePath)
			}
			if err == nil {
				err = rows.Err()
			}
			_ = rows.Close()
		}
		if err == nil {
			receiptDelta = delta
		}
	}
	// The binding sidecar follows the node/edge scope: an unscoped sweep would
	// strand bindings against nodes that no longer exist, and a scoped one must
	// not touch another generation's bindings for the same path.
	if _, err := tx.ExecContext(ctx, `DELETE FROM semantic_binding_types WHERE `+scoped, scopeArgs...); err != nil {
		return 0, 0, err
	}
	scopedNodes := `SELECT id FROM nodes WHERE ` + scoped
	for _, column := range []string{"from_id", "to_id"} {
		result, err := tx.ExecContext(ctx, `DELETE FROM edges WHERE `+column+` IN (`+scopedNodes+`)`+edgeScope, edgeArgs...)
		if err != nil {
			return 0, 0, err
		}
		removed, err := result.RowsAffected()
		if err != nil {
			return 0, 0, err
		}
		edgesRemoved += int(removed)
	}
	result, err := tx.ExecContext(ctx, `DELETE FROM nodes WHERE `+scoped, scopeArgs...)
	if err != nil {
		return 0, 0, err
	}
	removed, err := result.RowsAffected()
	if err != nil {
		return 0, 0, err
	}
	nodesRemoved = int(removed)
	changed := nodesRemoved > 0 || edgesRemoved > 0
	invalidatedAnalysis := false
	if changed && s.analysisGenerationPresent {
		if err := invalidateAnalysisGenerationTx(tx); err != nil {
			return 0, 0, err
		}
		invalidatedAnalysis = true
	}
	if err := tx.Commit(); err != nil {
		return 0, 0, err
	}

	if invalidatedAnalysis {
		s.analysisGenerationPresent = false
	}
	s.finishAnalysisMutationLocked(changed)
	if changed {
		if receiptDelta != nil {
			s.mergeMutationReceiptLocked(receiptDelta)
		} else if opts.scope == evictAllGenerations {
			s.markAllMutationReceiptsIncompleteLocked()
		} else {
			s.markMutationReceiptsIncompleteLocked()
		}
	}
	return nodesRemoved, edgesRemoved, nil
}

// recordSQLiteEvictedNode describes one doomed node to a receipt delta. An
// evicted resolver candidate is resolution-relevant the same way an added
// one is: pending references naming it elsewhere may resolve differently
// once it is gone (and, in the evict-then-readd reindex flow, the re-add
// records the successor identity), so its file joins the definition
// frontier and the stub names graph.ReceiptNamesForEvictedSymbol maps it to
// join the target set — mirroring recordSQLiteChangedNodeIdentity's
// treatment of a vanished old identity. A candidate kind without an exact
// stub mapping fails the receipt closed.
func recordSQLiteEvictedNode(acc *sqliteMutationReceiptAccumulator, id, kind, name, qualName, filePath string) {
	if acc == nil {
		return
	}
	if filePath != "" {
		acc.changedFiles[filePath] = struct{}{}
	}
	names, exact := graph.ReceiptNamesForEvictedSymbol(graph.NodeKind(kind), name, qualName)
	if !exact {
		acc.resolutionRelevant = true
		acc.noteIncomplete("evicted_import_candidate_kind")
		return
	}
	// An empty name set is not always proof of neutrality: a file node has no
	// stub key yet is an import candidate. See
	// graph.EvictedNodeNeedsResolutionFrontier.
	if len(names) == 0 && !graph.EvictedNodeNeedsResolutionFrontier(graph.NodeKind(kind)) {
		return
	}
	acc.resolutionRelevant = true
	if id != "" {
		acc.targetIDs[id] = struct{}{}
	}
	for _, stubName := range names {
		acc.targetNames[stubName] = struct{}{}
		acc.evictedNames[stubName] = struct{}{}
	}
	if filePath != "" {
		acc.definitionFiles[filePath] = struct{}{}
	} else {
		acc.noteIncomplete("evicted_node_without_exact_file")
	}
}

var (
	_ graph.FileBatchEvicter                 = (*Store)(nil)
	_ graph.CurrentGenerationRepoEvicter     = (*Store)(nil)
	_ graph.AllGenerationsRepoEvicter        = (*Store)(nil)
	_ graph.CheckedAllGenerationsRepoEvicter = (*Store)(nil)
)
