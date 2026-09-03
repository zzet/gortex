package query

import (
	"sort"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/search"
	"github.com/zzet/gortex/internal/viewmetrics"
)

// ViewLayerSource is one generation of a composed view's stack as candidate
// enumeration sees it.
//
// The composed reader answers every node and edge question for the whole
// stack, but it cannot answer a full-text one: a text index is a corpus, each
// generation carries its own rows, and a handle only ever matches the
// generation it is pinned to. So a search over a composed view enumerates
// candidates from every corpus in the stack — the indexed corpus through the
// engine's ordinary search provider, and one of these per generation on top of
// it — and composes the results the same way the reader composes rows.
//
// Layer is not the corpus's twin, it is the corpus's ownership claim: which
// paths the generation replaced or deleted and which identities it speaks for.
// It is the contract the composed reader itself applies, so masking a lower
// corpus's hits asks the same question the reader asks of a lower row.
type ViewLayerSource struct {
	// Search enumerates candidates from this generation alone.
	Search search.Backend
	// Layer is what this generation hides from everything below it.
	Layer graph.OverlayLayerReader
}

// WithViewLayers returns a clone that reads through the composed view reader r
// and enumerates candidates across every generation stacked in it.
//
// An empty layer slice is the base request: the clone is exactly what
// WithReader returns, and every search it serves takes the base path with no
// per-generation work at all.
func (e *Engine) WithViewLayers(r graph.Reader, layers []ViewLayerSource) *Engine {
	clone := e.WithReader(r)
	if clone == nil || len(layers) == 0 {
		return clone
	}
	clone.viewLayers = layers
	return clone
}

// viewLayersActive reports whether this engine enumerates candidates across a
// composed view's stack rather than the base corpus alone.
func (e *Engine) viewLayersActive() bool { return len(e.viewLayers) > 0 }

// viewTextCandidates enumerates the text channel across the whole stack and
// returns one merged ranked list.
//
// base is what the engine's ordinary search provider returned for the indexed
// corpus; it enters the merge as the bottom source, and each generation in the
// stack is queried on its own handle above it. The three composition rules run
// before the merge: a candidate a higher generation speaks for is dropped, an
// identity two generations both returned is kept only from the higher one, and
// what survives is interleaved by rank position. Payload correctness is not
// this function's job — every surviving id is materialised through the
// composed reader by the caller, which also drops whatever the view hides that
// ownership alone did not catch.
func (e *Engine) viewTextCandidates(
	query string,
	limit int,
	base []search.SearchResult,
	refillBase func(int) []search.SearchResult,
) []search.SearchResult {
	if limit <= 0 {
		return nil
	}

	fetch := limit
	if len(base) > fetch {
		fetch = len(base)
	}
	raw := make([][]search.SearchResult, len(e.viewLayers)+1)
	exhausted := make([]bool, len(raw))
	raw[0] = base
	exhausted[0] = refillBase == nil || len(base) < fetch
	for i, layer := range e.viewLayers {
		if layer.Search == nil {
			exhausted[i+1] = true
			continue
		}
		raw[i+1] = layer.Search.Search(query, fetch)
		exhausted[i+1] = len(raw[i+1]) < fetch
	}

	compose := func() ([][]search.SearchResult, []search.SearchResult) {
		sources := append([][]search.SearchResult(nil), raw...)
		e.composeViewSources(sources)
		merged := MergeRankedSources(sources, func(r search.SearchResult) string { return r.ID })
		return sources, merged
	}
	finish := func(sources [][]search.SearchResult, merged []search.SearchResult) []search.SearchResult {
		RecordViewSearchSources(viewmetrics.CorpusSymbol, sources)
		if len(merged) > limit {
			merged = merged[:limit]
		}
		return merged
	}

	for {
		sources, merged := compose()
		if len(merged) >= limit {
			return finish(sources, merged)
		}

		nextFetch := fetch * 2
		if nextFetch <= fetch {
			return finish(sources, merged)
		}
		active := false
		grew := false
		for i := range raw {
			if exhausted[i] {
				continue
			}
			active = true
			previous := len(raw[i])
			if i == 0 {
				raw[i] = refillBase(nextFetch)
			} else {
				raw[i] = e.viewLayers[i-1].Search.Search(query, nextFetch)
			}
			if len(raw[i]) > previous {
				grew = true
			}
			exhausted[i] = len(raw[i]) < nextFetch || len(raw[i]) <= previous
		}
		if !active {
			return finish(sources, merged)
		}
		fetch = nextFetch
		if !grew {
			// Use the most recent answers even when a backend could not grow;
			// then stop rather than reissuing the same capped query forever.
			sources, merged = compose()
			return finish(sources, merged)
		}
	}
}

// viewBaseTextRefill repeats only the indexed corpus's text lane at a deeper
// width. Bundle search is preferred because it preserves repository narrowing
// and never wakes the vector channel merely to refill masked BM25 candidates.
func viewBaseTextRefill(backend search.Backend, query string, repoAllow []string) func(int) []search.SearchResult {
	return func(limit int) []search.SearchResult {
		if backend == nil || limit <= 0 {
			return nil
		}
		if len(repoAllow) > 0 {
			if scoped, ok := backend.(search.ScopedSymbolBundleSearcherBackend); ok {
				if bundles := scoped.SearchSymbolBundlesScoped(query, repoAllow, limit); bundles != nil {
					return viewBundleResults(bundles)
				}
			}
		}
		if bundled, ok := backend.(search.SymbolBundleSearcherBackend); ok {
			if bundles := bundled.SearchSymbolBundles(query, limit); bundles != nil {
				return viewBundleResults(bundles)
			}
		}
		if channels, ok := backend.(search.ChannelSearcher); ok {
			text, _ := channels.SearchChannels(query, limit)
			return text
		}
		return backend.Search(query, limit)
	}
}

func viewBundleResults(bundles []search.SymbolBundle) []search.SearchResult {
	out := make([]search.SearchResult, 0, len(bundles))
	for _, bundle := range bundles {
		if bundle.Node == nil {
			continue
		}
		out = append(out, search.SearchResult{ID: bundle.Node.ID, Score: bundle.Score})
	}
	return out
}

// RecordViewSearchSources counts one composite search and how many of the
// stack's corpora survived composition with something to contribute.
//
// The pair is what makes "which layers served a query" answerable: the query
// counter is the denominator, the source counter the numerator, and their
// ratio says whether a routed view's generations are actually being read or
// whether every answer is coming out of the indexed corpus underneath them.
// Neither carries a stack position — a per-layer label would be a generation
// id by another name — so this counts contributing sources, not which ones.
func RecordViewSearchSources[T any](corpus string, sources [][]T) {
	viewmetrics.Count(viewmetrics.SearchQueryTotal, corpus)
	contributing := 0
	for _, source := range sources {
		if len(source) > 0 {
			contributing++
		}
	}
	if contributing > 0 {
		viewmetrics.Add(viewmetrics.SearchSourceTotal, int64(contributing), corpus)
	}
}

// composeViewSources applies masking and cross-source dedup to the per-source
// hit lists, rewriting each source in place with what survives.
//
// Masking is the reader's own ownership predicate: a generation speaks for an
// identity when it claims the identity's file — under either ownership mode,
// so a deletion hides the row as surely as a replacement — or when it carries
// or tombstoned the identity itself. A hit from a corpus below a generation
// that speaks for its identity is a row the view does not expose, and it is
// dropped before it can cost a candidate slot or a rank position.
//
// Dedup then keeps one occurrence per identity, from the highest source that
// returned it. In practice masking has already settled it — a generation whose
// corpus returned an id necessarily carries a node under that id, so the lower
// occurrence is masked — but the rule is stated here rather than inferred,
// because it is what makes the merge below order-independent.
func (e *Engine) composeViewSources(sources [][]search.SearchResult) {
	for i := range sources {
		if len(sources[i]) == 0 {
			continue
		}
		kept := make([]search.SearchResult, 0, len(sources[i]))
		for _, hit := range sources[i] {
			if hit.ID == "" || e.hiddenAboveSource(i, hit.ID) {
				continue
			}
			kept = append(kept, hit)
		}
		sources[i] = kept
	}
	// Highest source first, so the first occurrence of an id is the one to
	// keep and every lower repeat of it is dropped.
	seen := make(map[string]struct{})
	for i := len(sources) - 1; i >= 0; i-- {
		kept := make([]search.SearchResult, 0, len(sources[i]))
		for _, hit := range sources[i] {
			if _, dup := seen[hit.ID]; dup {
				continue
			}
			seen[hit.ID] = struct{}{}
			kept = append(kept, hit)
		}
		sources[i] = kept
	}
}

// hiddenAboveSource reports whether any generation above source speaks for an
// identity. Source 0 is the indexed corpus and source k>0 is viewLayers[k-1],
// so the generations above source k are viewLayers[k:].
func (e *Engine) hiddenAboveSource(source int, id string) bool {
	for i := source; i < len(e.viewLayers); i++ {
		layer := e.viewLayers[i].Layer
		if layer == nil {
			continue
		}
		if layer.CoversNodeID(id) || layer.OwnsNodeIdentity(id) {
			return true
		}
	}
	return false
}

// MergeRankedSources interleaves per-source ranked lists into one order.
//
// # The ranking policy, and why it is this one
//
// Each source is a different corpus. A generation's BM25 score is computed
// against the handful of files that generation re-derived, the indexed
// corpus's against a whole repository — the same document scores differently
// in each, and the difference is a property of the corpus statistics, not of
// the document's relevance. Comparing the two numbers, or rescaling one onto
// the other, would be inventing a comparison the data does not support.
//
// So the scores are not compared at all. What is compared is each hit's rank
// position inside its own source, which is the one quantity every corpus
// expresses on the same scale: "the best answer this corpus has" means the
// same thing whether the corpus holds four files or forty thousand. The
// sources are interleaved by that position. Equal-rank hits are ordered by
// logical identity when one is available, a deterministic tie-break that does
// not grant either the indexed corpus or an overlay a blanket priority.
//
// Duplicate ownership is decided separately from relevance: the highest source
// carrying an identity supplies that identity's payload, at that source's own
// rank. This is the only overlay priority the merge applies.
//
// This is an interim policy. The successor is exact per-view corpus
// statistics: with the document frequencies of the composed view rather than
// of each generation separately, every hit can be scored against one corpus
// and the merge becomes an ordinary sort. Until those statistics exist, rank
// position is the honest merge.
//
// id extracts the identity a duplicate is recognised by; pass nil, or return
// "", to keep every entry. Ranking within one source is preserved.
func MergeRankedSources[T any](sources [][]T, id func(T) string) []T {
	total := 0
	for _, source := range sources {
		total += len(source)
	}
	if total == 0 {
		return nil
	}

	// A later source is a higher layer. Resolve duplicate ownership before
	// ranking so a lower copy cannot win merely because it ranked earlier in
	// its own stale corpus.
	var owner map[string]int
	if id != nil {
		owner = make(map[string]int, total)
		for source, items := range sources {
			for _, item := range items {
				if key := id(item); key != "" {
					owner[key] = source
				}
			}
		}
	}

	type entry struct {
		item    T
		key     string
		rank    int
		source  int
		ordinal int
	}
	entries := make([]entry, 0, total)
	ordinal := 0
	for source, items := range sources {
		for rank, item := range items {
			key := ""
			if id != nil {
				key = id(item)
				if key != "" && owner[key] != source {
					ordinal++
					continue
				}
			}
			entries = append(entries, entry{
				item: item, key: key, rank: rank, source: source, ordinal: ordinal,
			})
			ordinal++
		}
	}
	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].rank != entries[j].rank {
			return entries[i].rank < entries[j].rank
		}
		if entries[i].key != "" && entries[j].key != "" && entries[i].key != entries[j].key {
			return entries[i].key < entries[j].key
		}
		return entries[i].ordinal < entries[j].ordinal
	})

	out := make([]T, 0, len(entries))
	var seen map[string]struct{}
	if id != nil {
		seen = make(map[string]struct{}, len(entries))
	}
	for _, candidate := range entries {
		if candidate.key != "" {
			if _, duplicate := seen[candidate.key]; duplicate {
				continue
			}
			seen[candidate.key] = struct{}{}
		}
		out = append(out, candidate.item)
	}
	return out
}
