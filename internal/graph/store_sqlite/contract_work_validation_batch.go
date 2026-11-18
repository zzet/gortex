package store_sqlite

import (
	"context"
	"database/sql"

	"github.com/zzet/gortex/internal/graph"
)

const contractWorkValidationMaxTokens = 128

// Work and encoded scopes have passed encodeContractWork, including its token
// uniqueness and existing work limit. There is no cross-target/job cache. A
// found row, even an invalid one, permanently shadows lower selected sources.
func validateCapturedContractWorkBatchTx(ctx context.Context, tx *sql.Tx, chain []int64, work []graph.ContractWork, encoded [][]byte, variableLimit int) error {
	if len(work) != len(encoded) {
		return ErrCatalogInvalidValue
	}
	if variableLimit <= 0 {
		variableLimit = sqliteFallbackVariableLimit
	}
	for start := 0; start < len(work); {
		if err := ctx.Err(); err != nil {
			return err
		}
		maxRows := batchRowsForVariableLimit(variableLimit, 1, contractWorkValidationMaxTokens)
		end := start
		boundBytes := 24 // generation bind, with the existing interface-byte margin
		for end < len(work) && end-start < maxRows {
			rowBytes := sqliteBoundArgsBytes([]any{work[end].Token})
			if end > start && rowBytes > sqliteBatchMaxBoundBytes-boundBytes {
				break
			}
			boundBytes += rowBytes
			end++
		}
		requested := make(map[string]int, end-start)
		pending := make([]string, 0, end-start)
		for i := start; i < end; i++ {
			if _, duplicate := requested[work[i].Token]; duplicate {
				return ErrCatalogInvalidValue
			}
			requested[work[i].Token] = i
			pending = append(pending, work[i].Token)
		}
		// Store only each first row's equality result, not its possibly large scope.
		// Query/scan/close/cancellation errors remain authoritative even if a stale
		// expected row elsewhere in this page would have failed the old serial loop.
		matched := make(map[string]bool, len(pending))
		for _, generation := range chain {
			if len(pending) == 0 {
				break
			}
			if err := visitCapturedContractWorkPageTx(ctx, tx, generation, pending, &variableLimit, func(row capturedContractWorkRow) error {
				i, wanted := requested[row.token]
				if !wanted {
					return ErrCatalogInvalidValue
				}
				if _, duplicate := matched[row.token]; duplicate {
					return ErrCatalogInvalidValue
				}
				expected := work[i]
				matched[row.token] = row.origin == expected.OriginGeneration && row.checkout == expected.CheckoutID && row.repo == expected.RepoPrefix && row.file == expected.FilePath && row.version == expected.InputVersion && row.fingerprint == expected.InputFingerprint && row.state == string(graph.ContractWorkPending) && row.scope == string(encoded[i])
				return nil
			}); err != nil {
				return err
			}
			next := pending[:0]
			for _, token := range pending {
				if _, found := matched[token]; !found {
					next = append(next, token)
				}
			}
			pending = next
		}
		// Preserve requested order when selecting a stale/missing-work refusal.
		for i := start; i < end; i++ {
			if !matched[work[i].Token] {
				return ErrCatalogStaleGuard
			}
		}
		start = end
	}
	return ctx.Err()
}

type capturedContractWorkRow struct {
	token                                                    string
	origin                                                   int64
	checkout, repo, file, version, fingerprint, state, scope string
}

func visitCapturedContractWorkPageTx(ctx context.Context, tx *sql.Tx, generation int64, tokens []string, variableLimit *int, visit func(capturedContractWorkRow) error) error {
	for start := 0; start < len(tokens); {
		if err := ctx.Err(); err != nil {
			return err
		}
		count := min(len(tokens)-start, batchRowsForVariableLimit(*variableLimit, 1, contractWorkValidationMaxTokens))
		args := make([]any, 1, count+1)
		args[0] = generation
		for _, token := range tokens[start : start+count] {
			args = append(args, token)
		}
		rows, err := tx.QueryContext(ctx, `SELECT token,origin_generation,checkout_id,repo_prefix,file_path,input_version,input_fingerprint,state,scope FROM generation_contract_work WHERE view_gen=? AND token IN (`+inPlaceholders(count)+`)`, args...)
		if tooManySQLVariables(err) && count > 1 {
			lowerBatchVariableLimit(variableLimit, 1, count)
			continue
		}
		if err != nil {
			return err
		}
		err = func() (err error) {
			defer func() {
				if closeErr := rows.Close(); err == nil {
					err = closeErr
				}
			}()
			seen := 0
			for rows.Next() {
				if err := ctx.Err(); err != nil {
					return err
				}
				var row capturedContractWorkRow
				if err := rows.Scan(&row.token, &row.origin, &row.checkout, &row.repo, &row.file, &row.version, &row.fingerprint, &row.state, &row.scope); err != nil {
					return err
				}
				seen++
				if seen > count {
					return ErrCatalogInvalidValue
				}
				if err := visit(row); err != nil {
					return err
				}
			}
			if err := rows.Err(); err != nil {
				return err
			}
			return ctx.Err()
		}()
		if err != nil {
			return err
		}
		start += count
	}
	return ctx.Err()
}
