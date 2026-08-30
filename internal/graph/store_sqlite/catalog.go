package store_sqlite

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"strings"
	"time"

	sqlite "modernc.org/sqlite"

	"github.com/zzet/gortex/internal/pathkey"
	"github.com/zzet/gortex/internal/viewmetrics"
)

// Catalog is the accessor for the checkout-lifecycle control plane — the
// families, checkouts, tracking intents, dedicated graphs, view generations,
// routes, ref views and cleanup work described by checkoutCatalogSchemaSQL.
//
// It is a separate handle rather than another few dozen methods on Store
// because none of it is graph payload: no call here reads or writes nodes,
// edges, files or their sidecars, and none of it participates in the
// analysis-generation invalidation that every payload mutation must run.
//
// Writes take the store's mutation gate and run on the active writer
// connection, so they serialise with graph writes exactly like every other
// durable write in this package. Reads go to the read pool.
type Catalog struct {
	store *Store
}

// Catalog returns the control-plane accessor for this store. It is pinned to
// the base handle: none of it is payload, so a catalog write must go through
// even when the caller holds a handle on a generation that no longer accepts
// payload writes.
func (s *Store) Catalog() *Catalog { return &Catalog{store: s.atBase()} }

// exec runs one control-plane statement under the mutation gate. The gate is
// taken under the caller's context for the reason withTx gives: a deadline
// that bounds only the statement bounds nothing at all while the queue in
// front of it is a whole build.
func (c *Catalog) exec(ctx context.Context, query string, args ...any) (sql.Result, error) {
	if err := c.store.writeMu.LockContext(ctx); err != nil {
		return nil, err
	}
	defer c.store.writeMu.Unlock()
	return c.store.execActiveWriteLocked(ctx, query, args...)
}

// execGuarded runs one compare-and-set statement and reports a no-op as a
// stale guard. A lone UPDATE is already its own transaction in SQLite, so the
// read of the guard columns and the write cannot be interleaved.
func (c *Catalog) execGuarded(ctx context.Context, subject string, query string, args ...any) error {
	result, err := c.exec(ctx, query, args...)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed == 0 {
		return fmt.Errorf("%w: %s", ErrCatalogStaleGuard, subject)
	}
	return nil
}

// execGuardedTx is execGuarded inside an open transaction, for a transition
// whose compare-and-set has to stand or fall with the other rows it writes.
func execGuardedTx(ctx context.Context, tx *sql.Tx, subject string, query string, args ...any) error {
	result, err := tx.ExecContext(ctx, query, args...)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed == 0 {
		return fmt.Errorf("%w: %s", ErrCatalogStaleGuard, subject)
	}
	return nil
}

// deleteOne runs a delete addressed at a single row and reports a no-op as
// ErrCatalogNotFound, so a caller can tell "it was already gone" from "the
// statement failed" without inspecting driver errors.
func (c *Catalog) deleteOne(ctx context.Context, subject string, query string, args ...any) error {
	result, err := c.exec(ctx, query, args...)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed == 0 {
		return fmt.Errorf("%w: %s", ErrCatalogNotFound, subject)
	}
	return nil
}

// withTx runs a multi-statement guarded transition as one transaction under
// the mutation gate.
//
// The gate is taken under the caller's context rather than unconditionally.
// It is held for as long as the pass in front holds it — a build's
// transactions run for as long as the build does — so a write with a deadline
// has to be able to stop waiting for its turn. Without that, the deadline
// bounded only the transaction and not the queue in front of it, and a caller
// that budgeted two seconds waited out the whole pass.
func (c *Catalog) withTx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	if err := c.store.writeMu.LockContext(ctx); err != nil {
		return err
	}
	defer c.store.writeMu.Unlock()
	tx, err := c.store.beginWriteContext(ctx)
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	committed = true
	return nil
}

// catalogNullString stores the empty string as NULL, so "unset" and "set to
// empty" cannot be confused by a partial index or a uniqueness rule.
func catalogNullString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

// catalogNullInt stores 0 as NULL for the generation pointers, whose zero
// value means "no generation".
func catalogNullInt(value int64) any {
	if value == 0 {
		return nil
	}
	return value
}

func catalogBoolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

// --- repository families ----------------------------------------------

// UpsertRepositoryFamily writes one family row. It never uses INSERT OR
// REPLACE: REPLACE deletes the existing row first, which would cascade
// through every checkout that references it.
func (c *Catalog) UpsertRepositoryFamily(ctx context.Context, family RepositoryFamily) error {
	if err := family.validate(); err != nil {
		return err
	}
	_, err := c.exec(ctx, `
INSERT INTO repository_families
  (family_id, common_dir_identity, display_remote, state, primary_epoch, created_at, last_seen)
VALUES (?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(family_id) DO UPDATE SET
  common_dir_identity = excluded.common_dir_identity,
  display_remote      = excluded.display_remote,
  state               = excluded.state,
  primary_epoch       = excluded.primary_epoch,
  created_at          = excluded.created_at,
  last_seen           = excluded.last_seen`,
		family.FamilyID, family.CommonDirIdentity, family.DisplayRemote, family.State,
		family.PrimaryEpoch, family.CreatedAt, family.LastSeen)
	return err
}

// GetRepositoryFamily returns one family. The bool is false when no row exists.
func (c *Catalog) GetRepositoryFamily(ctx context.Context, familyID string) (RepositoryFamily, bool, error) {
	family := RepositoryFamily{FamilyID: familyID}
	err := c.store.db.QueryRowContext(ctx, `
SELECT common_dir_identity, display_remote, state, primary_epoch, created_at, last_seen
  FROM repository_families WHERE family_id = ?`, familyID).Scan(
		&family.CommonDirIdentity, &family.DisplayRemote, &family.State,
		&family.PrimaryEpoch, &family.CreatedAt, &family.LastSeen)
	if err == sql.ErrNoRows {
		return RepositoryFamily{}, false, nil
	}
	if err != nil {
		return RepositoryFamily{}, false, err
	}
	return family, true, nil
}

// ListRepositoryFamilies returns every family the catalog holds, ordered by
// family id so two passes over an unchanged catalog see the same order.
//
// It is the entry point for a whole-catalog read. Every other listing is keyed
// by a family, a graph or a checkout, so without this the only way to enumerate
// what the daemon knows is to re-derive the families from the tracked corpus —
// which misses exactly the ones whose roots have gone away.
func (c *Catalog) ListRepositoryFamilies(ctx context.Context) ([]RepositoryFamily, error) {
	rows, err := c.store.db.QueryContext(ctx, `
SELECT family_id, common_dir_identity, display_remote, state, primary_epoch, created_at, last_seen
  FROM repository_families ORDER BY family_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RepositoryFamily
	for rows.Next() {
		var family RepositoryFamily
		if err := rows.Scan(&family.FamilyID, &family.CommonDirIdentity, &family.DisplayRemote,
			&family.State, &family.PrimaryEpoch, &family.CreatedAt, &family.LastSeen); err != nil {
			return nil, err
		}
		out = append(out, family)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// DeleteRepositoryFamily removes a family. Checkouts and dedicated graphs
// reference it with ON DELETE RESTRICT, so SQLite refuses the delete until the
// family is empty — the row is the last thing a family teardown removes.
func (c *Catalog) DeleteRepositoryFamily(ctx context.Context, familyID string) error {
	if err := requireCatalogID("family_id", familyID); err != nil {
		return err
	}
	return c.deleteOne(ctx, fmt.Sprintf("family %s", familyID),
		`DELETE FROM repository_families WHERE family_id = ?`, familyID)
}

// --- checkouts ---------------------------------------------------------

const checkoutColumns = `incarnation, family_id, root_path, git_dir, admin_name, state,
	desired_mode, effective_mode, locked, prunable, head_ref, head_commit, head_tree,
	last_accessible, unavailable_since, availability_deadline, removal_detected_at,
	removal_deadline, removal_evidence, active_intent_transition_id, last_seen, last_error`

// UpsertCheckout writes one checkout row.
func (c *Catalog) UpsertCheckout(ctx context.Context, checkout Checkout) error {
	if err := checkout.validate(); err != nil {
		return err
	}
	_, err := c.exec(ctx, `
INSERT INTO checkouts (checkout_id, `+checkoutColumns+`)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(checkout_id) DO UPDATE SET
  incarnation                 = excluded.incarnation,
  family_id                   = excluded.family_id,
  root_path                   = excluded.root_path,
  git_dir                     = excluded.git_dir,
  admin_name                  = excluded.admin_name,
  state                       = excluded.state,
  desired_mode                = excluded.desired_mode,
  effective_mode              = excluded.effective_mode,
  locked                      = excluded.locked,
  prunable                    = excluded.prunable,
  head_ref                    = excluded.head_ref,
  head_commit                 = excluded.head_commit,
  head_tree                   = excluded.head_tree,
  last_accessible             = excluded.last_accessible,
  unavailable_since           = excluded.unavailable_since,
  availability_deadline       = excluded.availability_deadline,
  removal_detected_at         = excluded.removal_detected_at,
  removal_deadline            = excluded.removal_deadline,
  removal_evidence            = excluded.removal_evidence,
  active_intent_transition_id = excluded.active_intent_transition_id,
  last_seen                   = excluded.last_seen,
  last_error                  = excluded.last_error`,
		checkout.CheckoutID, checkout.Incarnation, checkout.FamilyID, checkout.RootPath,
		checkout.GitDir, checkout.AdminName, string(checkout.State),
		string(checkout.DesiredMode), string(checkout.EffectiveMode),
		catalogBoolInt(checkout.Locked), catalogBoolInt(checkout.Prunable),
		checkout.HeadRef, checkout.HeadCommit, checkout.HeadTree,
		checkout.LastAccessible, checkout.UnavailableSince, checkout.AvailabilityDeadline,
		checkout.RemovalDetectedAt, checkout.RemovalDeadline, checkout.RemovalEvidence,
		catalogNullString(checkout.ActiveIntentTransitionID),
		checkout.LastSeen, checkout.LastError)
	return err
}

// AllocateCheckout mints the identity for a working copy the catalog has never
// seen. Unlike UpsertCheckout it refuses to add a second live identity for a
// (family_id, admin_name) that already has one: the insert carries its own
// existence test, so two actors racing to allocate the same working copy end
// with one row, and the loser gets ErrCatalogStaleGuard.
//
// The table's UNIQUE key cannot serve as that backstop — it includes the
// incarnation precisely so a removed-and-recreated path can be re-keyed under
// the same name — and the test is written into the statement rather than run
// as a read before it, because a separate read would leave open the very
// window it is meant to close.
func (c *Catalog) AllocateCheckout(ctx context.Context, checkout Checkout) error {
	if err := checkout.validate(); err != nil {
		return err
	}
	// The guard is keyed on the administrative name, so an allocation without
	// one cannot be guarded at all.
	if err := requireCatalogID("admin_name", checkout.AdminName); err != nil {
		return err
	}
	subject := fmt.Sprintf("family %s already holds admin name %s", checkout.FamilyID, checkout.AdminName)
	return c.execGuarded(ctx, subject, `
INSERT INTO checkouts (checkout_id, `+checkoutColumns+`)
SELECT ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?
 WHERE NOT EXISTS (SELECT 1 FROM checkouts WHERE family_id = ? AND admin_name = ?)`,
		checkout.CheckoutID, checkout.Incarnation, checkout.FamilyID, checkout.RootPath,
		checkout.GitDir, checkout.AdminName, string(checkout.State),
		string(checkout.DesiredMode), string(checkout.EffectiveMode),
		catalogBoolInt(checkout.Locked), catalogBoolInt(checkout.Prunable),
		checkout.HeadRef, checkout.HeadCommit, checkout.HeadTree,
		checkout.LastAccessible, checkout.UnavailableSince, checkout.AvailabilityDeadline,
		checkout.RemovalDetectedAt, checkout.RemovalDeadline, checkout.RemovalEvidence,
		catalogNullString(checkout.ActiveIntentTransitionID),
		checkout.LastSeen, checkout.LastError,
		checkout.FamilyID, checkout.AdminName)
}

// scanCheckout reads the checkoutColumns projection in order.
func scanCheckout(scan func(...any) error, checkout *Checkout) error {
	var (
		state, desiredMode, effectiveMode string
		locked, prunable                  int
		activeTransition                  sql.NullString
	)
	if err := scan(
		&checkout.Incarnation, &checkout.FamilyID, &checkout.RootPath, &checkout.GitDir,
		&checkout.AdminName, &state, &desiredMode, &effectiveMode, &locked, &prunable,
		&checkout.HeadRef, &checkout.HeadCommit, &checkout.HeadTree,
		&checkout.LastAccessible, &checkout.UnavailableSince, &checkout.AvailabilityDeadline,
		&checkout.RemovalDetectedAt, &checkout.RemovalDeadline, &checkout.RemovalEvidence,
		&activeTransition, &checkout.LastSeen, &checkout.LastError); err != nil {
		return err
	}
	checkout.State = CheckoutState(state)
	checkout.DesiredMode = CheckoutMode(desiredMode)
	checkout.EffectiveMode = CheckoutMode(effectiveMode)
	checkout.Locked = locked != 0
	checkout.Prunable = prunable != 0
	checkout.ActiveIntentTransitionID = activeTransition.String
	return nil
}

// GetCheckout returns one checkout. The bool is false when no row exists.
func (c *Catalog) GetCheckout(ctx context.Context, checkoutID string) (Checkout, bool, error) {
	checkout := Checkout{CheckoutID: checkoutID}
	row := c.store.db.QueryRowContext(ctx, `SELECT `+checkoutColumns+` FROM checkouts WHERE checkout_id = ?`, checkoutID)
	err := scanCheckout(row.Scan, &checkout)
	if err == sql.ErrNoRows {
		return Checkout{}, false, nil
	}
	if err != nil {
		return Checkout{}, false, err
	}
	return checkout, true, nil
}

// ListCheckouts returns one family's checkouts. The scan rides the
// UNIQUE(family_id, admin_name, incarnation) index, so it is bounded by the
// family rather than by the table.
func (c *Catalog) ListCheckouts(ctx context.Context, familyID string) ([]Checkout, error) {
	rows, err := c.store.db.QueryContext(ctx, `
SELECT checkout_id, `+checkoutColumns+`
  FROM checkouts WHERE family_id = ? ORDER BY admin_name, incarnation`, familyID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Checkout
	for rows.Next() {
		var checkout Checkout
		err := scanCheckout(func(dest ...any) error {
			return rows.Scan(append([]any{&checkout.CheckoutID}, dest...)...)
		}, &checkout)
		if err != nil {
			return nil, err
		}
		out = append(out, checkout)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// UpdateCheckoutState is the guarded checkout-state transition: it applies
// only when the stored incarnation still matches the caller's expectation, so
// a write aimed at a working copy that has since been removed and recreated
// changes nothing and reports ErrCatalogStaleGuard.
func (c *Catalog) UpdateCheckoutState(ctx context.Context, req UpdateCheckoutStateRequest) error {
	if err := requireCatalogID("checkout_id", req.CheckoutID); err != nil {
		return err
	}
	if err := requireCatalogID("incarnation", req.Incarnation); err != nil {
		return err
	}
	if err := requireCatalogValue("state", req.State, checkoutStates); err != nil {
		return err
	}
	if err := requireCatalogValue("desired_mode", req.DesiredMode, checkoutModes); err != nil {
		return err
	}
	if err := requireCatalogValue("effective_mode", req.EffectiveMode, checkoutModes); err != nil {
		return err
	}
	return c.execGuarded(ctx, fmt.Sprintf("checkout %s incarnation %s", req.CheckoutID, req.Incarnation), `
UPDATE checkouts
   SET state = ?, desired_mode = ?, effective_mode = ?, last_seen = ?, last_error = ?
 WHERE checkout_id = ? AND incarnation = ?`,
		string(req.State), string(req.DesiredMode), string(req.EffectiveMode),
		req.LastSeen, req.LastError, req.CheckoutID, req.Incarnation)
}

// UpdateCheckoutObservation is the guarded write a reconciliation pass makes
// after looking at a checkout: it moves the state axis, both durable clock
// axes and the observed git / filesystem facts in one statement, under the
// same incarnation guard UpdateCheckoutState uses.
//
// It exists beside UpdateCheckoutState because the two answer different
// questions. UpdateCheckoutState is the mode-transition write and touches only
// what a promotion or demotion changes. This is the observation write, and the
// clocks have to land in the same statement as the state they justify: split
// across two statements, a crash between them leaves a state whose deadline
// says something else. The identity columns (checkout_id, incarnation,
// family_id, admin_name) are deliberately absent — an observation never
// re-keys the row it observed.
//
// The two mode columns are absent for a different reason. An observer reads
// what git and the filesystem say; it has nothing to say about how a checkout
// is served. Writing back the modes it happened to read would let a pass whose
// read predates a promotion revert it, because the incarnation guard does not
// move on a mode transition. The two writers touch disjoint columns instead,
// so neither can lose the other's update.
func (c *Catalog) UpdateCheckoutObservation(ctx context.Context, req UpdateCheckoutObservationRequest) error {
	if err := req.validate(); err != nil {
		return err
	}
	const update = `
UPDATE checkouts
   SET state = ?,
       root_path = ?, git_dir = ?, locked = ?, prunable = ?,
       head_ref = ?, head_commit = ?, head_tree = ?,
       last_accessible = ?, unavailable_since = ?, availability_deadline = ?,
       removal_detected_at = ?, removal_deadline = ?, removal_evidence = ?,
       last_seen = ?, last_error = ?
	WHERE checkout_id = ? AND incarnation = ? AND root_path = ?`
	rootMoved := !pathkey.EqualPaths(
		pathkey.CanonicalExistingRoot(req.RootPath),
		pathkey.CanonicalExistingRoot(req.ExpectedRootPath),
	)
	observedRoot := req.RootPath
	if !rootMoved {
		// Keep the catalog's established spelling for one physical root. An
		// alias-only observation must not make a pending move journal's exact
		// current-root guard impossible to complete.
		observedRoot = req.ExpectedRootPath
	}
	args := []any{
		string(req.State),
		observedRoot, req.GitDir, catalogBoolInt(req.Locked), catalogBoolInt(req.Prunable),
		req.HeadRef, req.HeadCommit, req.HeadTree,
		req.LastAccessible, req.UnavailableSince, req.AvailabilityDeadline,
		req.RemovalDetectedAt, req.RemovalDeadline, req.RemovalEvidence,
		req.LastSeen, req.LastError, req.CheckoutID, req.Incarnation, req.ExpectedRootPath,
	}
	subject := fmt.Sprintf("checkout %s incarnation %s root %s",
		req.CheckoutID, req.Incarnation, req.ExpectedRootPath)
	if !rootMoved {
		return c.execGuarded(ctx, subject, update, args...)
	}

	// A changed root and its recovery marker are one commit. Preserving the
	// earliest displaced root across A -> B -> C lets a restart repair config
	// still at A. Active path-bearing intents retain any partially advanced
	// address, while current_root_path keeps stale completion from deleting the
	// marker for C.
	return c.withTx(ctx, func(tx *sql.Tx) error {
		if err := execGuardedTx(ctx, tx, subject, update, args...); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `
INSERT INTO checkout_root_moves
  (checkout_id, incarnation, previous_root_path, latest_previous_root_path,
	 config_root_path, config_prepared_from_path, config_prepared_to_path,
	 config_prepared_before_hash, config_prepared_after_hash,
	 current_root_path, observed_at)
VALUES (?, ?, ?, ?, ?, '', '', '', '', ?, ?)
ON CONFLICT(checkout_id) DO UPDATE SET
  incarnation = excluded.incarnation,
  previous_root_path = CASE
    WHEN checkout_root_moves.incarnation = excluded.incarnation
      THEN checkout_root_moves.previous_root_path
    ELSE excluded.previous_root_path
  END,
	latest_previous_root_path = excluded.latest_previous_root_path,
	config_root_path = CASE
	  WHEN checkout_root_moves.incarnation = excluded.incarnation
	    THEN checkout_root_moves.config_root_path
	  ELSE excluded.config_root_path
	END,
	config_prepared_from_path = CASE
	  WHEN checkout_root_moves.incarnation = excluded.incarnation
	    THEN checkout_root_moves.config_prepared_from_path
	  ELSE ''
	END,
	config_prepared_to_path = CASE
	  WHEN checkout_root_moves.incarnation = excluded.incarnation
	    THEN checkout_root_moves.config_prepared_to_path
	  ELSE ''
	END,
	config_prepared_before_hash = CASE
	  WHEN checkout_root_moves.incarnation = excluded.incarnation
	    THEN checkout_root_moves.config_prepared_before_hash
	  ELSE ''
	END,
	config_prepared_after_hash = CASE
	  WHEN checkout_root_moves.incarnation = excluded.incarnation
	    THEN checkout_root_moves.config_prepared_after_hash
	  ELSE ''
	END,
  current_root_path = excluded.current_root_path,
  observed_at = excluded.observed_at`,
			req.CheckoutID, req.Incarnation, req.ExpectedRootPath,
			req.ExpectedRootPath, req.ExpectedRootPath, req.RootPath, req.LastSeen)
		return err
	})
}

// GetCheckoutRootMove returns the uncompleted move marker for one checkout.
func (c *Catalog) GetCheckoutRootMove(
	ctx context.Context,
	checkoutID string,
) (CheckoutRootMove, bool, error) {
	move := CheckoutRootMove{CheckoutID: checkoutID}
	err := c.store.db.QueryRowContext(ctx, `
SELECT incarnation, previous_root_path, latest_previous_root_path, config_root_path,
       config_prepared_from_path, config_prepared_to_path,
       config_prepared_before_hash, config_prepared_after_hash,
       current_root_path, observed_at
  FROM checkout_root_moves WHERE checkout_id = ?`, checkoutID).Scan(
		&move.Incarnation, &move.PreviousRootPath, &move.LatestPreviousRootPath,
		&move.ConfigRootPath, &move.ConfigPreparedFromPath, &move.ConfigPreparedToPath,
		&move.ConfigPreparedBeforeHash, &move.ConfigPreparedAfterHash,
		&move.CurrentRootPath, &move.ObservedAt)
	if err == sql.ErrNoRows {
		return CheckoutRootMove{}, false, nil
	}
	if err != nil {
		return CheckoutRootMove{}, false, err
	}
	return move, true, nil
}

// ListCheckoutRootMoves returns every uncompleted root relocation. There is
// at most one row per checkout, so startup recovery is bounded by the number
// of live checkouts and never scans graph payload.
func (c *Catalog) ListCheckoutRootMoves(ctx context.Context) ([]CheckoutRootMove, error) {
	rows, err := c.store.db.QueryContext(ctx, `
SELECT checkout_id, incarnation, previous_root_path, latest_previous_root_path,
	   config_root_path, config_prepared_from_path, config_prepared_to_path,
	   config_prepared_before_hash, config_prepared_after_hash,
       current_root_path, observed_at
  FROM checkout_root_moves ORDER BY checkout_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []CheckoutRootMove
	for rows.Next() {
		var move CheckoutRootMove
		if err := rows.Scan(
			&move.CheckoutID, &move.Incarnation, &move.PreviousRootPath,
			&move.LatestPreviousRootPath, &move.ConfigRootPath,
			&move.ConfigPreparedFromPath, &move.ConfigPreparedToPath,
			&move.ConfigPreparedBeforeHash, &move.ConfigPreparedAfterHash,
			&move.CurrentRootPath, &move.ObservedAt,
		); err != nil {
			return nil, err
		}
		out = append(out, move)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// PrepareCheckoutRootMoveConfig publishes the exact cross-store transition
// before the YAML file is atomically replaced. A crash can therefore prove
// that config at the target belongs to this checkout rather than an unrelated
// entry which happened to occupy the same root. An unresolved earlier prepare
// must be acknowledged or cleared before a newer target can replace it.
func (c *Catalog) PrepareCheckoutRootMoveConfig(
	ctx context.Context,
	checkoutID, incarnation, expectedConfigRoot, targetRoot,
	beforeHash, afterHash string,
) error {
	if err := requireCatalogID("checkout_id", checkoutID); err != nil {
		return err
	}
	if err := requireCatalogID("incarnation", incarnation); err != nil {
		return err
	}
	if expectedConfigRoot == "" || targetRoot == "" ||
		beforeHash == "" || afterHash == "" {
		return fmt.Errorf("%w: prepared config root paths are required", ErrCatalogInvalidValue)
	}
	return c.execGuarded(ctx, fmt.Sprintf("checkout %s prepare root move config", checkoutID), `
UPDATE checkout_root_moves
   SET config_prepared_from_path = ?, config_prepared_to_path = ?,
       config_prepared_before_hash = ?, config_prepared_after_hash = ?
 WHERE checkout_id = ? AND incarnation = ?
   AND config_root_path = ? AND current_root_path = ?
	   AND (config_prepared_from_path = '' OR
	        (config_prepared_from_path = ? AND config_prepared_to_path = ?
	         AND config_prepared_before_hash = ? AND config_prepared_after_hash = ?))`,
		expectedConfigRoot, targetRoot, beforeHash, afterHash,
		checkoutID, incarnation, expectedConfigRoot, targetRoot,
		expectedConfigRoot, targetRoot, beforeHash, afterHash)
}

// ClearCheckoutRootMoveConfigPreparation records that inspection found the
// atomic replacement did not happen. The exact prepared tuple is a CAS, so a
// delayed recovery cannot erase a later transition.
func (c *Catalog) ClearCheckoutRootMoveConfigPreparation(
	ctx context.Context,
	checkoutID, incarnation, expectedConfigRoot, preparedTarget,
	beforeHash, afterHash string,
) error {
	if err := requireCatalogID("checkout_id", checkoutID); err != nil {
		return err
	}
	if err := requireCatalogID("incarnation", incarnation); err != nil {
		return err
	}
	if expectedConfigRoot == "" || preparedTarget == "" ||
		beforeHash == "" || afterHash == "" {
		return fmt.Errorf("%w: prepared config root paths are required", ErrCatalogInvalidValue)
	}
	return c.execGuarded(ctx, fmt.Sprintf("checkout %s clear root move config prepare", checkoutID), `
UPDATE checkout_root_moves
   SET config_prepared_from_path = '', config_prepared_to_path = '',
       config_prepared_before_hash = '', config_prepared_after_hash = ''
 WHERE checkout_id = ? AND incarnation = ? AND config_root_path = ?
	   AND config_prepared_from_path = ? AND config_prepared_to_path = ?
	   AND config_prepared_before_hash = ? AND config_prepared_after_hash = ?`,
		checkoutID, incarnation, expectedConfigRoot,
		expectedConfigRoot, preparedTarget, beforeHash, afterHash)
}

// AcknowledgeCheckoutRootMoveConfig records the exact root an atomic config
// save published and clears its prepared transition. It deliberately does not
// guard current_root_path: a later filesystem observation may advance B -> C
// while the A -> B save is waiting to acknowledge B. The prepared tuple keeps
// that delayed acknowledgement from overwriting newer config ownership.
func (c *Catalog) AcknowledgeCheckoutRootMoveConfig(
	ctx context.Context,
	checkoutID, incarnation, expectedConfigRoot, savedConfigRoot,
	beforeHash, afterHash string,
) error {
	if err := requireCatalogID("checkout_id", checkoutID); err != nil {
		return err
	}
	if err := requireCatalogID("incarnation", incarnation); err != nil {
		return err
	}
	if expectedConfigRoot == "" || savedConfigRoot == "" ||
		beforeHash == "" || afterHash == "" {
		return fmt.Errorf("%w: config root paths are required", ErrCatalogInvalidValue)
	}
	return c.execGuarded(ctx, fmt.Sprintf("checkout %s root move config", checkoutID), `
UPDATE checkout_root_moves
   SET config_root_path = ?,
       config_prepared_from_path = '', config_prepared_to_path = '',
       config_prepared_before_hash = '', config_prepared_after_hash = ''
 WHERE checkout_id = ? AND incarnation = ? AND config_root_path = ?
	   AND config_prepared_from_path = ? AND config_prepared_to_path = ?
	   AND config_prepared_before_hash = ? AND config_prepared_after_hash = ?`,
		savedConfigRoot, checkoutID, incarnation, expectedConfigRoot,
		expectedConfigRoot, savedConfigRoot, beforeHash, afterHash)
}

// CompleteCheckoutRootMove clears only the exact marker whose current root is
// still the checkout's durable root. Runtime convergence of B cannot clear a
// newer B -> C marker even if it finishes later.
func (c *Catalog) CompleteCheckoutRootMove(
	ctx context.Context,
	move CheckoutRootMove,
) error {
	if err := requireCatalogID("checkout_id", move.CheckoutID); err != nil {
		return err
	}
	if err := requireCatalogID("incarnation", move.Incarnation); err != nil {
		return err
	}
	if move.PreviousRootPath == "" || move.LatestPreviousRootPath == "" ||
		move.ConfigRootPath == "" ||
		move.CurrentRootPath == "" {
		return fmt.Errorf("%w: checkout root move paths are required", ErrCatalogInvalidValue)
	}
	if move.ConfigPreparedFromPath != "" || move.ConfigPreparedToPath != "" ||
		move.ConfigPreparedBeforeHash != "" || move.ConfigPreparedAfterHash != "" {
		return fmt.Errorf("%w: checkout root move config is still prepared", ErrCatalogInvalidValue)
	}
	return c.withTx(ctx, func(tx *sql.Tx) error {
		var currentRoot, effectiveMode string
		err := tx.QueryRowContext(ctx, `
SELECT root_path, effective_mode FROM checkouts
 WHERE checkout_id = ? AND incarnation = ?`,
			move.CheckoutID, move.Incarnation).Scan(&currentRoot, &effectiveMode)
		if err == sql.ErrNoRows {
			return fmt.Errorf("%w: checkout %s root move completion",
				ErrCatalogStaleGuard, move.CheckoutID)
		}
		if err != nil {
			return err
		}
		if currentRoot != move.CurrentRootPath {
			return fmt.Errorf("%w: checkout %s root move completion",
				ErrCatalogStaleGuard, move.CheckoutID)
		}
		if CheckoutMode(effectiveMode) == CheckoutModeDedicated && !pathkey.EqualPaths(
			pathkey.CanonicalExistingRoot(move.ConfigRootPath),
			pathkey.CanonicalExistingRoot(move.CurrentRootPath),
		) {
			return fmt.Errorf("%w: dedicated checkout %s config root %s is not current %s",
				ErrCatalogStaleGuard, move.CheckoutID,
				move.ConfigRootPath, move.CurrentRootPath)
		}
		return execGuardedTx(ctx, tx, fmt.Sprintf("checkout %s root move", move.CheckoutID), `
DELETE FROM checkout_root_moves
 WHERE checkout_id = ? AND incarnation = ?
	 AND previous_root_path = ? AND latest_previous_root_path = ?
	 AND config_root_path = ?
	 AND config_prepared_from_path = ? AND config_prepared_to_path = ?
	 AND config_prepared_before_hash = ? AND config_prepared_after_hash = ?
	 AND current_root_path = ?`,
			move.CheckoutID, move.Incarnation, move.PreviousRootPath,
			move.LatestPreviousRootPath, move.ConfigRootPath,
			move.ConfigPreparedFromPath, move.ConfigPreparedToPath,
			move.ConfigPreparedBeforeHash, move.ConfigPreparedAfterHash,
			move.CurrentRootPath)
	})
}

// DeleteCheckout removes a checkout. Its tracking intents, in-flight intent
// transition and path evidence go with it through ON DELETE CASCADE; a route
// does not cascade, so a routed checkout must be un-routed first and SQLite
// refuses the delete until then.
func (c *Catalog) DeleteCheckout(ctx context.Context, checkoutID string) error {
	if err := requireCatalogID("checkout_id", checkoutID); err != nil {
		return err
	}
	return c.deleteOne(ctx, fmt.Sprintf("checkout %s", checkoutID),
		`DELETE FROM checkouts WHERE checkout_id = ?`, checkoutID)
}

// DeleteCheckoutForIncarnation removes a checkout only while its durable
// identity still names the expected incarnation. The bool reports whether the
// expected row was deleted; an absent or replacement row is a safe stale no-op.
func (c *Catalog) DeleteCheckoutForIncarnation(
	ctx context.Context, checkoutID, incarnation string,
) (bool, error) {
	if err := requireCatalogID("checkout_id", checkoutID); err != nil {
		return false, err
	}
	if err := requireCatalogID("incarnation", incarnation); err != nil {
		return false, err
	}
	result, err := c.exec(ctx,
		`DELETE FROM checkouts WHERE checkout_id = ? AND incarnation = ?`,
		checkoutID, incarnation)
	if err != nil {
		return false, err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return changed != 0, nil
}

// --- tracking intents --------------------------------------------------

// UpsertTrackingIntent writes one tracking intent. A repeated request from the
// same source for the same checkout updates the existing row rather than
// adding a duplicate.
func (c *Catalog) UpsertTrackingIntent(ctx context.Context, intent TrackingIntent) error {
	if err := intent.validate(); err != nil {
		return err
	}
	_, err := c.exec(ctx, `
INSERT INTO tracking_intents
  (intent_id, checkout_id, source_kind, source_locator, active, created_at, revoked_at, last_error)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(checkout_id, source_kind, source_locator) DO UPDATE SET
  active     = excluded.active,
  revoked_at = excluded.revoked_at,
  last_error = excluded.last_error`,
		intent.IntentID, intent.CheckoutID, string(intent.SourceKind), intent.SourceLocator,
		catalogBoolInt(intent.Active), intent.CreatedAt, intent.RevokedAt, intent.LastError)
	return err
}

// ListTrackingIntents returns one checkout's intents, riding the
// UNIQUE(checkout_id, source_kind, source_locator) index.
func (c *Catalog) ListTrackingIntents(ctx context.Context, checkoutID string) ([]TrackingIntent, error) {
	rows, err := c.store.db.QueryContext(ctx, `
SELECT intent_id, source_kind, source_locator, active, created_at, revoked_at, last_error
  FROM tracking_intents WHERE checkout_id = ? ORDER BY source_kind, source_locator`, checkoutID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TrackingIntent
	for rows.Next() {
		intent := TrackingIntent{CheckoutID: checkoutID}
		var (
			sourceKind string
			active     int
		)
		if err := rows.Scan(&intent.IntentID, &sourceKind, &intent.SourceLocator, &active,
			&intent.CreatedAt, &intent.RevokedAt, &intent.LastError); err != nil {
			return nil, err
		}
		intent.SourceKind = IntentSourceKind(sourceKind)
		intent.Active = active != 0
		out = append(out, intent)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// RelocateActiveTrackingIntentLocators moves the path-bearing intent sources
// to a checkout's current root. Project membership locators are logical names
// ("project:<name>"), so they deliberately stay unchanged. The checkout root
// and incarnation are checked in the same write transaction as the intent
// updates: an A -> B repair racing a later B -> C observation cannot stamp B
// back into the newer checkout's intent rows.
//
// If repeated track calls already left an intent at currentRoot, the older
// locator is merged into that row rather than tripping the catalog's unique
// (checkout, source kind, locator) key. The returned count is the number of
// stale locator rows updated or removed.
func (c *Catalog) RelocateActiveTrackingIntentLocators(
	ctx context.Context,
	checkoutID, incarnation, currentRoot string,
) (int, error) {
	if err := requireCatalogID("checkout_id", checkoutID); err != nil {
		return 0, err
	}
	if err := requireCatalogID("incarnation", incarnation); err != nil {
		return 0, err
	}
	if currentRoot == "" {
		return 0, fmt.Errorf("%w: current_root is required", ErrCatalogInvalidValue)
	}

	moved := 0
	err := c.withTx(ctx, func(tx *sql.Tx) error {
		var guarded int
		err := tx.QueryRowContext(ctx, `
SELECT 1 FROM checkouts
 WHERE checkout_id = ? AND incarnation = ? AND root_path = ?`,
			checkoutID, incarnation, currentRoot).Scan(&guarded)
		if err == sql.ErrNoRows {
			return fmt.Errorf("%w: checkout %s incarnation %s root %s",
				ErrCatalogStaleGuard, checkoutID, incarnation, currentRoot)
		}
		if err != nil {
			return err
		}

		rows, err := tx.QueryContext(ctx, `
SELECT intent_id, source_kind, source_locator
  FROM tracking_intents
 WHERE checkout_id = ? AND active = 1
   AND source_kind IN (?, ?, ?)
 ORDER BY source_kind, source_locator`,
			checkoutID, string(IntentSourceCLITrack), string(IntentSourceMCPTrack),
			string(IntentSourceManualConfig))
		if err != nil {
			return err
		}
		type staleLocator struct {
			id, kind, locator string
		}
		var stale []staleLocator
		for rows.Next() {
			var row staleLocator
			if err := rows.Scan(&row.id, &row.kind, &row.locator); err != nil {
				_ = rows.Close()
				return err
			}
			if row.locator != currentRoot {
				stale = append(stale, row)
			}
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return err
		}
		if err := rows.Close(); err != nil {
			return err
		}

		for _, row := range stale {
			var (
				targetID     string
				targetActive int
			)
			err := tx.QueryRowContext(ctx, `
SELECT intent_id, active FROM tracking_intents
 WHERE checkout_id = ? AND source_kind = ? AND source_locator = ?`,
				checkoutID, row.kind, currentRoot).Scan(&targetID, &targetActive)
			switch {
			case err == nil:
				// A revoked historical row at the target is not ownership. Make
				// it the active union member before retiring the stale source so
				// relocation can never silently drop the explicit intent.
				if targetActive == 0 {
					if _, err := tx.ExecContext(ctx, `
UPDATE tracking_intents
   SET active = 1, revoked_at = 0, last_error = ''
 WHERE intent_id = ?`, targetID); err != nil {
						return err
					}
				}
				if _, err := tx.ExecContext(ctx,
					`DELETE FROM tracking_intents WHERE intent_id = ?`, row.id); err != nil {
					return err
				}
			case err == sql.ErrNoRows:
				if _, err := tx.ExecContext(ctx, `
UPDATE tracking_intents SET source_locator = ? WHERE intent_id = ?`,
					currentRoot, row.id); err != nil {
					return err
				}
			default:
				return err
			}
			moved++
		}
		return nil
	})
	return moved, err
}

// RevokeTrackingIntents atomically withdraws every active intent when, and
// only when, all of them belong to one of the caller-approved source kinds.
// The preflight read and the update share the catalog write transaction, so a
// cancellation or a non-revocable source cannot leave a partially revoked
// checkout.
func (c *Catalog) RevokeTrackingIntents(
	ctx context.Context,
	checkoutID string,
	revokedAt int64,
	revocableKinds []IntentSourceKind,
) (revoked, blocked []TrackingIntent, err error) {
	if checkoutID == "" {
		return nil, nil, fmt.Errorf("%w: checkout_id is required", ErrCatalogInvalidValue)
	}
	allowed := make(map[IntentSourceKind]struct{}, len(revocableKinds))
	for _, kind := range revocableKinds {
		if err := requireCatalogValue("source_kind", kind, intentSourceKinds); err != nil {
			return nil, nil, err
		}
		allowed[kind] = struct{}{}
	}

	var candidates []TrackingIntent
	err = c.withTx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `
SELECT intent_id, source_kind, source_locator, active, created_at, revoked_at, last_error
  FROM tracking_intents WHERE checkout_id = ? ORDER BY source_kind, source_locator`, checkoutID)
		if err != nil {
			return err
		}
		for rows.Next() {
			intent := TrackingIntent{CheckoutID: checkoutID}
			var (
				sourceKind string
				active     int
			)
			if err := rows.Scan(&intent.IntentID, &sourceKind, &intent.SourceLocator, &active,
				&intent.CreatedAt, &intent.RevokedAt, &intent.LastError); err != nil {
				_ = rows.Close()
				return err
			}
			intent.SourceKind = IntentSourceKind(sourceKind)
			intent.Active = active != 0
			if !intent.Active {
				continue
			}
			if _, ok := allowed[intent.SourceKind]; !ok {
				blocked = append(blocked, intent)
				continue
			}
			candidates = append(candidates, intent)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return err
		}
		if err := rows.Close(); err != nil {
			return err
		}
		if len(blocked) != 0 || len(candidates) == 0 {
			return nil
		}
		result, err := tx.ExecContext(ctx, `
UPDATE tracking_intents SET active = 0, revoked_at = ?
 WHERE checkout_id = ? AND active = 1`, revokedAt, checkoutID)
		if err != nil {
			return err
		}
		changed, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if changed != int64(len(candidates)) {
			return ErrCatalogStaleGuard
		}
		for i := range candidates {
			candidates[i].Active = false
			candidates[i].RevokedAt = revokedAt
		}
		revoked = candidates
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	if len(blocked) != 0 {
		return nil, blocked, nil
	}
	return revoked, nil, nil
}

// --- intent transitions ------------------------------------------------

// BeginIntentTransition records the single in-flight mode change for a
// checkout and points the checkout row at it, in one transaction. A checkout
// that already has one reports ErrCatalogIntentTransitionActive and nothing is
// written — UNIQUE(checkout_id) is the enforcement, the pre-check only turns
// it into a typed error.
func (c *Catalog) BeginIntentTransition(ctx context.Context, transition IntentTransition) error {
	if err := transition.validate(); err != nil {
		return err
	}
	return c.withTx(ctx, func(tx *sql.Tx) error {
		var existing string
		err := tx.QueryRowContext(ctx,
			`SELECT transition_id FROM intent_transitions WHERE checkout_id = ?`,
			transition.CheckoutID).Scan(&existing)
		switch {
		case err == nil:
			return fmt.Errorf("%w: checkout %s holds transition %s",
				ErrCatalogIntentTransitionActive, transition.CheckoutID, existing)
		case err != sql.ErrNoRows:
			return err
		}
		if _, err := tx.ExecContext(ctx, `
INSERT INTO intent_transitions
  (transition_id, checkout_id, cause, prior_desired_mode, prior_effective_mode,
   requested_mode, prior_checkout_state, source_snapshot_hash, state,
   created_at, last_progress, last_error)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			transition.TransitionID, transition.CheckoutID, transition.Cause,
			catalogNullString(string(transition.PriorDesiredMode)),
			catalogNullString(string(transition.PriorEffectiveMode)),
			catalogNullString(string(transition.RequestedMode)),
			catalogNullString(string(transition.PriorCheckoutState)),
			catalogNullString(transition.SourceSnapshotHash),
			string(transition.State), transition.CreatedAt,
			transition.LastProgress, transition.LastError); err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx,
			`UPDATE checkouts SET active_intent_transition_id = ? WHERE checkout_id = ?`,
			transition.TransitionID, transition.CheckoutID)
		if err != nil {
			return err
		}
		changed, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if changed == 0 {
			return fmt.Errorf("%w: checkout %s", ErrCatalogNotFound, transition.CheckoutID)
		}
		return nil
	})
}

// GetIntentTransition returns a checkout's in-flight transition. The bool is
// false when the checkout has none.
func (c *Catalog) GetIntentTransition(ctx context.Context, checkoutID string) (IntentTransition, bool, error) {
	transition := IntentTransition{CheckoutID: checkoutID}
	var (
		priorDesired, priorEffective, requested sql.NullString
		priorState, snapshotHash                sql.NullString
		state                                   string
	)
	err := c.store.db.QueryRowContext(ctx, `
SELECT transition_id, cause, prior_desired_mode, prior_effective_mode, requested_mode,
       prior_checkout_state, source_snapshot_hash, state, created_at, last_progress, last_error
  FROM intent_transitions WHERE checkout_id = ?`, checkoutID).Scan(
		&transition.TransitionID, &transition.Cause, &priorDesired, &priorEffective,
		&requested, &priorState, &snapshotHash, &state,
		&transition.CreatedAt, &transition.LastProgress, &transition.LastError)
	if err == sql.ErrNoRows {
		return IntentTransition{}, false, nil
	}
	if err != nil {
		return IntentTransition{}, false, err
	}
	transition.PriorDesiredMode = CheckoutMode(priorDesired.String)
	transition.PriorEffectiveMode = CheckoutMode(priorEffective.String)
	transition.RequestedMode = CheckoutMode(requested.String)
	transition.PriorCheckoutState = CheckoutState(priorState.String)
	transition.SourceSnapshotHash = snapshotHash.String
	transition.State = IntentTransitionState(state)
	return transition, true, nil
}

// UpdateIntentTransitionProgress records how far an in-flight mode change
// got. Both ids guard the write, so a caller holding a stale transition id
// cannot stamp progress onto the transition that replaced its own.
//
// A transition that stays pending is one a retry may adopt; the error it
// carries is why the last attempt stopped, not a terminal verdict.
func (c *Catalog) UpdateIntentTransitionProgress(
	ctx context.Context,
	checkoutID, transitionID string,
	state IntentTransitionState,
	lastError string,
	lastProgress int64,
) error {
	if err := requireCatalogID("checkout_id", checkoutID); err != nil {
		return err
	}
	if err := requireCatalogID("transition_id", transitionID); err != nil {
		return err
	}
	if err := requireCatalogValue("state", state, intentTransitionStates); err != nil {
		return err
	}
	return c.execGuarded(ctx, fmt.Sprintf("transition %s on checkout %s", transitionID, checkoutID), `
UPDATE intent_transitions
   SET state = ?, last_error = ?, last_progress = ?
 WHERE transition_id = ? AND checkout_id = ?`,
		string(state), lastError, lastProgress, transitionID, checkoutID)
}

// CompleteIntentTransition releases the transition slot: it deletes the row
// and clears the checkout's pointer in one transaction. The delete is guarded
// by both ids, so a caller holding a stale transition id cannot release a
// transition that replaced its own.
func (c *Catalog) CompleteIntentTransition(ctx context.Context, checkoutID, transitionID string) error {
	if err := requireCatalogID("checkout_id", checkoutID); err != nil {
		return err
	}
	if err := requireCatalogID("transition_id", transitionID); err != nil {
		return err
	}
	return c.withTx(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx,
			`DELETE FROM intent_transitions WHERE transition_id = ? AND checkout_id = ?`,
			transitionID, checkoutID)
		if err != nil {
			return err
		}
		changed, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if changed == 0 {
			return fmt.Errorf("%w: transition %s on checkout %s", ErrCatalogStaleGuard, transitionID, checkoutID)
		}
		_, err = tx.ExecContext(ctx, `
UPDATE checkouts SET active_intent_transition_id = NULL
 WHERE checkout_id = ? AND active_intent_transition_id = ?`, checkoutID, transitionID)
		return err
	})
}

// --- checkout path evidence --------------------------------------------

// UpsertCheckoutPathEvidence replaces a checkout's filesystem sample.
func (c *Catalog) UpsertCheckoutPathEvidence(ctx context.Context, evidence CheckoutPathEvidence) error {
	if err := requireCatalogID("checkout_id", evidence.CheckoutID); err != nil {
		return err
	}
	_, err := c.exec(ctx, `
INSERT INTO checkout_path_evidence
  (checkout_id, root_path_identity, root_volume_kind, root_volume_token,
   nearest_existing_ancestor_path, ancestor_volume_kind, ancestor_volume_token,
   common_dir_volume_kind, common_dir_volume_token, sampled_at, sample_generation)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(checkout_id) DO UPDATE SET
  root_path_identity             = excluded.root_path_identity,
  root_volume_kind               = excluded.root_volume_kind,
  root_volume_token              = excluded.root_volume_token,
  nearest_existing_ancestor_path = excluded.nearest_existing_ancestor_path,
  ancestor_volume_kind           = excluded.ancestor_volume_kind,
  ancestor_volume_token          = excluded.ancestor_volume_token,
  common_dir_volume_kind         = excluded.common_dir_volume_kind,
  common_dir_volume_token        = excluded.common_dir_volume_token,
  sampled_at                     = excluded.sampled_at,
  sample_generation              = excluded.sample_generation`,
		evidence.CheckoutID, evidence.RootPathIdentity, evidence.RootVolumeKind,
		evidence.RootVolumeToken, evidence.NearestExistingAncestorPath,
		evidence.AncestorVolumeKind, evidence.AncestorVolumeToken,
		evidence.CommonDirVolumeKind, evidence.CommonDirVolumeToken,
		evidence.SampledAt, evidence.SampleGeneration)
	return err
}

// GetCheckoutPathEvidence returns a checkout's last filesystem sample.
func (c *Catalog) GetCheckoutPathEvidence(ctx context.Context, checkoutID string) (CheckoutPathEvidence, bool, error) {
	evidence := CheckoutPathEvidence{CheckoutID: checkoutID}
	err := c.store.db.QueryRowContext(ctx, `
SELECT root_path_identity, root_volume_kind, root_volume_token,
       nearest_existing_ancestor_path, ancestor_volume_kind, ancestor_volume_token,
       common_dir_volume_kind, common_dir_volume_token, sampled_at, sample_generation
  FROM checkout_path_evidence WHERE checkout_id = ?`, checkoutID).Scan(
		&evidence.RootPathIdentity, &evidence.RootVolumeKind, &evidence.RootVolumeToken,
		&evidence.NearestExistingAncestorPath, &evidence.AncestorVolumeKind,
		&evidence.AncestorVolumeToken, &evidence.CommonDirVolumeKind,
		&evidence.CommonDirVolumeToken, &evidence.SampledAt, &evidence.SampleGeneration)
	if err == sql.ErrNoRows {
		return CheckoutPathEvidence{}, false, nil
	}
	if err != nil {
		return CheckoutPathEvidence{}, false, err
	}
	return evidence, true, nil
}

// --- dedicated graphs ---------------------------------------------------

// UpsertDedicatedGraph writes one dedicated-graph row. Setting IsPrimaryBase
// here is only legal while no other graph in the family holds it — the partial
// unique index refuses a second one. Moving the flag between graphs is
// SetPrimaryDedicatedGraph's job, which clears the incumbent first.
func (c *Catalog) UpsertDedicatedGraph(ctx context.Context, dedicated DedicatedGraph) error {
	if err := dedicated.validate(); err != nil {
		return err
	}
	_, err := c.exec(ctx, `
INSERT INTO dedicated_graphs
  (graph_id, owner_checkout_id, repo_prefix, family_id, is_primary_base, active_generation_id, state)
VALUES (?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(graph_id) DO UPDATE SET
  owner_checkout_id    = excluded.owner_checkout_id,
  repo_prefix          = excluded.repo_prefix,
  family_id            = excluded.family_id,
  is_primary_base      = excluded.is_primary_base,
  active_generation_id = excluded.active_generation_id,
  state                = excluded.state`,
		dedicated.GraphID, catalogNullString(dedicated.OwnerCheckoutID), dedicated.RepoPrefix,
		dedicated.FamilyID, catalogBoolInt(dedicated.IsPrimaryBase),
		catalogNullInt(dedicated.ActiveGenerationID), dedicated.State)
	return err
}

// GetDedicatedGraph returns one dedicated graph.
func (c *Catalog) GetDedicatedGraph(ctx context.Context, graphID string) (DedicatedGraph, bool, error) {
	dedicated := DedicatedGraph{GraphID: graphID}
	var (
		owner         sql.NullString
		activeGen     sql.NullInt64
		isPrimaryBase int
	)
	err := c.store.db.QueryRowContext(ctx, `
SELECT owner_checkout_id, repo_prefix, family_id, is_primary_base, active_generation_id, state
  FROM dedicated_graphs WHERE graph_id = ?`, graphID).Scan(
		&owner, &dedicated.RepoPrefix, &dedicated.FamilyID, &isPrimaryBase,
		&activeGen, &dedicated.State)
	if err == sql.ErrNoRows {
		return DedicatedGraph{}, false, nil
	}
	if err != nil {
		return DedicatedGraph{}, false, err
	}
	dedicated.OwnerCheckoutID = owner.String
	dedicated.ActiveGenerationID = activeGen.Int64
	dedicated.IsPrimaryBase = isPrimaryBase != 0
	return dedicated, true, nil
}

// GetDedicatedGraphByOwner returns the graph bound to one checkout owner.
// owner_checkout_id is unique, so it is the stable lookup when a restart or
// reload derives a different presentation prefix for an existing binding.
func (c *Catalog) GetDedicatedGraphByOwner(
	ctx context.Context, ownerCheckoutID string,
) (DedicatedGraph, bool, error) {
	if err := requireCatalogID("owner checkout", ownerCheckoutID); err != nil {
		return DedicatedGraph{}, false, err
	}
	dedicated := DedicatedGraph{OwnerCheckoutID: ownerCheckoutID}
	var (
		activeGen     sql.NullInt64
		isPrimaryBase int
	)
	err := c.store.db.QueryRowContext(ctx, `
SELECT graph_id, repo_prefix, family_id, is_primary_base, active_generation_id, state
  FROM dedicated_graphs WHERE owner_checkout_id = ?`, ownerCheckoutID).Scan(
		&dedicated.GraphID, &dedicated.RepoPrefix, &dedicated.FamilyID, &isPrimaryBase,
		&activeGen, &dedicated.State)
	if err == sql.ErrNoRows {
		return DedicatedGraph{}, false, nil
	}
	if err != nil {
		return DedicatedGraph{}, false, err
	}
	dedicated.ActiveGenerationID = activeGen.Int64
	dedicated.IsPrimaryBase = isPrimaryBase != 0
	return dedicated, true, nil
}

// ListDedicatedGraphs returns one family's dedicated graphs, ordered by graph
// id so two passes over an unchanged family see the same order. It is how a
// caller finds the family's primary base and the graph a given checkout owns,
// neither of which is addressable by a graph id it does not know yet.
func (c *Catalog) ListDedicatedGraphs(ctx context.Context, familyID string) ([]DedicatedGraph, error) {
	rows, err := c.store.db.QueryContext(ctx, `
SELECT graph_id, owner_checkout_id, repo_prefix, is_primary_base, active_generation_id, state
  FROM dedicated_graphs WHERE family_id = ? ORDER BY graph_id`, familyID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DedicatedGraph
	for rows.Next() {
		dedicated := DedicatedGraph{FamilyID: familyID}
		var (
			owner         sql.NullString
			activeGen     sql.NullInt64
			isPrimaryBase int
		)
		if err := rows.Scan(&dedicated.GraphID, &owner, &dedicated.RepoPrefix,
			&isPrimaryBase, &activeGen, &dedicated.State); err != nil {
			return nil, err
		}
		dedicated.OwnerCheckoutID = owner.String
		dedicated.ActiveGenerationID = activeGen.Int64
		dedicated.IsPrimaryBase = isPrimaryBase != 0
		out = append(out, dedicated)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// DeleteDedicatedGraph removes a dedicated-graph row. The graph's generations
// are not touched: active_generation_id is a plain integer, so a caller that
// wants them gone prunes them itself.
func (c *Catalog) DeleteDedicatedGraph(ctx context.Context, graphID string) error {
	if err := requireCatalogID("graph_id", graphID); err != nil {
		return err
	}
	return c.deleteOne(ctx, fmt.Sprintf("dedicated graph %s", graphID),
		`DELETE FROM dedicated_graphs WHERE graph_id = ?`, graphID)
}

// DeleteDedicatedGraphForIncarnation removes a graph only while its owner is
// still the checkout incarnation that authorized the cleanup. Graph IDs are
// deterministic from repository prefixes and can be reused after re-tracking;
// a stale saga must therefore treat a replacement binding as already released.
// The bool reports whether this call deleted the expected row.
func (c *Catalog) DeleteDedicatedGraphForIncarnation(
	ctx context.Context, graphID, checkoutID, incarnation string,
) (bool, error) {
	if err := requireCatalogID("graph_id", graphID); err != nil {
		return false, err
	}
	if err := requireCatalogID("checkout_id", checkoutID); err != nil {
		return false, err
	}
	if err := requireCatalogID("incarnation", incarnation); err != nil {
		return false, err
	}
	result, err := c.exec(ctx, `
		DELETE FROM dedicated_graphs
		WHERE graph_id = ?
		  AND owner_checkout_id = ?
		  AND EXISTS (
			SELECT 1 FROM checkouts
			WHERE checkout_id = ? AND incarnation = ?
		  )`, graphID, checkoutID, checkoutID, incarnation)
	if err != nil {
		return false, err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return changed != 0, nil
}

// RestoreDedicatedGraphForIncarnation restores a captured graph row after a
// later cross-store cleanup step failed. The bool is true only when the
// captured row is present when the transaction commits; false means the graph
// or checkout identity has moved and the caller must treat the cleanup as
// stale. Restoration is insert-only: it never updates a replacement row.
func (c *Catalog) RestoreDedicatedGraphForIncarnation(
	ctx context.Context, captured DedicatedGraph, checkoutID, incarnation string,
) (bool, error) {
	if err := captured.validate(); err != nil {
		return false, err
	}
	if err := requireCatalogID("checkout_id", checkoutID); err != nil {
		return false, err
	}
	if err := requireCatalogID("incarnation", incarnation); err != nil {
		return false, err
	}
	if captured.OwnerCheckoutID != checkoutID {
		return false, fmt.Errorf(
			"%w: graph %s is owned by checkout %s, not %s",
			ErrCatalogStaleGuard, captured.GraphID, captured.OwnerCheckoutID, checkoutID)
	}

	capturedPresent := false
	err := c.withTx(ctx, func(tx *sql.Tx) error {
		var checkoutMatches int
		if err := tx.QueryRowContext(ctx, `
SELECT COUNT(*) FROM checkouts
 WHERE checkout_id = ? AND incarnation = ? AND family_id = ?`,
			checkoutID, incarnation, captured.FamilyID).Scan(&checkoutMatches); err != nil {
			return err
		}
		if checkoutMatches == 0 {
			return nil
		}

		current := DedicatedGraph{GraphID: captured.GraphID}
		var (
			owner         sql.NullString
			activeGen     sql.NullInt64
			isPrimaryBase int
		)
		err := tx.QueryRowContext(ctx, `
SELECT owner_checkout_id, repo_prefix, family_id, is_primary_base, active_generation_id, state
  FROM dedicated_graphs WHERE graph_id = ?`, captured.GraphID).Scan(
			&owner, &current.RepoPrefix, &current.FamilyID, &isPrimaryBase,
			&activeGen, &current.State)
		switch {
		case err == nil:
			current.OwnerCheckoutID = owner.String
			current.ActiveGenerationID = activeGen.Int64
			current.IsPrimaryBase = isPrimaryBase != 0
			capturedPresent = current == captured
			return nil
		case err != sql.ErrNoRows:
			return err
		}

		// Owner and primary uniqueness are replacement signals, not errors a
		// compensating cleanup should retry against indefinitely.
		var conflicts int
		if err := tx.QueryRowContext(ctx, `
SELECT COUNT(*) FROM dedicated_graphs
 WHERE owner_checkout_id = ?
    OR (? = 1 AND family_id = ? AND is_primary_base = 1)`,
			checkoutID, catalogBoolInt(captured.IsPrimaryBase), captured.FamilyID).Scan(&conflicts); err != nil {
			return err
		}
		if conflicts != 0 {
			return nil
		}

		result, err := tx.ExecContext(ctx, `
INSERT INTO dedicated_graphs
  (graph_id, owner_checkout_id, repo_prefix, family_id, is_primary_base, active_generation_id, state)
VALUES (?, ?, ?, ?, ?, ?, ?)`,
			captured.GraphID, catalogNullString(captured.OwnerCheckoutID), captured.RepoPrefix,
			captured.FamilyID, catalogBoolInt(captured.IsPrimaryBase),
			catalogNullInt(captured.ActiveGenerationID), captured.State)
		if err != nil {
			return err
		}
		changed, err := result.RowsAffected()
		if err != nil {
			return err
		}
		capturedPresent = changed == 1
		return nil
	})
	return capturedPresent, err
}

// SetPrimaryDedicatedGraph moves the family's primary base to one graph. The
// family's primary_epoch is the compare-and-set token: a promotion carrying a
// stale epoch changes nothing and reports ErrCatalogStaleGuard, so two
// reconcilers cannot each believe they installed the primary. The incumbent is
// cleared before the new holder is set, because the partial unique index
// permits exactly one is_primary_base row per family at any point.
func (c *Catalog) SetPrimaryDedicatedGraph(ctx context.Context, req SetPrimaryDedicatedGraphRequest) error {
	if err := requireCatalogID("family_id", req.FamilyID); err != nil {
		return err
	}
	if err := requireCatalogID("graph_id", req.GraphID); err != nil {
		return err
	}
	return c.withTx(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `
UPDATE repository_families
   SET primary_epoch = primary_epoch + 1, last_seen = ?
 WHERE family_id = ? AND primary_epoch = ?`,
			req.LastSeen, req.FamilyID, req.ExpectedPrimaryEpoch)
		if err != nil {
			return err
		}
		changed, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if changed == 0 {
			return fmt.Errorf("%w: family %s primary epoch %d",
				ErrCatalogStaleGuard, req.FamilyID, req.ExpectedPrimaryEpoch)
		}
		if _, err := tx.ExecContext(ctx, `
UPDATE dedicated_graphs SET is_primary_base = 0
 WHERE family_id = ? AND is_primary_base = 1 AND graph_id <> ?`,
			req.FamilyID, req.GraphID); err != nil {
			return err
		}
		result, err = tx.ExecContext(ctx, `
UPDATE dedicated_graphs SET is_primary_base = 1
 WHERE graph_id = ? AND family_id = ?`, req.GraphID, req.FamilyID)
		if err != nil {
			return err
		}
		changed, err = result.RowsAffected()
		if err != nil {
			return err
		}
		if changed == 0 {
			return fmt.Errorf("%w: graph %s in family %s", ErrCatalogNotFound, req.GraphID, req.FamilyID)
		}
		return nil
	})
}

// --- view generations ---------------------------------------------------

const viewGenerationColumns = `owner_kind, graph_id, layer_id, checkout_id, generation_kind,
	base_generation_id, lower_view_fingerprint, tree_oid, provenance_commit_oid, config_hash,
	extractor_versions, resolver_version, state, covered_files, affected_files, storage_bytes,
	completeness, created_at, published_at, last_selected, error`

const insertViewGenerationSQL = `
INSERT INTO view_generations (` + viewGenerationColumns + `)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

// viewGenerationInsertArgs binds one generation row in viewGenerationColumns
// order, so the plain insert and the coalescing one cannot drift.
func viewGenerationInsertArgs(generation ViewGeneration) []any {
	return []any{
		generation.OwnerKind, generation.GraphID,
		catalogNullString(generation.LayerID), catalogNullString(generation.CheckoutID),
		generation.GenerationKind, catalogNullInt(generation.BaseGenerationID),
		generation.LowerViewFingerprint, generation.TreeOID,
		catalogNullString(generation.ProvenanceCommitOID), generation.ConfigHash,
		generation.ExtractorVersions, generation.ResolverVersion, string(generation.State),
		generation.CoveredFiles, generation.AffectedFiles, generation.StorageBytes,
		generation.Completeness, generation.CreatedAt, generation.PublishedAt,
		generation.LastSelected, generation.Error,
	}
}

// CreateViewGeneration inserts a generation and returns its assigned id. The
// row is written exactly once; afterwards only PublishViewGeneration may
// change it, and only out of the building state.
func (c *Catalog) CreateViewGeneration(ctx context.Context, generation ViewGeneration) (int64, error) {
	if err := generation.validate(); err != nil {
		return 0, err
	}
	result, err := c.exec(ctx, insertViewGenerationSQL, viewGenerationInsertArgs(generation)...)
	if err != nil {
		return 0, err
	}
	return result.LastInsertId()
}

// scanViewGeneration reads one row in viewGenerationColumns order, folding the
// nullable columns back to their zero value. The single read and the listing
// share it so the two cannot drift.
func scanViewGeneration(scan func(...any) error, generation *ViewGeneration) error {
	var (
		layerID, checkoutID, provenance sql.NullString
		baseGeneration                  sql.NullInt64
		state                           string
	)
	err := scan(
		&generation.OwnerKind, &generation.GraphID, &layerID, &checkoutID,
		&generation.GenerationKind, &baseGeneration, &generation.LowerViewFingerprint,
		&generation.TreeOID, &provenance, &generation.ConfigHash,
		&generation.ExtractorVersions, &generation.ResolverVersion, &state,
		&generation.CoveredFiles, &generation.AffectedFiles, &generation.StorageBytes,
		&generation.Completeness, &generation.CreatedAt, &generation.PublishedAt,
		&generation.LastSelected, &generation.Error)
	if err != nil {
		return err
	}
	generation.LayerID = layerID.String
	generation.CheckoutID = checkoutID.String
	generation.ProvenanceCommitOID = provenance.String
	generation.BaseGenerationID = baseGeneration.Int64
	generation.State = ViewGenerationState(state)
	return nil
}

// GetViewGeneration returns one generation.
func (c *Catalog) GetViewGeneration(ctx context.Context, generationID int64) (ViewGeneration, bool, error) {
	generation := ViewGeneration{GenerationID: generationID}
	row := c.store.db.QueryRowContext(ctx,
		`SELECT `+viewGenerationColumns+` FROM view_generations WHERE generation_id = ?`,
		generationID)
	err := scanViewGeneration(row.Scan, &generation)
	if err == sql.ErrNoRows {
		return ViewGeneration{}, false, nil
	}
	if err != nil {
		return ViewGeneration{}, false, err
	}
	return generation, true, nil
}

// maxViewGenerationListing bounds one ListViewGenerations call, whether or not
// the caller asked for a bound.
//
// The scan's only caller is a janitor pass that offers what it finds for
// retirement, and a pass that returned the whole table would grow with the
// installation while collecting the same handful of generations each time. 512
// is far more than the layers a family accumulates between two sweeps, so a
// healthy store is enumerated whole; a store that somehow holds more is
// collected across several passes instead of in one unbounded read.
const maxViewGenerationListing = 512

// ListViewGenerations enumerates generations, newest id first.
//
// It is the recovery read for retirement. Every other handle on a generation is
// something that points at it, so the set of generations nobody should be
// keeping is exactly the set nothing names — which cannot be derived from the
// pointers. The listing is what lets a caller re-derive it from the rows.
//
// A filter that names a graph rides view_generations_by_graph_state, whose
// trailing generation_id DESC is this ordering; one that does not scans the
// table under the same bound.
func (c *Catalog) ListViewGenerations(ctx context.Context, filter ViewGenerationFilter) ([]ViewGeneration, error) {
	if filter.Limit < 0 {
		return nil, fmt.Errorf("%w: limit %d", ErrCatalogInvalidValue, filter.Limit)
	}
	if filter.BeforeGenerationID < 0 {
		return nil, fmt.Errorf("%w: before_generation_id %d", ErrCatalogInvalidValue, filter.BeforeGenerationID)
	}
	if filter.GraphID != "" && filter.MissingGraph {
		return nil, fmt.Errorf("%w: graph_id and missing_graph are mutually exclusive", ErrCatalogInvalidValue)
	}
	limit := filter.Limit
	if limit == 0 || limit > maxViewGenerationListing {
		limit = maxViewGenerationListing
	}

	var (
		clauses []string
		args    []any
	)
	if filter.GraphID != "" {
		clauses = append(clauses, `graph_id = ?`)
		args = append(args, filter.GraphID)
	}
	if filter.MissingGraph {
		clauses = append(clauses, `graph_id <> '' AND NOT EXISTS (`+
			`SELECT 1 FROM dedicated_graphs WHERE dedicated_graphs.graph_id = view_generations.graph_id`+
			`)`)
	}
	if filter.BeforeGenerationID > 0 {
		clauses = append(clauses, `generation_id < ?`)
		args = append(args, filter.BeforeGenerationID)
	}
	if len(filter.States) > 0 {
		placeholders := make([]string, len(filter.States))
		for i, state := range filter.States {
			if err := requireCatalogValue("state", state, viewGenerationStates); err != nil {
				return nil, err
			}
			placeholders[i] = "?"
			args = append(args, string(state))
		}
		clauses = append(clauses, `state IN (`+strings.Join(placeholders, ", ")+`)`)
	}
	if filter.OwnerKind != "" {
		clauses = append(clauses, `owner_kind = ?`)
		args = append(args, filter.OwnerKind)
	}
	if filter.CheckoutID != "" {
		clauses = append(clauses, `checkout_id = ?`)
		args = append(args, filter.CheckoutID)
	}

	query := `SELECT generation_id, ` + viewGenerationColumns + ` FROM view_generations`
	if len(clauses) > 0 {
		query += ` WHERE ` + strings.Join(clauses, " AND ")
	}
	query += ` ORDER BY generation_id DESC LIMIT ?`
	args = append(args, limit)

	rows, err := c.store.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ViewGeneration
	for rows.Next() {
		var generation ViewGeneration
		err := scanViewGeneration(func(dest ...any) error {
			return rows.Scan(append([]any{&generation.GenerationID}, dest...)...)
		}, &generation)
		if err != nil {
			return nil, err
		}
		out = append(out, generation)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// PublishViewGeneration is the building -> ready transition and the only write
// a generation ever receives after creation. The WHERE clause carries the
// expected state, so a generation that is already ready (or failed, or
// retiring) is immutable: the update matches nothing and reports
// ErrCatalogStaleGuard.
func (c *Catalog) PublishViewGeneration(ctx context.Context, generationID, publishedAt int64) error {
	if generationID <= 0 {
		return fmt.Errorf("%w: generation_id %d", ErrCatalogInvalidValue, generationID)
	}
	return c.execGuarded(ctx, fmt.Sprintf("view generation %d is not building", generationID), `
UPDATE view_generations SET state = ?, published_at = ?
 WHERE generation_id = ? AND state = ?`,
		string(ViewGenerationReady), publishedAt, generationID, string(ViewGenerationBuilding))
}

// buildingViewGenerationMatchSQL finds the in-flight generation a repeat build
// request may adopt. Every column of the build identity is compared, so two
// requests coalesce only when they would produce the same payload. The
// nullable columns are folded to their zero value first: SQLite compares NULL
// to anything as NULL, which would make an unset layer match nothing including
// itself.
const buildingViewGenerationMatchSQL = `
SELECT generation_id FROM view_generations
 WHERE state = ? AND graph_id = ? AND owner_kind = ? AND generation_kind = ?
   AND IFNULL(layer_id, '') = ? AND IFNULL(checkout_id, '') = ?
   AND IFNULL(base_generation_id, 0) = ?
   AND lower_view_fingerprint = ? AND tree_oid = ?
   AND IFNULL(provenance_commit_oid, '') = ? AND config_hash = ?
   AND extractor_versions = ? AND resolver_version = ?
 ORDER BY generation_id LIMIT 1`

// AdoptOrCreateViewGeneration returns the id of the building generation this
// request may share, creating one when none matches. The bool reports adoption.
//
// Coalescing follows ref_view_builds_single_inflight: two requests for the same
// inputs share one in-flight build instead of racing to produce the same
// generation twice. A generation with no layer is deliberately outside the rule
// — the same exclusion that index makes for a build with no base generation —
// so an unnamed build always gets its own generation.
//
// The lookup and the insert run in one transaction under the mutation gate,
// which is what makes the check-then-insert atomic against a concurrent begin.
func (c *Catalog) AdoptOrCreateViewGeneration(ctx context.Context, generation ViewGeneration) (int64, bool, error) {
	if err := generation.validate(); err != nil {
		return 0, false, err
	}
	if generation.State != ViewGenerationBuilding {
		return 0, false, fmt.Errorf("%w: state %q, want %q",
			ErrCatalogInvalidValue, generation.State, ViewGenerationBuilding)
	}
	var (
		generationID int64
		adopted      bool
	)
	err := c.withTx(ctx, func(tx *sql.Tx) error {
		if generation.LayerID != "" {
			err := tx.QueryRowContext(ctx, buildingViewGenerationMatchSQL,
				string(ViewGenerationBuilding), generation.GraphID, generation.OwnerKind,
				generation.GenerationKind, generation.LayerID, generation.CheckoutID,
				generation.BaseGenerationID, generation.LowerViewFingerprint,
				generation.TreeOID, generation.ProvenanceCommitOID, generation.ConfigHash,
				generation.ExtractorVersions, generation.ResolverVersion).Scan(&generationID)
			if err == nil {
				adopted = true
				return nil
			}
			if !errors.Is(err, sql.ErrNoRows) {
				return err
			}
		}
		result, err := tx.ExecContext(ctx, insertViewGenerationSQL, viewGenerationInsertArgs(generation)...)
		if err != nil {
			return err
		}
		generationID, err = result.LastInsertId()
		return err
	})
	if err != nil {
		return 0, false, err
	}
	return generationID, adopted, nil
}

// SetViewGenerationState moves a generation to another lifecycle state. The
// expected states are the compare-and-set guard; passing none accepts whatever
// the row currently holds, which is what retirement needs — a crashed build and
// a superseded publish are both collectable.
func (c *Catalog) SetViewGenerationState(ctx context.Context, generationID int64, next ViewGenerationState, expected ...ViewGenerationState) error {
	if generationID <= 0 {
		return fmt.Errorf("%w: generation_id %d", ErrCatalogInvalidValue, generationID)
	}
	if err := requireCatalogValue("state", next, viewGenerationStates); err != nil {
		return err
	}
	query := `UPDATE view_generations SET state = ? WHERE generation_id = ?`
	args := []any{string(next), generationID}
	if len(expected) > 0 {
		placeholders := make([]string, len(expected))
		for i, state := range expected {
			if err := requireCatalogValue("expected_state", state, viewGenerationStates); err != nil {
				return err
			}
			placeholders[i] = "?"
			args = append(args, string(state))
		}
		query += ` AND state IN (` + strings.Join(placeholders, ", ") + `)`
	}
	return c.execGuarded(ctx, fmt.Sprintf("view generation %d cannot become %s", generationID, next), query, args...)
}

// UpdateViewGenerationRollup records the payload measurements taken just
// before a publish. It is guarded on the building state for the same reason
// PublishViewGeneration is: a published generation's row is immutable.
func (c *Catalog) UpdateViewGenerationRollup(ctx context.Context, generationID, coveredFiles, affectedFiles, storageBytes int64) error {
	if generationID <= 0 {
		return fmt.Errorf("%w: generation_id %d", ErrCatalogInvalidValue, generationID)
	}
	return c.execGuarded(ctx, fmt.Sprintf("view generation %d is not building", generationID), `
UPDATE view_generations SET covered_files = ?, affected_files = ?, storage_bytes = ?
 WHERE generation_id = ? AND state = ?`,
		coveredFiles, affectedFiles, storageBytes, generationID, string(ViewGenerationBuilding))
}

// WithdrawProducer marks one producer of a generation unavailable.
//
// It is the write behind a capability a view has stopped being able to serve
// — the source bytes it was built from have left the object store, say. One
// producer row moves and nothing else does, so everything the generation
// already holds keeps answering; the read that discovered the loss is what
// calls it, and a second discovery reports a stale guard rather than writing
// the same verdict twice.
//
// It is a control-plane write on purpose. A published generation is sealed
// against payload writes, and this has to go through on exactly such a
// generation: the withdrawal is a statement about what the payload can no
// longer produce, not a change to the payload.
const (
	producerWithdrawalSQLiteBusyQuantumMillis = 5
	producerWithdrawalBusyRestoreBudget       = 25 * time.Millisecond
)

func (c *Catalog) WithdrawProducer(ctx context.Context, generationID int64, producer, reason string) error {
	if generationID <= 0 {
		return fmt.Errorf("%w: generation_id %d", ErrCatalogInvalidValue, generationID)
	}
	if err := requireCatalogID("producer", producer); err != nil {
		return err
	}
	subject := fmt.Sprintf("producer %s of generation %d is already unavailable or undeclared", producer, generationID)
	return c.withProducerWithdrawalBusyRetry(ctx, func(attemptCtx context.Context) error {
		result, err := c.execProducerWithdrawalQuantum(attemptCtx, generationID, producer, reason)
		if err != nil {
			return err
		}
		changed, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if changed == 0 {
			return fmt.Errorf("%w: %s", ErrCatalogStaleGuard, subject)
		}
		return nil
	})
}

// withProducerWithdrawalBusyRetry is the quiet, context-aware retry loop for
// best-effort withdrawal maintenance. The general store retry helper logs
// recovery/exhaustion, which is useful for operator-initiated mutations but
// would turn repeated missing-object reads into a synchronous log storm. The
// manager emits one aggregate observer/stat event when each attempt finishes.
func (c *Catalog) withProducerWithdrawalBusyRetry(
	parent context.Context,
	fn func(context.Context) error,
) error {
	retryDeadline := time.Now().Add(c.store.sqliteBusyRetryWindow())
	delay := sqliteBusyRetryBaseDelay
	var lastBusy error
	for {
		if err := parent.Err(); err != nil {
			return err
		}
		err := fn(parent)
		if err == nil {
			return nil
		}
		if errors.Is(err, errSQLiteBusyRetryExhausted) {
			return err
		}
		if !isSQLiteBusyErr(err) {
			return err
		}
		lastBusy = err

		remaining := time.Until(retryDeadline)
		if remaining <= 0 {
			return fmt.Errorf("withdraw producer: %w", errors.Join(errSQLiteBusyRetryExhausted, lastBusy, context.DeadlineExceeded))
		}
		wait := minDuration(delay, remaining)
		timer := time.NewTimer(wait)
		select {
		case <-timer.C:
		case <-parent.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return fmt.Errorf("withdraw producer: %w", errors.Join(errSQLiteBusyRetryExhausted, lastBusy, parent.Err()))
		}
		if delay *= 2; delay > sqliteBusyRetryMaxDelay {
			delay = sqliteBusyRetryMaxDelay
		}
	}
}

// execProducerWithdrawalQuantum gives this one best-effort maintenance write a
// short connection-local SQLite busy wait. BUSY/LOCKED returns to Go quickly,
// where withSQLiteBusyRetry can honor the manager attempt or shared shutdown
// context between quanta. The store write gate is context-aware as well.
func (c *Catalog) execProducerWithdrawalQuantum(
	ctx context.Context,
	generationID int64,
	producer, reason string,
) (sql.Result, error) {
	if err := c.store.writeMu.LockContext(ctx); err != nil {
		return nil, err
	}
	defer c.store.writeMu.Unlock()

	conn, err := c.store.writerDB.Conn(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	var previousBusyTimeout int
	if err := conn.QueryRowContext(ctx, `PRAGMA busy_timeout`).Scan(&previousBusyTimeout); err != nil {
		return nil, err
	}
	if _, err := conn.ExecContext(ctx, fmt.Sprintf(`PRAGMA busy_timeout = %d`, producerWithdrawalSQLiteBusyQuantumMillis)); err != nil {
		return nil, err
	}
	result, execErr := conn.ExecContext(ctx, `
UPDATE generation_producer_completeness SET state = ?, reason = ?
 WHERE view_gen = ? AND producer = ? AND state != ?`,
		string(ProducerStateUnavailable), reason, generationID, producer, string(ProducerStateUnavailable))

	restoreCtx, cancel := context.WithTimeout(context.Background(), producerWithdrawalBusyRestoreBudget)
	_, restoreErr := conn.ExecContext(restoreCtx, fmt.Sprintf(`PRAGMA busy_timeout = %d`, previousBusyTimeout))
	cancel()
	if restoreErr != nil {
		// Returning driver.ErrBadConn from Raw marks this physical connection
		// unusable. Close then discards it instead of returning a connection with
		// the withdrawal-only short timeout to unrelated writers.
		_ = conn.Raw(func(any) error { return driver.ErrBadConn })
		return nil, errors.Join(execErr, fmt.Errorf("restore producer withdrawal busy_timeout: %w", restoreErr))
	}
	return result, execErr
}

// ProducerAvailability is the narrow readback needed to classify one
// asynchronous withdrawal attempt. Declared is false when either the
// generation or producer row no longer exists; both mean the capability is
// absent and the withdrawal invariant is already satisfied.
type ProducerAvailability struct {
	Declared bool
	State    ProducerState
}

// ReadProducerAvailability reads exactly one generation-producer row without
// acquiring the writer gate. It is context-bounded so classification shares
// the manager attempt's deadline rather than introducing a second wait.
func (c *Catalog) ReadProducerAvailability(ctx context.Context, generationID int64, producer string) (ProducerAvailability, error) {
	if generationID <= 0 {
		return ProducerAvailability{}, fmt.Errorf("%w: generation_id %d", ErrCatalogInvalidValue, generationID)
	}
	if err := requireCatalogID("producer", producer); err != nil {
		return ProducerAvailability{}, err
	}
	var raw string
	err := c.store.db.QueryRowContext(ctx, `
SELECT state
  FROM generation_producer_completeness
 WHERE view_gen = ? AND producer = ?`, generationID, producer).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return ProducerAvailability{}, nil
	}
	if err != nil {
		return ProducerAvailability{}, err
	}
	state := ProducerState(raw)
	switch state {
	case ProducerStateComplete,
		ProducerStateIncomplete,
		ProducerStateBuilding,
		ProducerStateUnavailable,
		ProducerStateDisabledByConfig:
		return ProducerAvailability{Declared: true, State: state}, nil
	default:
		return ProducerAvailability{}, fmt.Errorf("%w: producer %s of generation %d has state %q", ErrCatalogInvalidValue, producer, generationID, raw)
	}
}

func (c *Catalog) classifyProducerWithdrawal(
	ctx context.Context,
	generationID int64,
	producer string,
	withdrawErr error,
) (producerWithdrawalDisposition, error) {
	// A direct BUSY/LOCKED result is transient without readback. Reading through
	// the same attempt context can only turn a useful contention signal into a
	// deadline error, and the producer row could not have changed on BUSY.
	if isSQLiteBusyErr(withdrawErr) || errors.Is(withdrawErr, errSQLiteBusyRetryExhausted) {
		return producerWithdrawalTransient, nil
	}
	// A timed-out/canceled withdrawal cannot usefully read back through the
	// same attempt context. It is retryable by definition and must not be
	// mislabeled persistent because the verification query sees that deadline.
	if ctx.Err() != nil || errors.Is(withdrawErr, context.Canceled) || errors.Is(withdrawErr, context.DeadlineExceeded) {
		return producerWithdrawalTransient, nil
	}
	availability, err := c.ReadProducerAvailability(ctx, generationID, producer)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || isSQLiteBusyErr(err) {
			return producerWithdrawalTransient, nil
		}
		return producerWithdrawalPersistent, err
	}
	if !availability.Declared || availability.State == ProducerStateUnavailable {
		return producerWithdrawalSatisfied, nil
	}
	if isSQLiteBusyErr(withdrawErr) {
		return producerWithdrawalTransient, nil
	}
	return producerWithdrawalPersistent, nil
}

// ViewGenerationReferences is which kinds of pointer still name a generation.
// It is the reference guard's verdict broken out by holder, so a refused
// retirement can say why it was refused instead of only that it was.
type ViewGenerationReferences struct {
	// Routed is a checkout route naming the generation in either slot.
	Routed bool
	// RefViewed is a ref view serving it as its active generation.
	RefViewed bool
	// Based is another generation naming it as the layer beneath.
	Based bool
	// GraphActive is a dedicated graph's active pointer.
	GraphActive bool
}

// Any reports whether any pointer names the generation. It is the boolean the
// delete guard enforces.
func (r ViewGenerationReferences) Any() bool {
	return r.Routed || r.RefViewed || r.Based || r.GraphActive
}

// ViewGenerationReferenced reports whether anything still points at a
// generation. It asks exactly what DeleteViewGeneration's guard asks, so a
// caller about to delete a generation's payload can refuse before it starts
// instead of after.
func (c *Catalog) ViewGenerationReferenced(ctx context.Context, generationID int64) (bool, error) {
	refs, err := c.ViewGenerationReferences(ctx, generationID)
	return refs.Any(), err
}

// ViewGenerationReferences reports which pointers still name a generation, in
// one query. It is the same set of EXISTS clauses the delete guard runs, kept
// apart rather than OR-ed together so the caller can classify the refusal.
func (c *Catalog) ViewGenerationReferences(
	ctx context.Context, generationID int64,
) (ViewGenerationReferences, error) {
	var refs ViewGenerationReferences
	if generationID <= 0 {
		return refs, fmt.Errorf("%w: generation_id %d", ErrCatalogInvalidValue, generationID)
	}
	err := c.store.db.QueryRowContext(ctx, viewGenerationReferencesSQL,
		generationID, generationID, generationID, generationID, generationID,
	).Scan(&refs.Routed, &refs.RefViewed, &refs.Based, &refs.GraphActive)
	return refs, err
}

// viewGenerationReferencedSQL is the reference guard DeleteViewGeneration
// enforces inside its own transaction.
const viewGenerationReferencedSQL = `
SELECT EXISTS(SELECT 1 FROM checkout_routes WHERE commit_generation_id = ? OR dirty_generation_id = ?)
    OR EXISTS(SELECT 1 FROM ref_views WHERE active_generation_id = ?)
    OR EXISTS(SELECT 1 FROM view_generations WHERE base_generation_id = ?)
    OR EXISTS(SELECT 1 FROM dedicated_graphs WHERE active_generation_id = ?)`

// viewGenerationReferencesSQL is the same guard with its clauses kept apart,
// so one round trip answers both "is it referenced" and "by what".
const viewGenerationReferencesSQL = `
SELECT EXISTS(SELECT 1 FROM checkout_routes WHERE commit_generation_id = ? OR dirty_generation_id = ?),
       EXISTS(SELECT 1 FROM ref_views WHERE active_generation_id = ?),
       EXISTS(SELECT 1 FROM view_generations WHERE base_generation_id = ?),
       EXISTS(SELECT 1 FROM dedicated_graphs WHERE active_generation_id = ?)`

// DeleteViewGeneration removes a generation nothing points at. SQLite's own
// foreign keys already refuse a delete under a route, a ref view, or another
// generation's base pointer (a non-deferred NO ACTION constraint is enforced
// as RESTRICT); this checks the same references — plus dedicated_graphs'
// deliberately key-free active pointer — first, so the caller gets one typed
// refusal instead of a driver constraint string.
func (c *Catalog) DeleteViewGeneration(ctx context.Context, generationID int64) error {
	if generationID <= 0 {
		return fmt.Errorf("%w: generation_id %d", ErrCatalogInvalidValue, generationID)
	}
	return c.withTx(ctx, func(tx *sql.Tx) error {
		var referenced bool
		if err := tx.QueryRowContext(ctx, viewGenerationReferencedSQL,
			generationID, generationID, generationID, generationID, generationID,
		).Scan(&referenced); err != nil {
			return err
		}
		if referenced {
			return fmt.Errorf("%w: generation %d", ErrCatalogGenerationReferenced, generationID)
		}
		result, err := tx.ExecContext(ctx, `DELETE FROM view_generations WHERE generation_id = ?`, generationID)
		if err != nil {
			return err
		}
		changed, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if changed == 0 {
			return fmt.Errorf("%w: generation %d", ErrCatalogNotFound, generationID)
		}
		return nil
	})
}

// --- view layers --------------------------------------------------------

// UpsertViewLayer writes one layer row.
func (c *Catalog) UpsertViewLayer(ctx context.Context, layer ViewLayer) error {
	if err := layer.validate(); err != nil {
		return err
	}
	_, err := c.exec(ctx, `
INSERT INTO view_layers (layer_id, kind, graph_id, checkout_id, target_ref, target_commit, target_tree)
VALUES (?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(layer_id) DO UPDATE SET
  kind          = excluded.kind,
  graph_id      = excluded.graph_id,
  checkout_id   = excluded.checkout_id,
  target_ref    = excluded.target_ref,
  target_commit = excluded.target_commit,
  target_tree   = excluded.target_tree`,
		layer.LayerID, layer.Kind, layer.GraphID,
		catalogNullString(layer.CheckoutID), catalogNullString(layer.TargetRef),
		layer.TargetCommit, layer.TargetTree)
	return err
}

// GetViewLayer returns one layer.
func (c *Catalog) GetViewLayer(ctx context.Context, layerID string) (ViewLayer, bool, error) {
	layer := ViewLayer{LayerID: layerID}
	var checkoutID, targetRef sql.NullString
	err := c.store.db.QueryRowContext(ctx, `
SELECT kind, graph_id, checkout_id, target_ref, target_commit, target_tree
  FROM view_layers WHERE layer_id = ?`, layerID).Scan(
		&layer.Kind, &layer.GraphID, &checkoutID, &targetRef,
		&layer.TargetCommit, &layer.TargetTree)
	if err == sql.ErrNoRows {
		return ViewLayer{}, false, nil
	}
	if err != nil {
		return ViewLayer{}, false, err
	}
	layer.CheckoutID = checkoutID.String
	layer.TargetRef = targetRef.String
	return layer, true, nil
}

// --- checkout routes ----------------------------------------------------

// UpsertCheckoutRoute writes a checkout's route row, including its epoch.
// Repointing an existing route is FlipCheckoutRoute's job: this write does not
// compare-and-set, so it is for installing a route, not for moving one.
func (c *Catalog) UpsertCheckoutRoute(ctx context.Context, route CheckoutRoute) error {
	if err := route.validate(); err != nil {
		return err
	}
	return c.withTx(ctx, func(tx *sql.Tx) error {
		if err := checkoutRouteGraphAuthorizedTx(ctx, tx, route.CheckoutID, route.GraphID); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `
INSERT INTO checkout_routes
  (checkout_id, graph_id, commit_generation_id, dirty_generation_id, route_epoch, state)
VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT(checkout_id) DO UPDATE SET
  graph_id             = excluded.graph_id,
  commit_generation_id = excluded.commit_generation_id,
  dirty_generation_id  = excluded.dirty_generation_id,
  route_epoch          = excluded.route_epoch,
  state                = excluded.state`,
			route.CheckoutID, route.GraphID, catalogNullInt(route.CommitGenerationID),
			catalogNullInt(route.DirtyGenerationID), route.RouteEpoch, string(route.State))
		return err
	})
}

// GetCheckoutRoute returns one checkout's route.
func (c *Catalog) GetCheckoutRoute(ctx context.Context, checkoutID string) (CheckoutRoute, bool, error) {
	route := CheckoutRoute{CheckoutID: checkoutID}
	var (
		commitGen, dirtyGen sql.NullInt64
		state               string
	)
	err := c.store.db.QueryRowContext(ctx, `
SELECT graph_id, commit_generation_id, dirty_generation_id, route_epoch, state
  FROM checkout_routes WHERE checkout_id = ?`, checkoutID).Scan(
		&route.GraphID, &commitGen, &dirtyGen, &route.RouteEpoch, &state)
	if err == sql.ErrNoRows {
		return CheckoutRoute{}, false, nil
	}
	if err != nil {
		return CheckoutRoute{}, false, err
	}
	route.CommitGenerationID = commitGen.Int64
	route.DirtyGenerationID = dirtyGen.Int64
	route.State = RouteState(state)
	return route, true, nil
}

// checkoutRouteLookupBatchSize stays below SQLite's host-parameter ceiling and
// bounds both the generated statement and the rows one read can materialize.
const checkoutRouteLookupBatchSize = 512

type checkoutRouteBatchReader func(context.Context, []string) (map[string]CheckoutRoute, error)

// GetCheckoutRoutes returns the existing routes among checkoutIDs, keyed by
// checkout id. Missing and empty ids are omitted. Input is de-duplicated before
// the read and large requests are split into bounded batches.
func (c *Catalog) GetCheckoutRoutes(ctx context.Context, checkoutIDs []string) (map[string]CheckoutRoute, error) {
	return getCheckoutRoutesBatched(ctx, checkoutIDs, c.getCheckoutRouteBatch)
}

func getCheckoutRoutesBatched(
	ctx context.Context,
	checkoutIDs []string,
	readBatch checkoutRouteBatchReader,
) (map[string]CheckoutRoute, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	unique := make([]string, 0, len(checkoutIDs))
	seen := make(map[string]struct{}, len(checkoutIDs))
	for _, checkoutID := range checkoutIDs {
		if checkoutID == "" {
			continue
		}
		if _, duplicate := seen[checkoutID]; duplicate {
			continue
		}
		seen[checkoutID] = struct{}{}
		unique = append(unique, checkoutID)
	}
	routes := make(map[string]CheckoutRoute)
	for first := 0; first < len(unique); first += checkoutRouteLookupBatchSize {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		last := min(first+checkoutRouteLookupBatchSize, len(unique))
		batch, err := readBatch(ctx, unique[first:last])
		if err != nil {
			return nil, err
		}
		for checkoutID, route := range batch {
			routes[checkoutID] = route
		}
	}
	return routes, nil
}

func (c *Catalog) getCheckoutRouteBatch(ctx context.Context, checkoutIDs []string) (map[string]CheckoutRoute, error) {
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(checkoutIDs)), ",")
	args := make([]any, len(checkoutIDs))
	for i, checkoutID := range checkoutIDs {
		args[i] = checkoutID
	}
	rows, err := c.store.db.QueryContext(ctx, `
SELECT checkout_id, graph_id, commit_generation_id, dirty_generation_id, route_epoch, state
  FROM checkout_routes WHERE checkout_id IN (`+placeholders+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	routes := make(map[string]CheckoutRoute)
	for rows.Next() {
		var (
			route               CheckoutRoute
			commitGen, dirtyGen sql.NullInt64
			state               string
		)
		if err := rows.Scan(
			&route.CheckoutID, &route.GraphID, &commitGen, &dirtyGen, &route.RouteEpoch, &state,
		); err != nil {
			return nil, err
		}
		route.CommitGenerationID = commitGen.Int64
		route.DirtyGenerationID = dirtyGen.Int64
		route.State = RouteState(state)
		routes[route.CheckoutID] = route
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return routes, nil
}

// DeleteCheckoutRoute withdraws a checkout's route. The route row is the one
// child of a checkout that does not cascade, so removing it is what unblocks
// DeleteCheckout.
func (c *Catalog) DeleteCheckoutRoute(ctx context.Context, checkoutID string) error {
	if err := requireCatalogID("checkout_id", checkoutID); err != nil {
		return err
	}
	return c.deleteOne(ctx, fmt.Sprintf("route for checkout %s", checkoutID),
		`DELETE FROM checkout_routes WHERE checkout_id = ?`, checkoutID)
}

// FlipCheckoutRoute repoints a route and bumps its epoch in one guarded
// statement. A flip carrying a stale epoch changes nothing and reports
// ErrCatalogStaleGuard, so two reconcilers cannot interleave halves of two
// different routes.
func (c *Catalog) FlipCheckoutRoute(ctx context.Context, req FlipCheckoutRouteRequest) error {
	if err := requireCatalogID("checkout_id", req.CheckoutID); err != nil {
		return err
	}
	if err := requireCatalogID("graph_id", req.GraphID); err != nil {
		return err
	}
	if err := requireCatalogValue("state", req.State, routeStates); err != nil {
		return err
	}
	const query = `
UPDATE checkout_routes
   SET graph_id = ?, commit_generation_id = ?, dirty_generation_id = ?,
       route_epoch = route_epoch + 1, state = ?
 WHERE checkout_id = ? AND route_epoch = ?`
	subject := fmt.Sprintf("route for checkout %s at epoch %d", req.CheckoutID, req.ExpectedRouteEpoch)
	args := []any{
		req.GraphID, catalogNullInt(req.CommitGenerationID), catalogNullInt(req.DirtyGenerationID),
		string(req.State), req.CheckoutID, req.ExpectedRouteEpoch,
	}
	if req.RequireActiveGraphBase && req.ExpectedBaseGenerationID <= 0 {
		return fmt.Errorf("expected base generation id must be positive")
	}
	return c.withTx(ctx, func(tx *sql.Tx) error {
		if err := checkoutRouteGraphAuthorizedTx(ctx, tx, req.CheckoutID, req.GraphID); err != nil {
			return err
		}
		if req.RequireActiveGraphBase {
			active, err := graphBaseGenerationIsActiveTx(
				ctx, tx, req.GraphID, req.ExpectedBaseGenerationID)
			if err != nil {
				return err
			}
			if !active {
				return fmt.Errorf("%w: dedicated graph %s base moved", ErrCatalogStaleGuard, req.GraphID)
			}
		}
		return execGuardedTx(ctx, tx, subject, query, args...)
	})
}

// flipRouteSlotSQL is one guarded statement per slot. Naming a single column
// leaves the other slot's pointer exactly as it was without reading it first,
// so a flip of the dirty generation can never revert a commit generation a
// concurrent flip installed.
var flipRouteSlotSQL = map[RouteSlot]string{
	RouteSlotCommit: `
UPDATE checkout_routes
   SET commit_generation_id = ?, route_epoch = route_epoch + 1, state = ?
 WHERE checkout_id = ? AND route_epoch = ?`,
	RouteSlotDirty: `
UPDATE checkout_routes
   SET dirty_generation_id = ?, route_epoch = route_epoch + 1, state = ?
 WHERE checkout_id = ? AND route_epoch = ?`,
}

// FlipCheckoutRouteSlot repoints one of a route's two generation pointers and
// bumps its epoch, leaving the other pointer untouched. A generation id of 0
// clears the slot. Like FlipCheckoutRoute, a stale epoch changes nothing and
// reports ErrCatalogStaleGuard.
func (c *Catalog) FlipCheckoutRouteSlot(ctx context.Context, req FlipCheckoutRouteSlotRequest) error {
	if err := requireCatalogID("checkout_id", req.CheckoutID); err != nil {
		return err
	}
	if err := requireCatalogValue("slot", req.Slot, routeSlots); err != nil {
		return err
	}
	if err := requireCatalogValue("state", req.State, routeStates); err != nil {
		return err
	}
	subject := fmt.Sprintf("%s slot of route for checkout %s at epoch %d", req.Slot, req.CheckoutID, req.ExpectedRouteEpoch)
	args := []any{catalogNullInt(req.GenerationID), string(req.State), req.CheckoutID, req.ExpectedRouteEpoch}
	if !req.RequireActiveGraphBase {
		return c.execGuarded(ctx, subject, flipRouteSlotSQL[req.Slot], args...)
	}
	if req.ExpectedBaseGenerationID <= 0 {
		return fmt.Errorf("expected base generation id must be positive")
	}
	return c.withTx(ctx, func(tx *sql.Tx) error {
		active, err := checkoutRouteBaseGenerationIsActiveTx(
			ctx, tx, req.CheckoutID, req.ExpectedBaseGenerationID)
		if err != nil {
			return err
		}
		if !active {
			return fmt.Errorf("%w: checkout %s dedicated base moved", ErrCatalogStaleGuard, req.CheckoutID)
		}
		return execGuardedTx(ctx, tx, subject, flipRouteSlotSQL[req.Slot], args...)
	})
}

// --- ref views ----------------------------------------------------------

const refViewColumns = `graph_id, selector_kind, selector_value, desired_ref, desired_commit,
	desired_tree, active_generation_id, active_ref, active_commit, active_tree,
	enrichment_profile, desired_build_fingerprint, active_build_fingerprint, route_epoch,
	state, exact_view, last_resolved, last_selected, last_error`

// insertRefViewSQL is the row insert both writers share: the upsert below adds
// its conflict clause to it, and GetOrCreateRefView uses it as it stands.
const insertRefViewSQL = `
INSERT INTO ref_views (ref_view_id, ` + refViewColumns + `)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

// refViewInsertArgs renders a view in insertRefViewSQL's parameter order.
func refViewInsertArgs(view RefView) []any {
	return []any{
		view.RefViewID, view.GraphID, view.SelectorKind, view.SelectorValue,
		view.DesiredRef, view.DesiredCommit, view.DesiredTree,
		catalogNullInt(view.ActiveGenerationID), catalogNullString(view.ActiveRef),
		catalogNullString(view.ActiveCommit), catalogNullString(view.ActiveTree),
		view.EnrichmentProfile, view.DesiredBuildFingerprint,
		catalogNullString(view.ActiveBuildFingerprint), view.RouteEpoch,
		string(view.State), catalogBoolInt(view.ExactView),
		view.LastResolved, view.LastSelected, view.LastError,
	}
}

// UpsertRefView writes one ref-view row. The UNIQUE selector key means a
// second row for the same (graph, selector, profile) is a constraint failure,
// not a duplicate view.
func (c *Catalog) UpsertRefView(ctx context.Context, view RefView) error {
	if err := view.validate(); err != nil {
		return err
	}
	_, err := c.exec(ctx, insertRefViewSQL+`
ON CONFLICT(ref_view_id) DO UPDATE SET
  graph_id                  = excluded.graph_id,
  selector_kind             = excluded.selector_kind,
  selector_value            = excluded.selector_value,
  desired_ref               = excluded.desired_ref,
  desired_commit            = excluded.desired_commit,
  desired_tree              = excluded.desired_tree,
  active_generation_id      = excluded.active_generation_id,
  active_ref                = excluded.active_ref,
  active_commit             = excluded.active_commit,
  active_tree               = excluded.active_tree,
  enrichment_profile        = excluded.enrichment_profile,
  desired_build_fingerprint = excluded.desired_build_fingerprint,
  active_build_fingerprint  = excluded.active_build_fingerprint,
  route_epoch               = excluded.route_epoch,
  state                     = excluded.state,
  exact_view                = excluded.exact_view,
  last_resolved             = excluded.last_resolved,
  last_selected             = excluded.last_selected,
  last_error                = excluded.last_error`,
		refViewInsertArgs(view)...)
	return err
}

// GetOrCreateRefView returns the stored row for a view, creating it from the
// argument when the selector has never been asked for before.
//
// It is not UpsertRefView with a read after it: two selections of the same
// view race, and an upsert would let the second one reset a row the first has
// already advanced — its desire, its epoch, or the generation it is serving.
// The insert declines the conflict instead, so an existing row is read back
// exactly as it stands.
func (c *Catalog) GetOrCreateRefView(ctx context.Context, view RefView) (RefView, error) {
	if err := view.validate(); err != nil {
		return RefView{}, err
	}
	err := c.withTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, insertRefViewSQL+` ON CONFLICT DO NOTHING`, refViewInsertArgs(view)...)
		return err
	})
	if err != nil {
		return RefView{}, err
	}
	stored, found, err := c.GetRefView(ctx, view.RefViewID)
	if err != nil {
		return RefView{}, err
	}
	if !found {
		// The insert was declined and the id is not there, so another row
		// already owns this selector under a different id. That is a caller
		// minting ids two ways for one view, not a race.
		return RefView{}, fmt.Errorf("%w: selector %s=%s in graph %s is held by another ref view",
			ErrCatalogInvalidValue, view.SelectorKind, view.SelectorValue, view.GraphID)
	}
	return stored, nil
}

// UpdateRefViewDesire stamps what a selection resolved the view's selector to.
//
// The epoch arithmetic is the whole point: route_epoch moves only when the
// tree or the fingerprint changed. Two concurrent selections that resolve to
// the same state write the same values and leave the epoch alone, so neither
// invalidates a build the other captured; a selection that re-targets the view
// bumps it, and every build captured under the old epoch loses its adoption.
func (c *Catalog) UpdateRefViewDesire(ctx context.Context, req UpdateRefViewDesireRequest) error {
	if err := requireCatalogID("ref_view_id", req.RefViewID); err != nil {
		return err
	}
	if err := requireCatalogValue("state", req.State, refViewStates); err != nil {
		return err
	}
	return c.execGuarded(ctx, fmt.Sprintf("ref view %s", req.RefViewID), `
UPDATE ref_views
   SET desired_ref = ?, desired_commit = ?, desired_tree = ?,
       desired_build_fingerprint = ?, state = ?,
       last_resolved = ?, last_selected = ?, last_error = '',
       route_epoch = route_epoch +
           (CASE WHEN desired_tree = ? AND desired_build_fingerprint = ? THEN 0 ELSE 1 END)
 WHERE ref_view_id = ?`,
		req.DesiredRef, req.DesiredCommit, req.DesiredTree,
		req.DesiredBuildFingerprint, string(req.State),
		req.LastResolved, req.LastSelected,
		req.DesiredTree, req.DesiredBuildFingerprint, req.RefViewID)
}

// AdoptRefViewGeneration points a ref view at a finished build's generation
// and closes the attempt that produced it, in one transaction.
//
// The guard is the epoch the build captured plus the tree and fingerprint it
// was built for, so a view that moved while the build ran adopts nothing and
// reports ErrCatalogStaleGuard. The epoch is bumped on success for the same
// reason a route flip bumps it: whatever else was in flight against this view
// has just been overtaken.
//
// A named claim is guarded the same way and in the same transaction: an
// attempt reclaimed as abandoned while its worker was still running has left
// the building state, so its late adoption is refused rather than published
// behind the successor that now owns the slot — and an adoption the view
// refuses leaves the attempt open instead of recording a publish that did not
// happen.
func (c *Catalog) AdoptRefViewGeneration(ctx context.Context, req AdoptRefViewGenerationRequest) error {
	if err := requireCatalogID("ref_view_id", req.RefViewID); err != nil {
		return err
	}
	if req.GenerationID <= 0 {
		return fmt.Errorf("%w: generation_id %d", ErrCatalogInvalidValue, req.GenerationID)
	}
	if req.BuildID != "" {
		if err := requireCatalogID("build_token", req.BuildToken); err != nil {
			return err
		}
	}
	return c.withTx(ctx, func(tx *sql.Tx) error {
		if req.BuildID != "" {
			err := execGuardedTx(ctx, tx, fmt.Sprintf("ref view build %s", req.BuildID), `
UPDATE ref_view_builds SET state = ?, generation_id = ?, last_progress = ?, error = ''
 WHERE build_id = ? AND build_token = ? AND state = ?`,
				string(ViewGenerationReady), req.GenerationID, req.LastProgress,
				req.BuildID, req.BuildToken, string(ViewGenerationBuilding))
			if err != nil {
				return err
			}
		}
		return execGuardedTx(ctx, tx,
			fmt.Sprintf("ref view %s at epoch %d", req.RefViewID, req.ExpectedRouteEpoch), `
UPDATE ref_views
   SET active_generation_id = ?, active_ref = ?, active_commit = ?, active_tree = ?,
       active_build_fingerprint = ?, state = ?, exact_view = ?,
       last_resolved = ?, last_selected = ?, last_error = '',
       route_epoch = route_epoch + 1
 WHERE ref_view_id = ? AND route_epoch = ?
   AND desired_tree = ? AND desired_build_fingerprint = ?`,
			req.GenerationID, catalogNullString(req.ActiveRef), catalogNullString(req.ActiveCommit),
			catalogNullString(req.ActiveTree), catalogNullString(req.ActiveBuildFingerprint),
			string(RefViewReady), catalogBoolInt(req.ExactView),
			req.LastResolved, req.LastSelected,
			req.RefViewID, req.ExpectedRouteEpoch,
			req.ExpectedDesiredTree, req.ExpectedDesiredBuildFingerprint)
	})
}

// TouchRefViewSelection re-stamps the ref and commit a selection observed, and
// the selection clock, leaving the active generation exactly where it is. The
// epoch guard keeps the stamp off a view another actor has just re-targeted;
// it is not bumped, because nothing about what the view serves changed.
func (c *Catalog) TouchRefViewSelection(ctx context.Context, req TouchRefViewSelectionRequest) error {
	if err := requireCatalogID("ref_view_id", req.RefViewID); err != nil {
		return err
	}
	return c.execGuarded(ctx,
		fmt.Sprintf("ref view %s at epoch %d", req.RefViewID, req.ExpectedRouteEpoch), `
UPDATE ref_views
   SET active_ref = ?, active_commit = ?, last_resolved = ?, last_selected = ?
 WHERE ref_view_id = ? AND route_epoch = ?`,
		catalogNullString(req.ActiveRef), catalogNullString(req.ActiveCommit),
		req.LastResolved, req.LastSelected, req.RefViewID, req.ExpectedRouteEpoch)
}

// FailRefView records why a selection could not be served. The active pointer
// is deliberately absent from the statement: a failed resolution or build says
// nothing about the payload the view was already serving.
func (c *Catalog) FailRefView(ctx context.Context, req FailRefViewRequest) error {
	if err := requireCatalogID("ref_view_id", req.RefViewID); err != nil {
		return err
	}
	return c.execGuarded(ctx,
		fmt.Sprintf("ref view %s at epoch %d", req.RefViewID, req.ExpectedRouteEpoch), `
UPDATE ref_views SET state = ?, last_error = ?, last_resolved = ?
 WHERE ref_view_id = ? AND route_epoch = ?`,
		string(RefViewFailed), req.LastError, req.LastResolved,
		req.RefViewID, req.ExpectedRouteEpoch)
}

// scanRefView reads the refViewColumns projection in order.
func scanRefView(scan func(...any) error, view *RefView) error {
	var (
		activeGeneration                                       sql.NullInt64
		activeRef, activeCommit, activeTree, activeFingerprint sql.NullString
		state                                                  string
		exactView                                              int
	)
	if err := scan(
		&view.GraphID, &view.SelectorKind, &view.SelectorValue, &view.DesiredRef,
		&view.DesiredCommit, &view.DesiredTree, &activeGeneration, &activeRef,
		&activeCommit, &activeTree, &view.EnrichmentProfile,
		&view.DesiredBuildFingerprint, &activeFingerprint, &view.RouteEpoch,
		&state, &exactView, &view.LastResolved, &view.LastSelected, &view.LastError); err != nil {
		return err
	}
	view.ActiveGenerationID = activeGeneration.Int64
	view.ActiveRef = activeRef.String
	view.ActiveCommit = activeCommit.String
	view.ActiveTree = activeTree.String
	view.ActiveBuildFingerprint = activeFingerprint.String
	view.State = RefViewState(state)
	view.ExactView = exactView != 0
	return nil
}

// ListRefViews returns every named view rooted in one graph, ordered by view
// id. Retiring a graph has to find its views without knowing their ids, and
// the ref_views UNIQUE selector key makes graph_id the leading column of a
// usable index for the scan.
func (c *Catalog) ListRefViews(ctx context.Context, graphID string) ([]RefView, error) {
	rows, err := c.store.db.QueryContext(ctx,
		`SELECT ref_view_id, `+refViewColumns+` FROM ref_views WHERE graph_id = ? ORDER BY ref_view_id`, graphID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RefView
	for rows.Next() {
		var view RefView
		err := scanRefView(func(dest ...any) error {
			return rows.Scan(append([]any{&view.RefViewID}, dest...)...)
		}, &view)
		if err != nil {
			return nil, err
		}
		out = append(out, view)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// DeleteRefView removes a ref view. Its build attempts go with it through ON
// DELETE CASCADE; the generation it pointed at does not, because the pointer
// is the child side of the reference.
func (c *Catalog) DeleteRefView(ctx context.Context, refViewID string) error {
	if err := requireCatalogID("ref_view_id", refViewID); err != nil {
		return err
	}
	return c.deleteOne(ctx, fmt.Sprintf("ref view %s", refViewID),
		`DELETE FROM ref_views WHERE ref_view_id = ?`, refViewID)
}

// GetRefView returns one ref view.
func (c *Catalog) GetRefView(ctx context.Context, refViewID string) (RefView, bool, error) {
	view := RefView{RefViewID: refViewID}
	row := c.store.db.QueryRowContext(ctx,
		`SELECT `+refViewColumns+` FROM ref_views WHERE ref_view_id = ?`, refViewID)
	err := scanRefView(row.Scan, &view)
	if err == sql.ErrNoRows {
		return RefView{}, false, nil
	}
	if err != nil {
		return RefView{}, false, err
	}
	return view, true, nil
}

// --- ref view builds ----------------------------------------------------

const refViewBuildColumns = `ref_view_id, desired_ref, desired_commit, desired_tree,
	base_generation_id, enrichment_profile, build_fingerprint, generation_id,
	captured_route_epoch, state, build_token, created_at, last_progress, error`

// insertRefViewBuildSQL is the row insert both writers share.
const insertRefViewBuildSQL = `
INSERT INTO ref_view_builds (build_id, ` + refViewBuildColumns + `)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

// refViewBuildInsertArgs renders a build in insertRefViewBuildSQL's parameter
// order.
//
// base_generation_id is written as a plain integer, zero included: zero is the
// base corpus, which is a concrete layer to build over rather than an absent
// one. Storing it as NULL would put every build over the base corpus outside
// the coalescing index — SQLite compares NULLs as distinct — which is exactly
// the case where two selections of one branch would otherwise race.
func refViewBuildInsertArgs(build RefViewBuild) []any {
	return []any{
		build.BuildID, build.RefViewID, build.DesiredRef, build.DesiredCommit,
		build.DesiredTree, build.BaseGenerationID, build.EnrichmentProfile,
		build.BuildFingerprint, catalogNullInt(build.GenerationID),
		build.CapturedRouteEpoch, string(build.State), build.BuildToken,
		build.CreatedAt, build.LastProgress, build.Error,
	}
}

// UpsertRefViewBuild writes one build attempt. While the row is in the
// building state the partial unique index coalesces requests: a second attempt
// for the same ref view, tree, base and fingerprint fails rather than racing
// the first to produce the same generation twice.
func (c *Catalog) UpsertRefViewBuild(ctx context.Context, build RefViewBuild) error {
	if err := build.validate(); err != nil {
		return err
	}
	_, err := c.exec(ctx, insertRefViewBuildSQL+`
ON CONFLICT(build_id) DO UPDATE SET
  ref_view_id          = excluded.ref_view_id,
  desired_ref          = excluded.desired_ref,
  desired_commit       = excluded.desired_commit,
  desired_tree         = excluded.desired_tree,
  base_generation_id   = excluded.base_generation_id,
  enrichment_profile   = excluded.enrichment_profile,
  build_fingerprint    = excluded.build_fingerprint,
  generation_id        = excluded.generation_id,
  captured_route_epoch = excluded.captured_route_epoch,
  state                = excluded.state,
  build_token          = excluded.build_token,
  created_at           = excluded.created_at,
  last_progress        = excluded.last_progress,
  error                = excluded.error`,
		refViewBuildInsertArgs(build)...)
	return err
}

// abandonedRefViewBuildError is what a reclaimed attempt records as its cause.
const abandonedRefViewBuildError = "abandoned: the worker holding this claim stopped reporting progress"

// ClaimRefViewBuild starts one build attempt, or reports the attempt that is
// already running the same work.
//
// The partial unique index is the lock. A second claimer of the same ref view,
// tree, base generation and fingerprint has its insert refused by the index,
// and the row it collided with is read back inside the same transaction and
// returned alongside ErrRefViewBuildInFlight — so the loser gets the winner's
// build token instead of a bare failure, and never needs to guess whether the
// work it wanted is being done.
//
// A unique failure that no in-flight row explains is not coalescing: it is a
// caller reusing a build id or a build token, and it comes back unchanged.
//
// abandonedBefore is the liveness cutoff, in unix seconds. A colliding row
// whose last progress predates it is not a live claim but the wreckage of a
// worker that died holding one — a killed daemon, a canceled request — so it
// is failed and the new claim takes its place in the same transaction. Without
// that, the index would hand every later claimant of that tree a build nobody
// is running, forever. Zero disables the reclaim, and every collision
// coalesces.
func (c *Catalog) ClaimRefViewBuild(
	ctx context.Context,
	build RefViewBuild,
	abandonedBefore int64,
) (RefViewBuild, error) {
	if err := build.validate(); err != nil {
		return RefViewBuild{}, err
	}
	if build.State != ViewGenerationBuilding {
		return RefViewBuild{}, fmt.Errorf("%w: state %q, want %q",
			ErrCatalogInvalidValue, build.State, ViewGenerationBuilding)
	}
	var (
		inFlight  RefViewBuild
		coalesced bool
		reclaimed bool
	)
	err := c.withTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, insertRefViewBuildSQL, refViewBuildInsertArgs(build)...)
		if err == nil {
			return nil
		}
		if !isSQLiteUniqueViolation(err) {
			return err
		}
		row := tx.QueryRowContext(ctx, `
SELECT build_id, `+refViewBuildColumns+` FROM ref_view_builds
 WHERE ref_view_id = ? AND desired_tree = ? AND base_generation_id = ?
   AND build_fingerprint = ? AND state = ?`,
			build.RefViewID, build.DesiredTree, build.BaseGenerationID,
			build.BuildFingerprint, string(ViewGenerationBuilding))
		switch lookupErr := scanRefViewBuild(row.Scan, &inFlight); {
		case errors.Is(lookupErr, sql.ErrNoRows):
			return err
		case lookupErr != nil:
			return lookupErr
		}
		if abandonedBefore > 0 && inFlight.LastProgress < abandonedBefore {
			// Failing the dead attempt releases the index slot it held, so the
			// retry is an ordinary insert. A second reclaimer racing this one
			// collides with the fresh claim instead and coalesces on it.
			if _, failErr := tx.ExecContext(ctx, `
UPDATE ref_view_builds SET state = ?, last_progress = ?, error = ?
 WHERE build_id = ? AND state = ?`,
				string(ViewGenerationFailed), build.LastProgress, abandonedRefViewBuildError,
				inFlight.BuildID, string(ViewGenerationBuilding)); failErr != nil {
				return failErr
			}
			if _, retryErr := tx.ExecContext(ctx, insertRefViewBuildSQL, refViewBuildInsertArgs(build)...); retryErr != nil {
				return retryErr
			}
			reclaimed = true
			return nil
		}
		coalesced = true
		return nil
	})
	if err != nil {
		return RefViewBuild{}, err
	}
	if reclaimed {
		// Counted after the commit, and only here: the caller is handed a
		// plain claim either way, so a claim that took over a dead worker's
		// slot is invisible everywhere else — and it is the signal that a
		// build died holding one.
		viewmetrics.Count(viewmetrics.RefViewSelectionTotal, viewmetrics.RefViewReclaimed)
	}
	if coalesced {
		return inFlight, fmt.Errorf("%w: ref view %s at tree %s",
			ErrRefViewBuildInFlight, build.RefViewID, build.DesiredTree)
	}
	return build, nil
}

// CompleteRefViewBuild takes one attempt out of the building state. The token
// and the building state together are the guard, so an attempt is completed by
// its own worker and completed once; anything else reports ErrCatalogStaleGuard
// and leaves the row alone.
func (c *Catalog) CompleteRefViewBuild(ctx context.Context, req CompleteRefViewBuildRequest) error {
	if err := requireCatalogID("build_id", req.BuildID); err != nil {
		return err
	}
	if err := requireCatalogID("build_token", req.BuildToken); err != nil {
		return err
	}
	if err := requireCatalogValue("state", req.State, viewGenerationStates); err != nil {
		return err
	}
	if req.State == ViewGenerationBuilding {
		return fmt.Errorf("%w: completing a build into state %q", ErrCatalogInvalidValue, req.State)
	}
	return c.execGuarded(ctx, fmt.Sprintf("ref view build %s", req.BuildID), `
UPDATE ref_view_builds SET state = ?, generation_id = ?, last_progress = ?, error = ?
 WHERE build_id = ? AND build_token = ? AND state = ?`,
		string(req.State), catalogNullInt(req.GenerationID), req.LastProgress, req.Error,
		req.BuildID, req.BuildToken, string(ViewGenerationBuilding))
}

// TouchRefViewBuild stamps progress on one attempt that is still running.
//
// The liveness cutoff ClaimRefViewBuild applies reads last_progress and
// nothing else, so this is the only thing that tells a slow build from a dead
// one. The guard is the claim's: an attempt is stamped by the worker holding
// it, and only while it is still in the building state — re-stamping a
// finished or reclaimed attempt would resurrect a claim the coalescing index
// has already released.
func (c *Catalog) TouchRefViewBuild(ctx context.Context, buildID, buildToken string, lastProgress int64) error {
	if err := requireCatalogID("build_id", buildID); err != nil {
		return err
	}
	if err := requireCatalogID("build_token", buildToken); err != nil {
		return err
	}
	return c.execGuarded(ctx, fmt.Sprintf("ref view build %s", buildID), `
UPDATE ref_view_builds SET last_progress = ?
 WHERE build_id = ? AND build_token = ? AND state = ?`,
		lastProgress, buildID, buildToken, string(ViewGenerationBuilding))
}

// ListRefViewBuilds returns every attempt recorded for one ref view, oldest
// first. A view's build history is otherwise reachable only through a build id
// somebody is still holding, which the process that claimed the attempt stops
// holding the moment it dies.
func (c *Catalog) ListRefViewBuilds(ctx context.Context, refViewID string) ([]RefViewBuild, error) {
	rows, err := c.store.db.QueryContext(ctx,
		`SELECT build_id, `+refViewBuildColumns+` FROM ref_view_builds
		  WHERE ref_view_id = ? ORDER BY created_at, build_id`, refViewID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RefViewBuild
	for rows.Next() {
		var build RefViewBuild
		if err := scanRefViewBuild(rows.Scan, &build); err != nil {
			return nil, err
		}
		out = append(out, build)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// isSQLiteUniqueViolation reports whether err is SQLite refusing a write for a
// uniqueness reason. The two extended codes are the only ones a duplicate row
// can raise; every other constraint failure (a foreign key, a NOT NULL) is a
// different bug and must not be read as coalescing.
func isSQLiteUniqueViolation(err error) bool {
	var sqliteErr *sqlite.Error
	if !errors.As(err, &sqliteErr) {
		return false
	}
	switch sqliteErr.Code() {
	case sqliteConstraintUnique, sqliteConstraintPrimaryKey:
		return true
	default:
		return false
	}
}

// SQLite extended result codes for a duplicate row.
const (
	sqliteConstraintPrimaryKey = 1555
	sqliteConstraintUnique     = 2067
)

// scanRefViewBuild reads a "build_id, refViewBuildColumns" projection in order.
func scanRefViewBuild(scan func(...any) error, build *RefViewBuild) error {
	var (
		baseGeneration, generation sql.NullInt64
		state                      string
	)
	if err := scan(
		&build.BuildID, &build.RefViewID, &build.DesiredRef, &build.DesiredCommit,
		&build.DesiredTree, &baseGeneration, &build.EnrichmentProfile,
		&build.BuildFingerprint, &generation, &build.CapturedRouteEpoch, &state,
		&build.BuildToken, &build.CreatedAt, &build.LastProgress, &build.Error); err != nil {
		return err
	}
	build.BaseGenerationID = baseGeneration.Int64
	build.GenerationID = generation.Int64
	build.State = ViewGenerationState(state)
	return nil
}

// RefViewBuildKey names one coalescing slot: the four columns the partial
// unique index on the in-flight builds is keyed by.
type RefViewBuildKey struct {
	RefViewID        string
	DesiredTree      string
	BaseGenerationID int64
	BuildFingerprint string
}

// InFlightRefViewBuild returns the attempt holding one coalescing slot.
//
// It is the row ClaimRefViewBuild collides with, read instead of collided
// with, and it answers on the read pool. That is the whole of why it exists:
// a claim is an upsert on the writer, and while a build runs the writer is
// saturated by that build's own transactions, so a selection that wants
// nothing but the token to poll must not have to queue behind the build it is
// about to report.
//
// aliveAfter is the liveness cutoff ClaimRefViewBuild would apply. An attempt
// that has not stamped progress since is one the next claim reclaims, so it is
// not reported in flight here either — handing back a dead build's token is
// exactly the wedge the reclaim exists to break. A cutoff at or below zero
// disables the check, as it does on the claim.
func (c *Catalog) InFlightRefViewBuild(
	ctx context.Context,
	key RefViewBuildKey,
	aliveAfter int64,
) (RefViewBuild, bool, error) {
	var build RefViewBuild
	row := c.store.db.QueryRowContext(ctx, `
SELECT build_id, `+refViewBuildColumns+` FROM ref_view_builds
 WHERE ref_view_id = ? AND desired_tree = ? AND base_generation_id = ?
   AND build_fingerprint = ? AND state = ?`,
		key.RefViewID, key.DesiredTree, key.BaseGenerationID,
		key.BuildFingerprint, string(ViewGenerationBuilding))
	err := scanRefViewBuild(row.Scan, &build)
	if errors.Is(err, sql.ErrNoRows) {
		return RefViewBuild{}, false, nil
	}
	if err != nil {
		return RefViewBuild{}, false, err
	}
	if aliveAfter > 0 && build.LastProgress < aliveAfter {
		return build, false, nil
	}
	return build, true, nil
}

// GetRefViewBuild returns one build attempt.
func (c *Catalog) GetRefViewBuild(ctx context.Context, buildID string) (RefViewBuild, bool, error) {
	var build RefViewBuild
	row := c.store.db.QueryRowContext(ctx,
		`SELECT build_id, `+refViewBuildColumns+` FROM ref_view_builds WHERE build_id = ?`, buildID)
	err := scanRefViewBuild(row.Scan, &build)
	if errors.Is(err, sql.ErrNoRows) {
		return RefViewBuild{}, false, nil
	}
	if err != nil {
		return RefViewBuild{}, false, err
	}
	return build, true, nil
}

// --- cleanup journal ----------------------------------------------------

// UpsertCleanupEntry writes one deferred-deletion record. The journal has no
// foreign keys, so an entry outlives the rows it names.
func (c *Catalog) UpsertCleanupEntry(ctx context.Context, entry CleanupEntry) error {
	if err := entry.validate(); err != nil {
		return err
	}
	_, err := c.exec(ctx, `
INSERT INTO cleanup_journal
  (cleanup_id, opaque_target_ids, reason, phase, grace_deadline, primary_epoch, last_progress, last_error)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(cleanup_id) DO UPDATE SET
  opaque_target_ids = excluded.opaque_target_ids,
  reason            = excluded.reason,
  phase             = excluded.phase,
  grace_deadline    = excluded.grace_deadline,
  primary_epoch     = excluded.primary_epoch,
  last_progress     = excluded.last_progress,
  last_error        = excluded.last_error`,
		entry.CleanupID, entry.OpaqueTargetIDs, entry.Reason, string(entry.Phase),
		entry.GraceDeadline, entry.PrimaryEpoch, entry.LastProgress, entry.LastError)
	return err
}

// GetCleanupEntry returns one deferred-deletion record.
func (c *Catalog) GetCleanupEntry(ctx context.Context, cleanupID string) (CleanupEntry, bool, error) {
	entry := CleanupEntry{CleanupID: cleanupID}
	var phase string
	err := c.store.db.QueryRowContext(ctx, `
SELECT opaque_target_ids, reason, phase, grace_deadline, primary_epoch, last_progress, last_error
  FROM cleanup_journal WHERE cleanup_id = ?`, cleanupID).Scan(
		&entry.OpaqueTargetIDs, &entry.Reason, &phase, &entry.GraceDeadline,
		&entry.PrimaryEpoch, &entry.LastProgress, &entry.LastError)
	if err == sql.ErrNoRows {
		return CleanupEntry{}, false, nil
	}
	if err != nil {
		return CleanupEntry{}, false, err
	}
	entry.Phase = CleanupPhase(phase)
	return entry, true, nil
}

// ListCleanupEntries returns the whole journal, ordered by cleanup id. It is
// the recovery read: after a restart nobody knows which deletions were left
// half-done, so the resume pass enumerates the journal rather than addressing
// entries it would have to remember the ids of.
func (c *Catalog) ListCleanupEntries(ctx context.Context) ([]CleanupEntry, error) {
	rows, err := c.store.db.QueryContext(ctx, `
SELECT cleanup_id, opaque_target_ids, reason, phase, grace_deadline, primary_epoch,
       last_progress, last_error
  FROM cleanup_journal ORDER BY cleanup_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CleanupEntry
	for rows.Next() {
		var (
			entry CleanupEntry
			phase string
		)
		if err := rows.Scan(&entry.CleanupID, &entry.OpaqueTargetIDs, &entry.Reason, &phase,
			&entry.GraceDeadline, &entry.PrimaryEpoch, &entry.LastProgress, &entry.LastError); err != nil {
			return nil, err
		}
		entry.Phase = CleanupPhase(phase)
		out = append(out, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// DeleteCleanupEntry removes one journal row. A cleanup deletes its own entry
// last, so the entry's absence is the record that the work finished.
func (c *Catalog) DeleteCleanupEntry(ctx context.Context, cleanupID string) error {
	if err := requireCatalogID("cleanup_id", cleanupID); err != nil {
		return err
	}
	return c.deleteOne(ctx, fmt.Sprintf("cleanup entry %s", cleanupID),
		`DELETE FROM cleanup_journal WHERE cleanup_id = ?`, cleanupID)
}
