package mcp

import "context"

// admitBackground shares one permanent admission gate with prune. Add happens
// before launch or a blocking pass lock, so DrainBackground owns both running
// work and admitted constructors/waiters. The returned completion is called
// exactly once after the whole owner, including its terminal work, returns.
func (s *Server) admitBackground() (context.Context, func(), bool) {
	s.backgroundMaintenanceMu.Lock()
	defer s.backgroundMaintenanceMu.Unlock()
	if s.backgroundMaintenanceDrained {
		return nil, nil, false
	}
	if s.backgroundContext == nil {
		s.backgroundContext, s.backgroundCancel = context.WithCancel(context.Background())
	}
	s.backgroundMaintenance.Add(1)
	return s.backgroundContext, s.backgroundMaintenance.Done, true
}

// BeginBackgroundClose revokes independent analysis/mining/reader lifetimes
// before lifecycle joins can wait on them. It never joins, reads the store,
// retires spill files or acquires an analysis/controller mutex.
func (s *Server) BeginBackgroundClose() {
	if s == nil {
		return
	}
	s.backgroundMaintenanceMu.Lock()
	s.backgroundMaintenanceDrained = true
	cancel := s.backgroundCancel
	s.backgroundMaintenanceMu.Unlock()
	if cancel != nil {
		cancel()
	}
	if s.healthBroadcaster != nil {
		s.healthBroadcaster.beginClose()
	}
}
