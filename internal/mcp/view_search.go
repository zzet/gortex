package mcp

import (
	"context"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/query"
	"github.com/zzet/gortex/internal/search"
	"github.com/zzet/gortex/internal/viewmetrics"
)

// A composed view reads its nodes and edges through one reader, but its search
// candidates cannot come from one index. A text index is a corpus, every
// generation writes its own rows into it, and a handle only ever matches the
// generation it is pinned to — so the corpus that answers a query about the
// indexed graph knows nothing about the files a checkout's generations
// re-derived, and the reader composing those generations has no way to reach
// the rows.
//
// What a routed request therefore binds, once, next to its reader: the same
// stack expressed as corpora. One entry per generation for the symbol lanes,
// and one more for the content lanes, each paired with the ownership claims
// the reader itself applies so a lower corpus's hit can be masked exactly the
// way a lower row is.

// contentQuerier is the read half of graph.ContentSearcher, which is the only
// half a search lane ever uses. The full interface carries the write side too,
// and a composed view has nothing to write with — every corpus in its stack is
// owned by whoever built the generation.
type contentQuerier interface {
	SearchContent(query, repoPrefix string, limit int) ([]graph.ContentHit, error)
}

// viewContentSource is one content corpus of a composed view's stack: the
// searcher to ask and, for a generation, what it hides from below. The base
// corpus carries no layer — nothing masks it from underneath.
type viewContentSource struct {
	searcher contentQuerier
	layer    graph.OverlayLayerReader
}

// viewContentSearcher answers content search over a composed view by querying
// every corpus in its stack and composing the results.
//
// It is deliberately not a graph.Reader. The request reader is asserted for a
// dozen capabilities on the read paths (bounded subgraphs, frontier expansion,
// BFS), and wrapping it to add one more would hide every capability the
// wrapper failed to forward. This is the content capability alone, handed to
// the two call sites that ask for it.
type viewContentSearcher struct {
	// sources are the stack's corpora bottom first: the indexed corpus at
	// index 0, then one generation per entry above it.
	sources []viewContentSource
}

// SearchContent runs the query against every corpus in the stack, drops what a
// higher generation has taken ownership of, and interleaves what survives.
//
// The merge is the same policy the symbol lanes use and for the same reason —
// each generation scores against its own corpus statistics, so rank position
// is the only comparable quantity. See query.MergeRankedSources.
func (v *viewContentSearcher) SearchContent(text, repoPrefix string, limit int) ([]graph.ContentHit, error) {
	if v == nil {
		return nil, nil
	}
	sources := make([][]graph.ContentHit, 0, len(v.sources))
	for i, source := range v.sources {
		if source.searcher == nil {
			sources = append(sources, nil)
			continue
		}
		hits, err := source.searcher.SearchContent(text, repoPrefix, limit)
		if err != nil {
			return nil, err
		}
		sources = append(sources, v.visible(i, hits))
	}
	query.RecordViewSearchSources(viewmetrics.CorpusContent, sources)
	merged := query.MergeRankedSources(sources, func(h graph.ContentHit) string { return h.NodeID })
	if len(merged) > limit {
		merged = merged[:limit]
	}
	return merged, nil
}

// visible drops the hits of one corpus that a generation above it speaks for.
//
// A content section is answered by two of the layer's claims: the path it was
// extracted from, which a generation masks whole under either ownership mode,
// and its node identity, which a generation can claim on its own. Both are
// asked, so a section is hidden whichever way the generation above expressed
// the change.
func (v *viewContentSearcher) visible(source int, hits []graph.ContentHit) []graph.ContentHit {
	if source >= len(v.sources)-1 || len(hits) == 0 {
		return hits
	}
	kept := make([]graph.ContentHit, 0, len(hits))
	for _, hit := range hits {
		if v.hiddenAbove(source, hit) {
			continue
		}
		kept = append(kept, hit)
	}
	return kept
}

func (v *viewContentSearcher) hiddenAbove(source int, hit graph.ContentHit) bool {
	for i := source + 1; i < len(v.sources); i++ {
		layer := v.sources[i].layer
		if layer == nil {
			continue
		}
		if hit.FilePath != "" && layer.HasFile(hit.FilePath) {
			return true
		}
		if hit.NodeID != "" && (layer.CoversNodeID(hit.NodeID) || layer.OwnsNodeIdentity(hit.NodeID)) {
			return true
		}
	}
	return false
}

// bindSources records the stack's own corpora on the request view, so every
// search this request runs enumerates the whole view rather than the indexed
// corpus underneath it.
//
// It runs once per request, right after materialization: the handles are
// pinned to leased generations and stay readable for exactly as long as the
// view does.
func (v *requestView) bindSources(sources []graphview.GenerationSource, base graph.Reader) {
	if v == nil || len(sources) == 0 {
		return
	}
	candidates := make([]query.ViewLayerSource, 0, len(sources))
	// The indexed corpus is the bottom content source and is masked by
	// every generation above it; it claims nothing itself, so it carries
	// no layer.
	baseContent, _ := base.(contentQuerier)
	content := make([]viewContentSource, 0, len(sources)+1)
	content = append(content, viewContentSource{searcher: baseContent})
	for _, source := range sources {
		if source.Handle == nil {
			continue
		}
		candidates = append(candidates, query.ViewLayerSource{
			Search: search.NewSymbolSearcherBackend(source.Handle),
			Layer:  source.Layer,
		})
		content = append(content, viewContentSource{searcher: source.Handle, layer: source.Layer})
	}
	if len(candidates) == 0 {
		return
	}
	v.candidates = candidates
	v.content = &viewContentSearcher{sources: content}
}

// candidateLayers is the stack the query engine enumerates candidates across,
// nil for a request the base corpus serves.
func (v *requestView) candidateLayers() []query.ViewLayerSource {
	if v == nil {
		return nil
	}
	return v.candidates
}

// contentSearcherFor returns the content corpus this request searches.
//
// A base request asserts the capability on its reader exactly as it always
// has. A routed request cannot — its reader is a composition, and no
// composition carries a content index — so it answers from the stack's own
// corpora instead.
//
// A buffer overlay keeps today's posture either way: it finds no searcher and
// the lane merges nothing, rather than authenticating candidates from durable
// rows the editor buffer has already changed.
func (s *Server) contentSearcherFor(ctx context.Context) (contentQuerier, bool) {
	if cs, ok := s.readerFor(ctx).(graph.ContentSearcher); ok {
		return cs, true
	}
	if OverlayViewFromContext(ctx) != nil {
		return nil, false
	}
	view := requestViewFromContext(ctx)
	if view == nil || view.content == nil {
		return nil, false
	}
	return view.content, true
}
