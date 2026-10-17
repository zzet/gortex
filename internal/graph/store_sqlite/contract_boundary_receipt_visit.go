package store_sqlite

import (
	"context"
	"slices"

	"github.com/zzet/gortex/internal/graph"
)

const contractBoundaryReceiptVisitChunkSize = 128

// VisitContractBoundaryReceiptsContext streams existing physical receipt rows
// for exact file keys. It does not certify absence: callers needing known-empty
// proof must read the namespace baseline separately. Each chunk is one SQLite
// statement, with the same payload-free key gate and full decoder as point
// reads. The callback must not issue another read on this store while its cursor
// is open. No receipt payloads are accumulated by this method.
func (s *Store) VisitContractBoundaryReceiptsContext(ctx context.Context, repo, actor string, files []string, visit func(graph.ContractBoundaryReceipt) error) error {
	if err := s.requireContractBoundaryContext(ctx); err != nil {
		return err
	}
	if visit == nil || len(files) > contractBoundaryKeyLimit {
		return ErrCatalogInvalidValue
	}
	files = slices.Clone(files)
	slices.Sort(files)
	files = slices.Compact(files)
	for _, file := range files {
		if file == "" {
			return ErrCatalogInvalidValue
		}
	}
	for start := 0; start < len(files); start += contractBoundaryReceiptVisitChunkSize {
		chunk := files[start:min(start+contractBoundaryReceiptVisitChunkSize, len(files))]
		args := append([]any{s.viewGen, repo, actor}, toAnyArgs(chunk)...)
		query := `WITH receipt_key AS MATERIALIZED (
 SELECT view_gen,repo_prefix,checkout_id,file_path
 FROM generation_contract_boundary_receipt INDEXED BY contract_boundary_receipt_stamps
 WHERE view_gen=? AND repo_prefix=? AND checkout_id=? AND file_path IN (` + inPlaceholders(len(chunk)) + `)
)
SELECT r.repo_prefix,r.checkout_id,r.file_path,r.version,r.fingerprint,r.source_fingerprint,r.accepted,r.receipt
FROM receipt_key k CROSS JOIN generation_contract_boundary_receipt r
WHERE r.view_gen=k.view_gen AND r.repo_prefix=k.repo_prefix AND r.checkout_id=k.checkout_id AND r.file_path=k.file_path`
		rows, err := s.db.QueryContext(ctx, query, args...)
		if err != nil {
			return err
		}
		for rows.Next() {
			row, err := scanContractBoundaryReceipt(rows)
			if err == nil {
				err = visit(row)
			}
			if err != nil {
				_ = rows.Close()
				return err
			}
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return err
		}
		if err := rows.Close(); err != nil {
			return err
		}
	}
	return ctx.Err()
}
