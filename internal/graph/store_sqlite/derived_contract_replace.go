package store_sqlite

import (
	"database/sql"
	"encoding/json"
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
		rows, execErr := deleteEdgesIncidentToSetTx(tx, bridgeIDs, "bridges",
			[]any{bridgeJSON, s.viewGen, string(graph.KindContractBridge)}, s.viewGen)
		if execErr != nil {
			return graph.DerivedContractReplaceResult{}, execErr
		}
		result.EdgesRemoved += int(rows)

		removed, execErr := tx.Exec(bridgeIDs+`
DELETE FROM nodes WHERE id IN (SELECT id FROM bridges) AND view_gen = ?`,
			bridgeJSON, s.viewGen, string(graph.KindContractBridge), s.viewGen)
		if execErr != nil {
			return graph.DerivedContractReplaceResult{}, execErr
		}
		nodeRows, rowsErr := removed.RowsAffected()
		if rowsErr != nil {
			return graph.DerivedContractReplaceResult{}, rowsErr
		}
		result.NodesRemoved += int(nodeRows)
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
		// The orphan set is read once, before any edge is deleted, exactly as
		// the former single statement evaluated its uncorrelated subquery;
		// the two index-driven DELETEs then share that frozen set.
		orphanJSON, execErr := selectIDSetJSONTx(tx, orphanTopicIDs+`
SELECT id FROM orphan`,
			topicJSON, s.viewGen, string(graph.KindTopic),
			string(graph.EdgeProducesTopic), string(graph.EdgeConsumesTopic), s.viewGen)
		if execErr != nil {
			return graph.DerivedContractReplaceResult{}, execErr
		}
		rows, execErr := deleteEdgesIncidentToSetTx(tx, `
WITH frozen(id) AS (SELECT CAST(value AS TEXT) FROM json_each(?))`, "frozen",
			[]any{orphanJSON}, s.viewGen)
		if execErr != nil {
			return graph.DerivedContractReplaceResult{}, execErr
		}
		result.EdgesRemoved += int(rows)

		removed, execErr := tx.Exec(orphanTopicIDs+`
DELETE FROM nodes WHERE id IN (SELECT id FROM orphan) AND view_gen = ?`,
			topicJSON, s.viewGen, string(graph.KindTopic),
			string(graph.EdgeProducesTopic), string(graph.EdgeConsumesTopic),
			s.viewGen, s.viewGen)
		if execErr != nil {
			return graph.DerivedContractReplaceResult{}, execErr
		}
		nodeRows, rowsErr := removed.RowsAffected()
		if rowsErr != nil {
			return graph.DerivedContractReplaceResult{}, rowsErr
		}
		result.NodesRemoved += int(nodeRows)
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

// deleteEdgesIncidentToSetTx deletes the edges of generation viewGen whose
// source or target is in the id set named setName, which cte (a WITH clause
// bound by cteArgs) defines. It issues one DELETE per endpoint column: an
// `from_id IN set OR to_id IN set` predicate is planned as a scan of every edge
// of the generation (52 s on a 946k-edge store for an empty bridge set), while
// each single-column form is an index probe per id. The id set must not depend
// on the edges being deleted (callers freeze such sets first), so the two
// DELETEs remove exactly the rows of the former disjunction.
func deleteEdgesIncidentToSetTx(tx *sql.Tx, cte, setName string, cteArgs []any, viewGen int64) (int64, error) {
	var total int64
	for _, column := range []string{"from_id", "to_id"} {
		args := append(append([]any(nil), cteArgs...), viewGen)
		removed, err := tx.Exec(cte+`
DELETE FROM edges WHERE `+column+` IN (SELECT id FROM `+setName+`) AND view_gen = ?`, args...)
		if err != nil {
			return total, err
		}
		rows, err := removed.RowsAffected()
		if err != nil {
			return total, err
		}
		total += rows
	}
	return total, nil
}

// selectIDSetJSONTx reads a one-column id query into a JSON array for a later
// json_each binding.
func selectIDSetJSONTx(tx *sql.Tx, query string, args ...any) (string, error) {
	rows, err := tx.Query(query, args...)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	ids := make([]string, 0)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return "", err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	encoded, err := json.Marshal(ids)
	if err != nil {
		return "", err
	}
	return string(encoded), nil
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
