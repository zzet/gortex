package store_sqlite

import (
	"database/sql"
	"sort"
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// Compile-time assertion: *Store serves the endpoint-only projections.
var _ graph.EdgeEndpointReader = (*Store)(nil)

// The endpoint projections answer the questions an affected-closure walk asks
// of the layer below — which names does this file already account for, and
// does this source have adjacency in a file the generation does not claim —
// without decoding the full rows the generic readers return. Each statement is
// the full-row reader's statement with a narrower column list, so it reads
// exactly the rows the full-row reader would on the same handle.

// edgeEndpointsRecordedAtSQL reaches the rows through edges_by_file
// (file_path, kind). The generation is a residual filter: the index carries no
// generation, and the `+` keeps the planner from preferring the generation
// index, which would scan the whole generation to find a handful of files.
const edgeEndpointsRecordedAtPrefix = `SELECT from_id, to_id, kind, file_path FROM edges WHERE file_path IN (`
const edgeEndpointsRecordedAtSuffix = `) AND +view_gen = ?`

// edgeEndpointsRecordedAtPinnedPrefix is the same read through
// edges_by_file_generation (file_path, view_gen) once the lazy builder has
// built it: a seek per (file, generation), so a file's rows in other
// generations are never visited. recordedAtPinnedSuffix closes both pinned
// forms.
const edgeEndpointsRecordedAtPinnedPrefix = `SELECT from_id, to_id, kind, file_path FROM edges INDEXED BY ` + edgesByFileGenerationIndexName + ` WHERE file_path IN (`
const recordedAtPinnedSuffix = `) AND view_gen = ?`

// queryByFileGeneration runs a pinned by-file read when
// edges_by_file_generation is present. It reports false (and the caller runs
// the legacy edges_by_file form) when the index is absent or was dropped
// between the presence probe and the statement.
func (s *Store) queryByFileGeneration(q string, args []any) (*sql.Rows, bool) {
	if !s.fileGenerationIndexPresent() {
		return nil, false
	}
	rows, err := s.db.Query(q, args...)
	if err != nil {
		if isNoSuchIndexErr(err) {
			s.forgetFileGenerationIndex()
			return nil, false
		}
		panicOnFatal(err)
		return nil, false
	}
	return rows, true
}

// edgeEndpointsFromSQL seeks edges_by_from (view_gen, from_id, kind); with a
// kind list the whole predicate is served by the index key.
const edgeEndpointsFromPrefix = `SELECT from_id, to_id, kind, file_path FROM edges WHERE view_gen = ? AND from_id IN (`

// nodeNamesByIDsPrefix probes the (id, view_gen) primary key.
const nodeNamesByIDsPrefix = `SELECT id, name FROM nodes WHERE id IN (`

// structuralTargetInvalidSQL is graph.StructuralEdgeTargetInvalid spelled in
// SQL, so a projection that never materialises the target (OutEdgePathsFrom)
// still drops exactly the rows the full-row scanners drop. The kind list is
// built from the same constants; TestOutEdgePathsFromAppliesTheStructuralReadFilter
// pins the two spellings together.
var structuralTargetInvalidSQL = `(kind IN ('` + strings.Join([]string{
	string(graph.EdgeImplements), string(graph.EdgeExtends), string(graph.EdgeOverrides),
	string(graph.EdgeInstantiates), string(graph.EdgeMemberOf),
}, `','`) + `') AND (instr(to_id, '#param:') > 0 OR instr(to_id, '#local:') > 0))`

// outEdgePathsFromPrefix returns each source's distinct recording files. The
// seek is edges_by_from (view_gen, from_id); file_path and to_id come from the
// row. DISTINCT keeps a hub source's thousands of edges in one file to one
// row on the wire.
var outEdgePathsFromPrefix = `SELECT DISTINCT from_id, file_path FROM edges WHERE view_gen = ? AND NOT ` +
	structuralTargetInvalidSQL + ` AND from_id IN (`

// EdgeEndpointsRecordedAt returns the endpoints of every edge recorded in one
// of paths on this handle's generation. It reads the rows
// GetOutEdgesByNodeIDs and GetInEdgesByNodeIDs return for the files' own
// symbols when filtered to FilePath ∈ paths, and additionally any edge
// recorded there whose endpoints both live elsewhere.
func (s *Store) EdgeEndpointsRecordedAt(paths []string) []graph.EdgeEndpointRow {
	uniq := dedupeNonEmpty(paths)
	if len(uniq) == 0 {
		return nil
	}
	var out []graph.EdgeEndpointRow
	for i := 0; i < len(uniq); i += lookupChunkSize {
		end := minInt(i+lookupChunkSize, len(uniq))
		chunk := uniq[i:end]
		args := append(toAnyArgs(chunk), s.viewGen)
		if rows, ok := s.queryByFileGeneration(edgeEndpointsRecordedAtPinnedPrefix+inPlaceholders(len(chunk))+recordedAtPinnedSuffix, args); ok {
			out = s.appendEdgeEndpointRows(out, rows)
			continue
		}
		q := edgeEndpointsRecordedAtPrefix + inPlaceholders(len(chunk)) + edgeEndpointsRecordedAtSuffix
		out = s.appendEdgeEndpoints(out, q, args...)
	}
	return out
}

// EdgeEndpointsFrom returns the endpoints of the out-edges of ids on this
// handle's generation, restricted to kinds when kinds holds any non-empty
// kind. A kinds list of only empty kinds matches nothing.
func (s *Store) EdgeEndpointsFrom(ids []string, kinds []graph.EdgeKind) []graph.EdgeEndpointRow {
	uniq := dedupeNonEmpty(ids)
	if len(uniq) == 0 {
		return nil
	}
	_, kindArgs := aggDedupeEdgeKinds(kinds)
	if len(kinds) > 0 && len(kindArgs) == 0 {
		return nil
	}
	var out []graph.EdgeEndpointRow
	for i := 0; i < len(uniq); i += lookupChunkSize {
		end := minInt(i+lookupChunkSize, len(uniq))
		chunk := uniq[i:end]
		q := edgeEndpointsFromPrefix + inPlaceholders(len(chunk)) + `)`
		args := make([]any, 0, 1+len(chunk)+len(kindArgs))
		args = append(args, s.viewGen)
		args = append(args, toAnyArgs(chunk)...)
		if len(kindArgs) > 0 {
			q += ` AND kind IN (` + inPlaceholders(len(kindArgs)) + `)`
			args = append(args, kindArgs...)
		}
		out = s.appendEdgeEndpoints(out, q, args...)
	}
	return out
}

// NodeNamesByIDs returns id -> name for every requested id this handle's
// generation carries. A node with an empty name is present with "".
func (s *Store) NodeNamesByIDs(ids []string) map[string]string {
	uniq := dedupeNonEmpty(ids)
	if len(uniq) == 0 {
		return nil
	}
	out := make(map[string]string, len(uniq))
	for i := 0; i < len(uniq); i += lookupChunkSize {
		end := minInt(i+lookupChunkSize, len(uniq))
		chunk := uniq[i:end]
		q := nodeNamesByIDsPrefix + inPlaceholders(len(chunk)) + `) AND view_gen = ?`
		rows, err := s.db.Query(q, append(toAnyArgs(chunk), s.viewGen)...)
		if err != nil {
			panicOnFatal(err)
			return out
		}
		for rows.Next() {
			var id, name string
			if err := rows.Scan(&id, &name); err != nil {
				_ = rows.Close()
				panicOnFatal(err)
				return out
			}
			out[id] = name
		}
		err = rows.Err()
		_ = rows.Close()
		if err != nil {
			panicOnFatal(err)
			return out
		}
	}
	return out
}

// OutEdgePathsFrom returns, per source id, the sorted distinct files its
// out-edges on this handle's generation are recorded in. It is the
// GetOutEdgesByNodeIDs row set reduced to (From, FilePath).
func (s *Store) OutEdgePathsFrom(ids []string) map[string][]string {
	uniq := dedupeNonEmpty(ids)
	if len(uniq) == 0 {
		return nil
	}
	out := make(map[string][]string)
	for i := 0; i < len(uniq); i += lookupChunkSize {
		end := minInt(i+lookupChunkSize, len(uniq))
		chunk := uniq[i:end]
		q := outEdgePathsFromPrefix + inPlaceholders(len(chunk)) + `)`
		args := make([]any, 0, 1+len(chunk))
		args = append(args, s.viewGen)
		args = append(args, toAnyArgs(chunk)...)
		rows, err := s.db.Query(q, args...)
		if err != nil {
			panicOnFatal(err)
			return out
		}
		for rows.Next() {
			var from, path string
			if err := rows.Scan(&from, &path); err != nil {
				_ = rows.Close()
				panicOnFatal(err)
				return out
			}
			out[from] = append(out[from], path)
		}
		err = rows.Err()
		_ = rows.Close()
		if err != nil {
			panicOnFatal(err)
			return out
		}
	}
	for from := range out {
		sort.Strings(out[from])
	}
	return out
}

// appendEdgeEndpoints runs one endpoint projection and applies the same
// structural read filter the full-row scanners apply.
func (s *Store) appendEdgeEndpoints(out []graph.EdgeEndpointRow, q string, args ...any) []graph.EdgeEndpointRow {
	rows, err := s.db.Query(q, args...)
	if err != nil {
		panicOnFatal(err)
		return out
	}
	return s.appendEdgeEndpointRows(out, rows)
}

// appendEdgeEndpointRows scans an endpoint projection's rows and closes them.
func (s *Store) appendEdgeEndpointRows(out []graph.EdgeEndpointRow, rows *sql.Rows) []graph.EdgeEndpointRow {
	defer rows.Close()
	for rows.Next() {
		var e graph.EdgeEndpointRow
		var kind string
		if err := rows.Scan(&e.From, &e.To, &kind, &e.FilePath); err != nil {
			panicOnFatal(err)
			return out
		}
		e.Kind = graph.EdgeKind(kind)
		if graph.StructuralEdgeTargetInvalid(e.Kind, e.To) {
			s.noteStructuralReadDrop(graph.StructuralPathSQLiteLightRead, &graph.Edge{
				From: e.From, To: e.To, Kind: e.Kind, FilePath: e.FilePath,
			})
			continue
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		panicOnFatal(err)
	}
	return out
}
