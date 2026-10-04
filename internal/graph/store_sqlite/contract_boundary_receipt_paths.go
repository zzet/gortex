package store_sqlite

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/zzet/gortex/internal/graph"
)

// ContractBoundaryReceiptsForPathsContext reloads exact physical staged rows
// after the caller's outer core acceptance fence. Missing requested paths have
// nil values; this does not certify a baseline or infer accepted absence.
func (s *Store) ContractBoundaryReceiptsForPathsContext(ctx context.Context, repo, actor string, paths []string) (map[string]*graph.ContractBoundaryReceipt, error) {
	if err := s.requireContractBoundaryContext(ctx); err != nil {
		return nil, err
	}
	if len(paths) > 64 {
		return nil, ErrContractBoundaryReceiptLimit
	}
	out := make(map[string]*graph.ContractBoundaryReceipt, len(paths))
	unique := make([]string, 0, len(paths))
	for _, path := range paths {
		if path == "" {
			return nil, fmt.Errorf("%w: empty receipt path", ErrCatalogInvalidValue)
		}
		if _, found := out[path]; !found {
			out[path] = nil
			unique = append(unique, path)
		}
	}
	if len(unique) == 0 {
		return out, nil
	}
	query := `SELECT ` + contractBoundaryReceiptColumns + ` FROM generation_contract_boundary_receipt WHERE view_gen=? AND repo_prefix=? AND checkout_id=? AND file_path IN (` + inPlaceholders(len(unique)) + `)`
	args := []any{s.viewGen, repo, actor}
	args = append(args, toAnyArgs(unique)...)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var encodedBytes int
	for rows.Next() {
		row, err := scanContractBoundaryReceipt(rows)
		if err != nil {
			return nil, err
		}
		encoded, err := json.Marshal(row)
		if err != nil {
			return nil, err
		}
		encodedBytes += len(encoded)
		if encodedBytes > 4*contractBoundaryPayloadLimit {
			return nil, ErrContractBoundaryReceiptLimit
		}
		out[row.FilePath] = &row
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return out, nil
}
