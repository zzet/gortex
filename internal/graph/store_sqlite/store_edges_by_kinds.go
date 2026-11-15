package store_sqlite

import (
	"github.com/zzet/gortex/internal/graph"
)

var _ graph.EdgesByNodeIDsKindsReader = (*Store)(nil)

// GetOutEdgesByNodeIDsKinds is GetOutEdgesByNodeIDs restricted to kinds in
// SQL, so a hub node's unrelated rows are skipped on the index entry and never
// materialised (no text columns, no meta decode). Rows keep the full payload —
// callers may persist a returned edge — and come back ascending by line, then
// kind, then insertion: within one kind exactly GetOutEdges' (line, id) order.
func (s *Store) GetOutEdgesByNodeIDsKinds(ids []string, kinds []graph.EdgeKind) map[string][]*graph.Edge {
	if len(kinds) == 0 {
		return s.GetOutEdgesByNodeIDs(ids)
	}
	return s.edgesByNodeIDsKinds(ids, kinds, true)
}

// GetInEdgesByNodeIDsKinds is the incoming twin, in GetInEdges' declared
// order: grouped by kind, then insertion.
func (s *Store) GetInEdgesByNodeIDsKinds(ids []string, kinds []graph.EdgeKind) map[string][]*graph.Edge {
	if len(kinds) == 0 {
		return s.GetInEdgesByNodeIDs(ids)
	}
	return s.edgesByNodeIDsKinds(ids, kinds, false)
}

// edgesByNodeIDsKindsQuery is the chunk statement. The endpoint IN-list and
// kind IN-list both bind against an adjacency index whose (view_gen, endpoint)
// prefix seeds the probe; kind is an index column, so a row of an unrequested
// kind is rejected on the index entry before the table row is read. The
// ORDER BY is the one the single-node adjacency statements declare and the
// index serves without a sorter (plan locked in plan_lock_test.go).
func edgesByNodeIDsKindsQuery(outgoing bool, idCount, kindCount int) string {
	col, order := "to_id", "to_id, kind, id"
	if outgoing {
		col, order = "from_id", "from_id, line, kind, id"
	}
	return `SELECT ` + lookupEdgeCols + ` FROM edges WHERE ` + col + ` IN (` + inPlaceholders(idCount) +
		`) AND view_gen = ? AND kind IN (` + inPlaceholders(kindCount) + `) ORDER BY ` + order
}

func (s *Store) edgesByNodeIDsKinds(ids []string, kinds []graph.EdgeKind, outgoing bool) map[string][]*graph.Edge {
	uniq := dedupeNonEmpty(ids)
	if len(uniq) == 0 {
		return nil
	}
	kindArgs := make([]any, 0, len(kinds))
	seenKinds := make(map[graph.EdgeKind]struct{}, len(kinds))
	for _, kind := range kinds {
		if _, dup := seenKinds[kind]; dup {
			continue
		}
		seenKinds[kind] = struct{}{}
		kindArgs = append(kindArgs, string(kind))
	}
	out := make(map[string][]*graph.Edge, len(uniq))
	for i := 0; i < len(uniq); i += lookupChunkSize {
		end := minInt(i+lookupChunkSize, len(uniq))
		chunk := uniq[i:end]
		args := make([]any, 0, len(chunk)+1+len(kindArgs))
		args = append(args, toAnyArgs(chunk)...)
		args = append(args, s.viewGen)
		args = append(args, kindArgs...)
		for _, e := range s.queryEdgesSQL(edgesByNodeIDsKindsQuery(outgoing, len(chunk), len(kindArgs)), args...) {
			if e == nil {
				continue
			}
			key := e.To
			if outgoing {
				key = e.From
			}
			out[key] = append(out[key], e)
		}
	}
	return out
}
