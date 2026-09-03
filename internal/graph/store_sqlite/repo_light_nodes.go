package store_sqlite

import "github.com/zzet/gortex/internal/graph"

// RepoNodesLight projects only fields consumed by framework candidate
// preflights. The repo join is driven by nodes_by_repo and never decodes Meta,
// docs, signatures, or semantic sidecars.
func (s *Store) RepoNodesLight(repoPrefixes []string) []*graph.Node {
	reposJSON, ok := projectionJSON(repoPrefixes)
	if !ok {
		return nil
	}
	rows, err := s.db.Query(`
WITH requested(repo_prefix) AS (
    SELECT CAST(value AS TEXT) FROM json_each(?)
)
SELECT n.id, n.kind, n.name, n.file_path, n.language, n.repo_prefix
FROM requested AS r
JOIN nodes AS n ON n.repo_prefix = r.repo_prefix
WHERE n.view_gen = ?
ORDER BY n.id`, reposJSON, s.viewGen)
	if err != nil {
		panicOnFatal(err)
		return nil
	}
	defer rows.Close()
	var out []*graph.Node
	for rows.Next() {
		node := &graph.Node{}
		if err := rows.Scan(&node.ID, &node.Kind, &node.Name, &node.FilePath, &node.Language, &node.RepoPrefix); err != nil {
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

var _ graph.RepoLightNodeReader = (*Store)(nil)
