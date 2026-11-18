package store_sqlite

// stopStoreBackgroundReaders isolates pool-wide ownership assertions from the
// checkpoint and maintenance workers. Both stop methods join their worker.
func stopStoreBackgroundReaders(s *Store) {
	s.stopCheckpointLoop()
	s.stopMaintenanceLane()
}
