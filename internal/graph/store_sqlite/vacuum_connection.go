package store_sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
)

// SQLite's deprecated temp_store_directory pragma changes a process-global
// pointer. Serialize our dedicated FILE connections, including restoration and
// physical close. Ordinary store connections retain temp_store(MEMORY).
// This is separate from each store's write gate: two open stores can compact
// concurrently otherwise. Waiting for this guard remains cancelable.
var vacuumTempDirectoryGate sqliteWriteGate

// ErrVacuumTempDirectory means no VACUUM ran because its scratch volume could
// not be confirmed. Callers must skip instead of assuming the system temp
// volume has the headroom checked for the store directory.
var ErrVacuumTempDirectory = errors.New("sqlite vacuum: temporary directory unconfirmed")

func sqliteVacuumDSN(path string) string {
	return strings.Replace(sqliteWriterDSN(path), "temp_store(MEMORY)", "temp_store(FILE)", 1)
}

// withVacuumConnection is called under the maintenance lane and write gate.
// The callback is also the test seam for inspecting the actual dedicated
// connection; it is not used to change the pooled reader/writer policy.
func (s *Store) withVacuumConnection(ctx context.Context, run func(*sql.Conn) error) (result error) {
	if err := vacuumTempDirectoryGate.LockContext(ctx); err != nil {
		return err
	}
	defer vacuumTempDirectoryGate.Unlock()

	// Ask the existing physical database, rather than interpreting a possibly
	// escaped file: URI or guessing a default path. A memory database has no
	// main filename and is handled separately by vacuum.
	rows, err := s.writerDB.QueryContext(ctx, `PRAGMA database_list`)
	if err != nil {
		return fmt.Errorf("%w: main filename: %w", ErrVacuumTempDirectory, err)
	}
	var path string
	for rows.Next() {
		var sequence int
		var name, file string
		if err := rows.Scan(&sequence, &name, &file); err != nil {
			return errors.Join(fmt.Errorf("%w: main filename: %w", ErrVacuumTempDirectory, err), rows.Close())
		}
		if name == "main" {
			path = file
		}
	}
	err = errors.Join(rows.Err(), rows.Close())
	if err != nil || path == "" {
		return errors.Join(fmt.Errorf("%w: main filename unavailable", ErrVacuumTempDirectory), err)
	}

	db, err := openSQLiteStoreDB(sqliteVacuumDSN(path))
	if err != nil {
		return err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	defer func() { result = errors.Join(result, db.Close()) }()
	conn, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, conn.Close()) }()

	var mode int
	if err := conn.QueryRowContext(ctx, `PRAGMA temp_store`).Scan(&mode); err != nil || mode != 1 {
		return errors.Join(fmt.Errorf("%w: temp_store=%d, want FILE", ErrVacuumTempDirectory, mode), err)
	}
	previous, err := vacuumTempDirectory(ctx, conn)
	if err != nil {
		return fmt.Errorf("%w: read previous directory: %w", ErrVacuumTempDirectory, err)
	}
	// Restoration is owned cleanup, not canceled work. Join it and physical
	// close before releasing the process-wide guard, even when VACUUM fails or
	// ctx is canceled. No bounded-cleanup claim is made for this SQLite call.
	defer func() {
		result = errors.Join(result, setVacuumTempDirectory(context.Background(), conn, previous))
	}()
	if err := setVacuumTempDirectory(ctx, conn, filepath.Dir(path)); err != nil {
		return fmt.Errorf("%w: %w", ErrVacuumTempDirectory, err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return run(conn)
}

func vacuumTempDirectory(ctx context.Context, conn *sql.Conn) (string, error) {
	var directory string
	err := conn.QueryRowContext(ctx, `PRAGMA temp_store_directory`).Scan(&directory)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil // SQLite reports no row for its unset global directory.
	}
	return directory, err
}

func setVacuumTempDirectory(ctx context.Context, conn *sql.Conn, directory string) error {
	// PRAGMA assignment does not accept bound parameters. Quote the exact
	// directory as a SQLite string, including embedded single quotes.
	statement := `PRAGMA temp_store_directory='` + strings.ReplaceAll(directory, "'", "''") + `'`
	if _, err := conn.ExecContext(ctx, statement); err != nil {
		return fmt.Errorf("set vacuum temp directory: %w", err)
	}
	actual, err := vacuumTempDirectory(ctx, conn)
	if err != nil {
		return fmt.Errorf("verify vacuum temp directory: %w", err)
	}
	if actual != directory {
		return fmt.Errorf("verify vacuum temp directory: got %q, want %q", actual, directory)
	}
	return nil
}
