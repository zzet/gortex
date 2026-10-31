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
// The named repositories are probed before the empty prefix: in a
// one-repository scope an owned source costs one probe, and only unowned or
// out-of-scope sources pay for the empty prefix as well.
func (s *Store) GetInEdgesByNodeIDsFromRepos(ids, repos []string) (map[string][]*graph.Edge, bool, error) {
	uniq := dedupeNonEmpty(ids)
	if len(uniq) == 0 || len(repos) == 0 {
		return nil, true, nil
	}
	named := make([]string, 0, len(repos))
	unowned := false
	for _, repo := range repos {
		if repo == "" {
			unowned = true
			continue
		}
		named = append(named, repo)
	}
	allowed, err := json.Marshal(named)
	if err != nil {
		return nil, false, err
	}
	ctx := s.readContext()
	out := make(map[string][]*graph.Edge, len(uniq))
	for i := 0; i < len(uniq); i += lookupChunkSize {
		chunk := uniq[i:minInt(i+lookupChunkSize, len(uniq))]
		q := repoScopedInEdgesSQL(len(chunk), unowned)
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
// list of named repository prefixes; unowned also admits sources with the
// empty prefix. The source probes are covered by nodes_by_repo, whose key
// carries the node identity, so no wide node row is read. The empty prefix
// sorts ahead of every name, so it gets its own probe, tried last.
func repoScopedInEdgesSQL(count int, unowned bool) string {
	q := `SELECT ` + lookupQualifiedEdgeCols + ` FROM edges e INDEXED BY edges_by_to
WHERE e.to_id IN (` + inPlaceholders(count) + `) AND e.view_gen = ?
AND (EXISTS (SELECT 1 FROM nodes n INDEXED BY nodes_by_repo
	WHERE n.repo_prefix IN (SELECT CAST(value AS TEXT) FROM json_each(?)) AND n.view_gen = e.view_gen AND n.id = e.from_id)`
	if unowned {
		q += `
	OR EXISTS (SELECT 1 FROM nodes n INDEXED BY nodes_by_repo
	WHERE n.repo_prefix = '' AND n.view_gen = e.view_gen AND n.id = e.from_id)`
	}
	return q + `)`
}
