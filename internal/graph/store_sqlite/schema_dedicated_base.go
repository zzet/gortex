package store_sqlite

import "database/sql"

// createDedicatedBasePublicationsTable is the additive v22 catalog migration.
// It shares its idempotent DDL with cold initialization and performs no payload
// scan, authority installation, generation allocation, or active-pointer write.
func createDedicatedBasePublicationsTable(tx *sql.Tx) error {
	_, err := tx.Exec(dedicatedBasePublicationsSchemaSQL)
	return err
}
