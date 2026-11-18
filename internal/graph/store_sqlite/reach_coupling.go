package store_sqlite

import (
	"context"
)

// repoCouplingPairsSQL lists the distinct (source, target) repository pairs an
// edge joins. An empty prefix stands for nodes no repository owns; such a
// target counts only when it has edges of its own, since only then can a path
// run through it from one repository into another. Edges whose endpoints share
// a plain leading "<prefix>/" segment belong to one repository and are skipped
// before the node joins. That is most of the table, so one sequential scan
// beats walking an index and fetching every row it names.
const repoCouplingPairsSQL = `
SELECT DISTINCT nf.repo_prefix, nt.repo_prefix
FROM edges e NOT INDEXED
JOIN nodes nf ON nf.id = e.from_id AND nf.view_gen = e.view_gen
JOIN nodes nt ON nt.id = e.to_id AND nt.view_gen = e.view_gen
WHERE e.view_gen = ?
  AND NOT (instr(e.from_id, '/') > 1
       AND instr(substr(e.from_id, 1, instr(e.from_id, '/')), ':') = 0
       AND substr(e.to_id, 1, instr(e.from_id, '/')) = substr(e.from_id, 1, instr(e.from_id, '/')))
  AND nf.repo_prefix <> nt.repo_prefix
  AND (nt.repo_prefix <> '' OR EXISTS (
       SELECT 1 FROM edges o WHERE o.view_gen = e.view_gen AND o.from_id = e.to_id))`

// repoCouplingPairsForFilesSQL is repoCouplingPairsSQL over the edges sited in
// a list of files or incident to their nodes. CROSS JOIN keeps that frontier
// the driving loop, as in the cross-repository candidate query. An unowned
// node with an edge into the frontier may have just gained its first one, so
// the repositories with edges into it are listed as coupled with it too.
const repoCouplingPairsForFilesSQL = `
WITH frontier(id) AS (
  SELECT e.id FROM edges e
  WHERE e.file_path IN (SELECT CAST(value AS TEXT) FROM json_each(?))
  UNION
  SELECT e.id FROM nodes n JOIN edges e ON e.from_id = n.id AND e.view_gen = n.view_gen
  WHERE n.file_path IN (SELECT CAST(value AS TEXT) FROM json_each(?))
  UNION
  SELECT e.id FROM nodes n JOIN edges e ON e.to_id = n.id AND e.view_gen = n.view_gen
  WHERE n.file_path IN (SELECT CAST(value AS TEXT) FROM json_each(?))
),
joined(from_id, to_id, from_repo, to_repo) AS (
  SELECT e.from_id, e.to_id, nf.repo_prefix, nt.repo_prefix
  FROM frontier f CROSS JOIN edges e ON e.id = f.id
  JOIN nodes nf ON nf.id = e.from_id AND nf.view_gen = e.view_gen
  JOIN nodes nt ON nt.id = e.to_id AND nt.view_gen = e.view_gen
  WHERE e.view_gen = ? AND nf.repo_prefix <> nt.repo_prefix
)
SELECT from_repo, to_repo FROM joined j
WHERE j.to_repo <> '' OR EXISTS (SELECT 1 FROM edges o WHERE o.view_gen = ? AND o.from_id = j.to_id)
UNION
SELECT ni.repo_prefix, ''
FROM joined j
JOIN edges i ON i.to_id = j.from_id AND i.view_gen = ?
JOIN nodes ni ON ni.id = i.from_id AND ni.view_gen = i.view_gen
WHERE j.from_repo = '' AND ni.repo_prefix <> ''`

// RepoCouplingPairs reports which repositories the graph's edges join, so the
// reach index can tell which repositories a mutation of one may affect. It
// reads every edge of the handle's generation.
func (s *Store) RepoCouplingPairs(ctx context.Context) ([][2]string, error) {
	return s.repoCouplingPairs(ctx, repoCouplingPairsSQL, s.viewGen)
}

// RepoCouplingPairsForFiles is RepoCouplingPairs for the edges a mutation of
// the given files can have added or rebound.
func (s *Store) RepoCouplingPairsForFiles(ctx context.Context, files []string) ([][2]string, error) {
	scope, ok := projectionJSON(files)
	if !ok {
		return nil, nil
	}
	return s.repoCouplingPairs(ctx, repoCouplingPairsForFilesSQL, scope, scope, scope, s.viewGen, s.viewGen, s.viewGen)
}

func (s *Store) repoCouplingPairs(ctx context.Context, query string, args ...any) ([][2]string, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var pairs [][2]string
	for rows.Next() {
		var pair [2]string
		if err := rows.Scan(&pair[0], &pair[1]); err != nil {
			return nil, err
		}
		pairs = append(pairs, pair)
	}
	return pairs, rows.Err()
}

// NodeReadsDetached reports that reads stay consistent without ResolveMutex:
// each one runs on its own SQLite snapshot and returns decoded copies, never
// the rows a resolver pass is rewriting.
func (s *Store) NodeReadsDetached() bool {
	return true
}
