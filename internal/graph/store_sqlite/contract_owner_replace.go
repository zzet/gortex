package store_sqlite

import (
	"database/sql"
	"errors"

	"github.com/zzet/gortex/internal/graph"
)

var _ graph.ContractOwnerReplacer = (*Store)(nil)

// ReplaceContractOwners atomically replaces only the contract-ownership rows
// emitted by an exact repository/file frontier. Canonical contract IDs may be
// shared by several repositories, so stale edge deletion is guarded by both
// the source node's repository and the target node's KindContract row.
//
// Every statement here binds the handle's payload view generation, including
// the guard subqueries: the replacement nodes and edges are inserted at that
// generation, so the stale rows it supersedes are the ones that share it. The
// two node EXISTS guards pair explicitly rather than inheriting — an owner in
// another generation must neither authorize nor block a delete in this one.
func (s *Store) ReplaceContractOwners(replacement graph.ContractOwnerReplacement) (graph.ContractOwnerReplaceResult, error) {
	filesJSON, hasFiles := nonEmptyProjectionJSON(replacement.FilePaths)
	touchedJSON, hasTouched := nonEmptyProjectionJSON(contractOwnerPruneIDs(replacement))
	if !hasFiles && len(replacement.Nodes) == 0 && len(replacement.Edges) == 0 && !hasTouched {
		return graph.ContractOwnerReplaceResult{}, nil
	}

	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if !s.invalidateAnalysisBeforeMutationLocked() {
		return graph.ContractOwnerReplaceResult{}, errors.New("invalidate analysis before contract owner replacement")
	}

	tx, err := s.beginWrite()
	if err != nil {
		return graph.ContractOwnerReplaceResult{}, err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	result := graph.ContractOwnerReplaceResult{}
	if hasFiles && hasTouched {
		invalidated, invalidationErr := s.invalidateContractOwnerScalarsTx(
			tx, replacement.RepoPrefix, filesJSON, touchedJSON)
		if invalidationErr != nil {
			return graph.ContractOwnerReplaceResult{}, invalidationErr
		}
		result.NodesChanged = invalidated
	}
	if hasFiles {
		removed, execErr := tx.Exec(`
WITH owner_files(file_path) AS (
    SELECT CAST(value AS TEXT) FROM json_each(?)
)
DELETE FROM edges
WHERE kind IN (?, ?, ?)
  AND file_path IN (SELECT file_path FROM owner_files)
  AND view_gen = ?
  AND EXISTS (
      SELECT 1 FROM nodes AS source
      WHERE source.id = edges.from_id AND source.repo_prefix = ? AND source.view_gen = ?
  )
  AND EXISTS (
      SELECT 1 FROM nodes AS target
      WHERE target.id = edges.to_id AND target.kind = ? AND target.view_gen = ?
  )`, filesJSON,
			string(graph.EdgeProvides), string(graph.EdgeConsumes), string(graph.EdgeHandlesRoute),
			s.viewGen,
			replacement.RepoPrefix, s.viewGen, string(graph.KindContract), s.viewGen)
		if execErr != nil {
			return graph.ContractOwnerReplaceResult{}, execErr
		}
		rows, rowsErr := removed.RowsAffected()
		if rowsErr != nil {
			return graph.ContractOwnerReplaceResult{}, rowsErr
		}
		result.EdgesRemoved = int(rows)
	}

	nodesChanged, _, _, err := insertNodeChunksTx(tx, s.viewGen, replacement.Nodes, false)
	if err != nil {
		return graph.ContractOwnerReplaceResult{}, err
	}
	result.NodesChanged += nodesChanged
	edgesAdded, _, _, err := insertEdgeChunksTx(tx, s.viewGen, replacement.Edges, false)
	if err != nil {
		return graph.ContractOwnerReplaceResult{}, err
	}
	result.EdgesAdded = edgesAdded

	if hasTouched {
		// Exact invalidation above retired only removed scalar records. A
		// recoverable scalar from another file/repository remains a live owner
		// even if this replacement removed the final incoming owner edge.
		prunableJSON, err := s.prunableContractOwnerIDsTx(tx, touchedJSON)
		if err != nil {
			return graph.ContractOwnerReplaceResult{}, err
		}
		const orphanContractIDs = `
WITH touched(id) AS (
    SELECT CAST(value AS TEXT) FROM json_each(?)
), orphan(id) AS (
    SELECT node.id
    FROM touched
    JOIN nodes AS node ON node.id = touched.id AND node.view_gen = ?
    WHERE node.kind = ?
      AND NOT EXISTS (
          SELECT 1 FROM edges AS owner
          WHERE owner.to_id = node.id AND owner.kind IN (?, ?, ?)
            AND owner.view_gen = ?
      )
)`
		removed, execErr := tx.Exec(orphanContractIDs+`
DELETE FROM edges
WHERE (from_id IN (SELECT id FROM orphan)
    OR to_id IN (SELECT id FROM orphan))
  AND view_gen = ?`,
			prunableJSON, s.viewGen, string(graph.KindContract),
			string(graph.EdgeProvides), string(graph.EdgeConsumes), string(graph.EdgeHandlesRoute),
			s.viewGen, s.viewGen)
		if execErr != nil {
			return graph.ContractOwnerReplaceResult{}, execErr
		}
		rows, rowsErr := removed.RowsAffected()
		if rowsErr != nil {
			return graph.ContractOwnerReplaceResult{}, rowsErr
		}
		result.EdgesRemoved += int(rows)

		removed, execErr = tx.Exec(orphanContractIDs+`
DELETE FROM nodes WHERE id IN (SELECT id FROM orphan) AND view_gen = ?`,
			prunableJSON, s.viewGen, string(graph.KindContract),
			string(graph.EdgeProvides), string(graph.EdgeConsumes), string(graph.EdgeHandlesRoute),
			s.viewGen, s.viewGen)
		if execErr != nil {
			return graph.ContractOwnerReplaceResult{}, execErr
		}
		rows, rowsErr = removed.RowsAffected()
		if rowsErr != nil {
			return graph.ContractOwnerReplaceResult{}, rowsErr
		}
		result.NodesRemoved = int(rows)
	}

	if err := tx.Commit(); err != nil {
		return graph.ContractOwnerReplaceResult{}, err
	}
	committed = true
	changed := result.EdgesRemoved > 0 || result.NodesRemoved > 0 || result.NodesChanged > 0 || result.EdgesAdded > 0
	s.finishAnalysisMutationLocked(changed)
	if changed {
		s.markMutationReceiptsIncompleteLocked()
	}
	return result, nil
}

// invalidateContractOwnerScalarsTx runs only while the caller holds writeMu
// and its existing replacement transaction. It reads bounded candidate rows
// from the exact handle generation; no read-pool snapshot is blindly upserted.
func (s *Store) invalidateContractOwnerScalarsTx(tx *sql.Tx, repo, filesJSON, touchedJSON string) (int, error) {
	rows, err := tx.Query(`SELECT `+lookupNodeCols+` FROM nodes
WHERE view_gen = ? AND kind = ? AND repo_prefix = ?
  AND id IN (SELECT CAST(value AS TEXT) FROM json_each(?))
  AND file_path IN (SELECT CAST(value AS TEXT) FROM json_each(?))`,
		s.viewGen, string(graph.KindContract), repo, touchedJSON, filesJSON)
	if err != nil {
		return 0, err
	}
	var updates []*graph.Node
	for rows.Next() {
		node, scanErr := scanNodeCursor(rows)
		if scanErr != nil {
			_ = rows.Close()
			return 0, scanErr
		}
		removed, _ := node.Meta["contract_owner_removed"].(bool)
		ownerBacked, _ := node.Meta["contract_owner_record"].(bool)
		if removed || ownerBacked {
			continue
		}
		if node.Meta == nil {
			node.Meta = make(map[string]any, 1)
		}
		node.Meta["contract_owner_removed"] = true
		updates = append(updates, node)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return 0, err
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	changed, _, _, err := insertNodeChunksTx(tx, s.viewGen, updates, false)
	return changed, err
}

func (s *Store) prunableContractOwnerIDsTx(tx *sql.Tx, touchedJSON string) (string, error) {
	rows, err := tx.Query(`SELECT `+lookupNodeCols+` FROM nodes
WHERE view_gen = ? AND kind = ?
  AND id IN (SELECT CAST(value AS TEXT) FROM json_each(?))`,
		s.viewGen, string(graph.KindContract), touchedJSON)
	if err != nil {
		return "", err
	}
	var ids []string
	for rows.Next() {
		node, scanErr := scanNodeCursor(rows)
		if scanErr != nil {
			_ = rows.Close()
			return "", scanErr
		}
		removed, _ := node.Meta["contract_owner_removed"].(bool)
		ownerBacked, _ := node.Meta["contract_owner_record"].(bool)
		if removed || ownerBacked {
			ids = append(ids, node.ID)
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return "", err
	}
	if err := rows.Close(); err != nil {
		return "", err
	}
	encoded, _ := projectionJSON(ids)
	if encoded == "" {
		encoded = "[]"
	}
	return encoded, nil
}

func nonEmptyProjectionJSON(values []string) (string, bool) {
	filtered := make([]string, 0, len(values))
	for _, value := range values {
		if value != "" {
			filtered = append(filtered, value)
		}
	}
	return projectionJSON(filtered)
}

// contractOwnerPruneIDs omits current replacement nodes. A current contract
// without an admitted source owner remains a valid extracted scalar; only IDs
// absent from the new file frontier are orphan candidates.
func contractOwnerPruneIDs(replacement graph.ContractOwnerReplacement) []string {
	current := make(map[string]struct{}, len(replacement.Nodes))
	for _, node := range replacement.Nodes {
		if node != nil && node.ID != "" {
			current[node.ID] = struct{}{}
		}
	}
	prune := make([]string, 0, len(replacement.TouchedNodeIDs))
	for _, id := range replacement.TouchedNodeIDs {
		if id == "" {
			continue
		}
		if _, keep := current[id]; !keep {
			prune = append(prune, id)
		}
	}
	return prune
}
