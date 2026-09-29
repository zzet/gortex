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

// filter keeps only repoints whose exact source site exists in the dataflow
// placeholder census. It is used only by stores without exact identity lookup.
func (idx *placeholderSourceIndex) filter(repoints []graph.PlaceholderRepoint) []graph.PlaceholderRepoint {
	if len(idx.bySite) == 0 {
		return nil
	}
	kept := repoints[:0]
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

// applyDeferred applies the repoints deferRepoints recorded, in page order
// (first repoint of a site wins, as before), and forgets them.
func (idx *placeholderSourceIndex) applyDeferred(g graph.Store) int {
	repoints := idx.deferred
	idx.deferred = nil
	if len(repoints) == 0 {
		idx.retargeted = nil
		return 0
	}
	defer func() { idx.retargeted = nil }()
	if finder, ok := g.(graph.EdgeIdentityBatchFinder); ok {
		moves := idx.take(repoints)
		for i := range moves {
			moves[i].identity = idx.currentIdentity(moves[i].identity)
		}
		return reconcileIndexedPlaceholderSources(g, finder, moves)
	}
	repoints = idx.filter(repoints)
	if len(repoints) == 0 {
		return 0
	}
	return graph.ReconcilePlaceholderSources(g, repoints)
}

// reconcileIndexedPlaceholderSources exact-refetches only claimed identities
// in bounded pages. Each backend closes its read cursor (or releases graph
// locks) before ReindexEdges runs, so the SQLite one-connection contract and
// in-memory mutation safety are both preserved.
func reconcileIndexedPlaceholderSources(
	g graph.Store,
	finder graph.EdgeIdentityBatchFinder,
	moves []placeholderSourceMove,
) int {
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
		g.ReindexEdges(batch)
		moved += len(batch)
	}
	return moved
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
	if len(reindexes) == 0 || os.Getenv("GORTEX_RESOLVE_FROM_RECONCILE") == "0" {
		return 0
	}
	repoints := graph.PlaceholderSourceRepoints(reindexes)
	if len(repoints) == 0 {
		return 0
	}
	if idx != nil {
		idx.ensure(g)
		if finder, ok := g.(graph.EdgeIdentityBatchFinder); ok {
			return reconcileIndexedPlaceholderSources(g, finder, idx.take(repoints))
		}
		repoints = idx.filter(repoints)
		if len(repoints) == 0 {
			return 0
		}
	}
	return graph.ReconcilePlaceholderSources(g, repoints)
}
