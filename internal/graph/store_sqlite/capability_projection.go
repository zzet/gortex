package store_sqlite

import (
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// edgeGenerationHighWaterSQL is one generation's highest edge id. With the
// dense edges_by_generation (view_gen, id) index present it is pinned there
// (INDEXED BY): a single seek to the end of the generation's range. Unpinned,
// a store whose sqlite_stat1 carries a row for edges_by_generation but none
// for edges_by_to (what the open-time statistics repair leaves) answers the
// MAX by a covering scan of edges_by_to's view_gen prefix — every edge of the
// generation, seconds on a large store, on every scoped edge projection. The
// index is optional (a store may lack it), so without it the unpinned form
// keeps working.
func edgeGenerationHighWaterSQL(indexed bool) string {
	if indexed {
		return `SELECT COALESCE(MAX(id), 0) FROM edges INDEXED BY edges_by_generation WHERE view_gen = ?`
	}
	return `SELECT COALESCE(MAX(id), 0) FROM edges WHERE view_gen = ?`
}

// edgeGenerationIndexPresent reports whether edges_by_generation exists. It
// is probed per call (one catalog lookup) because the index can be dropped
// and recreated under an open store.
func (s *Store) edgeGenerationIndexPresent() bool {
	var present bool
	if err := s.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type = 'index' AND name = ?)`,
		edgesByGenerationIndexName).Scan(&present); err != nil {
		return false
	}
	return present
}

// edgeGenerationHighWater returns the current generation's highest edge id.
func (s *Store) edgeGenerationHighWater() (int64, error) {
	var maxID int64
	err := s.db.QueryRow(edgeGenerationHighWaterSQL(s.edgeGenerationIndexPresent()), s.viewGen).Scan(&maxID)
	return maxID, err
}

// capabilityProjectionHighWaterQuery is edgeGenerationHighWaterSQL; a derived
// generation adds the historical positive bound.
func capabilityProjectionHighWaterQuery(viewGen int64, indexed bool) string {
	query := edgeGenerationHighWaterSQL(indexed)
	if viewGen > 0 {
		return query + ` AND view_gen > 0`
	}
	return query
}

func capabilityProjectionPageQuery(viewGen int64, allRepos bool) string {
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
	if viewGen > 0 {
		// A sparse generation can sit behind millions of unrelated corpus
		// rows in this shared table. Do not force a global row-id traversal.
		// The explicit positive predicate proves eligibility for the existing
		// partial edges_by_generation index; equality to a parameter does not.
		// This is not INDEXED BY: an optional index being absent remains safe.
		query = strings.Replace(query, "edges AS e NOT INDEXED", "edges AS e", 1)
		query = strings.Replace(query, "e.view_gen = ?", "e.view_gen = ? AND e.view_gen > 0", 1)
	}
	return query
}

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
	if err := s.db.QueryRow(capabilityProjectionHighWaterQuery(s.viewGen, s.edgeGenerationIndexPresent()), s.viewGen).Scan(&highWater); err != nil {
		panicOnFatal(err)
		return
	}
	if highWater == 0 {
		return
	}

	stmt, err := s.db.Prepare(capabilityProjectionPageQuery(s.viewGen, allRepos))
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
