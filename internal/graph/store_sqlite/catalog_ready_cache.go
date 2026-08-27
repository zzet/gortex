package store_sqlite

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
)

const defaultReadyGenerationLeaseTTL = 5 * time.Minute

var (
	errReadyGenerationLeaseConflict = errors.New("ready generation lease token already names another generation")
)

// ReadyGenerationCacheKey is the immutable content identity of a reusable
// generation. Route ownership and selector provenance are intentionally absent:
// compatible worktrees and ref aliases inside one logical graph may share the
// payload, while GraphID and the exact base keep independently dedicated graphs
// and incompatible ancestry isolated.
type ReadyGenerationCacheKey struct {
	GraphID              string
	BaseGenerationID     int64
	TreeOID              string
	IndexConfigHash      string
	ExtractorFingerprint string
	SchemaPipelineEpoch  string
}

// ClaimReadyGenerationRequest asks the catalog to pin the canonical ready
// generation for Key. CandidateGenerationID is zero for a pre-build cache
// lookup. After a caller publishes a new ready generation it supplies that id;
// concurrent publishers then converge on the oldest compatible ready winner.
//
// LeaseToken is optional but should be stable across a caller retry. The
// catalog derives and renews its short expiry from SQLite's clock so the delete
// guard and the claimant cannot disagree about whether the handoff is live.
type ClaimReadyGenerationRequest struct {
	Key                   ReadyGenerationCacheKey
	CandidateGenerationID int64
	LeaseToken            string
}

// ReadyGenerationClaim pins WinnerGenerationID against deletion until the
// caller atomically binds a route/ref owner or releases LeaseToken. A future
// route-publication transaction must validate and consume this token in the
// same transaction as its route CAS; publishing only the returned id would
// reintroduce a deletion race.
type ReadyGenerationClaim struct {
	WinnerGenerationID int64
	LeaseToken         string
	ExpiresAt          int64
	Reused             bool
	RetiredCandidate   bool
}

// ClaimReadyGeneration finds one canonical, reusable ready generation and
// acquires a durable handoff lease before returning it. It returns found=false
// on a cold miss. The generation itself must be ready, and every ancestor must
// still be structurally live (ready or superseded) in the same logical graph.
func (c *Catalog) ClaimReadyGeneration(
	ctx context.Context,
	req ClaimReadyGenerationRequest,
) (claim ReadyGenerationClaim, found bool, err error) {
	if err := validateReadyGenerationCacheKey(req.Key); err != nil {
		return ReadyGenerationClaim{}, false, err
	}
	if req.CandidateGenerationID < 0 {
		return ReadyGenerationClaim{}, false, fmt.Errorf("candidate generation id must not be negative")
	}
	token := req.LeaseToken
	if token == "" {
		token, err = newReadyGenerationLeaseToken()
		if err != nil {
			return ReadyGenerationClaim{}, false, err
		}
	}

	core := c.store.storeCore
	if err := core.writeMu.LockContext(ctx); err != nil {
		return ReadyGenerationClaim{}, false, err
	}
	defer core.writeMu.Unlock()
	if err := ensureReadyGenerationCacheSchema(ctx, core); err != nil {
		return ReadyGenerationClaim{}, false, err
	}

	tx, err := core.writerDB.BeginTx(ctx, nil)
	if err != nil {
		return ReadyGenerationClaim{}, false, err
	}
	defer func() { _ = tx.Rollback() }()
	var now int64
	if err := tx.QueryRowContext(ctx, `SELECT unixepoch()`).Scan(&now); err != nil {
		return ReadyGenerationClaim{}, false, fmt.Errorf("read ready generation lease clock: %w", err)
	}
	expiresAt := now + int64(defaultReadyGenerationLeaseTTL/time.Second)
	if _, err := tx.ExecContext(ctx, `DELETE FROM ready_generation_leases WHERE expires_at <= ?`, now); err != nil {
		return ReadyGenerationClaim{}, false, fmt.Errorf("prune expired ready generation leases: %w", err)
	}

	if req.CandidateGenerationID != 0 {
		valid, err := candidateMatchesReadyGenerationKey(ctx, tx, req.CandidateGenerationID, req.Key)
		if err != nil {
			return ReadyGenerationClaim{}, false, err
		}
		if !valid {
			return ReadyGenerationClaim{}, false, fmt.Errorf(
				"candidate generation %d is not compatible and live for the requested ready cache key",
				req.CandidateGenerationID,
			)
		}
	}

	winner, err := canonicalReadyGeneration(ctx, tx, req.Key)
	if err != nil {
		return ReadyGenerationClaim{}, false, err
	}
	if winner == 0 {
		if err := tx.Commit(); err != nil {
			return ReadyGenerationClaim{}, false, err
		}
		return ReadyGenerationClaim{}, false, nil
	}

	var leasedGeneration int64
	var leasedExpiry int64
	leaseErr := tx.QueryRowContext(ctx, `
		SELECT generation_id, expires_at
		FROM ready_generation_leases
		WHERE lease_token = ?
	`, token).Scan(&leasedGeneration, &leasedExpiry)
	switch {
	case leaseErr == nil:
		if leasedGeneration != winner {
			return ReadyGenerationClaim{}, false, errReadyGenerationLeaseConflict
		}
		if leasedExpiry < expiresAt {
			if _, err := tx.ExecContext(ctx, `
				UPDATE ready_generation_leases SET expires_at = ? WHERE lease_token = ?
			`, expiresAt, token); err != nil {
				return ReadyGenerationClaim{}, false, fmt.Errorf("renew ready generation lease: %w", err)
			}
		} else {
			expiresAt = leasedExpiry
		}
	case errors.Is(leaseErr, sql.ErrNoRows):
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO ready_generation_leases(
				lease_token, generation_id, graph_id, created_at, expires_at
			) VALUES (?, ?, ?, ?, ?)
		`, token, winner, req.Key.GraphID, now, expiresAt); err != nil {
			return ReadyGenerationClaim{}, false, fmt.Errorf("acquire ready generation lease: %w", err)
		}
	default:
		return ReadyGenerationClaim{}, false, leaseErr
	}

	retired := false
	if req.CandidateGenerationID != 0 && req.CandidateGenerationID != winner {
		result, err := tx.ExecContext(ctx, `
			UPDATE view_generations
			SET state = 'superseded'
			WHERE generation_id = ?
			  AND state = 'ready'
			  AND NOT EXISTS (
				SELECT 1 FROM checkout_routes
				WHERE commit_generation_id = ? OR dirty_generation_id = ?
			  )
			  AND NOT EXISTS (
				SELECT 1 FROM ref_views WHERE active_generation_id = ?
			  )
			  AND NOT EXISTS (
				SELECT 1 FROM dedicated_graphs WHERE active_generation_id = ?
			  )
			  AND NOT EXISTS (
				SELECT 1 FROM view_generations WHERE base_generation_id = ?
			  )
		`, req.CandidateGenerationID, req.CandidateGenerationID, req.CandidateGenerationID,
			req.CandidateGenerationID, req.CandidateGenerationID, req.CandidateGenerationID)
		if err != nil {
			return ReadyGenerationClaim{}, false, fmt.Errorf("retire redundant ready generation: %w", err)
		}
		retiredRows, err := result.RowsAffected()
		if err != nil {
			return ReadyGenerationClaim{}, false, err
		}
		retired = retiredRows == 1
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE view_generations
		SET last_selected = CASE WHEN last_selected < ? THEN ? ELSE last_selected END
		WHERE generation_id = ?
	`, now, now, winner); err != nil {
		return ReadyGenerationClaim{}, false, fmt.Errorf("touch ready generation winner: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return ReadyGenerationClaim{}, false, err
	}
	return ReadyGenerationClaim{
		WinnerGenerationID: winner,
		LeaseToken:         token,
		ExpiresAt:          expiresAt,
		Reused:             req.CandidateGenerationID == 0 || req.CandidateGenerationID != winner,
		RetiredCandidate:   retired,
	}, true, nil
}

// ReleaseReadyGenerationLease drops a handoff lease after the caller either
// binds a durable owner or abandons the adoption attempt. It is idempotent.
func (c *Catalog) ReleaseReadyGenerationLease(ctx context.Context, token string) error {
	if token == "" {
		return nil
	}
	core := c.store.storeCore
	if err := core.writeMu.LockContext(ctx); err != nil {
		return err
	}
	defer core.writeMu.Unlock()
	if err := ensureReadyGenerationCacheSchema(ctx, core); err != nil {
		return err
	}
	tx, err := core.writerDB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `DELETE FROM ready_generation_leases WHERE lease_token = ?`, token); err != nil {
		return err
	}
	return tx.Commit()
}

func validateReadyGenerationCacheKey(key ReadyGenerationCacheKey) error {
	switch {
	case key.GraphID == "":
		return fmt.Errorf("ready generation cache graph id is required")
	case key.BaseGenerationID < 0:
		return fmt.Errorf("ready generation cache base generation id must not be negative")
	case key.TreeOID == "":
		return fmt.Errorf("ready generation cache tree oid is required")
	case key.IndexConfigHash == "":
		return fmt.Errorf("ready generation cache index config hash is required")
	case key.ExtractorFingerprint == "":
		return fmt.Errorf("ready generation cache extractor fingerprint is required")
	case key.SchemaPipelineEpoch == "":
		return fmt.Errorf("ready generation cache schema/pipeline epoch is required")
	default:
		return nil
	}
}

func ensureReadyGenerationCacheSchema(ctx context.Context, core *storeCore) error {
	statements := []string{
		`CREATE TABLE IF NOT EXISTS ready_generation_leases (
			lease_token   TEXT PRIMARY KEY,
			generation_id INTEGER NOT NULL
				REFERENCES view_generations(generation_id) ON DELETE CASCADE,
			graph_id      TEXT NOT NULL,
			created_at    INTEGER NOT NULL,
			expires_at    INTEGER NOT NULL
		) WITHOUT ROWID`,
		`CREATE INDEX IF NOT EXISTS ready_generation_leases_by_expiry
			ON ready_generation_leases(expires_at, lease_token)`,
		`CREATE TRIGGER IF NOT EXISTS ready_generation_leases_protect_delete
			BEFORE DELETE ON view_generations
			WHEN EXISTS (
				SELECT 1 FROM ready_generation_leases
				WHERE generation_id = OLD.generation_id
				  AND expires_at > unixepoch()
			)
			BEGIN
				SELECT RAISE(ABORT, 'ready generation has an active handoff lease');
			END`,
		`CREATE INDEX IF NOT EXISTS view_generations_ready_cache
			ON view_generations(
				graph_id,
				COALESCE(base_generation_id, 0),
				tree_oid,
				config_hash,
				extractor_versions,
				resolver_version,
				generation_id
			)
			WHERE state = 'ready'`,
	}
	for _, statement := range statements {
		if _, err := core.writerDB.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("ensure ready generation cache schema: %w", err)
		}
	}
	return nil
}

func canonicalReadyGeneration(ctx context.Context, tx *sql.Tx, key ReadyGenerationCacheKey) (int64, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT generation_id
		FROM view_generations
		WHERE graph_id = ?
		  AND COALESCE(base_generation_id, 0) = ?
		  AND tree_oid = ?
		  AND config_hash = ?
		  AND extractor_versions = ?
		  AND resolver_version = ?
		  AND state = 'ready'
		ORDER BY generation_id ASC
	`, key.GraphID, key.BaseGenerationID, key.TreeOID, key.IndexConfigHash,
		key.ExtractorFingerprint, key.SchemaPipelineEpoch)
	if err != nil {
		return 0, err
	}
	var candidates []int64
	for rows.Next() {
		var generationID int64
		if err := rows.Scan(&generationID); err != nil {
			_ = rows.Close()
			return 0, err
		}
		candidates = append(candidates, generationID)
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	for _, generationID := range candidates {
		live, err := readyGenerationAncestryLive(ctx, tx, generationID, key.GraphID, true)
		if err != nil {
			return 0, err
		}
		if live {
			return generationID, nil
		}
	}
	return 0, nil
}

func candidateMatchesReadyGenerationKey(
	ctx context.Context,
	tx *sql.Tx,
	generationID int64,
	key ReadyGenerationCacheKey,
) (bool, error) {
	var graphID, treeOID, configHash, extractorFingerprint, pipelineEpoch, state string
	var baseGenerationID int64
	err := tx.QueryRowContext(ctx, `
		SELECT graph_id, COALESCE(base_generation_id, 0), tree_oid, config_hash,
		       extractor_versions, resolver_version, state
		FROM view_generations
		WHERE generation_id = ?
	`, generationID).Scan(&graphID, &baseGenerationID, &treeOID, &configHash,
		&extractorFingerprint, &pipelineEpoch, &state)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if graphID != key.GraphID || baseGenerationID != key.BaseGenerationID ||
		treeOID != key.TreeOID || configHash != key.IndexConfigHash ||
		extractorFingerprint != key.ExtractorFingerprint || pipelineEpoch != key.SchemaPipelineEpoch ||
		(state != string(ViewGenerationReady) && state != string(ViewGenerationSuperseded)) {
		return false, nil
	}
	return readyGenerationAncestryLive(ctx, tx, generationID, key.GraphID, false)
}

func readyGenerationAncestryLive(
	ctx context.Context,
	tx *sql.Tx,
	generationID int64,
	graphID string,
	rootMustBeReady bool,
) (bool, error) {
	seen := make(map[int64]struct{})
	root := true
	for generationID != 0 {
		if _, duplicate := seen[generationID]; duplicate {
			return false, nil
		}
		seen[generationID] = struct{}{}
		var rowGraphID, state string
		var baseGenerationID int64
		err := tx.QueryRowContext(ctx, `
			SELECT graph_id, COALESCE(base_generation_id, 0), state
			FROM view_generations
			WHERE generation_id = ?
		`, generationID).Scan(&rowGraphID, &baseGenerationID, &state)
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		if rowGraphID != graphID {
			return false, nil
		}
		if root && rootMustBeReady {
			if state != string(ViewGenerationReady) {
				return false, nil
			}
		} else if state != string(ViewGenerationReady) && state != string(ViewGenerationSuperseded) {
			return false, nil
		}
		root = false
		generationID = baseGenerationID
	}
	return true, nil
}

func newReadyGenerationLeaseToken() (string, error) {
	var bytes [16]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return "", fmt.Errorf("create ready generation lease token: %w", err)
	}
	return hex.EncodeToString(bytes[:]), nil
}
