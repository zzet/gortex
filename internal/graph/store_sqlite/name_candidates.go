package store_sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"sort"

	"github.com/zzet/gortex/internal/graph"
)

const nameCandidateCols = `id, name, kind, file_path, repo_prefix, workspace_id, project_id`

// nameCandidateGenerationRepos lists the repository prefixes holding named
// rows of the generation, one index seek per prefix, so an unscoped read still
// seeks each repository's range by name length.
const nameCandidateGenerationRepos = `WITH RECURSIVE candidate_repos(repo) AS (
  SELECT (SELECT min(repo_prefix) FROM nodes INDEXED BY nodes_name_candidates WHERE name <> '' AND view_gen = ?)
  UNION ALL
  SELECT (SELECT min(repo_prefix) FROM nodes INDEXED BY nodes_name_candidates WHERE name <> '' AND view_gen = ? AND repo_prefix > candidate_repos.repo)
  FROM candidate_repos WHERE candidate_repos.repo IS NOT NULL
) `

// sqliteLikePatternLimit is SQLite's default SQLITE_MAX_LIKE_PATTERN_LENGTH:
// LIKE refuses a longer pattern on the first row it tests.
const sqliteLikePatternLimit = 50000

// nameCandidateSQL deliberately selects only columns covered by the candidate
// index. A leading wildcard scans narrow index entries, never wide node pages,
// and the length bound seeks past names too short to contain substr. The
// repository-less range preserves the synthetic-node scope policy.
func nameCandidateSQL(generation int64, substr string, filter graph.NameSearchFilter) (string, []any) {
	pattern := "%" + escapeLikePattern(substr) + "%"
	minLength := likeMatchMinLength(substr)
	if len(pattern) > sqliteLikePatternLimit {
		// Test every row, so the refusal surfaces exactly as an unbounded scan's.
		minLength = 0
	}
	with, source := nameCandidateGenerationRepos, `SELECT repo FROM candidate_repos WHERE repo IS NOT NULL`
	args := []any{generation, generation, generation}
	if len(filter.RepoAllow) > 0 {
		repos := []string{""}
		for repo, allowed := range filter.RepoAllow {
			if allowed && repo != "" {
				repos = append(repos, repo)
			}
		}
		sort.Strings(repos)
		encoded, _ := json.Marshal(repos) // strings cannot fail JSON encoding
		with, source = "", `SELECT value FROM json_each(?)`
		args = []any{generation, string(encoded)}
	}
	q := with + `SELECT ` + nameCandidateCols + ` FROM nodes INDEXED BY nodes_name_candidates WHERE name <> '' AND view_gen = ? AND repo_prefix IN (` +
		source + `) AND length(name) >= ? AND name LIKE ? ESCAPE '\' ORDER BY id`
	return q, append(args, minLength, pattern)
}

// likeMatchMinLength is the fewest characters, as length() counts them, in a
// name that the pattern built from substr matches. SQLite 3.54's length()
// groups continuation bytes after ANY non-ASCII byte, including an orphaned
// continuation byte. Counting those separately can exclude a matching legacy
// TEXT name containing invalid UTF-8. The pattern ends at its first NUL.
func likeMatchMinLength(substr string) int {
	n := 0
	for i := 0; i < len(substr) && substr[i] != 0; n++ {
		lead := substr[i]
		i++
		if lead >= 0x80 {
			for i < len(substr) && substr[i]&0xc0 == 0x80 {
				i++
			}
		}
	}
	return n
}

// FindNodesByNameContainingFilteredContext filters compact matching keys before
// the candidate cap and hydrates only admitted IDs. Both phases use one read
// snapshot, so a concurrent rename or deletion cannot mix old keys with new
// payload. No mutable-generation cache or invalidation policy is required.
func (s *Store) FindNodesByNameContainingFilteredContext(ctx context.Context, substr string, limit int, filter graph.NameSearchFilter) ([]*graph.Node, error) {
	ctx = liveNameContext(ctx)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s.coreless() || substr == "" {
		return nil, nil
	}
	index, indexed := s.sealedNameIndex(ctx)
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	if indexed {
		return s.findFilteredNameCandidatesIndexed(ctx, tx, index, substr, limit, filter)
	}
	q, args := nameCandidateSQL(s.viewGen, substr, filter)
	if filter.Accept == nil && limit > 0 {
		q += ` LIMIT ?`
		args = append(args, limit)
	}
	ids, err := collectNameCandidateIDs(ctx, tx, q, args, limit, filter)
	if err != nil {
		return nil, err
	}
	return hydrateNameCandidateIDs(ctx, tx, s.viewGen, ids)
}

func collectNameCandidateIDs(ctx context.Context, tx *sql.Tx, q string, args []any, limit int, filter graph.NameSearchFilter) ([]string, error) {
	rows, err := tx.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var key graph.Node
		if err := rows.Scan(&key.ID, &key.Name, &key.Kind, &key.FilePath, &key.RepoPrefix, &key.WorkspaceID, &key.ProjectID); err != nil {
			_ = rows.Close()
			return nil, err
		}
		if err := ctx.Err(); err != nil {
			_ = rows.Close()
			return nil, err
		}
		if filter.Allows(&key) {
			ids = append(ids, key.ID)
			if limit > 0 && len(ids) >= limit {
				break
			}
		}
	}
	rowsErr := rows.Err()
	closeErr := rows.Close()
	if rowsErr != nil {
		return nil, rowsErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	return ids, ctx.Err()
}

// Immutable generations retain their suffix-array acceleration. Only matching
// IDs need compact scope headers; rejected matches never hydrate payloads.
func (s *Store) findFilteredNameCandidatesIndexed(ctx context.Context, tx *sql.Tx, index *generationNameIndex, substr string, limit int, filter graph.NameSearchFilter) ([]*graph.Node, error) {
	folded := asciiLower(substr)
	ids, err := index.containingIDs(ctx, 0, folded, true, func(entry *generationNameEntry) bool {
		return likeContains(entry, folded)
	})
	if err != nil {
		return nil, err
	}
	var kept []string
	for start := 0; start < len(ids); start += generationNameIndexFetchChunk {
		chunk := ids[start:min(start+generationNameIndexFetchChunk, len(ids))]
		args := []any{s.viewGen}
		for _, id := range chunk {
			args = append(args, id)
		}
		q := `SELECT ` + nameCandidateCols + ` FROM ` + nodesByIDSource + ` WHERE view_gen = ? AND id IN (` + inPlaceholders(len(chunk)) + `) ORDER BY id`
		remaining := 0
		if limit > 0 {
			remaining = limit - len(kept)
		}
		admitted, err := collectNameCandidateIDs(ctx, tx, q, args, remaining, filter)
		if err != nil {
			return nil, err
		}
		kept = append(kept, admitted...)
		if limit > 0 && len(kept) >= limit {
			break
		}
	}
	return hydrateNameCandidateIDs(ctx, tx, s.viewGen, kept)
}

func liveNameContext(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	return ctx
}

func hydrateNameCandidateIDs(ctx context.Context, tx *sql.Tx, generation int64, ids []string) ([]*graph.Node, error) {
	var nodes []*graph.Node
	for start := 0; start < len(ids); start += generationNameIndexFetchChunk {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		chunk := ids[start:min(start+generationNameIndexFetchChunk, len(ids))]
		q := `SELECT ` + lookupNodeCols + ` FROM ` + nodesByIDSource + ` WHERE view_gen = ? AND id IN (` + inPlaceholders(len(chunk)) + `) ORDER BY id`
		args := []any{generation}
		for _, id := range chunk {
			args = append(args, id)
		}
		rows, err := tx.QueryContext(ctx, q, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			node, err := scanNodeCursor(rows)
			if err != nil {
				_ = rows.Close()
				return nil, err
			}
			nodes = append(nodes, node)
		}
		rowsErr := rows.Err()
		closeErr := rows.Close()
		if rowsErr != nil {
			return nil, rowsErr
		}
		if closeErr != nil {
			return nil, closeErr
		}
	}
	return nodes, ctx.Err()
}

var _ graph.FilteredContainingNameReader = (*Store)(nil)
