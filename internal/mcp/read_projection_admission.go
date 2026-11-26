package mcp

import (
	"context"

	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/pathguard"
)

// Read-built publication can request eventual canonical catch-up, without
// retaining a ticket or borrowing physical-write/ACK/completion authority.
func (s *Server) admitReadBuiltProjection(ctx context.Context, root string, idx *indexer.Indexer, tracked bool, entry *projectionEntry) {
	if !tracked || pathguard.ResolveRoot(root) != pathguard.ResolveRoot(idx.RootPath()) {
		return
	}
	if watcher, ok := s.currentWatcher().(interface {
		EnqueueToolMutation(context.Context, string) (*indexer.MutationTicket, error)
	}); ok {
		ticket, err := watcher.EnqueueToolMutation(ctx, entry.absPath)
		if err == nil && ticket != nil {
			noteReadBuiltCatchUp(pendingFileProjections, entry, ticket)
			return
		}
	}
	// This is only a request observation. A later cache hit cannot infer an
	// accepted admission or completion from the absence of this optional field.
	if _, ok := ctx.Value(requestProjectionFactsKey{}).(*requestProjectionFacts); ok {
		markRequestProjectionDeferred(ctx, entry.graphPath, "watcher_unavailable")
	}
}

// The catch-up's completion only permits a later read of the file to run the
// checked retirement for this entry; it drops nothing itself, so no read that
// already retained the entry can be refused for it.
func noteReadBuiltCatchUp(registry *pendingProjections, entry *projectionEntry, ticket *indexer.MutationTicket) {
	if ticket.Done == nil {
		return
	}
	go func() {
		if result, ok := <-ticket.Done; ok && result.Err == nil {
			registry.mu.Lock()
			entry.catchUpPublished = true
			registry.mu.Unlock()
		}
	}()
}
