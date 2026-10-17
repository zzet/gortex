package resolver

import (
	"time"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
)

// ResolveFilesOutgoing is ResolveFilesAndIncoming without the incoming leg:
// it re-binds the unresolved references recorded IN filePaths and runs their
// attribution passes, and never enumerates the references parked on the
// names the files declare.
//
// It is the re-resolution a referrer of a changed declaration needs. A file
// whose references target a changed declaration (the affected-by frontier)
// has its own references to re-bind; its declarations did not change, so the
// references parked on their names are exactly what they were, and the
// incoming leg would read every stub row of every name the file declares to
// re-attempt references nothing moved. The changed files' own incoming leg,
// and the vanished-name pass, cover the names that did change.
func (r *Resolver) ResolveFilesOutgoing(filePaths []string) *ResolveStats {
	stats := &ResolveStats{}
	if len(filePaths) == 0 {
		return stats
	}
	started := time.Now()
	logger := r.logger.With(zap.Int("input_files", len(filePaths)))
	var frontier incrementalFileFrontier
	var indexDuration, warmDuration, outgoingDuration, attributionDuration time.Duration
	outcome := "interrupted"
	defer func() {
		logger.Info("resolver: outgoing files phases",
			zap.String("outcome", outcome),
			zap.Int("files", len(frontier.paths)),
			zap.Int("pending", len(frontier.pending)),
			zap.Duration("outgoing_collect", frontier.outgoingCollect),
			zap.Duration("build_indexes", indexDuration),
			zap.Duration("warm_lookup", warmDuration),
			zap.Duration("resolve_outgoing", outgoingDuration),
			zap.Duration("attribution", attributionDuration),
			zap.Duration("total", time.Since(started)))
	}()
	r.mu.Lock()
	defer r.mu.Unlock()
	r.scopedCSharpVisibilityInvalidate(filePaths)
	frontier = collectOutgoingFileFrontier(r.graph, filePaths, r.incrementalSkipped)
	if len(frontier.pending) == 0 {
		outcome = "no_pending"
		return stats
	}
	finish := startIncrementalPhase(logger, "outgoing_build_indexes")
	clear := r.buildPassIndexesForPending(frontier.pending)
	indexDuration = finish(zap.Int("pending", len(frontier.pending)))
	defer clear()
	finish = startIncrementalPhase(logger, "outgoing_warm_lookup")
	if err := r.warmLookupCache(frontier.pending); err != nil {
		r.clearLookupCache()
		outcome = "metadata_error"
		stats.Unresolved = len(frontier.pending)
		logger.Warn("resolver: Go package preparation failed", zap.Error(err))
		return stats
	}
	warmDuration = finish()
	defer r.clearLookupCache()
	finish = startIncrementalPhase(logger, "outgoing_resolve")
	r.resolvePreparedFileEdgesLocked(frontier.paths, frontier.nodesByFile, frontier.outByNode, stats, frontier.detached...)
	outgoingDuration = finish(
		zap.Int("resolved", stats.Resolved), zap.Int("unresolved", stats.Unresolved), zap.Int("external", stats.External))
	finish = startIncrementalPhase(logger, "outgoing_attribution")
	r.prepareIncrementalAttributionCache(frontier)
	r.runFileAttributionPassesForFilesLocked(frontier)
	r.clearIncrementalAttributionCache()
	attributionDuration = finish()
	outcome = "complete"
	return stats
}

// collectOutgoingFileFrontier is the outgoing half of
// collectIncrementalFileFrontierKeys: the files' nodes, their out-edges, the
// detached references recorded in them, and every unresolved reference among
// those. It enumerates no stub key and reads no incoming row.
func collectOutgoingFileFrontier(g graph.Store, filePaths []string, skip func(*graph.Edge) bool) incrementalFileFrontier {
	var frontier incrementalFileFrontier
	if g == nil {
		return frontier
	}
	seen := make(map[string]struct{}, len(filePaths))
	for _, path := range filePaths {
		if path == "" {
			continue
		}
		if _, duplicate := seen[path]; duplicate {
			continue
		}
		seen[path] = struct{}{}
		frontier.paths = append(frontier.paths, path)
	}
	if len(frontier.paths) == 0 {
		return frontier
	}
	started := time.Now()
	frontier.nodesByFile = g.GetFileNodesByPaths(frontier.paths)
	var nodeIDs []string
	for _, path := range frontier.paths {
		for _, node := range frontier.nodesByFile[path] {
			if node != nil && node.ID != "" {
				nodeIDs = append(nodeIDs, node.ID)
			}
		}
	}
	frontier.outByNode = g.GetOutEdgesByNodeIDs(nodeIDs)
	frontier.detached = detachedPendingAt(g, frontier.paths, frontier.outByNode)
	for _, edge := range frontier.detached {
		if skip == nil || !skip(edge) {
			frontier.pending = append(frontier.pending, edge)
		}
	}
	for _, path := range frontier.paths {
		for _, node := range frontier.nodesByFile[path] {
			if node == nil {
				continue
			}
			for _, edge := range frontier.outByNode[node.ID] {
				if graph.IsUnresolvedTarget(edge.To) && (skip == nil || !skip(edge)) {
					frontier.pending = append(frontier.pending, edge)
				}
			}
		}
	}
	frontier.outgoingPending = len(frontier.pending)
	frontier.outgoingCollect = time.Since(started)
	return frontier
}
