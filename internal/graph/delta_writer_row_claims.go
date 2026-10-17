package graph

import "sort"

// Row and kind claims: the writes after an eviction that change single rows of
// a source the delta does not otherwise hold.
//
// A retarget, a provenance update, an exact removal or a kind eviction of an
// edge out of a source outside every covered path used to claim the source
// whole, which copies its complete outgoing set into the working graph. The
// resolver's rebinds and the derived passes touch a handful of rows each of
// sources with hundreds of edges, so a one-declaration change claimed tens of
// foreign sources and thousands of rows. These claims speak for exactly the
// rows the write addresses:
//
//   - an identity claim hides one lower row by its exact identity; a retarget
//     claims the row's old identity (materializing that one row so the write
//     applies to it) and its new one (which the lower view must not already
//     hold, or the source is claimed whole);
//   - a kind claim hides every lower row of one kind out of the source; a
//     kind eviction makes it, recording those rows, and a later write of that
//     kind lands under it.
//
// Identity claims are not closed over an endpoint tuple, so they are never
// made on import edges, the kind the delta's endpoint projections serve; an
// import write claims its source whole as before. Payload treats every
// partially claimed source alike (settleEdgeClaims): unchanged, it publishes
// nothing; changed, it is claimed whole and published under a marker.

// belowEdgesByIdentity reads, through the stack below the delta, the rows
// with exactly these identities that the view below serves. ok is false when
// the store at the bottom has no identity lookup.
func (dw *DeltaWriter) belowEdgesByIdentity(keys []edgeKey) (map[edgeHash]*Edge, bool) {
	out := make(map[edgeHash]*Edge, len(keys))
	if len(keys) == 0 {
		return out, true
	}
	s := dw.stack()
	if len(s.layers) > 0 && s.layers[len(s.layers)-1] == OverlayLayerReader(dw.layer) {
		s.layers = s.layers[:len(s.layers)-1]
	}
	finder, ok := s.base.(EdgeIdentityBatchFinder)
	if !ok {
		return nil, false
	}
	want := make(map[edgeHash]struct{}, len(keys))
	ids := make([]EdgeIdentity, 0, len(keys))
	var sources []string
	seenSource := make(map[string]struct{})
	for _, k := range keys {
		h := hashEdgeKey(k)
		if _, dup := want[h]; dup {
			continue
		}
		want[h] = struct{}{}
		ids = append(ids, EdgeIdentity(k))
		if _, dup := seenSource[k.From]; !dup {
			seenSource[k.From] = struct{}{}
			sources = append(sources, k.From)
		}
	}
	for _, e := range finder.FindEdgesByIdentities(ids) {
		if e != nil && s.edgeVisibleFrom(e, 0) {
			out[hashEdgeKey(keyOf(e))] = e
		}
	}
	for i, l := range s.layers {
		for _, edges := range dw.layerAdjacency(l, sources, false) {
			for _, e := range edges {
				if e == nil {
					continue
				}
				h := hashEdgeKey(keyOf(e))
				if _, wanted := want[h]; wanted && s.edgeVisibleFrom(e, i+1) {
					out[h] = e
				}
			}
		}
	}
	return out, true
}

// ensurePartialLocked registers a source as partially claimed. Callers hold
// the layer's write lock.
func (dw *DeltaWriter) ensurePartialLocked(id string) {
	if dw.layer.edgeClaims == nil {
		dw.layer.edgeClaims = make(map[string]map[edgeTuple]struct{})
	}
	if dw.layer.edgeClaims[id] == nil {
		dw.layer.edgeClaims[id] = make(map[edgeTuple]struct{})
	}
	if dw.claimedBelow == nil {
		dw.claimedBelow = make(map[string]map[edgeHash]*Edge)
	}
	if dw.claimedBelow[id] == nil {
		dw.claimedBelow[id] = make(map[edgeHash]*Edge)
	}
	dw.layer.hasEdgeClaims.Store(true)
}

// identityClaimableLocked reports whether a row out of a source can be
// claimed by identity: the source is not held whole, its path is not
// covered, and the row is not an import. Callers hold the layer's lock.
func (dw *DeltaWriter) identityClaimableLocked(k edgeKey) bool {
	if k.From == "" || k.Kind == EdgeImports {
		return false
	}
	if _, whole := dw.layer.claimed[k.From]; whole {
		return false
	}
	if _, covered := dw.layer.covered[k.FilePath]; covered && k.FilePath != "" {
		return false
	}
	return true
}

func (dw *DeltaWriter) identityClaimedLocked(k edgeKey) bool {
	if kinds := dw.layer.kindClaims[k.From]; kinds != nil {
		if _, ok := kinds[k.Kind]; ok {
			return true
		}
	}
	if ids := dw.layer.identityClaims[k.From]; ids != nil {
		if _, ok := ids[hashEdgeKey(k)]; ok {
			return true
		}
	}
	if tuples := dw.layer.edgeClaims[k.From]; tuples != nil {
		if _, ok := tuples[edgeTuple{to: k.To, kind: k.Kind, filePath: k.FilePath}]; ok {
			return true
		}
	}
	return false
}

// claimRows makes the delta speak for the rows under these identities (the
// rows a write addresses as they are now) and, when materialize is set,
// copies each lower one into the working graph so the write applies to it. A
// row already held (under a claim, or by a source held whole or a covered
// path) needs nothing. It returns the sources that must be claimed whole
// instead. Callers hold writeMu.
func (dw *DeltaWriter) claimRows(keys []edgeKey, materialize bool) []string {
	var fallback, lookup []edgeKey
	dw.layer.mu.RLock()
	for _, k := range keys {
		if k.From == "" {
			continue
		}
		if _, whole := dw.layer.claimed[k.From]; whole {
			continue
		}
		if _, covered := dw.layer.covered[k.FilePath]; covered && k.FilePath != "" {
			continue
		}
		if !dw.identityClaimableLocked(k) {
			fallback = append(fallback, k)
			continue
		}
		if dw.identityClaimedLocked(k) {
			continue
		}
		lookup = append(lookup, k)
	}
	dw.layer.mu.RUnlock()
	if len(lookup) > 0 {
		below, ok := dw.belowEdgesByIdentity(lookup)
		if !ok {
			fallback = append(fallback, lookup...)
			lookup = nil
		}
		var rows []*Edge
		dw.layer.mu.Lock()
		for _, k := range lookup {
			h := hashEdgeKey(k)
			dw.ensurePartialLocked(k.From)
			if dw.layer.identityClaims == nil {
				dw.layer.identityClaims = make(map[string]map[edgeHash]struct{})
			}
			if dw.layer.identityClaims[k.From] == nil {
				dw.layer.identityClaims[k.From] = make(map[edgeHash]struct{})
			}
			dw.layer.identityClaims[k.From][h] = struct{}{}
			if row := below[h]; row != nil {
				dw.claimedBelow[k.From][h] = row
				if materialize {
					rows = append(rows, cloneDeltaEdge(row))
				}
			}
		}
		if len(rows) > 0 {
			dw.work.materializeRows(nil, rows)
		}
		dw.layer.mu.Unlock()
		dw.noteBuiltinTargets(rows)
		dw.noteMaterialized(0, len(rows))
		dw.statsMu.Lock()
		dw.stats.IdentityClaims += len(lookup)
		dw.statsMu.Unlock()
	}
	return edgeKeySources(fallback)
}

// claimNewRows makes the delta speak for the identities a write is about to
// create out of partially claimed or unclaimed sources: the view below must
// hold no row under them, or the source is claimed whole. Callers hold
// writeMu.
func (dw *DeltaWriter) claimNewRows(keys []edgeKey) []string {
	var fallback, lookup []edgeKey
	dw.layer.mu.RLock()
	for _, k := range keys {
		if k.From == "" {
			continue
		}
		if _, whole := dw.layer.claimed[k.From]; whole {
			continue
		}
		if _, covered := dw.layer.covered[k.FilePath]; covered && k.FilePath != "" {
			continue
		}
		if !dw.identityClaimableLocked(k) {
			fallback = append(fallback, k)
			continue
		}
		if dw.identityClaimedLocked(k) || dw.work.storedEdge(k) != nil {
			continue
		}
		lookup = append(lookup, k)
	}
	dw.layer.mu.RUnlock()
	if len(lookup) == 0 {
		return edgeKeySources(fallback)
	}
	below, ok := dw.belowEdgesByIdentity(lookup)
	if !ok {
		return edgeKeySources(append(fallback, lookup...))
	}
	dw.layer.mu.Lock()
	for _, k := range lookup {
		h := hashEdgeKey(k)
		if below[h] != nil {
			fallback = append(fallback, k)
			continue
		}
		dw.ensurePartialLocked(k.From)
		if dw.layer.identityClaims == nil {
			dw.layer.identityClaims = make(map[string]map[edgeHash]struct{})
		}
		if dw.layer.identityClaims[k.From] == nil {
			dw.layer.identityClaims[k.From] = make(map[edgeHash]struct{})
		}
		dw.layer.identityClaims[k.From][h] = struct{}{}
	}
	dw.layer.mu.Unlock()
	return edgeKeySources(fallback)
}

func edgeKeySources(keys []edgeKey) []string {
	if len(keys) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(keys))
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		if _, dup := seen[k.From]; !dup {
			seen[k.From] = struct{}{}
			out = append(out, k.From)
		}
	}
	sort.Strings(out)
	return out
}

// claimKinds makes the delta speak for every lower row of these kinds out of
// each source it neither holds whole nor covers: the rows are recorded for
// the payload and hidden, and later writes of those kinds out of the source
// land under the claim. rows is the source's current outgoing set through
// the delta. Callers hold writeMu.
func (dw *DeltaWriter) claimKinds(ids []string, kinds []EdgeKind, rows map[string][]*Edge) []string {
	wanted := make(map[EdgeKind]struct{}, len(kinds))
	for _, k := range kinds {
		if k == EdgeImports {
			return ids // import adjacency is claimed whole (see above)
		}
		wanted[k] = struct{}{}
	}
	claimed := 0
	dw.layer.mu.Lock()
	for _, id := range ids {
		if id == "" {
			continue
		}
		if _, whole := dw.layer.claimed[id]; whole {
			continue
		}
		dw.ensurePartialLocked(id)
		if dw.layer.kindClaims == nil {
			dw.layer.kindClaims = make(map[string]map[EdgeKind]struct{})
		}
		if dw.layer.kindClaims[id] == nil {
			dw.layer.kindClaims[id] = make(map[EdgeKind]struct{})
		}
		for _, e := range rows[id] {
			if e == nil {
				continue
			}
			if _, ok := wanted[e.Kind]; !ok {
				continue
			}
			if _, covered := dw.layer.covered[e.FilePath]; covered && e.FilePath != "" {
				continue
			}
			k := keyOf(e)
			if dw.work.storedEdge(k) != nil {
				continue // the delta's own row, removed from the working graph
			}
			h := hashEdgeKey(k)
			if _, seen := dw.claimedBelow[id][h]; !seen {
				dw.claimedBelow[id][h] = e
				claimed++
			}
		}
		for k := range wanted {
			dw.layer.kindClaims[id][k] = struct{}{}
		}
	}
	dw.layer.mu.Unlock()
	dw.statsMu.Lock()
	dw.stats.KindClaimRows += claimed
	dw.statsMu.Unlock()
	return nil
}
