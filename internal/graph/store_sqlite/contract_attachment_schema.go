package store_sqlite

import "database/sql"

const contractInputStateTableBody = ` (
 view_gen INTEGER NOT NULL,
 repo_prefix TEXT NOT NULL,
 checkout_id TEXT NOT NULL,
 input_version TEXT NOT NULL,
 input_fingerprint TEXT NOT NULL,
 previous_input_version TEXT NOT NULL,
 previous_input_fingerprint TEXT NOT NULL,
 accepted INTEGER NOT NULL CHECK(accepted IN (0,1)),
 PRIMARY KEY(view_gen, repo_prefix, checkout_id)
) WITHOUT ROWID`

const contractAttachmentSchemaSQL = `
CREATE TABLE IF NOT EXISTS contract_attachments (
 repo_prefix TEXT NOT NULL,
 checkout_id TEXT NOT NULL,
 input_version TEXT NOT NULL,
 input_fingerprint TEXT NOT NULL,
 payload_generation INTEGER NOT NULL,
 PRIMARY KEY(repo_prefix, checkout_id, input_version, input_fingerprint)
) WITHOUT ROWID;
CREATE INDEX IF NOT EXISTS contract_attachment_payload ON contract_attachments(payload_generation);
CREATE INDEX IF NOT EXISTS contract_input_identity ON generation_contract_input_state(repo_prefix, checkout_id, input_version, input_fingerprint);
CREATE TABLE IF NOT EXISTS contract_attachment_work (
 repo_prefix TEXT NOT NULL,
 checkout_id TEXT NOT NULL,
 attachment_version TEXT NOT NULL,
 attachment_fingerprint TEXT NOT NULL,
 token TEXT NOT NULL,
 origin_generation INTEGER NOT NULL,
 file_path TEXT NOT NULL,
 input_version TEXT NOT NULL,
 input_fingerprint TEXT NOT NULL,
 scope TEXT NOT NULL,
 PRIMARY KEY(repo_prefix, checkout_id, attachment_version, attachment_fingerprint, token)
) WITHOUT ROWID;
CREATE INDEX IF NOT EXISTS contract_attachment_work_token ON contract_attachment_work(repo_prefix, checkout_id, token);
`

func createContractAttachmentTables(tx *sql.Tx) error {
	if _, err := tx.Exec(`CREATE TABLE IF NOT EXISTS generation_contract_input_state` + contractInputStateTableBody); err != nil {
		return err
	}
	_, err := tx.Exec(contractAttachmentSchemaSQL)
	return err
}

// Historical attachments remain referenced while any core generation retains
// their input identity, or debt rows retain exact acknowledged old scopes.
const contractAttachmentReferenceSQL = `EXISTS(SELECT 1 FROM contract_attachments a
 WHERE a.payload_generation=? AND (
 EXISTS(SELECT 1 FROM generation_contract_input_state i WHERE i.repo_prefix=a.repo_prefix AND i.checkout_id=a.checkout_id AND ((i.input_version=a.input_version AND i.input_fingerprint=a.input_fingerprint) OR (i.previous_input_version=a.input_version AND i.previous_input_fingerprint=a.input_fingerprint)))
 OR EXISTS(SELECT 1 FROM contract_attachment_work w JOIN generation_contract_work d ON d.repo_prefix=w.repo_prefix AND d.checkout_id=w.checkout_id AND d.token=w.token AND d.origin_generation=w.origin_generation AND d.file_path=w.file_path AND d.input_version=w.input_version AND d.input_fingerprint=w.input_fingerprint AND d.scope=w.scope WHERE w.repo_prefix=a.repo_prefix AND w.checkout_id=a.checkout_id AND w.attachment_version=a.input_version AND w.attachment_fingerprint=a.input_fingerprint)))`

const contractWorkAcknowledgedSQL = `EXISTS(SELECT 1 FROM contract_attachment_work a JOIN contract_attachments h ON h.repo_prefix=a.repo_prefix AND h.checkout_id=a.checkout_id AND h.input_version=a.attachment_version AND h.input_fingerprint=a.attachment_fingerprint JOIN view_generations v ON v.generation_id=h.payload_generation
 WHERE a.repo_prefix=d.repo_prefix AND a.checkout_id=d.checkout_id AND a.token=d.token AND a.origin_generation=d.origin_generation AND a.file_path=d.file_path AND a.input_version=d.input_version AND a.input_fingerprint=d.input_fingerprint AND a.scope=d.scope AND v.state IN ('ready','superseded'))`
