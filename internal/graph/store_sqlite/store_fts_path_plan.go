package store_sqlite

import (
	"context"
	"database/sql"
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// These bounds select a query plan, never the returned candidate set. A wider
// path/owner set retains the original unrestricted ranked query.
const symbolPathPointNodes = 128
const symbolPathPlanOwners = 64
const symbolPathPlanRanges = 256

const symbolPathOwnersSQL = `WITH RECURSIVE owners(repo) AS (
 SELECT (SELECT repo_prefix FROM nodes INDEXED BY nodes_by_repo ORDER BY repo_prefix LIMIT 1)
 UNION ALL
 SELECT (SELECT repo_prefix FROM nodes INDEXED BY nodes_by_repo WHERE repo_prefix > owners.repo ORDER BY repo_prefix LIMIT 1)
 FROM owners WHERE owners.repo IS NOT NULL)
SELECT repo FROM owners WHERE repo IS NOT NULL LIMIT ?`

// searchSymbolPathPointPlan discovers a complete small path corpus, then uses
// docid-constrained MATCH with the same corpus-wide BM25. All phases share one
// read snapshot, including owners that differ from the supplied FTS owner.
func (s *Store) searchSymbolPathPointPlan(ctx context.Context, match string, repos, paths []string, limit int) ([]graph.SymbolHit, bool, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, true, err
	}
	defer func() { _ = tx.Rollback() }()
	var indexes int
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_schema WHERE type='index' AND name IN ('nodes_by_repo','nodes_by_file')`).Scan(&indexes); err != nil {
		return nil, true, err
	}
	if indexes != 2 {
		return nil, false, nil
	}
	rows, err := tx.QueryContext(ctx, symbolPathOwnersSQL, symbolPathPlanOwners+1)
	if err != nil {
		return nil, true, err
	}
	var owners []string
	for rows.Next() {
		var owner string
		if err = rows.Scan(&owner); err != nil {
			rows.Close()
			return nil, true, err
		}
		owners = append(owners, owner)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, true, err
	}
	if len(owners) > symbolPathPlanOwners {
		return nil, false, nil
	}
	ranges, ok := symbolPathRawRanges(paths, owners)
	if !ok {
		return nil, false, nil
	}
	var values []string
	var args []any
	for _, r := range ranges {
		values = append(values, `(?,?)`)
		args = append(args, r[0], r[1])
	}
	predicate, pathArgs := symbolPathPredicate(paths)
	query := `WITH ranges(lo,hi) AS (VALUES ` + strings.Join(values, ",") + `),
raw_nodes AS MATERIALIZED (
 SELECT DISTINCT path_node.id, path_node.repo_prefix, path_node.file_path
 FROM ranges CROSS JOIN nodes AS path_node INDEXED BY nodes_by_file
 WHERE path_node.file_path >= ranges.lo AND path_node.file_path < ranges.hi
  AND path_node.view_gen = ? LIMIT ?)
SELECT CASE WHEN (` + predicate + `) THEN ownership.fts_rowid END
FROM raw_nodes AS path_node LEFT JOIN symbol_fts_rowid AS ownership
 ON ownership.view_gen = ? AND ownership.node_id = path_node.id`
	args = append(args, s.viewGen, symbolPathPointNodes+1)
	args = append(args, pathArgs...)
	args = append(args, s.viewGen)
	if len(repos) > 0 {
		query += ` AND ownership.repo_prefix IN ('',?` + strings.Repeat(`,?`, len(repos)-1) + `)`
		for _, r := range repos {
			args = append(args, r)
		}
	}
	rows, err = tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, true, err
	}
	var ids []int64
	var examined int
	for rows.Next() {
		var id sql.NullInt64
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return nil, true, err
		}
		examined++
		if id.Valid {
			ids = append(ids, id.Int64)
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, true, err
	}
	// Count raw candidates before scope/path/FTS filtering. Otherwise a broad
	// path with few allowed documents could traverse an entire foreign corpus.
	if examined > symbolPathPointNodes {
		return nil, false, nil
	}
	if len(ids) == 0 {
		return []graph.SymbolHit{}, true, nil
	}
	values = nil
	args = nil
	for _, id := range ids {
		values = append(values, `(?)`)
		args = append(args, id)
	}
	query = `WITH wanted(id) AS (VALUES ` + strings.Join(values, ",") + `)
SELECT symbol_fts.node_id, bm25(symbol_fts)
FROM wanted CROSS JOIN symbol_fts
WHERE symbol_fts.rowid = wanted.id AND symbol_fts MATCH ?
 AND symbol_fts.rank MATCH 'bm25()'
ORDER BY bm25(symbol_fts), symbol_fts.rowid LIMIT ?`
	args = append(args, match, limit)
	rows, err = tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, true, err
	}
	defer rows.Close()
	var hits []graph.SymbolHit
	for rows.Next() {
		var id string
		var score float64
		if err = rows.Scan(&id, &score); err != nil {
			return nil, true, err
		}
		if id != "" {
			hits = append(hits, graph.SymbolHit{NodeID: id, Score: -score})
		}
	}
	if err = rows.Err(); err != nil {
		return nil, true, err
	}
	return hits, true, ctx.Err()
}

// Raw prefixes are only a superset: the exact repo-relative predicate remains
// authoritative. Every observed node owner is included, independently of FTS
// ownership. Deep spellings broaden to four components instead of enumerating
// exponentially many mixed slash/backslash combinations.
func symbolPathRawRanges(paths, owners []string) ([][2]string, bool) {
	seen := make(map[string]bool)
	var ranges [][2]string
	owners = append([]string{""}, owners...)
	for _, path := range paths {
		if path == "" {
			return nil, false
		}
		for _, owner := range owners {
			// file_path is normalized, repo_prefix is not; a backslash-bearing owner
			// cannot be stripped and is covered by the unqualified path ranges.
			if strings.Contains(owner, `\`) {
				continue
			}
			full := path
			if owner != "" {
				full = owner + "/" + path
			}
			parts := strings.Split(full, "/")
			if len(parts) > 4 {
				parts = parts[:4]
			}
			prefixes := []string{parts[0]}
			for _, part := range parts[1:] {
				var next []string
				for _, p := range prefixes {
					next = append(next, p+"/"+part, p+`\`+part)
				}
				prefixes = next
			}
			for _, p := range prefixes {
				if seen[p] {
					continue
				}
				seen[p] = true
				b := []byte(p)
				for len(b) > 0 && b[len(b)-1] == 255 {
					b = b[:len(b)-1]
				}
				if len(b) == 0 {
					return nil, false
				}
				b[len(b)-1]++
				ranges = append(ranges, [2]string{p, string(b)})
				if len(ranges) > symbolPathPlanRanges {
					return nil, false
				}
			}
		}
	}
	return ranges, len(ranges) > 0
}
