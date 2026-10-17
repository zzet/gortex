package mcp

import (
	"sync"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/search"
)

// answerAnalysisSnapshot is what an answer-path read of the analysis snapshot
// observed the last time analysisMu was free: the published generation
// receipt, the graph token it was computed against, and whether the lock-path
// currentness checks passed for it at that instant.
//
// The answer path (symbol-search ranking and its query expansion) reads the
// analysis snapshot on every request, and RunAnalysis holds analysisMu for its
// whole pass — 13 s on a cache hit, and 5–80 minutes on a large workspace when
// the graph changed. A reader that waits for that lock makes every symbol
// search wait for the whole pass. These reads never wait: when the lock is
// held (or a writer is queued for it), they answer from the last receipt, and
// only while that receipt is still current for the live graph. A pass that
// runs because the graph changed makes the old receipt stale, so the answer is
// "no analysis signal" — the same answer a request gets before the first pass
// — rather than a ranking scored from another graph. A pass that republishes
// the same revision leaves the receipt current, and ranking is unchanged.
type answerAnalysisSnapshot struct {
	header graph.AnalysisGenerationHeader
	token  communityCacheToken
	valid  bool
}

// answerAnalysisCache keeps the per-generation values the answer path would
// otherwise re-read from SQLite on every request: the two normalization maxima
// and the concept vocabulary. All three are immutable for one analysis
// generation (its rows never change once published), so they are keyed by the
// generation id and dropped when a different generation is observed.
type answerAnalysisCache struct {
	mu           sync.Mutex
	generationID int64
	topMetric    map[graph.AnalysisMetric]float64
	concepts     *search.AutoConcepts
	conceptsSet  bool
}

// answerAnalysisObserve refreshes the answer snapshot under a read lock it
// does not wait for, and returns the snapshot the caller should use: the fresh
// one when the lock was free, else the last one observed.
func (s *Server) answerAnalysisObserve() *answerAnalysisSnapshot {
	if s.analysisMu.TryRLock() {
		snap := &answerAnalysisSnapshot{
			header: s.analysisGeneration,
			token:  s.communitiesToken,
			valid: s.analysisGenerationReady && s.analysisGeneration.GenerationID > 0 &&
				s.analysisGeneration.FormatVersion == analysisGenerationFormatVersion &&
				s.analysisGeneration.GraphRevision == s.communitiesToken.analysisRevision &&
				s.analysisEpoch > 0,
		}
		s.analysisMu.RUnlock()
		s.answerAnalysis.Store(snap)
		return snap
	}
	return s.answerAnalysis.Load()
}

// activeAnalysisQueryForAnswer is activeAnalysisQuery for the answer path: the
// same receipt and the same currentness rule, without ever waiting for a
// running analysis pass. When analysisMu is free it answers exactly what
// activeAnalysisQuery answers.
func (s *Server) activeAnalysisQueryForAnswer() (graph.AnalysisQueryStore, graph.AnalysisGenerationHeader, bool) {
	snap := s.answerAnalysisObserve()
	if snap == nil || !snap.valid || snap.token != s.communityTokenForAnswer() {
		return nil, graph.AnalysisGenerationHeader{}, false
	}
	_, query := s.analysisGenerationBackends()
	if query == nil {
		return nil, graph.AnalysisGenerationHeader{}, false
	}
	return query, snap.header, true
}

// answerAnalysisCacheFor returns the per-generation cache, reset when the
// generation it holds is not the one named.
func (s *Server) answerAnalysisCacheFor(generationID int64) *answerAnalysisCache {
	c := &s.answerAnalysisValues
	c.mu.Lock()
	if c.generationID != generationID {
		c.generationID = generationID
		c.topMetric = nil
		c.concepts = nil
		c.conceptsSet = false
	}
	c.mu.Unlock()
	return c
}

// answerAnalysisNodeMetricsBatched is analysisNodeMetricsBatched through the
// answer-path receipt.
func (s *Server) answerAnalysisNodeMetricsBatched(nodeIDs []string) ([]graph.AnalysisNodeMetric, graph.AnalysisGenerationHeader, bool) {
	ids := dedupeStrings(nodeIDs)
	query, header, ok := s.activeAnalysisQueryForAnswer()
	if !ok {
		return nil, header, false
	}
	rows := make([]graph.AnalysisNodeMetric, 0, len(ids))
	for start := 0; start < len(ids); start += analysisGenerationQueryMax {
		end := min(start+analysisGenerationQueryMax, len(ids))
		page, err := boundedAnalysisIDs(ids[start:end])
		if err != nil {
			return nil, header, false
		}
		if len(page) == 0 {
			continue
		}
		metrics, err := query.AnalysisNodeMetrics(header.GenerationID, page)
		if err != nil {
			return nil, header, false
		}
		rows = append(rows, metrics...)
	}
	return rows, header, true
}

// answerTopAnalysisMetricValue is topAnalysisMetricValue memoized per analysis
// generation: the maximum of a metric over an immutable generation never
// changes, so it is read once per generation instead of once per search.
func (s *Server) answerTopAnalysisMetricValue(header graph.AnalysisGenerationHeader, metric graph.AnalysisMetric) float64 {
	cache := s.answerAnalysisCacheFor(header.GenerationID)
	cache.mu.Lock()
	if value, ok := cache.topMetric[metric]; ok && cache.generationID == header.GenerationID {
		cache.mu.Unlock()
		return value
	}
	cache.mu.Unlock()
	_, query := s.analysisGenerationBackends()
	if query == nil {
		return 0
	}
	value := 0.0
	rows, _, err := query.TopAnalysisNodeMetrics(header.GenerationID, metric, 1, nil)
	if err != nil {
		// Not memoized: a failed read is retried by the next request.
		return 0
	}
	if len(rows) > 0 {
		value = analysisMetricValue(rows[0], metric)
	}
	cache.mu.Lock()
	if cache.generationID == header.GenerationID {
		if cache.topMetric == nil {
			cache.topMetric = make(map[graph.AnalysisMetric]float64, 2)
		}
		cache.topMetric[metric] = value
	}
	cache.mu.Unlock()
	return value
}

// autoConceptsForAnswer is getAutoConcepts for the answer path. It never waits
// for a running analysis pass, and it keeps the concept vocabulary of the
// current generation for the answer path's own use: the shared compatibility
// view is dropped after every idle tool call (releaseTransientAnalysisIfIdle),
// which made every expansion re-read the whole vocabulary from SQLite.
func (s *Server) autoConceptsForAnswer() *search.AutoConcepts {
	// The published in-memory vocabulary, when the lock is free and it is
	// current, is what getAutoConcepts itself answers first.
	if s.analysisMu.TryRLock() {
		concepts, epoch, token := s.autoConcepts, s.analysisEpoch, s.communitiesToken
		s.analysisMu.RUnlock()
		current := concepts != nil && epoch > 0 && token == s.communityTokenForAnswer()
		if current {
			return concepts
		}
	}
	query, header, ok := s.activeAnalysisQueryForAnswer()
	if !ok {
		return nil
	}
	cache := s.answerAnalysisCacheFor(header.GenerationID)
	cache.mu.Lock()
	if cache.conceptsSet && cache.generationID == header.GenerationID {
		concepts := cache.concepts
		cache.mu.Unlock()
		return concepts
	}
	cache.mu.Unlock()

	combined := graph.AnalysisConceptQueryResult{}
	cursor := ""
	for {
		page, next, err := query.ListAnalysisConcepts(header.GenerationID, analysisGenerationQueryPage, cursor)
		if err != nil {
			s.logAnalysisMaterializationError("concepts", err)
			return nil
		}
		combined.Concepts = append(combined.Concepts, page.Concepts...)
		combined.Relations = append(combined.Relations, page.Relations...)
		if len(page.Concepts) == 0 || next == "" || next == cursor {
			break
		}
		cursor = next
	}
	concepts := conceptsFromQuery(combined)
	cache.mu.Lock()
	if cache.generationID == header.GenerationID {
		cache.concepts = concepts
		cache.conceptsSet = true
	}
	cache.mu.Unlock()
	return concepts
}

// answerTokenMemo is the last graph token the answer path computed, with the
// cheap revision counters it was computed at.
type answerTokenMemo struct {
	revision     uint64
	edgeIdentity int
	token        communityCacheToken
}

// communityTokenForAnswer is currentCommunityToken without its two whole-table
// counts whenever the graph has not been written since the last computation.
//
// currentCommunityToken counts every node and every edge of the corpus
// (SELECT COUNT(*) over each table), and the answer path asked for it several
// times per symbol search: on a live-sized store that is seconds of page
// reads per search. The counts can only change through a committed graph
// write, and every committed write advances the store's analysis mutation
// revision (finishAnalysisMutationLocked) — the counter the analysis
// publication itself trusts to detect a changed graph. So while that revision
// and the edge-identity revision stand where they stood, the last token is
// still the token, and it is returned without counting. A store without that
// revision is counted every time, as before.
func (s *Server) communityTokenForAnswer() communityCacheToken {
	writer, _ := s.analysisGenerationBackends()
	if writer == nil {
		return s.currentCommunityToken()
	}
	revision := writer.AnalysisMutationRevision()
	edgeIdentity := s.graph.EdgeIdentityRevisions()
	if memo := s.answerToken.Load(); memo != nil && memo.revision == revision && memo.edgeIdentity == edgeIdentity {
		return memo.token
	}
	token := s.currentCommunityToken()
	// Keep it only when no write landed while it was counted: the token then
	// describes the graph at exactly this revision.
	if token.analysisRevision == revision && token.edgeIdentity == edgeIdentity {
		s.answerToken.Store(&answerTokenMemo{revision: revision, edgeIdentity: edgeIdentity, token: token})
	}
	return token
}
