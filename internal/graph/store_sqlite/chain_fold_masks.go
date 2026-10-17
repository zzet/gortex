package store_sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// Receipt keys precede this member's receipt rows: only receipts from members
// above may suppress them. These clauses are shared with the one-shot flatten.
type foldMaskOperation struct {
	table string
	keys  []string
	query string
	args  func(member, to int64) []any
}

var foldMaskOperations = []foldMaskOperation{
	{table: "generation_contract_input_state", keys: []string{"repo_prefix", "checkout_id"}, query: `INSERT OR IGNORE INTO generation_contract_input_state(view_gen,repo_prefix,checkout_id,input_version,input_fingerprint,accepted,previous_input_version,previous_input_fingerprint) SELECT ?,repo_prefix,checkout_id,input_version,input_fingerprint,accepted,previous_input_version,previous_input_fingerprint FROM generation_contract_input_state WHERE view_gen=?`, args: func(member, to int64) []any { return []any{to, member} }},
	{table: "generation_contract_boundary_keys", keys: []string{"key_kind", "lookup_key", "repo_prefix", "checkout_id", "file_path"}, query: `INSERT OR IGNORE INTO generation_contract_boundary_keys(view_gen,key_kind,lookup_key,repo_prefix,checkout_id,file_path) SELECT ?,key_kind,lookup_key,repo_prefix,checkout_id,file_path FROM generation_contract_boundary_keys k WHERE k.view_gen=? AND NOT EXISTS(SELECT 1 FROM generation_contract_boundary_receipt r WHERE r.view_gen=? AND r.repo_prefix=k.repo_prefix AND r.checkout_id=k.checkout_id AND r.file_path=k.file_path)`, args: func(member, to int64) []any { return []any{to, member, to} }},
	{table: "generation_contract_boundary_receipt", keys: []string{"repo_prefix", "checkout_id", "file_path"}, query: `INSERT OR IGNORE INTO generation_contract_boundary_receipt(view_gen,repo_prefix,checkout_id,file_path,version,fingerprint,source_fingerprint,accepted,receipt) SELECT ?,repo_prefix,checkout_id,file_path,version,fingerprint,source_fingerprint,accepted,receipt FROM generation_contract_boundary_receipt WHERE view_gen=?`, args: func(member, to int64) []any { return []any{to, member} }},
	{table: "generation_contract_boundary_baseline", keys: []string{"repo_prefix", "checkout_id"}, query: `INSERT OR IGNORE INTO generation_contract_boundary_baseline(view_gen,repo_prefix,checkout_id,version,fingerprint) SELECT ?,repo_prefix,checkout_id,version,fingerprint FROM generation_contract_boundary_baseline WHERE view_gen=?`, args: func(member, to int64) []any { return []any{to, member} }},
	{table: "generation_contract_work", keys: []string{"token"}, query: `INSERT OR IGNORE INTO generation_contract_work
 (view_gen, token, origin_generation, checkout_id, repo_prefix, file_path, input_version, input_fingerprint, state, scope)
 SELECT ?, token, origin_generation, checkout_id, repo_prefix, file_path, input_version, input_fingerprint, state, scope
 FROM generation_contract_work d WHERE view_gen = ? AND NOT ` + contractWorkAcknowledgedSQL, args: func(member, to int64) []any { return []any{to, member} }},
	{table: "generation_file_masks", keys: []string{"repo_prefix", "file_path"}, query: `
INSERT OR IGNORE INTO generation_file_masks (view_gen, repo_prefix, file_path, ownership_mode)
SELECT ?, repo_prefix, file_path, ownership_mode FROM generation_file_masks
 WHERE view_gen = ? AND ownership_mode <> ?`, args: func(member, to int64) []any { return []any{to, member, string(OwnershipContext)} }},
	{table: "generation_node_tombstones", keys: []string{"node_id"}, query: `
INSERT OR IGNORE INTO generation_node_tombstones (view_gen, node_id, claim_kind)
SELECT ?, node_id, claim_kind FROM generation_node_tombstones
 WHERE view_gen = ?`, args: func(member, to int64) []any { return []any{to, member} }},
	{table: "generation_edge_sources", keys: []string{"source_id"}, query: `
INSERT OR IGNORE INTO generation_edge_sources (view_gen, source_id, ownership_mode)
SELECT ?, source_id, ownership_mode FROM generation_edge_sources
 WHERE view_gen = ?`, args: func(member, to int64) []any { return []any{to, member} }},
	{table: "generation_producer_completeness", keys: []string{"producer"}, query: `
INSERT INTO generation_producer_completeness (view_gen, producer, state, reason)
SELECT ?, producer, state, reason FROM generation_producer_completeness WHERE view_gen = ?
ON CONFLICT(view_gen, producer) DO UPDATE SET
  state  = CASE WHEN excluded.state <> 'complete' AND generation_producer_completeness.state = 'complete'
                THEN excluded.state ELSE generation_producer_completeness.state END,
  reason = CASE WHEN excluded.state <> 'complete' AND generation_producer_completeness.state = 'complete'
                THEN excluded.reason ELSE generation_producer_completeness.reason END`, args: func(member, to int64) []any { return []any{to, member} }},
}

// A serialized in-package test seam, after mask work and before commit.
// Production leaves it nil.
var chainFoldMaskStepHook func(context.Context, string) error

// Suppressed rows also advance the source-key cursor. All reads and writes
// remain in the same checked writer transaction.
func foldMaskPageTx(ctx context.Context, tx *sql.Tx, op foldMaskOperation, member, to int64, after []any, budget int) ([]any, bool, error) {
	keys := strings.Join(op.keys, ", ")
	tuple := "(" + keys + ")"
	if len(op.keys) == 1 {
		tuple = keys
	}
	where := "view_gen = ?"
	args := []any{member}
	if after != nil {
		where += " AND " + tuple + " > (" + inPlaceholders(len(after)) + ")"
		args = append(args, after...)
	}
	rows, err := tx.QueryContext(ctx, "SELECT "+keys+" FROM "+op.table+" WHERE "+where+" ORDER BY "+keys+" LIMIT ?", append(args, budget)...)
	if err != nil {
		return nil, false, err
	}
	count := 0
	var last []any
	for rows.Next() {
		values := make([]string, len(op.keys))
		scan := make([]any, len(op.keys))
		for i := range values {
			scan[i] = &values[i]
		}
		if err = rows.Scan(scan...); err != nil {
			break
		}
		last = make([]any, len(values))
		for i, value := range values {
			last[i] = value
		}
		count++
	}
	if err == nil {
		err = rows.Err()
	}
	closeErr := rows.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return nil, false, err
	}
	if count == 0 {
		return nil, true, nil
	}
	bounded := where + " AND " + tuple + " <= (" + inPlaceholders(len(last)) + ")"
	pageArgs := append(append([]any(nil), args...), last...)
	query := strings.Replace(op.query, "FROM "+op.table, "FROM fold_mask_page", 1)
	query = "WITH fold_mask_page AS (SELECT * FROM " + op.table + " WHERE " + bounded + ") " + query
	_, err = tx.ExecContext(ctx, query, append(pageArgs, op.args(member, to)...)...)
	if err != nil {
		return nil, false, fmt.Errorf("store_sqlite: fold mask page %s: %w", op.table, err)
	}
	return last, count < budget, nil
}

// Context-only rows are not speakers, but still advance the read cursor.
func foldMemberIDsPageTx(ctx context.Context, tx *sql.Tx, member int64, after []any, contextPaths map[string]struct{}, budget int) (map[string]struct{}, []any, bool, error) {
	where := "view_gen = ?"
	args := []any{member}
	if after != nil {
		where += " AND id > ?"
		args = append(args, after[0])
	}
	rows, err := tx.QueryContext(ctx, "SELECT id, file_path FROM nodes WHERE "+where+" ORDER BY id LIMIT ?", append(args, budget)...)
	if err != nil {
		return nil, nil, false, err
	}
	ids := map[string]struct{}{}
	var last []any
	count := 0
	for rows.Next() {
		var id, path string
		if err = rows.Scan(&id, &path); err != nil {
			break
		}
		last = []any{id}
		count++
		if _, contextOnly := contextPaths[path]; !contextOnly {
			ids[id] = struct{}{}
		}
	}
	if err == nil {
		err = rows.Err()
	}
	closeErr := rows.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return nil, nil, false, err
	}
	return ids, last, count < budget, nil
}
