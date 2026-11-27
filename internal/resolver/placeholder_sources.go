package resolver

import (
	"os"
	"slices"
	"strings"
	"time"

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
	// (applyDeferredForScopeApplied). retargeted maps the census identity of a
	// placeholder-sourced dataflow edge the pass itself re-targeted to its
	// current identity, so the deferred move finds it.
	deferred       []graph.PlaceholderRepoint
	retargeted     map[graph.EdgeIdentity]graph.EdgeIdentity
	needsDedup     bool
	guardPruned    bool // fail-safe: false means pruneSitesMulti must run against pendingGuardReverts
	hasUniformRepo bool
	uniformRepo    string
	headDropped    int
}

type placeholderSourceSite struct {
	from     string
	filePath string
	line     int
}

type placeholderRepointKey struct {
	oldFrom  string
	filePath string
	line     int
	newFrom  string
}

type placeholderSourceMove struct {
	identity graph.EdgeIdentity
	newFrom  string
}

const (
	placeholderSourceReindexBatchSize = 256
	// directProbeRepointThreshold caps when ResolveAllContext's start-of-pass
	// flush prefers direct probes over building the whole-graph census.
	directProbeRepointThreshold = placeholderSourceReindexBatchSize
	// incrementalFlushRepointBudget bounds non-frontier same-repo repoints
	// drained opportunistically on an incremental save (~32 multi-repo sites,
	// since multi-repo emits both bare and repo-prefixed placeholder forms).
	incrementalFlushRepointBudget = 64
	// opportunisticDrainWriteTimeout bounds Step 3's wait on s.writeMu /
	// SQLite busy retries so opportunistic backlog draining never stalls an
	// interactive save for the full 15s mandatory busy window.
	opportunisticDrainWriteTimeout = 50 * time.Millisecond
)

// unresolvedBareSymbolName extracts the bare symbol name from any unresolved
// placeholder shape: bare `unresolved::Foo`, wildcard `unresolved::*.Foo`,
// repo-colon `repo::unresolved::Foo`, and dataflow slash-prefixed
// `repo/unresolved::*.Foo`.
func unresolvedBareSymbolName(id string) string {
	_, after, ok := strings.Cut(id, graph.UnresolvedMarker)
	if !ok {
		return ""
	}
	return strings.TrimPrefix(after, "*.")
}

// DedupPlaceholderRepoints collapses duplicate repoints sharing the same
// 4-tuple (OldFrom, FilePath, Line, NewFrom), preserving first-seen order and
// preferring a non-empty RepoPrefix when folding duplicates.
func DedupPlaceholderRepoints(repoints []graph.PlaceholderRepoint) []graph.PlaceholderRepoint {
	if len(repoints) <= 1 {
		return repoints
	}
	seen := make(map[placeholderRepointKey]int, len(repoints))
	out := make([]graph.PlaceholderRepoint, 0, len(repoints))
	changed := false
	for _, rp := range repoints {
		key := placeholderRepointKey{
			oldFrom:  rp.OldFrom,
			filePath: rp.FilePath,
			line:     rp.Line,
			newFrom:  rp.NewFrom,
		}
		if pos, dup := seen[key]; dup {
			changed = true
			if out[pos].RepoPrefix == "" && rp.RepoPrefix != "" {
				out[pos].RepoPrefix = rp.RepoPrefix
			}
			continue
		}
		seen[key] = len(out)
		out = append(out, rp)
	}
	if !changed {
		return repoints
	}
	return out
}

// repointsFromReindexes extracts eligible placeholder repoints from reindexes
// and stamps each repoint's RepoPrefix from an in-memory caller lookup without
// firing store point queries.
func repointsFromReindexes(reindexes []graph.EdgeReindex, repoForEdge func(*graph.Edge) string) []graph.PlaceholderRepoint {
	var out []graph.PlaceholderRepoint
	for _, ri := range reindexes {
		if ri.Edge == nil || ri.OldTo == "" {
			continue
		}
		if !graph.IsUnresolvedTarget(ri.OldTo) || graph.IsUnresolvedTarget(ri.Edge.To) || ri.Edge.To == "" {
			continue
		}
		repo := ""
		if repoForEdge != nil {
			repo = repoForEdge(ri.Edge)
		}
		out = append(out, graph.PlaceholderRepoint{
			OldFrom:    ri.OldTo,
			FilePath:   ri.Edge.FilePath,
			Line:       ri.Edge.Line,
			NewFrom:    ri.Edge.To,
			RepoPrefix: repo,
		})
		if prefix := graph.RepoPrefixOfID(ri.Edge.From); prefix != "" && strings.HasPrefix(ri.OldTo, graph.UnresolvedMarker) {
			out = append(out, graph.PlaceholderRepoint{
				OldFrom:    prefix + "/" + ri.OldTo,
				FilePath:   ri.Edge.FilePath,
				Line:       ri.Edge.Line,
				NewFrom:    ri.Edge.To,
				RepoPrefix: repo,
			})
		}
	}
	return out
}

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

func (idx *placeholderSourceIndex) resetDeferredEmpty(keepGuardPruned bool) {
	idx.deferred = nil
	idx.headDropped = 0
	idx.needsDedup = false
	idx.hasUniformRepo = false
	idx.uniformRepo = ""
	if !keepGuardPruned {
		idx.guardPruned = false
	}
}

func (idx *placeholderSourceIndex) hasDeferred() bool {
	return len(idx.deferred) > 0
}

func (idx *placeholderSourceIndex) retainRepoints(repoints []graph.PlaceholderRepoint) {
	if len(repoints) == 0 {
		return
	}
	old := idx.deferred
	idx.deferred = append(idx.deferred, repoints...)
	idx.deferred = DedupPlaceholderRepoints(idx.deferred)
	if len(old) == 0 || len(idx.deferred) == 0 || &idx.deferred[0] != &old[0] {
		idx.headDropped = 0
	}
	idx.needsDedup = true
	idx.guardPruned = false
	idx.hasUniformRepo = false
	idx.uniformRepo = ""
	idx.beginPass()
}

type deferredRepointSnapshot struct {
	repoints       []graph.PlaceholderRepoint
	needsDedup     bool
	guardPruned    bool
	hasUniformRepo bool
	uniformRepo    string
	headDropped    int
}

func (idx *placeholderSourceIndex) takeDeferredSnapshot() deferredRepointSnapshot {
	snap := deferredRepointSnapshot{
		repoints:       idx.deferred,
		needsDedup:     idx.needsDedup,
		guardPruned:    idx.guardPruned,
		hasUniformRepo: idx.hasUniformRepo,
		uniformRepo:    idx.uniformRepo,
		headDropped:    idx.headDropped,
	}
	idx.resetDeferredEmpty(false)
	return snap
}

func (idx *placeholderSourceIndex) takeDeferred() []graph.PlaceholderRepoint {
	return idx.takeDeferredSnapshot().repoints
}

func (idx *placeholderSourceIndex) adoptOwnedDeferred(
	repoints []graph.PlaceholderRepoint,
	needsDedup bool,
	guardPruned bool,
	uniformRepo string,
	hasUniformRepo bool,
	headDropped int,
) {
	idx.beginPass()
	if len(idx.deferred) == 0 {
		if len(repoints) == 0 {
			idx.resetDeferredEmpty(false)
			idx.guardPruned = guardPruned
			return
		}
		idx.deferred = repoints
		idx.needsDedup = needsDedup
		idx.guardPruned = guardPruned
		idx.hasUniformRepo = hasUniformRepo
		idx.uniformRepo = uniformRepo
		idx.headDropped = headDropped
		return
	}
	if len(repoints) == 0 {
		return
	}
	old := idx.deferred
	idx.deferred = append(idx.deferred, repoints...)
	if len(old) == 0 || &idx.deferred[0] != &old[0] {
		idx.headDropped = 0
	}
	idx.needsDedup = true
	idx.guardPruned = false
	idx.hasUniformRepo = false
	idx.uniformRepo = ""
}

func (idx *placeholderSourceIndex) inheritDeferred(repoints []graph.PlaceholderRepoint) {
	if len(repoints) == 0 {
		return
	}
	old := idx.deferred
	idx.deferred = append(idx.deferred, repoints...)
	if len(old) == 0 || &idx.deferred[0] != &old[0] {
		idx.headDropped = 0
	}
	idx.needsDedup = true
	idx.guardPruned = false
	idx.hasUniformRepo = false
	idx.uniformRepo = ""
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
	clear(idx.deferred[len(kept):])
	if len(kept) == 0 {
		idx.resetDeferredEmpty(true)
		return
	}
	idx.deferred = kept
}

// pruneSitesMulti scans idx.deferred once against all queued PendingGuardRevert
// SourceReverts and marks idx.guardPruned = true so subsequent incremental
// saves skip repeated O(G + R) scans when no new repoints or reverts arrived.
func (idx *placeholderSourceIndex) pruneSitesMulti(reverts []PendingGuardRevert) {
	total := 0
	for i := range reverts {
		total += len(reverts[i].SourceReverts)
	}
	if len(idx.deferred) == 0 || total == 0 {
		idx.guardPruned = true
		return
	}
	rejected := make(map[placeholderSourceSite]struct{}, total)
	for i := range reverts {
		for _, rev := range reverts[i].SourceReverts {
			rejected[placeholderSourceSite{from: rev.OldFrom, filePath: rev.FilePath, line: rev.Line}] = struct{}{}
		}
	}
	kept := idx.deferred[:0]
	for _, rp := range idx.deferred {
		if _, drop := rejected[placeholderSourceSite{from: rp.NewFrom, filePath: rp.FilePath, line: rp.Line}]; drop {
			continue
		}
		kept = append(kept, rp)
	}
	clear(idx.deferred[len(kept):])
	if len(kept) == 0 {
		idx.resetDeferredEmpty(false)
		idx.guardPruned = true
		return
	}
	idx.deferred = kept
	idx.guardPruned = true
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

// deferRepoints records one page's repoints for the end of a ResolveAll pass
// instead of applying them now.
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
func (idx *placeholderSourceIndex) deferRepoints(g graph.Store, reindexes []graph.EdgeReindex, repoForEdge func(*graph.Edge) string) {
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
	if len(idx.bySite) == 0 {
		return
	}
	for _, ri := range reindexes {
		e := ri.Edge
		if e == nil || ri.OldTo == "" {
			continue
		}
		if !graph.IsUnresolvedTarget(ri.OldTo) || graph.IsUnresolvedTarget(e.To) || e.To == "" {
			continue
		}
		bareSite := placeholderSourceSite{from: ri.OldTo, filePath: e.FilePath, line: e.Line}
		bareHit := len(idx.bySite[bareSite]) != 0
		var prefixedOld string
		prefixedHit := false
		if prefix := graph.RepoPrefixOfID(e.From); prefix != "" && strings.HasPrefix(ri.OldTo, graph.UnresolvedMarker) {
			prefixedOld = prefix + "/" + ri.OldTo
			prefixedHit = len(idx.bySite[placeholderSourceSite{from: prefixedOld, filePath: e.FilePath, line: e.Line}]) != 0
		}
		if !bareHit && !prefixedHit {
			continue
		}
		repo := ""
		if repoForEdge != nil {
			repo = repoForEdge(e)
		}
		if bareHit {
			idx.deferred = append(idx.deferred, graph.PlaceholderRepoint{
				OldFrom:    ri.OldTo,
				FilePath:   e.FilePath,
				Line:       e.Line,
				NewFrom:    e.To,
				RepoPrefix: repo,
			})
		}
		if prefixedHit {
			idx.deferred = append(idx.deferred, graph.PlaceholderRepoint{
				OldFrom:    prefixedOld,
				FilePath:   e.FilePath,
				Line:       e.Line,
				NewFrom:    e.To,
				RepoPrefix: repo,
			})
		}
		idx.guardPruned = false
		idx.hasUniformRepo = false
		idx.uniformRepo = ""
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

func applyRepointsDirectApplied(
	g graph.Store,
	repoints []graph.PlaceholderRepoint,
	maxWait time.Duration,
) (moved int, remaining []graph.PlaceholderRepoint, ok bool) {
	if len(repoints) == 0 || os.Getenv("GORTEX_RESOLVE_FROM_RECONCILE") == "0" {
		return 0, nil, true
	}
	for start := 0; start < len(repoints); start += placeholderSourceReindexBatchSize {
		end := min(start+placeholderSourceReindexBatchSize, len(repoints))
		n, applied := graph.ReconcilePlaceholderSourcesTryApplied(g, repoints[start:end], maxWait)
		moved += n
		if !applied {
			if start == 0 {
				return moved, repoints, false
			}
			return moved, append([]graph.PlaceholderRepoint(nil), repoints[start:]...), false
		}
	}
	return moved, nil, true
}

// flushMatchingDirectApplied flushes only deferred repoints matching the
// caller's frontier using direct exact-site probes (never calling idx.ensure).
// When zero deferred repoints match, it returns (0, true) with zero allocations.
func (idx *placeholderSourceIndex) flushMatchingDirectApplied(
	g graph.Store,
	match func(graph.PlaceholderRepoint) bool,
	maxWait time.Duration,
) (int, bool) {
	if len(idx.deferred) == 0 {
		return 0, true
	}
	matchedCount := 0
	for _, rp := range idx.deferred {
		if match == nil || match(rp) {
			matchedCount++
		}
	}
	if matchedCount == 0 {
		return 0, true
	}
	if matchedCount == len(idx.deferred) {
		moved, remaining, ok := applyRepointsDirectApplied(g, idx.deferred, maxWait)
		if !ok {
			if len(remaining) != len(idx.deferred) {
				idx.deferred = remaining
				idx.headDropped = 0
				idx.hasUniformRepo = false
				idx.uniformRepo = ""
			}
			return moved, false
		}
		idx.resetDeferredEmpty(true)
		return moved, true
	}
	matched := make([]graph.PlaceholderRepoint, 0, matchedCount)
	kept := make([]graph.PlaceholderRepoint, 0, len(idx.deferred)-matchedCount)
	for _, rp := range idx.deferred {
		if match(rp) {
			matched = append(matched, rp)
		} else {
			kept = append(kept, rp)
		}
	}
	moved, remaining, ok := applyRepointsDirectApplied(g, matched, maxWait)
	if !ok {
		idx.deferred = append(remaining, kept...)
		idx.headDropped = 0
		idx.hasUniformRepo = false
		idx.uniformRepo = ""
		return moved, false
	}
	if len(kept) == 0 {
		idx.resetDeferredEmpty(true)
		return moved, true
	}
	idx.deferred = kept
	idx.headDropped = 0
	return moved, true
}

// flushBoundedDirectApplied opportunistically drains up to maxItems deferred
// repoints allowed by allowedRepo using direct probes. The bridged single-repo
// fast path pops from the head in O(1) amortized copies without disturbing the
// tail on contention refusal.
func (idx *placeholderSourceIndex) flushBoundedDirectApplied(
	g graph.Store,
	maxItems int,
	allowedRepo func(graph.PlaceholderRepoint) bool,
	maxWait time.Duration,
) (int, bool) {
	if len(idx.deferred) == 0 || maxItems <= 0 {
		return 0, true
	}
	take := min(maxItems, len(idx.deferred))
	headUniform := true
	if allowedRepo != nil {
		for _, rp := range idx.deferred[:take] {
			if !allowedRepo(rp) {
				headUniform = false
				break
			}
		}
	}
	if headUniform {
		moved, remaining, ok := applyRepointsDirectApplied(g, idx.deferred[:take], maxWait)
		if !ok {
			if len(remaining) == take {
				// Chunk-0 refusal: zero allocations, leave queue untouched.
				return moved, false
			}
			consumed := take - len(remaining)
			clear(idx.deferred[:consumed])
			idx.deferred = idx.deferred[consumed:]
			idx.headDropped += consumed
			if idx.headDropped >= 256 && idx.headDropped >= len(idx.deferred) {
				idx.deferred = slices.Clone(idx.deferred)
				idx.headDropped = 0
			}
			return moved, false
		}
		clear(idx.deferred[:take])
		idx.deferred = idx.deferred[take:]
		idx.headDropped += take
		if len(idx.deferred) == 0 {
			idx.resetDeferredEmpty(true)
			return moved, true
		}
		if idx.headDropped >= 256 && idx.headDropped >= len(idx.deferred) {
			idx.deferred = slices.Clone(idx.deferred)
			idx.headDropped = 0
		}
		return moved, true
	}

	selectedCount := 0
	for _, rp := range idx.deferred {
		if allowedRepo(rp) {
			selectedCount++
			if selectedCount == maxItems {
				break
			}
		}
	}
	if selectedCount == 0 {
		return 0, true
	}
	selected := make([]graph.PlaceholderRepoint, 0, selectedCount)
	kept := make([]graph.PlaceholderRepoint, 0, len(idx.deferred)-selectedCount)
	for _, rp := range idx.deferred {
		if len(selected) < maxItems && allowedRepo(rp) {
			selected = append(selected, rp)
		} else {
			kept = append(kept, rp)
		}
	}
	moved, remaining, ok := applyRepointsDirectApplied(g, selected, maxWait)
	if !ok {
		idx.deferred = append(remaining, kept...)
		idx.headDropped = 0
		idx.hasUniformRepo = false
		idx.uniformRepo = ""
		return moved, false
	}
	if len(kept) == 0 {
		idx.resetDeferredEmpty(true)
		return moved, true
	}
	idx.deferred = kept
	idx.headDropped = 0
	idx.hasUniformRepo = false
	idx.uniformRepo = ""
	return moved, true
}

// applyDeferredApplied flushes all deferred repoints across all repositories.
func (idx *placeholderSourceIndex) applyDeferredApplied(g graph.Store) (int, bool) {
	return idx.applyDeferredForScopeApplied(g, nil)
}

// applyDeferredForScopeApplied applies deferred repoints matching allowedRepo
// (all when nil), preferring direct exact-site probes for small queues when no
// whole-graph census has been built yet, and clearing them only once the store
// confirms every write landed. Every exit resets the pass census so a reused
// resolver never consults a stale or depleted bySite map.
func (idx *placeholderSourceIndex) applyDeferredForScopeApplied(
	g graph.Store,
	allowedRepo func(graph.PlaceholderRepoint) bool,
) (int, bool) {
	if len(idx.deferred) == 0 {
		idx.beginPass()
		return 0, true
	}
	matched := idx.deferred
	var kept []graph.PlaceholderRepoint
	if allowedRepo != nil {
		matchedCount := 0
		for _, rp := range idx.deferred {
			if allowedRepo(rp) {
				matchedCount++
			}
		}
		if matchedCount == 0 {
			idx.beginPass()
			return 0, true
		}
		if matchedCount < len(idx.deferred) {
			matched = make([]graph.PlaceholderRepoint, 0, matchedCount)
			kept = make([]graph.PlaceholderRepoint, 0, len(idx.deferred)-matchedCount)
			for _, rp := range idx.deferred {
				if allowedRepo(rp) {
					matched = append(matched, rp)
				} else {
					kept = append(kept, rp)
				}
			}
		}
	}

	if !idx.built && len(idx.retargeted) == 0 && len(matched) <= directProbeRepointThreshold {
		moved, remaining, ok := applyRepointsDirectApplied(g, matched, 0)
		if !ok {
			if len(kept) > 0 || len(remaining) != len(matched) {
				idx.deferred = append(remaining, kept...)
				idx.headDropped = 0
				idx.hasUniformRepo = false
				idx.uniformRepo = ""
			}
			idx.beginPass()
			return moved, false
		}
		if len(kept) == 0 {
			idx.resetDeferredEmpty(true)
		} else {
			idx.deferred = kept
			idx.headDropped = 0
			idx.hasUniformRepo = false
			idx.uniformRepo = ""
		}
		idx.beginPass()
		return moved, true
	}

	idx.ensure(g)
	if finder, ok := g.(graph.EdgeIdentityBatchFinder); ok {
		moves := idx.take(matched)
		for i := range moves {
			moves[i].identity = idx.currentIdentity(moves[i].identity)
		}
		moved, ok := reconcileIndexedPlaceholderSourcesApplied(g, finder, moves, 0)
		if !ok {
			// Retain idx.deferred for retry and clear the pass census so the
			// next pass re-discovers surviving unresolved-from sites with their
			// current store identities (skipping any pages that already moved).
			idx.beginPass()
			return moved, false
		}
		if len(kept) == 0 {
			idx.resetDeferredEmpty(true)
		} else {
			idx.deferred = kept
			idx.headDropped = 0
			idx.hasUniformRepo = false
			idx.uniformRepo = ""
		}
		idx.beginPass()
		return moved, true
	}
	repoints := idx.filter(matched)
	if len(repoints) == 0 {
		if len(kept) == 0 {
			idx.resetDeferredEmpty(true)
		} else {
			idx.deferred = kept
			idx.headDropped = 0
			idx.hasUniformRepo = false
			idx.uniformRepo = ""
		}
		idx.beginPass()
		return 0, true
	}
	moved, ok := graph.ReconcilePlaceholderSourcesApplied(g, repoints)
	if !ok {
		idx.beginPass()
		return moved, false
	}
	if len(kept) == 0 {
		idx.resetDeferredEmpty(true)
	} else {
		idx.deferred = kept
		idx.headDropped = 0
		idx.hasUniformRepo = false
		idx.uniformRepo = ""
	}
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
	maxWait time.Duration,
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
		clones := make([]graph.Edge, len(page))
		batch := make([]graph.EdgeReindex, 0, len(page))
		for _, move := range page {
			e := current[move.identity]
			if e == nil || graph.EdgeIdentityFor(e) != move.identity {
				continue
			}
			delete(current, move.identity)
			n := len(batch)
			clones[n] = *e
			clones[n].From = move.newFrom
			batch = append(batch, graph.EdgeReindex{
				Edge:            &clones[n],
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
		if !reindexEdgesAppliedWithWindow(g, batch, maxWait) {
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
	return reconcilePlaceholderSourcesWithWindow(g, idx, reindexes, 0)
}

func reconcilePlaceholderSourcesWithWindow(g graph.Store, idx *placeholderSourceIndex, reindexes []graph.EdgeReindex, maxWait time.Duration) (int, bool) {
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
			moved, ok := reconcileIndexedPlaceholderSourcesApplied(g, finder, idx.take(repoints), maxWait)
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
	moved, ok := graph.ReconcilePlaceholderSourcesTryApplied(g, repoints, maxWait)
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
