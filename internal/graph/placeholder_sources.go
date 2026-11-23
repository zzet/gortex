package graph

import "strings"

// From-side placeholder reconciliation.
//
// Extractors key dataflow edges (arg_of, value_flow) FROM the same
// `unresolved::` placeholder string a sibling reference edge carries as its
// target. The resolver rewrites unresolved TARGETS, but nothing ever
// re-pointed the from-side placeholders — a production audit found ~160k
// dataflow edges permanently stuck on placeholder sources, invisible to
// taint/flow queries. When a resolution batch rewrites `unresolved::X → N`
// at a site, the dataflow edges keyed from that placeholder AT THE SAME
// file+line re-point to N in the same breath.
//
// The site match is deliberately EXACT: placeholder strings (e.g.
// `repo/unresolved::*.id`) are shared across many sites, and different
// sites resolve to different nodes — a blanket from==placeholder repoint
// would mis-bind. Non-matching placeholders simply stay pending for the
// pass that resolves their own site.

// PlaceholderRepoint names one eligible rewrite: dataflow edges from
// OldFrom at (FilePath, Line) move to NewFrom.
type PlaceholderRepoint struct {
	OldFrom  string
	FilePath string
	Line     int
	NewFrom  string
}

// PlaceholderSourceKind reports whether kind participates in from-side
// placeholder reconciliation. Deliberately narrow: the audit found exactly
// these two classes stuck.
func PlaceholderSourceKind(kind EdgeKind) bool {
	return kind == EdgeArgOf || kind == EdgeValueFlow
}

// PlaceholderSourceRepoints extracts the eligible repoints from a
// resolution reindex batch: entries whose OldTo was an unresolved
// placeholder and whose edge now carries a real target.
//
// Two placeholder string conventions coexist for the SAME site: reference
// targets stay in the bare `unresolved::X` form, while applyRepoPrefix
// prefixes dataflow SOURCES like node IDs — `<repo>/unresolved::X` (a form
// IsUnresolvedTarget does not even classify, which is precisely why no pass
// ever touched these edges). Each eligible entry therefore emits a repoint
// per candidate form; probing an absent form is one empty adjacency lookup.
func PlaceholderSourceRepoints(reindexes []EdgeReindex) []PlaceholderRepoint {
	var out []PlaceholderRepoint
	for _, r := range reindexes {
		if r.Edge == nil || r.OldTo == "" {
			continue
		}
		if !IsUnresolvedTarget(r.OldTo) || IsUnresolvedTarget(r.Edge.To) || r.Edge.To == "" {
			continue
		}
		out = append(out, PlaceholderRepoint{
			OldFrom:  r.OldTo,
			FilePath: r.Edge.FilePath,
			Line:     r.Edge.Line,
			NewFrom:  r.Edge.To,
		})
		if prefix := RepoPrefixOfID(r.Edge.From); prefix != "" && strings.HasPrefix(r.OldTo, UnresolvedMarker) {
			out = append(out, PlaceholderRepoint{
				OldFrom:  prefix + "/" + r.OldTo,
				FilePath: r.Edge.FilePath,
				Line:     r.Edge.Line,
				NewFrom:  r.Edge.To,
			})
		}
	}
	return out
}

// ReconcilePlaceholderSources applies the repoints through the store's
// exact-site candidate contract. The SQLite backend probes its from/kind
// index and decodes only matching rows; it closes every read cursor before
// this function starts the ReindexEdges write. The in-memory backend filters
// its adjacency buckets without copying edge payloads. Point lookups remain
// forbidden inside resolver passes.
// Returns the number of dataflow edges re-pointed.
func ReconcilePlaceholderSources(g Store, repoints []PlaceholderRepoint) int {
	moved, _ := ReconcilePlaceholderSourcesApplied(g, repoints)
	return moved
}

// ReconcilePlaceholderSourcesApplied is ReconcilePlaceholderSources with
// checked writer-gate reporting when the backing store supports
// ReindexEdgesApplied.
func ReconcilePlaceholderSourcesApplied(g Store, repoints []PlaceholderRepoint) (int, bool) {
	if g == nil || len(repoints) == 0 {
		return 0, true
	}
	candidateKinds := [...]EdgeKind{EdgeArgOf, EdgeValueFlow}
	sites := make([]EdgeSite, 0, len(repoints)*len(candidateKinds))
	seenSites := make(map[EdgeSite]struct{}, cap(sites))
	for _, rp := range repoints {
		if rp.OldFrom == "" || rp.NewFrom == "" || rp.OldFrom == rp.NewFrom {
			continue
		}
		for _, kind := range candidateKinds {
			site := EdgeSite{From: rp.OldFrom, Line: rp.Line, Kind: kind}
			if _, dup := seenSites[site]; dup {
				continue
			}
			seenSites[site] = struct{}{}
			sites = append(sites, site)
		}
	}
	if len(sites) == 0 {
		return 0, true
	}
	candidates := LookupEdgeCandidates(g, nil, sites)
	var batch []EdgeReindex
	for _, rp := range repoints {
		if rp.OldFrom == "" || rp.NewFrom == "" || rp.OldFrom == rp.NewFrom {
			continue
		}
		for _, kind := range candidateKinds {
			for _, e := range candidates.Site(rp.OldFrom, rp.Line, kind) {
				if e == nil || !PlaceholderSourceKind(e.Kind) {
					continue
				}
				// A duplicate repoint (two references at one site resolving in
				// the same batch) must not double-move an edge the first
				// repoint already claimed.
				if e.From != rp.OldFrom {
					continue
				}
				if e.FilePath != rp.FilePath || e.Line != rp.Line {
					continue
				}
				oldTo, oldKind := e.To, e.Kind
				e.From = rp.NewFrom
				// RefreshIdentity routes both backends through their from-aware
				// identity repair — the default reindex path only handles
				// target moves.
				batch = append(batch, EdgeReindex{
					Edge:            e,
					OldFrom:         rp.OldFrom,
					OldTo:           oldTo,
					OldKind:         oldKind,
					OldFilePath:     e.FilePath,
					OldLine:         e.Line,
					RefreshIdentity: true,
				})
			}
		}
	}
	if len(batch) == 0 {
		return 0, true
	}
	if writer, ok := g.(interface {
		ReindexEdgesApplied([]EdgeReindex) bool
	}); ok {
		if !writer.ReindexEdgesApplied(batch) {
			return 0, false
		}
		return len(batch), true
	}
	g.ReindexEdges(batch)
	return len(batch), true
}
