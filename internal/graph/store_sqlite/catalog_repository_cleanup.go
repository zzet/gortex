package store_sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// DedicatedGraphClosing is durable withdrawal, not successful cleanup. Its
// owner and prefix remain reserved until the cleanup saga deletes the graph.
const DedicatedGraphClosing = "closing"

// ErrCatalogGraphClosing refuses new publication or build admission while a
// dedicated graph's durable cleanup is pending.
var ErrCatalogGraphClosing = errors.New("store_sqlite: dedicated graph is closing")

// RepositoryCleanupIdentity is the durable identity needed to restore a
// process-local owner fence. It deliberately includes generation-zero payload.
type RepositoryCleanupIdentity struct {
	GraphID     string
	CheckoutID  string
	Incarnation string
	RepoPrefix  string
	FamilyID    string
	RootPath    string
}

// BeginRepositoryCleanup fences a graph before physical teardown. The caller
// must already hold lifecycle cleanup authorization; this does not authorize
// untracking or revoke tracking intents. A missing graph is already released.
// The owner is retained, so restart can restore the exact admission tombstone.
func (c *Catalog) BeginRepositoryCleanup(ctx context.Context, graphID string) (RepositoryCleanupIdentity, bool, error) {
	var identity RepositoryCleanupIdentity
	var found bool
	if graphID == "" {
		return identity, false, fmt.Errorf("%w: cleanup graph is empty", ErrCatalogInvalidValue)
	}
	err := c.withTx(ctx, func(tx *sql.Tx) error {
		var err error
		identity, found, err = repositoryCleanupIdentityTx(ctx, tx, graphID)
		if err != nil || !found {
			return err
		}
		return closeDedicatedGraphAdmissionTx(ctx, tx, graphID)
	})
	if err != nil {
		return RepositoryCleanupIdentity{}, false, err
	}
	return identity, found, nil
}

func repositoryCleanupIdentityTx(ctx context.Context, tx *sql.Tx, graphID string) (RepositoryCleanupIdentity, bool, error) {
	identity := RepositoryCleanupIdentity{GraphID: graphID}
	err := tx.QueryRowContext(ctx, `
SELECT COALESCE(d.owner_checkout_id, ''), COALESCE(c.incarnation, ''),
       d.repo_prefix, d.family_id, COALESCE(c.root_path, '')
  FROM dedicated_graphs d
  LEFT JOIN checkouts c ON c.checkout_id = d.owner_checkout_id
 WHERE d.graph_id = ?`, graphID).Scan(
		&identity.CheckoutID, &identity.Incarnation, &identity.RepoPrefix, &identity.FamilyID, &identity.RootPath)
	if errors.Is(err, sql.ErrNoRows) {
		return RepositoryCleanupIdentity{}, false, nil
	}
	if err != nil {
		return RepositoryCleanupIdentity{}, false, err
	}
	if identity.CheckoutID == "" || identity.Incarnation == "" || identity.RepoPrefix == "" {
		return RepositoryCleanupIdentity{}, false, fmt.Errorf("%w: cleanup graph %s has no complete owner identity", ErrCatalogStaleGuard, graphID)
	}
	return identity, true, nil
}

// closeDedicatedGraphAdmissionTx belongs inside the same transaction as an
// authorized cleanup journal insertion (and demotion publication). Clearing the
// active pointer withdraws publication but does not mutate a captured reader.
func closeDedicatedGraphAdmissionTx(ctx context.Context, tx *sql.Tx, graphID string) error {
	if graphID == "" {
		return nil // An automatic checkout may own no dedicated graph.
	}
	_, err := tx.ExecContext(ctx, `
UPDATE dedicated_graphs SET state = ?, active_generation_id = NULL
 WHERE graph_id = ? AND (state != ? OR active_generation_id IS NOT NULL)`,
		DedicatedGraphClosing, graphID, DedicatedGraphClosing)
	return err
}

func closeAuthorizedUntrackGraphTx(ctx context.Context, tx *sql.Tx, req AuthorizeUntrackRequest) error {
	graphID := req.OwnedGraphID
	if req.Plan == UntrackAuthorizationPrimaryClosure {
		graphID = req.PrimaryGraphID
	}
	return closeDedicatedGraphAdmissionTx(ctx, tx, graphID)
}

// validateDedicatedGraphAdmissionTx preserves uncatalogued-generation support;
// graph lifecycle entry points separately validate ownership. For a catalogued
// graph, closing is a durable admission fence, including zero-generation refs.
func validateDedicatedGraphAdmissionTx(ctx context.Context, tx *sql.Tx, graphID string) error {
	if graphID == "" {
		return nil
	}
	var state string
	err := tx.QueryRowContext(ctx, `SELECT state FROM dedicated_graphs WHERE graph_id = ?`, graphID).Scan(&state)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if state == DedicatedGraphClosing {
		return fmt.Errorf("%w: graph %s", ErrCatalogGraphClosing, graphID)
	}
	return nil
}

func validateRefGraphAdmissionTx(ctx context.Context, tx *sql.Tx, refViewID string) error {
	var graphID string
	if err := tx.QueryRowContext(ctx, `SELECT graph_id FROM ref_views WHERE ref_view_id = ?`, refViewID).Scan(&graphID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: ref view %s", ErrCatalogNotFound, refViewID)
		}
		return err
	}
	return validateDedicatedGraphAdmissionTx(ctx, tx, graphID)
}

func validateRouteGraphAdmissionTx(ctx context.Context, tx *sql.Tx, checkoutID string) error {
	var graphID string
	if err := tx.QueryRowContext(ctx, `SELECT graph_id FROM checkout_routes WHERE checkout_id = ?`, checkoutID).Scan(&graphID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: route %s", ErrCatalogNotFound, checkoutID)
		}
		return err
	}
	return validateDedicatedGraphAdmissionTx(ctx, tx, graphID)
}

// ListRepositoryCleanupGenerations returns every positive generation owned by
// this closed graph, newest first. No default query limit or log-and-empty
// fallback is permitted at the physical-deletion authorization boundary.
func (c *Catalog) ListRepositoryCleanupGenerations(ctx context.Context, graphID string) ([]int64, error) {
	var ids []int64
	err := c.withTx(ctx, func(tx *sql.Tx) error {
		var state string
		if err := tx.QueryRowContext(ctx, `SELECT state FROM dedicated_graphs WHERE graph_id = ?`, graphID).Scan(&state); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("%w: graph %s", ErrCatalogNotFound, graphID)
			}
			return err
		}
		if state != DedicatedGraphClosing {
			return fmt.Errorf("%w: graph %s is not closing", ErrCatalogStaleGuard, graphID)
		}
		rows, err := tx.QueryContext(ctx, `SELECT generation_id FROM view_generations WHERE graph_id = ? AND generation_id > 0 ORDER BY generation_id DESC`, graphID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				return err
			}
			ids = append(ids, id)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return ids, nil
}

// ListRepositoryCleanupOwners restores closing admission before startup work.
// This is a cleanup/control read; it neither opens nor reopens an owner.
func (c *Catalog) ListRepositoryCleanupOwners(ctx context.Context) ([]RepositoryCleanupIdentity, error) {
	var owners []RepositoryCleanupIdentity
	err := c.withTx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `
SELECT d.graph_id, COALESCE(d.owner_checkout_id, ''), COALESCE(c.incarnation, ''),
       d.repo_prefix, d.family_id, COALESCE(c.root_path, '')
  FROM dedicated_graphs d LEFT JOIN checkouts c ON c.checkout_id = d.owner_checkout_id
 WHERE d.state = ? ORDER BY d.graph_id`, DedicatedGraphClosing)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var owner RepositoryCleanupIdentity
			if err := rows.Scan(&owner.GraphID, &owner.CheckoutID, &owner.Incarnation, &owner.RepoPrefix, &owner.FamilyID, &owner.RootPath); err != nil {
				return err
			}
			if owner.CheckoutID == "" || owner.Incarnation == "" || owner.RepoPrefix == "" {
				return fmt.Errorf("%w: closing graph %s has no complete owner identity", ErrCatalogStaleGuard, owner.GraphID)
			}
			owners = append(owners, owner)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return owners, nil
}

// ReleaseRepositoryCleanupPublication removes the current publication root
// only after the lifecycle has joined every admitted publisher and reader.
// The closing graph and owner stay durable for subsequent retirement retries.
// It does not delete a generation, relax reference guards, or authorize cleanup.
func (c *Catalog) ReleaseRepositoryCleanupPublication(ctx context.Context, expected RepositoryCleanupIdentity) error {
	return c.withTx(ctx, func(tx *sql.Tx) error {
		identity, found, err := repositoryCleanupIdentityTx(ctx, tx, expected.GraphID)
		if err != nil {
			return err
		}
		if !found || identity.GraphID != expected.GraphID || identity.CheckoutID != expected.CheckoutID || identity.Incarnation != expected.Incarnation || identity.RepoPrefix != expected.RepoPrefix || identity.FamilyID != expected.FamilyID {
			return fmt.Errorf("%w: repository cleanup owner changed", ErrCatalogStaleGuard)
		}
		var state string
		if err := tx.QueryRowContext(ctx, `SELECT state FROM dedicated_graphs WHERE graph_id = ?`, expected.GraphID).Scan(&state); err != nil {
			return err
		}
		if state != DedicatedGraphClosing {
			return fmt.Errorf("%w: graph %s is not closing", ErrCatalogStaleGuard, expected.GraphID)
		}
		_, err = tx.ExecContext(ctx, `DELETE FROM dedicated_base_publications WHERE graph_id = ?`, expected.GraphID)
		return err
	})
}
