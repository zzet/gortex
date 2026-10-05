package store_sqlite

import (
	"context"
	"fmt"

	"github.com/zzet/gortex/internal/graph"
)

var _ graph.NodeKindMembershipReader = (*Store)(nil)

// GetNodeIDsByKindsContext reads only matching identities from this physical
// generation. Requested IDs use the existing bounded lookup chunks; no node
// location or metadata is decoded. Errors never return a usable partial set.
func (s *Store) GetNodeIDsByKindsContext(ctx context.Context, ids []string, kinds []graph.NodeKind) (map[string]struct{}, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	ids = dedupeNonEmpty(ids)
	kindArgs := make([]any, 0, len(kinds))
	seen := make(map[graph.NodeKind]bool, len(kinds))
	for _, kind := range kinds {
		if !seen[kind] {
			seen[kind] = true
			kindArgs = append(kindArgs, kind)
		}
	}
	result := make(map[string]struct{})
	if len(kindArgs) == 0 {
		return result, nil
	}
	for start := 0; start < len(ids); start += lookupChunkSize {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		chunk := ids[start:min(start+lookupChunkSize, len(ids))]
		q := `SELECT id FROM nodes WHERE id IN (` + inPlaceholders(len(chunk)) + `) AND view_gen = ? AND kind IN (` + inPlaceholders(len(kindArgs)) + `)`
		args := toAnyArgs(chunk)
		args = append(args, s.viewGen)
		args = append(args, kindArgs...)
		rows, err := s.db.QueryContext(ctx, q, args...)
		if err != nil {
			return nil, fmt.Errorf("get node kind membership: %w", err)
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				_ = rows.Close()
				return nil, fmt.Errorf("scan node kind membership: %w", err)
			}
			result[id] = struct{}{}
		}
		err = rows.Err()
		closeErr := rows.Close()
		if err != nil {
			return nil, fmt.Errorf("read node kind membership: %w", err)
		}
		if closeErr != nil {
			return nil, closeErr
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return result, nil
}
