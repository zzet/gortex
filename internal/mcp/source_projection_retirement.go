package mcp

import "github.com/zzet/gortex/internal/pathkey"

// This work belongs only to the daemon's canonical backend, whose lifetime is
// independent of a request/view pin. Checkout tickets never use this fallback:
// their retirement needs checked metadata from the selected generation stack.
type sourceProjectionRetirement struct {
	registry *pendingProjections
	backend  projectionStore
	key      string
}

func (s *Server) canonicalProjectionRetirement(absPath string) *sourceProjectionRetirement {
	registry := pendingFileProjections
	if !registry.enabled || len(registry.snap.Load().entries) == 0 {
		return nil
	}
	backend, ok := s.graph.(projectionStore)
	if !ok {
		return nil
	}
	return &sourceProjectionRetirement{registry: registry, backend: backend, key: pathkey.MutationKey(absPath)}
}

func (work *sourceProjectionRetirement) run() {
	if work == nil {
		return
	}
	// Retain the physical key captured at admission, rather than resolving an
	// alias again after publication. The same hash/node/hash and COW checks run.
	work.registry.retireSelected(work.backend, map[string]struct{}{work.key: {}})
}
