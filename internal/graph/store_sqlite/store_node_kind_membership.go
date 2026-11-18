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
	kindArgs := nodeKindMembershipArgs(kinds)
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

// nodeKindMembershipArgs binds each distinct kind once, in first-seen order.
func nodeKindMembershipArgs(kinds []graph.NodeKind) []any {
	args := make([]any, 0, len(kinds))
	seen := make(map[graph.NodeKind]bool, len(kinds))
	for _, kind := range kinds {
		if !seen[kind] {
			seen[kind] = true
			args = append(args, kind)
		}
	}
	return args
}

// nodesByKindIndexName is the (kind, view_gen) index a whole-kind identity
// read is pinned to. Cold bulk loads drop it until their index rebuild.
const nodesByKindIndexName = "nodes_by_kind"

// NodeIDsByKindsBoundedContext reads the identities of every node of kinds in
// this physical generation: the membership GetNodeIDsByKindsContext answers
// per requested ID, read whole. The statement is pinned to the kind index, so
// it never visits node payloads or scans the generation. It returns at most
// limit IDs: when more match, or while the index is dropped, complete is false
// and no IDs are returned, so a caller keeps its per-ID lookups. Errors never
// return a usable partial set.
func (s *Store) NodeIDsByKindsBoundedContext(ctx context.Context, kinds []graph.NodeKind, limit int) (ids []string, complete bool, err error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	if s.coreless() || limit <= 0 {
		return nil, false, nil
	}
	kindArgs := nodeKindMembershipArgs(kinds)
	if len(kindArgs) == 0 {
		return nil, true, nil
	}
	q := `SELECT id FROM nodes INDEXED BY ` + nodesByKindIndexName + ` WHERE kind IN (` + inPlaceholders(len(kindArgs)) + `) AND view_gen = ? LIMIT ?`
	args := append(kindArgs, s.viewGen, limit+1)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		// A pinned statement fails to prepare without its index; that is the
		// bulk window, not a read failure. The error alone settles it: the
		// index can be rebuilt before any probe runs, and a probe that finds it
		// again must not turn the declined read into a failure.
		if isNoSuchIndexErr(err) {
			return nil, false, ctx.Err()
		}
		if ctx.Err() == nil {
			if present, probeErr := s.nodesByKindIndexPresent(ctx); probeErr == nil && !present {
				return nil, false, nil
			}
		}
		return nil, false, fmt.Errorf("get node identities by kind: %w", err)
	}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return nil, false, fmt.Errorf("scan node identities by kind: %w", err)
		}
		if len(ids) == limit {
			_ = rows.Close()
			return nil, false, ctx.Err()
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	closeErr := rows.Close()
	if err != nil {
		return nil, false, fmt.Errorf("read node identities by kind: %w", err)
	}
	if closeErr != nil {
		return nil, false, closeErr
	}
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	return ids, true, nil
}

// nodesByKindIndexPresent reports whether the kind index exists; it can be
// dropped and recreated under an open store.
func (s *Store) nodesByKindIndexPresent(ctx context.Context) (bool, error) {
	var present bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type = 'index' AND name = ?)`, nodesByKindIndexName).Scan(&present)
	return present, err
}
