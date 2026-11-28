package resolver

import (
	"os"
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// placeholderSourceIndex is the per-pass exact-site identity census for
// unresolved dataflow sources. The light scan transfers no Meta blobs. Keeping
// identities by site matters because one wildcard placeholder can be shared by
// thousands of source locations resolved across many compute chunks: decoding
// that placeholder's complete adjacency once per chunk caused quadratic churn.
type placeholderSourceIndex struct {
	built  bool
	bySite map[placeholderSourceSite][]graph.EdgeIdentity

	// deferred holds, for a ResolveAll pass, the repoints of every page whose
	// site the census knows; they are applied once, after the last page
	// (applyDeferred). retargeted maps the census identity of a
	// placeholder-sourced dataflow edge the pass itself re-targeted to its
	// current identity, so the deferred move finds it.
	deferred   []graph.PlaceholderRepoint
	retargeted map[graph.EdgeIdentity]graph.EdgeIdentity
}

type placeholderSourceSite struct {
	from     string
	filePath string
	line     int
}

type placeholderSourceMove struct {
	identity graph.EdgeIdentity
	newFrom  string
}

const placeholderSourceReindexBatchSize = 256

func (idx *placeholderSourceIndex) ensure(g graph.Store) {
	if idx.built {
		return
	}
	idx.built = true
	for e := range graph.EdgesLightSeq(g, graph.EdgeArgOf, graph.EdgeValueFlow) {
		if e == nil || !strings.Contains(e.From, graph.UnresolvedMarker) {
			continue
		}
		if idx.bySite == nil {
			idx.bySite = make(map[placeholderSourceSite][]graph.EdgeIdentity)
		}
		site := placeholderSourceSite{from: e.From, filePath: e.FilePath, line: e.Line}
		idx.bySite[site] = append(idx.bySite[site], graph.EdgeIdentityFor(e))
	}
}

// beginPass discards the previous pass's site census and per-pass re-target
// map while keeping any deferred repoints a refused write left unapplied.
func (idx *placeholderSourceIndex) beginPass() {
	idx.built = false
	idx.bySite = nil
	idx.retargeted = nil
}

func (idx *placeholderSourceIndex) hasDeferred() bool {
	return len(idx.deferred) > 0
}

func (idx *placeholderSourceIndex) retainRepoints(repoints []graph.PlaceholderRepoint) {
	if len(repoints) == 0 {
		return
	}
	idx.deferred = append(idx.deferred, repoints...)
	idx.beginPass()
}

func (idx *placeholderSourceIndex) takeDeferred() []graph.PlaceholderRepoint {
	if len(idx.deferred) == 0 {
		return nil
	}
	out := idx.deferred
	idx.deferred = nil
	return out
}

func (idx *placeholderSourceIndex) inheritDeferred(repoints []graph.PlaceholderRepoint) {
	if len(repoints) == 0 {
		return
	}
	idx.deferred = append(idx.deferred, repoints...)
	idx.beginPass()
}

// pruneSites removes any deferred placeholder repoint whose destination matches
// a cross-package guard revert at the same site, preventing a rejected weak-tier
// binding from moving a dataflow source onto an unimported target on retry.
func (idx *placeholderSourceIndex) pruneSites(reverts []graph.PlaceholderRepoint) {
	if len(idx.deferred) == 0 || len(reverts) == 0 {
		return
	}
	rejected := make(map[placeholderSourceSite]struct{}, len(reverts))
	for _, rev := range reverts {
		rejected[placeholderSourceSite{from: rev.OldFrom, filePath: rev.FilePath, line: rev.Line}] = struct{}{}
	}
	kept := idx.deferred[:0]
	for _, rp := range idx.deferred {
		if _, drop := rejected[placeholderSourceSite{from: rp.NewFrom, filePath: rp.FilePath, line: rp.Line}]; drop {
			continue
		}
		kept = append(kept, rp)
	}
	if len(kept) == 0 {
		idx.deferred = nil
		return
	}
	idx.deferred = kept
}

// filter keeps only repoints whose exact source site exists in the dataflow
// placeholder census. It is used only by stores without exact identity lookup.
func (idx *placeholderSourceIndex) filter(repoints []graph.PlaceholderRepoint) []graph.PlaceholderRepoint {
	if len(idx.bySite) == 0 {
		return nil
	}
	kept := make([]graph.PlaceholderRepoint, 0, len(repoints))
	for _, rp := range repoints {
		site := placeholderSourceSite{from: rp.OldFrom, filePath: rp.FilePath, line: rp.Line}
		if len(idx.bySite[site]) != 0 {
			kept = append(kept, rp)
		}
	}
	return kept
}

// take returns each exact edge identity at most once, preserving repoint order
// so conflicting resolutions retain the legacy first-wins behaviour. Claimed
// sites leave the census immediately; a later compute chunk cannot decode or
// attempt to move them again.
func (idx *placeholderSourceIndex) take(repoints []graph.PlaceholderRepoint) []placeholderSourceMove {
	if len(idx.bySite) == 0 {
		return nil
	}
	var moves []placeholderSourceMove
	for _, rp := range repoints {
		if rp.OldFrom == "" || rp.NewFrom == "" || rp.OldFrom == rp.NewFrom {
			continue
		}
		site := placeholderSourceSite{from: rp.OldFrom, filePath: rp.FilePath, line: rp.Line}
		identities := idx.bySite[site]
		if len(identities) == 0 {
			continue
		}
		delete(idx.bySite, site)
		for _, identity := range identities {
			moves = append(moves, placeholderSourceMove{identity: identity, newFrom: rp.NewFrom})
		}
	}
	return moves
}

// defer records one page's repoints for the end of a ResolveAll pass instead
// of applying them now.
//
// Applying them per page made the pass's result depend on how it was paged: a
// dataflow edge sourced from a placeholder whose reference resolved in an
// earlier page was moved to the resolved source before its own page ran, and
// its page then either resolved its callee against the moved source or (a
// spooled page rehydrated by the old site) dropped it, while in one page the
// same edge kept its source and bound its callee. Deferring every move to the
// end gives each edge exactly one resolution attempt against the source it had
// when the pass began, then the move — the single-page outcome, whatever the
// page size. Only repoints whose site holds a placeholder-sourced dataflow edge
// are kept, so the deferred set is bounded by the census, not by the pass.
func (idx *placeholderSourceIndex) deferRepoints(g graph.Store, reindexes []graph.EdgeReindex) {
	if len(reindexes) == 0 || os.Getenv("GORTEX_RESOLVE_FROM_RECONCILE") == "0" {
		return
	}
	idx.ensure(g)
	for _, ri := range reindexes {
		e := ri.Edge
		if e == nil || !graph.PlaceholderSourceKind(e.Kind) || !strings.Contains(e.From, graph.UnresolvedMarker) {
			continue
		}
		// A census edge the pass re-targeted: remember where it went.
		oldKind := ri.OldKind
		if oldKind == "" {
			oldKind = e.Kind
		}
		from := ri.OldFrom
		if from == "" {
			from = e.From
		}
		filePath, line := ri.OldFilePath, ri.OldLine
		if filePath == "" {
			filePath = e.FilePath
		}
		if line == 0 {
			line = e.Line
		}
		before := graph.EdgeIdentity{From: from, To: ri.OldTo, Kind: oldKind, FilePath: filePath, Line: line}
		if before.To == "" {
			continue
		}
		after := graph.EdgeIdentityFor(e)
		if before == after {
			continue
		}
		if idx.retargeted == nil {
			idx.retargeted = make(map[graph.EdgeIdentity]graph.EdgeIdentity)
		}
		idx.retargeted[before] = after
	}
	for _, rp := range graph.PlaceholderSourceRepoints(reindexes) {
		site := placeholderSourceSite{from: rp.OldFrom, filePath: rp.FilePath, line: rp.Line}
		if len(idx.bySite[site]) != 0 {
			idx.deferred = append(idx.deferred, rp)
		}
	}
}

// currentIdentity follows the pass's re-targets of a census identity (an edge
// re-targeted twice resolves to its latest form).
func (idx *placeholderSourceIndex) currentIdentity(identity graph.EdgeIdentity) graph.EdgeIdentity {
	for hops := 0; hops <= len(idx.retargeted); hops++ {
		next, moved := idx.retargeted[identity]
		if !moved || next == identity {
			break
		}
		identity = next
	}
	return identity
}

// applyDeferredApplied applies the repoints deferRepoints recorded, in page
// order (first repoint of a site wins, as before), and clears them only once
// the store confirms every write landed. Every exit resets the pass census so
// a reused resolver (e.g. an incremental save after startup ResolveAll) never
// consults a stale or depleted bySite map.
func (idx *placeholderSourceIndex) applyDeferredApplied(g graph.Store) (int, bool) {
	if len(idx.deferred) == 0 {
		idx.beginPass()
		return 0, true
	}
	idx.ensure(g)
	if finder, ok := g.(graph.EdgeIdentityBatchFinder); ok {
		moves := idx.take(idx.deferred)
		for i := range moves {
			moves[i].identity = idx.currentIdentity(moves[i].identity)
		}
		moved, ok := reconcileIndexedPlaceholderSourcesApplied(g, finder, moves)
		if !ok {
			// Retain idx.deferred for retry and clear the pass census so the
			// next pass re-discovers surviving unresolved-from sites with their
			// current store identities (skipping any pages that already moved).
			idx.beginPass()
			return moved, false
		}
		idx.deferred = nil
		idx.beginPass()
		return moved, true
	}
	repoints := idx.filter(idx.deferred)
	if len(repoints) == 0 {
		idx.deferred = nil
		idx.beginPass()
		return 0, true
	}
	moved, ok := graph.ReconcilePlaceholderSourcesApplied(g, repoints)
	if !ok {
		idx.beginPass()
		return moved, false
	}
	idx.deferred = nil
	idx.beginPass()
	return moved, true
}

// reconcileIndexedPlaceholderSourcesApplied exact-refetches only claimed
// identities in bounded pages. Each backend closes its read cursor (or releases
// graph locks) before ReindexEdges runs, so the SQLite one-connection contract
// and in-memory mutation safety are both preserved.
func reconcileIndexedPlaceholderSourcesApplied(
	g graph.Store,
	finder graph.EdgeIdentityBatchFinder,
	moves []placeholderSourceMove,
) (int, bool) {
	moved := 0
	for start := 0; start < len(moves); start += placeholderSourceReindexBatchSize {
		end := start + placeholderSourceReindexBatchSize
		if end > len(moves) {
			end = len(moves)
		}
		page := moves[start:end]
		identities := make([]graph.EdgeIdentity, len(page))
		for i := range page {
			identities[i] = page[i].identity
		}
		current := finder.FindEdgesByIdentities(identities)
		batch := make([]graph.EdgeReindex, 0, len(page))
		for _, move := range page {
			e := current[move.identity]
			if e == nil || graph.EdgeIdentityFor(e) != move.identity {
				continue
			}
			e.From = move.newFrom
			batch = append(batch, graph.EdgeReindex{
				Edge:            e,
				OldFrom:         move.identity.From,
				OldTo:           move.identity.To,
				OldKind:         move.identity.Kind,
				OldFilePath:     move.identity.FilePath,
				OldLine:         move.identity.Line,
				RefreshIdentity: true,
			})
		}
		if len(batch) == 0 {
			continue
		}
		if !reindexEdgesApplied(g, batch) {
			return moved, false
		}
		moved += len(batch)
	}
	return moved, true
}

// reconcilePlaceholderSources re-points dataflow edges keyed FROM an
// unresolved placeholder once a resolution batch rewrites that placeholder's
// reference at the same site — see graph.ReconcilePlaceholderSources for the
// exact-site contract. idx amortizes the placeholder-source discovery to one
// stream per pass; a nil idx (incremental single-file / scoped passes) probes
// candidates directly — batches there are file-sized, so the prefilter's
// whole-graph stream would cost more than the probes it avoids.
// GORTEX_RESOLVE_FROM_RECONCILE=0 disables the reconciliation entirely.
func reconcilePlaceholderSources(g graph.Store, idx *placeholderSourceIndex, reindexes []graph.EdgeReindex) int {
	moved, _ := reconcilePlaceholderSourcesApplied(g, idx, reindexes)
	return moved
}

func reconcilePlaceholderSourcesApplied(g graph.Store, idx *placeholderSourceIndex, reindexes []graph.EdgeReindex) (int, bool) {
	if len(reindexes) == 0 || os.Getenv("GORTEX_RESOLVE_FROM_RECONCILE") == "0" {
		return 0, true
	}
	repoints := graph.PlaceholderSourceRepoints(reindexes)
	if len(repoints) == 0 {
		return 0, true
	}
	if idx != nil {
		idx.ensure(g)
		if finder, ok := g.(graph.EdgeIdentityBatchFinder); ok {
			moved, ok := reconcileIndexedPlaceholderSourcesApplied(g, finder, idx.take(repoints))
			if !ok {
				idx.beginPass()
			}
			return moved, ok
		}
		repoints = idx.filter(repoints)
		if len(repoints) == 0 {
			return 0, true
		}
	}
	moved, ok := graph.ReconcilePlaceholderSourcesApplied(g, repoints)
	if !ok && idx != nil {
		idx.beginPass()
	}
	return moved, ok
}

// RepointPlaceholderSourcesInBatch applies the from-side placeholder
// reconciliation to edges that are not in a store yet: the fresh extraction of
// a re-parsed file, whose reference edges a per-save path re-targets in memory
// from the prior resolution (the out-edge reuse) instead of through a
// resolution batch. Without it the re-used reference is bound while the
// dataflow edges the extractor keyed from its placeholder at the same site
// keep the placeholder source, where a whole index — which resolves the
// reference and then moves them — shows the bound source.
//
// resolved lists the reference edges the caller re-targeted, each with the
// unresolved target it had (EdgeReindex.OldTo). The site contract is
// graph.PlaceholderSourceRepoints': a dataflow edge moves only when its source
// is that placeholder (bare, or repository-prefixed) at the reference's exact
// file and line, and the first repoint of a site wins. Returns the number of
// edges re-pointed.
func RepointPlaceholderSourcesInBatch(edges []*graph.Edge, resolved []graph.EdgeReindex) int {
	repoints := graph.PlaceholderSourceRepoints(resolved)
	if len(repoints) == 0 || os.Getenv("GORTEX_RESOLVE_FROM_RECONCILE") == "0" {
		return 0
	}
	newFromBySite := make(map[placeholderSourceSite]string, len(repoints))
	for _, rp := range repoints {
		if rp.OldFrom == "" || rp.NewFrom == "" || rp.OldFrom == rp.NewFrom {
			continue
		}
		site := placeholderSourceSite{from: rp.OldFrom, filePath: rp.FilePath, line: rp.Line}
		if _, claimed := newFromBySite[site]; !claimed {
			newFromBySite[site] = rp.NewFrom
		}
	}
	moved := 0
	for _, e := range edges {
		if e == nil || !graph.PlaceholderSourceKind(e.Kind) || !strings.Contains(e.From, graph.UnresolvedMarker) {
			continue
		}
		if newFrom, ok := newFromBySite[placeholderSourceSite{from: e.From, filePath: e.FilePath, line: e.Line}]; ok {
			e.From = newFrom
			moved++
		}
	}
	return moved
}
