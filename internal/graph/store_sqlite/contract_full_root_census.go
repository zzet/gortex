package store_sqlite

import (
	"context"

	"github.com/zzet/gortex/internal/graph"
)

// RestartContractFullRootCensusContext restarts only an owned, unpublished
// dedicated root's interrupted census. Its old debt remains owed; old receipt
// and absence evidence cannot survive the full source rebuild. The exact input
// CAS and namespace reset commit together before the caller rewrites core rows.
func (s *Store) RestartContractFullRootCensusContext(ctx context.Context, expected, next graph.ContractInputState, work []graph.ContractWork) error {
	if ctx == nil || s.viewGen <= 0 || !expected.Accepted || !next.Accepted ||
		expected.RepoPrefix != next.RepoPrefix || expected.CheckoutID == "" || expected.CheckoutID != next.CheckoutID || expected.InputVersion != next.InputVersion {
		return ErrCatalogInvalidValue
	}
	if err := validateContractInput(expected); err != nil {
		return err
	}
	if err := validateContractInput(next); err != nil {
		return err
	}
	if len(work) > contractWorkReadLimit {
		return ErrContractWorkLimit
	}
	for _, row := range work {
		if row.RepoPrefix != next.RepoPrefix || row.CheckoutID != next.CheckoutID || row.State != graph.ContractWorkPending {
			return ErrCatalogInvalidValue
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
	var owned bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(
SELECT 1 FROM view_generations v JOIN dedicated_base_publications p ON p.graph_id=v.graph_id
WHERE v.generation_id=? AND v.state='building' AND v.owner_kind='dedicated_graph' AND v.generation_kind='dedicated'
AND v.checkout_id=? AND v.base_generation_id IS NULL AND v.layer_id IS NULL AND v.lower_view_fingerprint=''
AND p.generation_id=v.generation_id AND p.attempt_state='building' AND p.owner_checkout_id=v.checkout_id AND p.repo_prefix=?
AND p.tree_oid=v.tree_oid AND p.config_hash=v.config_hash AND p.extractor_versions=v.extractor_versions
AND p.resolver_version=v.resolver_version AND p.dependency_revision=v.dependency_revision)`, s.viewGen, expected.CheckoutID, expected.RepoPrefix).Scan(&owned); err != nil {
		return err
	}
	if !owned {
		return ErrCatalogStaleGuard
	}
	// A root has no ancestry, so the existing CAS can only match this physical
	// target. A concurrent input change refuses before deleting any evidence.
	if err := s.setContractInputStateWithWorkTx(ctx, tx, &expected, next, work, encoded, true); err != nil {
		return err
	}
	for _, table := range []string{"generation_contract_boundary_keys", "generation_contract_boundary_receipt", "generation_contract_boundary_baseline"} {
		if _, err := tx.ExecContext(ctx, `DELETE FROM `+table+` WHERE view_gen=? AND repo_prefix=? AND checkout_id=?`, s.viewGen, expected.RepoPrefix, expected.CheckoutID); err != nil {
			return err
		}
	}
	return tx.Commit()
}
