package mcp

// Shared `_truncated_by_limit` disclosure helpers (issue #672).
//
// A result bound by `limit` is byte-indistinguishable from a complete one
// unless the response says so: the count field corroborates the wrong
// number exactly like a truncated count does. The byte budget has always
// disclosed its own cut (`_truncated_by_budget`); these helpers are the
// limit path's equivalent, shared by every tool that clamps — search_text,
// find_declaration, and graph_query.

// boundByLimit reports whether a result set stopped because of the limit
// rather than because the corpus ran out.
//
// rawCount is the count the producer returned, before any post-filter.
// Landing on the effective limit is the signal, and it can fire on a corpus
// holding exactly that many entries — a spurious "verify this" is the safe
// direction to be wrong in, against silently losing most of the result.
func boundByLimit(rawCount, limit int) bool {
	return limit > 0 && rawCount >= limit
}

// stampLimitTruncation writes the disclosure fields onto a map-shaped
// response: `_truncated_by_limit` (the flag a caller checks first),
// `_limit_applied` (the effective ceiling), `count_is_exact: false`
// (the count is a floor, not a total), and `truncation_note` (the
// human-readable escape hatch). When the hard cap rather than the caller
// chose the effective limit, the caller's requested value rides along as
// `_limit_requested`.
//
// graph_query stamps the same flat shape on its SubGraph
// (query.SubGraph's TruncatedByLimit field family), and additionally
// folds the cut into sg.Truncated so the compact renderers that carry a
// `truncated` header (gcx, TOON, compact) corroborate the flag too.
func stampLimitTruncation(resp map[string]any, requested, applied int, note string) {
	resp["_truncated_by_limit"] = true
	resp["_limit_applied"] = applied
	resp["count_is_exact"] = false
	resp["truncation_note"] = note
	if requested > applied {
		resp["_limit_requested"] = requested
	}
}
