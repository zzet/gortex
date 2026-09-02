package graph

// Restub provenance handoff.
//
// When a file is re-parsed, its symbols are evicted and every incoming
// reference edge is restubbed to an `unresolved::<name>` target so the
// incoming-resolve pass can rebind it once the symbols come back. The edge
// keeps its identity, but its resolved provenance (origin / tier / confidence)
// is now a lie: it points at an unresolved stub. Two failure modes follow if
// the provenance is left untouched — a genuinely deleted definition leaves a
// stub advertising a compiler-grade tier, and a rebind to a *different* target
// inherits the old tier it never earned.
//
// StashRestubProvenance clears the live provenance and records it in the edge's
// Meta; RestoreRestubProvenance re-applies it only when the stub rebinds to the
// exact same target (an idempotent re-parse: unchanged binding, unchanged
// tier), and always drops the transient keys. The restub pass (indexer) and the
// incoming-resolve pass (resolver) live in different packages, so this handoff
// rides the edge Meta between them.
const (
	metaRestubPrevTo     = "restub_prev_to"
	metaRestubPrevOrigin = "restub_prev_origin"
	metaRestubPrevTier   = "restub_prev_tier"
	metaRestubPrevConf   = "restub_prev_conf"
)

// StashRestubProvenance records e's current target + resolved provenance in its
// Meta and clears the live provenance, so a restubbed edge no longer advertises
// the resolved tier it held while bound. Call it just before rewriting e.To to
// the unresolved stub. No-op on a nil edge.
func StashRestubProvenance(e *Edge) {
	if e == nil {
		return
	}
	if e.Meta == nil {
		e.Meta = map[string]any{}
	}
	e.Meta[metaRestubPrevTo] = e.To
	e.Meta[metaRestubPrevOrigin] = e.Origin
	e.Meta[metaRestubPrevTier] = e.Tier
	e.Meta[metaRestubPrevConf] = e.Confidence
	e.Origin, e.Tier, e.Confidence = "", "", 0
}

// RestoreRestubProvenance re-applies stashed provenance to e when e is now
// resolved to the SAME target it had before the restub — the binding is
// unchanged, so its verified tier is too — and always drops the transient stash
// keys (whether or not it restored). A rebind to a different target, or a stub
// that never rebound, keeps the honest fresh tier the resolver assigned (or
// none). No-op when there is nothing stashed.
//
// Returns true when it actually re-applied the stashed provenance — the caller
// uses that to persist the in-place mutation, since a disk backend hands out
// freshly-decoded edge pointers whose changes are lost unless written back.
func RestoreRestubProvenance(e *Edge) bool {
	if e == nil || e.Meta == nil {
		return false
	}
	prevTo, ok := e.Meta[metaRestubPrevTo]
	if !ok {
		return false
	}
	restored := false
	if to, isStr := prevTo.(string); isStr && to == e.To && !IsUnresolvedTarget(e.To) {
		if o, ok := e.Meta[metaRestubPrevOrigin].(string); ok {
			e.Origin = o
		}
		if tr, ok := e.Meta[metaRestubPrevTier].(string); ok {
			e.Tier = tr
		}
		if c, ok := e.Meta[metaRestubPrevConf].(float64); ok {
			e.Confidence = c
		}
		restored = true
	}
	delete(e.Meta, metaRestubPrevTo)
	delete(e.Meta, metaRestubPrevOrigin)
	delete(e.Meta, metaRestubPrevTier)
	delete(e.Meta, metaRestubPrevConf)
	return restored
}

// HasRestubProvenance reports whether e still carries the restub stash — the
// mark that this unresolved write is a surviving edge parked by the re-parse
// flow, not a genuinely new unresolved reference. Receipt recorders use it to
// keep the surviving source's file out of the forward resolution frontier:
// only the incoming/name frontier restores the stash, and a forward re-resolve
// would bind the stub with a fresh tier and silently drop the stashed one.
func HasRestubProvenance(e *Edge) bool {
	if e == nil || e.Meta == nil {
		return false
	}
	_, ok := e.Meta[metaRestubPrevTo]
	return ok
}
