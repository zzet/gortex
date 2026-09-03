package store_sqlite

import "github.com/zzet/gortex/internal/graph"

var _ graph.RepoLanguageNodeSummaryReader = (*Store)(nil)

// GetRepoNodeSummariesByLanguage pushes both repository and language filters
// into SQLite and scans only identity/location columns. In particular, neither
// the opaque Meta blob nor promoted docs/signatures cross the driver boundary.
func (s *Store) GetRepoNodeSummariesByLanguage(repoPrefix, language string) []*graph.Node {
	if language == "" {
		return nil
	}
	rows, err := s.db.Query(
		`SELECT `+lookupNodeSummaryCols+` FROM nodes WHERE repo_prefix = ? AND language = ? AND view_gen = ?`,
		repoPrefix, language, s.viewGen,
	)
	if err != nil {
		panicOnFatal(err)
		return nil
	}
	defer rows.Close()

	var out []*graph.Node
	for rows.Next() {
		node, err := scanNodeSummary(rows)
		if err != nil {
			panicOnFatal(err)
			return out
		}
		out = append(out, node)
	}
	if err := rows.Err(); err != nil {
		panicOnFatal(err)
	}
	return out
}
