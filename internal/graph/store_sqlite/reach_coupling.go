package store_sqlite

// NodeReadsDetached reports that reads stay consistent without ResolveMutex:
// each one runs on its own SQLite snapshot and returns decoded copies, never
// the rows a resolver pass is rewriting.
func (s *Store) NodeReadsDetached() bool {
	return true
}
