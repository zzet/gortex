package store_sqlite

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/zzet/gortex/internal/graph"
)

var _ graph.ContractBoundaryReceiptReader = (*Store)(nil)
var ErrContractBoundaryReceiptLimit = errors.New("store_sqlite: contract boundary receipt limit exceeded")

const contractBoundaryReceiptColumns = `repo_prefix,checkout_id,file_path,version,fingerprint,source_fingerprint,accepted,receipt`
const contractBoundaryPayloadLimit = 8 << 20
const contractBoundaryKeyLimit = 4096

func normalizeContractBoundaryReceipt(row graph.ContractBoundaryReceipt) (graph.ContractBoundaryReceipt, error) {
	if row.FilePath == "" || row.Version == "" || row.Fingerprint == "" || row.SourceFingerprint == "" {
		return row, fmt.Errorf("%w: incomplete contract boundary receipt", ErrCatalogInvalidValue)
	}
	if len(row.Payload) > contractBoundaryPayloadLimit || len(row.LookupKeys)+len(row.ProducedKeys) > contractBoundaryKeyLimit {
		return row, ErrContractBoundaryReceiptLimit
	}
	row.Payload = bytes.Clone(row.Payload)
	for _, keys := range []*[]string{&row.LookupKeys, &row.ProducedKeys} {
		*keys = slices.Clone(*keys)
		slices.Sort(*keys)
		*keys = slices.Compact(*keys)
		for _, key := range *keys {
			if key == "" || len(key) > 8192 {
				return row, fmt.Errorf("%w: empty boundary key", ErrCatalogInvalidValue)
			}
		}
	}
	return row, nil
}

type contractBoundaryScanner interface{ Scan(...any) error }

func scanContractBoundaryReceipt(scan contractBoundaryScanner) (graph.ContractBoundaryReceipt, error) {
	var columns graph.ContractBoundaryReceipt
	var payload []byte
	if err := scan.Scan(&columns.RepoPrefix, &columns.CheckoutID, &columns.FilePath, &columns.Version, &columns.Fingerprint, &columns.SourceFingerprint, &columns.Accepted, &payload); err != nil {
		return columns, err
	}
	if len(payload) > 4*contractBoundaryPayloadLimit {
		return columns, ErrContractBoundaryReceiptLimit
	}
	var row graph.ContractBoundaryReceipt
	if err := json.Unmarshal(payload, &row); err != nil {
		return row, fmt.Errorf("decode boundary receipt: %w", err)
	}
	if row.RepoPrefix != columns.RepoPrefix || row.CheckoutID != columns.CheckoutID || row.FilePath != columns.FilePath || row.Version != columns.Version || row.Fingerprint != columns.Fingerprint || row.SourceFingerprint != columns.SourceFingerprint || row.Accepted != columns.Accepted {
		return row, fmt.Errorf("%w: boundary receipt identity mismatch", ErrCatalogInvalidValue)
	}
	if _, err := normalizeContractBoundaryReceipt(row); err != nil {
		return row, err
	}
	if row.Previous != nil {
		if _, err := normalizeContractBoundaryReceipt(*row.Previous); err != nil {
			return row, err
		}
	}
	if row.Previous != nil && (row.Previous.Previous != nil || !row.Previous.Accepted || row.Previous.RepoPrefix != row.RepoPrefix || row.Previous.CheckoutID != row.CheckoutID || row.Previous.FilePath != row.FilePath) {
		return row, fmt.Errorf("%w: boundary predecessor identity", ErrCatalogInvalidValue)
	}
	return row, nil
}

func (s *Store) ContractBoundaryReceiptContext(ctx context.Context, repo, actor, file string) (*graph.ContractBoundaryReceipt, bool, error) {
	if err := s.requireContractBoundaryContext(ctx); err != nil {
		return nil, false, err
	}
	row, err := scanContractBoundaryReceipt(s.db.QueryRowContext(ctx, `SELECT `+contractBoundaryReceiptColumns+` FROM generation_contract_boundary_receipt WHERE view_gen=? AND repo_prefix=? AND checkout_id=? AND file_path=?`, s.viewGen, repo, actor, file))
	if errors.Is(err, sql.ErrNoRows) {
		baseline, err := s.ContractBoundaryReceiptBaselineContext(ctx, repo, actor)
		return nil, baseline != nil, err
	}
	if err != nil {
		return nil, false, err
	}
	return &row, true, nil
}

func (s *Store) ContractBoundaryReceiptBaselineContext(ctx context.Context, repo, actor string) (*graph.ContractBoundaryReceiptBaseline, error) {
	if err := s.requireContractBoundaryContext(ctx); err != nil {
		return nil, err
	}
	row := &graph.ContractBoundaryReceiptBaseline{RepoPrefix: repo, CheckoutID: actor}
	err := s.db.QueryRowContext(ctx, `SELECT version,fingerprint FROM generation_contract_boundary_baseline WHERE view_gen=? AND repo_prefix=? AND checkout_id=?`, s.viewGen, repo, actor).Scan(&row.Version, &row.Fingerprint)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if row.Version == "" || row.Fingerprint == "" {
		return nil, ErrCatalogInvalidValue
	}
	return row, nil
}

// SetContractBoundaryReceiptBaselineContext certifies only a caller-completed
// accepted namespace census. A missing file becomes known empty only afterward.
func (s *Store) SetContractBoundaryReceiptBaselineContext(ctx context.Context, row graph.ContractBoundaryReceiptBaseline) error {
	if row.Version == "" || row.Fingerprint == "" {
		return ErrCatalogInvalidValue
	}
	return s.contractBoundaryTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO generation_contract_boundary_baseline(view_gen,repo_prefix,checkout_id,version,fingerprint) VALUES(?,?,?,?,?) ON CONFLICT(view_gen,repo_prefix,checkout_id) DO UPDATE SET version=excluded.version,fingerprint=excluded.fingerprint`, s.viewGen, row.RepoPrefix, row.CheckoutID, row.Version, row.Fingerprint)
		return err
	})
}

func (s *Store) ContractBoundaryReceiptsForLookupKeysContext(ctx context.Context, keys []string, limit int) ([]graph.ContractBoundaryReceipt, error) {
	return s.contractBoundaryReceiptsForKeys(ctx, "lookup", keys, limit)
}
func (s *Store) ContractBoundaryReceiptsForProducedKeysContext(ctx context.Context, keys []string, limit int) ([]graph.ContractBoundaryReceipt, error) {
	return s.contractBoundaryReceiptsForKeys(ctx, "produced", keys, limit)
}

// Each key query starts from the generation/kind/key PK, then fetches complete
// owner records by exact file PK. No JSON SQL or registry scan is involved.
func (s *Store) contractBoundaryReceiptsForKeys(ctx context.Context, kind string, keys []string, limit int) ([]graph.ContractBoundaryReceipt, error) {
	if err := s.requireContractBoundaryContext(ctx); err != nil {
		return nil, err
	}
	if limit <= 0 || limit > contractWorkReadLimit {
		limit = contractWorkReadLimit
	}
	keys = slices.Clone(keys)
	slices.Sort(keys)
	keys = slices.Compact(keys)
	if len(keys) > contractBoundaryKeyLimit {
		return nil, ErrContractBoundaryReceiptLimit
	}
	if len(keys) == 0 {
		return nil, nil
	}
	// All chunks observe one physical snapshot, not mixed staged/accepted owners.
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	type owner struct{ repo, actor, file string }
	seen := make(map[owner]bool)
	var out []graph.ContractBoundaryReceipt
	totalBytes := 0
	for start := 0; start < len(keys); start += lookupChunkSize {
		chunk := keys[start:min(start+lookupChunkSize, len(keys))]
		for _, key := range chunk {
			if key == "" || len(key) > 8192 {
				return nil, ErrCatalogInvalidValue
			}
		}
		args := []any{s.viewGen, kind}
		args = append(args, toAnyArgs(chunk)...)
		args = append(args, limit+1)
		// SELECT DISTINCT affects only this bounded indexed key cohort.
		rows, err := tx.QueryContext(ctx, `SELECT DISTINCT r.`+stringsBoundaryColumns()+` FROM generation_contract_boundary_keys k JOIN generation_contract_boundary_receipt r ON r.view_gen=k.view_gen AND r.repo_prefix=k.repo_prefix AND r.checkout_id=k.checkout_id AND r.file_path=k.file_path WHERE k.view_gen=? AND k.key_kind=? AND k.lookup_key IN (`+inPlaceholders(len(chunk))+`) LIMIT ?`, args...)
		if err != nil {
			return nil, err
		}
		chunkRows := 0
		for rows.Next() {
			chunkRows++
			if chunkRows > limit {
				_ = rows.Close()
				return nil, ErrContractBoundaryReceiptLimit
			}
			row, err := scanContractBoundaryReceipt(rows)
			if err != nil {
				_ = rows.Close()
				return nil, err
			}
			key := owner{row.RepoPrefix, row.CheckoutID, row.FilePath}
			if seen[key] {
				continue
			}
			encoded, err := json.Marshal(row)
			if err != nil {
				_ = rows.Close()
				return nil, err
			}
			totalBytes += len(encoded)
			if totalBytes > 4*contractBoundaryPayloadLimit {
				_ = rows.Close()
				return nil, ErrContractBoundaryReceiptLimit
			}
			seen[key] = true
			out = append(out, row)
			if len(out) > limit {
				_ = rows.Close()
				return nil, ErrContractBoundaryReceiptLimit
			}
		}
		err = rows.Err()
		_ = rows.Close()
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}
func stringsBoundaryColumns() string {
	return `repo_prefix,r.checkout_id,r.file_path,r.version,r.fingerprint,r.source_fingerprint,r.accepted,r.receipt`
}

func (s *Store) requireContractBoundaryContext(ctx context.Context) error {
	if ctx == nil {
		return ErrCatalogInvalidValue
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.coreless() || s.db == nil {
		return sql.ErrConnDone
	}
	return nil
}
func (s *Store) contractBoundaryTx(ctx context.Context, fn func(*sql.Tx) error) error {
	if err := s.requireContractBoundaryContext(ctx); err != nil {
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
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// SetContractBoundaryReceiptsContext stages primary receipts before core writes.
// Positive building payloads may initialize accepted receipts before publication.
func (s *Store) SetContractBoundaryReceiptsContext(ctx context.Context, rows []graph.ContractBoundaryReceipt) error {
	for _, row := range rows {
		if s.viewGen == 0 && row.Accepted {
			return fmt.Errorf("%w: primary receipt needs staged acceptance", ErrCatalogInvalidValue)
		}
	}
	return s.contractBoundaryTx(ctx, func(tx *sql.Tx) error { return s.setContractBoundaryReceiptsTx(ctx, tx, rows) })
}

func (s *Store) setContractBoundaryReceiptsTx(ctx context.Context, tx *sql.Tx, rows []graph.ContractBoundaryReceipt) error {
	if len(rows) > contractWorkReadLimit {
		return ErrContractBoundaryReceiptLimit
	}
	type owner struct{ repo, actor, file string }
	seen := make(map[owner]bool)
	for _, input := range rows {
		row, err := normalizeContractBoundaryReceipt(input)
		if err != nil {
			return err
		}
		key := owner{row.RepoPrefix, row.CheckoutID, row.FilePath}
		if seen[key] {
			return ErrCatalogInvalidValue
		}
		seen[key] = true
		old, err := scanContractBoundaryReceipt(tx.QueryRowContext(ctx, `SELECT `+contractBoundaryReceiptColumns+` FROM generation_contract_boundary_receipt WHERE view_gen=? AND repo_prefix=? AND checkout_id=? AND file_path=?`, s.viewGen, row.RepoPrefix, row.CheckoutID, row.FilePath))
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		row.Previous = nil
		if err == nil {
			if old.Accepted {
				old.Previous = nil
				row.Previous = &old
			} else {
				row.Previous = old.Previous
			}
		}
		if row.Accepted {
			row.Previous = nil
			if _, err := tx.ExecContext(ctx, `DELETE FROM generation_contract_boundary_keys WHERE view_gen=? AND repo_prefix=? AND checkout_id=? AND file_path=?`, s.viewGen, row.RepoPrefix, row.CheckoutID, row.FilePath); err != nil {
				return err
			}
		}
		encoded, err := json.Marshal(row)
		if err != nil {
			return err
		}
		if len(encoded) > 4*contractBoundaryPayloadLimit {
			return ErrContractBoundaryReceiptLimit
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO generation_contract_boundary_receipt(view_gen,repo_prefix,checkout_id,file_path,version,fingerprint,source_fingerprint,accepted,receipt) VALUES(?,?,?,?,?,?,?,?,?) ON CONFLICT(view_gen,repo_prefix,checkout_id,file_path) DO UPDATE SET version=excluded.version,fingerprint=excluded.fingerprint,source_fingerprint=excluded.source_fingerprint,accepted=excluded.accepted,receipt=excluded.receipt`, s.viewGen, row.RepoPrefix, row.CheckoutID, row.FilePath, row.Version, row.Fingerprint, row.SourceFingerprint, row.Accepted, encoded); err != nil {
			return err
		}
		for _, group := range []struct {
			kind string
			keys []string
		}{{"lookup", row.LookupKeys}, {"produced", row.ProducedKeys}} {
			for _, k := range group.keys {
				if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO generation_contract_boundary_keys(view_gen,key_kind,lookup_key,repo_prefix,checkout_id,file_path) VALUES(?,?,?,?,?,?)`, s.viewGen, group.kind, k, row.RepoPrefix, row.CheckoutID, row.FilePath); err != nil {
					return err
				}
			}
		}
		// Repeated failed edits cannot grow the staged union without bound. Refuse
		// before committing core admission instead of dropping invalidation evidence.
		var keyCount, keyBytes int64
		if err := tx.QueryRowContext(ctx, `SELECT count(*),coalesce(sum(length(lookup_key)+length(repo_prefix)+length(checkout_id)+length(file_path)),0) FROM generation_contract_boundary_keys WHERE view_gen=? AND repo_prefix=? AND checkout_id=? AND file_path=?`, s.viewGen, row.RepoPrefix, row.CheckoutID, row.FilePath).Scan(&keyCount, &keyBytes); err != nil {
			return err
		}
		if keyCount > contractBoundaryKeyLimit || keyBytes > 4*contractBoundaryPayloadLimit {
			return ErrContractBoundaryReceiptLimit
		}
	}
	return nil
}

// AcceptContractBoundaryReceiptsContext is receipt-only outer acceptance for an
// ordinary edit whose contract identity carries unchanged. Exact bytes and all
// captured fields must still match the staged receipt.
func (s *Store) AcceptContractBoundaryReceiptsContext(ctx context.Context, expected []graph.ContractBoundaryReceipt) error {
	return s.contractBoundaryTx(ctx, func(tx *sql.Tx) error { return s.acceptContractBoundaryReceiptsTx(ctx, tx, expected) })
}
func (s *Store) acceptContractBoundaryReceiptsTx(ctx context.Context, tx *sql.Tx, expected []graph.ContractBoundaryReceipt) error {
	if len(expected) > contractWorkReadLimit {
		return ErrContractBoundaryReceiptLimit
	}
	var accepted []graph.ContractBoundaryReceipt
	for _, capture := range expected {
		row, err := scanContractBoundaryReceipt(tx.QueryRowContext(ctx, `SELECT `+contractBoundaryReceiptColumns+` FROM generation_contract_boundary_receipt WHERE view_gen=? AND repo_prefix=? AND checkout_id=? AND file_path=?`, s.viewGen, capture.RepoPrefix, capture.CheckoutID, capture.FilePath))
		if err != nil {
			return err
		}
		normalized, err := normalizeContractBoundaryReceipt(capture)
		if err != nil {
			return err
		}
		normalized.Previous = row.Previous
		a, err := json.Marshal(normalized)
		if err != nil {
			return err
		}
		b, err := json.Marshal(row)
		if err != nil {
			return err
		}
		if row.Accepted || capture.Accepted || !bytes.Equal(a, b) {
			return ErrCatalogStaleGuard
		}
		row.Accepted = true
		accepted = append(accepted, row)
	}
	return s.setContractBoundaryReceiptsTx(ctx, tx, accepted)
}

// ContractBoundaryReceiptForVersionContext binds known-empty results to the
// collector's exact baseline format; a stale namespace baseline is unknown.
func (s *Store) ContractBoundaryReceiptForVersionContext(ctx context.Context, repo, actor, file, version string) (*graph.ContractBoundaryReceipt, bool, error) {
	if version == "" {
		return nil, false, ErrCatalogInvalidValue
	}
	row, known, err := s.ContractBoundaryReceiptContext(ctx, repo, actor, file)
	if err != nil {
		return nil, false, err
	}
	if row != nil {
		if row.Version != version {
			return nil, false, nil
		}
		return row, known, nil
	}
	if !known {
		return nil, false, nil
	}
	baseline, err := s.ContractBoundaryReceiptBaselineContext(ctx, repo, actor)
	if err != nil {
		return nil, false, err
	}
	return nil, baseline != nil && baseline.Version == version, nil
}

// SetContractBoundaryReceiptsWithSourcesContext seeds an empty building
// positive file namespace from exact leased selected physical predecessors.
// The caller owns source-view selection/lease; physical0 must actually inherit.
func (s *Store) SetContractBoundaryReceiptsWithSourcesContext(ctx context.Context, rows []graph.ContractBoundaryReceipt, sources []graph.ContractBoundaryReceiptSource) error {
	if s.viewGen == 0 {
		return ErrCatalogInvalidValue
	}
	return s.contractBoundaryTx(ctx, func(tx *sql.Tx) error {
		if err := s.carryContractBoundarySourcesTx(ctx, tx, sources); err != nil {
			return err
		}
		return s.setContractBoundaryReceiptsTx(ctx, tx, rows)
	})
}
func (s *Store) carryContractBoundarySourcesTx(ctx context.Context, tx *sql.Tx, sources []graph.ContractBoundaryReceiptSource) error {
	if len(sources) > contractWorkReadLimit {
		return ErrContractBoundaryReceiptLimit
	}
	if len(sources) == 0 {
		return nil
	}
	if s.viewGen == 0 {
		return ErrCatalogInvalidValue
	}
	var targetState string
	if err := tx.QueryRowContext(ctx, `SELECT state FROM view_generations WHERE generation_id=?`, s.viewGen).Scan(&targetState); err != nil {
		return err
	}
	if targetState != string(ViewGenerationBuilding) {
		return ErrCatalogStaleGuard
	}
	for _, source := range sources {
		if source.GenerationID < 0 || source.GenerationID == s.viewGen {
			return ErrCatalogInvalidValue
		}
		if source.GenerationID > 0 {
			var state string
			if err := tx.QueryRowContext(ctx, `SELECT state FROM view_generations WHERE generation_id=?`, source.GenerationID).Scan(&state); err != nil {
				return err
			}
			if state != string(ViewGenerationReady) && state != string(ViewGenerationSuperseded) {
				return ErrCatalogStaleGuard
			}
		}
		capture, err := normalizeContractBoundaryReceipt(source.Receipt)
		if err != nil {
			return err
		}
		row, err := scanContractBoundaryReceipt(tx.QueryRowContext(ctx, `SELECT `+contractBoundaryReceiptColumns+` FROM generation_contract_boundary_receipt WHERE view_gen=? AND repo_prefix=? AND checkout_id=? AND file_path=?`, source.GenerationID, capture.RepoPrefix, capture.CheckoutID, capture.FilePath))
		if err != nil {
			return err
		}
		a, err := json.Marshal(capture)
		if err != nil {
			return err
		}
		b, err := json.Marshal(row)
		if err != nil {
			return err
		}
		if !bytes.Equal(a, b) {
			return ErrCatalogStaleGuard
		}
		var exists int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM generation_contract_boundary_receipt WHERE view_gen=? AND repo_prefix=? AND checkout_id=? AND file_path=?`, s.viewGen, row.RepoPrefix, source.TargetCheckoutID, row.FilePath).Scan(&exists); err != nil {
			return err
		}
		if exists != 0 {
			return ErrCatalogStaleGuard
		}
		oldActor := row.CheckoutID
		row.CheckoutID = source.TargetCheckoutID
		row.Carried = true
		row.CarriedFromGeneration = source.GenerationID
		row.CarriedFromCheckoutID = oldActor
		if row.Previous != nil {
			previous := *row.Previous
			previous.CheckoutID = source.TargetCheckoutID
			previous.Carried = true
			previous.CarriedFromGeneration = source.GenerationID
			previous.CarriedFromCheckoutID = oldActor
			row.Previous = &previous
		}
		encoded, err := json.Marshal(row)
		if err != nil {
			return err
		}
		if len(encoded) > 4*contractBoundaryPayloadLimit {
			return ErrContractBoundaryReceiptLimit
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO generation_contract_boundary_receipt(view_gen,repo_prefix,checkout_id,file_path,version,fingerprint,source_fingerprint,accepted,receipt) VALUES(?,?,?,?,?,?,?,?,?)`, s.viewGen, row.RepoPrefix, row.CheckoutID, row.FilePath, row.Version, row.Fingerprint, row.SourceFingerprint, row.Accepted, encoded); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO generation_contract_boundary_keys(view_gen,key_kind,lookup_key,repo_prefix,checkout_id,file_path) SELECT ?,key_kind,lookup_key,repo_prefix,?,file_path FROM generation_contract_boundary_keys WHERE view_gen=? AND repo_prefix=? AND checkout_id=? AND file_path=?`, s.viewGen, row.CheckoutID, source.GenerationID, row.RepoPrefix, oldActor, row.FilePath); err != nil {
			return err
		}
	}
	return nil
}
