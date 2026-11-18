package store_sqlite

import (
	"context"
	"fmt"
	"strings"
)

// RetirementCandidateShape is what the deferred retirement sweep orders its
// candidates by: whether a generation is already fenced and part-swept, how
// large its payload was at publication, and whether any generation is still
// built on it (which refuses its retirement as "based" until that child's row
// is gone).
type RetirementCandidateShape struct {
	State        ViewGenerationState
	StorageBytes int64
	HasChildren  bool
}

// retirementShapesBatch bounds one IN list.
const retirementShapesBatch = 500

// RetirementCandidateShapes reads the shape of each listed generation. A
// generation the catalog no longer holds is absent from the result.
func (c *Catalog) RetirementCandidateShapes(ctx context.Context, ids []int64) (map[int64]RetirementCandidateShape, error) {
	out := make(map[int64]RetirementCandidateShape, len(ids))
	for start := 0; start < len(ids); start += retirementShapesBatch {
		batch := ids[start:min(start+retirementShapesBatch, len(ids))]
		args := make([]any, len(batch))
		for i, id := range batch {
			args[i] = id
		}
		rows, err := c.store.db.QueryContext(ctx, `SELECT g.generation_id, g.state, g.storage_bytes,
 EXISTS(SELECT 1 FROM view_generations child WHERE child.base_generation_id = g.generation_id)
 FROM view_generations g WHERE g.generation_id IN (`+strings.TrimSuffix(strings.Repeat("?,", len(batch)), ",")+`)`, args...)
		if err != nil {
			return out, fmt.Errorf("retirement candidate shapes: %w", err)
		}
		for rows.Next() {
			var id int64
			var shape RetirementCandidateShape
			if err := rows.Scan(&id, &shape.State, &shape.StorageBytes, &shape.HasChildren); err != nil {
				_ = rows.Close()
				return out, fmt.Errorf("retirement candidate shapes: %w", err)
			}
			out[id] = shape
		}
		err = rows.Err()
		_ = rows.Close()
		if err != nil {
			return out, fmt.Errorf("retirement candidate shapes: %w", err)
		}
	}
	return out, nil
}
