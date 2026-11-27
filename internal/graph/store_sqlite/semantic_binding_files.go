package store_sqlite

import (
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

var _ graph.SemanticBindingFileReader = (*Store)(nil)

func (s *Store) SemanticBindingTypesForFiles(repo string, files []string) (map[graph.SemanticBindingSite]string, error) {
	out := make(map[graph.SemanticBindingSite]string)
	for start := 0; start < len(files); start += semanticBindingChunk {
		chunk := files[start:min(start+semanticBindingChunk, len(files))]
		args := []any{s.viewGen, repo}
		for _, file := range chunk {
			args = append(args, file)
		}
		query := `SELECT file_path, line, name, type_name FROM semantic_binding_types
WHERE view_gen = ? AND repo_prefix = ? AND file_path IN (` + strings.TrimSuffix(strings.Repeat("?,", len(chunk)), ",") + `)`
		rows, err := s.db.Query(query, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			site := graph.SemanticBindingSite{RepoPrefix: repo}
			var value string
			if err := rows.Scan(&site.FilePath, &site.Line, &site.Name, &value); err != nil {
				_ = rows.Close()
				return nil, err
			}
			out[site] = value
		}
		err = rows.Err()
		_ = rows.Close()
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}
