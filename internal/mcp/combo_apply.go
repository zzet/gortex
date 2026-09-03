package mcp

import (
	"context"
	"time"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/search/rerank"
)

// applyRerankBoostsTimed is the I13 entry point that runs the full
// 11-signal rerank.Pipeline over the candidate set with the session-
// aware Context wired in (locality, combo, frecency, feedback, churn,
// community). Structural signals (BM25 rank, fan-in / fan-out,
// MinHash similarity, signature match, recency) are computed off the
// graph + the candidate's current index.
//
// rerankCtx is the per-request Context built by the server; pass nil
// and the pipeline falls back to a structural-only rerank using just
// the graph data on the nodes. lastResults is the optional rich
// candidate slice — when non-nil it carries per-signal contributions
// out to the caller for debug / winnow surfacing; pass nil if the
// caller only wants the sorted nodes.
//
// Returns the rerank's prepare and signals phase durations separately
// so the search_symbols handler's per-phase Debug log can attribute
// time honestly between the batched edge fetch (prepare) and the
// in-process scoring loop (signals). Zero durations when there's no
// work to do.
func applyRerankBoostsTimed(ctx context.Context, s *Server, nodes []*graph.Node, query string, rerankCtx *rerank.Context, lastResults *[]*rerank.Candidate) (result []*graph.Node, prepare time.Duration, signals time.Duration) {
	if len(nodes) < 2 || s == nil || s.engine == nil {
		return nodes, 0, 0
	}
	pipeline := s.engine.Rerank()
	if pipeline == nil {
		return nodes, 0, 0
	}
	cands := make([]*rerank.Candidate, 0, len(nodes))
	for i, n := range nodes {
		cands = append(cands, &rerank.Candidate{
			Node: n, TextRank: i, VectorRank: -1,
		})
	}
	if rerankCtx == nil {
		rerankCtx = &rerank.Context{}
	}
	if rerankCtx.Graph == nil {
		// Same reader buildRerankContext would have set, so a Context the
		// caller left ungraphed still scores against the request's view.
		rerankCtx.Graph = s.readerFor(ctx)
	}

	// Phase 1: prepare — the batched in/out edge fetch + scratch fields.
	// Exposed via the explicit Prepare call; Pipeline.Rerank detects the
	// already-prepared slice and skips the duplicate work.
	prepStart := time.Now()
	rerankCtx.Prepare(cands)
	prepare = time.Since(prepStart)

	// Phase 2: signals — the in-process scoring loop + final sort.
	sigStart := time.Now()
	pipeline.Rerank(query, cands, rerankCtx)
	signals = time.Since(sigStart)

	result = make([]*graph.Node, 0, len(cands))
	for _, c := range cands {
		result = append(result, c.Node)
	}
	if lastResults != nil {
		*lastResults = cands
	}
	return result, prepare, signals
}

// recordLastSearchFromNodes stores exactly one returned search page so later
// symbol/file consumption can be attributed without graph reads. Empty pages
// intentionally clear the prior page.
func recordLastSearchFromNodes(sess *sessionState, query string, nodes []*graph.Node) {
	if sess == nil {
		return
	}
	sess.recordLastSearchPage(query, nodes)
}
