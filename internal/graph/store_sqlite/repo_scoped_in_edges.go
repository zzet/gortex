package store_sqlite

import (
	"encoding/json"

	"github.com/zzet/gortex/internal/graph"
)

var _ graph.RepoScopedInEdgeReader = (*Store)(nil)

// GetInEdgesByNodeIDsFromRepos implements graph.RepoScopedInEdgeReader. It
// walks the same incoming index as GetInEdgesByNodeIDs and probes each source
// through the repository index, so the rows other repositories park on a
// shared target are skipped inside SQLite rather than decoded and returned.
func (s *Store) GetInEdgesByNodeIDsFromRepos(ids, repos []string) (map[string][]*graph.Edge, bool, error) {
	uniq := dedupeNonEmpty(ids)
	if len(uniq) == 0 || len(repos) == 0 {
		return nil, true, nil
	}
	allowed, err := json.Marshal(repos)
	if err != nil {
		return nil, false, err
	}
	ctx := s.readContext()
	out := make(map[string][]*graph.Edge, len(uniq))
	for i := 0; i < len(uniq); i += lookupChunkSize {
		chunk := uniq[i:minInt(i+lookupChunkSize, len(uniq))]
		q := repoScopedInEdgesSQL(len(chunk))
		rows, err := outlineRows(ctx, s.db, outlineQuery{sql: q, args: append(toAnyArgs(chunk), s.viewGen, string(allowed))})
		if err != nil {
			return nil, false, err
		}
		for rows.Next() {
			edge, err := s.scanEdgeCursor(rows)
			if err != nil {
				_ = rows.Close()
				return nil, false, err
			}
			if edge != nil {
				out[edge.To] = append(out[edge.To], edge)
			}
		}
		err = rows.Err()
		_ = rows.Close()
		if err != nil {
			return nil, false, err
		}
	}
	return out, true, nil
}

// repoScopedInEdgesSQL reads the incoming edges of count targets, then a JSON
// list of repository prefixes. The source probe is covered by nodes_by_repo,
// whose key carries the node identity, so no wide node row is read.
func repoScopedInEdgesSQL(count int) string {
	return `SELECT ` + lookupQualifiedEdgeCols + ` FROM edges e INDEXED BY edges_by_to
WHERE e.to_id IN (` + inPlaceholders(count) + `) AND e.view_gen = ?
AND EXISTS (SELECT 1 FROM nodes n INDEXED BY nodes_by_repo
	WHERE n.repo_prefix IN (SELECT CAST(value AS TEXT) FROM json_each(?)) AND n.view_gen = e.view_gen AND n.id = e.from_id)`
}
