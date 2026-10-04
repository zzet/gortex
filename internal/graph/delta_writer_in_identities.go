package graph

// GetInEdgeIdentitiesByNodeIDs answers an incoming-adjacency batch as logical
// identities through the stack: the store at the bottom's identity projection
// (no payload, provenance or metadata decoded), each layer's rows as
// identities, every row kept only when no layer above its level hides it —
// the rows GetInEdgesByNodeIDs serves, without their payload. With a per-stack
// projection cache installed over a delta, the composition of the part of the
// stack the cache is kept for is kept per stack and identity (those layers
// are immutable), and the layers above it — the dirty chain's and the delta's
// own — are applied per read. A bottom store with no
// identity projection answers from the composed rows.
func (dw *DeltaWriter) GetInEdgeIdentitiesByNodeIDs(ids []string) map[string][]EdgeIdentity {
	s := dw.stack()
	reader, ok := s.base.(InEdgeIdentityBatchReader)
	if !ok {
		return incomingEdgeIdentityMap(ids, dw.GetInEdgesByNodeIDs(ids))
	}
	uniq := make([]string, 0, len(ids))
	seen := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		if id == "" {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		uniq = append(uniq, id)
	}
	if len(uniq) == 0 {
		return make(map[string][]EdgeIdentity)
	}
	k, split := dw.cacheSplit(s)
	if !split {
		return dw.composeInIdentities(s, reader, uniq)
	}
	below := deltaStack{base: s.base, layers: s.layers[:k]}
	kept := dw.baseCache.stackInIdentities(uniq, func(missing []string) map[string][]EdgeIdentity {
		return dw.composeInIdentities(below, reader, missing)
	})
	out := make(map[string][]EdgeIdentity, len(uniq))
	for _, id := range uniq {
		for _, identity := range kept[id] {
			e := Edge{From: identity.From, To: identity.To, Kind: identity.Kind, FilePath: identity.FilePath, Line: identity.Line}
			if s.edgeVisibleFrom(&e, k) {
				out[id] = append(out[id], identity)
			}
		}
	}
	for i := k; i < len(s.layers); i++ {
		for id, edges := range dw.layerAdjacency(s.layers[i], uniq, true) {
			for _, e := range edges {
				if e != nil && e.To == id && s.edgeVisibleFrom(e, i+1) {
					out[id] = append(out[id], EdgeIdentityFor(e))
				}
			}
		}
	}
	for id, identities := range out {
		if len(identities) == 0 {
			delete(out, id)
		}
	}
	return out
}

// composeInIdentities is the identity composition over stack s for distinct,
// nonempty ids.
func (dw *DeltaWriter) composeInIdentities(s deltaStack, reader InEdgeIdentityBatchReader, ids []string) map[string][]EdgeIdentity {
	out := make(map[string][]EdgeIdentity, len(ids))
	rows := reader.GetInEdgeIdentitiesByNodeIDs(ids)
	for _, id := range ids {
		for _, identity := range rows[id] {
			if identity.To != id {
				continue
			}
			e := Edge{From: identity.From, To: identity.To, Kind: identity.Kind, FilePath: identity.FilePath, Line: identity.Line}
			if s.edgeVisibleFrom(&e, 0) {
				out[id] = append(out[id], identity)
			}
		}
	}
	for i, l := range s.layers {
		for id, edges := range dw.layerAdjacency(l, ids, true) {
			for _, e := range edges {
				if e != nil && e.To == id && s.edgeVisibleFrom(e, i+1) {
					out[id] = append(out[id], EdgeIdentityFor(e))
				}
			}
		}
	}
	for id, identities := range out {
		if len(identities) == 0 {
			delete(out, id)
		}
	}
	return out
}

// stackInIdentities answers the stack's incoming identities below the delta
// per target identity from the cache or loads the missing ones.
func (c *BaseProjectionCache) stackInIdentities(ids []string, load func([]string) map[string][]EdgeIdentity) map[string][]EdgeIdentity {
	out := make(map[string][]EdgeIdentity, len(ids))
	var missing []string
	c.mu.Lock()
	if c.stackInIDs == nil {
		c.stackInIDs = make(map[string][]EdgeIdentity)
	}
	for _, id := range ids {
		if rows, ok := c.stackInIDs[id]; ok {
			out[id] = rows
			c.stackInIDHit++
			continue
		}
		missing = append(missing, id)
	}
	c.stackInIDMis += len(missing)
	c.mu.Unlock()
	if len(missing) == 0 {
		return out
	}
	loaded := load(missing)
	c.mu.Lock()
	for _, id := range missing {
		rows := loaded[id]
		if rows == nil {
			rows = []EdgeIdentity{}
		}
		c.stackInIDs[id] = rows
		out[id] = rows
	}
	c.mu.Unlock()
	return out
}

// StackInIdentityStats reports the stack-level incoming identity hits and
// misses.
func (c *BaseProjectionCache) StackInIdentityStats() (hits, misses int) {
	if c == nil {
		return 0, 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.stackInIDHit, c.stackInIDMis
}

var _ InEdgeIdentityBatchReader = (*DeltaWriter)(nil)
