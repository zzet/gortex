package mcp

import (
	"bytes"
	"compress/gzip"
	"encoding/gob"
	"fmt"
	"io"
	"time"

	"github.com/zzet/gortex/internal/analysis"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/search"
	"go.uber.org/zap"
)

const analysisBlobDecodeLimit = int64(1 << 30)

type persistedAnalysis struct {
	communities  *analysis.CommunityResult
	leiden       *analysis.LeidenPartitionCache
	processes    *analysis.ProcessResult
	pageRank     *analysis.PageRankResult
	adjacency    *analysis.AdjacencySnapshot
	autoConcepts *search.AutoConcepts
	hits         *analysis.HITSResult
}

func encodeAnalysisBlobValue(value any) ([]byte, error) {
	var buf bytes.Buffer
	zw, err := gzip.NewWriterLevel(&buf, gzip.BestSpeed)
	if err != nil {
		return nil, err
	}
	if err := gob.NewEncoder(zw).Encode(value); err != nil {
		_ = zw.Close()
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func decodeAnalysisBlobValue(payload []byte, destination any) error {
	zr, err := gzip.NewReader(bytes.NewReader(payload))
	if err != nil {
		return err
	}
	defer zr.Close()
	limited := &limitedAnalysisBlobReader{Reader: io.LimitReader(zr, analysisBlobDecodeLimit+1)}
	if err := gob.NewDecoder(limited).Decode(destination); err != nil {
		return err
	}
	if limited.consumed > analysisBlobDecodeLimit {
		return fmt.Errorf("analysis blob exceeds %d decoded bytes", analysisBlobDecodeLimit)
	}
	return nil
}

type limitedAnalysisBlobReader struct {
	io.Reader
	consumed int64
}

func (r *limitedAnalysisBlobReader) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	r.consumed += int64(n)
	return n, err
}

type analysisRunMetrics struct {
	cacheHit     bool
	cacheLoad    time.Duration
	cacheSave    time.Duration
	cacheSaveErr error
	snapshot     time.Duration
	leiden       time.Duration
	processes    time.Duration
	pageRank     time.Duration
	adjacency    time.Duration
	autoConcepts time.Duration
	hits         time.Duration
}

// populateAnalysisLocked either publishes a current durable generation header
// without materializing its rows, or computes one complete analysis snapshot
// and activates it through the generation store's mutation-revision gate. The
// caller holds analysisMu.
func (s *Server) populateAnalysisLocked() analysisRunMetrics {
	const maxSnapshotAttempts = 3

	generationWriter, generationQuery := s.analysisGenerationBackends()
	generationBacked := generationWriter != nil && generationQuery != nil
	var lastMetrics analysisRunMetrics
	for attempt := 0; attempt < maxSnapshotAttempts; attempt++ {
		var metrics analysisRunMetrics

		// Header-only warm start: normalized rows and dense algorithm blobs stay
		// in SQLite until a bounded consumer asks for them.
		if generationBacked {
			started := time.Now()
			header, found, err := generationQuery.LoadActiveAnalysisHeader(analysisGenerationFormatVersion)
			metrics.cacheLoad = time.Since(started)
			if err != nil {
				if s.logger != nil {
					s.logger.Warn("mcp: active analysis generation rejected", zap.Error(err))
				}
			} else if found {
				sourceToken := s.currentCommunityToken()
				if sourceToken.analysisRevision != header.GraphRevision {
					lastMetrics = metrics
					continue
				}
				installHeader := func() {
					s.communities = nil
					s.leidenCache = nil
					s.processes = nil
					s.pageRank = nil
					s.adjacency = nil
					s.autoConcepts = nil
					s.hits = nil
					s.analysisGeneration = header
					s.analysisGenerationReady = true
					s.communitiesToken = sourceToken
					s.adjacencyToken = sourceToken
					s.hotspots = nil
					s.hotspotsReady = false
					s.analysisEpoch++
				}
				if generationWriter.CommitAnalysisSnapshot(header.GraphRevision, installHeader) {
					metrics.cacheHit = true
					return metrics
				}
				lastMetrics = metrics
				continue
			}
		}

		expectedRevision := uint64(0)
		if generationWriter != nil {
			expectedRevision = generationWriter.AnalysisMutationRevision()
		}
		sourceToken := s.currentCommunityToken()
		if generationWriter != nil && sourceToken.analysisRevision != expectedRevision {
			lastMetrics = metrics
			continue
		}

		// Each analyzer now consumes a cursor-backed, predicate-scoped projection
		// directly from the store. Do not retain a second complete node+edge
		// corpus in Go: SQLite is the snapshot and its generation/revision gate
		// below still provides the same atomic publication semantics.
		analysisGraph := s.graph
		candidate := persistedAnalysis{}
		// Start breadcrumb per analyzer: these are whole-graph walks that can
		// each run minutes on a large cold workspace, and metrics-only logging
		// left the whole chain silent until it finished.
		analyzerStart := func(pass string) {
			if s.logger != nil {
				s.logger.Info("analysis pass starting", zap.String("pass", pass))
			}
		}
		analyzerStart("leiden_communities")
		started := time.Now()
		candidate.communities, candidate.leiden, _ = analysis.DetectCommunitiesLeidenIncremental(analysisGraph, s.leidenCache)
		metrics.leiden = time.Since(started)
		analyzerStart("processes")
		started = time.Now()
		candidate.processes = analysis.DiscoverProcesses(analysisGraph)
		metrics.processes = time.Since(started)
		analyzerStart("pagerank")
		started = time.Now()
		candidate.pageRank = analysis.ComputePageRank(analysisGraph)
		metrics.pageRank = time.Since(started)
		analyzerStart("adjacency")
		started = time.Now()
		candidate.adjacency = analysis.BuildAdjacencySnapshot(analysisGraph)
		metrics.adjacency = time.Since(started)
		analyzerStart("auto_concepts")
		started = time.Now()
		candidate.autoConcepts = search.BuildAutoConcepts(analysisGraph)
		metrics.autoConcepts = time.Since(started)
		analyzerStart("hits")
		started = time.Now()
		candidate.hits = analysis.ComputeHITS(analysisGraph)
		metrics.hits = time.Since(started)

		var generationHeader graph.AnalysisGenerationHeader
		generationReady := false
		if generationBacked {
			started = time.Now()
			storedHeader, stored, err := persistAnalysisGeneration(generationWriter, expectedRevision, candidate)
			metrics.cacheSave = time.Since(started)
			if err != nil {
				metrics.cacheSaveErr = err
				if s.logger != nil {
					s.logger.Warn("mcp: normalized analysis generation save failed", zap.Error(err))
				}
			} else if !stored {
				lastMetrics = metrics
				continue
			} else {
				generationHeader = storedHeader
				generationReady = true
			}
		}

		install := func() {
			s.communities = candidate.communities
			s.leidenCache = candidate.leiden
			s.processes = candidate.processes
			s.pageRank = candidate.pageRank
			s.adjacency = candidate.adjacency
			s.autoConcepts = candidate.autoConcepts
			s.hits = candidate.hits
			s.analysisGeneration = generationHeader
			s.analysisGenerationReady = generationReady
			s.communitiesToken = sourceToken
			s.adjacencyToken = sourceToken
			// The fingerprints are derived from candidate.leiden, which was
			// computed over analysisGraph — s.graph, i.e. the payload view
			// generation analysisViewGeneration() names. They must be
			// installed through a handle pinned to THAT generation: the
			// bundle cache holds one authoritative map per generation and
			// validates a generation's entries against its own map only
			// (store_sqlite/bundle_cache.go), so installing through
			// backendStore() — the indexer's base handle — would stamp the
			// base corpus with another snapshot's fingerprints and leave the
			// analysed generation permanently uncacheable. When the two agree
			// (every unrouted deployment) analysisGenerationStore() returns
			// the backend unchanged and this is the behaviour it always had.
			if sink, ok := s.analysisGenerationStore().(graph.BundleFingerprintSink); ok && candidate.leiden != nil {
				sink.SetBundleFingerprints(candidate.leiden.PackageFingerprints())
			}
			s.hotspots = nil
			s.hotspotsReady = false
			s.analysisEpoch++
		}

		installed := false
		if generationWriter != nil {
			installed = generationWriter.CommitAnalysisSnapshot(expectedRevision, install)
		} else if sourceToken == s.currentCommunityToken() {
			install()
			installed = true
		}
		if installed {
			return metrics
		}
		lastMetrics = metrics
	}

	// Continuous writes denied every bounded publish attempt. Fail closed: a
	// result that cannot be tied to a stable graph revision is never exposed.
	s.communities = nil
	s.leidenCache = nil
	s.processes = nil
	s.pageRank = nil
	s.adjacency = nil
	s.autoConcepts = nil
	s.hits = nil
	s.analysisGeneration = graph.AnalysisGenerationHeader{}
	s.analysisGenerationReady = false
	s.communitiesToken = communityCacheToken{}
	s.adjacencyToken = communityCacheToken{}
	s.hotspots = nil
	s.hotspotsReady = false
	s.analysisEpoch++
	if s.logger != nil {
		s.logger.Warn("mcp: analysis publication abandoned after concurrent graph mutations", zap.Int("attempts", maxSnapshotAttempts))
	}
	return lastMetrics
}
