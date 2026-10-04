package store_sqlite

import "database/sql"

const contractBoundaryReceiptTableBody = ` (
 view_gen INTEGER NOT NULL,repo_prefix TEXT NOT NULL,checkout_id TEXT NOT NULL,file_path TEXT NOT NULL,
 version TEXT NOT NULL,fingerprint TEXT NOT NULL,source_fingerprint TEXT NOT NULL,accepted INTEGER NOT NULL,
 receipt BLOB NOT NULL,
 PRIMARY KEY(view_gen,repo_prefix,checkout_id,file_path)
) WITHOUT ROWID`

const contractBoundaryKeysTableBody = ` (
 view_gen INTEGER NOT NULL,key_kind TEXT NOT NULL,lookup_key TEXT NOT NULL,
 repo_prefix TEXT NOT NULL,checkout_id TEXT NOT NULL,file_path TEXT NOT NULL,
 PRIMARY KEY(view_gen,key_kind,lookup_key,repo_prefix,checkout_id,file_path)
) WITHOUT ROWID`

const contractBoundaryBaselineTableBody = ` (
 view_gen INTEGER NOT NULL,repo_prefix TEXT NOT NULL,checkout_id TEXT NOT NULL,
 version TEXT NOT NULL,fingerprint TEXT NOT NULL,
 PRIMARY KEY(view_gen,repo_prefix,checkout_id)
) WITHOUT ROWID`

const contractBoundaryKeysFileIndexDDL = `CREATE INDEX IF NOT EXISTS contract_boundary_keys_file ON generation_contract_boundary_keys(view_gen,repo_prefix,checkout_id,file_path,key_kind,lookup_key);`

func createContractBoundaryReceiptTables(tx *sql.Tx) error {
	for _, t := range []generationMaskTable{{table: "generation_contract_boundary_receipt", body: contractBoundaryReceiptTableBody}, {table: "generation_contract_boundary_keys", body: contractBoundaryKeysTableBody}, {table: "generation_contract_boundary_baseline", body: contractBoundaryBaselineTableBody}} {
		if _, err := tx.Exec(`CREATE TABLE IF NOT EXISTS ` + t.table + t.body); err != nil {
			return err
		}
	}
	_, err := tx.Exec(contractBoundaryKeysFileIndexDDL)
	return err
}
