package store_sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/zzet/gortex/internal/graph"
)

var _ graph.ContractWorkReader = (*Store)(nil)

const contractWorkReadLimit = 32768

var ErrContractWorkLimit = errors.New("store_sqlite: contract work read limit exceeded")

const contractWorkTableBody = ` (
    view_gen INTEGER NOT NULL,
    token TEXT NOT NULL,
    origin_generation INTEGER NOT NULL,
    checkout_id TEXT NOT NULL,
    repo_prefix TEXT NOT NULL,
    file_path TEXT NOT NULL,
    input_version TEXT NOT NULL,
    input_fingerprint TEXT NOT NULL,
    state TEXT NOT NULL,
    scope TEXT NOT NULL,
    PRIMARY KEY (view_gen, token)
) WITHOUT ROWID`

const contractWorkScopeIndexDDL = `CREATE INDEX IF NOT EXISTS generation_contract_work_scope
 ON generation_contract_work(view_gen, repo_prefix, checkout_id, token);`

// SetContractWork writes accepted debt or an acknowledgment into this
// generation. An acknowledgment must use the exact captured token. It does
// not clear other work for the same path. Published payloads remain sealed.
func (s *Store) SetContractWork(ctx context.Context, work []graph.ContractWork) error {
	if ctx == nil {
		return fmt.Errorf("%w: nil contract work context", ErrCatalogInvalidValue)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	encoded := make([][]byte, len(work))
	seen := make(map[string]bool, len(work))
	for i, row := range work {
		if row.Token == "" || row.OriginGeneration < 0 || row.FilePath == "" || row.InputVersion == "" || row.InputFingerprint == "" || seen[row.Token] ||
			(row.State != graph.ContractWorkPending && row.State != graph.ContractWorkComplete) {
			return fmt.Errorf("%w: invalid contract work row", ErrCatalogInvalidValue)
		}
		seen[row.Token] = true
		var err error
		encoded[i], err = json.Marshal(row.Scope)
		if err != nil {
			return fmt.Errorf("encode contract work: %w", err)
		}
	}
	if len(work) == 0 {
		return nil
	}
	if err := s.refuseSealedPayloadWrite(); err != nil {
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
	for i, row := range work {
		result, err := tx.ExecContext(ctx, `INSERT INTO generation_contract_work
 (view_gen, token, origin_generation, checkout_id, repo_prefix, file_path, input_version, input_fingerprint, state, scope)
 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
 ON CONFLICT(view_gen, token) DO UPDATE SET state = excluded.state
 WHERE generation_contract_work.origin_generation = excluded.origin_generation
 AND generation_contract_work.checkout_id = excluded.checkout_id
 AND generation_contract_work.repo_prefix = excluded.repo_prefix
 AND generation_contract_work.file_path = excluded.file_path
 AND generation_contract_work.input_version = excluded.input_version
 AND generation_contract_work.input_fingerprint = excluded.input_fingerprint
 AND generation_contract_work.scope = excluded.scope
 AND (generation_contract_work.state = 'pending' OR excluded.state = 'complete')`,
			s.viewGen, row.Token, row.OriginGeneration, row.CheckoutID, row.RepoPrefix, row.FilePath,
			row.InputVersion, row.InputFingerprint, string(row.State), string(encoded[i]))
		if err != nil {
			return err
		}
		if affected, err := result.RowsAffected(); err != nil || affected != 1 {
			return fmt.Errorf("%w: contract work token identity changed or acknowledgment withdrawn", ErrCatalogInvalidValue)
		}
	}
	return tx.Commit()
}

func (s *Store) ContractWorkContext(ctx context.Context) ([]graph.ContractWork, error) {
	return s.readContractWork(ctx, "", nil)
}

// ContractWorkForScopeContext narrows mutable-corpus work before reading it.
// An empty checkout is the primary repository, not every linked checkout.
func (s *Store) ContractWorkForScopeContext(ctx context.Context, repo, checkout string) ([]graph.ContractWork, error) {
	return s.readContractWork(ctx, " AND repo_prefix = ? AND checkout_id = ?", []any{repo, checkout})
}

func (s *Store) readContractWork(ctx context.Context, filter string, args []any) ([]graph.ContractWork, error) {
	if ctx == nil {
		return nil, fmt.Errorf("%w: nil contract work context", ErrCatalogInvalidValue)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s.coreless() || s.db == nil {
		return nil, sql.ErrConnDone
	}
	args = append([]any{s.viewGen}, args...)
	rows, err := s.db.QueryContext(ctx, `SELECT token, origin_generation, checkout_id, repo_prefix, file_path,
 input_version, input_fingerprint, state, scope FROM generation_contract_work
 WHERE view_gen = ?`+filter+` ORDER BY token LIMIT 32769`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var work []graph.ContractWork
	for rows.Next() {
		var row graph.ContractWork
		var state, scope string
		if err := rows.Scan(&row.Token, &row.OriginGeneration, &row.CheckoutID, &row.RepoPrefix, &row.FilePath, &row.InputVersion,
			&row.InputFingerprint, &state, &scope); err != nil {
			return nil, err
		}
		row.State = graph.ContractWorkState(state)
		if row.State != graph.ContractWorkPending && row.State != graph.ContractWorkComplete {
			return nil, fmt.Errorf("%w: invalid stored contract work state", ErrCatalogInvalidValue)
		}
		if err := json.Unmarshal([]byte(scope), &row.Scope); err != nil {
			return nil, fmt.Errorf("decode contract work: %w", err)
		}
		work = append(work, row)
		if len(work) > contractWorkReadLimit {
			return nil, ErrContractWorkLimit
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return work, nil
}

func createContractWorkTable(tx *sql.Tx) error {
	if _, err := tx.Exec(`CREATE TABLE IF NOT EXISTS generation_contract_work` + contractWorkTableBody); err != nil {
		return err
	}
	_, err := tx.Exec(contractWorkScopeIndexDDL)
	return err
}
