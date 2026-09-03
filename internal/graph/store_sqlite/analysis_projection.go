package store_sqlite

import (
	"iter"

	"github.com/zzet/gortex/internal/graph"
)

// NodesLightSeq keeps SQLite's compact identity/location projection
// disk-resident and yields one row at a time. In particular it never builds the
// full []*Node retained by the former RunAnalysis snapshot wrapper.
func (s *Store) NodesLightSeq() iter.Seq[*graph.Node] {
	return func(yield func(*graph.Node) bool) {
		rows, err := s.db.Query(`SELECT `+lookupNodeSummaryCols+` FROM nodes WHERE view_gen = ? ORDER BY id`, s.viewGen)
		if err != nil {
			panicOnFatal(err)
			return
		}
		defer rows.Close()
		for rows.Next() {
			node, scanErr := scanNodeSummary(rows)
			if scanErr != nil {
				panicOnFatal(scanErr)
				return
			}
			if node != nil && !yield(node) {
				return
			}
		}
		if rowsErr := rows.Err(); rowsErr != nil {
			panicOnFatal(rowsErr)
		}
	}
}

// EdgesLightSeq is a true cursor-backed version of AllEdgesLight. Only the
// fixed promoted columns needed by analysis cross the driver; Meta remains on
// disk and early stop closes the cursor immediately.
func (s *Store) EdgesLightSeq(kinds ...graph.EdgeKind) iter.Seq[*graph.Edge] {
	_, args := aggDedupeEdgeKinds(kinds)
	return func(yield func(*graph.Edge) bool) {
		if len(args) == 0 {
			return
		}
		query := `SELECT ` + edgeColsLight + ` FROM edges WHERE kind IN (` +
			inPlaceholders(len(args)) + `) AND view_gen = ?`
		rows, err := s.db.Query(query, append(args, s.viewGen)...)
		if err != nil {
			panicOnFatal(err)
			return
		}
		defer rows.Close()
		for rows.Next() {
			edge, scanErr := s.scanEdgeLight(rows)
			if scanErr != nil {
				panicOnFatal(scanErr)
				return
			}
			if edge != nil && !yield(edge) {
				return
			}
		}
		if rowsErr := rows.Err(); rowsErr != nil {
			panicOnFatal(rowsErr)
		}
	}
}

// NodesByKindsSeq streams full node rows for a compact fixed kind set. Process
// scoring needs three Meta keys, but only function/method rows pay that decode;
// the rest of the node corpus is never scanned or materialized.
func (s *Store) NodesByKindsSeq(kinds ...graph.NodeKind) iter.Seq[*graph.Node] {
	_, args := aggDedupeNodeKinds(kinds)
	return func(yield func(*graph.Node) bool) {
		if len(args) == 0 {
			return
		}
		query := `SELECT ` + lookupNodeCols + ` FROM nodes WHERE kind IN (` +
			inPlaceholders(len(args)) + `) AND view_gen = ?`
		rows, err := s.db.Query(query, append(args, s.viewGen)...)
		if err != nil {
			panicOnFatal(err)
			return
		}
		defer rows.Close()
		for rows.Next() {
			node, scanErr := scanNodeCursor(rows)
			if scanErr != nil {
				panicOnFatal(scanErr)
				return
			}
			if node != nil && !yield(node) {
				return
			}
		}
		if rowsErr := rows.Err(); rowsErr != nil {
			panicOnFatal(rowsErr)
		}
	}
}

// NodeIDNamesByKindsSeq keeps symbol-name indexing disk-resident and projects
// only the two consumed columns. Empty repoPrefix is the global view used by
// cross-repository content linking; a non-empty prefix is pushed into SQL.
func (s *Store) NodeIDNamesByKindsSeq(repoPrefix string, kinds ...graph.NodeKind) iter.Seq[graph.NodeIDName] {
	_, kindArgs := aggDedupeNodeKinds(kinds)
	return func(yield func(graph.NodeIDName) bool) {
		if len(kindArgs) == 0 {
			return
		}
		query := `SELECT id, name FROM nodes WHERE kind IN (` + inPlaceholders(len(kindArgs)) + `)`
		args := append([]any(nil), kindArgs...)
		if repoPrefix != "" {
			query += ` AND repo_prefix = ?`
			args = append(args, repoPrefix)
		}
		query += ` AND view_gen = ? ORDER BY name, id`
		args = append(args, s.viewGen)
		rows, err := s.db.Query(query, args...)
		if err != nil {
			panicOnFatal(err)
			return
		}
		defer rows.Close()
		for rows.Next() {
			var row graph.NodeIDName
			if err := rows.Scan(&row.ID, &row.Name); err != nil {
				panicOnFatal(err)
				return
			}
			if !yield(row) {
				return
			}
		}
		if err := rows.Err(); err != nil {
			panicOnFatal(err)
		}
	}
}

var (
	_ graph.NodeLightSequencer          = (*Store)(nil)
	_ graph.LightEdgeSequencer          = (*Store)(nil)
	_ graph.NodesByKindsSequencer       = (*Store)(nil)
	_ graph.NodeIDNamesByKindsSequencer = (*Store)(nil)
)
