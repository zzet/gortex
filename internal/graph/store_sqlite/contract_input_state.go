package store_sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"github.com/zzet/gortex/internal/graph"
)

var _ graph.ContractInputStateReader = (*Store)(nil)

func validateContractInput(state graph.ContractInputState) error {
	if state.InputVersion == "" || state.InputFingerprint == "" {
		return fmt.Errorf("%w: empty contract input identity", ErrCatalogInvalidValue)
	}
	return nil
}

// ContractInputStateContext reads ONLY this physical generation. A pending
// primary mutation deliberately has no usable identity until accepted.
func (s *Store) ContractInputStateContext(ctx context.Context, repo, checkout string) (graph.ContractInputState, bool, error) {
	var state graph.ContractInputState
	if ctx == nil {
		return state, false, fmt.Errorf("%w: nil context", ErrCatalogInvalidValue)
	}
	if err := ctx.Err(); err != nil {
		return state, false, err
	}
	if s.coreless() || s.db == nil {
		return state, false, sql.ErrConnDone
	}
	state.RepoPrefix, state.CheckoutID = repo, checkout
	var accepted bool
	err := s.db.QueryRowContext(ctx, `SELECT input_version,input_fingerprint,accepted,previous_input_version,previous_input_fingerprint FROM generation_contract_input_state WHERE view_gen=? AND repo_prefix=? AND checkout_id=?`, s.viewGen, repo, checkout).Scan(&state.InputVersion, &state.InputFingerprint, &accepted, &state.PreviousInputVersion, &state.PreviousInputFingerprint)
	if errors.Is(err, sql.ErrNoRows) {
		return state, false, nil
	}
	if err != nil {
		return graph.ContractInputState{}, false, err
	}
	state.Accepted = accepted
	if err := validateContractInput(state); err != nil {
		return graph.ContractInputState{}, false, err
	}
	return state, true, nil
}

// SetContractInputStateWithWorkContext stages accepted state and immutable debt
// in one payload transaction. Primary callers must use the begin/accept protocol
// around existing core mutations instead of accepting ahead of those writes.
func (s *Store) SetContractInputStateWithWorkContext(ctx context.Context, expected *graph.ContractInputState, next graph.ContractInputState, work []graph.ContractWork) error {
	if s.viewGen == 0 {
		return fmt.Errorf("%w: primary contract inputs need begin/accept", ErrCatalogInvalidValue)
	}
	return s.setContractInputStateWithWork(ctx, expected, next, work, true)
}

// BeginContractInputMutationContext blocks analysis durably before primary core
// writes. The caller must serialize its whole apply interval per affected actor;
// failure leaves this identity unavailable until explicit reconciliation.
func (s *Store) BeginContractInputMutationContext(ctx context.Context, expected *graph.ContractInputState, next graph.ContractInputState, work []graph.ContractWork) error {
	if s.viewGen != 0 {
		return fmt.Errorf("%w: pending mutation requires primary generation", ErrCatalogInvalidValue)
	}
	return s.setContractInputStateWithWork(ctx, expected, next, work, false)
}

func (s *Store) setContractInputStateWithWork(ctx context.Context, expected *graph.ContractInputState, next graph.ContractInputState, work []graph.ContractWork, accepted bool) error {
	if ctx == nil {
		return fmt.Errorf("%w: nil context", ErrCatalogInvalidValue)
	}
	if err := validateContractInput(next); err != nil {
		return err
	}
	if len(work) > contractWorkReadLimit {
		return ErrContractWorkLimit
	}
	if expected != nil && (expected.RepoPrefix != next.RepoPrefix || expected.CheckoutID != next.CheckoutID) {
		return fmt.Errorf("%w: contract input namespace changed", ErrCatalogInvalidValue)
	}
	for _, row := range work {
		if row.RepoPrefix != next.RepoPrefix || row.CheckoutID != next.CheckoutID || row.State != graph.ContractWorkPending {
			return fmt.Errorf("%w: contract debt namespace/state", ErrCatalogInvalidValue)
		}
	}
	encoded, err := encodeContractWork(work)
	if err != nil {
		return err
	}
	release, err := s.HoldWriteGate(ctx)
	if err != nil {
		return err
	}
	defer release()
	tx, err := s.beginWriteContext(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := s.setContractInputStateWithWorkTx(ctx, tx, expected, next, work, encoded, accepted); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) setContractInputStateWithWorkTx(ctx context.Context, tx *sql.Tx, expected *graph.ContractInputState, next graph.ContractInputState, work []graph.ContractWork, encoded [][]byte, accepted bool) error {
	// Missing physical rows inherit only explicit catalog ancestry, never an
	// actor's latest input. Topmost present rows win, including pending blockers.
	generation := s.viewGen
	found := false
	var actual graph.ContractInputState
	var oldAccepted bool
	for steps := 0; steps < 1024; steps++ {
		actual.RepoPrefix, actual.CheckoutID = next.RepoPrefix, next.CheckoutID
		err := tx.QueryRowContext(ctx, `SELECT input_version,input_fingerprint,accepted,previous_input_version,previous_input_fingerprint FROM generation_contract_input_state WHERE view_gen=? AND repo_prefix=? AND checkout_id=?`, generation, next.RepoPrefix, next.CheckoutID).Scan(&actual.InputVersion, &actual.InputFingerprint, &oldAccepted, &actual.PreviousInputVersion, &actual.PreviousInputFingerprint)
		if err == nil {
			actual.Accepted = oldAccepted
			found = true
			break
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if generation == 0 {
			break
		}
		var base sql.NullInt64
		if err := tx.QueryRowContext(ctx, `SELECT base_generation_id FROM view_generations WHERE generation_id=?`, generation).Scan(&base); err != nil {
			return err
		}
		if !base.Valid {
			break
		}
		if base.Int64 >= generation || base.Int64 < 0 {
			return ErrCatalogStaleGuard
		}
		generation = base.Int64
		if steps == 1023 {
			return fmt.Errorf("%w: contract input ancestry limit", ErrCatalogInvalidValue)
		}
	}
	if expected == nil {
		if found {
			return ErrCatalogStaleGuard
		}
	} else if !found || oldAccepted != expected.Accepted || !sameContractInput(actual, *expected) {
		return ErrCatalogStaleGuard
	}
	previousVersion, previousFingerprint := actual.PreviousInputVersion, actual.PreviousInputFingerprint
	if found {
		var published bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM contract_attachments a JOIN view_generations v ON v.generation_id=a.payload_generation WHERE a.repo_prefix=? AND a.checkout_id=? AND a.input_version=? AND a.input_fingerprint=? AND v.state IN ('ready','superseded')) OR EXISTS(SELECT 1 FROM contract_attachment_inputs x JOIN contract_attachments a ON a.repo_prefix=x.repo_prefix AND a.checkout_id=x.checkout_id AND a.input_version=x.input_version AND a.input_fingerprint=x.input_fingerprint JOIN view_generations v ON v.generation_id=a.payload_generation WHERE x.source_repo=? AND x.source_version=? AND x.source_fingerprint=? AND x.source_found=1 AND x.source_effective=1 AND v.state IN ('ready','superseded'))`, actual.RepoPrefix, actual.CheckoutID, actual.InputVersion, actual.InputFingerprint, actual.RepoPrefix, actual.InputVersion, actual.InputFingerprint).Scan(&published); err != nil {
			return err
		}
		if published {
			previousVersion, previousFingerprint = actual.InputVersion, actual.InputFingerprint
		}
	}
	if err := setContractWorkTx(ctx, tx, s.viewGen, work, encoded); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO generation_contract_input_state(view_gen,repo_prefix,checkout_id,input_version,input_fingerprint,accepted,previous_input_version,previous_input_fingerprint) VALUES(?,?,?,?,?,?,?,?) ON CONFLICT(view_gen,repo_prefix,checkout_id) DO UPDATE SET input_version=excluded.input_version,input_fingerprint=excluded.input_fingerprint,accepted=excluded.accepted,previous_input_version=excluded.previous_input_version,previous_input_fingerprint=excluded.previous_input_fingerprint`, s.viewGen, next.RepoPrefix, next.CheckoutID, next.InputVersion, next.InputFingerprint, accepted, previousVersion, previousFingerprint)
	return err
}

// AcceptContractInputMutationContext is only valid after the caller has proved
// its core apply receipt matches this exact pending input and source capture.
func (s *Store) AcceptContractInputMutationContext(ctx context.Context, expected graph.ContractInputState) error {
	if ctx == nil {
		return fmt.Errorf("%w: nil context", ErrCatalogInvalidValue)
	}
	if s.viewGen != 0 {
		return fmt.Errorf("%w: accept requires primary generation", ErrCatalogInvalidValue)
	}
	if err := validateContractInput(expected); err != nil {
		return err
	}
	release, err := s.HoldWriteGate(ctx)
	if err != nil {
		return err
	}
	defer release()
	tx, err := s.beginWriteContext(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	result, err := tx.ExecContext(ctx, `UPDATE generation_contract_input_state SET accepted=1 WHERE view_gen=0 AND repo_prefix=? AND checkout_id=? AND input_version=? AND input_fingerprint=? AND accepted=0`, expected.RepoPrefix, expected.CheckoutID, expected.InputVersion, expected.InputFingerprint)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrCatalogStaleGuard
	}
	return tx.Commit()
}

func sameContractInput(a, b graph.ContractInputState) bool {
	return a.RepoPrefix == b.RepoPrefix && a.CheckoutID == b.CheckoutID && a.InputVersion == b.InputVersion && a.InputFingerprint == b.InputFingerprint
}

var _ graph.ContractInputStateCohortReader = (*Store)(nil)

func (s *Store) ContractInputStatesForRepoContext(ctx context.Context, repo string) ([]graph.ContractInputState, error) {
	if ctx == nil {
		return nil, fmt.Errorf("%w: nil context", ErrCatalogInvalidValue)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s.coreless() || s.db == nil {
		return nil, sql.ErrConnDone
	}
	rows, err := s.db.QueryContext(ctx, `SELECT checkout_id,input_version,input_fingerprint,accepted,previous_input_version,previous_input_fingerprint FROM generation_contract_input_state WHERE view_gen=? AND repo_prefix=? ORDER BY checkout_id LIMIT 32769`, s.viewGen, repo)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []graph.ContractInputState
	for rows.Next() {
		row := graph.ContractInputState{RepoPrefix: repo}
		if err := rows.Scan(&row.CheckoutID, &row.InputVersion, &row.InputFingerprint, &row.Accepted, &row.PreviousInputVersion, &row.PreviousInputFingerprint); err != nil {
			return nil, err
		}
		if err := validateContractInput(row); err != nil {
			return nil, err
		}
		out = append(out, row)
		if len(out) > contractWorkReadLimit {
			return nil, ErrContractWorkLimit
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}
