package store_sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"sort"

	"github.com/zzet/gortex/internal/graph"
)

const nameCandidateCols = `id, name, kind, file_path, repo_prefix, workspace_id, project_id`

// nameCandidateSQL deliberately selects only columns covered by the candidate
// index. A leading wildcard scans narrow index entries, never wide node pages.
// The repository-less range preserves the synthetic-node scope policy.
func nameCandidateSQL(generation int64, substr string, filter graph.NameSearchFilter) (string, []any) {
	q := `SELECT ` + nameCandidateCols + ` FROM nodes INDEXED BY nodes_name_candidates WHERE name <> '' AND view_gen = ? AND name LIKE ? ESCAPE '\'`
	args := []any{generation, "%" + escapeLikePattern(substr) + "%"}
	if len(filter.RepoAllow) > 0 {
		repos := []string{""}
		for repo, allowed := range filter.RepoAllow {
			if allowed && repo != "" {
				repos = append(repos, repo)
			}
		}
		sort.Strings(repos)
		encoded, _ := json.Marshal(repos) // strings cannot fail JSON encoding
		q += ` AND repo_prefix IN (SELECT value FROM json_each(?))`
		args = append(args, string(encoded))
	}
	return q + ` ORDER BY id`, args
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
		q := `SELECT ` + nameCandidateCols + ` FROM nodes WHERE view_gen = ? AND id IN (` + inPlaceholders(len(chunk)) + `) ORDER BY id`
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
		q := `SELECT ` + lookupNodeCols + ` FROM nodes WHERE view_gen = ? AND id IN (` + inPlaceholders(len(chunk)) + `) ORDER BY id`
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
