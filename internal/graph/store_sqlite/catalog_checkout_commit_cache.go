package store_sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"
)

// CheckoutCommitCachePin is one checkout's durable claim that a commit
// generation should remain available for a later branch switch. GraphID is
// copied from the generation so graph-local retention never has to infer
// ownership from the checkout's current route.
type CheckoutCommitCachePin struct {
	CheckoutID   string
	GenerationID int64
	GraphID      string
	LastSelected int64
	StorageBytes int64
}

// CheckoutCommitCacheRetention is the graph-local eviction contract. The
// caller supplies an absolute cutoff so tests and lifecycle clocks remain
// deterministic. Routed generations are excluded from all three bounds.
type CheckoutCommitCacheRetention struct {
	InactiveCutoff  int64
	MaxGenerations  int
	MaxStorageBytes int64
}

// CheckoutCommitCachePruneResult reports unique generations whose last cache
// holder was evicted. The lifecycle uses these ids to drop process-local hints
// and offer payload retirement without rescanning the whole catalog.
type CheckoutCommitCachePruneResult struct {
	EvictedGenerationIDs []int64
	DeletedPins          int64
}

const backfillRoutedCheckoutCommitCachePinsSQL = `
INSERT INTO checkout_commit_cache_pins(
    checkout_id, generation_id, graph_id, last_selected
)
SELECT route.checkout_id, route.commit_generation_id, generation.graph_id,
       MAX(generation.last_selected, generation.published_at, generation.created_at)
  FROM checkout_routes AS route
  JOIN checkouts AS checkout ON checkout.checkout_id = route.checkout_id
  JOIN view_generations AS generation
    ON generation.generation_id = route.commit_generation_id
 WHERE route.commit_generation_id IS NOT NULL
   AND generation.generation_kind = 'commit'
   AND generation.graph_id = route.graph_id
ON CONFLICT(checkout_id, generation_id) DO UPDATE SET
  graph_id = excluded.graph_id,
  last_selected = MAX(checkout_commit_cache_pins.last_selected, excluded.last_selected)`

const backfillOwnedCheckoutCommitCachePinsSQL = `
INSERT INTO checkout_commit_cache_pins(
    checkout_id, generation_id, graph_id, last_selected
)
SELECT generation.checkout_id, generation.generation_id, generation.graph_id,
       MAX(generation.last_selected, generation.published_at, generation.created_at)
  FROM view_generations AS generation
  JOIN checkouts AS checkout ON checkout.checkout_id = generation.checkout_id
 WHERE generation.checkout_id IS NOT NULL
   AND generation.checkout_id <> ''
   AND generation.generation_kind = 'commit'
   AND generation.state IN ('ready', 'superseded')
ON CONFLICT(checkout_id, generation_id) DO UPDATE SET
  graph_id = excluded.graph_id,
  last_selected = MAX(checkout_commit_cache_pins.last_selected, excluded.last_selected)`

// BackfillRoutedCheckoutCommitCachePins repairs the current-route ownership
// bridge before startup retirement scans. It is idempotent and never touches
// dirty generations or refreshes an existing pin beyond the generation's
// already-persisted selection clock.
func (c *Catalog) BackfillRoutedCheckoutCommitCachePins(ctx context.Context) error {
	return c.withTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, backfillRoutedCheckoutCommitCachePinsSQL); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `
DELETE FROM checkout_commit_cache_retirements
 WHERE generation_id IN (SELECT generation_id FROM checkout_commit_cache_pins)`)
		return err
	})
}

// DeleteCheckoutCommitCachePins revokes every reusable commit held by one
// checkout. It is idempotent because purge/forget sagas resume the same phase
// after a crash and checkout deletion may already have cascaded the rows.
func (c *Catalog) DeleteCheckoutCommitCachePins(ctx context.Context, checkoutID string) error {
	if err := requireCatalogID("checkout_id", checkoutID); err != nil {
		return err
	}
	return c.withTx(ctx, func(tx *sql.Tx) error {
		return deleteCheckoutCommitCachePinsForCheckoutTx(ctx, tx, checkoutID)
	})
}

// upsertCheckoutCommitCachePinTx records one valid commit generation. Zero is
// the optional/unset route value. A positive generation must select exactly
// one retainable commit row; silently accepting a miss would publish a route
// without its durable cache owner and make later retirement unsafe.
func upsertCheckoutCommitCachePinTx(
	ctx context.Context,
	tx *sql.Tx,
	checkoutID, graphID string,
	generationID, selectedAt int64,
) error {
	if generationID <= 0 {
		return nil
	}
	if err := requireCatalogID("checkout_id", checkoutID); err != nil {
		return err
	}
	if err := requireCatalogID("graph_id", graphID); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `
INSERT INTO checkout_commit_cache_pins(
    checkout_id, generation_id, graph_id, last_selected
)
SELECT ?, generation_id, graph_id, ?
  FROM view_generations
 WHERE generation_id = ?
   AND graph_id = ?
   AND generation_kind = 'commit'
   AND state IN ('ready', 'superseded')
ON CONFLICT(checkout_id, generation_id) DO UPDATE SET
  graph_id = excluded.graph_id,
  last_selected = MAX(checkout_commit_cache_pins.last_selected, excluded.last_selected)`,
		checkoutID, selectedAt, generationID, graphID)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows != 1 {
		return fmt.Errorf("%w: checkout %s commit generation %d is not retainable on graph %s",
			ErrCatalogStaleGuard, checkoutID, generationID, graphID)
	}
	_, err = tx.ExecContext(ctx,
		`DELETE FROM checkout_commit_cache_retirements WHERE generation_id = ?`, generationID)
	return err
}

func deleteCheckoutCommitCachePinsForCheckoutTx(
	ctx context.Context, tx *sql.Tx, checkoutID string,
) error {
	_, err := tx.ExecContext(ctx,
		`DELETE FROM checkout_commit_cache_pins WHERE checkout_id = ?`, checkoutID)
	return err
}

// deleteCheckoutCommitCachePinsOutsideGraphTx removes ownership inherited from
// the checkout's previous graph while preserving branch history already built
// in the transition's target graph. Promotion/demotion publication may be
// replayed while its external-cleanup journal is still standing; deleting all
// pins on that replay would erase newer same-graph selections.
func deleteCheckoutCommitCachePinsOutsideGraphTx(
	ctx context.Context, tx *sql.Tx, checkoutID, graphID string,
) error {
	_, err := tx.ExecContext(ctx, `
DELETE FROM checkout_commit_cache_pins
 WHERE checkout_id = ? AND graph_id <> ?`, checkoutID, graphID)
	return err
}

func deleteCheckoutCommitCachePinsForGraphTx(
	ctx context.Context, tx *sql.Tx, graphID string,
) error {
	_, err := tx.ExecContext(ctx,
		`DELETE FROM checkout_commit_cache_pins WHERE graph_id = ?`, graphID)
	return err
}

// ListCheckoutCommitCachePins returns holder rows in deterministic order. It
// is primarily an observability/testing surface; retention groups the same
// generation across holders inside its own write transaction.
func (c *Catalog) ListCheckoutCommitCachePins(
	ctx context.Context, graphID string,
) ([]CheckoutCommitCachePin, error) {
	query := `
SELECT pin.checkout_id, pin.generation_id, pin.graph_id, pin.last_selected,
       generation.storage_bytes
  FROM checkout_commit_cache_pins AS pin
  JOIN view_generations AS generation
    ON generation.generation_id = pin.generation_id`
	var args []any
	if graphID != "" {
		query += ` WHERE pin.graph_id = ?`
		args = append(args, graphID)
	}
	query += ` ORDER BY pin.graph_id, pin.last_selected, pin.generation_id, pin.checkout_id`
	rows, err := c.store.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CheckoutCommitCachePin
	for rows.Next() {
		var pin CheckoutCommitCachePin
		if err := rows.Scan(&pin.CheckoutID, &pin.GenerationID, &pin.GraphID,
			&pin.LastSelected, &pin.StorageBytes); err != nil {
			return nil, err
		}
		out = append(out, pin)
	}
	return out, rows.Err()
}

// CheckoutCommitCachePinnedGenerations returns the ids in generationIDs that
// still have at least one holder. Input is de-duplicated and split below
// SQLite's host-parameter ceiling.
func (c *Catalog) CheckoutCommitCachePinnedGenerations(
	ctx context.Context, generationIDs []int64,
) (map[int64]struct{}, error) {
	out := make(map[int64]struct{})
	ids := slices.Clone(generationIDs)
	slices.Sort(ids)
	ids = slices.Compact(ids)
	for len(ids) > 0 && ids[0] <= 0 {
		ids = ids[1:]
	}
	const batchSize = 512
	for first := 0; first < len(ids); first += batchSize {
		last := min(first+batchSize, len(ids))
		batch := ids[first:last]
		placeholders := strings.TrimSuffix(strings.Repeat("?,", len(batch)), ",")
		args := make([]any, len(batch))
		for i, id := range batch {
			args[i] = id
		}
		rows, err := c.store.db.QueryContext(ctx, `
SELECT DISTINCT generation_id
  FROM checkout_commit_cache_pins
 WHERE generation_id IN (`+placeholders+`)`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				_ = rows.Close()
				return nil, err
			}
			out[id] = struct{}{}
		}
		if err := rows.Close(); err != nil {
			return nil, err
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// ListCheckoutCommitCacheRetirementCandidates pages through the durable
// handoff written by pin eviction. It is deliberately read-only and returns
// only generations with no currently visible durable owner. Retirement still
// repeats these guards under the catalog write gate, so a concurrent route,
// re-pin, child layer, or handoff lease wins atomically rather than being
// deleted from under its publisher.
func (c *Catalog) ListCheckoutCommitCacheRetirementCandidates(
	ctx context.Context,
	afterGenerationID int64,
	limit int,
) ([]int64, error) {
	if afterGenerationID < 0 {
		return nil, fmt.Errorf("after generation id must not be negative")
	}
	if limit <= 0 {
		return nil, fmt.Errorf("retirement candidate limit must be positive")
	}
	rows, err := c.store.db.QueryContext(ctx, `
SELECT queued.generation_id
  FROM checkout_commit_cache_retirements AS queued
 WHERE queued.generation_id > ?
   AND NOT EXISTS (
       SELECT 1 FROM checkout_commit_cache_pins AS pin
        WHERE pin.generation_id = queued.generation_id
   )
   AND NOT EXISTS (
       SELECT 1 FROM checkout_routes AS route
        WHERE route.commit_generation_id = queued.generation_id
           OR route.dirty_generation_id = queued.generation_id
   )
   AND NOT EXISTS (
       SELECT 1 FROM ref_views AS view
        WHERE view.active_generation_id = queued.generation_id
   )
   AND NOT EXISTS (
       SELECT 1 FROM view_generations AS child
        WHERE child.base_generation_id = queued.generation_id
   )
   AND NOT EXISTS (
       SELECT 1 FROM dedicated_graphs AS graph
        WHERE graph.active_generation_id = queued.generation_id
   )
 ORDER BY queued.generation_id
 LIMIT ?`, afterGenerationID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]int64, 0, min(limit, 512))
	for rows.Next() {
		var generationID int64
		if err := rows.Scan(&generationID); err != nil {
			return nil, err
		}
		out = append(out, generationID)
	}
	return out, rows.Err()
}

type checkoutCommitCacheCandidate struct {
	graphID      string
	generationID int64
	selected     int64
	bytes        int64
	routed       bool
}

// PruneCheckoutCommitCachePins applies the age/count/byte bounds to unique
// non-routed generations. Selection and deletion share one transaction under
// the store write gate, so a concurrent A->B publication cannot refresh a pin
// between the retention decision and its deletion.
func (c *Catalog) PruneCheckoutCommitCachePins(
	ctx context.Context,
	retention CheckoutCommitCacheRetention,
) (CheckoutCommitCachePruneResult, error) {
	var out CheckoutCommitCachePruneResult
	if retention.MaxGenerations <= 0 {
		return out, fmt.Errorf("checkout commit cache max generations must be positive")
	}
	if retention.MaxStorageBytes <= 0 {
		return out, fmt.Errorf("checkout commit cache max storage bytes must be positive")
	}
	err := c.withTx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `
SELECT pin.graph_id, pin.generation_id, MAX(pin.last_selected),
       MAX(generation.storage_bytes),
       EXISTS(
         SELECT 1 FROM checkout_routes AS route
          WHERE route.commit_generation_id = pin.generation_id
             OR route.dirty_generation_id = pin.generation_id
       )
  FROM checkout_commit_cache_pins AS pin
  JOIN view_generations AS generation
    ON generation.generation_id = pin.generation_id
 GROUP BY pin.graph_id, pin.generation_id
 ORDER BY pin.graph_id, MAX(pin.last_selected), pin.generation_id`)
		if err != nil {
			return err
		}
		byGraph := make(map[string][]checkoutCommitCacheCandidate)
		var graphOrder []string
		for rows.Next() {
			var candidate checkoutCommitCacheCandidate
			var routed int
			if err := rows.Scan(&candidate.graphID, &candidate.generationID,
				&candidate.selected, &candidate.bytes, &routed); err != nil {
				_ = rows.Close()
				return err
			}
			candidate.routed = routed != 0
			if _, exists := byGraph[candidate.graphID]; !exists {
				graphOrder = append(graphOrder, candidate.graphID)
			}
			byGraph[candidate.graphID] = append(byGraph[candidate.graphID], candidate)
		}
		if err := rows.Close(); err != nil {
			return err
		}
		if err := rows.Err(); err != nil {
			return err
		}

		for _, graphID := range graphOrder {
			candidates := byGraph[graphID]
			count := 0
			var bytes int64
			for _, candidate := range candidates {
				if candidate.routed {
					continue
				}
				count++
				if candidate.bytes > 0 {
					bytes += candidate.bytes
				}
			}
			for _, candidate := range candidates {
				if candidate.routed {
					continue
				}
				stale := candidate.selected < retention.InactiveCutoff
				if !stale && count <= retention.MaxGenerations && bytes <= retention.MaxStorageBytes {
					continue
				}
				result, err := tx.ExecContext(ctx, `
DELETE FROM checkout_commit_cache_pins
 WHERE graph_id = ? AND generation_id = ?`, graphID, candidate.generationID)
				if err != nil {
					return err
				}
				changed, err := result.RowsAffected()
				if err != nil {
					return err
				}
				if changed > 0 {
					out.DeletedPins += changed
					out.EvictedGenerationIDs = append(out.EvictedGenerationIDs, candidate.generationID)
				}
				count--
				if candidate.bytes > 0 {
					bytes -= candidate.bytes
				}
			}
		}
		return nil
	})
	return out, err
}
