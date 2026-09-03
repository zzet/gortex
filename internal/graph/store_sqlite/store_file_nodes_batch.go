package store_sqlite

import (
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// GetFileNodesByPaths returns nodes for a changed-file frontier in bounded IN
// queries. The result is grouped by the promoted file_path column, so SQLite
// can satisfy the request from idx_nodes_file without decoding unrelated rows.
func (s *Store) GetFileNodesByPaths(filePaths []string) map[string][]*graph.Node {
	if len(filePaths) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(filePaths))
	paths := make([]string, 0, len(filePaths))
	for _, path := range filePaths {
		if path == "" {
			continue
		}
		if _, duplicate := seen[path]; duplicate {
			continue
		}
		seen[path] = struct{}{}
		paths = append(paths, path)
	}
	if len(paths) == 0 {
		return nil
	}

	out := make(map[string][]*graph.Node, len(paths))
	for start := 0; start < len(paths); start += lookupChunkSize {
		end := minInt(start+lookupChunkSize, len(paths))
		chunk := paths[start:end]
		placeholders := strings.Repeat(",?", len(chunk))[1:]
		query := `SELECT ` + lookupNodeCols + ` FROM nodes WHERE file_path IN (` + placeholders + `) AND view_gen = ?`
		args := make([]any, 0, len(chunk)+1)
		for _, path := range chunk {
			args = append(args, path)
		}
		args = append(args, s.viewGen)
		for _, node := range s.queryNodesSQL(query, args...) {
			if node != nil {
				out[node.FilePath] = append(out[node.FilePath], node)
			}
		}
	}
	return out
}
