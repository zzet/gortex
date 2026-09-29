package store_sqlite

import "github.com/zzet/gortex/internal/graph"

// Compile-time assertion: *Store serves full edge rows by recording file.
var _ graph.RecordedEdgeReader = (*Store)(nil)

// recordedEdgesAtPrefix reaches the rows through edges_by_file (file_path,
// kind), with the generation as a residual filter; the `+` keeps the planner
// from preferring the generation index, as in edgeEndpointsRecordedAt. The
// column list is the full-row readers' own (lookupEdgeCols), scanned by the
// same cursor scanner, so a row carries what GetOutEdgesByNodeIDs returns for
// it and the structural read filter applies identically.
const recordedEdgesAtPrefix = `SELECT ` + lookupEdgeCols + ` FROM edges WHERE file_path IN (`
const recordedEdgesAtSuffix = `) AND +view_gen = ?`

// RecordedEdgesAt returns every edge recorded in one of paths on this
// handle's generation, full rows. The empty path names the edges recorded at
// no file (a stub's module membership, say).
func (s *Store) RecordedEdgesAt(paths []string) []*graph.Edge {
	uniq := graph.UniqueRecordingPaths(paths)
	if len(uniq) == 0 {
		return nil
	}
	var out []*graph.Edge
	for i := 0; i < len(uniq); i += lookupChunkSize {
		end := minInt(i+lookupChunkSize, len(uniq))
		chunk := uniq[i:end]
		q := recordedEdgesAtPrefix + inPlaceholders(len(chunk)) + recordedEdgesAtSuffix
		out = append(out, s.queryEdgesSQL(q, append(toAnyArgs(chunk), s.viewGen)...)...)
	}
	return out
}
