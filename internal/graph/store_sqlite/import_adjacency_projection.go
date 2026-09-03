package store_sqlite

import (
	"encoding/json"
	"path/filepath"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graphpath"
)

// importAdjacencyProjectionSQL has two deliberately narrow branches. The
// first is the hot path: edges_by_file selects only import rows for requested
// caller files and the node primary key excludes orphan/misattributed edges,
// matching Store adjacency semantics. The second emits only a sentinel when
// an import owned by a requested file has blank or mismatched edge provenance;
// the resolver then falls back to the ordinary node/adjacency path.
const importAdjacencyProjectionSQL = `
WITH requested(file_path) AS (
    SELECT CAST(value AS TEXT) FROM json_each(?)
)
SELECT e.file_path, e.to_id, 0 AS malformed
FROM requested AS r
JOIN edges AS e INDEXED BY edges_by_file
  ON e.file_path = r.file_path AND e.kind = ? AND e.view_gen = ?
JOIN nodes AS source
  ON source.id = e.from_id AND source.file_path = e.file_path AND source.view_gen = e.view_gen
UNION ALL
SELECT r.file_path, '', 1 AS malformed
FROM requested AS r
WHERE EXISTS (
    SELECT 1
    FROM nodes AS source INDEXED BY nodes_by_file
    JOIN edges AS e INDEXED BY edges_by_from
      ON e.from_id = source.id AND e.view_gen = source.view_gen
    WHERE source.file_path = r.file_path
      AND e.kind = ?
      AND e.file_path <> source.file_path
      AND source.view_gen = ?
)`

var _ graph.ImportAdjacencyProjector = (*Store)(nil)

// ProjectImportAdjacency projects only direct import targets for a bounded
// caller-file frontier. It never decodes Node or Edge payloads. complete=false
// is an explicit request for the caller to use normal Store adjacency when a
// path or persisted import provenance is not canonical.
func (s *Store) ProjectImportAdjacency(filePaths []string) (map[string][]string, bool) {
	if len(filePaths) == 0 {
		return nil, true
	}
	seen := make(map[string]struct{}, len(filePaths))
	paths := make([]string, 0, len(filePaths))
	for _, path := range filePaths {
		if path == "" || path == "." {
			return nil, false
		}
		// Indexed paths keep '/' after the repo prefix with the rest
		// OS-native, so on Windows filepath.Clean rewrites separators on
		// every stored path. A separator-only difference is not a
		// canonicality violation — only a structural one (traversal,
		// duplicate separators, a trailing "/." segment) rejects the
		// request. Comparing the normalized forms is the whole test: Clean
		// never inserts characters, so equal normalized forms mean the two
		// spellings differ only in separators.
		if cleaned := filepath.Clean(path); graphpath.Norm(cleaned) != graphpath.Norm(path) {
			return nil, false
		}
		if _, duplicate := seen[path]; duplicate {
			continue
		}
		seen[path] = struct{}{}
		paths = append(paths, path)
	}
	if len(paths) == 0 {
		return nil, true
	}

	out := make(map[string][]string, len(paths))
	for start := 0; start < len(paths); start += lookupChunkSize {
		end := minInt(start+lookupChunkSize, len(paths))
		payload, err := json.Marshal(paths[start:end])
		if err != nil {
			return nil, false
		}
		rows, err := s.db.Query(importAdjacencyProjectionSQL,
			string(payload), string(graph.EdgeImports), s.viewGen,
			string(graph.EdgeImports), s.viewGen)
		if err != nil {
			return nil, false
		}
		complete := true
		for rows.Next() {
			var filePath, targetID string
			var malformed int
			if err := rows.Scan(&filePath, &targetID, &malformed); err != nil {
				complete = false
				break
			}
			if malformed != 0 {
				complete = false
				break
			}
			out[filePath] = append(out[filePath], targetID)
		}
		if err := rows.Err(); err != nil {
			complete = false
		}
		_ = rows.Close()
		if !complete {
			return nil, false
		}
	}
	return out, true
}
