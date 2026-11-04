package store_sqlite

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/zzet/gortex/internal/graph"
)

// FindFoldedNameCandidatesContext retains the field grammar's Go Unicode
// predicate before the horizon. Compact selection and bounded hydration use
// the same physical read transaction; no cache token certifies exhaustion.
func (s *Store) FindFoldedNameCandidatesContext(ctx context.Context, needle string, limit int, filter graph.NameSearchFilter) ([]*graph.Node, bool, error) {
	ctx = liveNameContext(ctx)
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	if s.coreless() || s.db == nil {
		return nil, false, sql.ErrConnDone
	}
	if needle == "" || limit <= 0 {
		return nil, false, fmt.Errorf("store_sqlite: invalid folded-name candidate head")
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = tx.Rollback() }()
	keys, more, err := selectFoldedNameCandidates(ctx, tx, s.viewGen, needle, limit, filter)
	if err != nil {
		return nil, false, err
	}
	ids := make([]string, len(keys))
	for i, key := range keys {
		ids[i] = key.ID
	}
	nodes, err := hydrateNameCandidateIDs(ctx, tx, s.viewGen, ids)
	if err != nil {
		return nil, false, err
	}
	byID := make(map[string]*graph.Node, len(nodes))
	for _, node := range nodes {
		byID[node.ID] = node
	}
	out := make([]*graph.Node, 0, len(keys))
	for _, key := range keys {
		node := byID[key.ID]
		if node == nil || node.Name != key.Name {
			return nil, false, fmt.Errorf("store_sqlite: inconsistent folded-name snapshot")
		}
		out = append(out, node)
	}
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	return out, more, nil
}

func selectFoldedNameCandidates(ctx context.Context, tx *sql.Tx, generation int64, needle string, limit int, filter graph.NameSearchFilter) ([]graph.NodeSearchKey, bool, error) {
	rows, err := tx.QueryContext(ctx, `SELECT `+nameCandidateCols+` FROM nodes WHERE view_gen = ? ORDER BY id`, generation)
	if err != nil {
		return nil, false, err
	}
	set := graph.NewFoldedNameCandidateSet(needle, limit)
	for rows.Next() {
		var key graph.NodeSearchKey
		if err := rows.Scan(&key.ID, &key.Name, &key.Kind, &key.FilePath, &key.RepoPrefix, &key.WorkspaceID, &key.ProjectID); err != nil {
			_ = rows.Close()
			return nil, false, err
		}
		if err := ctx.Err(); err != nil {
			_ = rows.Close()
			return nil, false, err
		}
		compact := &graph.Node{ID: key.ID, Name: key.Name, Kind: key.Kind, FilePath: key.FilePath, RepoPrefix: key.RepoPrefix, WorkspaceID: key.WorkspaceID, ProjectID: key.ProjectID}
		if filter.Allows(compact) {
			set.Add(key)
		}
	}
	rowsErr, closeErr := rows.Err(), rows.Close()
	if rowsErr != nil {
		return nil, false, rowsErr
	}
	if closeErr != nil {
		return nil, false, closeErr
	}
	return set.Keys(), set.More(), ctx.Err()
}

var _ graph.FoldedNameCandidateReader = (*Store)(nil)
