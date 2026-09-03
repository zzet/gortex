package store_sqlite

import "github.com/zzet/gortex/internal/graph"

// ScanRepoCapabilityEdges reads only source repository and logical identity
// columns needed by capability synthesis. nil repoPrefixes scans all sources;
// a non-nil empty slice scans none. The id keyset freezes the generation and
// bounds every allocation; each cursor is closed before yield runs so the
// callback may safely re-enter the store.
func (s *Store) ScanRepoCapabilityEdges(
	repoPrefixes []string,
	pageSize int,
	yield func([]graph.RepoCapabilityEdge) bool,
) {
	if yield == nil || (repoPrefixes != nil && len(repoPrefixes) == 0) {
		return
	}
	allRepos := repoPrefixes == nil
	var reposJSON string
	if !allRepos {
		var ok bool
		reposJSON, ok = projectionJSON(repoPrefixes)
		if !ok {
			return
		}
	}
	if pageSize <= 0 {
		pageSize = 4096
	}

	var highWater int64
	if err := s.db.QueryRow(`SELECT COALESCE(MAX(id), 0) FROM edges WHERE view_gen = ?`, s.viewGen).Scan(&highWater); err != nil {
		panicOnFatal(err)
		return
	}
	if highWater == 0 {
		return
	}

	const scopedQuery = `
WITH requested_repos(repo_prefix) AS (
    SELECT CAST(value AS TEXT) FROM json_each(?)
)
SELECT e.id, n.repo_prefix,
       e.from_id, e.to_id, e.kind, e.file_path, e.line
FROM edges AS e NOT INDEXED
JOIN nodes AS n ON n.id = e.from_id AND n.view_gen = e.view_gen
JOIN requested_repos AS r ON r.repo_prefix = n.repo_prefix
LEFT JOIN nodes AS target ON target.id = e.to_id AND target.view_gen = e.view_gen
WHERE e.id > ? AND e.id <= ? AND e.view_gen = ?
  AND e.kind IN (?, ?, ?, ?)
  AND (e.kind NOT IN (?, ?) OR target.kind = ?)
ORDER BY e.id
LIMIT ?`
	const allQuery = `
SELECT e.id, n.repo_prefix,
       e.from_id, e.to_id, e.kind, e.file_path, e.line
FROM edges AS e NOT INDEXED
JOIN nodes AS n ON n.id = e.from_id AND n.view_gen = e.view_gen
LEFT JOIN nodes AS target ON target.id = e.to_id AND target.view_gen = e.view_gen
WHERE e.id > ? AND e.id <= ? AND e.view_gen = ?
  AND e.kind IN (?, ?, ?, ?)
  AND (e.kind NOT IN (?, ?) OR target.kind = ?)
ORDER BY e.id
LIMIT ?`
	query := scopedQuery
	if allRepos {
		query = allQuery
	}
	stmt, err := s.db.Prepare(query)
	if err != nil {
		panicOnFatal(err)
		return
	}
	defer stmt.Close()

	lastID := int64(0)
	for lastID < highWater {
		args := make([]any, 0, 13)
		if !allRepos {
			args = append(args, reposJSON)
		}
		args = append(args,
			lastID, highWater, s.viewGen,
			string(graph.EdgeReadsConfig), string(graph.EdgeReads),
			string(graph.EdgeWrites), string(graph.EdgeCalls),
			string(graph.EdgeReads), string(graph.EdgeWrites), string(graph.KindField),
			pageSize,
		)
		rows, err := stmt.Query(args...)
		if err != nil {
			panicOnFatal(err)
			return
		}

		page := make([]graph.RepoCapabilityEdge, 0, pageSize)
		for rows.Next() {
			var (
				edgeID int64
				row    graph.RepoCapabilityEdge
			)
			if err := rows.Scan(
				&edgeID,
				&row.RepoPrefix,
				&row.Identity.From,
				&row.Identity.To,
				&row.Identity.Kind,
				&row.Identity.FilePath,
				&row.Identity.Line,
			); err != nil {
				_ = rows.Close()
				panicOnFatal(err)
				return
			}
			lastID = edgeID
			page = append(page, row)
		}
		rowsErr := rows.Err()
		closeErr := rows.Close()
		if rowsErr != nil {
			panicOnFatal(rowsErr)
			return
		}
		if closeErr != nil {
			panicOnFatal(closeErr)
			return
		}
		if len(page) == 0 {
			return
		}
		if !yield(page) {
			return
		}
		if len(page) < pageSize {
			return
		}
	}
}

var _ graph.RepoCapabilityEdgeScanner = (*Store)(nil)
