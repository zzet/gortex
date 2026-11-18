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
		query := contractBoundaryReceiptVisitQuery(len(chunk))
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

// A full-row WITHOUT ROWID seek may compare large receipt records at each tree
// level. Use a sequential primary-key range only when the payload-free index
// proves that every physical key in that range was requested. Otherwise retain
// exact point reads. The 129th metadata key rules out a range beyond the existing
// 128-key chunk; this does not change any payload, result or caller limit.
// The unary + keeps SQLite from turning the range back into repeated IN seeks.
func contractBoundaryReceiptVisitQuery(n int) string {
	return `WITH receipt_key AS MATERIALIZED (
 SELECT view_gen,repo_prefix,checkout_id,file_path
 FROM generation_contract_boundary_receipt INDEXED BY contract_boundary_receipt_stamps
 WHERE view_gen=? AND repo_prefix=? AND checkout_id=? AND file_path IN (` + inPlaceholders(n) + `)
),
 bounds AS MATERIALIZED (SELECT count(*) AS n,min(view_gen) AS g,min(repo_prefix) AS repo,min(checkout_id) AS actor,min(file_path) AS lo,max(file_path) AS hi FROM receipt_key),
 span_keys AS MATERIALIZED (
 SELECT s.file_path FROM bounds b CROSS JOIN generation_contract_boundary_receipt s INDEXED BY contract_boundary_receipt_stamps
 WHERE b.n>1 AND s.view_gen=b.g AND s.repo_prefix=b.repo AND s.checkout_id=b.actor AND s.file_path>=b.lo AND s.file_path<=b.hi LIMIT 129
 ),
 dense AS MATERIALIZED (SELECT b.n>1 AND b.n=(SELECT count(*) FROM span_keys) AS use_range FROM bounds b)
SELECT r.repo_prefix,r.checkout_id,r.file_path,r.version,r.fingerprint,r.source_fingerprint,r.accepted,r.receipt
FROM dense d CROSS JOIN bounds b CROSS JOIN generation_contract_boundary_receipt r NOT INDEXED
WHERE d.use_range AND r.view_gen=b.g AND r.repo_prefix=b.repo AND r.checkout_id=b.actor AND r.file_path>=b.lo AND r.file_path<=b.hi
 AND +r.file_path IN (SELECT file_path FROM receipt_key)
UNION ALL
SELECT r.repo_prefix,r.checkout_id,r.file_path,r.version,r.fingerprint,r.source_fingerprint,r.accepted,r.receipt
FROM dense d CROSS JOIN receipt_key k CROSS JOIN generation_contract_boundary_receipt r
WHERE NOT d.use_range AND r.view_gen=k.view_gen AND r.repo_prefix=k.repo_prefix AND r.checkout_id=k.checkout_id AND r.file_path=k.file_path`
}
