package graph

import "sort"

// Edge-level claims.
//
// A file eviction removes every edge into the evicted nodes wherever it was
// recorded. For an edge recorded outside every covered path, out of a source
// the delta does not claim, the delta has to express that removal. Claiming
// the source (claimSources) does it by copying the source's whole outgoing set
// into the working graph, which costs the size of every such source's
// adjacency: an evicted file that the rest of the repository calls into (a
// configuration package) claims thousands of sources to remove a handful of
// edges each, and the engine then re-states almost all of those edges in the
// same batch.
//
// An edge claim speaks for exactly the rows the eviction removes. It is kept
// per source at the edge's endpoint tuple (target, kind, recorded path): the
// composition hides every lower row out of the source with a claimed tuple,
// and the working graph holds the delta's own rows under those tuples (the
// re-stated edges, or none when the edge stays removed). The source's other
// edges stay in the view below, unread.
//
// Invariant: a claimed tuple is closed — every lower row with that tuple is a
// row the claim was made for — because an eviction claims every row into an
// evicted identity, and rows sharing a tuple share their source, target and
// recorded path. So a reader that sees only endpoint tuples (the endpoint
// projections carry no line) hides exactly the rows a full-row reader hides.
//
// A claim ends in one of two ways:
//
//   - a write that needs the source's whole set (a retarget, a removal, a
//     provenance update) claims the source; the tuple claims are dropped then,
//     after the materialization left their lower rows out;
//   - at Payload, a source whose claimed rows ended equal to the rows they
//     hid publishes nothing; one that differs (or is tombstoned) is claimed
//     then, and publishes an edge-source marker exactly as a source claimed at
//     eviction does.

// edgeTuple is an edge's identity without its source and line.
type edgeTuple struct {
	to       string
	kind     EdgeKind
	filePath string
}

// ClaimsEdgeEndpoints reports whether the layer speaks for single edges of a
// source whose whole outgoing set it does not replace: a lower edge out of
// from with that target, kind and recorded path is hidden, and the layer's own
// edges under the tuple are served instead. Only the delta's layer makes such
// claims; the compositions ask it by its concrete type.
func (l *deltaLayer) ClaimsEdgeEndpoints(from, to string, kind EdgeKind, filePath string) bool {
	if from == "" || !l.hasEdgeClaims.Load() {
		return false
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	if kinds := l.kindClaims[from]; kinds != nil {
		if _, ok := kinds[kind]; ok {
			return true
		}
	}
	tuples := l.edgeClaims[from]
	if tuples == nil {
		return false
	}
	_, ok := tuples[edgeTuple{to: to, kind: kind, filePath: filePath}]
	return ok
}

// ClaimsLowerEdge reports whether the layer speaks for one lower edge row: by
// its tuple, by its source's claimed kind, or by its exact identity.
func (l *deltaLayer) ClaimsLowerEdge(e *Edge) bool {
	if e == nil || e.From == "" || !l.hasEdgeClaims.Load() {
		return false
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	if tuples := l.edgeClaims[e.From]; tuples != nil {
		if _, ok := tuples[edgeTuple{to: e.To, kind: e.Kind, filePath: e.FilePath}]; ok {
			return true
		}
	}
	if kinds := l.kindClaims[e.From]; kinds != nil {
		if _, ok := kinds[e.Kind]; ok {
			return true
		}
	}
	if ids := l.identityClaims[e.From]; ids != nil {
		_, ok := ids[hashEdgeKey(keyOf(e))]
		return ok
	}
	return false
}

// layerClaimsEdge applies a layer's edge claims, when it has any, to one edge.
func layerClaimsEdge(l OverlayLayerReader, e *Edge) bool {
	dl, ok := l.(*deltaLayer)
	return ok && dl.ClaimsLowerEdge(e)
}

// overlayClaimsBaseEdge reports whether the view's layer claims one lower
// edge row.
func (v *OverlaidView) overlayClaimsBaseEdge(e *Edge) bool {
	if v == nil || v.layer == nil {
		return false
	}
	dl, ok := v.layer.(*deltaLayer)
	return ok && dl.ClaimsLowerEdge(e)
}

// overlayClaimsBaseEndpoints reports whether the view's layer claims every
// lower row of an endpoint tuple. Identity claims are not closed over a
// tuple, so only tuple and kind claims answer here; an identity claim is
// never made on an import edge, the kind the delta's endpoint projections
// serve (claimRowsForWrite).
func (v *OverlaidView) overlayClaimsBaseEndpoints(from, to string, kind EdgeKind, filePath string) bool {
	if v == nil || v.layer == nil {
		return false
	}
	dl, ok := v.layer.(*deltaLayer)
	return ok && dl.ClaimsEdgeEndpoints(from, to, kind, filePath)
}

// edgeWriteClaimed reports whether a write of e lands under an edge claim: its
// source is not claimed whole, its path is not covered, and its tuple is
// claimed. The working graph holds the delta's rows for that tuple, so the
// write needs no further claim.
func (dw *DeltaWriter) edgeWriteClaimed(e *Edge) bool {
	return e != nil && (dw.layer.ClaimsEdgeEndpoints(e.From, e.To, e.Kind, e.FilePath) || dw.layer.ClaimsLowerEdge(e))
}

// claimEdges makes the delta speak for each lower row: its tuple is claimed
// for its source, and the row is kept for the payload's comparison. The rows
// are the composition's current rows out of sources neither claimed nor
// covered. Callers hold writeMu.
func (dw *DeltaWriter) claimEdges(rows []*Edge) {
	if len(rows) == 0 {
		return
	}
	var imports []*Edge
	dw.layer.mu.Lock()
	if dw.layer.edgeClaims == nil {
		dw.layer.edgeClaims = make(map[string]map[edgeTuple]struct{})
	}
	if dw.claimedBelow == nil {
		dw.claimedBelow = make(map[string]map[edgeHash]*Edge)
	}
	// A tuple claimed by an earlier call already recorded every lower row it
	// hides (the invariant above); a row under it now is the working graph's
	// own, which the eviction removes there.
	type sourceTuple struct {
		from string
		t    edgeTuple
	}
	fresh := make(map[sourceTuple]struct{})
	claimed := 0
	for _, e := range rows {
		if e == nil || e.From == "" {
			continue
		}
		if _, whole := dw.layer.claimed[e.From]; whole {
			continue
		}
		tuples := dw.layer.edgeClaims[e.From]
		if tuples == nil {
			tuples = make(map[edgeTuple]struct{})
			dw.layer.edgeClaims[e.From] = tuples
		}
		t := edgeTuple{to: e.To, kind: e.Kind, filePath: e.FilePath}
		key := sourceTuple{from: e.From, t: t}
		if _, dup := tuples[t]; dup {
			if _, now := fresh[key]; !now {
				continue
			}
		} else {
			tuples[t] = struct{}{}
			fresh[key] = struct{}{}
			claimed++
		}
		below := dw.claimedBelow[e.From]
		if below == nil {
			below = make(map[edgeHash]*Edge)
			dw.claimedBelow[e.From] = below
		}
		h := hashEdgeKey(keyOf(e))
		if _, seen := below[h]; !seen {
			below[h] = e
		}
		if e.Kind == EdgeImports {
			imports = append(imports, e)
		}
	}
	if claimed > 0 {
		dw.layer.hasEdgeClaims.Store(true)
	}
	dw.layer.mu.Unlock()
	// The importer's adjacency changes with the claim, so its path is read
	// live by the import projections from now on.
	dw.noteImportSources(imports)
	dw.statsMu.Lock()
	dw.stats.EdgeClaims += claimed
	dw.statsMu.Unlock()
}

// dropEdgeClaimsLocked forgets the edge claims of sources the delta now claims
// whole. Callers hold writeMu and the layer's write lock.
func (dw *DeltaWriter) dropEdgeClaimsLocked(ids []string) {
	if len(dw.layer.edgeClaims) == 0 {
		return
	}
	for _, id := range ids {
		delete(dw.layer.edgeClaims, id)
		delete(dw.layer.identityClaims, id)
		delete(dw.layer.kindClaims, id)
		delete(dw.claimedBelow, id)
	}
}

// edgeClaimSources lists the sources with edge claims, sorted.
func (dw *DeltaWriter) edgeClaimSources() []string {
	dw.layer.mu.RLock()
	out := make([]string, 0, len(dw.layer.edgeClaims))
	for id := range dw.layer.edgeClaims {
		out = append(out, id)
	}
	dw.layer.mu.RUnlock()
	sort.Strings(out)
	return out
}

// edgeClaimRestated reports whether a source's claimed rows ended equal to
// the lower rows they hide, as the published generation will compose them:
// rows recorded at a replaced path are the path's to publish, and a lower row
// whose endpoint the generation hides is not served either way.
func (dw *DeltaWriter) edgeClaimRestated(id string, inFinal func(string) bool, identityVisible func(string) bool) bool {
	// Every working-graph row of a source the delta does not claim whole,
	// outside the covered paths, is a row under one of its claims; rows at a
	// covered path are the path's to publish.
	dw.layer.mu.RLock()
	below := dw.claimedBelow[id]
	var final []*Edge
	for _, e := range dw.work.GetOutEdges(id) {
		if e == nil || inFinal(e.FilePath) {
			continue
		}
		if _, covered := dw.layer.covered[e.FilePath]; covered {
			continue
		}
		final = append(final, e)
	}
	var hidden, under []*Edge
	for _, e := range below {
		if inFinal(e.FilePath) {
			continue
		}
		if _, covered := dw.layer.covered[e.FilePath]; covered {
			continue
		}
		under = append(under, e)
		if identityVisible(id) && identityVisible(e.From) && identityVisible(e.To) {
			hidden = append(hidden, e)
		}
	}
	dw.layer.mu.RUnlock()
	if deltaEdgeSetsEqual(final, hidden) {
		return true
	}
	if !identityVisible(id) {
		return false
	}
	// A source whose final rows are all rows it hid, unchanged, and whose
	// missing rows are all of a kind no pass of the save re-decides
	// (edgeKindsSurvivingEviction), lost them only to the eviction: they
	// still hold. The claim is dropped and the lower layer keeps serving them,
	// instead of the source being claimed whole and republished without them.
	// A missing row of any other kind (a reference the restub made the
	// incoming pass re-decide, say) is a removal, and is published.
	// Every claimed lower row is weighed, a row into an identity the
	// generation hides included: dropping the claim would serve it again.
	return deltaEdgeSubsetMissingOnly(final, under, func(e *Edge) bool {
		return edgeKindsSurvivingEviction[e.Kind] && identityVisible(e.From) && identityVisible(e.To)
	})
}

// edgeKindsSurvivingEviction are the kinds of rows another file records into
// an identity that no pass of a save re-derives or re-decides when the
// identity's file is re-derived and the identity survives: a method's
// member_of its receiver type (derived from the method's own declaration), a
// test's tests edge into the function it exercises, a goroutine launch's
// spawns into the launched function, and a JSX parent's renders_child into
// the child component (each derived from the source's own file, bound by
// name, and kept by no restub). A clone partner's similar_to is not one: it
// holds only while both bodies are unchanged, which the delta's clone carry
// decides and re-states.
var edgeKindsSurvivingEviction = map[EdgeKind]bool{
	EdgeMemberOf:     true,
	EdgeTests:        true,
	EdgeSpawns:       true,
	EdgeRendersChild: true,
}

// deltaEdgeSubsetMissingOnly reports whether every row of a has a row of b
// with the same identity and the same persisted content, and every row of b
// with no counterpart in a may go missing. An identity b holds more than once
// is not decided here (false).
func deltaEdgeSubsetMissingOnly(a, b []*Edge, mayGo func(*Edge) bool) bool {
	if len(a) > len(b) {
		return false
	}
	byKey := make(map[edgeHash][]*Edge, len(b))
	for _, e := range b {
		if e == nil {
			return false
		}
		h := hashEdgeKey(keyOf(e))
		byKey[h] = append(byKey[h], e)
	}
	matched := make(map[edgeHash]struct{}, len(a))
	for _, e := range a {
		if e == nil {
			return false
		}
		h := hashEdgeKey(keyOf(e))
		candidates := byKey[h]
		if len(candidates) != 1 || !deltaEdgeContentEqual(e, candidates[0]) {
			return false
		}
		matched[h] = struct{}{}
	}
	for h, rows := range byKey {
		if _, ok := matched[h]; ok {
			continue
		}
		for _, e := range rows {
			if !mayGo(e) {
				return false
			}
		}
	}
	return true
}

// settleEdgeClaims claims whole, for the payload, every source whose edge
// claims did not end restating the rows they hid, and every tombstoned one.
// It returns the sources it claimed and counts the rest as dropped sources.
// Callers hold writeMu.
func (dw *DeltaWriter) settleEdgeClaims(tombstoned map[string]struct{}, inFinal func(string) bool, identityVisible func(string) bool, out *DeltaPayload) []string {
	var promote []string
	for _, id := range dw.edgeClaimSources() {
		if _, carried := tombstoned[id]; !carried && dw.edgeClaimRestated(id, inFinal, identityVisible) {
			out.DroppedSources++
			continue
		}
		promote = append(promote, id)
	}
	if len(promote) == 0 {
		return nil
	}
	dw.claimSources(promote)
	dw.statsMu.Lock()
	dw.stats.EdgeClaimsPromoted += len(promote)
	dw.statsMu.Unlock()
	return promote
}
