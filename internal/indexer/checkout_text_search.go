package indexer

import (
	"context"
	"fmt"
	"regexp"
	"sort"

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
// searches never pays for an index — and it is keyed by the working tree it
// was built for, so the next search after an edit rebuilds it instead of
// answering out of stale postings.

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
// served is false when no coordinator holds the checkout. That is the whole of
// the answer rather than a reason to look elsewhere: nothing else in the daemon
// indexes that working copy, and the canonical checkout's bytes describe a
// different tree.
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
	searcher, err := coordinator.textSearcher(ctx)
	if err != nil {
		return nil, true, err
	}
	if q.Regexp != nil {
		return searcher.GrepRegexp(q.Regexp, extractRegexLiterals(q.Query), "", q.Limit), true, nil
	}
	return searcher.Grep(q.Query, q.Limit), true, nil
}

// textSearcher returns the checkout's trigram searcher, building it when there
// is none or when the working tree has moved since the cached one was built.
//
// The lock is held across the build on purpose, the way the per-repository
// searcher does it: a burst of concurrent searches on one checkout collapses
// into a single build instead of several racing ones, each paying full corpus
// memory.
func (c *CheckoutCoordinator) textSearcher(ctx context.Context) (*trigram.Searcher, error) {
	key := c.dirtyKey()

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
	paths, err := c.textCorpus(ctx)
	if err != nil {
		return nil, err
	}
	c.textIndex = trigram.Build(c.root, paths)
	c.textKey = key
	viewmetrics.Count(viewmetrics.CheckoutSearcherBuiltTotal)
	return c.textIndex, nil
}

// noteDirtyFingerprint records the working tree the cycle just sampled.
//
// It does not rebuild, and it does not drop what is cached: a rebuild costs a
// pass over the checkout and only a search needs one. The recorded fingerprint
// is the invalidation — the next search finds the cached searcher keyed to a
// tree that has moved and builds over the current one, replacing it.
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
	c.textMu.Unlock()
}

// textCorpus is the repo-relative file set the checkout's working tree
// presents to a search: the base corpus for this repository with the routed
// layers' file claims applied over it, bottom layer first.
//
// It is the composition the view's reader performs, expressed as paths. A
// replace claim puts the path in — which is how a file the branch added, and
// which the base corpus has never seen, becomes searchable — and a delete claim
// takes it out, so a file the checkout removed cannot match through the base
// corpus's copy of it.
//
// A checkout with no route yet is searched over the base corpus's paths alone.
// The bytes still come from the checkout root, so the answer is about the right
// working copy; what it cannot yet see is a path only the layers know about.
func (c *CheckoutCoordinator) textCorpus(ctx context.Context) ([]string, error) {
	base, err := c.primaryBase(ctx)
	if err != nil {
		return nil, fmt.Errorf("indexer: resolve the base file inventory of %q: %w", c.repoPrefix, err)
	}
	rows, err := c.store.AtGeneration(base.generationID).FileMetasForRepo(c.repoPrefix)
	if err != nil {
		return nil, fmt.Errorf(
			"indexer: read the base file inventory of %q at generation %d: %w",
			c.repoPrefix,
			base.generationID,
			err,
		)
	}
	paths := make(map[string]struct{}, len(rows))
	for _, row := range rows {
		if rel, owned := builderRelPath(c.repoPrefix, row.FilePath); owned {
			paths[rel] = struct{}{}
		}
	}

	route, found, err := c.catalog.GetCheckoutRoute(ctx, c.checkoutID)
	if err != nil {
		return nil, fmt.Errorf("indexer: read the route of checkout %q: %w", c.checkoutID, err)
	}
	if found {
		for _, generationID := range []int64{route.CommitGenerationID, route.DirtyGenerationID} {
			if generationID <= 0 {
				continue
			}
			if err := c.applyLayerClaims(generationID, paths); err != nil {
				return nil, err
			}
		}
	}

	out := make([]string, 0, len(paths))
	for rel := range paths {
		out = append(out, rel)
	}
	sort.Strings(out)
	return out, nil
}

// applyLayerClaims folds one routed generation's file claims into the path set.
func (c *CheckoutCoordinator) applyLayerClaims(generationID int64, paths map[string]struct{}) error {
	layer, err := graphview.NewGenerationLayer(c.store.AtGeneration(generationID))
	if err != nil {
		return fmt.Errorf("indexer: open generation %d: %w", generationID, err)
	}
	for _, graphPath := range layer.FilePaths() {
		rel, owned := builderRelPath(c.repoPrefix, graphPath)
		if !owned {
			continue
		}
		if layer.IsTombstone(graphPath) {
			delete(paths, rel)
			continue
		}
		paths[rel] = struct{}{}
	}
	return nil
}
