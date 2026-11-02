package daemon

import "errors"

// EnableStartupMetadata allows the existing socket owner to serve connection
// metadata before a store/controller exists. Configure it before Listen.
func (s *Server) EnableStartupMetadata() { s.startupMetadata = true }

// PublishStartupRuntime is the single store-open handoff. The same listener
// and accepted connections survive it. Shutdown fences publication before it
// calls owners, so no completed store can be attached after close admission.
func (s *Server) PublishStartupRuntime(controller Controller, dispatcher MCPDispatcher, ready func() (bool, string), cancel, join func(), statePath string) error {
	s.runtimeMu.Lock()
	defer s.runtimeMu.Unlock()
	if s.runtimeClosing {
		return errors.New("daemon: startup publication refused after shutdown")
	}
	if !s.startupMetadata {
		return errors.New("daemon: startup runtime is already published")
	}
	s.Controller, s.MCPDispatcher, s.Ready = controller, dispatcher, ready
	s.CancelShutdownOwners, s.JoinRuntimeWriter = cancel, join
	// Before this handoff the startup reporter owns its failure record. Keep
	// that record available to the spawning CLI if opening/migration fails.
	s.statePath = statePath
	s.startupMetadata = false
	return nil
}

func (s *Server) StartupClosed() bool {
	s.runtimeMu.RLock()
	defer s.runtimeMu.RUnlock()
	return s.runtimeClosing
}

func (s *Server) currentDispatcher() MCPDispatcher {
	s.runtimeMu.RLock()
	defer s.runtimeMu.RUnlock()
	return s.MCPDispatcher
}

func (s *Server) currentController() (Controller, bool) {
	s.runtimeMu.RLock()
	defer s.runtimeMu.RUnlock()
	return s.Controller, s.startupMetadata
}

func (s *Server) currentReadiness() func() (bool, string) {
	s.runtimeMu.RLock()
	defer s.runtimeMu.RUnlock()
	return s.Ready
}

func (s *Server) closeRuntimeAdmission() (func(), func()) {
	s.runtimeMu.Lock()
	defer s.runtimeMu.Unlock()
	s.runtimeClosing = true
	return s.CancelShutdownOwners, s.JoinRuntimeWriter
}

func (s *Server) StartupPublished() bool {
	s.runtimeMu.RLock()
	defer s.runtimeMu.RUnlock()
	return !s.startupMetadata && !s.runtimeClosing
}
