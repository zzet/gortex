package store_sqlite

import (
	"context"
	"database/sql"
	"errors"

	"github.com/zzet/gortex/internal/graph"
)

// ContractInputIdentityPublishedContext is a discovery filter only. It uses the
// existing source index and exact physical identity; another cohort's matching
// row cannot prove a requested whole cohort. Retirement is checked by exact open.
func (s *Store) ContractInputIdentityPublishedContext(ctx context.Context, w graph.ContractInputWitness) (bool, error) {
	if ctx == nil {
		return false, ErrCatalogInvalidValue
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if s.coreless() || s.db == nil {
		return false, sql.ErrConnDone
	}
	if !w.Found || w.State.InputVersion == "" || w.State.InputFingerprint == "" {
		return false, graph.ErrContractInputVector
	}
	var published bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM contract_attachment_inputs x INDEXED BY contract_attachment_input_source JOIN contract_attachments a ON a.repo_prefix=x.repo_prefix AND a.checkout_id=x.checkout_id AND a.input_version=x.input_version AND a.input_fingerprint=x.input_fingerprint WHERE x.source_repo=? AND x.source_version=? AND x.source_fingerprint=? AND x.source_found=1 AND x.source_effective=1 AND x.source_generation=? AND x.source_checkout=?)`, w.State.RepoPrefix, w.State.InputVersion, w.State.InputFingerprint, w.GenerationID, w.State.CheckoutID).Scan(&published)
	return published, err
}

// ContractAttachmentProofContext reads one exact immutable header and physical
// witness vector. It never searches latest history; absence and known retirement
// differ so callers do not label unknown history as a retired predecessor.
func (s *Store) ContractAttachmentProofContext(ctx context.Context, key graph.ContractAttachmentKey) (*graph.ContractAttachment, []graph.ContractInputWitness, error) {
	if ctx == nil {
		return nil, nil, ErrCatalogInvalidValue
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	if s.coreless() || s.db == nil {
		return nil, nil, sql.ErrConnDone
	}
	if key.InputVersion == "" || key.InputFingerprint == "" {
		return nil, nil, ErrCatalogInvalidValue
	}
	attachment := &graph.ContractAttachment{RepoPrefix: key.RepoPrefix, CheckoutID: key.CheckoutID, InputVersion: key.InputVersion, InputFingerprint: key.InputFingerprint}
	err := s.db.QueryRowContext(ctx, `SELECT payload_generation FROM contract_attachments WHERE repo_prefix=? AND checkout_id=? AND input_version=? AND input_fingerprint=?`, key.RepoPrefix, key.CheckoutID, key.InputVersion, key.InputFingerprint).Scan(&attachment.PayloadGeneration)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	row, found, err := s.Catalog().GetViewGeneration(ctx, attachment.PayloadGeneration)
	if err != nil {
		return nil, nil, err
	}
	if !found || row.State == ViewGenerationRetiring {
		return nil, nil, graph.ErrContractPredecessorRetired
	}
	if row.State != ViewGenerationReady && row.State != ViewGenerationSuperseded {
		return nil, nil, graph.ErrContractProjectionIncomplete
	}
	rows, err := s.db.QueryContext(ctx, `SELECT source_generation,source_repo,source_checkout,source_version,source_fingerprint,source_accepted,source_found,source_effective FROM contract_attachment_inputs WHERE repo_prefix=? AND checkout_id=? AND input_version=? AND input_fingerprint=? ORDER BY source_repo,source_generation,source_checkout LIMIT 1025`, key.RepoPrefix, key.CheckoutID, key.InputVersion, key.InputFingerprint)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var witnesses []graph.ContractInputWitness
	var flags []bool
	for rows.Next() {
		var w graph.ContractInputWitness
		var effective bool
		if err := rows.Scan(&w.GenerationID, &w.State.RepoPrefix, &w.State.CheckoutID, &w.State.InputVersion, &w.State.InputFingerprint, &w.State.Accepted, &w.Found, &effective); err != nil {
			return nil, nil, err
		}
		witnesses = append(witnesses, w)
		flags = append(flags, effective)
		if len(witnesses) > 1024 {
			return nil, nil, graph.ErrContractInputVector
		}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	logical, err := graph.ComposeContractInputState(key.RepoPrefix, key.CheckoutID, witnesses)
	if err != nil || !logical.Accepted || logical.InputVersion != key.InputVersion || logical.InputFingerprint != key.InputFingerprint {
		return nil, nil, graph.ErrContractInputVector
	}
	effective := effectiveContractWitnesses(witnesses)
	for i, flag := range flags {
		if effective[i] != flag {
			return nil, nil, graph.ErrContractInputVector
		}
	}
	return attachment, witnesses, nil
}
