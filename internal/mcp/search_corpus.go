package mcp

import (
	"context"
	"fmt"
	"strings"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/query"
)

// searchCorpus selects which slice of the index search_symbols draws
// from: code symbols, documentation prose, or both.
type searchCorpus int

const (
	// corpusCode is the default -- only code symbols (functions,
	// types, ...), prose-section KindDoc nodes excluded.
	corpusCode searchCorpus = iota
	// corpusDocs returns only KindDoc prose-section nodes.
	corpusDocs
	// corpusAll returns code symbols and prose sections together.
	corpusAll
	// corpusContent returns only content KindDoc nodes — the
	// data_class=content chunks from pdf / office / text documents,
	// excluding Markdown prose sections.
	corpusContent
)

// parseCorpus reads the `corpus` argument. "" / "code" -> corpusCode,
// "docs" / "doc" / "prose" -> corpusDocs, "all" / "both" -> corpusAll.
// An unrecognised value is an error so a typo surfaces clearly rather
// than silently returning the wrong corpus.
func parseCorpus(req mcpgo.CallToolRequest) (searchCorpus, error) {
	switch strings.ToLower(strings.TrimSpace(req.GetString("corpus", ""))) {
	case "", "code":
		return corpusCode, nil
	case "docs", "doc", "prose":
		return corpusDocs, nil
	case "all", "both":
		return corpusAll, nil
	case "content":
		return corpusContent, nil
	default:
		return corpusCode, fmt.Errorf("invalid corpus: %q (want code, docs, content, or all)",
			req.GetString("corpus", ""))
	}
}

// includesDocs reports whether the corpus admits Markdown prose-section
// nodes, which the symbol search still serves. Content (pdf / office / text)
// now rides its own content-index channel — see includesContent — so
// corpusContent does NOT pull the prose channel.
func (c searchCorpus) includesDocs() bool {
	return c == corpusDocs || c == corpusAll
}

// includesContent reports whether the corpus admits content-index sections
// (data_class=content), served by the dedicated ContentSearcher rather than
// the symbol search. corpusDocs keeps the historical superset semantic (all
// KindDoc), so it pulls the content channel too.
func (c searchCorpus) includesContent() bool {
	return c == corpusContent || c == corpusDocs || c == corpusAll
}

// includesCode reports whether the corpus admits code-symbol nodes.
func (c searchCorpus) includesCode() bool { return c == corpusCode || c == corpusAll }

// docChannelFetchMultiple widens the limit of the parallel doc-biased
// fetch relative to the primary fetch. Prose sections are a minority
// of the corpus, so a doc that the query genuinely matches can sit
// well past the primary fetchLimit behind code symbols that share its
// tokens. Over-fetching here, then keeping only the KindDoc hits, lets
// those prose sections enter the candidate pool. Bounded so the extra
// fetch stays cheap on a large index.
const docChannelFetchMultiple = 4

// docChannelMaxLimit caps the absolute size of the doc-channel fetch
// so a large `limit` request can't blow the over-fetch up unboundedly.
const docChannelMaxLimit = 200

// mergeDocChannel runs a parallel, wider-limit fetch biased to surface
// prose-section (KindDoc) nodes and merges the new doc hits into the
// existing candidate slice. It is the real retrieval channel behind
// corpus:"docs" / "all": the primary fetch is code-shaped and a
// relevant doc can rank just past its limit, so the corpus post-filter
// would have nothing to keep. By over-fetching and admitting only the
// KindDoc hits not already present, prose competes on its own terms
// before the corpus filter and the rerank run.
//
// Existing candidates keep their position; new doc hits append in
// their fetch order (the rerank pass settles final ranking). Dedup is
// by node ID. When the wider fetch surfaces no new docs the input is
// returned unchanged.
func (s *Server) mergeDocChannel(ctx context.Context, query string, nodes []*graph.Node, fetchLimit int, scope query.QueryOptions, timings *query.SearchTimings) []*graph.Node {
	if strings.TrimSpace(query) == "" {
		return nodes
	}
	docLimit := fetchLimit * docChannelFetchMultiple
	if docLimit > docChannelMaxLimit {
		docLimit = docChannelMaxLimit
	}
	if docLimit <= fetchLimit {
		docLimit = fetchLimit
	}

	start := time.Now()
	wide := s.engineFor(ctx).SearchSymbolsScoped(query, docLimit, scope)
	if timings != nil {
		timings.BM25ExpansionMS += time.Since(start).Milliseconds()
	}
	if len(wide) == 0 {
		return nodes
	}

	seen := make(map[string]struct{}, len(nodes))
	for _, n := range nodes {
		if n != nil {
			seen[n.ID] = struct{}{}
		}
	}
	merged := nodes
	for _, n := range wide {
		if n == nil || n.Kind != graph.KindDoc {
			continue
		}
		if _, dup := seen[n.ID]; dup {
			continue
		}
		seen[n.ID] = struct{}{}
		merged = append(merged, n)
	}
	return merged
}

// mergeContentChannel is the retrieval channel for content sections. Since
// content (data_class=content) bodies live in the dedicated content index
// (content_fts) and not the symbol search, the primary + doc fetches above
// can never surface them — this queries the ContentSearcher directly,
// scoped to the session's repo, materialises the matched content nodes by
// ID, and merges them into the candidate pool (dedup by node ID). A nil
// content searcher (in-memory store) or empty result returns the input
// unchanged.
func (s *Server) mergeContentChannel(ctx context.Context, query string, nodes []*graph.Node, fetchLimit int) []*graph.Node {
	if strings.TrimSpace(query) == "" {
		return nodes
	}
	// The content-index capability comes from the request reader on a base
	// call and from the routed stack's own corpora on a composed one; an
	// overlay-active call finds no searcher either way and merges nothing
	// rather than pulling durable rows for files the editor buffer already
	// changed.
	reader := s.readerFor(ctx)
	cs, ok := s.contentSearcherFor(ctx)
	if !ok {
		return nodes
	}
	limit := fetchLimit * docChannelFetchMultiple
	if limit > docChannelMaxLimit {
		limit = docChannelMaxLimit
	}
	if limit <= fetchLimit {
		limit = fetchLimit
	}
	repoPrefix, _ := s.sessionLocality(ctx)
	// A workspace-root binding has no home repo, so the prefix above is
	// empty and SearchContent would span every repo in every workspace —
	// the one channel the session's workspace boundary doesn't reach
	// (the merged hits are materialised by raw GetNode below). Clamp the
	// hits to the bound workspace's repo set; a bound session whose
	// workspace enumerates no repos fails closed and merges nothing.
	var allow map[string]bool
	if repoPrefix == "" {
		if ws, _, bound := s.sessionScope(ctx); bound {
			if s.multiIndexer == nil {
				return nodes
			}
			allow = s.multiIndexer.ReposInWorkspace(ws)
			if len(allow) == 0 {
				return nodes
			}
		}
	}
	hits, err := cs.SearchContent(query, repoPrefix, limit)
	if err != nil || len(hits) == 0 {
		return nodes
	}

	seen := make(map[string]struct{}, len(nodes))
	for _, n := range nodes {
		if n != nil {
			seen[n.ID] = struct{}{}
		}
	}
	merged := nodes
	for _, h := range hits {
		if _, dup := seen[h.NodeID]; dup {
			continue
		}
		n := reader.GetNode(h.NodeID)
		if n == nil {
			continue
		}
		if allow != nil && !allow[n.RepoPrefix] {
			continue
		}
		seen[h.NodeID] = struct{}{}
		merged = append(merged, n)
	}
	return merged
}

// filterNodesByCorpus drops nodes that fall outside the selected
// corpus. KindDoc nodes are the "docs" corpus; every other kind is
// "code". corpusAll is a no-op.
func filterNodesByCorpus(nodes []*graph.Node, c searchCorpus) []*graph.Node {
	if c == corpusAll {
		return nodes
	}
	out := make([]*graph.Node, 0, len(nodes))
	for _, n := range nodes {
		if n == nil {
			continue
		}
		isDoc := n.Kind == graph.KindDoc
		if c == corpusContent {
			// Content corpus: only the data_class=content chunks (pdf /
			// office / text), not Markdown prose sections.
			if isDoc && isContentNode(n) {
				out = append(out, n)
			}
			continue
		}
		if isDoc && c.includesDocs() {
			out = append(out, n)
		} else if !isDoc && c.includesCode() {
			out = append(out, n)
		}
	}
	return out
}

// isContentNode is the mcp-local alias for graph.IsContentNode — a KindDoc
// node tagged data_class=content by a content extractor (pdf / pptx / xlsx /
// txt).
func isContentNode(n *graph.Node) bool {
	return graph.IsContentNode(n)
}
