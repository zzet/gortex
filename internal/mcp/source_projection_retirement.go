package mcp

import (
	"context"

	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/pathguard"
	"github.com/zzet/gortex/internal/pathkey"
)

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

// A canonical read that selects a projection after the watcher has published
// every generation admitted for its file answers from the published graph
// once the graph holds the projection's bytes, retiring it with the same
// checks a publication receipt runs. Only an edit's receipt retires its own
// projection; a projection a read built, for an out-of-band write or to
// replace an edit's projection during that edit's retirement, has no receipt.
// Without this every later read of the file serves it and reports pending
// relations until the file changes again. The read retires the projection
// before retaining it, so this read is never refused for it; while any
// generation for the file is admitted, reads keep serving the projection, and
// a read-built projection stays until the catch-up its read admitted has
// completed. Like read-built admission, it applies only to a file read through
// the owning indexer's own root: equal bytes in another checkout are not
// evidence about the canonical backend.
func (s *Server) retireCaughtUpProjection(ctx context.Context, root string, idx *indexer.Indexer, entry *projectionEntry) bool {
	if view := requestViewFromContext(ctx); view.routed() || view.readsOwnCheckout() {
		return false // a selected checkout retires through its own reader
	}
	if entry.repoPrefix != idx.RepoPrefix() || pathguard.ResolveRoot(root) != pathguard.ResolveRoot(idx.RootPath()) {
		return false
	}
	watcher, ok := s.currentWatcher().(interface {
		TierState(string) indexer.MutationTierState
	})
	if !ok || watcher.TierState(entry.absPath).Generation != 0 {
		return false
	}
	backend, ok := s.graph.(projectionStore)
	return ok && pendingFileProjections.retireCaughtUp(backend, entry)
}
