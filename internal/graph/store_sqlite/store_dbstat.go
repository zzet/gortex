package store_sqlite

import "os"

// DBStats returns the on-disk size of the SQLite database file and its
// write-ahead log, in bytes. A missing file (or a store opened without a
// path) reports 0 for that component. Surfaced in daemon_health so a
// runaway WAL high-water mark is observable instead of silently filling
// the disk.
func (s *Store) DBStats() (dbBytes, walBytes int64) {
	if s.coreless() || s.dbPath == "" {
		return 0, 0
	}
	if fi, err := os.Stat(s.dbPath); err == nil {
		dbBytes = fi.Size()
	}
	if fi, err := os.Stat(s.dbPath + "-wal"); err == nil {
		walBytes = fi.Size()
	}
	return dbBytes, walBytes
}
