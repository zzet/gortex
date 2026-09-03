package store_sqlite

import "github.com/zzet/gortex/internal/graph"

// Compile-time assertion: *Store serves the meta-less kind-scoped edge scan.
var _ graph.LightEdgeScanner = (*Store)(nil)

// edgeColsLight is the meta-less edge column projection: the promoted struct
// columns WITHOUT the meta blob (and without resolve_terminal, which lives in
// Meta). It is exactly the ten columns scanEdgeLight scans and is shared by
// every meta-less SQLite edge query so projections cannot drift from the
// scanner. Adding meta back here would defeat the whole point — the per-row
// JSON decode this projection exists to skip.
const edgeColsLight = `from_id, to_id, kind, file_path, line, confidence, confidence_label, origin, tier, cross_repo`

// AllEdgesLight implements graph.LightEdgeScanner: a kind-scoped edge scan that
// never decodes the meta blob. An empty kinds list scans every edge; supplying
// only empty-string kinds matches nothing (parity with the aggregators). The
// edges_by_kind index serves the IN filter. Meta is left nil; only the promoted
// fields (origin/tier/confidence/confidence_label/cross_repo/line) are hydrated.
func (s *Store) AllEdgesLight(kinds ...graph.EdgeKind) []*graph.Edge {
	_, args := aggDedupeEdgeKinds(kinds)
	if len(args) == 0 {
		if len(kinds) > 0 {
			return nil // caller passed only empty kinds — nothing matches
		}
		return s.queryEdgesLightSQL(`SELECT `+edgeColsLight+` FROM edges WHERE view_gen = ? ORDER BY id`, s.viewGen)
	}
	q := `SELECT ` + edgeColsLight + ` FROM edges WHERE kind IN (` +
		inPlaceholders(len(args)) + `) AND view_gen = ? ORDER BY id`
	return s.queryEdgesLightSQL(q, append(args, s.viewGen)...)
}

// queryEdgesLightSQL is the meta-less sibling of queryEdgesSQL: it materialises
// the rows into a slice and closes the cursor before returning (releasing the
// single pooled connection), but scans through scanEdgeLight so the meta column
// is never transferred or decoded. Failures route through panicOnFatal like
// queryEdgesSQL: a teardown-race read degrades to empty, and anything else —
// including a row whose promoted scalars will not scan — is raised rather than
// returned as a silently truncated slice.
func (s *Store) queryEdgesLightSQL(q string, args ...any) []*graph.Edge {
	rows, err := s.db.Query(q, args...)
	if err != nil {
		panicOnFatal(err)
		return nil
	}
	defer rows.Close()
	var out []*graph.Edge
	for rows.Next() {
		e, err := s.scanEdgeLight(rows)
		if err != nil {
			panicOnFatal(err)
			return out
		}
		if e == nil {
			continue
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		panicOnFatal(err)
	}
	return out
}
