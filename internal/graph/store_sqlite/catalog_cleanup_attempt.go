package store_sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
)

const cleanupAttemptJSONKey = "cleanup_attempt_id"

func cleanupAttemptID(entry CleanupEntry) (string, error) {
	var envelope struct {
		ID string `json:"cleanup_attempt_id"`
	}
	if err := json.Unmarshal([]byte(entry.OpaqueTargetIDs), &envelope); err != nil {
		return "", err
	}
	return envelope.ID, nil
}

func requireCleanupAttempt(entry CleanupEntry, expected string) error {
	id, err := cleanupAttemptID(entry)
	if err != nil || expected == "" || id != expected {
		return fmt.Errorf("%w: cleanup %s attempt changed", ErrCatalogStaleGuard, entry.CleanupID)
	}
	return nil
}

func setCleanupAttempt(entry CleanupEntry, id string) (CleanupEntry, error) {
	if id == "" {
		return CleanupEntry{}, fmt.Errorf("%w: empty cleanup attempt", ErrCatalogStaleGuard)
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal([]byte(entry.OpaqueTargetIDs), &payload); err != nil {
		return CleanupEntry{}, err
	}
	if payload == nil {
		return CleanupEntry{}, fmt.Errorf("%w: cleanup target is not an object", ErrCatalogStaleGuard)
	}
	encoded, err := json.Marshal(id)
	if err != nil {
		return CleanupEntry{}, err
	}
	payload[cleanupAttemptJSONKey] = encoded
	encoded, err = json.Marshal(payload)
	if err != nil {
		return CleanupEntry{}, err
	}
	entry.OpaqueTargetIDs = string(encoded)
	return entry, nil
}

// CreateCleanupAttempt is insert-only. A caller which lost the journal race
// must reload/re-authorize; it cannot overwrite the winning cleanup attempt.
// Generic UpsertCleanupEntry remains available for its existing callers.
func (c *Catalog) CreateCleanupAttempt(ctx context.Context, entry CleanupEntry, expected string) error {
	if err := entry.validate(); err != nil {
		return err
	}
	if err := requireCleanupAttempt(entry, expected); err != nil {
		return err
	}
	return c.withTx(ctx, func(tx *sql.Tx) error {
		_, found, err := cleanupEntryTx(ctx, tx, entry.CleanupID)
		if err != nil {
			return err
		}
		if found {
			return fmt.Errorf("%w: cleanup %s already exists", ErrCatalogStaleGuard, entry.CleanupID)
		}
		return insertCleanupEntryTx(ctx, tx, entry)
	})
}

// UpgradeCleanupAttempt binds an exact legacy journal snapshot once. Missing,
// replaced, or already token-bearing rows are stale, never a reason to insert
// or adopt a replacement. Unknown opaque JSON members are preserved.
func (c *Catalog) UpgradeCleanupAttempt(ctx context.Context, snapshot CleanupEntry, attemptID string) (CleanupEntry, error) {
	var out CleanupEntry
	err := c.withTx(ctx, func(tx *sql.Tx) error {
		current, found, err := cleanupEntryTx(ctx, tx, snapshot.CleanupID)
		if err != nil {
			return err
		}
		if !found || !sameCleanupSnapshot(current, snapshot) {
			return fmt.Errorf("%w: legacy cleanup %s moved", ErrCatalogStaleGuard, snapshot.CleanupID)
		}
		id, err := cleanupAttemptID(current)
		if err != nil {
			return err
		}
		if id != "" {
			return fmt.Errorf("%w: cleanup %s already has an attempt", ErrCatalogStaleGuard, snapshot.CleanupID)
		}
		out, err = setCleanupAttempt(current, attemptID)
		if err != nil {
			return err
		}
		return replaceCleanupSnapshotTx(ctx, tx, current, out)
	})
	return out, err
}

func sameCleanupSnapshot(a, b CleanupEntry) bool {
	return a.CleanupID == b.CleanupID && a.OpaqueTargetIDs == b.OpaqueTargetIDs &&
		a.Reason == b.Reason && a.Phase == b.Phase && a.GraceDeadline == b.GraceDeadline &&
		a.PrimaryEpoch == b.PrimaryEpoch && a.LastProgress == b.LastProgress && a.LastError == b.LastError
}

// GetCleanupAttempt is a read-only admission recheck after the execution gate.
func (c *Catalog) GetCleanupAttempt(ctx context.Context, cleanupID, expected string) (CleanupEntry, error) {
	entry, found, err := c.GetCleanupEntry(ctx, cleanupID)
	if err != nil {
		return CleanupEntry{}, err
	}
	if !found {
		return CleanupEntry{}, fmt.Errorf("%w: cleanup %s completed", ErrCatalogStaleGuard, cleanupID)
	}
	if err := requireCleanupAttempt(entry, expected); err != nil {
		return CleanupEntry{}, err
	}
	return entry, nil
}

// AdvanceCleanupAttempt never creates a journal. The load, token comparison,
// and write share the mutation gate and one SQLite transaction.
func (c *Catalog) AdvanceCleanupAttempt(ctx context.Context, entry CleanupEntry, expected string) error {
	if err := entry.validate(); err != nil {
		return err
	}
	if err := requireCleanupAttempt(entry, expected); err != nil {
		return err
	}
	return c.withTx(ctx, func(tx *sql.Tx) error {
		current, found, err := cleanupEntryTx(ctx, tx, entry.CleanupID)
		if err != nil {
			return err
		}
		if !found {
			return fmt.Errorf("%w: cleanup %s completed", ErrCatalogStaleGuard, entry.CleanupID)
		}
		if err := requireCleanupAttempt(current, expected); err != nil {
			return err
		}
		return replaceCleanupSnapshotTx(ctx, tx, current, entry)
	})
}

func replaceCleanupSnapshotTx(ctx context.Context, tx *sql.Tx, previous, entry CleanupEntry) error {
	result, err := tx.ExecContext(ctx, `
UPDATE cleanup_journal SET opaque_target_ids=?, reason=?, phase=?, grace_deadline=?,
  primary_epoch=?, last_progress=?, last_error=?
WHERE cleanup_id=? AND opaque_target_ids=?`,
		entry.OpaqueTargetIDs, entry.Reason, string(entry.Phase), entry.GraceDeadline,
		entry.PrimaryEpoch, entry.LastProgress, entry.LastError,
		entry.CleanupID, previous.OpaqueTargetIDs)
	if err != nil {
		return err
	}
	return requireCleanupMutation(result, entry.CleanupID)
}

func requireCleanupMutation(result sql.Result, cleanupID string) error {
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return fmt.Errorf("%w: cleanup %s changed", ErrCatalogStaleGuard, cleanupID)
	}
	return nil
}

// DeleteCleanupAttempt removes only this attempt, not a successor at the same
// deterministic ID. Missing is stale, not successful completion by this actor.
func (c *Catalog) DeleteCleanupAttempt(ctx context.Context, cleanupID, expected string) error {
	if err := requireCatalogID("cleanup_id", cleanupID); err != nil {
		return err
	}
	return c.withTx(ctx, func(tx *sql.Tx) error {
		current, found, err := cleanupEntryTx(ctx, tx, cleanupID)
		if err != nil {
			return err
		}
		if !found {
			return fmt.Errorf("%w: cleanup %s completed", ErrCatalogStaleGuard, cleanupID)
		}
		if err := requireCleanupAttempt(current, expected); err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx, `DELETE FROM cleanup_journal WHERE cleanup_id=? AND opaque_target_ids=?`, cleanupID, current.OpaqueTargetIDs)
		if err != nil {
			return err
		}
		return requireCleanupMutation(result, cleanupID)
	})
}

// Called inside the existing authorization transaction. New reconcile callers
// propose a token; old generic catalog callers with opaque/tokenless payloads
// keep their compatibility behavior. A legacy row is upgraded once without
// changing its persisted phase or target identifiers.
func authorizeCleanupAttemptTx(ctx context.Context, tx *sql.Tx, existing, proposed CleanupEntry) (CleanupEntry, error) {
	requested, err := cleanupAttemptID(proposed)
	if err != nil || requested == "" {
		return existing, nil
	}
	current, err := cleanupAttemptID(existing)
	if err != nil {
		return CleanupEntry{}, err
	}
	if current != "" {
		return existing, nil
	}
	upgraded, err := setCleanupAttempt(existing, requested)
	if err != nil {
		return CleanupEntry{}, err
	}
	if err := replaceCleanupSnapshotTx(ctx, tx, existing, upgraded); err != nil {
		return CleanupEntry{}, err
	}
	return upgraded, nil
}
