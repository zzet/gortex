package store_sqlite

import (
	"context"
	"database/sql"

	"github.com/zzet/gortex/internal/graph"
)

// AcceptContractInputMutationWithBaselineContext certifies a caller-validated
// complete accepted census and its pending primary input in one transaction.
// The caller holds the short repository mutation lane and source observation
// fence; contract-only parsing must finish before entering that lane.
func (s *Store) AcceptContractInputMutationWithBaselineContext(ctx context.Context, expected graph.ContractInputState, baseline graph.ContractBoundaryReceiptBaseline) error {
	if s.viewGen != 0 || expected.Accepted || baseline.RepoPrefix != expected.RepoPrefix || baseline.CheckoutID != expected.CheckoutID || baseline.Version == "" || baseline.Fingerprint == "" {
		return ErrCatalogInvalidValue
	}
	if err := validateContractInput(expected); err != nil {
		return err
	}
	return s.contractBoundaryTx(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `UPDATE generation_contract_input_state SET accepted=1 WHERE view_gen=0 AND repo_prefix=? AND checkout_id=? AND input_version=? AND input_fingerprint=? AND accepted=0`, expected.RepoPrefix, expected.CheckoutID, expected.InputVersion, expected.InputFingerprint)
		if err != nil {
			return err
		}
		count, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if count != 1 {
			return ErrCatalogStaleGuard
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO generation_contract_boundary_baseline(view_gen,repo_prefix,checkout_id,version,fingerprint) VALUES(0,?,?,?,?) ON CONFLICT(view_gen,repo_prefix,checkout_id) DO UPDATE SET version=excluded.version,fingerprint=excluded.fingerprint`, baseline.RepoPrefix, baseline.CheckoutID, baseline.Version, baseline.Fingerprint)
		return err
	})
}
