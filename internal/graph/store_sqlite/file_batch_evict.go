package store_sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

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

// EvictFiles removes a bounded file replacement set in one transaction. The
// two edge deletes deliberately use indexed from_id/to_id predicates instead
// of an OR expression. Ordinary node frontiers stay in SQLite; only affected
// shared contract records are read to preserve surviving owners.
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
	return s.evictByPredicate(evictFilesPredicate, pathsJSON, evictThisGeneration)
}

// evictByPredicate is the common SQLite-native scope eviction path. The
// predicate is always one of the package constants above, never caller SQL.
// The scope also selects the exact-receipt path: a this-generation eviction
// names a bounded set of nodes that can be described exactly to active
// mutation receipts, while an all-generations repo sweep fails the receipt
// closed as before.
func (s *Store) evictByPredicate(predicate string, arg any, scope evictScope) (nodesRemoved, edgesRemoved int) {
	nodesRemoved, edgesRemoved, err := s.evictByPredicateResult(predicate, arg, scope)
	if err != nil {
		panicOnFatal(err)
		return 0, 0
	}
	return nodesRemoved, edgesRemoved
}

// evictByPredicateResult keeps the entire binding/edge/node change in one
// IMMEDIATE transaction. Ordinary node IDs remain in SQLite; the bounded
// contract-only plan freezes shared canonical retention before the two indexed
// edge deletes. Active observation receipts additionally read doomed identities:
// the exact-receipt path while a mutation receipt is active, a bounded
// this-generation file eviction reads the doomed nodes' identities first (same
// pattern as mutationNodeIdentitiesTx — paid only while receipts observe) so
// the receipt can stay complete instead of forcing the whole-graph fallback
// resolve.
func (s *Store) evictByPredicateResult(predicate string, arg any, scope evictScope) (nodesRemoved, edgesRemoved int, retErr error) {
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
	edgeScope := ""
	if scope == evictThisGeneration {
		scoped += ` AND view_gen = ?`
		scopeArgs = append(scopeArgs, s.viewGen)
		edgeScope = ` AND view_gen = ?`
	}
	// The binding sidecar follows the node/edge scope: an unscoped sweep would
	// strand bindings against nodes that no longer exist, and a scoped one must
	// not touch another generation's bindings for the same path.
	if _, err := tx.ExecContext(ctx, `DELETE FROM semantic_binding_types WHERE `+scoped, scopeArgs...); err != nil {
		return 0, 0, err
	}
	// File eviction is also used while reparsing. Keep its failure ledger until
	// the indexer confirms that indexing succeeded or the file was deleted.
	if predicate == evictRepoPredicate || predicate == evictNonEmptyRepoPredicate {
		if _, err := tx.ExecContext(ctx, `DELETE FROM file_index_failures WHERE `+scoped, scopeArgs...); err != nil {
			return 0, 0, err
		}
	}
	// A repository's first cold drain has no existing nodes, but the incoming
	// edge DELETE can still scan the growing edge table while its index is
	// deferred. Prove emptiness in the same writer transaction and generation
	// as the deletes: persisted repository counters are not a freshness proof.
	// Sidecar cleanup stays above this guard because bindings and failures can
	// outlive every candidate node.
	var hasCandidates bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM nodes WHERE `+scoped+` LIMIT 1)`, scopeArgs...).Scan(&hasCandidates); err != nil {
		return 0, 0, err
	}
	if !hasCandidates {
		if err := tx.Commit(); err != nil {
			return 0, 0, err
		}
		s.finishAnalysisMutationLocked(false)
		return 0, 0, nil
	}

	// File eviction may retain shared canonicals or retire off-file targets
	// whose final source owner is removed. Repository eviction is unchanged.
	nodeScoped, nodeArgs := scoped, append([]any(nil), scopeArgs...)
	plan := contractFileEvictionPlan{}
	if scope == evictThisGeneration && (predicate == evictFilePredicate || predicate == evictFilesPredicate) {
		plan, err = s.planContractFileEvictionTx(tx, predicate, arg, scoped, scopeArgs)
		if err != nil {
			return 0, 0, err
		}
		if plan.affectedJSON != "" {
			nodeScoped = `((` + scoped + `) AND kind <> ?) OR (id IN (SELECT CAST(value AS TEXT) FROM json_each(?)) AND kind = ? AND view_gen = ?)`
			nodeArgs = append(nodeArgs, string(graph.KindContract), plan.orphanJSON, string(graph.KindContract), s.viewGen)
		}
	}
	// Node subqueries and their edge-generation residual bind independently.
	edgeArgs := append(append([]any(nil), nodeArgs...), s.viewGen)
	if scope != evictThisGeneration {
		edgeArgs = append([]any(nil), nodeArgs...)
	}
	// A failure in this receipt-only read degrades the receipt to incomplete
	// (receiptDelta stays nil, the post-commit branch marks the fallback)
	// rather than blocking the eviction itself - the same choice
	// prepareSQLiteReindexReceiptTx makes for its identity read.
	//
	// The read is bound to the same generation scope as the eviction itself
	// (scoped/scopeArgs), so it describes exactly the rows this handle will
	// delete and never another generation's copy of the same path. Only a
	// this-generation file eviction is bounded enough to describe exactly; an
	// all-generations repo sweep never takes this path.
	var receiptDelta *sqliteMutationReceiptAccumulator
	if scope == evictThisGeneration && s.hasActiveMutationReceiptsLocked() {
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
		rows, err := tx.QueryContext(ctx, `SELECT id, kind, name, qual_name, file_path FROM nodes WHERE `+nodeScoped, nodeArgs...)
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
	scalarChanges, ownerEdgesRemoved, err := s.applyContractFileEvictionTx(tx, plan)
	if err != nil {
		return 0, 0, err
	}
	edgesRemoved += ownerEdgesRemoved

	scopedNodes := `SELECT id FROM nodes WHERE ` + nodeScoped
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
	result, err := tx.ExecContext(ctx, `DELETE FROM nodes WHERE `+nodeScoped, nodeArgs...)
	if err != nil {
		return 0, 0, err
	}
	removed, err := result.RowsAffected()
	if err != nil {
		return 0, 0, err
	}
	nodesRemoved = int(removed)
	changed := nodesRemoved > 0 || edgesRemoved > 0 || scalarChanges > 0
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
		if receiptDelta != nil && scalarChanges == 0 {
			s.mergeMutationReceiptLocked(receiptDelta)
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

// contractFileEvictionPlan freezes the bounded canonical frontier before any
// owner edge is deleted. Ordinary node rows stay in SQL; only endangered
// contract records and their incoming owner payloads are hydrated.
type contractFileEvictionPlan struct {
	affectedJSON  string
	orphanJSON    string
	orphanIDs     []string
	filesJSON     string
	scalarUpdates []*graph.Node
}

func (s *Store) planContractFileEvictionTx(tx *sql.Tx, predicate string, arg any, scoped string, scopeArgs []any) (contractFileEvictionPlan, error) {
	var plan contractFileEvictionPlan
	if predicate != evictFilePredicate && predicate != evictFilesPredicate {
		return plan, nil
	}
	value, ok := arg.(string)
	if !ok {
		return plan, errors.New("invalid file eviction argument")
	}
	var files []string
	if predicate == evictFilePredicate {
		files = []string{value} // Direct EvictFile retains its empty-path semantics.
		plan.filesJSON, _ = projectionJSON(files)
	} else {
		plan.filesJSON = value
		if err := json.Unmarshal([]byte(value), &files); err != nil {
			return plan, err
		}
	}
	fileSet := make(map[string]struct{}, len(files))
	for _, path := range files {
		fileSet[path] = struct{}{}
	}
	args := []any{s.viewGen, string(graph.KindContract)}
	args = append(args, scopeArgs...)
	args = append(args, scopeArgs...)
	args = append(args, s.viewGen, string(graph.EdgeProvides), string(graph.EdgeConsumes), string(graph.EdgeHandlesRoute))
	//nolint:rowserrcheck // readNodes checks Err and closes every result below.
	rows, err := tx.Query(contractFileEvictionFrontierSQL(scoped), args...)
	if err != nil {
		return plan, err
	}
	nodes := make(map[string]*graph.Node)
	var ids []string
	readNodes := func(rows *sql.Rows) ([]string, error) {
		var added []string
		for rows.Next() {
			node, scanErr := scanNodeCursor(rows)
			if scanErr != nil {
				_ = rows.Close()
				return nil, scanErr
			}
			if nodes[node.ID] != nil {
				continue
			}
			nodes[node.ID] = node
			ids = append(ids, node.ID)
			added = append(added, node.ID)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return nil, err
		}
		return added, rows.Close()
	}
	pending, err := readNodes(rows)
	if err != nil {
		return plan, err
	}
	if len(pending) == 0 {
		return plan, nil
	}
	liveOwners := make(map[string]int)
	dependents := make(map[string][]string)
	doomed := make(map[string]bool)
	var orphanIDs []string
	discovered := make(map[string]bool, len(ids))
	for _, id := range ids {
		discovered[id] = true
	}
	// Every canonical is hydrated and has incoming owners read once. Every
	// newly doomed canonical's outgoing owners are followed once, in fixed
	// batches. This is an incident closure, never a scan of all ownership rows.
	// Doomed membership is monotone and fixed before any edge DELETE.
	for len(pending) > 0 {
		for start := 0; start < len(pending); start += 128 {
			chunkJSON, _ := projectionJSON(pending[start:min(start+128, len(pending))])
			rows, err = tx.Query(`SELECT owner.to_id, owner.from_id, source.repo_prefix, owner.meta
FROM edges AS owner
JOIN nodes AS source ON source.id = owner.from_id AND source.view_gen = ?
WHERE owner.to_id IN (SELECT CAST(value AS TEXT) FROM json_each(?))
  AND owner.view_gen = ? AND owner.kind IN (?, ?, ?)
  AND source.file_path NOT IN (SELECT CAST(value AS TEXT) FROM json_each(?))
  AND owner.file_path NOT IN (SELECT CAST(value AS TEXT) FROM json_each(?))`,
				s.viewGen, chunkJSON, s.viewGen,
				string(graph.EdgeProvides), string(graph.EdgeConsumes), string(graph.EdgeHandlesRoute),
				plan.filesJSON, plan.filesJSON)
			if err != nil {
				return plan, err
			}
			for rows.Next() {
				var id, source, repo string
				var blob []byte
				if err := rows.Scan(&id, &source, &repo, &blob); err != nil {
					_ = rows.Close()
					return plan, err
				}
				if len(blob) > 0 {
					meta, err := decodeMeta(blob)
					if err != nil {
						_ = rows.Close()
						return plan, err
					}
					if claimed, explicit := meta["contract_owner_repo_prefix"].(string); explicit && claimed != repo {
						continue
					}
				}
				if !doomed[source] {
					liveOwners[id]++
				}
				dependents[source] = append(dependents[source], id)
			}
			if err := rows.Err(); err != nil {
				_ = rows.Close()
				return plan, err
			}
			if err := rows.Close(); err != nil {
				return plan, err
			}
		}
		queue := append([]string(nil), pending...)
		var newlyDoomed []string
		for head := 0; head < len(queue); head++ {
			id := queue[head]
			if doomed[id] {
				continue
			}
			node := nodes[id]
			_, fileRemoved := fileSet[node.FilePath]
			removed, _ := node.Meta["contract_owner_removed"].(bool)
			ownerBacked, _ := node.Meta["contract_owner_record"].(bool)
			if !fileRemoved && !removed && !ownerBacked {
				continue // A genuine outside-frontier legacy record survives.
			}
			if liveOwners[id] > 0 {
				continue
			}
			doomed[id] = true
			orphanIDs = append(orphanIDs, id)
			newlyDoomed = append(newlyDoomed, id)
			// A newly doomed off-file canonical is no longer a live source
			// owner for another already-loaded candidate.
			for _, dependent := range dependents[id] {
				liveOwners[dependent]--
				if liveOwners[dependent] == 0 {
					queue = append(queue, dependent)
				}
			}
		}
		pending = nil
		for start := 0; start < len(newlyDoomed); start += 128 {
			chunkJSON, _ := projectionJSON(newlyDoomed[start:min(start+128, len(newlyDoomed))])
			// Discover target IDs first so a high-fan-in canonical is not
			// repeatedly decoded when many doomed owners name it.
			rows, err = tx.Query(`SELECT DISTINCT to_id FROM edges
WHERE from_id IN (SELECT CAST(value AS TEXT) FROM json_each(?))
  AND view_gen = ? AND kind IN (?, ?, ?)`, chunkJSON, s.viewGen,
				string(graph.EdgeProvides), string(graph.EdgeConsumes), string(graph.EdgeHandlesRoute))
			if err != nil {
				return plan, err
			}
			var unseen []string
			for rows.Next() {
				var id string
				if err := rows.Scan(&id); err != nil {
					_ = rows.Close()
					return plan, err
				}
				if !discovered[id] {
					discovered[id] = true
					unseen = append(unseen, id)
				}
			}
			if err := rows.Err(); err != nil {
				_ = rows.Close()
				return plan, err
			}
			if err := rows.Close(); err != nil {
				return plan, err
			}
			for offset := 0; offset < len(unseen); offset += 128 {
				targetJSON, _ := projectionJSON(unseen[offset:min(offset+128, len(unseen))])
				//nolint:rowserrcheck // readNodes checks Err and closes every result below.
				rows, err = tx.Query(`SELECT `+lookupNodeCols+` FROM nodes
WHERE view_gen = ? AND kind = ?
  AND id IN (SELECT CAST(value AS TEXT) FROM json_each(?))`,
					s.viewGen, string(graph.KindContract), targetJSON)
				if err != nil {
					return plan, err
				}
				added, err := readNodes(rows)
				if err != nil {
					return plan, err
				}
				pending = append(pending, added...)
			}
		}
	}
	for _, id := range ids {
		if doomed[id] {
			continue
		}
		node := nodes[id]
		_, fileRemoved := fileSet[node.FilePath]
		removed, _ := node.Meta["contract_owner_removed"].(bool)
		ownerBacked, _ := node.Meta["contract_owner_record"].(bool)
		if fileRemoved && !removed && !ownerBacked {
			if node.Meta == nil {
				node.Meta = make(map[string]any, 1)
			}
			node.Meta["contract_owner_removed"] = true
			plan.scalarUpdates = append(plan.scalarUpdates, node)
		}
	}
	plan.affectedJSON, _ = projectionJSON(ids)
	// JSON projection binds a single parameter regardless of contract count.
	// Keep this frozen: deleting owner edges must not change orphan identity
	// between the outgoing/incoming edge deletes and the node delete.
	plan.orphanIDs = orphanIDs
	plan.orphanJSON, _ = projectionJSON(orphanIDs)
	if plan.orphanJSON == "" {
		plan.orphanJSON = "[]"
	}
	return plan, nil
}

func (s *Store) applyContractFileEvictionTx(tx *sql.Tx, plan contractFileEvictionPlan) (nodesChanged, edgesRemoved int, err error) {
	if plan.affectedJSON == "" {
		return 0, 0, nil
	}
	// Only frozen actually-doomed canonical IDs are removed from FTS. Ordinary
	// raw file-eviction FTS accounting remains the caller's responsibility.
	if err = s.deleteSymbolFTSTx(tx, plan.orphanIDs); err != nil {
		return 0, 0, err
	}
	nodesChanged, _, _, err = insertNodeChunksTx(tx, s.viewGen, plan.scalarUpdates, false)
	if err != nil {
		return 0, 0, err
	}
	// A retained canonical used to be deleted wholesale. Retire its removed
	// file's incident owner rows even when their symbolic source survives.
	// Do not discover unrelated FilePath-only edges outside affectedJSON.
	result, err := tx.Exec(`DELETE FROM edges
WHERE to_id IN (SELECT CAST(value AS TEXT) FROM json_each(?))
  AND file_path IN (SELECT CAST(value AS TEXT) FROM json_each(?))
  AND view_gen = ? AND kind IN (?, ?, ?)`,
		plan.affectedJSON, plan.filesJSON, s.viewGen,
		string(graph.EdgeProvides), string(graph.EdgeConsumes), string(graph.EdgeHandlesRoute))
	if err != nil {
		return 0, 0, err
	}
	count, err := result.RowsAffected()
	return nodesChanged, int(count), err
}

// contractFileEvictionFrontierSQL selects the contract records a file eviction
// endangers: contract nodes of the doomed files, and contract nodes their doomed
// source nodes own through provides/consumes/handles_route edges. The second
// arm follows only outgoing owners of doomed source nodes; an owner whose file
// matches but whose two endpoints both survive was outside EvictFiles' old
// frontier and remains outside it here.
//
// UNION ALL, not UNION: the list only feeds an IN test, so duplicates are
// irrelevant, and a de-duplicating UNION is planned as a sorted MERGE whose arms
// the planner drives from the generation indexes (id order for free), a scan of
// every node and every edge of the generation per save (4.8 s on a 946k-edge
// store).
func contractFileEvictionFrontierSQL(scoped string) string {
	affected := `SELECT id FROM nodes WHERE ` + scoped + `
UNION ALL
SELECT to_id FROM edges WHERE from_id IN (SELECT id FROM nodes WHERE ` + scoped + `)
  AND view_gen = ? AND kind IN (?, ?, ?)`
	return `SELECT ` + lookupNodeCols + ` FROM nodes
WHERE view_gen = ? AND kind = ? AND id IN (` + affected + `)`
}
