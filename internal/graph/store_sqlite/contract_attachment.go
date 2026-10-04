package store_sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"github.com/zzet/gortex/internal/graph"
	"slices"
)

var _ graph.ContractAttachmentReader = (*Store)(nil)

// GetContractAttachmentContext is an indexed exact historical identity lookup.
// It never scans actor history or falls back to its latest payload.
func (s *Store) GetContractAttachmentContext(ctx context.Context, key graph.ContractAttachmentKey) (*graph.ContractAttachment, error) {
	if ctx == nil {
		return nil, fmt.Errorf("%w: nil context", ErrCatalogInvalidValue)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if key.InputVersion == "" || key.InputFingerprint == "" {
		return nil, fmt.Errorf("%w: empty attachment key", ErrCatalogInvalidValue)
	}
	if s.coreless() || s.db == nil {
		return nil, sql.ErrConnDone
	}
	a := &graph.ContractAttachment{RepoPrefix: key.RepoPrefix, CheckoutID: key.CheckoutID, InputVersion: key.InputVersion, InputFingerprint: key.InputFingerprint}
	err := s.db.QueryRowContext(ctx, `SELECT a.payload_generation FROM contract_attachments a JOIN view_generations v ON v.generation_id=a.payload_generation WHERE a.repo_prefix=? AND a.checkout_id=? AND a.input_version=? AND a.input_fingerprint=? AND v.state IN ('ready','superseded')`, key.RepoPrefix, key.CheckoutID, key.InputVersion, key.InputFingerprint).Scan(&a.PayloadGeneration)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT token FROM contract_attachment_work WHERE repo_prefix=? AND checkout_id=? AND attachment_version=? AND attachment_fingerprint=? ORDER BY token LIMIT 32769`, key.RepoPrefix, key.CheckoutID, key.InputVersion, key.InputFingerprint)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var token string
		if err := rows.Scan(&token); err != nil {
			return nil, err
		}
		a.CompletedTokens = append(a.CompletedTokens, token)
		if len(a.CompletedTokens) > contractWorkReadLimit {
			return nil, ErrContractWorkLimit
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return a, nil
}

// PublishContractAttachmentContext seals the isolated managed payload and binds
// it to the selected accepted input together with immutable debt acknowledgments.
// The receiver selects the core generation whose ancestry the worker captured;
// no mutable corpus clock or latest-actor attachment is consulted.
func (s *Store) PublishContractAttachmentContext(ctx context.Context, expected graph.ContractInputState, attachment graph.ContractAttachment, work []graph.ContractWork, publishedAt int64) error {
	if ctx == nil {
		return fmt.Errorf("%w: nil context", ErrCatalogInvalidValue)
	}
	if err := validateContractInput(expected); err != nil {
		return err
	}
	if attachment.PayloadGeneration <= 0 || attachment.PayloadGeneration == s.viewGen || attachment.RepoPrefix != expected.RepoPrefix || attachment.CheckoutID != expected.CheckoutID || attachment.InputVersion != expected.InputVersion || attachment.InputFingerprint != expected.InputFingerprint {
		return fmt.Errorf("%w: attachment identity", ErrCatalogInvalidValue)
	}
	if len(work) > contractWorkReadLimit {
		return ErrContractWorkLimit
	}
	encoded, err := encodeContractWork(work)
	if err != nil {
		return err
	}
	tokens := make([]string, len(work))
	for i, row := range work {
		if row.State != graph.ContractWorkPending || row.RepoPrefix != expected.RepoPrefix || row.CheckoutID != expected.CheckoutID {
			return fmt.Errorf("%w: attachment work namespace/state", ErrCatalogInvalidValue)
		}
		tokens[i] = row.Token
	}
	slices.Sort(tokens)
	declared := slices.Clone(attachment.CompletedTokens)
	slices.Sort(declared)
	if !slices.Equal(tokens, declared) {
		return fmt.Errorf("%w: attachment acknowledgment batch", ErrCatalogInvalidValue)
	}
	release, err := s.HoldWriteGate(ctx)
	if err != nil {
		return err
	}
	defer release()
	// Catalog/ack writes are separate from sealed core payload writes. The base
	// handle uses the existing managed write admission for the sealing UPDATE.
	tx, err := s.AtGeneration(0).beginWriteContext(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	chain, err := contractInputAncestryTx(ctx, tx, s.viewGen, expected)
	if err != nil {
		return err
	}
	var state string
	if err := tx.QueryRowContext(ctx, `SELECT state FROM generation_producer_completeness WHERE view_gen=? AND producer='graph.contracts'`, attachment.PayloadGeneration).Scan(&state); err != nil {
		return err
	}
	if state != string(ProducerStateComplete) {
		return fmt.Errorf("%w: contract payload incomplete", ErrCatalogInvalidValue)
	}
	var masks int
	if err := tx.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM generation_file_masks WHERE view_gen=?)+(SELECT count(*) FROM generation_node_tombstones WHERE view_gen=?)+(SELECT count(*) FROM generation_edge_sources WHERE view_gen=?)`, attachment.PayloadGeneration, attachment.PayloadGeneration, attachment.PayloadGeneration).Scan(&masks); err != nil {
		return err
	}
	if masks != 0 {
		return fmt.Errorf("%w: isolated attachment cannot mask core", ErrCatalogInvalidValue)
	}
	for i, row := range work {
		if err := validateCapturedContractWorkTx(ctx, tx, chain, row, string(encoded[i])); err != nil {
			return err
		}
	}
	// Immutable key: a competing publication cannot replace historical bytes.
	if _, err := tx.ExecContext(ctx, `INSERT INTO contract_attachments(repo_prefix,checkout_id,input_version,input_fingerprint,payload_generation) VALUES(?,?,?,?,?)`, expected.RepoPrefix, expected.CheckoutID, expected.InputVersion, expected.InputFingerprint, attachment.PayloadGeneration); err != nil {
		return err
	}
	for i, row := range work {
		if _, err := tx.ExecContext(ctx, `INSERT INTO contract_attachment_work(repo_prefix,checkout_id,attachment_version,attachment_fingerprint,token,origin_generation,file_path,input_version,input_fingerprint,scope) VALUES(?,?,?,?,?,?,?,?,?,?)`, row.RepoPrefix, row.CheckoutID, expected.InputVersion, expected.InputFingerprint, row.Token, row.OriginGeneration, row.FilePath, row.InputVersion, row.InputFingerprint, string(encoded[i])); err != nil {
			return err
		}
	}
	if err := execGuardedTx(ctx, tx, "contract attachment payload is not building", `UPDATE view_generations SET state=?,published_at=? WHERE generation_id=? AND state=?`, string(ViewGenerationReady), publishedAt, attachment.PayloadGeneration, string(ViewGenerationBuilding)); err != nil {
		return err
	}
	return tx.Commit()
}

func contractInputAncestryTx(ctx context.Context, tx *sql.Tx, generation int64, expected graph.ContractInputState) ([]int64, error) {
	var chain []int64
	matched := false
	for steps := 0; steps < 1024; steps++ {
		chain = append(chain, generation)
		if !matched {
			var v, f string
			var accepted bool
			err := tx.QueryRowContext(ctx, `SELECT input_version,input_fingerprint,accepted FROM generation_contract_input_state WHERE view_gen=? AND repo_prefix=? AND checkout_id=?`, generation, expected.RepoPrefix, expected.CheckoutID).Scan(&v, &f, &accepted)
			if err == nil {
				if !accepted || v != expected.InputVersion || f != expected.InputFingerprint {
					return nil, ErrCatalogStaleGuard
				}
				matched = true
			} else if !errors.Is(err, sql.ErrNoRows) {
				return nil, err
			}
		}
		if generation == 0 {
			if !matched {
				return nil, ErrCatalogStaleGuard
			}
			return chain, nil
		}
		var base sql.NullInt64
		if err := tx.QueryRowContext(ctx, `SELECT base_generation_id FROM view_generations WHERE generation_id=?`, generation).Scan(&base); err != nil {
			return nil, err
		}
		if !base.Valid {
			if !matched {
				return nil, ErrCatalogStaleGuard
			}
			return chain, nil
		}
		if base.Int64 >= generation || base.Int64 < 0 {
			return nil, ErrCatalogStaleGuard
		}
		generation = base.Int64
	}
	return nil, fmt.Errorf("%w: contract ancestry limit", ErrCatalogInvalidValue)
}

func validateCapturedContractWorkTx(ctx context.Context, tx *sql.Tx, chain []int64, w graph.ContractWork, scope string) error {
	for _, generation := range chain {
		var origin int64
		var checkout, repo, file, version, fingerprint, state, storedScope string
		err := tx.QueryRowContext(ctx, `SELECT origin_generation,checkout_id,repo_prefix,file_path,input_version,input_fingerprint,state,scope FROM generation_contract_work WHERE view_gen=? AND token=?`, generation, w.Token).Scan(&origin, &checkout, &repo, &file, &version, &fingerprint, &state, &storedScope)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return err
		}
		if origin != w.OriginGeneration || checkout != w.CheckoutID || repo != w.RepoPrefix || file != w.FilePath || version != w.InputVersion || fingerprint != w.InputFingerprint || state != string(graph.ContractWorkPending) || storedScope != scope {
			return ErrCatalogStaleGuard
		}
		return nil
	}
	return ErrCatalogStaleGuard
}
