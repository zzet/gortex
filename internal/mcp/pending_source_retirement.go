package mcp

import (
	"context"
	"time"

	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// retireDeadPendingSourceIncarnations consumes authoritative catalog evidence,
// not a grace deadline or filesystem observation. No catalog/read error can
// prove an incarnation dead. All SQL runs outside registry and session locks.
func (s *Server) retireDeadPendingSourceIncarnations() {
	type identity struct{ checkoutID, incarnation string }
	p := &s.pendingSourceFiles
	p.mu.Lock()
	identities := make(map[identity]struct{})
	for _, record := range p.versions {
		record.mu.RLock()
		if record.checkoutID != "" {
			identities[identity{record.checkoutID, record.checkoutIncarnation}] = struct{}{}
		}
		record.mu.RUnlock()
	}
	p.mu.Unlock()
	if len(identities) == 0 {
		return
	}
	var catalog *store_sqlite.Catalog
	if s.materializer != nil {
		catalog = s.materializer.Catalog
	}
	if catalog == nil {
		if store, ok := s.graph.(*store_sqlite.Store); ok {
			catalog = store.Catalog()
		}
	}
	if catalog == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	for id := range identities {
		checkout, found, err := catalog.GetCheckout(ctx, id.checkoutID)
		if err != nil {
			continue
		}
		if found && checkout.Incarnation == id.incarnation {
			continue
		}
		// Recheck identities under the registry lock: a newly admitted version
		// of a different incarnation must not be swept with the retired one.
		p.mu.Lock()
		for key, record := range p.versions {
			record.mu.RLock()
			dead := record.checkoutID == id.checkoutID && record.checkoutIncarnation == id.incarnation
			record.mu.RUnlock()
			if dead {
				delete(p.versions, key)
			}
		}
		for key := range p.freshCommits {
			if key.checkoutID == id.checkoutID && key.incarnation == id.incarnation {
				delete(p.freshCommits, key)
			}
		}
		p.mu.Unlock()
	}
}
