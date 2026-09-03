package store_sqlite

import (
	"errors"
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

var _ graph.DerivedContractReplacer = (*Store)(nil)

// ReplaceDerivedContracts atomically replaces one exact match/topic/bridge
// frontier. Every selector is a bounded VALUES/json_each set; no whole-kind
// materialization or per-edge mutation loop is used.
//
// Every selector also binds the handle's payload view generation, matching the
// generation the replacement nodes and edges are inserted at. The bridge and
// orphan-topic CTEs pair their node join and their owner-edge guard explicitly
// so a bridge still owned in another generation is not reported orphaned here.
func (s *Store) ReplaceDerivedContracts(replacement graph.DerivedContractReplacement) (graph.DerivedContractReplaceResult, error) {
	removeEdges := uniqueDerivedContractEdges(replacement.RemoveEdges)
	bridgeJSON, hasBridges := nonEmptyProjectionJSON(replacement.RemoveBridgeNodeIDs)
	topicJSON, hasTopics := nonEmptyProjectionJSON(replacement.TouchedTopicNodeIDs)
	if len(removeEdges) == 0 && !hasBridges && len(replacement.Nodes) == 0 && len(replacement.Edges) == 0 && !hasTopics {
		return graph.DerivedContractReplaceResult{}, nil
	}

	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if !s.invalidateAnalysisBeforeMutationLocked() {
		return graph.DerivedContractReplaceResult{}, errors.New("invalidate analysis before derived contract replacement")
	}
	tx, err := s.beginWrite()
	if err != nil {
		return graph.DerivedContractReplaceResult{}, err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	result := graph.DerivedContractReplaceResult{}
	for start := 0; start < len(removeEdges); start += exactEdgeRemoveChunkSize {
		end := minInt(start+exactEdgeRemoveChunkSize, len(removeEdges))
		chunk := removeEdges[start:end]
		var values strings.Builder
		args := make([]any, 0, len(chunk)*5+1)
		for i, edge := range chunk {
			if i > 0 {
				values.WriteByte(',')
			}
			values.WriteString("(?,?,?,?,?)")
			args = append(args, edge.From, edge.To, string(edge.Kind), edge.FilePath, edge.Line)
		}
		args = append(args, s.viewGen)
		removed, execErr := tx.Exec(edgeExactDeleteByIdentitySQL(values.String()), args...)
		if execErr != nil {
			return graph.DerivedContractReplaceResult{}, execErr
		}
		rows, rowsErr := removed.RowsAffected()
		if rowsErr != nil {
			return graph.DerivedContractReplaceResult{}, rowsErr
		}
		result.EdgesRemoved += int(rows)
	}

	if hasBridges {
		const bridgeIDs = `
WITH selected(id) AS (
    SELECT CAST(value AS TEXT) FROM json_each(?)
), bridges(id) AS (
    SELECT node.id
    FROM selected
    JOIN nodes AS node ON node.id = selected.id AND node.view_gen = ?
    WHERE node.kind = ?
)`
		removed, execErr := tx.Exec(bridgeIDs+`
DELETE FROM edges
WHERE (from_id IN (SELECT id FROM bridges)
    OR to_id IN (SELECT id FROM bridges))
  AND view_gen = ?`, bridgeJSON, s.viewGen, string(graph.KindContractBridge), s.viewGen)
		if execErr != nil {
			return graph.DerivedContractReplaceResult{}, execErr
		}
		rows, rowsErr := removed.RowsAffected()
		if rowsErr != nil {
			return graph.DerivedContractReplaceResult{}, rowsErr
		}
		result.EdgesRemoved += int(rows)

		removed, execErr = tx.Exec(bridgeIDs+`
DELETE FROM nodes WHERE id IN (SELECT id FROM bridges) AND view_gen = ?`,
			bridgeJSON, s.viewGen, string(graph.KindContractBridge), s.viewGen)
		if execErr != nil {
			return graph.DerivedContractReplaceResult{}, execErr
		}
		rows, rowsErr = removed.RowsAffected()
		if rowsErr != nil {
			return graph.DerivedContractReplaceResult{}, rowsErr
		}
		result.NodesRemoved += int(rows)
	}

	nodesChanged, _, _, err := insertNodeChunksTx(tx, s.viewGen, replacement.Nodes, false)
	if err != nil {
		return graph.DerivedContractReplaceResult{}, err
	}
	result.NodesChanged = nodesChanged
	edgesAdded, _, _, err := insertEdgeChunksTx(tx, s.viewGen, replacement.Edges, false)
	if err != nil {
		return graph.DerivedContractReplaceResult{}, err
	}
	result.EdgesAdded = edgesAdded

	if hasTopics {
		const orphanTopicIDs = `
WITH touched(id) AS (
    SELECT CAST(value AS TEXT) FROM json_each(?)
), orphan(id) AS (
    SELECT node.id
    FROM touched
    JOIN nodes AS node ON node.id = touched.id AND node.view_gen = ?
    WHERE node.kind = ?
      AND NOT EXISTS (
          SELECT 1 FROM edges AS owner
          WHERE owner.to_id = node.id AND owner.kind IN (?, ?)
            AND owner.view_gen = ?
      )
)`
		removed, execErr := tx.Exec(orphanTopicIDs+`
DELETE FROM edges
WHERE (from_id IN (SELECT id FROM orphan)
    OR to_id IN (SELECT id FROM orphan))
  AND view_gen = ?`,
			topicJSON, s.viewGen, string(graph.KindTopic),
			string(graph.EdgeProducesTopic), string(graph.EdgeConsumesTopic),
			s.viewGen, s.viewGen)
		if execErr != nil {
			return graph.DerivedContractReplaceResult{}, execErr
		}
		rows, rowsErr := removed.RowsAffected()
		if rowsErr != nil {
			return graph.DerivedContractReplaceResult{}, rowsErr
		}
		result.EdgesRemoved += int(rows)

		removed, execErr = tx.Exec(orphanTopicIDs+`
DELETE FROM nodes WHERE id IN (SELECT id FROM orphan) AND view_gen = ?`,
			topicJSON, s.viewGen, string(graph.KindTopic),
			string(graph.EdgeProducesTopic), string(graph.EdgeConsumesTopic),
			s.viewGen, s.viewGen)
		if execErr != nil {
			return graph.DerivedContractReplaceResult{}, execErr
		}
		rows, rowsErr = removed.RowsAffected()
		if rowsErr != nil {
			return graph.DerivedContractReplaceResult{}, rowsErr
		}
		result.NodesRemoved += int(rows)
	}

	if err := tx.Commit(); err != nil {
		return graph.DerivedContractReplaceResult{}, err
	}
	committed = true
	changed := result.EdgesRemoved > 0 || result.NodesRemoved > 0 || result.NodesChanged > 0 || result.EdgesAdded > 0
	s.finishAnalysisMutationLocked(changed)
	if changed {
		s.markMutationReceiptsIncompleteLocked()
	}
	return result, nil
}

func uniqueDerivedContractEdges(edges []*graph.Edge) []*graph.Edge {
	type key struct {
		from, to, kind, file string
		line                 int
	}
	seen := make(map[key]struct{}, len(edges))
	out := make([]*graph.Edge, 0, len(edges))
	for _, edge := range edges {
		if edge == nil {
			continue
		}
		identity := key{edge.From, edge.To, string(edge.Kind), edge.FilePath, edge.Line}
		if _, duplicate := seen[identity]; duplicate {
			continue
		}
		seen[identity] = struct{}{}
		out = append(out, edge)
	}
	return out
}
