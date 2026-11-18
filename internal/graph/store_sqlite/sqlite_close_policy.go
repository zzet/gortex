package store_sqlite

import (
	"database/sql"
	"database/sql/driver"

	"github.com/zzet/gortex/internal/sqliteconn"
)

// Graph pools suppress implicit last-close checkpoints. Store.Close still owns
// its explicit final checkpoint and its ordinary Close/export policy.
func openSQLiteStoreDB(dsn string) (*sql.DB, error) { return sqliteconn.Open(dsn) }

func newSQLiteStoreConnector(dsn string) (driver.Connector, error) {
	return sqliteconn.NewConnector(dsn)
}
