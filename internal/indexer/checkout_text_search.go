package indexer

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"sort"

	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/search/trigram"
	"github.com/zzet/gortex/internal/viewmetrics"
)

// Text search over a routed checkout.
//
// The per-repository trigram searchers are built over the canonical checkout's
// root, so they answer about the bytes the primary working copy holds. A
// routed automatic checkout is a different working copy on a different branch
// with its own uncommitted edits, and searching the canonical root for it
// would return lines that exist nowhere in the tree the caller is reading.
//
// So each coordinator carries one searcher of its own, over its checkout root.
// It is built on first use rather than on every cycle — a checkout nobody
// searches never pays for an index — and it is keyed by the working tree and
// route it was built for, so the next search after an edit brings it up to
// date instead of answering out of stale postings. Bringing it up to date is a
// per-path patch driven by the working-tree chain's input manifest; a rebuild
// over the whole corpus is reserved for the cases with nothing to diff.
//
// What that means for the capability a generation declares is the other half of
// this file's contract, and it lives at textSearchProducer in
// builder_generation.go: the bytes below are the working copy's, so only the
// layer that IS the working copy claims graphview.CapSearchText, and the layers
// under it say nothing rather than narrowing a stack they do not answer for.
//
// The claim a generation cannot make is made here instead. Whether an answer
// off this root describes the view a caller is reading is a property of the
// checkout's ROUTE, not of any one generation in its stack — the same commit
// layer is served both with a working-tree layer over it and, for the window a
// commit slot is being moved, alone. GrepCheckout reads the route and refuses
// the second case.

// CheckoutTextQuery is one text search over a routed checkout's working tree.
type CheckoutTextQuery struct {
	// CheckoutID names the routed checkout whose working copy is searched.
	CheckoutID string
	// Query is the literal to find, or the source of the mandatory literals a
	// regexp search pre-filters candidate files with.
	Query string
	// Regexp, when set, verifies each candidate line against this expression
	// instead of testing it for the literal. The caller compiles it, so a
	// pattern that does not compile is reported as the caller's own error
	// rather than as a failure to search.
	Regexp *regexp.Regexp
	// Limit bounds the matches returned; a non-positive limit returns every
	// match.
	Limit int
}

// GrepCheckout runs a text search over one routed checkout's working tree and
// reports whether anything served it.
//
// served is false in two cases, and neither is a reason to look elsewhere:
// nothing else in the daemon indexes that working copy, and the canonical
// checkout's bytes describe a different tree.
//
//   - No coordinator holds the checkout, so no searcher exists for it.
//   - The checkout is routed, and its route names no working-tree layer. The
//     view a caller reads in that window is the committed tree alone, and this
//     root is free to hold edits that tree does not contain; answering would
//     return lines the view does not have.
//   - The checkout has no route at all, so nothing has published a view of it
//     and no layer vouches for what is on the root.
func (l *CheckoutLifecycle) GrepCheckout(ctx context.Context, q CheckoutTextQuery) ([]trigram.Match, bool, error) {
	if l == nil || q.CheckoutID == "" || q.Query == "" {
		return nil, false, nil
	}
	l.coordMu.Lock()
	coordinator := l.coordinators[q.CheckoutID]
	l.coordMu.Unlock()
	if coordinator == nil {
		return nil, false, nil
	}
	describes, err := coordinator.routeDescribesTheWorkingCopy(ctx)
	if err != nil {
		return nil, true, err
	}
	if !describes {
		return nil, false, nil
	}
	searcher, err := coordinator.textSearcher(ctx)
	if err != nil {
		return nil, true, err
	}
	if q.Regexp != nil {
		return searcher.GrepRegexp(q.Regexp, extractRegexLiterals(q.Query), "", q.Limit), true, nil
	}
	return searcher.Grep(q.Query, q.Limit), true, nil
}

// routeDescribesTheWorkingCopy reports whether some routed layer of this
// checkout describes the bytes on its root.
//
// The working-tree slot is that layer. A coordinator withdraws it whenever the
// commit slot moves under it (checkout_coordinator.go, moveCommitSlot and
// clearDirtySlot both flip the route to RoutePending with
// DirtyGenerationID = 0), and a view materialized in that window is the commit
// layer alone: graphview's MaterializeCheckout appends the dirty generation
// only when the route names one, and it serves a pending route. Searching this
// root for that view would answer with the working copy's uncommitted lines,
// which the committed tree the caller is reading does not contain — so nothing
// serves it, and the caller is told that rather than shown them.
//
// A checkout with no route at all is refused too, and this arm is the reason
// the gate is a predicate on the route rather than on the generation: an
// unrouted checkout has published nothing, so there is no evidence that any
// layer describes what is on the root. The only production caller reaches
// this through a materialized view, which cannot exist without a route
// (graphview's Materializer.route refuses an unrouted checkout with
// CodeCheckoutInaccessible), so no live request loses an answer here — and a
// future caller that arrives without one gets a refusal rather than a raw
// working-copy answer that no view vouches for.
func (c *CheckoutCoordinator) routeDescribesTheWorkingCopy(ctx context.Context) (bool, error) {
	route, found, err := c.catalog.GetCheckoutRoute(ctx, c.checkoutID)
	if err != nil {
		return false, fmt.Errorf("indexer: read the route of checkout %q: %w", c.checkoutID, err)
	}
	if !found {
		return false, nil
	}
	return route.DirtyGenerationID > 0, nil
}

// Reasons the checkout searcher is rebuilt over its whole corpus instead of
// patched path by path. Every other key change is a patch.
const (
	// textRebuildFirstUse: the coordinator has no searcher yet.
	textRebuildFirstUse = "first_use"
	// textRebuildNoRoute: the checkout has no route, or its route names no
	// working-tree layer, so there is no manifest to diff against.
	textRebuildNoRoute = "no_route"
	// textRebuildCommitMoved: the route's commit generation is not the one
	// the searcher was built over. Committed paths are not in any
	// working-tree manifest, so nothing short of a rebuild says which of
	// them moved.
	textRebuildCommitMoved = "commit_moved"
	// textRebuildNoManifest: the routed working-tree chain, or the one the
	// searcher was built over, has no resolvable input manifest.
	textRebuildNoManifest = "no_manifest"
)

// textRouteAttempts bounds how often textSearcher re-reads a route that moved
// while its generations were being pinned, the materializer's own retry.
const textRouteAttempts = 4

// checkoutTextState is what the checkout's searcher was last built or patched
// to describe. It lives under textMu with the searcher itself.
type checkoutTextState struct {
	// routed records that the searcher was composed over a route; commit
	// and dirty are that route's generations.
	routed bool
	commit int64
	dirty  int64
	// corpus is the repo-relative path set the searcher indexes.
	corpus textCorpusSet
	// commitSide caches the commit side of the corpus — the base inventory
	// with the commit generation's claims folded in — for commitSideFor,
	// the commit generation it was composed over. It is reused for as long
	// as the route names that commit generation.
	commitSide    map[string]struct{}
	commitSideFor int64
	// manifest is the resolved input manifest of the routed working-tree
	// chain the searcher describes; hasManifest is false when there was
	// none to resolve, which forces the next change to a rebuild.
	manifest    map[string]store_sqlite.InputManifestEntry
	hasManifest bool

	// claims caches the file claims of the generations in the routed stack
	// (cachedLayerClaims).
	claims map[int64][]textClaim

	stats checkoutTextStats

	// skipPatch is a test seam: a path it reports is left out of a patch,
	// which is how a test proves the delta-equals-rebuild check can fail.
	skipPatch func(rel string) bool
}

// checkoutTextStats counts what the checkout searcher has done since the
// coordinator started: full rebuilds by reason, and patches with the paths
// each touched.
type checkoutTextStats struct {
	Rebuilds     map[string]int
	Patches      int
	PathsUpdated int
	PathsRemoved int
	// Last describes the most recent key change: the rebuild reason, or
	// "patch"; LastPaths is the corpus size for a rebuild and the number
	// of paths a patch touched.
	Last      string
	LastPaths int
}

// textSearchStats returns a copy of the searcher's counters.
func (c *CheckoutCoordinator) textSearchStats() checkoutTextStats {
	c.textMu.Lock()
	defer c.textMu.Unlock()
	out := c.textState.stats
	out.Rebuilds = make(map[string]int, len(c.textState.stats.Rebuilds))
	for reason, n := range c.textState.stats.Rebuilds {
		out.Rebuilds[reason] = n
	}
	return out
}

// textSearcher returns the checkout's trigram searcher, bringing it up to the
// checkout's current working tree and route first.
//
// The searcher is keyed by the last sampled working-tree fingerprint and the
// route's commit and working-tree generations. On a key change it is patched,
// not rebuilt: the routed chain's resolved input manifest is diffed against the
// manifest the searcher was last brought to, and every path whose admitted
// input changed — plus every path that entered or left the corpus — is
// re-read (trigram.Searcher.Update) or dropped (Remove). A rebuild over the
// whole corpus happens only when there is nothing to diff: the first use, a
// route with no working-tree layer, a commit generation that moved, or a
// chain without a resolvable manifest (textRebuild*).
//
// The route is part of the key because the cycle records a new fingerprint
// before it builds: a search in between keeps the published state it has, and
// the publication that follows changes the key and brings the patch.
//
// The lock is held across the update on purpose, the way the per-repository
// searcher does it: a burst of concurrent searches on one checkout collapses
// into a single build instead of several racing ones, each paying full corpus
// memory. A search already running against the searcher when a patch lands
// sees each file either before or after it; every candidate line is verified
// against the bytes on disk, as it always was.
func (c *CheckoutCoordinator) textSearcher(ctx context.Context) (*trigram.Searcher, error) {
	fingerprint := c.dirtyKey()
	route, routed, lease, err := c.pinTextRoute(ctx)
	if err != nil {
		return nil, err
	}
	defer lease.Release()
	key := fingerprint
	if routed {
		key = fmt.Sprintf("%s|%d|%d", fingerprint, route.CommitGenerationID, route.DirtyGenerationID)
	}

	c.textMu.Lock()
	defer c.textMu.Unlock()
	if c.textIndex != nil && c.textKey == key {
		return c.textIndex, nil
	}
	if c.textIndex != nil {
		// A cached index keyed to a tree that has moved: the working copy was
		// edited under it, which is the only thing that invalidates one.
		viewmetrics.Count(viewmetrics.CheckoutSearcherInvalidatedTotal)
	}

	chain := c.textChain(ctx, route, routed)
	corpus, err := c.textCorpus(ctx, route, routed, chain)
	if err != nil {
		return nil, err
	}
	manifest, hasManifest := c.textChainManifest(ctx, chain)

	state := &c.textState
	reason := ""
	switch {
	case c.textIndex == nil:
		reason = textRebuildFirstUse
	case !routed || route.DirtyGenerationID <= 0:
		reason = textRebuildNoRoute
	case !state.routed || state.commit != route.CommitGenerationID:
		reason = textRebuildCommitMoved
	case !hasManifest || !state.hasManifest:
		reason = textRebuildNoManifest
	}
	if state.stats.Rebuilds == nil {
		state.stats.Rebuilds = map[string]int{}
	}
	if reason != "" {
		paths := corpus.paths()
		c.textIndex = trigram.Build(c.root, paths)
		viewmetrics.Count(viewmetrics.CheckoutSearcherBuiltTotal)
		state.stats.Rebuilds[reason]++
		state.stats.Last, state.stats.LastPaths = reason, len(paths)
	} else {
		updated, removed := c.patchTextSearcher(state, corpus, manifest)
		state.stats.Patches++
		state.stats.PathsUpdated += updated
		state.stats.PathsRemoved += removed
		state.stats.Last, state.stats.LastPaths = "patch", updated+removed
	}
	state.routed = routed
	state.commit, state.dirty = route.CommitGenerationID, route.DirtyGenerationID
	state.corpus = corpus
	state.manifest, state.hasManifest = manifest, hasManifest
	c.textKey = key
	return c.textIndex, nil
}

// patchTextSearcher brings the cached searcher from state to (corpus,
// manifest): every path whose resolved manifest entry differs between the two,
// and every path that entered or left the corpus, is re-read when the new
// corpus holds it and dropped when it does not. It returns how many paths it
// re-read and dropped.
//
// It runs only when the commit generation has not moved (a moved one is a
// rebuild), so both corpora share one commit side and membership can differ
// only at a path some working-tree generation claims; and a path in neither
// manifest is in its committed state on both sides, so its bytes on disk are
// the ones the searcher already holds. The work is O(dirty paths), whatever
// the size of the checkout.
func (c *CheckoutCoordinator) patchTextSearcher(
	state *checkoutTextState,
	corpus textCorpusSet,
	manifest map[string]store_sqlite.InputManifestEntry,
) (updated, removed int) {
	changed := map[string]struct{}{}
	for p, next := range manifest {
		if prev, ok := state.manifest[p]; !ok || !manifestEntriesEqual(prev, next) {
			changed[p] = struct{}{}
		}
	}
	for p := range state.manifest {
		if _, ok := manifest[p]; !ok {
			changed[p] = struct{}{}
		}
	}
	for p := range corpus.dirty {
		if corpus.has(p) != state.corpus.has(p) {
			changed[p] = struct{}{}
		}
	}
	for p := range state.corpus.dirty {
		if corpus.has(p) != state.corpus.has(p) {
			changed[p] = struct{}{}
		}
	}
	for _, p := range sortedTextPaths(changed) {
		if state.skipPatch != nil && state.skipPatch(p) {
			continue
		}
		if corpus.has(p) {
			c.textIndex.Update(p)
			updated++
			continue
		}
		if state.corpus.has(p) {
			c.textIndex.Remove(p)
			removed++
		}
	}
	return updated, removed
}

// textCorpusSet is a checkout's text corpus as two halves: the commit side
// (the base inventory with the commit generation's claims) and the
// working-tree chain's claims over it, true for a path a generation puts in
// and false for one it takes out. The newest claim on a path wins.
type textCorpusSet struct {
	commit map[string]struct{}
	dirty  map[string]bool
}

// has reports whether the corpus holds rel.
func (s textCorpusSet) has(rel string) bool {
	if present, claimed := s.dirty[rel]; claimed {
		return present
	}
	_, held := s.commit[rel]
	return held
}

// paths lists the corpus in ascending order.
func (s textCorpusSet) paths() []string {
	out := make([]string, 0, len(s.commit)+len(s.dirty))
	for rel := range s.commit {
		if present, claimed := s.dirty[rel]; claimed && !present {
			continue
		}
		out = append(out, rel)
	}
	for rel, present := range s.dirty {
		if _, held := s.commit[rel]; present && !held {
			out = append(out, rel)
		}
	}
	sort.Strings(out)
	return out
}

// pinTextRoute reads the checkout's route and holds a lease on every
// generation the corpus is composed from — the commit generation and the
// routed working-tree chain — for as long as the caller reads them, the way
// the materializer pins a view: pin, then confirm the route did not move, and
// retry a bounded number of times when it did. An unrouted checkout pins
// nothing. The returned lease is never nil.
func (c *CheckoutCoordinator) pinTextRoute(ctx context.Context) (store_sqlite.CheckoutRoute, bool, *graphview.Lease, error) {
	for attempt := 0; ; attempt++ {
		route, found, err := c.catalog.GetCheckoutRoute(ctx, c.checkoutID)
		if err != nil {
			return store_sqlite.CheckoutRoute{}, false, nil, fmt.Errorf("indexer: read the route of checkout %q: %w", c.checkoutID, err)
		}
		if !found || c.leases == nil {
			return route, found, nil, nil
		}
		ids := []int64{}
		if route.CommitGenerationID > 0 {
			ids = append(ids, route.CommitGenerationID)
		}
		if route.DirtyGenerationID > 0 {
			ids = append(ids, c.dirtyChainMembers(ctx, route.DirtyGenerationID)...)
		}
		lease := c.leases.Acquire(ids...)
		current, found, err := c.catalog.GetCheckoutRoute(ctx, c.checkoutID)
		if err != nil {
			lease.Release()
			return store_sqlite.CheckoutRoute{}, false, nil, fmt.Errorf("indexer: read the route of checkout %q: %w", c.checkoutID, err)
		}
		if found && current == route {
			return route, true, lease, nil
		}
		lease.Release()
		if attempt+1 >= textRouteAttempts {
			return store_sqlite.CheckoutRoute{}, false, nil, fmt.Errorf("indexer: the route of checkout %q kept changing while its text corpus was read", c.checkoutID)
		}
	}
}

// textChain lists the routed working-tree chain oldest first: the generation
// that stands on the commit generation, then each child up to the routed top.
// A route with no working-tree layer has no chain.
func (c *CheckoutCoordinator) textChain(ctx context.Context, route store_sqlite.CheckoutRoute, routed bool) []int64 {
	if !routed || route.DirtyGenerationID <= 0 {
		return nil
	}
	chain := c.dirtyChainMembers(ctx, route.DirtyGenerationID)
	slices.Reverse(chain)
	return chain
}

// textChainManifest resolves the input manifests of a working-tree chain,
// oldest first, into the chain's dirty state (resolveChainLinks). It reports
// false when the chain is empty or any link's manifest is missing, of another
// version, or under another policy. Unlike loadDirtyChainManifest it does not
// refuse a chain whose closure was truncated: that decides whether a build may
// stack on the chain, not which bytes the working copy holds.
func (c *CheckoutCoordinator) textChainManifest(ctx context.Context, chain []int64) (map[string]store_sqlite.InputManifestEntry, bool) {
	if len(chain) == 0 {
		return nil, false
	}
	links := make([]chainManifestLink, 0, len(chain))
	for _, generationID := range chain {
		meta, entries, found, err := c.store.AtGeneration(generationID).InputManifest(ctx)
		if err != nil {
			return nil, false
		}
		links = append(links, chainManifestLink{Meta: meta, Entries: entries, OK: found})
	}
	resolved, reason := resolveChainLinks(links...)
	if reason != "" {
		return nil, false
	}
	return resolved.entries, true
}

// noteDirtyFingerprint records the working tree the cycle just sampled.
//
// It does not rebuild, and it does not drop what is cached: a rebuild costs a
// pass over the checkout and only a search needs one. The recorded fingerprint
// is the invalidation — the next search finds the cached searcher keyed to a
// tree that has moved and brings it up to the current one.
func (c *CheckoutCoordinator) noteDirtyFingerprint(fingerprint string) {
	c.mu.Lock()
	c.dirtyFingerprint = fingerprint
	c.mu.Unlock()
}

// dirtyKey is the working tree the searcher is keyed by: the fingerprint of
// the last sample a cycle took, which covers the checkout's HEAD commit and
// every path it differs from it by. A coordinator that has not run a cycle yet
// has no fingerprint, and the searcher it builds is re-keyed by the first one.
func (c *CheckoutCoordinator) dirtyKey() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.dirtyFingerprint
}

// releaseTextSearcher drops the built index. It runs when the coordinator
// stops, so the searcher lives exactly as long as the checkout it describes.
func (c *CheckoutCoordinator) releaseTextSearcher() {
	c.textMu.Lock()
	c.textIndex = nil
	c.textKey = ""
	stats, skip := c.textState.stats, c.textState.skipPatch
	c.textState = checkoutTextState{stats: stats, skipPatch: skip}
	c.textMu.Unlock()
}

// textCorpus is the repo-relative file set the checkout's working tree
// presents to a search: the base corpus for this repository with the routed
// layers' file claims applied over it, bottom layer first — the commit
// generation, then every generation of the working-tree chain (chain, oldest
// first) up to the routed top.
//
// It is the composition the view's reader performs, expressed as paths, and
// it follows the reader's visibility rules. A replace claim puts the path in —
// which is how a file the branch added, and which the base corpus has never
// seen, becomes searchable — and a delete claim takes it out, so a file the
// checkout removed cannot match through the base corpus's copy of it. A
// context claim hides nothing: the generation read the path for other files'
// sake and the layer beneath still speaks for it. Every chain member counts: a
// chained child claims only its own delta, so a file added lower in the chain
// and untouched since is claimed by the lower generation alone.
//
// The commit side is composed once per commit generation and cached on the
// searcher's state; the chain's claims are read per generation through the
// claim cache, so a new child costs one generation's file claims.
//
// A checkout with no route yet is searched over the base corpus's paths alone.
// The bytes still come from the checkout root, so the answer is about the right
// working copy; what it cannot yet see is a path only the layers know about.
// Callers hold textMu.
func (c *CheckoutCoordinator) textCorpus(ctx context.Context, route store_sqlite.CheckoutRoute, routed bool, chain []int64) (textCorpusSet, error) {
	state := &c.textState
	commit := state.commitSide
	if !routed || route.CommitGenerationID <= 0 || state.commitSideFor != route.CommitGenerationID || commit == nil {
		rows, err := c.store.AtGeneration(0).FileMetasForRepo(c.repoPrefix)
		if err != nil {
			return textCorpusSet{}, fmt.Errorf("indexer: read the base file inventory of %q: %w", c.repoPrefix, err)
		}
		commit = make(map[string]struct{}, len(rows))
		for _, row := range rows {
			if rel, owned := builderRelPath(c.repoPrefix, row.FilePath); owned {
				commit[rel] = struct{}{}
			}
		}
		state.commitSide, state.commitSideFor = nil, 0
		if routed && route.CommitGenerationID > 0 {
			claims, err := c.cachedLayerClaims(ctx, route.CommitGenerationID, nil)
			if err != nil {
				return textCorpusSet{}, err
			}
			foldTextClaims(commit, claims)
			state.commitSide, state.commitSideFor = commit, route.CommitGenerationID
		}
	}
	dirty := map[string]bool{}
	for _, generationID := range chain {
		claims, err := c.cachedLayerClaims(ctx, generationID, chain)
		if err != nil {
			return textCorpusSet{}, err
		}
		for _, claim := range claims {
			dirty[claim.rel] = !claim.gone
		}
	}
	return textCorpusSet{commit: commit, dirty: dirty}, nil
}

// textClaim is one path a generation claims, as the text corpus folds it:
// present (a replace claim) or gone (a delete claim).
type textClaim struct {
	rel  string
	gone bool
}

// applyLayerClaims folds one routed generation's file claims into the path set.
func (c *CheckoutCoordinator) applyLayerClaims(generationID int64, paths map[string]struct{}) error {
	claims, err := c.loadLayerClaims(context.Background(), generationID)
	if err != nil {
		return err
	}
	foldTextClaims(paths, claims)
	return nil
}

// loadLayerClaims reads one generation's file claims through
// graphview.GenerationLayer, so the corpus follows the reader's ownership
// rules exactly and a generation the reader would refuse to compose is refused
// here too. A context path is not a claim (FilePaths leaves it out).
func (c *CheckoutCoordinator) loadLayerClaims(ctx context.Context, generationID int64) ([]textClaim, error) {
	layer, err := graphview.NewGenerationLayerContext(ctx, c.store.AtGeneration(generationID))
	if err != nil {
		return nil, fmt.Errorf("indexer: open generation %d: %w", generationID, err)
	}
	var claims []textClaim
	for _, graphPath := range layer.FilePaths() {
		rel, owned := builderRelPath(c.repoPrefix, graphPath)
		if !owned {
			continue
		}
		claims = append(claims, textClaim{rel: rel, gone: layer.IsTombstone(graphPath)})
	}
	return claims, nil
}

// cachedLayerClaims is loadLayerClaims through the searcher's per-generation
// cache. A published generation's claims never change, so a layer is read
// once for as long as it stays in the routed stack; keep, when not nil, is the
// working-tree chain being composed, and every other cached generation is
// forgotten. Callers hold textMu.
func (c *CheckoutCoordinator) cachedLayerClaims(ctx context.Context, generationID int64, keep []int64) ([]textClaim, error) {
	state := &c.textState
	if state.claims == nil {
		state.claims = map[int64][]textClaim{}
	}
	if keep != nil {
		for id := range state.claims {
			if id != generationID && !slices.Contains(keep, id) {
				delete(state.claims, id)
			}
		}
	}
	if claims, ok := state.claims[generationID]; ok {
		return claims, nil
	}
	claims, err := c.loadLayerClaims(ctx, generationID)
	if err != nil {
		return nil, err
	}
	state.claims[generationID] = claims
	return claims, nil
}

// foldTextClaims applies one generation's claims over the layers beneath it.
func foldTextClaims(paths map[string]struct{}, claims []textClaim) {
	for _, claim := range claims {
		if claim.gone {
			delete(paths, claim.rel)
			continue
		}
		paths[claim.rel] = struct{}{}
	}
}

// sortedTextPaths returns a path set in ascending order, the order a searcher
// is built in so docID order is path order.
func sortedTextPaths(set map[string]struct{}) []string {
	out := make([]string, 0, len(set))
	for rel := range set {
		out = append(out, rel)
	}
	sort.Strings(out)
	return out
}
