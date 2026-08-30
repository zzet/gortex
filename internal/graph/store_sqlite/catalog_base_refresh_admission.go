package store_sqlite

import (
	"context"
	"database/sql"
	"fmt"
)

// BeginDedicatedBaseRefresh makes a pipeline-stale graph visibly inexact
// before the replacement build starts. Generation pointers remain intact for
// pinned readers and labeled fallback, but new exact checkout/ref publication
// is fenced by the non-ready graph state. Re-admission after failure/restart is
// idempotent and does not repeatedly advance ref epochs.
func (c *Catalog) BeginDedicatedBaseRefresh(
	ctx context.Context, graphID string, expectedBaseGenerationID int64,
) error {
	if err := requireCatalogID("graph_id", graphID); err != nil {
		return err
	}
	if expectedBaseGenerationID <= 0 {
		return fmt.Errorf("expected base generation id must be positive")
	}
	return c.withTx(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `
UPDATE dedicated_graphs
   SET state = ?
 WHERE graph_id = ? AND active_generation_id = ? AND state = ?`,
			DedicatedGraphStateRefreshing, graphID, expectedBaseGenerationID,
			DedicatedGraphStateReady)
		if err != nil {
			return err
		}
		changed, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if changed == 0 {
			var state string
			var activeGenerationID int64
			err := tx.QueryRowContext(ctx, `
SELECT state, COALESCE(active_generation_id, 0)
  FROM dedicated_graphs WHERE graph_id = ?`, graphID).Scan(&state, &activeGenerationID)
			if err != nil {
				return err
			}
			if state == DedicatedGraphStateRefreshing && activeGenerationID == expectedBaseGenerationID {
				return nil
			}
			return fmt.Errorf("%w: dedicated graph %s moved before refresh admission",
				ErrCatalogStaleGuard, graphID)
		}

		if _, err := tx.ExecContext(ctx, `
UPDATE checkout_routes
   SET state = ?, route_epoch = route_epoch + 1
 WHERE graph_id = ? AND state <> ?`,
			string(RoutePending), graphID, string(RouteRetired)); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
UPDATE ref_views
   SET state = ?, exact_view = 0, route_epoch = route_epoch + 1,
       last_error = ?
 WHERE graph_id = ?`,
			string(RefViewBuilding), "dedicated base pipeline refresh in progress", graphID); err != nil {
			return err
		}
		return nil
	})
}
