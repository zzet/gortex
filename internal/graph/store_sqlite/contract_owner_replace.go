package store_sqlite

import (
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
	result.NodesChanged = nodesChanged
	edgesAdded, _, _, err := insertEdgeChunksTx(tx, s.viewGen, replacement.Edges, false)
	if err != nil {
		return graph.ContractOwnerReplaceResult{}, err
	}
	result.EdgesAdded = edgesAdded

	if hasTouched {
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
			touchedJSON, s.viewGen, string(graph.KindContract),
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
			touchedJSON, s.viewGen, string(graph.KindContract),
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
// without a symbol has no provides/consumes edge but remains a valid extracted
// contract; only IDs absent from the new file frontier are orphan candidates.
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
