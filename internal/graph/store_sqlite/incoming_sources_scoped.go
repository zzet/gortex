package store_sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"sort"

	"github.com/zzet/gortex/internal/graph"
)

// edges_by_to(to_id,kind) has the integer rowid suffix. Equality on both
// leading columns and id keyset ordering need no OFFSET or temporary sort.
// view_gen stays explicit; a read transaction preserves mutable-gen0 coherence.
const scopedIncomingSourcePageSQL = `SELECT id, from_id, file_path
FROM edges INDEXED BY edges_by_to
WHERE to_id = ? AND kind = ? AND id > ? AND from_id <> '' AND view_gen = ?
ORDER BY id
LIMIT ?`

const incomingSourceNodeExistsSQL = `SELECT EXISTS(SELECT 1 FROM nodes WHERE id = ? AND view_gen = ?)`

const maxIncomingSourcePageRows = 256

type incomingSourcePageRow struct {
	id        int64
	candidate graph.IncomingSourceCandidate
}

type incomingSourceTxQuery struct {
	tx *sql.Tx
	db *sql.DB
}

func (q incomingSourceTxQuery) LookupIncomingSourceNode(ctx context.Context, reader graph.Reader, id string) (bool, bool, error) {
	s, ok := reader.(*Store)
	if !ok || s.db != q.db {
		return false, false, nil
	}
	var exists bool
	err := q.tx.QueryRowContext(ctx, incomingSourceNodeExistsSQL, id, s.viewGen).Scan(&exists)
	return exists, true, err
}

func (s *Store) IncomingSourceNodeExists(ctx context.Context, id string, query graph.IncomingSourceNodeQuery) (bool, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if query != nil {
		exists, handled, err := query.LookupIncomingSourceNode(ctx, s, id)
		if handled || err != nil {
			return exists, err
		}
	}
	var exists bool
	err := s.db.QueryRowContext(ctx, incomingSourceNodeExistsSQL, id, s.viewGen).Scan(&exists)
	return exists, err
}

func readScopedIncomingSourcePage(ctx context.Context, tx *sql.Tx, target string, kind graph.EdgeKind, generation, after int64, limit int, reuse []incomingSourcePageRow) ([]incomingSourcePageRow, error) {
	rows, err := tx.QueryContext(ctx, scopedIncomingSourcePageSQL, target, kind, after, generation, limit)
	if err != nil {
		return nil, err
	}
	page := reuse[:0]
	for rows.Next() {
		page = append(page, incomingSourcePageRow{})
		row := &page[len(page)-1]
		if err := rows.Scan(&row.id, &row.candidate.From, &row.candidate.FilePath); err != nil {
			_ = rows.Close()
			return nil, err
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, fmt.Errorf("incoming-source page: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	return page, nil
}

func (s *Store) FindIncomingSourcesScoped(ctx context.Context, targetIDs []string, kind graph.EdgeKind, limit int, scope graph.IncomingSourceScope, budget *graph.IncomingSourceBudget) (graph.BoundedIncomingSourceProjection, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if budget == nil {
		budget = &graph.IncomingSourceBudget{}
	}
	if err := ctx.Err(); err != nil {
		return graph.BoundedIncomingSourceProjection{}, err
	}
	ids, err := graph.ValidateScopedIncomingSources(targetIDs, limit)
	if err != nil {
		return graph.BoundedIncomingSourceProjection{}, err
	}
	out := graph.BoundedIncomingSourceProjection{Sources: make(map[string][]string), Truncated: make(map[string]bool)}
	if limit <= 0 {
		for _, id := range ids {
			out.Truncated[id] = true
		}
		return out, nil
	}
	if len(ids) == 0 {
		return out, nil
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return graph.BoundedIncomingSourceProjection{}, err
	}
	defer func() { _ = tx.Rollback() }()
	query := incomingSourceTxQuery{tx: tx, db: s.db}
	filter := scope.NewFilter()
	var page []incomingSourcePageRow
	for _, target := range ids {
		if err := ctx.Err(); err != nil {
			return graph.BoundedIncomingSourceProjection{}, err
		}
		visible, err := filter.TargetVisible(ctx, target, query)
		if err != nil {
			return graph.BoundedIncomingSourceProjection{}, err
		}
		if !visible {
			continue
		}
		seen := make(map[string]struct{})
		var lastCandidate graph.IncomingSourceCandidate
		hasLastCandidate := false
		groupRepeatedSites := false
		after := int64(0)
		pageSize := min(limit+1, maxIncomingSourcePageRows)
		for {
			if err := ctx.Err(); err != nil {
				return graph.BoundedIncomingSourceProjection{}, err
			}
			readLimit := min(pageSize, budget.Remaining()+1)
			physicalRows, next := 0, after
			if groupRepeatedSites {
				page, physicalRows, next, err = readScopedIncomingSourceGroupedPage(ctx, tx, target, kind, s.viewGen, after, readLimit, page)
			} else {
				page, err = readScopedIncomingSourcePage(ctx, tx, target, kind, s.viewGen, after, readLimit, page)
				physicalRows = len(page)
				if physicalRows > 0 {
					next = page[physicalRows-1].id
				}
			}
			if err != nil {
				return graph.BoundedIncomingSourceProjection{}, err
			}
			// Preserve the existing eager-page budget: charge every selected raw
			// row before filtering, even if a sentinel exists inside this page.
			if err := budget.Charge(physicalRows); err != nil {
				return graph.BoundedIncomingSourceProjection{}, err
			}
			// The cursor is closed before any ownership/presence callback. The
			// bridge routes same-pool node reads through this exact transaction.
			for index, row := range page {
				if index&127 == 0 {
					if err := ctx.Err(); err != nil {
						return graph.BoundedIncomingSourceProjection{}, err
					}
				}
				// Consecutive sites with identical source/file provenance have the
				// same immutable ownership decision. Still charge EVERY raw row.
				if hasLastCandidate && row.candidate == lastCandidate {
					groupRepeatedSites = true
					continue
				}
				lastCandidate, hasLastCandidate = row.candidate, true
				allowed, err := filter.Allows(ctx, row.candidate, query)
				if err != nil {
					return graph.BoundedIncomingSourceProjection{}, err
				}
				if !allowed {
					continue
				}
				seen[row.candidate.From] = struct{}{}
				if len(seen) > limit {
					out.Truncated[target] = true
					break
				}
			}
			if out.Truncated[target] || physicalRows < readLimit {
				break
			}
			after = next
			pageSize = min(pageSize*2, maxIncomingSourcePageRows)
		}
		if out.Truncated[target] {
			continue
		}
		for source := range seen {
			out.Sources[target] = append(out.Sources[target], source)
		}
		sort.Strings(out.Sources[target])
	}
	if err := ctx.Err(); err != nil {
		return graph.BoundedIncomingSourceProjection{}, err
	}
	if err := tx.Commit(); err != nil {
		return graph.BoundedIncomingSourceProjection{}, err
	}
	return out, nil
}

var (
	_ graph.ScopedIncomingSourceReader = (*Store)(nil)
	_ graph.IncomingSourceNodeChecker  = (*Store)(nil)
)
