package store_sqlite

// ProxyNodeCountAtLeast uses the origin-namespaced proxy ID range. Proxy nodes
// normally live only in the in-memory federation layer, but this keeps the
// budget correct if one is ever handed to the durable store.
func (s *Store) ProxyNodeCountAtLeast(limit int) bool {
	if limit <= 0 {
		return false
	}
	var present int
	err := s.db.QueryRow(`
		SELECT EXISTS(
			SELECT 1 FROM nodes
			WHERE id >= 'remote:' AND id < 'remote;' AND view_gen = ?
			LIMIT 1 OFFSET ?
		)`, s.viewGen, limit-1).Scan(&present)
	if err != nil {
		panicOnFatal(err)
		return true
	}
	return present != 0
}
