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
	// attempts counts computing attempts; superseded those stopped at a
	// checkpoint because the graph was written, supersededAt the last
	// such checkpoint. yields and yielded are the background pass's waits.
	attempts int
	// paceParks and paceParked are the parks inside sub-analyses
	// (analysis.Pace); projection*Scans the store reads of the shared
	// call/reference projection in the last computing attempt.
	paceParks           int
	paceParked          time.Duration
	projectionNodeScans int
	projectionEdgeScans int
	superseded          int
	supersededAt        string
	yields              int
	yielded             time.Duration
}

// populateAnalysisLocked is populateAnalysis for a caller that already holds
// analysisMu (tests); it never yields.
func (s *Server) populateAnalysisLocked() analysisRunMetrics {
	return s.populateAnalysis(false, false)
}

// populateAnalysis either publishes a current durable generation header
// without materializing its rows, or computes one complete analysis snapshot
// and activates it through the generation store's mutation-revision gate.
//
// With takeLock the computation runs without analysisMu and only each install
// takes it (analysis_pass.go); without it the caller holds analysisMu for the
// whole call. A background pass yields at its checkpoints.
func (s *Server) populateAnalysis(takeLock, background bool) analysisRunMetrics {
	const maxSnapshotAttempts = 3

	locked := func(fn func()) func() {
		if !takeLock {
			return fn
		}
		return func() {
			s.analysisMu.Lock()
			defer s.analysisMu.Unlock()
			fn()
		}
	}
	generationWriter, generationQuery := s.analysisGenerationBackends()
	generationBacked := generationWriter != nil && generationQuery != nil
	var lastMetrics analysisRunMetrics
	attempts, superseded, supersededAt, yields, paceParks := 0, 0, "", 0, 0
	var yielded, paceParked time.Duration
	finish := func(m analysisRunMetrics) analysisRunMetrics {
		m.attempts, m.superseded, m.supersededAt, m.yields, m.yielded = attempts, superseded, supersededAt, yields, yielded
		m.paceParks, m.paceParked = paceParks, paceParked
		return m
	}
	for attempt := 0; attempt < maxSnapshotAttempts; attempt++ {
		var metrics analysisRunMetrics
		// The attempt's first checkpoint, before its header load: a background
		// pass yields here too. No revision is pinned yet.
		(&analysisPassControl{s: s, background: background, metrics: &metrics}).checkpoint("load")
		yields += metrics.yields
		yielded += metrics.yielded
		metrics.yields, metrics.yielded = 0, 0

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
				if generationWriter.CommitAnalysisSnapshot(header.GraphRevision, locked(installHeader)) {
					metrics.cacheHit = true
					return finish(metrics)
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

		// Light node/edge scans consume bounded, predicate-scoped pages directly
		// from the store, without retaining a second complete node+edge corpus.
		// Pages may observe different revisions; the generation/revision gate
		// below rejects superseded results before atomic publication.
		analysisGraph := graph.BindAnalysisPages(s.graph)
		candidate := persistedAnalysis{}
		attempts++
		control := &analysisPassControl{s: s, writer: generationWriter, expected: expectedRevision, background: background, metrics: &metrics}
		stop := func(stage string) bool {
			if control.checkpoint(stage) {
				return false
			}
			superseded++
			supersededAt = stage
			return true
		}
		// A background pass paces its hot loops (analysis.Pace): it parks
		// within ~10 ms of an edit cycle or a tool call starting, even in the
		// middle of a sub-analysis. PageRank, the adjacency snapshot and HITS
		// read one shared call/reference projection instead of three store
		// scans.
		var pace *analysis.Pace
		if background {
			pace = analysis.NewPace(s.analysisShouldYield)
		}
		projection := analysis.NewCallRefProjection(analysisGraph, pace)
		collectPace := func() {
			parks, parked := pace.Stats()
			metrics.paceParks, metrics.paceParked = parks, parked
		}
		collect := func() {
			collectPace()
			paceParks += metrics.paceParks
			paceParked += metrics.paceParked
			yields += metrics.yields
			yielded += metrics.yielded
		}
		leidenInput := s.leidenCache
		if takeLock {
			s.analysisMu.RLock()
			leidenInput = s.leidenCache
			s.analysisMu.RUnlock()
		}
		// Start breadcrumb per analyzer: these are whole-graph walks that can
		// each run minutes on a large cold workspace, and metrics-only logging
		// left the whole chain silent until it finished.
		analyzerStart := func(pass string) {
			if s.logger != nil {
				s.logger.Info("analysis pass starting", zap.String("pass", pass))
			}
		}
		if stop("leiden_communities") {
			collect()
			lastMetrics = metrics
			continue
		}
		analyzerStart("leiden_communities")
		started := time.Now()
		candidate.communities, candidate.leiden, _ = analysis.DetectCommunitiesLeidenIncrementalPaced(analysisGraph, leidenInput, pace)
		metrics.leiden = time.Since(started)
		if stop("processes") {
			collect()
			lastMetrics = metrics
			continue
		}
		analyzerStart("processes")
		started = time.Now()
		candidate.processes = analysis.DiscoverProcesses(analysisGraph)
		metrics.processes = time.Since(started)
		if stop("pagerank") {
			collect()
			lastMetrics = metrics
			continue
		}
		analyzerStart("pagerank")
		started = time.Now()
		candidate.pageRank = analysis.ComputePageRankPaced(projection, pace)
		metrics.pageRank = time.Since(started)
		if stop("adjacency") {
			collect()
			lastMetrics = metrics
			continue
		}
		analyzerStart("adjacency")
		started = time.Now()
		candidate.adjacency = analysis.BuildAdjacencySnapshotPaced(projection, pace)
		metrics.adjacency = time.Since(started)
		if stop("auto_concepts") {
			collect()
			lastMetrics = metrics
			continue
		}
		analyzerStart("auto_concepts")
		started = time.Now()
		candidate.autoConcepts = search.BuildAutoConcepts(analysisGraph)
		metrics.autoConcepts = time.Since(started)
		if stop("hits") {
			collect()
			lastMetrics = metrics
			continue
		}
		analyzerStart("hits")
		started = time.Now()
		candidate.hits = analysis.ComputeHITSPaced(projection, pace)
		metrics.projectionNodeScans, metrics.projectionEdgeScans = projection.StoreScans()
		projection.Release()
		metrics.hits = time.Since(started)

		if stop("save") {
			collect()
			lastMetrics = metrics
			continue
		}
		collect()
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
			installed = generationWriter.CommitAnalysisSnapshot(expectedRevision, locked(install))
		} else if sourceToken == s.currentCommunityToken() {
			locked(install)()
			installed = true
		}
		if installed {
			return finish(metrics)
		}
		lastMetrics = metrics
	}

	// Continuous writes denied every bounded publish attempt. Fail closed: a
	// result that cannot be tied to a stable graph revision is never exposed.
	if takeLock {
		s.analysisMu.Lock()
		defer s.analysisMu.Unlock()
	}
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
		s.logger.Warn("mcp: analysis publication abandoned after concurrent graph mutations",
			zap.Int("attempts", maxSnapshotAttempts), zap.Int("superseded", superseded), zap.String("superseded_at", supersededAt))
	}
	return finish(lastMetrics)
}
