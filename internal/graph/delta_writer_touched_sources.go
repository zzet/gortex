package graph

import "sort"

// TouchedSources returns the sources the delta wrote or claimed rows of any of
// kinds out of: every source of a working-graph edge of the kinds, every
// source whose whole outgoing set it claims, and every source it holds edge
// claims for. A derived pass run over a delta names, through it, every source
// whose rows of its kinds the pass may have changed (a callee's change
// reaching its callers included).
func (dw *DeltaWriter) TouchedSources(kinds ...EdgeKind) []string {
	seen := make(map[string]struct{})
	for _, kind := range kinds {
		for e := range dw.work.EdgesByKind(kind) {
			if e != nil && e.From != "" {
				seen[e.From] = struct{}{}
			}
		}
	}
	l := dw.layer
	l.mu.RLock()
	for id := range l.claimed {
		seen[id] = struct{}{}
	}
	for id := range l.edgeClaims {
		seen[id] = struct{}{}
	}
	l.mu.RUnlock()
	out := make([]string, 0, len(seen))
	for id := range seen {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}
