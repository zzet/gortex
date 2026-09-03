package mcp

import (
	"context"

	"github.com/zzet/gortex/internal/embedding"
	"github.com/zzet/gortex/internal/search/rerank"
)

// buildRerankContext assembles the per-request rerank.Context with
// every session-aware data source the server holds: locality, combo,
// frecency, feedback, and churn. Pure structural signals (BM25 rank,
// fan-in / fan-out, MinHash, signature match, recency, community) do
// not depend on session state and read the graph directly through the
// Context.Graph reader set here — the calling request's reader, so an
// overlay-active search scores against the editor buffer.
//
// Returned Context is safe to reuse for the lifetime of the request
// but should not be cached across requests — the combo boost map is
// query-specific and the locality fields are session-specific.
func (s *Server) buildRerankContext(ctx context.Context, query string) *rerank.Context {
	repo, project := s.sessionLocality(ctx)
	rctx := &rerank.Context{
		Graph:             s.readerFor(ctx),
		RepoPrefix:        repo,
		ProjectID:         project,
		AnalysisMetricsOf: s.rerankAnalysisMetrics,
		BatchedCentrality: func(seeds, candidateIDs []string) rerank.CentralityResult {
			return s.rerankBoundedCentrality(ctx, seeds, candidateIDs)
		},
	}

	if s.combo != nil {
		// The combo boost fuses two stores: the exact whole-query
		// index (BoostMap) and the per-keyword association index
		// (KeywordBoostMap). They are max()-merged so a symbol picks
		// up the stronger of "this exact query led here before" and
		// "queries sharing these keywords led here before" -- the
		// exact-query boost is capped higher, so it dominates a
		// keyword-only boost whenever both fire. FeedbackSignal reads
		// the merged closure unchanged.
		boosts := s.combo.BoostMap(query)
		kwBoosts := s.combo.KeywordBoostMap(query)
		if len(boosts) > 0 || len(kwBoosts) > 0 {
			merged := make(map[string]float64, len(boosts)+len(kwBoosts))
			for id, v := range kwBoosts {
				merged[id] = v
			}
			for id, v := range boosts {
				if existing, ok := merged[id]; !ok || v > existing {
					merged[id] = v
				}
			}
			rctx.ComboBoostOf = func(id string) float64 {
				if v, ok := merged[id]; ok {
					return v
				}
				return 1.0
			}
		}
	}

	if s.frecency != nil && s.frecency.HasData() {
		ft := s.frecency
		rctx.FrecencyBoostOf = func(id string) float64 { return ft.BoostFor(id) }
	}

	if s.feedback != nil && s.feedback.HasData() {
		fb := s.feedback
		rctx.FeedbackOf = func(id string) float64 { return fb.GetSymbolScoreForQuery(id, query) }
	}

	if s.symHistory != nil {
		churn := s.churnCounts()
		if len(churn) > 0 {
			rctx.ChurnOf = func(id string) int { return churn[id] }
		}
	}

	// Centrality is built from a capped candidate-seeded neighborhood when
	// Prepare knows the complete result batch. This avoids restoring the
	// whole-graph CSR for every interactive search.

	// Co-change feeds the rerank pipeline once the git-history mine
	// has run (lazily, on the first find_co_changing_symbols call, or
	// from an enriched snapshot). Until then the signal sits at 0.
	if s.hasCoChangeData() {
		rctx.CoChangeOf = s.coChangeScores
	}

	// Semantic-cosine channel: the in-process static code-embedding
	// model (with the baked GloVe word vectors as offline fallback)
	// re-scores the BM25 top-N against the query with no ANN index and
	// no index-time vector build. Wired unconditionally — the per-class
	// weight table damps it hard on identifier / path queries so it
	// earns its keep only on natural-language intent queries, where
	// BM25 alone cannot bridge "decode bson body" to BindBody. An empty
	// query vector leaves the signal at 0, so a pure-identifier query
	// is unaffected even before the class damping.
	if emb := embedding.SharedCodeEmbedder(); emb != nil {
		rctx.EmbedText = embedding.EmbedTextFunc(emb)
		if qv, err := emb.Embed(ctx, query); err == nil {
			rctx.QueryVec = qv
		}
	}

	return rctx
}
