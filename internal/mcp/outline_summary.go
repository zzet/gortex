package mcp

import (
	"context"
	"slices"
	"time"

	"github.com/zzet/gortex/internal/graph"
)

const outlineSummaryWarmupLimit = time.Minute

type outlineSummaryWarmup struct {
	elapsed              time.Duration
	scopes, ready, bytes int
}

// knownOutlineScopes is registry-derived, never borrowed from an active
// session/project. Deterministic workspace scopes are preferred; exact repo
// narrows fill remaining slots. Unknown/custom scopes use the normal query.
func (s *Server) knownOutlineScopes() []graph.NodeScope {
	if s.multiIndexer == nil {
		return nil
	}
	prefixes := s.multiIndexer.RepoPrefixes()
	slices.Sort(prefixes)
	var workspaces, narrows []graph.NodeScope
	seen := make(map[string]bool)
	add := func(dst *[]graph.NodeScope, scope graph.NodeScope) {
		identity := outlineScopeIdentity(scope)
		if !seen[identity] {
			seen[identity] = true
			*dst = append(*dst, scope)
		}
	}
	for _, prefix := range prefixes {
		root, ok := s.multiIndexer.RepoRoot(prefix)
		if !ok {
			continue
		}
		ws, _, owner, ok := s.multiIndexer.ScopeForCWD(root)
		if !ok || ws == "" || owner != prefix {
			continue
		}
		add(&workspaces, graph.NodeScope{WorkspaceID: ws})
		add(&narrows, graph.NodeScope{WorkspaceID: ws, RepoAllow: map[string]bool{prefix: true}})
	}
	scopes := append(workspaces, narrows...)
	if len(scopes) > outlineSummaryEntries {
		scopes = scopes[:outlineSummaryEntries]
	}
	return scopes
}

// warmOutlineSummaries prepares only the fixed known scopes after explicit
// analysis. It never runs from an outline miss or an edit callback. Results
// remain exact only for this complete mutation/analysis proof: a write or scope
// reset invalidates them, and the next request takes the original COUNT path.
func (s *Server) warmOutlineSummaries(parent context.Context) outlineSummaryWarmup {
	var metrics outlineSummaryWarmup
	if parent.Err() != nil {
		return metrics
	}
	if !s.analysisSnapshotStillCurrent() {
		return metrics
	}
	scopes := s.knownOutlineScopes()
	if len(scopes) == 0 {
		return metrics
	}
	reader := s.readerFor(parent)
	first, ok := s.outlineCacheKey(reader, scopes[0], true)
	// Only canonical/default adjacency, never arbitrary identity masks.
	if !ok || (first.mask != "" && first.mask != "kinds") {
		return metrics
	}
	started := time.Now()
	ctx, cancel := context.WithTimeout(parent, outlineSummaryWarmupLimit)
	defer cancel()
	done := make(chan struct{})
	stopped := make(chan struct{})
	defer func() { close(done); <-stopped }()
	go func() {
		defer close(stopped)
		ticker := time.NewTicker(25 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				if s.editLaneBusy() || first.reader.MutationRevision() != first.revision || first.analysisReader.MutationRevision() != first.analysisRevision {
					cancel()
					return
				}
			}
		}
	}()
	metrics.scopes = len(scopes)
	for _, scope := range scopes {
		if ctx.Err() != nil || s.editLaneBusy() {
			break
		}
		key, ok := s.outlineCacheKey(reader, scope, true)
		if !ok || key.revision != first.revision || key.analysisRevision != first.analysisRevision || key.epoch != first.epoch || key.scopeEpoch != first.scopeEpoch {
			break
		}
		counts, supported, err := scopedOutlineCountsFor(graph.BindReadContext(reader, ctx), scope)
		if err != nil || !supported {
			break
		}
		var communitySection map[string]any
		communities, available, err := s.outlineGenerationCommunities(ctx, reader, scope)
		if err != nil {
			break
		}
		if available {
			communitySection = map[string]any{"count": 0}
			if communities != nil {
				communitySection = outlineCommunitySection(communities.Communities, communities.Modularity)
			}
		}
		after, ok := s.outlineCacheKey(reader, scope, true)
		if !ok || after != key || ctx.Err() != nil {
			break
		}
		if bytes, stored := s.outlineResponses.putSummary(key, counts, communitySection); stored {
			metrics.ready++
			metrics.bytes += bytes
		}
	}
	metrics.elapsed = time.Since(started)
	return metrics
}
